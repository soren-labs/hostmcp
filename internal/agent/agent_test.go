package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/soren-labs/hostmcp/internal/config"
	"github.com/soren-labs/hostmcp/internal/protocol"
)

type counterExecutor struct{ count atomic.Int64 }

func (c *counterExecutor) Close() error { return nil }
func (c *counterExecutor) Call(_ context.Context, _ string, _ string, _ json.RawMessage) (protocol.Result, error) {
	return protocol.JSONResult(map[string]any{"executions": c.count.Add(1)}), nil
}

func TestDuplicateWireRequestDoesNotExecuteTwice(t *testing.T) {
	exec := &counterExecutor{}
	result := make(chan error, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			result <- err
			return
		}
		defer conn.CloseNow()
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		var hello protocol.Message
		if err := wsjson.Read(ctx, conn, &hello); err != nil {
			result <- err
			return
		}
		if err := wsjson.Write(ctx, conn, protocol.Message{Type: "welcome", Version: 1}); err != nil {
			result <- err
			return
		}
		request := protocol.Message{Type: "request", ID: "same-id", Principal: "owner", Tool: "side-effect", Args: json.RawMessage("{}")}
		for range 2 {
			if err := wsjson.Write(ctx, conn, request); err != nil {
				result <- err
				return
			}
		}
		var successful, duplicates int
		for range 2 {
			var reply protocol.Message
			if err := wsjson.Read(ctx, conn, &reply); err != nil {
				result <- err
				return
			}
			if reply.Result == nil {
				result <- fmt.Errorf("no result")
				return
			}
			if reply.Result.Error != "" {
				duplicates++
			} else {
				successful++
			}
		}
		if successful != 1 || duplicates != 1 {
			result <- fmt.Errorf("success=%d duplicate errors=%d", successful, duplicates)
			return
		}
		result <- nil
	}))
	defer srv.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, config.Agent{Version: 1, HubURL: srv.URL, DeviceID: "test", Token: "0123456789012345678901234567890123456789012"}, exec, protocol.Device{})
	}()
	select {
	case err := <-result:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("duplicate test timed out")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("agent shutdown blocked")
	}
	if exec.count.Load() != 1 {
		t.Fatalf("executions=%d", exec.count.Load())
	}
}

func TestAgentRejectsCredentialLeakingURLs(t *testing.T) {
	for _, raw := range []string{"http://remote.example", "https://owner:secret@example.com", "https://example.com?token=secret", "https://example.com/mcp", "file:///etc/passwd"} {
		if _, err := endpointURL(config.Agent{Version: 1, HubURL: raw, DeviceID: "test", Token: "0123456789012345678901234567890123456789012"}); err == nil {
			t.Errorf("accepted %s", raw)
		}
	}
}
