package main

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/soren-labs/hostmcp/internal/config"
)

func TestComponentRolesRejectUnsupportedCommands(t *testing.T) {
	for _, tc := range []struct {
		role, command string
		allow         bool
	}{
		{"all", "stdio", true}, {"all", "agent", true}, {"hub", "hub", true}, {"hub", "token", true}, {"hub", "agent", false}, {"hub", "stdio", false}, {"agent", "agent", true}, {"agent", "hub", false}, {"agent", "token", false}, {"agent", "stdio", false}, {"agent", "version", true}, {"unknown", "version", false},
	} {
		if err := checkRole(tc.role, tc.command); (err == nil) != tc.allow {
			t.Fatalf("%s %s: %v", tc.role, tc.command, err)
		}
	}
	previous := component
	component = "agent"
	defer func() { component = previous }()
	if err := run(context.Background(), []string{"hub", "init"}); err == nil {
		t.Fatal("run bypassed component check")
	}
	if err := serviceCommand([]string{"install", "--role", "hub", "--config", "/unused"}); err == nil {
		t.Fatal("service installation bypassed component check")
	}
}
func TestOAuthCLIConfigurePreservesLegacyConfiguration(t *testing.T) {
	file, store := testHub(t)
	before, err := store.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	policy := config.OAuth{Issuer: "https://issuer.example", Scopes: []string{"hostmcp:tools"}, Principals: []config.OAuthPrincipal{{Subject: "subject", Devices: []string{"node"}}}}
	policyFile := filepath.Join(t.TempDir(), "oauth.json")
	if err = config.WriteJSON(policyFile, policy, false); err != nil {
		t.Fatal(err)
	}
	if err = hubCommand(context.Background(), []string{"configure", "--config", file, "--oauth-file", policyFile}); err != nil {
		t.Fatal(err)
	}
	after, err := store.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if after.OAuth == nil || after.Clients[0].TokenHash != before.Clients[0].TokenHash || after.Version != before.Version {
		t.Fatal("OAuth changed existing credentials or format")
	}
	if err = hubCommand(context.Background(), []string{"configure", "--config", file, "--disable-oauth"}); err != nil {
		t.Fatal(err)
	}
	after, err = store.Snapshot()
	if err != nil || after.OAuth != nil {
		t.Fatal("OAuth disable failed", err)
	}
	policy.Principals[0].Devices = []string{"*"}
	if err = config.WriteJSON(policyFile, policy, true); err != nil {
		t.Fatal(err)
	}
	if err = hubCommand(context.Background(), []string{"configure", "--config", file, "--oauth-file", policyFile}); err == nil {
		t.Fatal("wildcard OAuth mapping accepted")
	}
}

func TestComponentUsageOnlyShowsSupportedCommands(t *testing.T) {
	for _, role := range []string{"hub", "agent"} {
		text := roleUsage(role)
		if strings.Contains(text, "hostmcp stdio") || strings.Contains(text, "hostmcp-"+role+" stdio") {
			t.Fatal("unsupported stdio in component usage")
		}
		if role == "agent" && strings.Contains(text, "hostmcp-agent hub") {
			t.Fatal("Hub command shown for Agent")
		}
		if !strings.Contains(text, "hostmcp-"+role+" version") {
			t.Fatal("missing component usage")
		}
	}
}
