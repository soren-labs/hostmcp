package node

import (
	"bytes"
	"context"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/soren-labs/rmc/internal/config"
	"github.com/soren-labs/rmc/internal/protocol"
)

type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// A hub that stops answering pings (here: it never reads, so no pong is sent)
// must be reported as a heartbeat failure, not as the misleading
// "use of closed network connection" produced when the ping closes the socket.
func TestHeartbeatFailureIsLoggedAsHeartbeat(t *testing.T) {
	logs := &lockedBuffer{}
	previousLog := log.Writer()
	log.SetOutput(logs)
	t.Cleanup(func() { log.SetOutput(previousLog) })

	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		defer cancel()
		var hello protocol.Message
		if wsjson.Read(ctx, conn, &hello) != nil {
			return
		}
		if wsjson.Write(ctx, conn, protocol.Message{Type: "welcome", Version: protocol.WireVersion}) != nil {
			return
		}
		// Stop reading: pings from the agent now go unanswered.
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(release) })

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- runWithHeartbeat(ctx, config.Node{Version: 1, HubURL: srv.URL, DeviceID: "dev1", Token: "0123456789012345678901234567890123456789012"}, &counterExecutor{}, protocol.Device{}, 50*time.Millisecond, 100*time.Millisecond, Hooks{})
	}()
	deadline := time.Now().Add(3 * time.Second)
	for !strings.Contains(logs.String(), "disconnected; reconnecting") && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("agent did not stop")
	}
	out := logs.String()
	if !strings.Contains(out, "heartbeat failed") {
		t.Fatalf("disconnect reason did not name the heartbeat: %q", out)
	}
	if strings.Contains(out, "use of closed network connection") {
		t.Fatalf("misleading closed-connection error was logged: %q", out)
	}
}
