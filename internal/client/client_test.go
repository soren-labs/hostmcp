package client

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestCredentialsNeverFollowRedirects(t *testing.T) {
	var received atomic.Int32
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { received.Add(1) }))
	defer other.Close()
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+strings.Repeat("x", 43) {
			t.Error("missing authorized credential")
		}
		http.Redirect(w, r, other.URL, 307)
	}))
	defer origin.Close()
	c := HTTPClient(origin.URL, strings.Repeat("x", 43))
	if _, err := c.Get(origin.URL); err == nil {
		t.Fatal("redirect followed")
	}
	if received.Load() != 0 {
		t.Fatal("credential reached another server")
	}
	if _, err := c.Get(other.URL); err == nil {
		t.Fatal("cross-origin credential request accepted")
	}
}

func TestBridgePreservesInstructionsArgumentsAndResults(t *testing.T) {
	for _, version := range []string{"2025-11-25", "2026-07-28"} {
		t.Run(version, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			const instructions = "Each node executes as its OS account; do not replay unknown outcomes."
			remote := mcp.NewServer(&mcp.Implementation{Name: "remote", Version: "test"}, &mcp.ServerOptions{Instructions: instructions})
			var calls atomic.Int32
			remote.AddTool(&mcp.Tool{Name: "identity", InputSchema: map[string]any{"type": "object"}}, func(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				calls.Add(1)
				var args map[string]any
				if err := json.Unmarshal(req.Params.Arguments, &args); err != nil || args["device"] != "linux-worker" {
					t.Errorf("arguments changed in transit: %s (%v)", req.Params.Arguments, err)
				}
				return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "remote identity"}}, StructuredContent: map[string]any{"uid": "1001"}}, nil
			})
			remote.AddTool(&mcp.Tool{Name: "fail", InputSchema: map[string]any{"type": "object"}}, func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				calls.Add(1)
				return nil, errors.New("connection outcome is unknown")
			})
			upstream := inMemorySession(t, ctx, remote, version)
			bridge, err := bridgeServer(ctx, upstream)
			if err != nil {
				t.Fatal(err)
			}
			downstream := inMemorySession(t, ctx, bridge, version)
			if got := downstream.InitializeResult().Instructions; got != instructions {
				t.Fatalf("instructions lost: %q", got)
			}
			catalog, err := downstream.ListTools(ctx, nil)
			if err != nil || len(catalog.Tools) != 2 {
				t.Fatalf("catalog: %v, %v", catalog, err)
			}
			result, err := downstream.CallTool(ctx, &mcp.CallToolParams{Name: "identity", Arguments: map[string]any{"device": "linux-worker"}})
			if err != nil || result.IsError || len(result.Content) != 1 {
				t.Fatalf("identity call: %v, %v", result, err)
			}
			encoded, err := json.Marshal(result.StructuredContent)
			if err != nil || string(encoded) != `{"uid":"1001"}` {
				t.Fatalf("structured result changed: %s, %v", encoded, err)
			}
			failed, err := downstream.CallTool(ctx, &mcp.CallToolParams{Name: "fail", Arguments: map[string]any{}})
			if err != nil || !failed.IsError {
				t.Fatalf("remote failure was not preserved as a tool error: %v, %v", failed, err)
			}
			if calls.Load() != 2 {
				t.Fatalf("a call was replayed: %d requests", calls.Load())
			}
		})
	}
}

func inMemorySession(t *testing.T, ctx context.Context, server *mcp.Server, version string) *mcp.ClientSession {
	t.Helper()
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	ss, err := server.Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ss.Close() })
	c := mcp.NewClient(&mcp.Implementation{Name: "bridge-test", Version: "test"}, nil)
	cs, err := c.Connect(ctx, clientTransport, &mcp.ClientSessionOptions{ProtocolVersion: version})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

func TestTokenEnvironmentAndResponseLimit(t *testing.T) {
	for _, secret := range []string{strings.Repeat("a", 1025), strings.Repeat("a", 32) + " b", strings.Repeat("a", 32) + "\nother"} {
		t.Setenv("HOSTMCP_TOKEN", secret)
		if _, err := Token(""); err == nil {
			t.Fatal("malformed credential accepted")
		}
	}
	b := &boundedBody{ReadCloser: io.NopCloser(strings.NewReader("123456")), remaining: 5}
	out, err := io.ReadAll(b)
	if err == nil || string(out) != "12345" {
		t.Fatalf("response limit failed: %q %v", out, err)
	}
}
