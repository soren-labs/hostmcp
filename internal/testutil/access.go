package testutil

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// AccessIssuer is an ephemeral in-memory Access cert endpoint, never a real team.
// Mutable fields are changed only between requests in sequential tests.
type AccessIssuer struct {
	URL, Audience, KID string
	Key                *rsa.PrivateKey
	JWKS               any
	Fail               bool
	Requests           atomic.Int64
}

func NewAccessIssuer(t *testing.T) *AccessIssuer {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	i := &AccessIssuer{URL: "https://isolated-test.cloudflareaccess.com", Audience: "test-application-aud"}
	i.SetKey(key, "access-key-1")
	return i
}
func (i *AccessIssuer) SetKey(key *rsa.PrivateKey, kid string) {
	i.Key, i.KID = key, kid
	enc := base64.RawURLEncoding.EncodeToString
	i.JWKS = map[string]any{"keys": []any{map[string]any{"kty": "RSA", "alg": "RS256", "use": "sig", "kid": kid, "n": enc(key.N.Bytes()), "e": enc(big.NewInt(int64(key.E)).Bytes())}}}
}
func (i *AccessIssuer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	i.Requests.Add(1)
	if r.URL.Scheme != "https" || r.URL.Host != "isolated-test.cloudflareaccess.com" || r.URL.Path != "/cdn-cgi/access/certs" {
		http.Error(w, "unexpected cert endpoint", 400)
		return
	}
	if i.Fail {
		http.Error(w, "unavailable", 503)
		return
	}
	_ = json.NewEncoder(w).Encode(i.JWKS)
}
func (i *AccessIssuer) Install(t *testing.T) {
	t.Helper()
	previous := http.DefaultTransport
	http.DefaultTransport = HandlerTransport{i}
	t.Cleanup(func() { http.DefaultTransport = previous })
}
func (i *AccessIssuer) Claims(email string) jwt.MapClaims {
	return jwt.MapClaims{"iss": i.URL, "aud": i.Audience, "sub": "access-subject", "email": email, "exp": time.Now().Add(time.Hour).Unix(), "nbf": time.Now().Add(-time.Minute).Unix(), "iat": time.Now().Add(-time.Minute).Unix()}
}
func (i *AccessIssuer) Sign(t *testing.T, claims jwt.MapClaims) string {
	t.Helper()
	token := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	token.Header["kid"] = i.KID
	raw, err := token.SignedString(i.Key)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
