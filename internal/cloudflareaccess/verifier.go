// Package cloudflareaccess validates Cloudflare Access application assertions.
// It never consumes the opaque Managed OAuth bearer token or trusts email headers.
package cloudflareaccess

import (
	"bytes"
	"context"
	"crypto/rsa"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/golang-jwt/jwt/v5"
	"github.com/soren-labs/hostmcp/internal/config"
)

const (
	Header          = "Cf-Access-Jwt-Assertion"
	maxDocument     = 512 << 10
	maxJWT          = 16 << 10
	cacheLifetime   = 5 * time.Minute
	refreshInterval = 30 * time.Second
)

type Verifier struct {
	issuer, audience     string
	client               *http.Client
	mu                   sync.Mutex
	keys                 map[string]*rsa.PublicKey
	refreshed, attempted time.Time
}

type Claims struct {
	jwt.RegisteredClaims
	Email string `json:"email"`
}

// New downloads and validates the key set before permitting Hub startup. There
// is no insecure transport setting, discovery URL or token-controlled key source.
func New(ctx context.Context, policy config.CloudflareAccess) (*Verifier, error) {
	return newVerifier(ctx, policy, nil)
}

// The transport injection is private and used only by isolated tests.
func newVerifier(ctx context.Context, policy config.CloudflareAccess, transport http.RoundTripper) (*Verifier, error) {
	if err := policy.Validate(); err != nil {
		return nil, err
	}
	v := &Verifier{issuer: policy.TeamDomain, audience: policy.Audience, client: &http.Client{
		Transport: transport, Timeout: 5 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("Access cert redirects refused") },
	}}
	if err := v.refresh(ctx); err != nil {
		return nil, errors.New("Access signing keys unavailable or invalid")
	}
	return v, nil
}

func (v *Verifier) Matches(policy *config.CloudflareAccess) bool {
	return policy != nil && policy.TeamDomain == v.issuer && policy.Audience == v.audience
}

func (v *Verifier) refresh(ctx context.Context) error {
	v.attempted = time.Now()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, v.issuer+"/cdn-cgi/access/certs", nil)
	if err != nil {
		return err
	}
	resp, err := v.client.Do(req)
	if err != nil {
		return errors.New("Access certs unavailable")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return errors.New("Access certs unavailable")
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxDocument+1))
	if err != nil || len(b) > maxDocument {
		return errors.New("Access cert document unreadable or too large")
	}
	if _, err = object(b); err != nil {
		return err
	}
	var doc struct {
		Keys []json.RawMessage `json:"keys"`
	}
	if err = json.Unmarshal(b, &doc); err != nil || len(doc.Keys) == 0 || len(doc.Keys) > 128 {
		return errors.New("invalid Access key set")
	}
	keys := make(map[string]*rsa.PublicKey, len(doc.Keys))
	for _, raw := range doc.Keys {
		if _, err = object(raw); err != nil {
			return err
		}
		var k struct {
			Kty, Kid, Use, Alg, N, E string
			KeyOps                   []string `json:"key_ops"`
		}
		if err = json.Unmarshal(raw, &k); err != nil || k.Kty != "RSA" || k.Kid == "" || len(k.Kid) > 256 || keys[k.Kid] != nil || (k.Use != "" && k.Use != "sig") || (k.Alg != "" && k.Alg != "RS256") {
			return errors.New("invalid Access signing key")
		}
		if len(k.KeyOps) > 0 && (len(k.KeyOps) != 1 || k.KeyOps[0] != "verify") {
			return errors.New("invalid Access key operations")
		}
		nb, ne := base64.RawURLEncoding.Strict().DecodeString(k.N)
		eb, ee := base64.RawURLEncoding.Strict().DecodeString(k.E)
		n, e := new(big.Int).SetBytes(nb), new(big.Int).SetBytes(eb)
		if ne != nil || ee != nil || n.BitLen() < 2048 || n.BitLen() > 8192 || n.Bit(0) == 0 || !e.IsInt64() || e.Int64() < 3 || e.Int64() > 2147483647 || e.Bit(0) == 0 {
			return errors.New("invalid Access RSA key")
		}
		keys[k.Kid] = &rsa.PublicKey{N: n, E: int(e.Int64())}
	}
	v.keys, v.refreshed = keys, time.Now()
	return nil
}

func (v *Verifier) key(ctx context.Context, token *jwt.Token) (any, error) {
	kid, ok := token.Header["kid"].(string)
	if !ok || kid == "" || len(kid) > 256 {
		return nil, errors.New("invalid Access key ID")
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	expired := time.Since(v.refreshed) >= cacheLifetime
	if expired || (v.keys[kid] == nil && time.Since(v.attempted) >= refreshInterval) {
		if time.Since(v.attempted) < refreshInterval {
			return nil, errors.New("Access key refresh unavailable")
		}
		if err := v.refresh(ctx); err != nil {
			return nil, err
		}
	}
	if v.keys[kid] == nil {
		return nil, errors.New("unknown Access signing key")
	}
	return v.keys[kid], nil
}

// object rejects duplicate JSON members (including case aliases accepted by Go's
// decoder), so a signed document never has ambiguous security fields.
func object(raw []byte) (map[string]json.RawMessage, error) {
	if !utf8.Valid(raw) {
		return nil, errors.New("invalid Access JSON encoding")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	start, err := dec.Token()
	if err != nil || start != json.Delim('{') {
		return nil, errors.New("invalid Access JSON object")
	}
	fields := map[string]json.RawMessage{}
	for dec.More() {
		name, err := dec.Token()
		if err != nil {
			return nil, errors.New("invalid Access JSON member")
		}
		key, ok := name.(string)
		key = strings.ToLower(key)
		if !ok || fields[key] != nil {
			return nil, errors.New("duplicate Access JSON member")
		}
		var value json.RawMessage
		if err = dec.Decode(&value); err != nil {
			return nil, errors.New("invalid Access JSON value")
		}
		fields[key] = value
	}
	if _, err = dec.Token(); err != nil {
		return nil, errors.New("invalid Access JSON end")
	}
	if _, err = dec.Token(); err != io.EOF {
		return nil, errors.New("trailing Access JSON")
	}
	return fields, nil
}

func (v *Verifier) Verify(ctx context.Context, raw string) (*Claims, error) {
	invalid := errors.New("invalid Access assertion")
	if len(raw) == 0 || len(raw) > maxJWT || strings.ContainsAny(raw, " \r\n\t") {
		return nil, invalid
	}
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		return nil, invalid
	}
	for index, part := range parts[:2] {
		decoded, err := base64.RawURLEncoding.Strict().DecodeString(part)
		if err != nil {
			return nil, invalid
		}
		fields, err := object(decoded)
		if err != nil {
			return nil, invalid
		}
		if index == 0 {
			if fields["crit"] != nil || fields["b64"] != nil {
				return nil, invalid
			}
		} else {
			for _, key := range []string{"exp", "nbf", "iat"} {
				if value, present := fields[key]; present {
					value = bytes.TrimSpace(value)
					if len(value) == 0 || (value[0] != '-' && (value[0] < '0' || value[0] > '9')) {
						return nil, invalid
					}
				}
			}
		}
	}
	claims := new(Claims)
	_, err := jwt.ParseWithClaims(raw, claims, func(token *jwt.Token) (any, error) { return v.key(ctx, token) },
		jwt.WithValidMethods([]string{"RS256"}), jwt.WithIssuer(v.issuer), jwt.WithAudience(v.audience), jwt.WithExpirationRequired(), jwt.WithIssuedAt(), jwt.WithStrictDecoding())
	if err != nil || strings.TrimSpace(claims.Subject) == "" || len(claims.Subject) > 512 || strings.ContainsFunc(claims.Subject, unicode.IsControl) {
		return nil, invalid
	}
	claims.Email, err = config.AccessEmail(claims.Email)
	if err != nil {
		return nil, invalid
	}
	return claims, nil
}

// Principal binds sessions to the verified subject and application, with a
// separate namespace and hashed email. It fits existing wire-v1's 128-byte limit.
func (v *Verifier) Principal(resource, email, subject string) string {
	return v.emailPrincipal(resource, email) + "~" + config.Hash(subject)[:24]
}
func (v *Verifier) emailPrincipal(resource, email string) string {
	b, _ := json.Marshal([]string{v.issuer, v.audience, resource, strings.ToLower(email)})
	return "access~" + config.Hash(string(b))
}
func (v *Verifier) MatchesPrincipal(principal, resource, email string) bool {
	prefix := v.emailPrincipal(resource, email) + "~"
	if !strings.HasPrefix(principal, prefix) || len(principal) != len(prefix)+24 {
		return false
	}
	_, err := hex.DecodeString(strings.TrimPrefix(principal, prefix))
	return err == nil
}
