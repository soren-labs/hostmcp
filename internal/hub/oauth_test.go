package hub

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/golang-jwt/jwt/v5"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/soren-labs/hostmcp/internal/config"
	"github.com/soren-labs/hostmcp/internal/oauth"
	"github.com/soren-labs/hostmcp/internal/testutil"
)

func oauthFixture(t *testing.T) (*Hub, *config.Store, *testutil.Issuer) {
	t.Helper()
	issuer := testutil.NewIssuer(t)
	issuer.Install(t)
	cfg := config.Hub{Version: 1, Listen: "127.0.0.1:0", PublicURL: "https://mcp.example", Clients: []config.Client{{ID: "owner", TokenHash: config.Hash(aliceToken), Devices: []string{"*"}}}, Devices: []config.Device{{ID: "allowed", TokenHash: config.Hash(deviceToken)}, {ID: "root", TokenHash: config.Hash(deviceToken)}}, OAuth: &config.OAuth{Issuer: issuer.URL, Scopes: []string{"hostmcp:tools"}, Principals: []config.OAuthPrincipal{{Subject: "owner", Devices: []string{"allowed"}}, {Subject: "second", Devices: []string{"allowed"}}}}}
	file := filepath.Join(t.TempDir(), "hub.json")
	if err := config.WriteJSON(file, cfg, false); err != nil {
		t.Fatal(err)
	}
	store := config.NewStore(file)
	h, err := New(store, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { h.Close() })
	return h, store, issuer
}
func TestOAuthHubChallengesAndPolicy(t *testing.T) {
	h, store, i := oauthFixture(t)
	resource := "https://mcp.example/mcp"
	valid := i.Sign(t, i.Claims("owner", resource))
	wrongAudience := i.Claims("owner", "different")
	expired := i.Claims("owner", resource)
	expired["exp"] = 1
	noScope := i.Claims("owner", resource)
	noScope["scope"] = "* owner admin"
	tests := []struct {
		name, token string
		want        int
	}{
		{"missing", "", 401}, {"invalid", "nonsense", 401}, {"wrong-audience", i.Sign(t, wrongAudience), 401}, {"expired", i.Sign(t, expired), 401},
		{"unmapped-subject", i.Sign(t, i.Claims("unmapped", resource)), 403}, {"scope-alone", i.Sign(t, noScope), 403}, {"legacy", aliceToken, 200}, {"oauth", valid, 200},
	}
	body := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"test","version":"1"}}}`
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := testutil.BearerRequest("POST", resource, tc.token, body)
			w := httptest.NewRecorder()
			h.Handler().ServeHTTP(w, r)
			if w.Code != tc.want {
				t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
			}
			if tc.want == 401 || tc.want == 403 {
				challenge := w.Header().Get("WWW-Authenticate")
				if !strings.Contains(challenge, `resource_metadata="https://mcp.example/.well-known/oauth-protected-resource/mcp"`) || !strings.Contains(challenge, `scope="hostmcp:tools"`) {
					t.Fatal(challenge)
				}
			}
		})
	}
	for _, path := range []string{"/.well-known/oauth-protected-resource", "/.well-known/oauth-protected-resource/mcp"} {
		r := httptest.NewRequest("GET", "https://mcp.example"+path, nil)
		r.Header.Set("X-Forwarded-Host", "evil.example")
		w := httptest.NewRecorder()
		h.Handler().ServeHTTP(w, r)
		var metadata struct {
			Resource string   `json:"resource"`
			Servers  []string `json:"authorization_servers"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &metadata); err != nil || w.Code != 200 || metadata.Resource != resource || len(metadata.Servers) != 1 || metadata.Servers[0] != i.URL {
			t.Fatalf("metadata %s %v", w.Body.String(), err)
		}
	}
	for _, path := range []string{"/mcp", "/agent/allowed"} {
		r := testutil.BearerRequest("GET", "https://mcp.example"+path, valid, "")
		r.Header.Set("Origin", "https://evil.example")
		w := httptest.NewRecorder()
		h.Handler().ServeHTTP(w, r)
		if w.Code != 403 {
			t.Fatal("origin checks bypassed")
		}
	}
	w := httptest.NewRecorder()
	h.Handler().ServeHTTP(w, testutil.BearerRequest("GET", "https://mcp.example/agent/allowed", valid, ""))
	if w.Code != 401 {
		t.Fatal("OAuth token authenticated agent channel")
	}
	r := testutil.BearerRequest("POST", resource, valid, body)
	r.Header.Add("Authorization", "Bearer "+aliceToken)
	w = httptest.NewRecorder()
	h.Handler().ServeHTTP(w, r)
	if w.Code != 401 {
		t.Fatal("duplicate authorization accepted")
	}
	principal := oauth.Principal(i.URL, resource, "owner", "test-client")
	devices, err := h.ListDevices(context.Background(), principal)
	if err != nil || len(devices) != 1 || devices[0].ID != "allowed" {
		t.Fatalf("OAuth inherited owner access: %v %v", devices, err)
	}
	if _, err = h.Call(context.Background(), principal, "root", "exec_command", json.RawMessage(`{}`)); err == nil || !strings.Contains(err.Error(), "not authorized") {
		t.Fatalf("root grant leaked: %v", err)
	}
	if principal == clientPrincipal(config.Client{ID: "owner", TokenHash: config.Hash(aliceToken)}) || principal == oauth.Principal(i.URL, resource, "second", "test-client") {
		t.Fatal("principals overlap")
	}
	if err = store.Update(func(c *config.Hub) error { c.OAuth.Principals = nil; return nil }); err != nil {
		t.Fatal(err)
	}
	if _, err = h.client(principal); err == nil {
		t.Fatal("removed OAuth grant retained authority")
	}
}
func TestOAuthSDKClientInMemory(t *testing.T) {
	h, _, i := oauthFixture(t)
	transport := testutil.HandlerTransport{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Header.Set("Authorization", "Bearer "+i.Sign(t, i.Claims("owner", "https://mcp.example/mcp")))
		h.Handler().ServeHTTP(w, r)
	})}
	client := mcp.NewClient(&mcp.Implementation{Name: "isolated-oauth-test", Version: "1"}, nil)
	ctx := context.Background()
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: "https://mcp.example/mcp", HTTPClient: &http.Client{Transport: transport}, DisableStandaloneSSE: true, MaxRetries: -1}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "list_devices", Arguments: map[string]any{}})
	if err != nil || result.IsError {
		t.Fatalf("call: %v %v", result, err)
	}
	b, _ := json.Marshal(result)
	if !strings.Contains(string(b), "allowed") || strings.Contains(string(b), `\"id\":\"root\"`) {
		t.Fatalf("scoped SDK result: %s", b)
	}
}

func TestOAuthClientSessionIdentityAndPolicyChanges(t *testing.T) {
	h, store, i := oauthFixture(t)
	resource := "https://mcp.example/mcp"
	first := i.Claims("owner", resource)
	second := i.Claims("owner", resource)
	second["client_id"] = "another-client"
	authenticate := func(claims jwt.MapClaims) (string, int) {
		info, code := h.authenticate(testutil.BearerRequest("POST", resource, i.Sign(t, claims), ""))
		if info == nil {
			return "", code
		}
		return info.UserID, code
	}
	p1, code := authenticate(first)
	if code != 0 {
		t.Fatal(code)
	}
	p2, code := authenticate(second)
	if code != 0 || p1 == p2 {
		t.Fatal("OAuth clients share sessions")
	}
	refreshed, code := authenticate(i.Claims("owner", resource))
	if code != 0 || p1 != refreshed {
		t.Fatal("refresh lost session identity")
	}
	delete(first, "client_id")
	if _, code = authenticate(first); code != 401 {
		t.Fatal("missing OAuth client identity accepted")
	}
	first["azp"] = "test-client"
	if p, code := authenticate(first); code != 0 || p != p1 {
		t.Fatal("azp fallback failed")
	}
	first["client_id"] = "different"
	if _, code = authenticate(first); code != 401 {
		t.Fatal("conflicting client identity accepted")
	}
	if err := store.Update(func(c *config.Hub) error { c.OAuth.Scopes = []string{"new-scope"}; return nil }); err != nil {
		t.Fatal(err)
	}
	if _, code = authenticate(second); code != 401 {
		t.Fatal("changed discovery policy used without restart")
	}
	if _, err := h.client(p1); err == nil {
		t.Fatal("in-flight call bypassed changed policy")
	}
}
