package agent

import (
	"context"
	"errors"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/soren-labs/roost/internal/protocol"
	"github.com/soren-labs/roost/internal/testutil"
)

// Delay the ping writer after its transport is closed. The websocket package
// closes timed-out writes internally, so the reader can fail before Ping has
// returned its error to the heartbeat goroutine.
type delayedWriteErrorConn struct{ net.Conn }

func (c delayedWriteErrorConn) Write(p []byte) (int, error) {
	n, err := c.Conn.Write(p)
	if err != nil {
		time.Sleep(50 * time.Millisecond)
	}
	return n, err
}

// A peer close wakes Ping before the reader finishes processing the close
// frame. This is a transport shutdown, not a missed heartbeat deadline.
type delayedCloseConn struct{ net.Conn }

func (c delayedCloseConn) Close() error {
	err := c.Conn.Close()
	time.Sleep(50 * time.Millisecond)
	return err
}

func heartbeatTestConnection(t *testing.T, wrap func(net.Conn) net.Conn, serve func(context.Context, *websocket.Conn)) string {
	t.Helper()
	release := make(chan struct{})
	memory := testutil.NewMemoryServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
			OnPingReceived: func(context.Context, []byte) bool { return false },
		})
		if err != nil {
			return
		}
		defer conn.CloseNow()
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		var hello protocol.Message
		if wsjson.Read(ctx, conn, &hello) != nil {
			return
		}
		if wsjson.Write(ctx, conn, protocol.Message{Type: "welcome", Version: protocol.WireVersion}) != nil {
			return
		}
		if serve != nil {
			serve(ctx, conn)
		}
		select {
		case <-release:
		case <-ctx.Done():
		}
	}))
	t.Cleanup(func() { close(release) })
	dial := memory.Transport.DialContext
	memory.Transport.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		conn, err := dial(ctx, network, addr)
		if err == nil {
			conn = wrap(conn)
		}
		return conn, err
	}
	previous := http.DefaultTransport
	http.DefaultTransport = memory.Transport
	t.Cleanup(func() { http.DefaultTransport = previous })
	return memory.URL
}

func TestHeartbeatWriteTimeoutPreservesCause(t *testing.T) {
	endpoint := heartbeatTestConnection(t, func(c net.Conn) net.Conn { return delayedWriteErrorConn{c} }, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	var workers sync.WaitGroup
	err := connect(ctx, endpoint, "test-credential", &counterExecutor{}, protocol.Device{ID: "test"}, make(chan struct{}, maxConcurrentRequests), newRequestIDs(), &workers, 20*time.Millisecond, 80*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "heartbeat failed") || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("ping write timeout lost its cause: %v", err)
	}
	if strings.Contains(err.Error(), "no pong within") {
		t.Fatalf("a ping write failure was described as an unanswered pong: %v", err)
	}
}

func TestPeerCloseDuringPingKeepsCloseReason(t *testing.T) {
	endpoint := heartbeatTestConnection(t, func(c net.Conn) net.Conn { return delayedCloseConn{c} }, func(ctx context.Context, conn *websocket.Conn) {
		// Reading consumes the ping so its write finishes. A timer then closes
		// the peer well before the heartbeat's one-second deadline.
		closed := conn.CloseRead(ctx)
		time.Sleep(100 * time.Millisecond)
		_ = conn.Close(websocket.StatusGoingAway, "maintenance")
		<-closed.Done()
	})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	var workers sync.WaitGroup
	err := connect(ctx, endpoint, "test-credential", &counterExecutor{}, protocol.Device{ID: "test"}, make(chan struct{}, maxConcurrentRequests), newRequestIDs(), &workers, 20*time.Millisecond, time.Second)
	if websocket.CloseStatus(err) != websocket.StatusGoingAway {
		t.Fatalf("peer close reason was replaced by a heartbeat error: %v", err)
	}
	if strings.Contains(err.Error(), "heartbeat failed") {
		t.Fatalf("peer shutdown was reported as a heartbeat failure: %v", err)
	}
}
