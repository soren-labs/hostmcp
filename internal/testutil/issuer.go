// Package testutil contains isolated in-memory fixtures, never live credentials.
package testutil

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

type HandlerTransport struct{ Handler http.Handler }

func (t HandlerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	w := httptest.NewRecorder()
	t.Handler.ServeHTTP(w, r)
	return w.Result(), nil
}

type Issuer struct {
	URL      string
	Key      *ecdsa.PrivateKey
	KID      string
	OIDCOnly bool
	Fail     bool
	JWKS     any
}

func NewIssuer(t *testing.T) *Issuer {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	i := &Issuer{URL: "https://issuer.example/tenant", Key: key, KID: "key-1"}
	i.SetKey(key, "key-1")
	return i
}
func (i *Issuer) SetKey(key *ecdsa.PrivateKey, kid string) {
	i.Key = key
	i.KID = kid
	enc := base64.RawURLEncoding.EncodeToString
	i.JWKS = map[string]any{"keys": []any{map[string]any{"kty": "EC", "crv": "P-256", "alg": "ES256", "use": "sig", "kid": kid, "x": enc(key.X.FillBytes(make([]byte, 32))), "y": enc(key.Y.FillBytes(make([]byte, 32)))}}}
}
func (i *Issuer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if i.Fail {
		http.Error(w, "unavailable", 503)
		return
	}
	switch r.URL.Path {
	case "/.well-known/oauth-authorization-server/tenant":
		if i.OIDCOnly {
			http.NotFound(w, r)
			return
		}
	case "/tenant/.well-known/openid-configuration":
	case "/keys":
		_ = json.NewEncoder(w).Encode(i.JWKS)
		return
	default:
		http.NotFound(w, r)
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"issuer": i.URL, "jwks_uri": "https://issuer.example/keys", "authorization_endpoint": i.URL + "/authorize", "token_endpoint": i.URL + "/token"})
}

// Install is for sequential tests only. No network listener is opened.
func (i *Issuer) Install(t *testing.T) {
	t.Helper()
	previous := http.DefaultTransport
	http.DefaultTransport = HandlerTransport{i}
	t.Cleanup(func() { http.DefaultTransport = previous })
}
func (i *Issuer) Claims(subject, resource string) jwt.MapClaims {
	return jwt.MapClaims{"client_id": "test-client", "iss": i.URL, "sub": subject, "aud": resource, "exp": time.Now().Add(time.Hour).Unix(), "iat": time.Now().Add(-time.Minute).Unix(), "scope": "hostmcp:tools"}
}
func (i *Issuer) Sign(t *testing.T, claims jwt.MapClaims) string {
	t.Helper()
	token := jwt.NewWithClaims(jwt.SigningMethodES256, claims)
	token.Header["kid"] = i.KID
	signed, err := token.SignedString(i.Key)
	if err != nil {
		t.Fatal(err)
	}
	return signed
}
func BearerRequest(method, url, token, body string) *http.Request {
	r := httptest.NewRequest(method, url, strings.NewReader(body))
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Accept", "application/json, text/event-stream")
	return r
}
