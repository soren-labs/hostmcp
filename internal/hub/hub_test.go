package hub

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/soren-labs/hostmcp/internal/agent"
	"github.com/soren-labs/hostmcp/internal/config"
	"github.com/soren-labs/hostmcp/internal/protocol"
)

const aliceToken = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
const bobToken = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
const deviceToken = "ddddddddddddddddddddddddddddddddddddddddddd"

type recordingExecutor struct {
	calls      atomic.Int64
	mu         sync.Mutex
	principals []string
	started    chan struct{}
	release    chan struct{}
}

func (e *recordingExecutor) Close() error { return nil }
func (e *recordingExecutor) Call(ctx context.Context, p, tool string, args json.RawMessage) (protocol.Result, error) {
	n := e.calls.Add(1)
	e.mu.Lock()
	e.principals = append(e.principals, p)
	e.mu.Unlock()
	if e.started != nil && n == 1 {
		close(e.started)
		select {
		case <-e.release:
		case <-ctx.Done():
			return protocol.Result{}, ctx.Err()
		}
	}
	return protocol.JSONResult(map[string]any{"principal": p, "tool": tool, "call": n, "args": args}), nil
}
func specs() []protocol.Tool {
	return []protocol.Tool{{Name: "echo", Description: "test routing", ReadOnly: true, InputSchema: map[string]any{
		"type": "object", "properties": map[string]any{"text": map[string]any{"type": "string"}}, "required": []string{"text"}, "additionalProperties": false,
	}}}
}

func fixture(t *testing.T) (*Hub, *httptest.Server, *config.Store) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "hub.json")
	cfg := config.Hub{Version: config.FormatVersion, Listen: "127.0.0.1:0", PublicURL: "http://127.0.0.1", Clients: []config.Client{
		{ID: "alice", TokenHash: config.Hash(aliceToken), Devices: []string{"dev1"}},
		{ID: "bob", TokenHash: config.Hash(bobToken), Devices: []string{"dev2"}},
	}, Devices: []config.Device{{ID: "dev1", TokenHash: config.Hash(deviceToken)}, {ID: "dev2", TokenHash: config.Hash(strings.Repeat("e", 43))}}}
	if err := config.WriteJSON(path, cfg, false); err != nil {
		t.Fatal(err)
	}
	store := config.NewStore(path)
	h, err := New(store, specs())
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h.Handler())
	t.Cleanup(func() { h.Close(); srv.Close() })
	return h, srv, store
}
func startAgent(t *testing.T, url string, exec protocol.Executor) context.CancelFunc {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- agent.Run(ctx, config.Agent{Version: 1, HubURL: url, DeviceID: "dev1", Token: deviceToken}, exec, protocol.Device{ID: "dev1", Hostname: "test", OS: "test", Username: "test-user"})
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Logf("agent stopped: %v", err)
			}
		case <-time.After(3 * time.Second):
			t.Error("agent shutdown blocked")
		}
	})
	return cancel
}
func eventually(t *testing.T, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(6 * time.Second)
	for time.Now().Before(deadline) {
		if fn() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition did not become true")
}
func online(h *Hub) bool {
	list, err := h.ListDevices(context.Background(), "alice")
	return err == nil && len(list) == 1 && list[0].Online
}

type bearerTransport struct{ token string }

func (b bearerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	cloned := r.Clone(r.Context())
	cloned.Header = r.Header.Clone()
	cloned.Header.Set("Authorization", "Bearer "+b.token)
	return http.DefaultTransport.RoundTrip(cloned)
}
func mcpClient(t *testing.T, url, token, version string) *mcp.ClientSession {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c := mcp.NewClient(&mcp.Implementation{Name: "hostmcp-test", Version: "1"}, nil)
	s, err := c.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: url + "/mcp", HTTPClient: &http.Client{Transport: bearerTransport{token}}, MaxRetries: -1, DisableStandaloneSSE: true}, &mcp.ClientSessionOptions{ProtocolVersion: version})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestHTTPAuthenticationAndOrigin(t *testing.T) {
	_, srv, _ := fixture(t)
	cases := []struct {
		name, token, origin, host string
		duplicate                 bool
		want                      int
	}{
		{"missing", "", "", "", false, 401},
		{"wrong", "wrong", "", "", false, 401},
		{"bad-origin", aliceToken, "https://evil.example", "", false, 403},
		{"null-origin", aliceToken, "null", "", false, 403},
		{"bad-host", aliceToken, "", "evil.example", false, 403},
		{"duplicate-authorization", aliceToken, "", "", true, 401},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req, _ := http.NewRequest(http.MethodPost, srv.URL+"/mcp", strings.NewReader("{}"))
			req.Header.Set("Content-Type", "application/json")
			if tc.token != "" {
				req.Header.Set("Authorization", "Bearer "+tc.token)
			}
			if tc.duplicate {
				req.Header.Add("Authorization", "Bearer "+bobToken)
			}
			if tc.origin != "" {
				req.Header.Set("Origin", tc.origin)
			}
			if tc.host != "" {
				req.Host = tc.host
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != tc.want {
				b, _ := io.ReadAll(resp.Body)
				t.Fatalf("got %d want %d: %s", resp.StatusCode, tc.want, b)
			}
		})
	}
}

func TestSDKProtocolVersionsAndScopes(t *testing.T) {
	h, srv, _ := fixture(t)
	exec := &recordingExecutor{}
	startAgent(t, srv.URL, exec)
	eventually(t, func() bool { return online(h) })
	for _, version := range []string{"2025-06-18", "2025-11-25", "2026-07-28"} {
		t.Run(version, func(t *testing.T) {
			session := mcpClient(t, srv.URL, aliceToken, version)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			tools, err := session.ListTools(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			if len(tools.Tools) != 2 {
				t.Fatalf("tools=%d", len(tools.Tools))
			}
			result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "echo", Arguments: map[string]any{"device": "dev1", "text": "hello"}})
			if err != nil || result.IsError {
				t.Fatalf("call: result=%+v err=%v", result, err)
			}
			raw, _ := json.Marshal(result.StructuredContent)
			if !bytes.Contains(raw, []byte("\"principal\":\""+clientPrincipal(config.Client{ID: "alice", TokenHash: config.Hash(aliceToken)})+"\"")) {
				t.Fatalf("wrong principal: %s", raw)
			}
			// Required argument validation happens before dispatch.
			before := exec.calls.Load()
			bad, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "echo", Arguments: map[string]any{"device": "dev1"}})
			if err != nil || !bad.IsError || before != exec.calls.Load() {
				t.Fatalf("schema validation failed: %+v %v", bad, err)
			}
		})
	}
	bob := mcpClient(t, srv.URL, bobToken, "2026-07-28")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	before := exec.calls.Load()
	denied, err := bob.CallTool(ctx, &mcp.CallToolParams{Name: "echo", Arguments: map[string]any{"device": "dev1", "text": "forbidden"}})
	if err != nil || !denied.IsError || exec.calls.Load() != before {
		t.Fatalf("device scope was not enforced: %+v %v", denied, err)
	}
	listed, err := bob.CallTool(ctx, &mcp.CallToolParams{Name: "list_devices", Arguments: map[string]any{}})
	if err != nil || listed.IsError {
		t.Fatalf("list: %+v %v", listed, err)
	}
	raw, _ := json.Marshal(listed.StructuredContent)
	if bytes.Contains(raw, []byte("dev1")) || !bytes.Contains(raw, []byte("dev2")) {
		t.Fatalf("inventory leaked device: %s", raw)
	}
}

func TestDisconnectDoesNotReplayAndRevocationClosesConnection(t *testing.T) {
	h, srv, store := fixture(t)
	exec := &recordingExecutor{started: make(chan struct{}), release: make(chan struct{})}
	startAgent(t, srv.URL, exec)
	eventually(t, func() bool { return online(h) })
	done := make(chan error, 1)
	go func() {
		_, err := h.Call(context.Background(), "alice", "dev1", "echo", json.RawMessage(`{"text":"side-effect"}`))
		done <- err
	}()
	select {
	case <-exec.started:
	case <-time.After(3 * time.Second):
		t.Fatal("tool did not start")
	}
	h.mu.RLock()
	old := h.connections["dev1"]
	h.mu.RUnlock()
	old.close()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "outcome unknown") {
			t.Fatalf("unexpected outcome: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("disconnect did not fail pending request")
	}
	close(exec.release)
	eventually(t, func() bool {
		h.mu.RLock()
		defer h.mu.RUnlock()
		return h.connections["dev1"] != nil && h.connections["dev1"] != old
	})
	if exec.calls.Load() != 1 {
		t.Fatalf("side effect replayed %d times", exec.calls.Load())
	}
	result, err := h.Call(context.Background(), "alice", "dev1", "echo", json.RawMessage(`{"text":"after reconnect"}`))
	if err != nil || result.Error != "" {
		t.Fatalf("reconnect did not route: %+v %v", result, err)
	}
	if err := store.Update(func(c *config.Hub) error { c.Devices = c.Devices[1:]; return nil }); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool { h.mu.RLock(); defer h.mu.RUnlock(); return h.connections["dev1"] == nil })
	if _, err := h.Call(context.Background(), "alice", "dev1", "echo", json.RawMessage("{}")); err == nil {
		t.Fatal("revoked device was callable")
	}
}

func TestEnrollmentIsSingleUseAndDeviceTokenIsNotClientToken(t *testing.T) {
	_, srv, store := fixture(t)
	invite, err := store.InviteDevice("new-device", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(map[string]string{"device_id": invite.DeviceID, "token": invite.Token})
	resp, err := http.Post(srv.URL+"/enroll", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	var cfg config.Agent
	if err := json.NewDecoder(resp.Body).Decode(&cfg); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 || cfg.DeviceID != "new-device" || !store.AuthDevice(cfg.DeviceID, cfg.Token) {
		t.Fatalf("enrollment failed: %d", resp.StatusCode)
	}
	second, err := http.Post(srv.URL+"/enroll", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	second.Body.Close()
	if second.StatusCode != 401 {
		t.Fatalf("invitation reused: %d", second.StatusCode)
	}
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/mcp", strings.NewReader("{}"))
	req.Header.Set("Authorization", "Bearer "+cfg.Token)
	invalid, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	invalid.Body.Close()
	if invalid.StatusCode != 401 {
		t.Fatalf("device credential accepted as client: %d", invalid.StatusCode)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, strings.Replace(srv.URL, "http:", "ws:", 1)+"/agent/dev1", &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": []string{"Bearer " + aliceToken}}})
	if conn != nil {
		conn.CloseNow()
	}
	if err == nil {
		t.Fatal("client credential accepted as device")
	}
}

func TestHostAuthorityIncludesPort(t *testing.T) {
	cfg := config.Hub{Listen: "127.0.0.1:8787", PublicURL: "https://mcp.example.com"}
	for _, host := range []string{"mcp.example.com", "mcp.example.com:443", "127.0.0.1:8787", "localhost:8787"} {
		if !hostAllowed(host, cfg) {
			t.Errorf("expected allowed: %s", host)
		}
	}
	for _, host := range []string{"mcp.example.com:1234", "evil.example", "127.0.0.1:9999", "mcp.example.com.evil:443"} {
		if hostAllowed(host, cfg) {
			t.Errorf("expected denied: %s", host)
		}
	}
}

func TestHeartbeatMarksUnresponsiveDeviceOffline(t *testing.T) {
	h, srv, _ := fixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, strings.Replace(srv.URL, "http:", "ws:", 1)+"/agent/dev1", &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": []string{"Bearer " + deviceToken}}})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()
	if err := wsjson.Write(ctx, conn, protocol.Message{Type: "hello", Version: 1, Device: &protocol.Device{ID: "dev1"}}); err != nil {
		t.Fatal(err)
	}
	var welcome protocol.Message
	if err := wsjson.Read(ctx, conn, &welcome); err != nil {
		t.Fatal(err)
	}
	h.mu.RLock()
	d := h.connections["dev1"]
	h.mu.RUnlock()
	if d == nil {
		t.Fatal("device did not register")
	}
	// Stop reading the device socket: coder/websocket cannot answer a Ping
	// without a read loop, simulating an alive TCP connection with a dead peer.
	go h.heartbeat(d, 10*time.Millisecond, 30*time.Millisecond)
	eventually(t, func() bool { return !online(h) })
}

func TestSameMachineAccountsHaveIndependentNodeScopes(t *testing.T) {
	h, srv, store := fixture(t)
	if err := store.Update(func(c *config.Hub) error {
		c.Devices[0].ExpectedUser = "worker"
		c.Devices[1].ExpectedUser = "root"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	workers := &recordingExecutor{}
	roots := &recordingExecutor{}
	for _, entry := range []struct {
		id, token, user, uid string
		executor             *recordingExecutor
	}{
		{"dev1", deviceToken, "worker", "1000", workers},
		{"dev2", strings.Repeat("e", 43), "root", "0", roots},
	} {
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() {
			done <- agent.Run(ctx, config.Agent{Version: 1, HubURL: srv.URL, DeviceID: entry.id, Token: entry.token, ExpectedUser: entry.user}, entry.executor, protocol.Device{ID: entry.id, Hostname: "same-machine", OS: "linux", Username: entry.user, UID: entry.uid, EUID: entry.uid})
		}()
		t.Cleanup(func() {
			cancel()
			select {
			case <-done:
			case <-time.After(3 * time.Second):
				t.Error("account node did not stop")
			}
		})
	}
	eventually(t, func() bool {
		a, errA := h.ListDevices(context.Background(), "alice")
		b, errB := h.ListDevices(context.Background(), "bob")
		return errA == nil && errB == nil && len(a) == 1 && len(b) == 1 && a[0].Online && b[0].Online
	})
	alice := mcpClient(t, srv.URL, aliceToken, "2026-07-28")
	bob := mcpClient(t, srv.URL, bobToken, "2026-07-28")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, tc := range []struct {
		session         *mcp.ClientSession
		allowed, denied string
	}{{alice, "dev1", "dev2"}, {bob, "dev2", "dev1"}} {
		result, err := tc.session.CallTool(ctx, &mcp.CallToolParams{Name: "echo", Arguments: map[string]any{"device": tc.allowed, "text": "allowed"}})
		if err != nil || result.IsError {
			t.Fatalf("allowed account call: %+v %v", result, err)
		}
		beforeWorker, beforeRoot := workers.calls.Load(), roots.calls.Load()
		result, err = tc.session.CallTool(ctx, &mcp.CallToolParams{Name: "echo", Arguments: map[string]any{"device": tc.denied, "text": "denied"}})
		if err != nil || !result.IsError || workers.calls.Load() != beforeWorker || roots.calls.Load() != beforeRoot {
			t.Fatalf("unauthorized account was dispatched: %+v %v", result, err)
		}
	}
	devices, err := h.ListDevices(ctx, "bob")
	if err != nil || len(devices) != 1 || devices[0].Username != "root" || devices[0].EUID != "0" || devices[0].ExpectedUser != "root" {
		t.Fatalf("account inventory missing: %+v %v", devices, err)
	}
	// A policy edit must take effect at dispatch without waiting for the once-per-
	// second connection sweep; it is still only an account deployment binding.
	if err := store.Update(func(c *config.Hub) error {
		c.Devices[1].ExpectedUser = "worker"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	before := roots.calls.Load()
	if _, err := h.Call(ctx, "bob", "dev2", "echo", json.RawMessage(`{"text":"stale binding"}`)); err == nil || roots.calls.Load() != before {
		t.Fatal("changed account binding still dispatched")
	}
}

func TestHubRejectsMismatchedAccountEvenIfNodeOmitsLocalBinding(t *testing.T) {
	h, srv, store := fixture(t)
	if err := store.Update(func(c *config.Hub) error {
		c.Devices[0].ExpectedUser = "root"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, strings.Replace(srv.URL, "http:", "ws:", 1)+"/agent/dev1", &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": []string{"Bearer " + deviceToken}}})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()
	if err := wsjson.Write(ctx, conn, protocol.Message{Type: "hello", Version: 1, Device: &protocol.Device{ID: "dev1", Username: "worker", UID: "1000", EUID: "1000"}}); err != nil {
		t.Fatal(err)
	}
	var welcome protocol.Message
	err = wsjson.Read(ctx, conn, &welcome)
	if websocket.CloseStatus(err) != websocket.StatusPolicyViolation || online(h) {
		t.Fatalf("mismatched account welcomed: %+v %v", welcome, err)
	}
}
