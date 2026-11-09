package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/soren-labs/rmc/internal/update"
)

func TestUpdateSettingsCommands(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "node.json")
	settings, state := update.Paths(cfg)
	if settings != filepath.Join(dir, "updates.json") || state != filepath.Join(dir, "node.update-state.json") {
		t.Fatalf("paths %s %s", settings, state)
	}
	if s, err := update.LoadSettings(settings); err != nil || !s.Enabled() || s.ChannelName() != "stable" {
		t.Fatalf("default settings must be automatic stable updates: %+v %v", s, err)
	}
	steps := []struct {
		args    []string
		enabled bool
		channel string
	}{
		{[]string{"disable"}, false, "stable"},
		{[]string{"channel", "canary"}, false, "canary"},
		{[]string{"enable"}, true, "canary"},
		{[]string{"channel", "stable"}, true, "stable"},
	}
	for _, step := range steps {
		if err := autoUpdateCommand(context.Background(), append(step.args, "--config", cfg)); err != nil {
			t.Fatalf("%v: %v", step.args, err)
		}
		s, err := update.LoadSettings(settings)
		if err != nil || s.Enabled() != step.enabled || s.ChannelName() != step.channel {
			t.Fatalf("%v: %+v %v", step.args, s, err)
		}
	}
	if st, err := os.Stat(settings); err != nil || st.Mode().Perm() != 0600 {
		t.Fatalf("settings must be private: %v %v", st, err)
	}
	for _, bad := range [][]string{{"channel", "beta"}, {"channel"}, {"frobnicate"}, {}} {
		if err := autoUpdateCommand(context.Background(), append(bad, "--config", cfg)); err == nil {
			t.Fatalf("accepted %v", bad)
		}
	}
}

func TestReleaseSignAndVerifyCommands(t *testing.T) {
	dir := t.TempDir()
	pub, priv, err := update.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	key := filepath.Join(dir, "release.key")
	if err = os.WriteFile(key, []byte(priv+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	sums := filepath.Join(dir, "SHA256SUMS")
	if err = os.WriteFile(sums, []byte(strings.Repeat("a", 64)+"  rmc_0.3.0_linux_amd64.tar.gz\n"), 0644); err != nil {
		t.Fatal(err)
	}
	sig := filepath.Join(dir, "SHA256SUMS.sig")
	if err = releaseCommand([]string{"sign", "--key", key, "--tag", "v0.3.0", "--sums", sums, "--output", sig}); err != nil {
		t.Fatal(err)
	}
	if err = releaseCommand([]string{"verify", "--tag", "v0.3.0", "--sums", sums, "--sig", sig, "--public-key", pub}); err != nil {
		t.Fatal(err)
	}
	if err = releaseCommand([]string{"verify", "--tag", "v0.3.1", "--sums", sums, "--sig", sig, "--public-key", pub}); err == nil {
		t.Fatal("signature verified for another tag")
	}
	// The compiled-in keys do not include this test key.
	if err = releaseCommand([]string{"verify", "--tag", "v0.3.0", "--sums", sums, "--sig", sig}); err == nil {
		t.Fatal("test signature verified against the release keys")
	}
	if err = releaseCommand([]string{"sign", "--key", key, "--tag", "v0.3.0", "--sums", sums, "--output", sig}); err == nil {
		t.Fatal("overwrote an existing signature")
	}
	if err = os.Chmod(key, 0644); err != nil {
		t.Fatal(err)
	}
	if err = releaseCommand([]string{"sign", "--key", key, "--tag", "v0.3.0", "--sums", sums, "--output", filepath.Join(dir, "other.sig")}); err == nil {
		t.Fatal("signed with a group/other readable key")
	}
}

func TestUpdateCommandInComponentBuilds(t *testing.T) {
	for _, role := range []string{"hub", "node"} {
		if err := checkRole(role, "update"); err != nil {
			t.Fatalf("%s: %v", role, err)
		}
		if err := checkRole(role, "release"); err == nil {
			t.Fatalf("%s build offers release signing", role)
		}
		if !strings.Contains(roleUsage(role), "rmc-"+role+" update status") || strings.Contains(roleUsage(role), "release keygen") {
			t.Fatalf("%s usage:\n%s", role, roleUsage(role))
		}
	}
}
