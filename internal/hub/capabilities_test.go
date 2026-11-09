package hub

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/soren-labs/rmc/internal/config"
	"github.com/soren-labs/rmc/internal/host"
	"github.com/soren-labs/rmc/internal/protocol"
)

func schema(args ...string) map[string]any {
	props := map[string]any{}
	for _, a := range args {
		props[a] = map[string]any{"type": "string"}
	}
	return map[string]any{"type": "object", "properties": props}
}

// gatedSpecs is a Hub tool surface with one new argument on a legacy native
// tool, one post-v1 native tool and one tool unknown to native Agents.
func gatedSpecs() []protocol.Tool {
	return []protocol.Tool{
		{Name: "write_stdin", InputSchema: schema("session_id", "chars", "future_arg")},
		{Name: "future_tool", InputSchema: schema("path")},
		{Name: "echo", InputSchema: schema("text")},
	}
}

type advertisingExecutor struct {
	recordingExecutor
	tools []protocol.Tool
}

func (e *advertisingExecutor) Tools() []protocol.Tool { return e.tools }

func gatedFixture(t *testing.T) (*Hub, *httptest.Server) {
	t.Helper()
	postV1Tools["future_tool"] = true
	t.Cleanup(func() { delete(postV1Tools, "future_tool") })
	path := filepath.Join(t.TempDir(), "hub.json")
	cfg := config.Hub{Version: config.FormatVersion, Listen: "127.0.0.1:0", PublicURL: "http://127.0.0.1", Clients: []config.Client{
		{ID: "alice", TokenHash: config.Hash(aliceToken), Devices: []string{"dev1"}},
	}, Devices: []config.Device{{ID: "dev1", TokenHash: config.Hash(deviceToken)}}}
	if err := config.WriteJSON(path, cfg, false); err != nil {
		t.Fatal(err)
	}
	h, err := New(config.NewStore(path), gatedSpecs())
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h.Handler())
	t.Cleanup(func() { h.Close(); srv.Close() })
	return h, srv
}

func TestLegacyAgentIsNotSentNewerNativeFeatures(t *testing.T) {
	h, srv := gatedFixture(t)
	exec := &recordingExecutor{}
	startAgent(t, srv.URL, exec)
	eventually(t, func() bool { return online(h) })
	call := func(tool, args string) error {
		_, err := h.Call(context.Background(), "alice", "dev1", tool, json.RawMessage(args))
		return err
	}
	for _, tc := range []struct{ tool, args, want string }{
		{"write_stdin", `{"session_id":"s","future_arg":"x"}`, "the future_arg argument of write_stdin"},
		{"future_tool", `{"path":"."}`, "the future_tool tool"},
	} {
		err := call(tc.tool, tc.args)
		if err == nil || !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), "not dispatched") {
			t.Fatalf("%s %s: %v", tc.tool, tc.args, err)
		}
	}
	if n := exec.calls.Load(); n != 0 {
		t.Fatalf("unsupported features reached the legacy agent %d times", n)
	}
	// Legacy arguments, unknown arguments (left to the Agent) and tools that
	// are not native still dispatch.
	for _, tc := range []struct{ tool, args string }{
		{"write_stdin", `{"session_id":"s","chars":"x"}`},
		{"write_stdin", `{"session_id":"s","typo":"x"}`},
		{"echo", `{"text":"hi"}`},
	} {
		if err := call(tc.tool, tc.args); err != nil {
			t.Fatalf("%s %s: %v", tc.tool, tc.args, err)
		}
	}
	if n := exec.calls.Load(); n != 3 {
		t.Fatalf("calls=%d, want 3", n)
	}
}

func TestAdvertisingAgentReceivesWhatItDeclares(t *testing.T) {
	h, srv := gatedFixture(t)
	exec := &advertisingExecutor{tools: []protocol.Tool{
		{Name: "write_stdin", InputSchema: schema("session_id", "chars", "future_arg")},
		{Name: "future_tool", InputSchema: schema("path")},
	}}
	startAgent(t, srv.URL, exec)
	eventually(t, func() bool { return online(h) })
	for _, tc := range []struct{ tool, args string }{
		{"write_stdin", `{"session_id":"s","future_arg":"x"}`},
		{"future_tool", `{"path":"."}`},
	} {
		if _, err := h.Call(context.Background(), "alice", "dev1", tc.tool, json.RawMessage(tc.args)); err != nil {
			t.Fatalf("%s: %v", tc.tool, err)
		}
	}
	// A Hub-known tool the Agent did not declare is refused even if it is
	// not registered as post-v1: the advertisement is authoritative.
	if _, err := h.Call(context.Background(), "alice", "dev1", "echo", json.RawMessage(`{"text":"hi"}`)); err == nil || !strings.Contains(err.Error(), "the echo tool") {
		t.Fatalf("undeclared tool: %v", err)
	}
	if n := exec.calls.Load(); n != 2 {
		t.Fatalf("calls=%d, want 2", n)
	}
}

func TestAdvertisementWithoutNewArgumentIsRefused(t *testing.T) {
	h, srv := gatedFixture(t)
	exec := &advertisingExecutor{tools: []protocol.Tool{{Name: "write_stdin", InputSchema: schema("session_id", "chars")}}}
	startAgent(t, srv.URL, exec)
	eventually(t, func() bool { return online(h) })
	_, err := h.Call(context.Background(), "alice", "dev1", "write_stdin", json.RawMessage(`{"session_id":"s","future_arg":"x"}`))
	if err == nil || !strings.Contains(err.Error(), "future_arg") {
		t.Fatalf("got %v", err)
	}
	if exec.calls.Load() != 0 {
		t.Fatal("refused call was dispatched")
	}
}

func TestOversizedAdvertisementFallsBackToLegacy(t *testing.T) {
	big := map[string][]string{}
	for i := range maxAdvertisedTools + 1 {
		big[fmt.Sprintf("tool%d", i)] = nil
	}
	if advertisedTools(big) != nil || advertisedTools(map[string][]string{"x": {""}}) != nil || advertisedTools(nil) != nil {
		t.Fatal("invalid advertisement accepted")
	}
}

// The frozen legacy table must stay a subset of the native tools, and every
// native tool newer than it must be registered so legacy Agents never get it.
func TestLegacyTableMatchesNativeTools(t *testing.T) {
	native := protocol.ToolArgNames(host.Tools())
	for tool, args := range legacyToolArgs {
		have, ok := native[tool]
		if !ok {
			t.Fatalf("legacy tool %s is no longer native", tool)
		}
		for a := range args {
			if !slices.Contains(have, a) {
				t.Fatalf("legacy argument %s.%s is no longer native", tool, a)
			}
		}
	}
	for tool := range native {
		if _, legacy := legacyToolArgs[tool]; !legacy && !postV1Tools[tool] {
			t.Fatalf("native tool %s is newer than legacyToolArgs; add it to postV1Tools", tool)
		}
	}
}
