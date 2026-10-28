package cloudflareaccess

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/soren-labs/hostmcp/internal/config"
	"github.com/soren-labs/hostmcp/internal/testutil"
)

func verifierFixture(t *testing.T) (*Verifier, *testutil.AccessIssuer) {
	t.Helper()
	i := testutil.NewAccessIssuer(t)
	v, err := newVerifier(context.Background(), config.CloudflareAccess{TeamDomain: i.URL, Audience: i.Audience}, testutil.HandlerTransport{Handler: i})
	if err != nil {
		t.Fatal(err)
	}
	return v, i
}

func TestAccessJWTValidation(t *testing.T) {
	v, i := verifierFixture(t)
	for _, tc := range []struct {
		name  string
		edit  func(jwt.MapClaims)
		valid bool
	}{
		{"valid", func(jwt.MapClaims) {}, true},
		{"audience-array", func(c jwt.MapClaims) { c["aud"] = []string{"other", i.Audience} }, true},
		{"wrong-audience", func(c jwt.MapClaims) { c["aud"] = i.Audience + "x" }, false},
		{"missing-audience", func(c jwt.MapClaims) { delete(c, "aud") }, false},
		{"wrong-issuer", func(c jwt.MapClaims) { c["iss"] = "https://other.cloudflareaccess.com" }, false},
		{"issuer-slash", func(c jwt.MapClaims) { c["iss"] = i.URL + "/" }, false},
		{"missing-issuer", func(c jwt.MapClaims) { delete(c, "iss") }, false},
		{"expired", func(c jwt.MapClaims) { c["exp"] = time.Now().Add(-time.Second).Unix() }, false},
		{"missing-expiry", func(c jwt.MapClaims) { delete(c, "exp") }, false},
		{"null-expiry", func(c jwt.MapClaims) { c["exp"] = nil }, false},
		{"future-nbf", func(c jwt.MapClaims) { c["nbf"] = time.Now().Add(time.Hour).Unix() }, false},
		{"future-iat", func(c jwt.MapClaims) { c["iat"] = time.Now().Add(time.Hour).Unix() }, false},
		{"optional-times-absent", func(c jwt.MapClaims) { delete(c, "iat"); delete(c, "nbf") }, true},
		{"null-nbf", func(c jwt.MapClaims) { c["nbf"] = nil }, false},
		{"null-iat", func(c jwt.MapClaims) { c["iat"] = nil }, false},
		{"bad-time-type", func(c jwt.MapClaims) { c["iat"] = "tomorrow" }, false},
		{"numeric-string-time", func(c jwt.MapClaims) { c["iat"] = "0" }, false},
		{"missing-email", func(c jwt.MapClaims) { delete(c, "email") }, false},
		{"email-is-owner", func(c jwt.MapClaims) { c["email"] = "owner" }, false},
		{"email-display-name", func(c jwt.MapClaims) { c["email"] = "Owner <owner@example.com>" }, false},
		{"email-wildcard", func(c jwt.MapClaims) { c["email"] = "*@example.com" }, false},
		{"missing-sub", func(c jwt.MapClaims) { delete(c, "sub") }, false},
		{"blank-sub", func(c jwt.MapClaims) { c["sub"] = "  " }, false},
		{"long-sub", func(c jwt.MapClaims) { c["sub"] = strings.Repeat("s", 513) }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			claims := i.Claims("OWNER@EXAMPLE.COM")
			tc.edit(claims)
			verified, err := v.Verify(context.Background(), i.Sign(t, claims))
			if (err == nil) != tc.valid {
				t.Fatalf("valid=%v error=%v", tc.valid, err)
			}
			if tc.valid && verified.Email != "owner@example.com" {
				t.Fatal("email was not normalized")
			}
		})
	}
	other := testutil.NewAccessIssuer(t)
	if _, err := v.Verify(context.Background(), other.Sign(t, i.Claims("owner@example.com"))); err == nil {
		t.Fatal("forged signature accepted")
	}
	for _, method := range []jwt.SigningMethod{jwt.SigningMethodHS256, jwt.SigningMethodNone, jwt.SigningMethodRS384} {
		token := jwt.NewWithClaims(method, i.Claims("owner@example.com"))
		token.Header["kid"] = i.KID
		var key any = i.Key
		if method == jwt.SigningMethodHS256 {
			key = []byte("untrusted")
		}
		if method == jwt.SigningMethodNone {
			key = jwt.UnsafeAllowNoneSignatureType
		}
		raw, err := token.SignedString(key)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = v.Verify(context.Background(), raw); err == nil {
			t.Fatal("disallowed signing algorithm accepted")
		}
	}
	for _, kid := range []any{nil, "", "missing-key", 12, strings.Repeat("x", 257)} {
		token := jwt.NewWithClaims(jwt.SigningMethodRS256, i.Claims("owner@example.com"))
		token.Header["kid"] = kid
		raw, err := token.SignedString(i.Key)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = v.Verify(context.Background(), raw); err == nil {
			t.Fatal("invalid kid accepted")
		}
	}
	// Header/claim ambiguity is rejected even when correctly signed.
	claims, _ := json.Marshal(i.Claims("owner@example.com"))
	for _, tc := range []struct{ header, payload string }{
		{`{"alg":"RS256","kid":"access-key-1","kid":"other"}`, string(claims)},
		{`{"alg":"RS256","kid":"access-key-1","ALG":"RS256"}`, string(claims)},
		{`{"alg":"RS256","kid":"access-key-1","crit":["unknown"]}`, string(claims)},
		{`{"alg":"RS256","kid":"access-key-1","b64":false}`, string(claims)},
		{`{"alg":"RS256","kid":"access-key-1"}`, strings.TrimSuffix(string(claims), "}") + `,"email":"other@example.com"}`},
		{`{"alg":"RS256","kid":"access-key-1"}`, strings.TrimSuffix(string(claims), "}") + `,"EMAIL":"other@example.com"}`},
	} {
		enc := base64.RawURLEncoding.EncodeToString
		input := enc([]byte(tc.header)) + "." + enc([]byte(tc.payload))
		signature, err := jwt.SigningMethodRS256.Sign(input, i.Key)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = v.Verify(context.Background(), input+"."+enc(signature)); err == nil {
			t.Fatal("ambiguous JWT accepted")
		}
	}
	for _, raw := range []string{"", "not-a-jwt", "a.b.c", strings.Repeat("x", maxJWT+1), "\n" + i.Sign(t, i.Claims("owner@example.com"))} {
		if _, err := v.Verify(context.Background(), raw); err == nil {
			t.Fatal("malformed or oversized assertion accepted")
		}
	}
	if i.Requests.Load() != 1 {
		t.Fatal("invalid tokens caused unbounded cert fetches")
	}
}

func TestAccessKeyRotationCacheAndConcurrency(t *testing.T) {
	v, i := verifierFixture(t)
	old := i.Sign(t, i.Claims("owner@example.com"))
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	i.SetKey(key, "access-key-2")
	rotated := i.Sign(t, i.Claims("owner@example.com"))
	v.attempted = time.Now().Add(-refreshInterval)
	var workers sync.WaitGroup
	for range 24 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			if _, err := v.Verify(context.Background(), rotated); err != nil {
				t.Error(err)
			}
		}()
	}
	workers.Wait()
	if i.Requests.Load() != 2 {
		t.Fatal("concurrent key refresh was not coalesced")
	}
	if _, err = v.Verify(context.Background(), old); err == nil {
		t.Fatal("removed key accepted after refresh")
	}
	i.Fail = true
	if _, err = v.Verify(context.Background(), rotated); err != nil {
		t.Fatal("fresh cached key rejected", err)
	}
	v.refreshed, v.attempted = time.Now().Add(-cacheLifetime), time.Now().Add(-refreshInterval)
	if _, err = v.Verify(context.Background(), rotated); err == nil {
		t.Fatal("stale key cache failed open")
	}
	if _, err = v.Verify(context.Background(), rotated); err == nil {
		t.Fatal("refresh cooldown failed open")
	}
	if i.Requests.Load() != 3 {
		t.Fatal("failed refresh was not rate limited")
	}
	i.Fail = false
	v.attempted = time.Now().Add(-refreshInterval)
	if _, err = v.Verify(context.Background(), rotated); err != nil {
		t.Fatal("refresh did not recover", err)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestAccessStartupCertFailures(t *testing.T) {
	i := testutil.NewAccessIssuer(t)
	jwk := i.JWKS.(map[string]any)["keys"].([]any)[0]
	valid, _ := json.Marshal(i.JWKS)
	duplicate, _ := json.Marshal(map[string]any{"keys": []any{jwk, jwk}})
	tooMany, _ := json.Marshal(map[string]any{"keys": make([]any, 129)})
	for _, body := range []string{"garbage", "null", `{}`, `{"keys":[]}`, `{"keys":[{}]}`, `{"keys":[],"keys":[]}`, string(duplicate), string(tooMany), strings.Repeat(" ", maxDocument+1), string(valid) + `{}`} {
		transport := testutil.HandlerTransport{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, body) })}
		if _, err := newVerifier(context.Background(), config.CloudflareAccess{TeamDomain: i.URL, Audience: i.Audience}, transport); err == nil {
			t.Fatal("invalid cert document allowed startup")
		}
	}
	for _, edit := range []func(map[string]any){
		func(k map[string]any) { k["kty"] = "EC" }, func(k map[string]any) { k["alg"] = "HS256" }, func(k map[string]any) { k["use"] = "enc" },
		func(k map[string]any) { k["key_ops"] = []string{"encrypt"} }, func(k map[string]any) { k["n"] = "AQID" }, func(k map[string]any) { k["n"] = "bad!" },
		func(k map[string]any) { k["e"] = "Ag" }, func(k map[string]any) { delete(k, "kid") },
	} {
		var copyDoc map[string]any
		if err := json.Unmarshal(valid, &copyDoc); err != nil {
			t.Fatal(err)
		}
		edit(copyDoc["keys"].([]any)[0].(map[string]any))
		i.JWKS = copyDoc
		if _, err := newVerifier(context.Background(), config.CloudflareAccess{TeamDomain: i.URL, Audience: i.Audience}, testutil.HandlerTransport{Handler: i}); err == nil {
			t.Fatal("unusable key allowed startup")
		}
	}
	requests := 0
	redirect := testutil.HandlerTransport{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		http.Redirect(w, r, "https://other.example/certs", 302)
	})}
	if _, err := newVerifier(context.Background(), config.CloudflareAccess{TeamDomain: i.URL, Audience: i.Audience}, redirect); err == nil || requests != 1 {
		t.Fatal("redirect accepted or followed")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	blocked := roundTripFunc(func(r *http.Request) (*http.Response, error) { <-r.Context().Done(); return nil, r.Context().Err() })
	if _, err := newVerifier(ctx, config.CloudflareAccess{TeamDomain: i.URL, Audience: i.Audience}, blocked); err == nil {
		t.Fatal("cert timeout allowed startup")
	}
}

func TestAccessPrincipalSeparation(t *testing.T) {
	v, _ := verifierFixture(t)
	p := v.Principal("https://mcp.example/mcp", "OWNER@example.com", "owner")
	if len(p) > 128 || strings.Contains(p, "owner") || !v.MatchesPrincipal(p, "https://mcp.example/mcp", "owner@example.com") {
		t.Fatal("invalid or identifying principal")
	}
	for _, other := range []string{v.Principal("https://other.example/mcp", "owner@example.com", "owner"), v.Principal("https://mcp.example/mcp", "owner@example.com", "other"), v.Principal("https://mcp.example/mcp", "other@example.com", "owner")} {
		if other == p {
			t.Fatal("unrelated identities share sessions")
		}
	}
}
