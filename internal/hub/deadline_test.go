package hub

import (
	"bytes"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func callBounded(handler http.Handler, token string) int {
	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader("{}"))
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	return w.Code
}

func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var out bytes.Buffer
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
	if _, err := io.ReadAll(resp.Body); err != nil {
		t.Fatalf("subscriptions/listen stream stayed open: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("subscriptions/listen status=%d", resp.StatusCode)
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
