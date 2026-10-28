package oauth

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"math/big"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/soren-labs/hostmcp/internal/testutil"
)

func TestOAuthJWTValidation(t *testing.T) {
	i := testutil.NewIssuer(t)
	i.Install(t)
	resource := "https://mcp.example/mcp"
	v, err := New(context.Background(), i.URL, resource)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name  string
		edit  func(jwt.MapClaims)
		valid bool
	}{
		{"valid", func(jwt.MapClaims) {}, true},
		{"audience-array", func(c jwt.MapClaims) { c["aud"] = []string{"other", resource} }, true},
		{"wrong-audience", func(c jwt.MapClaims) { c["aud"] = "https://other.example/mcp" }, false},
		{"no-audience", func(c jwt.MapClaims) { delete(c, "aud") }, false},
		{"wrong-issuer", func(c jwt.MapClaims) { c["iss"] = "https://evil.example" }, false},
		{"issuer-slash", func(c jwt.MapClaims) { c["iss"] = i.URL + "/" }, false},
		{"expired", func(c jwt.MapClaims) { c["exp"] = time.Now().Add(-time.Minute).Unix() }, false},
		{"no-expiry", func(c jwt.MapClaims) { delete(c, "exp") }, false},
		{"future-nbf", func(c jwt.MapClaims) { c["nbf"] = time.Now().Add(time.Hour).Unix() }, false},
		{"future-iat", func(c jwt.MapClaims) { c["iat"] = time.Now().Add(time.Hour).Unix() }, false},
		{"missing-client", func(c jwt.MapClaims) { delete(c, "client_id") }, false},
		{"no-subject", func(c jwt.MapClaims) { delete(c, "sub") }, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			claims := i.Claims("subject", resource)
			tc.edit(claims)
			_, err := v.Verify(context.Background(), i.Sign(t, claims))
			if (err == nil) != tc.valid {
				t.Fatalf("valid=%v error=%v", tc.valid, err)
			}
		})
	}
	raw := i.Sign(t, i.Claims("subject", resource))
	other := testutil.NewIssuer(t)
	if _, err = v.Verify(context.Background(), other.Sign(t, i.Claims("subject", resource))); err == nil {
		t.Fatal("wrong signature accepted")
	}
	for _, method := range []jwt.SigningMethod{jwt.SigningMethodHS256, jwt.SigningMethodNone} {
		token := jwt.NewWithClaims(method, i.Claims("subject", resource))
		token.Header["kid"] = i.KID
		key := any([]byte("irrelevant"))
		if method == jwt.SigningMethodNone {
			key = jwt.UnsafeAllowNoneSignatureType
		}
		signed, err := token.SignedString(key)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = v.Verify(context.Background(), signed); err == nil {
			t.Fatal("disallowed algorithm accepted")
		}
	}
	var workers sync.WaitGroup
	for range 20 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			if _, err := v.Verify(context.Background(), raw); err != nil {
				t.Error(err)
			}
		}()
	}
	workers.Wait()
}
func TestOAuthDiscoveryRotationAndFailure(t *testing.T) {
	i := testutil.NewIssuer(t)
	i.OIDCOnly = true
	i.Install(t)
	resource := "https://mcp.example/mcp"
	v, err := New(context.Background(), i.URL, resource)
	if err != nil {
		t.Fatal(err)
	}
	old := i.Sign(t, i.Claims("subject", resource))
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	i.SetKey(key, "key-2")
	rotated := i.Sign(t, i.Claims("subject", resource))
	v.attempted = time.Now().Add(-time.Minute)
	if _, err = v.Verify(context.Background(), rotated); err != nil {
		t.Fatal("key rotation:", err)
	}
	if _, err = v.Verify(context.Background(), old); err == nil {
		t.Fatal("removed signing key accepted after refresh")
	}
	i.Fail = true
	// Cached, already trusted keys remain usable only within the bounded lifetime.
	if _, err = v.Verify(context.Background(), rotated); err != nil {
		t.Fatal(err)
	}
	v.refreshed = time.Now().Add(-6 * time.Minute)
	v.attempted = time.Now().Add(-time.Minute)
	if _, err = v.Verify(context.Background(), rotated); err == nil {
		t.Fatal("expired key cache failed open")
	}
	if _, err = New(context.Background(), i.URL, resource); err == nil {
		t.Fatal("failed discovery permitted startup")
	}
	i.Fail = false
	i.JWKS = map[string]any{"keys": []any{}}
	if _, err = New(context.Background(), i.URL, resource); err == nil {
		t.Fatal("empty keyset permitted startup")
	}
	if _, err = New(context.Background(), "https://different.example", resource); err == nil {
		t.Fatal("issuer mismatch permitted")
	}
}
func TestOAuthRSAAndKeyRestrictions(t *testing.T) {
	i := testutil.NewIssuer(t)
	i.Install(t)
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	enc := base64.RawURLEncoding.EncodeToString
	jwk := map[string]any{"kty": "RSA", "kid": "rsa", "alg": "RS256", "n": enc(key.N.Bytes()), "e": enc(big.NewInt(int64(key.E)).Bytes())}
	i.JWKS = map[string]any{"keys": []any{jwk}}
	resource := "https://mcp.example/mcp"
	v, err := New(context.Background(), i.URL, resource)
	if err != nil {
		t.Fatal(err)
	}
	token := jwt.NewWithClaims(jwt.SigningMethodRS256, i.Claims("subject", resource))
	token.Header["kid"] = "rsa"
	raw, err := token.SignedString(key)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = v.Verify(context.Background(), raw); err != nil {
		t.Fatal(err)
	}
	for _, edit := range []func(){func() { jwk["use"] = "enc" }, func() { delete(jwk, "use"); jwk["key_ops"] = []string{"encrypt"} }, func() { delete(jwk, "key_ops"); jwk["n"] = enc([]byte{1, 2, 3}) }} {
		edit()
		if _, err = New(context.Background(), i.URL, resource); err == nil {
			t.Fatal("invalid key accepted")
		}
	}
}
