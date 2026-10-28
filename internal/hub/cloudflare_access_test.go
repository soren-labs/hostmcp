package hub

import (
	"bytes"
	"context"
	"encoding/json"
	"log"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/golang-jwt/jwt/v5"
	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/soren-labs/hostmcp/internal/cloudflareaccess"
	"github.com/soren-labs/hostmcp/internal/config"
	"github.com/soren-labs/hostmcp/internal/host"
	"github.com/soren-labs/hostmcp/internal/protocol"
	"github.com/soren-labs/hostmcp/internal/testutil"
)

const accessResource = "https://mcp.example/mcp"
const initializeBody = `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"test","version":"1"}}}`

func accessFixture(t *testing.T) (*Hub, *config.Store, *testutil.AccessIssuer) {
	t.Helper()
	i := testutil.NewAccessIssuer(t)
	i.Install(t)
	cfg := config.Hub{Version: 1, Listen: "127.0.0.1:0", PublicURL: "https://mcp.example",
		Clients:          []config.Client{{ID: "owner", TokenHash: config.Hash(aliceToken), Devices: []string{"*"}}},
		Devices:          []config.Device{{ID: "allowed", TokenHash: config.Hash(deviceToken)}, {ID: "root", TokenHash: config.Hash(deviceToken)}},
		CloudflareAccess: &config.CloudflareAccess{TeamDomain: i.URL, Audience: i.Audience, Principals: []config.CloudflareAccessPrincipal{{Email: "Owner@Example.com", Devices: []string{"allowed"}}}},
	}
	file := filepath.Join(t.TempDir(), "hub.json")
	if err := config.WriteJSON(file, cfg, false); err != nil {
		t.Fatal(err)
	}
	store := config.NewStore(file)
	h, err := New(store, specs())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { h.Close() })
	return h, store, i
}

func accessRequest(raw, bearerToken string) *http.Request {
	r := testutil.BearerRequest("POST", accessResource, bearerToken, initializeBody)
	if raw != "" {
		r.Header.Set(cloudflareaccess.Header, raw)
	}
	return r
}

func TestAccessAuthenticationAndLegacyPaths(t *testing.T) {
	h, store, i := accessFixture(t)
	claims := i.Claims("OWNER@EXAMPLE.COM")
	claims["sub"] = "owner"
	valid := i.Sign(t, claims)
	other := testutil.NewAccessIssuer(t)
	for _, tc := range []struct {
		name, raw, token string
		code             int
	}{
		{"valid-missing-authorization", valid, "", 200}, {"valid-opaque-bearer", valid, "oauth:opaque-client-token", 200},
		{"no-header", "", "", 401}, {"legacy", "", aliceToken, 200}, {"device-is-not-client", "", deviceToken, 401},
		{"forged", other.Sign(t, claims), "", 401}, {"forged-with-legacy", other.Sign(t, claims), aliceToken, 401},
		{"malformed-with-legacy", "garbage", aliceToken, 401}, {"synthetic-is-not-credential", "", "access-assertion-validated", 401},
		{"assertion-in-bearer-is-not-credential", "", valid, 401},
		{"oversized", strings.Repeat("x", (16<<10)+1), "", 401},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			h.Handler().ServeHTTP(w, accessRequest(tc.raw, tc.token))
			if w.Code != tc.code {
				t.Fatalf("status=%d want=%d", w.Code, tc.code)
			}
		})
	}
	for _, edit := range []func(jwt.MapClaims){
		func(c jwt.MapClaims) { c["exp"] = 1 }, func(c jwt.MapClaims) { c["aud"] = "different" }, func(c jwt.MapClaims) { c["iss"] = "https://other.cloudflareaccess.com" },
		func(c jwt.MapClaims) { c["email"] = "unknown@example.com" }, func(c jwt.MapClaims) { c["email"] = "owner" },
	} {
		c := i.Claims("owner@example.com")
		edit(c)
		w := httptest.NewRecorder()
		h.Handler().ServeHTTP(w, accessRequest(i.Sign(t, c), aliceToken))
		want := 401
		if c["email"] == "unknown@example.com" {
			want = 403
		}
		if w.Code != want {
			t.Fatalf("invalid assertion with legacy: %d want %d", w.Code, want)
		}
	}
	for _, edit := range []func(*http.Request){
		func(r *http.Request) { r.Header.Add(cloudflareaccess.Header, valid) },
		func(r *http.Request) { r.Header.Set(cloudflareaccess.Header, "") },
		func(r *http.Request) { r.Header.Set(cloudflareaccess.Header, valid+","+valid) },
		func(r *http.Request) { r.Header.Add("Authorization", "Bearer "+aliceToken) },
	} {
		r := accessRequest(valid, aliceToken)
		edit(r)
		w := httptest.NewRecorder()
		h.Handler().ServeHTTP(w, r)
		if w.Code != 401 {
			t.Fatal("empty/duplicate header accepted")
		}
	}
	for _, edit := range []func(*http.Request){
		func(r *http.Request) { r.Host = "evil.example"; r.Header.Set("X-Forwarded-Host", "mcp.example") },
		func(r *http.Request) { r.Header.Set("Origin", "https://evil.example") },
		func(r *http.Request) {
			r.Header.Add("Origin", "https://mcp.example")
			r.Header.Add("Origin", "https://mcp.example")
		},
	} {
		r := accessRequest(valid, "")
		edit(r)
		w := httptest.NewRecorder()
		h.Handler().ServeHTTP(w, r)
		if w.Code != 403 {
			t.Fatal("Access bypassed host/origin protection")
		}
	}
	for _, path := range []string{"https://mcp.example/agent/allowed", "https://mcp.example/enroll"} {
		method, body := "GET", ""
		if strings.HasSuffix(path, "/enroll") {
			method, body = "POST", `{"device_id":"new-node","token":""}`
		}
		r := testutil.BearerRequest(method, path, valid, body)
		r.Header.Set(cloudflareaccess.Header, valid)
		w := httptest.NewRecorder()
		h.Handler().ServeHTTP(w, r)
		if w.Code != 401 {
			t.Fatal("assertion authorized agent or enrollment")
		}
	}
	invite, err := store.InviteDevice("new-node", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(map[string]string{"device_id": "new-node", "token": invite.Token})
	r := testutil.BearerRequest("POST", "https://mcp.example/enroll", "", string(body))
	r.Header.Set(cloudflareaccess.Header, "invalid-ignored-on-enrollment")
	w := httptest.NewRecorder()
	h.Handler().ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatal("Access interfered with valid invitation credential")
	}
}

func TestAccessSDKWrapperClonesRequest(t *testing.T) {
	h, _, i := accessFixture(t)
	raw := i.Sign(t, i.Claims("owner@example.com"))
	for _, token := range []string{"", "oauth:opaque-token"} {
		r := accessRequest(raw, token)
		original := r.Header.Get("Authorization")
		called := false
		next := auth.RequireBearerToken(cachedTokenInfo, nil)(http.HandlerFunc(func(_ http.ResponseWriter, inner *http.Request) {
			called = true
			info := auth.TokenInfoFromContext(inner.Context())
			if inner == r || inner.Header.Get("Authorization") != "Bearer access-assertion-validated" || inner.Header.Get(cloudflareaccess.Header) != "" || info == nil || info.UserID != h.access.Principal(accessResource, "owner@example.com", "access-subject") {
				t.Fatal("SDK identity/clone missing")
			}
		}))
		w := httptest.NewRecorder()
		h.boundedClient(next).ServeHTTP(w, r)
		if !called || r.Header.Get("Authorization") != original || r.Header.Get(cloudflareaccess.Header) != raw {
			t.Fatal("original request was changed")
		}
	}
	if _, err := cachedTokenInfo(context.Background(), "access-assertion-validated", accessRequest("", "access-assertion-validated")); err == nil {
		t.Fatal("SDK sentinel bypassed validation context")
	}
}

func TestAccessSDKAuthenticationMatrix(t *testing.T) {
	h, _, i := accessFixture(t)
	other := testutil.NewAccessIssuer(t)
	valid := i.Sign(t, i.Claims("owner@example.com"))
	invalid := func(edit func(jwt.MapClaims)) string {
		c := i.Claims("owner@example.com")
		edit(c)
		return i.Sign(t, c)
	}
	for _, tc := range []struct {
		name, assertion, token string
		duplicate              bool
		devices                int
	}{
		{"missing", "", "", false, 0},
		{"forged", other.Sign(t, i.Claims("owner@example.com")), "", false, 0},
		{"forged-with-legacy", other.Sign(t, i.Claims("owner@example.com")), aliceToken, false, 0},
		{"expired", invalid(func(c jwt.MapClaims) { c["exp"] = 1 }), "", false, 0},
		{"wrong-aud", invalid(func(c jwt.MapClaims) { c["aud"] = "other" }), "", false, 0},
		{"wrong-iss", invalid(func(c jwt.MapClaims) { c["iss"] = "https://other.cloudflareaccess.com" }), "", false, 0},
		{"unknown-email", invalid(func(c jwt.MapClaims) { c["email"] = "unknown@example.com" }), aliceToken, false, 0},
		{"duplicate", valid, aliceToken, true, 0},
		{"sentinel", "", "access-assertion-validated", false, 0},
		{"legacy", "", aliceToken, false, 2},
		{"access-with-legacy", valid, aliceToken, false, 1},
		{"access-with-opaque", valid, "oauth:opaque-fixture", false, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			transport := testutil.HandlerTransport{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if tc.token != "" {
					r.Header.Set("Authorization", "Bearer "+tc.token)
				}
				if tc.assertion != "" {
					r.Header.Set(cloudflareaccess.Header, tc.assertion)
				}
				if tc.duplicate {
					r.Header.Add(cloudflareaccess.Header, tc.assertion)
				}
				h.Handler().ServeHTTP(w, r)
			})}
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			client := mcp.NewClient(&mcp.Implementation{Name: "isolated-access-auth-test", Version: "1"}, nil)
			session, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: accessResource, HTTPClient: &http.Client{Transport: transport}, DisableStandaloneSSE: true, MaxRetries: -1}, nil)
			if tc.devices == 0 {
				if err == nil {
					session.Close()
					t.Fatal("SDK accepted invalid credentials")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			defer session.Close()
			result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "list_devices", Arguments: map[string]any{}})
			if err != nil || result.IsError {
				t.Fatal("SDK inventory failed", err)
			}
			var payload struct {
				Devices []protocol.Device `json:"devices"`
			}
			b, _ := json.Marshal(result.StructuredContent)
			if err = json.Unmarshal(b, &payload); err != nil || len(payload.Devices) != tc.devices {
				t.Fatal("SDK identity lost node scope", err)
			}
			if tc.devices == 1 && payload.Devices[0].ID != "allowed" {
				t.Fatal("Access inherited legacy owner grant")
			}
		})
	}
}

func TestAccessSDKClientAndLegacyAgentInMemory(t *testing.T) {
	h, store, i := accessFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	memory := testutil.NewMemoryServer(t, h.Handler())
	// An unchanged wire-v1 agent still authenticates only with its device token,
	// even when an invalid Access assertion happens to accompany the handshake.
	conn, _, err := websocket.Dial(ctx, "ws://127.0.0.1/agent/allowed", &websocket.DialOptions{HTTPClient: &http.Client{Transport: memory.Transport}, HTTPHeader: http.Header{"Authorization": {"Bearer " + deviceToken}, cloudflareaccess.Header: {"invalid-ignored-on-agent"}}})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()
	if err = wsjson.Write(ctx, conn, map[string]any{"type": "hello", "version": 1, "device": map[string]any{"id": "allowed", "version": "0.2.2", "username": "fixture"}}); err != nil {
		t.Fatal(err)
	}
	var welcome protocol.Message
	if err = wsjson.Read(ctx, conn, &welcome); err != nil || welcome.Type != "welcome" {
		t.Fatal("legacy handshake", err)
	}
	done := make(chan error, 1)
	go func() {
		var request protocol.Message
		if err := wsjson.Read(ctx, conn, &request); err != nil {
			done <- err
			return
		}
		done <- wsjson.Write(ctx, conn, protocol.Message{Type: "result", ID: request.ID, Result: &protocol.Result{Data: protocol.JSONResult(map[string]string{"principal": request.Principal}).Data}})
	}()
	raw := i.Sign(t, i.Claims("OWNER@EXAMPLE.COM"))
	transport := testutil.HandlerTransport{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Header.Set(cloudflareaccess.Header, raw)
		h.Handler().ServeHTTP(w, r)
	})}
	client := mcp.NewClient(&mcp.Implementation{Name: "isolated-access-test", Version: "1"}, nil)
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: accessResource, HTTPClient: &http.Client{Transport: transport}, DisableStandaloneSSE: true, MaxRetries: -1}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "list_devices", Arguments: map[string]any{}})
	if err != nil || result.IsError {
		t.Fatalf("list_devices: %v %v", result, err)
	}
	var payload struct {
		Devices []protocol.Device `json:"devices"`
	}
	b, _ := json.Marshal(result.StructuredContent)
	if err = json.Unmarshal(b, &payload); err != nil || len(payload.Devices) != 1 || payload.Devices[0].ID != "allowed" || !payload.Devices[0].Online {
		t.Fatalf("unscoped inventory: %s error=%v", b, err)
	}
	result, err = session.CallTool(ctx, &mcp.CallToolParams{Name: "echo", Arguments: map[string]any{"device": "allowed", "text": "fixture"}})
	if err != nil || result.IsError {
		t.Fatalf("dispatch: %v %v", result, err)
	}
	b, _ = json.Marshal(result.StructuredContent)
	if !bytes.Contains(b, []byte(h.access.Principal(accessResource, "owner@example.com", "access-subject"))) {
		t.Fatal("wire request lost validated principal")
	}
	if err = <-done; err != nil {
		t.Fatal(err)
	}
	result, err = session.CallTool(ctx, &mcp.CallToolParams{Name: "echo", Arguments: map[string]any{"device": "root", "text": "fixture"}})
	if err != nil || !result.IsError {
		t.Fatal("Access inherited owner root access", err)
	}
	if err = store.Update(func(c *config.Hub) error { c.CloudflareAccess.Principals = nil; return nil }); err != nil {
		t.Fatal(err)
	}
	if _, err = session.CallTool(ctx, &mcp.CallToolParams{Name: "list_devices", Arguments: map[string]any{}}); err == nil {
		t.Fatal("existing SDK connection retained revoked authority")
	}
}

func TestAccessPolicyRefreshAndIdentity(t *testing.T) {
	h, store, i := accessFixture(t)
	claims := i.Claims("owner@example.com")
	claims["sub"] = "owner"
	raw := i.Sign(t, claims)
	info, code := h.authenticate(accessRequest(raw, ""))
	if code != 0 {
		t.Fatal(code)
	}
	principal := info.UserID
	claims["sub"] = "second-subject"
	second, code := h.authenticate(accessRequest(i.Sign(t, claims), ""))
	if code != 0 || principal == second.UserID || principal == clientPrincipal(config.Client{ID: "owner", TokenHash: config.Hash(aliceToken)}) {
		t.Fatal("Access identity overlap")
	}
	claims["sub"] = "owner"
	refreshed, code := h.authenticate(accessRequest(i.Sign(t, claims), ""))
	if code != 0 || principal != refreshed.UserID {
		t.Fatal("assertion refresh changed session owner")
	}
	// Dispatch rechecks current grants even after request authentication.
	if err := store.Update(func(c *config.Hub) error { c.CloudflareAccess.Principals[0].Devices = []string{"root"}; return nil }); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Call(context.Background(), principal, "allowed", "echo", json.RawMessage(`{}`)); err == nil || !strings.Contains(err.Error(), "not authorized") {
		t.Fatal("dispatch used stale node grants")
	}
	if err := store.Update(func(c *config.Hub) error { c.CloudflareAccess.Principals = nil; return nil }); err != nil {
		t.Fatal(err)
	}
	if _, err := h.client(principal); err == nil {
		t.Fatal("revoked mapping retained session authority")
	}
	if _, code := h.authenticate(accessRequest(raw, aliceToken)); code != 403 {
		t.Fatal("revoked mapping fell back to legacy")
	}
}

func TestAccessIncompatibleSettingsFailClosed(t *testing.T) {
	for _, change := range []struct {
		name string
		edit func(*config.Hub)
	}{
		{"team", func(c *config.Hub) { c.CloudflareAccess.TeamDomain = "https://other.cloudflareaccess.com" }},
		{"audience", func(c *config.Hub) { c.CloudflareAccess.Audience = "other-aud" }},
		{"resource", func(c *config.Hub) { c.PublicURL = "https://other.example" }},
		{"disable", func(c *config.Hub) { c.CloudflareAccess = nil }},
	} {
		t.Run(change.name, func(t *testing.T) {
			h, store, i := accessFixture(t)
			raw := i.Sign(t, i.Claims("owner@example.com"))
			info, _ := h.authenticate(accessRequest(raw, ""))
			if err := store.Update(func(c *config.Hub) error { change.edit(c); return nil }); err != nil {
				t.Fatal(err)
			}
			if _, code := h.authenticate(accessRequest(raw, aliceToken)); code != 401 {
				t.Fatal("incompatible policy retained assertion authority")
			}
			if _, err := h.client(info.UserID); err == nil {
				t.Fatal("incompatible policy retained dispatch authority")
			}
			if _, code := h.authenticate(accessRequest("", aliceToken)); code != 0 {
				t.Fatal("Access setting change revoked legacy credential")
			}
		})
	}
}

func TestAccessStartupFailureAndDisabledCompatibility(t *testing.T) {
	h, store, i := accessFixture(t)
	_ = h.Close()
	i.Fail = true
	if other, err := New(store, nil); err == nil {
		other.Close()
		t.Fatal("cert failure permitted Hub startup")
	}
	i.Fail = false
	i.JWKS = map[string]any{"keys": []any{}}
	if other, err := New(store, nil); err == nil {
		other.Close()
		t.Fatal("malformed certs permitted Hub startup")
	}
	if err := store.Update(func(c *config.Hub) error { c.CloudflareAccess = nil; return nil }); err != nil {
		t.Fatal(err)
	}
	i.Fail = true
	requests := i.Requests.Load()
	other, err := New(store, nil)
	if err != nil {
		t.Fatal("disabled Access affected legacy startup", err)
	}
	defer other.Close()
	if i.Requests.Load() != requests {
		t.Fatal("disabled Access fetched certs")
	}
	if _, code := other.authenticate(accessRequest("incidental-header", aliceToken)); code != 0 {
		t.Fatal("never-enabled Access changed legacy authentication")
	}
	if err = store.Update(func(c *config.Hub) error {
		c.CloudflareAccess = &config.CloudflareAccess{TeamDomain: i.URL, Audience: i.Audience}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, code := other.authenticate(accessRequest("uninitialized-assertion", aliceToken)); code != 401 {
		t.Fatal("Access enabled without startup validation")
	}
}

func TestAccessRestartBindsNewAudience(t *testing.T) {
	h, store, i := accessFixture(t)
	oldToken := i.Sign(t, i.Claims("owner@example.com"))
	oldIdentity, code := h.authenticate(accessRequest(oldToken, ""))
	if code != 0 {
		t.Fatal(code)
	}
	if err := store.Update(func(c *config.Hub) error { c.CloudflareAccess.Audience = "new-application-aud"; return nil }); err != nil {
		t.Fatal(err)
	}
	restarted, err := New(store, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	if _, code := restarted.authenticate(accessRequest(oldToken, aliceToken)); code != 401 {
		t.Fatal("restart accepted old application assertion")
	}
	if _, err := restarted.client(oldIdentity.UserID); err == nil {
		t.Fatal("restart inherited old application sessions")
	}
	claims := i.Claims("owner@example.com")
	claims["aud"] = "new-application-aud"
	identity, code := restarted.authenticate(accessRequest(i.Sign(t, claims), ""))
	if code != 0 || identity.UserID == oldIdentity.UserID {
		t.Fatal("restart did not adopt new application trust")
	}
	// Invalid enabled config is also a startup error, before any key fetch.
	cfg, err := store.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	cfg.CloudflareAccess.TeamDomain = "https://untrusted.example"
	if err = config.WriteJSON(store.Path, cfg, true); err != nil {
		t.Fatal(err)
	}
	if invalid, err := New(store, nil); err == nil {
		invalid.Close()
		t.Fatal("invalid enabled Access config allowed startup")
	}
}

func TestAccessLogsOmitIdentityAndCredentials(t *testing.T) {
	h, _, i := accessFixture(t)
	raw := i.Sign(t, i.Claims("owner@example.com"))
	var output bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&output)
	defer log.SetOutput(previous)
	info, _ := h.authenticate(accessRequest(raw, "opaque-private-token"))
	if _, err := h.ListDevices(context.Background(), info.UserID); err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	h.Handler().ServeHTTP(w, accessRequest("invalid-private-jwt", "opaque-private-token"))
	for _, secret := range []string{raw, "owner@example.com", "opaque-private-token", "invalid-private-jwt"} {
		if strings.Contains(output.String(), secret) {
			t.Fatal("authentication log leaked identity or credential")
		}
	}
}

func TestAccessCoexistsWithExternalOAuth(t *testing.T) {
	old, store, issuer := oauthFixture(t)
	_ = old.Close()
	i := testutil.NewAccessIssuer(t)
	previous := http.DefaultTransport
	http.DefaultTransport = testutil.HandlerTransport{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Host == "isolated-test.cloudflareaccess.com" {
			i.ServeHTTP(w, r)
		} else {
			issuer.ServeHTTP(w, r)
		}
	})}
	t.Cleanup(func() { http.DefaultTransport = previous })
	if err := store.Update(func(c *config.Hub) error {
		c.CloudflareAccess = &config.CloudflareAccess{TeamDomain: i.URL, Audience: i.Audience, Principals: []config.CloudflareAccessPrincipal{{Email: "owner@example.com", Devices: []string{"allowed"}}}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	h, err := New(store, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	accessToken := i.Sign(t, i.Claims("owner@example.com"))
	oauthToken := issuer.Sign(t, issuer.Claims("owner", accessResource))
	identities := map[string]bool{}
	for _, r := range []*http.Request{accessRequest("", aliceToken), accessRequest("", oauthToken), accessRequest(accessToken, "")} {
		info, code := h.authenticate(r)
		if code != 0 || identities[info.UserID] {
			t.Fatal("independent authentication modes overlap or fail")
		}
		identities[info.UserID] = true
		w := httptest.NewRecorder()
		h.Handler().ServeHTTP(w, r)
		if w.Code != 200 {
			t.Fatal("coexisting SDK wrapper failed")
		}
	}
	if _, code := h.authenticate(accessRequest("invalid-assertion", oauthToken)); code != 401 {
		t.Fatal("invalid Access downgraded to external OAuth")
	}
	if err = store.Update(func(c *config.Hub) error { c.OAuth = nil; return nil }); err != nil {
		t.Fatal(err)
	}
	if _, code := h.authenticate(accessRequest(accessToken, "")); code != 0 {
		t.Fatal("disabling OAuth disabled Access")
	}
}

func TestAccessNativeSessionOwnership(t *testing.T) {
	h, _, i := accessFixture(t)
	claims := i.Claims("owner@example.com")
	owner, code := h.authenticate(accessRequest(i.Sign(t, claims), ""))
	if code != 0 {
		t.Fatal(code)
	}
	claims["sub"] = "different-verified-subject"
	other, code := h.authenticate(accessRequest(i.Sign(t, claims), ""))
	if code != 0 {
		t.Fatal(code)
	}
	runtime, err := host.NewForDevice("allowed")
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	// This process belongs only to this fixture and is closed by runtime.Close.
	result, err := runtime.Call(context.Background(), owner.UserID, "exec_command", json.RawMessage(`{"cmd":"cat","shell":"/bin/sh","login":false,"yield_time_ms":0}`))
	if err != nil {
		t.Fatal(err)
	}
	var process struct {
		SessionID string `json:"session_id"`
	}
	if err = json.Unmarshal(result.Data, &process); err != nil || process.SessionID == "" {
		t.Fatal("missing session", err)
	}
	input, _ := json.Marshal(map[string]any{"session_id": process.SessionID, "yield_time_ms": 0})
	for _, unauthorized := range []string{other.UserID, clientPrincipal(config.Client{ID: "owner", TokenHash: config.Hash(aliceToken)}), "owner"} {
		if _, err = runtime.Call(context.Background(), unauthorized, "write_stdin", input); err == nil {
			t.Fatal("unrelated principal accessed Access session")
		}
	}
	refreshed, code := h.authenticate(accessRequest(i.Sign(t, i.Claims("owner@example.com")), ""))
	if code != 0 {
		t.Fatal(code)
	}
	if _, err = runtime.Call(context.Background(), refreshed.UserID, "write_stdin", input); err != nil {
		t.Fatal("assertion refresh lost session ownership", err)
	}
}
