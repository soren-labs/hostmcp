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

type waitingExecutor struct {
	started   chan struct{}
	cancelled chan struct{}
	calls     atomic.Int64
	closed    atomic.Bool
}

func (e *waitingExecutor) Close() error { e.closed.Store(true); return nil }
func (e *waitingExecutor) Call(ctx context.Context, _ string, tool string, _ json.RawMessage) (protocol.Result, error) {
	n := e.calls.Add(1)
	if tool == "wait" {
		if n != 1 {
			return protocol.Result{}, fmt.Errorf("abandoned request executed again")
		}
		close(e.started)
		<-ctx.Done()
		close(e.cancelled)
		return protocol.Result{}, ctx.Err()
	}
	return protocol.JSONResult(map[string]any{"executions": n}), nil
}

func TestDisconnectCancelsAbandonedCallAndReconnectsWithoutReplay(t *testing.T) {
	executor := &waitingExecutor{started: make(chan struct{}), cancelled: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var connections atomic.Int64
	result := make(chan error, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := connections.Add(1)
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			result <- err
			return
		}
		defer conn.CloseNow()
		wireCtx, stop := context.WithTimeout(ctx, 5*time.Second)
		defer stop()
		var hello protocol.Message
		if err := wsjson.Read(wireCtx, conn, &hello); err != nil {
			result <- err
			return
		}
		if err := wsjson.Write(wireCtx, conn, protocol.Message{Type: "welcome", Version: protocol.WireVersion}); err != nil {
			result <- err
			return
		}
		request := protocol.Message{Type: "request", ID: "abandoned-wait", Principal: "owner", Tool: "wait", Args: json.RawMessage("{}")}
		if n == 1 {
			if err := wsjson.Write(wireCtx, conn, request); err != nil {
				result <- err
				return
			}
			select {
			case <-executor.started:
			case <-wireCtx.Done():
				result <- fmt.Errorf("first executor call did not start")
				return
			}
			_ = conn.CloseNow()
			select {
			case <-executor.cancelled:
			case <-time.After(2 * time.Second):
				result <- fmt.Errorf("disconnected request continued waiting on the agent lifetime context")
			}
			return
		}
		if ctx.Err() != nil || executor.closed.Load() || executor.calls.Load() != 1 {
			result <- fmt.Errorf("reconnect cancelled the agent, closed its executor, or replayed a request")
			return
		}
		select {
		case <-executor.cancelled:
		default:
			result <- fmt.Errorf("old call was still waiting after reconnect")
			return
		}
		// The same wire ID on the new connection remains suppressed, then a fresh
		// request proves this agent and the same caller-owned executor are usable.
		if err := wsjson.Write(wireCtx, conn, request); err != nil {
			result <- err
			return
		}
		var duplicate protocol.Message
		if err := wsjson.Read(wireCtx, conn, &duplicate); err != nil || duplicate.Result == nil || duplicate.Result.Error == "" {
			result <- fmt.Errorf("abandoned wire ID was not rejected after reconnect: %v", err)
			return
		}
		request.ID, request.Tool = "new-inspection", "inspect"
		if err := wsjson.Write(wireCtx, conn, request); err != nil {
			result <- err
			return
		}
		var reply protocol.Message
		if err := wsjson.Read(wireCtx, conn, &reply); err != nil || reply.ID != request.ID || reply.Result == nil || reply.Result.Error != "" {
			result <- fmt.Errorf("fresh call after reconnect failed: %v", err)
			return
		}
		result <- nil
	}))
	defer srv.Close()
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, config.Agent{Version: 1, HubURL: srv.URL, DeviceID: "test", Token: "0123456789012345678901234567890123456789012"}, executor, protocol.Device{})
	}()
	defer func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("agent shutdown: %v", err)
			}
		case <-time.After(time.Second):
			t.Error("agent shutdown blocked")
		}
	}()
	select {
	case err := <-result:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(7 * time.Second):
		t.Fatal("disconnect/reconnect regression timed out")
	}
	if executor.calls.Load() != 2 || executor.closed.Load() {
		t.Fatalf("executions=%d executor closed=%v", executor.calls.Load(), executor.closed.Load())
	}
}
