// Package oauth implements a JWT resource server for an operator-selected issuer.
// It does not implement authorization redirects, passwords, or token issuance.
package oauth

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rsa"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/soren-labs/hostmcp/internal/config"
)

const maxDocument = 1 << 20

type Verifier struct {
	Issuer, Resource     string
	client               *http.Client
	jwks                 string
	mu                   sync.Mutex
	keys                 map[string]any
	refreshed, attempted time.Time
}
type Claims struct {
	jwt.RegisteredClaims
	ClientID        string   `json:"client_id"`
	AuthorizedParty string   `json:"azp"`
	Scope           string   `json:"scope"`
	SCP             []string `json:"scp"`
}

func (c *Claims) Scopes() []string { return append(strings.Fields(c.Scope), c.SCP...) }

func fetch(ctx context.Context, c *http.Client, endpoint string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	resp, err := c.Do(req)
	if err != nil {
		return errors.New("issuer endpoint unavailable")
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("issuer endpoint returned HTTP %d", resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxDocument+1))
	if err != nil || len(b) > maxDocument {
		return errors.New("issuer document exceeds limit or cannot be read")
	}
	return json.Unmarshal(b, out)
}

// New eagerly discovers the issuer and downloads keys; startup fails closed.
func New(ctx context.Context, issuer, resource string) (*Verifier, error) {
	if err := config.OAuthURL(issuer); err != nil {
		return nil, err
	}
	if err := config.OAuthURL(resource); err != nil {
		return nil, err
	}
	v := &Verifier{Issuer: issuer, Resource: resource, client: &http.Client{Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("issuer redirects refused") }}}
	u, _ := url.Parse(issuer)
	path := strings.TrimSuffix(u.Path, "/")
	base := u.Scheme + "://" + u.Host
	endpoints := []string{base + "/.well-known/oauth-authorization-server" + path, base + "/.well-known/openid-configuration" + path, strings.TrimSuffix(issuer, "/") + "/.well-known/openid-configuration"}
	var metadata struct {
		Issuer        string `json:"issuer"`
		JWKS          string `json:"jwks_uri"`
		Authorization string `json:"authorization_endpoint"`
		Token         string `json:"token_endpoint"`
	}
	var err error
	for _, endpoint := range endpoints {
		metadata = struct {
			Issuer        string `json:"issuer"`
			JWKS          string `json:"jwks_uri"`
			Authorization string `json:"authorization_endpoint"`
			Token         string `json:"token_endpoint"`
		}{}
		err = fetch(ctx, v.client, endpoint, &metadata)
		if err == nil {
			break
		}
	}
	if err != nil {
		return nil, fmt.Errorf("OAuth discovery failed: %w", err)
	}
	if metadata.Issuer != issuer {
		return nil, errors.New("OAuth discovery issuer mismatch")
	}
	for _, endpoint := range []string{metadata.JWKS, metadata.Authorization, metadata.Token} {
		if err = config.OAuthURL(endpoint); err != nil {
			return nil, fmt.Errorf("invalid issuer metadata endpoint: %w", err)
		}
	}
	v.jwks = metadata.JWKS
	if err = v.refresh(ctx); err != nil {
		return nil, fmt.Errorf("OAuth key discovery failed: %w", err)
	}
	return v, nil
}

func (v *Verifier) refresh(ctx context.Context) error {
	v.attempted = time.Now()
	var doc struct {
		Keys []struct {
			Kty, Kid, Use, Alg, N, E, Crv, X, Y string
			KeyOps                              []string `json:"key_ops"`
		} `json:"keys"`
	}
	if err := fetch(ctx, v.client, v.jwks, &doc); err != nil {
		return err
	}
	if len(doc.Keys) > 128 {
		return errors.New("too many signing keys")
	}
	keys := map[string]any{}
	decode := func(s string) *big.Int {
		b, e := base64.RawURLEncoding.DecodeString(s)
		if e != nil {
			return nil
		}
		return new(big.Int).SetBytes(b)
	}
	for _, k := range doc.Keys {
		if k.Kid == "" || len(k.Kid) > 256 || (k.Use != "" && k.Use != "sig") {
			continue
		}
		if len(k.KeyOps) > 0 {
			allowed := false
			for _, op := range k.KeyOps {
				if op == "verify" {
					allowed = true
				}
			}
			if !allowed {
				continue
			}
		}
		var key any
		switch k.Kty {
		case "RSA":
			if k.Alg != "" && k.Alg != "RS256" {
				continue
			}
			n, e := decode(k.N), decode(k.E)
			if n == nil || e == nil || n.BitLen() < 2048 || n.BitLen() > 8192 || !e.IsInt64() || e.Int64() < 3 || e.Int64() > 2147483647 || e.Bit(0) == 0 {
				continue
			}
			key = &rsa.PublicKey{N: n, E: int(e.Int64())}
		case "EC":
			if k.Crv != "P-256" || (k.Alg != "" && k.Alg != "ES256") {
				continue
			}
			x, y := decode(k.X), decode(k.Y)
			if x == nil || y == nil || !elliptic.P256().IsOnCurve(x, y) {
				continue
			}
			key = &ecdsa.PublicKey{Curve: elliptic.P256(), X: x, Y: y}
		default:
			continue
		}
		if keys[k.Kid] != nil {
			return errors.New("duplicate signing key ID")
		}
		keys[k.Kid] = key
	}
	if len(keys) == 0 {
		return errors.New("no usable RS256/ES256 signing keys")
	}
	v.keys = keys
	v.refreshed = time.Now()
	return nil
}
func (v *Verifier) key(ctx context.Context, t *jwt.Token) (any, error) {
	kid, ok := t.Header["kid"].(string)
	if !ok || kid == "" || len(kid) > 256 {
		return nil, errors.New("missing key ID")
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	// Bound network traffic from random kids; expire cached keys after five minutes.
	expired := time.Since(v.refreshed) >= 5*time.Minute
	if expired || (v.keys[kid] == nil && time.Since(v.attempted) >= 30*time.Second) {
		if time.Since(v.attempted) < 30*time.Second {
			return nil, errors.New("key discovery unavailable")
		}
		if err := v.refresh(ctx); err != nil {
			return nil, err
		}
	}
	if v.keys[kid] == nil {
		return nil, errors.New("unknown signing key")
	}
	return v.keys[kid], nil
}
func (v *Verifier) Verify(ctx context.Context, raw string) (*Claims, error) {
	if len(raw) > 16<<10 {
		return nil, errors.New("token too large")
	}
	claims := new(Claims)
	_, err := jwt.ParseWithClaims(raw, claims, func(t *jwt.Token) (any, error) { return v.key(ctx, t) }, jwt.WithValidMethods([]string{"RS256", "ES256"}), jwt.WithIssuer(v.Issuer), jwt.WithAudience(v.Resource), jwt.WithExpirationRequired(), jwt.WithIssuedAt())
	if err != nil || claims.Subject == "" || len(claims.Subject) > 512 {
		return nil, errors.New("invalid access token")
	}
	if claims.ClientID == "" {
		claims.ClientID = claims.AuthorizedParty
	}
	if claims.ClientID == "" || len(claims.ClientID) > 512 || (claims.AuthorizedParty != "" && claims.AuthorizedParty != claims.ClientID) {
		return nil, errors.New("invalid OAuth client identity")
	}
	return claims, nil
}

// Principal keeps OAuth identities separate from legacy credentials and other
// OAuth clients acting for the same subject. Refreshes retain session ownership.
func Principal(issuer, resource, subject, clientID string) string {
	return subjectPrincipal(issuer, resource, subject) + "~" + config.Hash(clientID)[:24]
}
func subjectPrincipal(issuer, resource, subject string) string {
	b, _ := json.Marshal([]string{issuer, resource, subject})
	return "oauth~" + config.Hash(string(b))
}
func MatchesPrincipal(principal, issuer, resource, subject string) bool {
	prefix := subjectPrincipal(issuer, resource, subject) + "~"
	if !strings.HasPrefix(principal, prefix) || len(principal) != len(prefix)+24 {
		return false
	}
	_, err := hex.DecodeString(strings.TrimPrefix(principal, prefix))
	return err == nil
}
