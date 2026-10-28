package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/soren-labs/hostmcp/internal/config"
	"github.com/soren-labs/hostmcp/internal/protocol"
	"github.com/soren-labs/hostmcp/internal/testutil"
)

func TestCompatibilityNewAgentWithLegacyHub(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result := make(chan error, 1)
	memory := testutil.NewMemoryServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			result <- err
			return
		}
		defer conn.CloseNow()
		// A v0.2.1 Hub decodes the v1 hello and ignores new optional fields.
		var hello struct {
			Type    string           `json:"type"`
			Version int              `json:"version"`
			Device  *protocol.Device `json:"device"`
		}
		if err = wsjson.Read(ctx, conn, &hello); err != nil {
			result <- err
			return
		}
		if hello.Type != "hello" || hello.Version != 1 || hello.Device.Component != "agent" || hello.Device.Version != protocol.Version {
			t.Error("invalid new agent identity")
		}
		if err = wsjson.Write(ctx, conn, map[string]any{"type": "welcome", "version": 1}); err != nil {
			result <- err
			return
		}
		if err = wsjson.Write(ctx, conn, protocol.Message{Type: "request", ID: "request", Principal: "legacy-owner", Tool: "echo", Args: json.RawMessage(`{}`)}); err != nil {
			result <- err
			return
		}
		var reply protocol.Message
		err = wsjson.Read(ctx, conn, &reply)
		if err == nil && (reply.Type != "result" || reply.Result == nil) {
			t.Error("missing v1 result")
		}
		result <- err
	}))
	previous := http.DefaultTransport
	http.DefaultTransport = memory.Transport
	defer func() { http.DefaultTransport = previous }()
	finished := make(chan error, 1)
	go func() {
		finished <- Run(ctx, config.Agent{Version: 1, HubURL: memory.URL, DeviceID: "node", Token: "0123456789012345678901234567890123456789"}, &counterExecutor{}, protocol.Device{})
	}()
	select {
	case err := <-result:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	cancel()
	if err := <-finished; err != nil {
		t.Fatal(err)
	}
}
