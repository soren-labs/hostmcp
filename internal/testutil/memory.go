package testutil

import (
	"context"
	"errors"
	"net"
	"net/http"
	"sync"
	"testing"
)

// MemoryServer exercises HTTP and websocket framing over net.Pipe; it neither
// opens a TCP port nor connects to another process. TLS is tested separately by
// deployment operators, not simulated here.
type MemoryServer struct {
	URL       string
	Transport *http.Transport
}
type memoryListener struct {
	incoming chan net.Conn
	done     chan struct{}
	once     sync.Once
}

func (l *memoryListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.incoming:
		return c, nil
	case <-l.done:
		return nil, net.ErrClosed
	}
}
func (l *memoryListener) Close() error { l.once.Do(func() { close(l.done) }); return nil }
func (*memoryListener) Addr() net.Addr { return memoryAddress{} }

type memoryAddress struct{}

func (memoryAddress) Network() string { return "pipe" }
func (memoryAddress) String() string  { return "127.0.0.1:80" }
func NewMemoryServer(t *testing.T, handler http.Handler) *MemoryServer {
	t.Helper()
	listener := &memoryListener{incoming: make(chan net.Conn), done: make(chan struct{})}
	server := &http.Server{Handler: handler}
	finished := make(chan error, 1)
	go func() { finished <- server.Serve(listener) }()
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		client, peer := net.Pipe()
		select {
		case listener.incoming <- peer:
			return client, nil
		case <-ctx.Done():
			client.Close()
			peer.Close()
			return nil, ctx.Err()
		case <-listener.done:
			client.Close()
			peer.Close()
			return nil, net.ErrClosed
		}
	}}
	t.Cleanup(func() {
		transport.CloseIdleConnections()
		server.Close()
		if err := <-finished; err != nil && !errors.Is(err, http.ErrServerClosed) {
			t.Error(err)
		}
	})
	return &MemoryServer{URL: "http://127.0.0.1", Transport: transport}
}
