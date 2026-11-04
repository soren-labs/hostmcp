package hub

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func callBounded(handler http.Handler, token string) int {
	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader("{}"))
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	return w.Code
}

type lockedLogBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedLogBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedLogBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func captureLog(t *testing.T) *lockedLogBuffer {
	t.Helper()
	var out lockedLogBuffer
	previous := log.Writer()
	log.SetOutput(&out)
	t.Cleanup(func() { log.SetOutput(previous) })
	return &out
}

// Handlers that wait for their request context to end, as a SEP-2575
// subscriptions/listen stream does when a listChanged or subscribe capability
// is advertised, must still release their concurrency slots. Four of them once
// locked a principal out until the Hub restarted.
func TestRequestDeadlineReleasesParkedSlots(t *testing.T) {
	h, _, _ := fixture(t)
	h.requestDeadline = 150 * time.Millisecond
	logs := captureLog(t)
	parked := h.boundedClient(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
		w.WriteHeader(http.StatusNoContent)
	}))
	fast := h.boundedClient(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	var workers sync.WaitGroup
	for range 4 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			callBounded(parked, aliceToken)
		}()
	}
	finished := make(chan struct{})
	go func() { workers.Wait(); close(finished) }()
	select {
	case <-finished:
	case <-time.After(3 * time.Second):
		t.Fatal("parked handlers kept their slots past the request deadline")
	}
	if status := callBounded(fast, aliceToken); status != http.StatusNoContent {
		t.Fatalf("request after parked handlers expired returned %d, want 204", status)
	}
	if !strings.Contains(logs.String(), "event=mcp_deadline") {
		t.Fatalf("deadline expiry was not logged: %q", logs.String())
	}
}

// Even a client that asks for every notification kind must get an immediate
// answer while the Hub advertises no listChanged or subscribe capability. This
// guards against a future prompt or resource silently re-enabling parked
// streams through the SDK's default capabilities.
func TestSubscriptionsListenForAllKindsReturnsImmediately(t *testing.T) {
	_, srv, _ := fixture(t)
	session := mcpClient(t, srv.URL, aliceToken, "2026-07-28")
	caps := session.InitializeResult().Capabilities
	if caps.Tools == nil || caps.Tools.ListChanged ||
		(caps.Prompts != nil && caps.Prompts.ListChanged) ||
		(caps.Resources != nil && (caps.Resources.ListChanged || caps.Resources.Subscribe)) {
		t.Fatalf("Hub must advertise static capabilities: %+v", caps)
	}
	body := `{"jsonrpc":"2.0","id":1,"method":"subscriptions/listen","params":{"notifications":{"toolsListChanged":true,"promptsListChanged":true,"resourcesListChanged":true,"resourceSubscriptions":["file:///etc/hostname"]},"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}}}`
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/mcp", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+aliceToken)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Mcp-Protocol-Version", "2026-07-28")
	req.Header.Set("Mcp-Method", "subscriptions/listen")
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("subscriptions/listen did not complete promptly: %v", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("subscriptions/listen stream stayed open: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("subscriptions/listen status=%d", resp.StatusCode)
	}
	if !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("subscriptions/listen did not return SSE: %s", data)
	}
	// A 200 response alone does not prove that the request was understood:
	// require the actual acknowledgement with every requested kind disabled.
	acked, completed := false, false
	for _, line := range strings.Split(string(data), "\n") {
		payload, ok := strings.CutPrefix(line, "data: ")
		if !ok {
			continue
		}
		var msg struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
			Result json.RawMessage `json:"result"`
			Error  json.RawMessage `json:"error"`
		}
		if err := json.Unmarshal([]byte(payload), &msg); err != nil || len(msg.Error) > 0 {
			t.Fatalf("invalid subscription response: %s (%v)", payload, err)
		}
		if msg.Method == "notifications/subscriptions/acknowledged" {
			var ack mcp.SubscriptionsAcknowledgedParams
			if err := json.Unmarshal(msg.Params, &ack); err != nil {
				t.Fatal(err)
			}
			n := ack.Notifications
			if n.ToolsListChanged || n.PromptsListChanged || n.ResourcesListChanged || len(n.ResourceSubscriptions) != 0 {
				t.Fatalf("unexpected active subscriptions: %+v", n)
			}
			acked = true
		}
		if string(msg.ID) == "1" && len(msg.Result) > 0 {
			completed = true
		}
	}
	if !acked || !completed {
		t.Fatalf("expected subscription acknowledgement and completion: %s", data)
	}
}

// Exercise the actual SDK cancellation path as well as the synthetic parked
// handler above. If a future capability enables subscriptions, the HTTP request
// and SDK session must still unwind when the Hub cancels their context.
func TestRequestDeadlineReleasesSDKSubscription(t *testing.T) {
	h, _, _ := fixture(t)
	h.requestDeadline = 300 * time.Millisecond
	server := mcp.NewServer(&mcp.Implementation{Name: "deadline-test", Version: "1"}, &mcp.ServerOptions{
		Capabilities: &mcp.ServerCapabilities{Tools: &mcp.ToolCapabilities{ListChanged: true}},
	})
	streamable := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, &mcp.StreamableHTTPOptions{
		Stateless: true, JSONResponse: true, PropagateRequestCancellation: true,
	})
	bounded := h.boundedClient(streamable)
	finished := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bounded.ServeHTTP(w, r)
		close(finished)
	}))
	t.Cleanup(srv.Close)
	body := `{"jsonrpc":"2.0","id":1,"method":"subscriptions/listen","params":{"notifications":{"toolsListChanged":true},"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}}}`
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/mcp", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+aliceToken)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Mcp-Protocol-Version", "2026-07-28")
	req.Header.Set("Mcp-Method", "subscriptions/listen")
	client := &http.Client{Timeout: 2 * time.Second}
	started := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	// At expiry the connection may close before a final SSE frame can be
	// flushed. What matters is the positive ACK, then prompt slot release.
	data, _ := io.ReadAll(resp.Body)
	if time.Since(started) > time.Second {
		t.Fatal("subscription outlived the Hub deadline and required client cancellation")
	}
	if resp.StatusCode != http.StatusOK || !bytes.Contains(data, []byte(`"toolsListChanged":true`)) {
		t.Fatalf("SDK did not open a real subscription: status=%d body=%s", resp.StatusCode, data)
	}
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("SDK subscription did not release its request after expiry")
	}
	assertClientSlotsReleased(t, h)
}

func TestRequestDeadlineBoundsLegacyToolCall(t *testing.T) {
	for _, version := range []string{"2025-06-18", "2025-11-25"} {
		t.Run(version, func(t *testing.T) {
			h, srv, _ := fixture(t)
			h.requestDeadline = 150 * time.Millisecond
			exec := &recordingExecutor{started: make(chan struct{}), release: make(chan struct{})}
			defer close(exec.release)
			startAgent(t, srv.URL, exec)
			eventually(t, func() bool { return online(h) })
			session := mcpClient(t, srv.URL, aliceToken, version)
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			callDone := make(chan struct{})
			go func() {
				defer close(callDone)
				_, _ = session.CallTool(ctx, &mcp.CallToolParams{Name: "echo", Arguments: map[string]any{"device": "dev1", "text": "wait"}})
			}()
			t.Cleanup(func() {
				select {
				case <-callDone:
				case <-time.After(3 * time.Second):
					t.Error("client call did not stop")
				}
			})
			select {
			case <-exec.started:
			case <-time.After(time.Second):
				t.Fatal("tool was not dispatched")
			}
			// The SDK intentionally detaches legacy handler contexts from HTTP
			// cancellation. Its session drain must still obey the Hub deadline,
			// instead of retaining the slot until the separate 45s Call timeout.
			until := time.Now().Add(time.Second)
			for len(h.requests) != 0 && time.Now().Before(until) {
				time.Sleep(10 * time.Millisecond)
			}
			if n := len(h.requests); n != 0 {
				t.Fatalf("legacy SDK session retained %d request slot past the deadline", n)
			}
		})
	}
}

func TestRequestDeadlinePreservesDispatchOutcome(t *testing.T) {
	h, srv, _ := fixture(t)
	exec := &recordingExecutor{started: make(chan struct{}), release: make(chan struct{})}
	defer close(exec.release)
	startAgent(t, srv.URL, exec)
	eventually(t, func() bool { return online(h) })
	args := json.RawMessage(`{"text":"deadline"}`)
	expired := context.WithValue(context.Background(), requestDeadlineKey{}, time.Now().Add(-time.Second))
	_, err := h.Call(expired, "alice", "dev1", "echo", args)
	if err == nil || !strings.Contains(err.Error(), "command was not dispatched") || exec.calls.Load() != 0 {
		t.Fatalf("expired request must never dispatch: calls=%d err=%v", exec.calls.Load(), err)
	}
	if !online(h) {
		t.Fatal("rejecting an expired request disconnected the healthy agent")
	}
	admitted := context.WithValue(context.Background(), requestDeadlineKey{}, time.Now().Add(150*time.Millisecond))
	_, err = h.Call(admitted, "alice", "dev1", "echo", args)
	if err == nil || !strings.Contains(err.Error(), "execution outcome unknown") || exec.calls.Load() != 1 {
		t.Fatalf("dispatched request must retain unknown-outcome semantics: calls=%d err=%v", exec.calls.Load(), err)
	}
	// The first executor call is still waiting on release. Its response timeout
	// must not close the agent connection or stop it from serving another call.
	if !online(h) {
		t.Fatal("response deadline disconnected the agent")
	}
	if _, err := h.Call(context.Background(), "alice", "dev1", "echo", args); err != nil {
		t.Fatalf("agent could not serve another request after deadline: %v", err)
	}
	if exec.calls.Load() != 2 {
		t.Fatalf("unexpected execution count: %d", exec.calls.Load())
	}
}

// One noisy event kind, such as a stale agent retrying a revoked credential,
// must not hide other kinds, and suppressed volume must stay visible.
func TestAuditSamplingIsPerKindAndCountsSuppressed(t *testing.T) {
	h, _, _ := fixture(t)
	logs := captureLog(t)
	h.auditMu.Lock()
	h.auditEvery = time.Hour
	h.auditMu.Unlock()
	for range 3 {
		h.auditDenied("agent", "invalid_credentials", "device", "renamed-node")
	}
	h.audit("mcp_busy", "mcp", "principal_busy")
	out := logs.String()
	if strings.Count(out, "event=auth_denied") != 1 {
		t.Fatalf("repeated denial was not sampled: %q", out)
	}
	if !strings.Contains(out, `device="renamed-node"`) {
		t.Fatalf("agent denial omitted the node ID: %q", out)
	}
	if !strings.Contains(out, `event=mcp_busy endpoint="mcp" reason="principal_busy"`) {
		t.Fatalf("capacity rejection was hidden by agent denials: %q", out)
	}
	h.auditMu.Lock()
	h.auditEvery = 0
	h.auditMu.Unlock()
	h.auditDenied("agent", "invalid_credentials", "device", "renamed-node")
	if !strings.Contains(logs.String(), "suppressed=2") {
		t.Fatalf("suppressed count was not reported: %q", logs.String())
	}
}

func TestAgentDenialLogsOnlyValidNodeIDs(t *testing.T) {
	_, srv, _ := fixture(t)
	logs := captureLog(t)
	req, err := http.NewRequest(http.MethodGet, srv.URL+"/agent/bad%20id%22", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+strings.Repeat("x", 43))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	out := logs.String()
	if !strings.Contains(out, `device="invalid"`) || strings.Contains(out, "bad id") {
		t.Fatalf("unvalidated node ID reached the log: %q", out)
	}
}
