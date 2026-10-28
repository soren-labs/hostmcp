package main

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/soren-labs/hostmcp/internal/config"
)

func testHub(t *testing.T) (string, *config.Store) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "hub.json")
	if err := hubCommand(context.Background(), []string{"init", "--config", path}); err != nil {
		t.Fatal(err)
	}
	return path, config.NewStore(path)
}

func readToken(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(path)
	if err != nil || st.Mode().Perm() != 0600 {
		t.Fatalf("credential must be owner-only: %v", err)
	}
	return strings.TrimSpace(string(b))
}

func TestTokenLifecyclePreservesIdentityAndChangesAccess(t *testing.T) {
	path, store := testHub(t)
	oldPath := filepath.Join(filepath.Dir(path), "worker.token")
	if err := tokenCommand([]string{"create", "--config", path, "--id", "coding-agent", "--devices", "mac-user,linux-user", "--output", oldPath}); err != nil {
		t.Fatal(err)
	}
	oldToken := readToken(t, oldPath)
	c, ok := store.AuthClient(oldToken)
	if !ok || c.ID != "coding-agent" || !config.Allows(c, "mac-user") || config.Allows(c, "linux-root") {
		t.Fatal("initial node authorization is incorrect")
	}
	if err := tokenCommand([]string{"update", "--config", path, "--id", c.ID, "--devices", "linux-root"}); err != nil {
		t.Fatal(err)
	}
	c, ok = store.AuthClient(oldToken)
	if !ok || !config.Allows(c, "linux-root") || config.Allows(c, "mac-user") {
		t.Fatal("authorization update did not take effect with the existing credential")
	}
	if err := tokenCommand([]string{"update", "--config", path, "--id", c.ID, "--devices", "../invalid"}); err == nil {
		t.Fatal("invalid device scope was accepted")
	}
	newPath := filepath.Join(filepath.Dir(path), "rotated.token")
	if err := tokenCommand([]string{"rotate", "--config", path, "--id", c.ID, "--output", newPath}); err != nil {
		t.Fatal(err)
	}
	newToken := readToken(t, newPath)
	if _, ok := store.AuthClient(oldToken); ok {
		t.Fatal("rotated credential still authenticates")
	}
	c, ok = store.AuthClient(newToken)
	if !ok || c.ID != "coding-agent" || !config.Allows(c, "linux-root") || config.Allows(c, "mac-user") {
		t.Fatal("rotation changed identity or failed to preserve authorized nodes")
	}
	if err := tokenCommand([]string{"rotate", "--config", path, "--id", c.ID, "--output", newPath}); err == nil {
		t.Fatal("rotation overwrote an existing credential file")
	}
	if _, ok := store.AuthClient(newToken); !ok {
		t.Fatal("failed rotation changed active credential")
	}
	if err := tokenCommand([]string{"revoke", "--config", path, "--id", c.ID}); err != nil {
		t.Fatal(err)
	}
	if _, ok := store.AuthClient(newToken); ok {
		t.Fatal("revoked credential still authenticates")
	}
}

func TestTokenListDoesNotExposeCredentials(t *testing.T) {
	path, store := testHub(t)
	out, err := os.CreateTemp(t.TempDir(), "stdout")
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	original := os.Stdout
	defer func() { os.Stdout = original }()
	os.Stdout = out
	err = tokenCommand([]string{"list", "--config", path})
	os.Stdout = original
	if err != nil {
		t.Fatal(err)
	}
	if _, err := out.Seek(0, 0); err != nil {
		t.Fatal(err)
	}
	b, err := io.ReadAll(out)
	if err != nil {
		t.Fatal(err)
	}
	var listed struct {
		Clients []struct {
			ID      string   `json:"id"`
			Devices []string `json:"devices"`
		} `json:"clients"`
	}
	if err := json.Unmarshal(b, &listed); err != nil || len(listed.Clients) != 1 || listed.Clients[0].ID != "owner" {
		t.Fatalf("unexpected public inventory: %s (%v)", b, err)
	}
	h, err := store.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "token") || strings.Contains(string(b), h.Clients[0].TokenHash) || strings.Contains(string(b), readToken(t, path+".owner-token")) {
		t.Fatal("client inventory leaked credential material")
	}
}

func TestRotateUnknownClientLeavesNoCredential(t *testing.T) {
	path, store := testHub(t)
	out := filepath.Join(filepath.Dir(path), "missing.token")
	if err := tokenCommand([]string{"rotate", "--config", path, "--id", "missing", "--output", out}); err == nil {
		t.Fatal("unknown client was rotated")
	}
	if _, err := os.Stat(out); !os.IsNotExist(err) {
		t.Fatal("failed rotation left an unusable credential file")
	}
	if _, ok := store.AuthClient(readToken(t, path+".owner-token")); !ok {
		t.Fatal("failed rotation changed unrelated owner credential")
	}
}

func TestDeviceInviteBindsExpectedAccount(t *testing.T) {
	path, store := testHub(t)
	out := filepath.Join(filepath.Dir(path), "root-invite.json")
	if err := deviceCommand([]string{"invite", "--config", path, "--id", "linux-root", "--user", "root", "--output", out}); err != nil {
		t.Fatal(err)
	}
	var invite config.Invite
	if err := config.ReadJSON(out, &invite); err != nil {
		t.Fatal(err)
	}
	if invite.ExpectedUser != "root" {
		t.Fatal("CLI did not persist expected account")
	}
	agent, err := store.Redeem(invite.DeviceID, invite.Token)
	if err != nil || agent.ExpectedUser != "root" {
		t.Fatalf("enrollment did not preserve expected account: %v", err)
	}
}
