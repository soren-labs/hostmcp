package hub

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/soren-labs/hostmcp/internal/config"
)

// Eight long polls from a single principal once blocked all other MCP
// clients and enrollment. Four concurrent calls now exhaust only that
// credential's share, without affecting a second credential or enrollment.
func TestReviewIndependentClientAndEnrollmentCapacity(t *testing.T) {
	h, _, _ := fixture(t)
	started := make(chan struct{}, 4)
	release := make(chan struct{})
	var workers sync.WaitGroup
	defer func() { close(release); workers.Wait() }()
	slow := h.boundedClient(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started <- struct{}{}
		<-release
		w.WriteHeader(204)
	}))
	fast := h.boundedClient(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(204)
	}))
	call := func(handler http.Handler, token string) int {
		req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader("{}"))
		req.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		return w.Code
	}
	for range 4 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			if status := call(slow, aliceToken); status != 204 {
				t.Errorf("initial call returned %d", status)
			}
		}()
	}
	for range 4 {
		select {
		case <-started:
		case <-time.After(3 * time.Second):
			t.Fatal("four calls did not occupy their own client slots")
		}
	}
	if status := call(fast, aliceToken); status != 503 {
		t.Fatalf("excess request from same credential=%d, want 503", status)
	}
	if status := call(fast, bobToken); status != 204 {
		t.Fatalf("independent credential was blocked: %d", status)
	}
	if status := call(fast, "invalid"); status != 401 {
		t.Fatalf("invalid token was not rejected before admission: %d", status)
	}
	enroll := h.bounded(h.enrollments, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }))
	w := httptest.NewRecorder()
	enroll.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/enroll", nil))
	if w.Code != 204 {
		t.Fatalf("enrollment was blocked by MCP load: %d", w.Code)
	}
}

func TestReviewConfigReadFailureKeepsExistingAgent(t *testing.T) {
	h, srv, store := fixture(t)
	startAgent(t, srv.URL, &recordingExecutor{})
	eventually(t, func() bool { return online(h) })
	h.mu.RLock()
	old := h.connections["dev1"]
	h.mu.RUnlock()
	if err := os.Chmod(store.Path, 0644); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(store.Path, 0600) })
	// A bad config must deny new tool dispatch but not forcibly disconnect the
	// already-connected agent or forget its inventory entry.
	time.Sleep(1200 * time.Millisecond)
	h.mu.RLock()
	current, exists := h.connections["dev1"]
	h.mu.RUnlock()
	if !exists || current != old {
		t.Fatal("temporary config read error disconnected the agent")
	}
	if _, err := h.Call(context.Background(), "alice", "dev1", "echo", json.RawMessage(`{"text":"fail closed"}`)); err == nil {
		t.Fatal("tool dispatch was authorized when the config was unreadable")
	}
	if err := os.Chmod(store.Path, 0600); err != nil {
		t.Fatal(err)
	}
	if result, err := h.Call(context.Background(), "alice", "dev1", "echo", json.RawMessage(`{"text":"restored"}`)); err != nil || result.Error != "" {
		t.Fatalf("restored config did not resume without reconnect: result=%+v err=%v", result, err)
	}
}

func TestReviewTokenRecreationCannotInheritProcessPrincipal(t *testing.T) {
	h, _, store := fixture(t)
	old := clientPrincipal(config.Client{ID: "alice", TokenHash: config.Hash(aliceToken)})
	if _, err := h.client(old); err != nil {
		t.Fatal(err)
	}
	newToken := strings.Repeat("n", 43)
	if err := store.Update(func(cfg *config.Hub) error {
		cfg.Clients[0].TokenHash = config.Hash(newToken)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	newPrincipal := clientPrincipal(config.Client{ID: "alice", TokenHash: config.Hash(newToken)})
	if old == newPrincipal {
		t.Fatal("credential rotation did not change the process owner identity")
	}
	if _, err := h.client(old); err == nil {
		t.Fatal("revoked credential still owns sessions")
	}
	if _, err := h.client(newPrincipal); err != nil {
		t.Fatalf("new credential is not authorized: %v", err)
	}
}
