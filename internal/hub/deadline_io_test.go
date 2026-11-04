package hub

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// These tests use real connections: cancelling a request's context does not
// itself interrupt net/http's request-body reads or response writes.
func TestRequestDeadlineInterruptsBodyRead(t *testing.T) {
	h, _, _ := fixture(t)
	h.requestDeadline = 150 * time.Millisecond
	finished := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.Handler().ServeHTTP(w, r)
		close(finished)
	}))
	t.Cleanup(srv.Close)
	conn := openDeadlineConnection(t, srv)
	if _, err := fmt.Fprintf(conn, "POST /mcp HTTP/1.1\r\nHost: %s\r\nAuthorization: Bearer %s\r\nContent-Type: application/json\r\nAccept: application/json, text/event-stream\r\nContent-Length: 1024\r\n\r\n{", srv.Listener.Addr(), aliceToken); err != nil {
		t.Fatal(err)
	}
	select {
	case <-finished:
	case <-time.After(2 * time.Second):
		t.Fatal("MCP body read retained its slot after the request deadline")
	}
	assertClientSlotsReleased(t, h)
}

func TestRequestDeadlineInterruptsResponseWrite(t *testing.T) {
	h, _, _ := fixture(t)
	h.requestDeadline = 150 * time.Millisecond
	finished := make(chan struct{})
	writeErr := make(chan error, 1)
	bounded := h.boundedClient(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		chunk := make([]byte, 64<<10)
		for {
			if _, err := w.Write(chunk); err != nil {
				writeErr <- err
				return
			}
		}
	}))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bounded.ServeHTTP(w, r)
		close(finished)
	}))
	t.Cleanup(srv.Close)
	conn := openDeadlineConnection(t, srv)
	// Never consume the response; a small receive buffer makes the write block
	// promptly without buffering a large response in memory.
	if err := conn.SetReadBuffer(1024); err != nil {
		t.Fatal(err)
	}
	if _, err := fmt.Fprintf(conn, "GET /mcp HTTP/1.1\r\nHost: %s\r\nAuthorization: Bearer %s\r\n\r\n", srv.Listener.Addr(), aliceToken); err != nil {
		t.Fatal(err)
	}
	select {
	case <-finished:
	case <-time.After(2 * time.Second):
		t.Fatal("blocked response write retained its slot after the request deadline")
	}
	err := <-writeErr
	var networkErr net.Error
	if !errors.As(err, &networkErr) || !networkErr.Timeout() {
		t.Fatalf("response write should end with an I/O timeout, got %v", err)
	}
	assertClientSlotsReleased(t, h)
}

func TestRequestDeadlinePreservesShorterServerReadTimeout(t *testing.T) {
	h, _, _ := fixture(t)
	h.requestDeadline = 3 * time.Second
	finished := make(chan struct{})
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.Handler().ServeHTTP(w, r)
		close(finished)
	}))
	srv.Config.ReadTimeout = 100 * time.Millisecond
	srv.Start()
	t.Cleanup(srv.Close)
	conn := openDeadlineConnection(t, srv)
	if _, err := fmt.Fprintf(conn, "POST /mcp HTTP/1.1\r\nHost: %s\r\nAuthorization: Bearer %s\r\nContent-Type: application/json\r\nAccept: application/json, text/event-stream\r\nContent-Length: 1024\r\n\r\n{", srv.Listener.Addr(), aliceToken); err != nil {
		t.Fatal(err)
	}
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("request deadline extended the server's shorter read timeout")
	}
	assertClientSlotsReleased(t, h)
}

func TestRequestDeadlineLeavesSuccessfulKeepAliveUsable(t *testing.T) {
	h, _, _ := fixture(t)
	h.requestDeadline = 100 * time.Millisecond
	srv := httptest.NewServer(h.boundedClient(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})))
	t.Cleanup(srv.Close)
	conn := openDeadlineConnection(t, srv)
	reader := bufio.NewReader(conn)
	for i := range 2 {
		if _, err := fmt.Fprintf(conn, "GET /mcp HTTP/1.1\r\nHost: %s\r\nAuthorization: Bearer %s\r\n\r\n", srv.Listener.Addr(), aliceToken); err != nil {
			t.Fatal(err)
		}
		resp, err := http.ReadResponse(reader, &http.Request{Method: http.MethodGet})
		if err != nil {
			t.Fatalf("response %d on the same connection: %v", i, err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusNoContent {
			t.Fatalf("response %d status=%d", i, resp.StatusCode)
		}
		if i == 0 {
			// A completed request's cancellation callback must not set a late
			// deadline on a connection that net/http has already reused.
			time.Sleep(2 * h.requestDeadline)
		}
	}
}

func openDeadlineConnection(t *testing.T, srv *httptest.Server) *net.TCPConn {
	t.Helper()
	conn, err := net.DialTimeout("tcp", srv.Listener.Addr().String(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	if err := conn.SetDeadline(time.Now().Add(4 * time.Second)); err != nil {
		t.Fatal(err)
	}
	tcp, ok := conn.(*net.TCPConn)
	if !ok {
		t.Fatal("expected a TCP connection")
	}
	return tcp
}

func assertClientSlotsReleased(t *testing.T, h *Hub) {
	t.Helper()
	if n := len(h.requests); n != 0 {
		t.Fatalf("%d global request slots remain occupied", n)
	}
	h.limitsMu.Lock()
	defer h.limitsMu.Unlock()
	if n := len(h.clientSlots); n != 0 {
		t.Fatalf("%d principal limits remain occupied", n)
	}
}
