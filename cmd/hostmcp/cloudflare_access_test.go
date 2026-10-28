package main

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/soren-labs/hostmcp/internal/config"
)

func TestAccessCLIIndependentPolicies(t *testing.T) {
	file, store := testHub(t)
	if err := store.Update(func(h *config.Hub) error {
		h.OAuth = &config.OAuth{Issuer: "https://issuer.example", Scopes: []string{"hostmcp:tools"}, Principals: []config.OAuthPrincipal{{Subject: "owner", Devices: []string{"worker"}}}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	before, err := store.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	policy := config.CloudflareAccess{TeamDomain: "https://example-team.cloudflareaccess.com", Audience: "application-audience", Principals: []config.CloudflareAccessPrincipal{{Email: "owner@example.com", Devices: []string{"worker"}}}}
	policyFile := filepath.Join(t.TempDir(), "access.json")
	if err = config.WriteJSON(policyFile, policy, false); err != nil {
		t.Fatal(err)
	}
	runConfigure := func(flags ...string) error {
		return hubCommand(context.Background(), append([]string{"configure", "--config", file}, flags...))
	}
	if err = runConfigure("--cloudflare-access-file", policyFile); err != nil {
		t.Fatal(err)
	}
	after, err := store.Snapshot()
	if err != nil || !reflect.DeepEqual(after.CloudflareAccess, &policy) {
		t.Fatal("Access configure failed", err)
	}
	after.CloudflareAccess = nil
	if !reflect.DeepEqual(before, after) {
		t.Fatal("Access changed other settings, credentials or format")
	}
	if err = runConfigure("--disable-oauth"); err != nil {
		t.Fatal(err)
	}
	after, err = store.Snapshot()
	if err != nil || after.CloudflareAccess == nil || after.OAuth != nil {
		t.Fatal("OAuth disable affected Access", err)
	}
	oauthFile := filepath.Join(t.TempDir(), "oauth.json")
	if err = config.WriteJSON(oauthFile, before.OAuth, false); err != nil {
		t.Fatal(err)
	}
	if err = runConfigure("--oauth-file", oauthFile, "--disable-cloudflare-access"); err != nil {
		t.Fatal(err)
	}
	after, err = store.Snapshot()
	if err != nil || after.CloudflareAccess != nil || !reflect.DeepEqual(before.OAuth, after.OAuth) {
		t.Fatal("Access disable affected OAuth", err)
	}
	if err = runConfigure("--oauth-file", oauthFile, "--cloudflare-access-file", policyFile); err != nil {
		t.Fatal("independent enable flags conflict", err)
	}
	for _, flags := range [][]string{{"--cloudflare-access-file", policyFile, "--disable-cloudflare-access"}, {"--oauth-file", oauthFile, "--disable-oauth"}} {
		if err = runConfigure(flags...); err == nil {
			t.Fatal("conflicting flags accepted")
		}
	}
	stable, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	policy.Principals[0].Devices = []string{"*"}
	if err = config.WriteJSON(policyFile, policy, true); err != nil {
		t.Fatal(err)
	}
	if err = runConfigure("--cloudflare-access-file", policyFile); err == nil {
		t.Fatal("wildcard Access policy accepted")
	}
	unchanged, err := os.ReadFile(file)
	if err != nil || string(stable) != string(unchanged) {
		t.Fatal("failed policy update changed config", err)
	}
}
