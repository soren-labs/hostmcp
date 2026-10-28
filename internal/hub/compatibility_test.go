package hub

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/soren-labs/hostmcp/internal/oauth"
	"github.com/soren-labs/hostmcp/internal/protocol"
	"github.com/soren-labs/hostmcp/internal/testutil"
)

func TestCompatibilityLegacyAgentWithOAuthHub(t *testing.T) {
	h, _, issuer := oauthFixture(t)
	memory := testutil.NewMemoryServer(t, h.Handler())
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, "ws://127.0.0.1/agent/allowed", &websocket.DialOptions{HTTPClient: &http.Client{Transport: memory.Transport}, HTTPHeader: http.Header{"Authorization": []string{"Bearer " + deviceToken}}})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()
	// This is the exact old v1 field set: no component, wire_version or build_version.
	hello := map[string]any{"type": "hello", "version": 1, "device": map[string]any{"id": "allowed", "version": "0.2.1", "username": "test-user"}}
	if err = wsjson.Write(ctx, conn, hello); err != nil {
		t.Fatal(err)
	}
	var legacyWelcome struct {
		Type    string `json:"type"`
		Version int    `json:"version"`
	}
	if err = wsjson.Read(ctx, conn, &legacyWelcome); err != nil {
		t.Fatal(err)
	}
	if legacyWelcome.Type != "welcome" || legacyWelcome.Version != 1 {
		t.Fatalf("legacy welcome: %+v", legacyWelcome)
	}
	done := make(chan error, 1)
	go func() {
		var request protocol.Message
		if err := wsjson.Read(ctx, conn, &request); err != nil {
			done <- err
			return
		}
		done <- wsjson.Write(ctx, conn, protocol.Message{Type: "result", ID: request.ID, Result: &protocol.Result{Data: json.RawMessage(`{"legacy":true}`)}})
	}()
	principal := oauth.Principal(issuer.URL, "https://mcp.example/mcp", "owner", "test-client")
	result, err := h.Call(ctx, principal, "allowed", "echo", json.RawMessage(`{}`))
	if err != nil || !strings.Contains(string(result.Data), "legacy") {
		t.Fatalf("OAuth dispatch to old agent: %s %v", result.Data, err)
	}
	if err = <-done; err != nil {
		t.Fatal(err)
	}
	devices, err := h.ListDevices(ctx, principal)
	if err != nil || len(devices) != 1 || !devices[0].Online || devices[0].Version != "0.2.1" || devices[0].WireVersion != 1 || devices[0].LastSeen.IsZero() {
		t.Fatalf("inventory: %+v %v", devices, err)
	}
	firstSeen := devices[0].LastSeen
	// Keep a read active so websocket control frames receive pong responses.
	readDone := make(chan error, 1)
	go func() { _, _, err := conn.Read(ctx); readDone <- err }()
	h.mu.RLock()
	connected := h.connections["allowed"]
	h.mu.RUnlock()
	go h.heartbeat(connected, 10*time.Millisecond, 100*time.Millisecond)
	eventually(t, func() bool {
		devices, _ := h.ListDevices(ctx, principal)
		return len(devices) == 1 && devices[0].LastSeen.After(firstSeen)
	})
	conn.CloseNow()
	<-readDone
	eventually(t, func() bool {
		devices, _ := h.ListDevices(ctx, principal)
		return len(devices) == 1 && !devices[0].Online && !devices[0].DisconnectedAt.IsZero()
	})
}
func TestCompatibilityWireMismatchRejectsAgent(t *testing.T) {
	h, _, _ := oauthFixture(t)
	memory := testutil.NewMemoryServer(t, h.Handler())
	for _, version := range []int{0, 2} {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		conn, _, err := websocket.Dial(ctx, "ws://127.0.0.1/agent/allowed", &websocket.DialOptions{HTTPClient: &http.Client{Transport: memory.Transport}, HTTPHeader: http.Header{"Authorization": []string{"Bearer " + deviceToken}}})
		if err != nil {
			cancel()
			t.Fatal(err)
		}
		if err = wsjson.Write(ctx, conn, protocol.Message{Type: "hello", Version: version, Device: &protocol.Device{ID: "allowed", Version: "999.0.0"}}); err != nil {
			cancel()
			t.Fatal(err)
		}
		var welcome protocol.Message
		if err = wsjson.Read(ctx, conn, &welcome); err == nil {
			t.Fatal("incompatible agent welcomed")
		}
		conn.CloseNow()
		cancel()
	}
}
