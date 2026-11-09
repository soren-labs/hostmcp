package hub

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/soren-labs/rmc/internal/config"
	"github.com/soren-labs/rmc/internal/node"
	"github.com/soren-labs/rmc/internal/protocol"
)

// benchHub registers 100 clients and 200 devices; the benchmarked client is
// the last one, so token lookup scans every client.
func benchHub(b *testing.B) (*Hub, *httptest.Server) {
	b.Helper()
	// Per-call audit lines would dominate the measurement and corrupt the
	// benchmark output format.
	log.SetOutput(io.Discard)
	b.Cleanup(func() { log.SetOutput(os.Stderr) })
	cfg := config.Hub{Version: config.FormatVersion, Listen: "127.0.0.1:0", PublicURL: "http://127.0.0.1"}
	for i := range 99 {
		cfg.Clients = append(cfg.Clients, config.Client{ID: fmt.Sprintf("c%03d", i), TokenHash: config.Hash(fmt.Sprintf("client-token-%032d", i)), Devices: []string{"*"}})
	}
	cfg.Clients = append(cfg.Clients, config.Client{ID: "alice", TokenHash: config.Hash(aliceToken), Devices: []string{"dev1"}})
	cfg.Devices = append(cfg.Devices, config.Device{ID: "dev1", TokenHash: config.Hash(deviceToken)})
	for i := range 199 {
		cfg.Devices = append(cfg.Devices, config.Device{ID: fmt.Sprintf("d%03d", i), TokenHash: config.Hash(fmt.Sprintf("device-token-%032d", i))})
	}
	path := filepath.Join(b.TempDir(), "hub.json")
	if err := config.WriteJSON(path, cfg, false); err != nil {
		b.Fatal(err)
	}
	h, err := New(config.NewStore(path), specs())
	if err != nil {
		b.Fatal(err)
	}
	srv := httptest.NewServer(h.Handler())
	b.Cleanup(func() { h.Close(); srv.Close() })
	return h, srv
}

func BenchmarkAuthenticate(b *testing.B) {
	h, _ := benchHub(b)
	r := httptest.NewRequest("POST", "http://127.0.0.1/mcp", nil)
	r.Header.Set("Authorization", "Bearer "+aliceToken)
	for b.Loop() {
		if _, code := h.authenticate(r); code != 0 {
			b.Fatal(code)
		}
	}
}

// BenchmarkToolCallHTTP measures one MCP tools/call through the full Hub
// handler and a loopback Agent connection.
func BenchmarkToolCallHTTP(b *testing.B) {
	h, srv := benchHub(b)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = node.Run(ctx, config.Node{Version: 1, HubURL: srv.URL, DeviceID: "dev1", Token: deviceToken}, &recordingExecutor{}, protocol.Device{ID: "dev1", Hostname: "bench", OS: "test", Username: "bench"})
	}()
	b.Cleanup(func() { cancel(); <-done })
	for deadline := time.Now().Add(5 * time.Second); ; {
		if list, err := h.ListDevices(context.Background(), "alice"); err == nil && len(list) == 1 && list[0].Online {
			break
		}
		if time.Now().After(deadline) {
			b.Fatal("agent did not connect")
		}
		time.Sleep(10 * time.Millisecond)
	}
	c := mcp.NewClient(&mcp.Implementation{Name: "bench", Version: "1"}, nil)
	s, err := c.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: srv.URL + "/mcp", HTTPClient: &http.Client{Transport: bearerTransport{aliceToken}}, MaxRetries: -1, DisableStandaloneSSE: true}, nil)
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = s.Close() })
	args := map[string]any{"device": "dev1", "text": "bench"}
	for b.Loop() {
		res, err := s.CallTool(ctx, &mcp.CallToolParams{Name: "echo", Arguments: args})
		if err != nil || res.IsError {
			b.Fatalf("%v %+v", err, res)
		}
	}
}
