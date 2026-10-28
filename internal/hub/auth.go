package hub

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/soren-labs/hostmcp/internal/config"
	"github.com/soren-labs/hostmcp/internal/oauth"
)

type tokenInfoKey struct{}

func (h *Hub) authenticate(r *http.Request) (*auth.TokenInfo, int) {
	token := bearer(r)
	if token == "" {
		return nil, 401
	}
	cfg, err := h.store.Snapshot()
	if err != nil {
		return nil, 503
	}
	for _, c := range cfg.Clients {
		if len(token) >= 32 && len(token) <= 1024 && subtle.ConstantTimeCompare([]byte(config.Hash(token)), []byte(c.TokenHash)) == 1 {
			return &auth.TokenInfo{UserID: clientPrincipal(c)}, 0
		}
	}
	if h.oauth == nil || cfg.OAuth == nil || cfg.OAuth.Issuer != h.oauth.Issuer || strings.TrimSuffix(cfg.PublicURL, "/")+"/mcp" != h.oauth.Resource || !slices.Equal(cfg.OAuth.Scopes, h.oauthScopes) {
		return nil, 401
	}
	claims, err := h.oauth.Verify(r.Context(), token)
	if err != nil {
		return nil, 401
	}
	scopes := claims.Scopes()
	for _, s := range cfg.OAuth.Scopes {
		if !slices.Contains(scopes, s) {
			return nil, 403
		}
	}
	for _, p := range cfg.OAuth.Principals {
		if p.Subject == claims.Subject {
			return &auth.TokenInfo{UserID: oauth.Principal(h.oauth.Issuer, h.oauth.Resource, p.Subject, claims.ClientID), Expiration: claims.ExpiresAt.Time, Scopes: scopes}, 0
		}
	}
	return nil, 403
}
func (h *Hub) challenge(w http.ResponseWriter, code int) {
	challenge := `Bearer realm="hostmcp"`
	if h.oauth != nil {
		challenge += `, resource_metadata="` + h.metadataURL + `", scope="` + strings.Join(h.oauthScopes, " ") + `"`
	}
	if code == 403 {
		challenge += `, error="insufficient_scope"`
	} else if code == 401 {
		challenge += `, error="invalid_token"`
	}
	w.Header().Set("WWW-Authenticate", challenge)
}
func (h *Hub) metadata(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodOptions {
		w.Header().Set("Allow", "GET, OPTIONS")
		http.Error(w, "method not allowed", 405)
		return
	}
	// Public information only, still behind the Hub's host/origin checks.
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "GET, OPTIONS")
	if r.Method == http.MethodOptions {
		w.WriteHeader(204)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"resource": h.oauth.Resource, "authorization_servers": []string{h.oauth.Issuer}, "bearer_methods_supported": []string{"header"}, "scopes_supported": h.oauthScopes})
}
func cachedTokenInfo(ctx context.Context, _ string, _ *http.Request) (*auth.TokenInfo, error) {
	info, _ := ctx.Value(tokenInfoKey{}).(*auth.TokenInfo)
	if info == nil {
		return nil, errors.Join(auth.ErrInvalidToken, errors.New("authentication required"))
	}
	return info, nil
}
