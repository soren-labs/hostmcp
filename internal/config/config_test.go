package config

import (
	"encoding/json"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

func fixture(t *testing.T) (*Store, string) {
	t.Helper()
	token, _ := NewToken()
	path := filepath.Join(t.TempDir(), "hub.json")
	h := Hub{Version: 1, Listen: "127.0.0.1:8787", PublicURL: "http://localhost:8787", Clients: []Client{{ID: "owner", TokenHash: Hash(token), Devices: []string{"*"}}}}
	if err := WriteJSON(path, h, false); err != nil {
		t.Fatal(err)
	}
	return NewStore(path), token
}

func TestEnrollmentSingleUseAcrossStores(t *testing.T) {
	s, clientToken := fixture(t)
	invite, err := s.InviteDevice("mac", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	var accepted atomic.Int32
	var wg sync.WaitGroup
	for n := 0; n < 12; n++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			other := NewStore(s.Path)
			agent, err := other.Redeem("mac", invite.Token)
			if err == nil {
				accepted.Add(1)
				if !other.AuthDevice("mac", agent.Token) {
					t.Error("enrolled token rejected")
				}
			}
		}()
	}
	wg.Wait()
	if accepted.Load() != 1 {
		t.Fatalf("single-use invitation redeemed %d times", accepted.Load())
	}
	if _, ok := s.AuthClient(clientToken); !ok {
		t.Fatal("client token rejected")
	}
	if _, ok := s.AuthClient(invite.Token); ok {
		t.Fatal("enrollment token authenticated as client")
	}
	bytes, err := os.ReadFile(s.Path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(bytes), clientToken) || strings.Contains(string(bytes), invite.Token) {
		t.Fatal("plaintext credentials persisted in hub")
	}
}

func TestScopeAndRevocation(t *testing.T) {
	s, _ := fixture(t)
	invite, _ := s.InviteDevice("linux", time.Minute)
	a, err := s.Redeem("linux", invite.Token)
	if err != nil {
		t.Fatal(err)
	}
	if !s.AuthDevice("linux", a.Token) || s.AuthDevice("mac", a.Token) {
		t.Fatal("device authentication mismatch")
	}
	if err = s.Update(func(h *Hub) error { h.Devices = nil; return nil }); err != nil {
		t.Fatal(err)
	}
	if s.AuthDevice("linux", a.Token) {
		t.Fatal("revoked token accepted")
	}
	if !Allows(Client{Devices: []string{"linux"}}, "linux") || Allows(Client{Devices: []string{"linux"}}, "mac") {
		t.Fatal("scope check failed")
	}
}

func TestConfigurationPermissionsAndNoClobber(t *testing.T) {
	s, _ := fixture(t)
	st, _ := os.Stat(s.Path)
	if st.Mode().Perm() != 0600 {
		t.Fatal("file not private")
	}
	if err := WriteSecret(s.Path, []byte("other"), false); err == nil {
		t.Fatal("overwrote config")
	}
	if err := os.Chmod(s.Path, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Snapshot(); err == nil {
		t.Fatal("accepted world-readable config")
	}
	link := filepath.Join(filepath.Dir(s.Path), "link")
	if err := os.Symlink(s.Path, link); err != nil {
		t.Fatal(err)
	}
	if err := WriteSecret(link, []byte("other"), true); err == nil {
		t.Fatal("replaced symlink")
	}
}

func TestURLAndExpiredInvite(t *testing.T) {
	for _, raw := range []string{"http://example.com", "https://user@example.com", "https://example.com/mcp", "https://example.com?token=secret", "file:///tmp"} {
		if _, err := CheckURL(raw, false); err == nil {
			t.Errorf("accepted %s", raw)
		}
	}
	if _, err := CheckURL("http://127.0.0.1:8787", false); err != nil {
		t.Fatal(err)
	}
	s, _ := fixture(t)
	i, _ := s.InviteDevice("linux", time.Minute)
	if err := s.Update(func(h *Hub) error { h.Invitations[0].ExpiresAt = time.Now().Add(-time.Minute); return nil }); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Redeem("linux", i.Token); err == nil {
		t.Fatal("expired invite accepted")
	}
	b, _ := json.Marshal(i)
	if !strings.Contains(string(b), "hub_url") {
		t.Fatal("wire config missing hub_url")
	}
}

func TestAccountBindingSurvivesInvitationAndEnrollment(t *testing.T) {
	s, _ := fixture(t)
	invite, err := s.InviteDeviceForUser("workstation-root", "root", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if invite.ExpectedUser != "root" {
		t.Fatalf("invitation lost account binding: %+v", invite)
	}
	a, err := s.Redeem(invite.DeviceID, invite.Token)
	if err != nil {
		t.Fatal(err)
	}
	if a.ExpectedUser != "root" || !s.DeviceValidForUser(a.DeviceID, Hash(a.Token), "root") {
		t.Fatal("enrolled node lost its account binding")
	}
	if s.DeviceValidForUser(a.DeviceID, Hash(a.Token), "dev") {
		t.Fatal("ordinary account accepted for a root node")
	}
	if s.DeviceValidForUser(a.DeviceID, Hash("wrong-token"), "root") {
		t.Fatal("account name bypassed the device credential")
	}
	legacy, err := s.InviteDevice("workstation-user", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	unbound, err := s.Redeem(legacy.DeviceID, legacy.Token)
	if err != nil || unbound.ExpectedUser != "" || !s.DeviceValidForUser(unbound.DeviceID, Hash(unbound.Token), "dev") {
		t.Fatalf("legacy invitation was not compatible: %v", err)
	}
	for _, name := range []string{"root\n", "two users", "../root", "user:group", "a\\b", strings.Repeat("u", 257)} {
		if _, err := s.InviteDeviceForUser("invalid-user", name, time.Minute); err == nil {
			t.Errorf("accepted invalid account %q", name)
		}
	}
}

func TestConfigurationLockRejectsSymlink(t *testing.T) {
	s, _ := fixture(t)
	other := filepath.Join(filepath.Dir(s.Path), "unrelated")
	if err := os.WriteFile(other, []byte("unchanged"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(other, s.Path+".lock"); err != nil {
		t.Fatal(err)
	}
	if err := s.Update(func(*Hub) error { t.Fatal("followed lock symlink"); return nil }); err == nil {
		t.Fatal("accepted a lock symlink")
	}
	b, err := os.ReadFile(other)
	if err != nil || string(b) != "unchanged" {
		t.Fatalf("changed unrelated lock target: %q %v", b, err)
	}
}

// This helper is executed as a real unprivileged account by the root-only test
// below. Keeping it in a subprocess avoids changing the test runner's UID.
func TestConfigurationOwnershipHelper(t *testing.T) {
	path := os.Getenv("HOSTMCP_TEST_OWNER_PATH")
	if path == "" {
		return
	}
	switch os.Getenv("HOSTMCP_TEST_OWNER_ACTION") {
	case "update":
		s := NewStore(path)
		if _, err := s.Snapshot(); err != nil {
			t.Fatal(err)
		}
		if err := s.Update(func(h *Hub) error { h.Listen = "127.0.0.1:8899"; return nil }); err != nil {
			t.Fatal(err)
		}
	case "replace-denied":
		err := WriteSecret(path, []byte("must not replace the original"), true)
		if err == nil || !strings.Contains(err.Error(), "cannot preserve configuration owner") {
			t.Fatalf("expected an ownership failure, got %v", err)
		}
	default:
		t.Fatal("unknown ownership helper action")
	}
}

func TestRootUpdatePreservesConfigurationOwnership(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires root to verify maintenance across actual OS accounts")
	}
	account, err := user.Lookup("nobody")
	if err != nil {
		t.Skipf("unprivileged test account unavailable: %v", err)
	}
	uid, err := strconv.ParseUint(account.Uid, 10, 32)
	if err != nil {
		t.Skipf("unprivileged test UID unavailable: %v", err)
	}
	gid, err := strconv.ParseUint(account.Gid, 10, 32)
	if err != nil {
		t.Skipf("unprivileged test GID unavailable: %v", err)
	}
	base, err := os.MkdirTemp("", "hostmcp-ownership-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(base)
	if err := os.Chmod(base, 0755); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(base, "state")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chown(dir, int(uid), int(gid)); err != nil {
		t.Fatal(err)
	}
	// Go's temporary build directory is not necessarily traversable by nobody.
	// Copy only this test executable into our disposable, readable fixture.
	binary := filepath.Join(base, "ownership-test")
	contents, err := os.ReadFile(os.Args[0])
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(binary, contents, 0755); err != nil {
		t.Fatal(err)
	}
	runAsOwner := func(path, action string) {
		t.Helper()
		cmd := exec.Command(binary, "-test.run=^TestConfigurationOwnershipHelper$", "-test.v")
		cmd.Dir = base
		cmd.Env = append(os.Environ(), "HOSTMCP_TEST_OWNER_PATH="+path, "HOSTMCP_TEST_OWNER_ACTION="+action)
		cmd.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: uint32(uid), Gid: uint32(gid), Groups: []uint32{uint32(gid)}}}
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("unprivileged %s: %v\n%s", action, err, out)
		}
	}
	path := filepath.Join(dir, "hub.json")
	cfg := Hub{Version: FormatVersion, Listen: "127.0.0.1:8787", PublicURL: "http://localhost:8787"}
	if err := WriteJSON(path, cfg, false); err != nil {
		t.Fatal(err)
	}
	if err := os.Chown(path, int(uid), int(gid)); err != nil {
		t.Fatal(err)
	}
	assertOwner := func(path string) {
		t.Helper()
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		stat := info.Sys().(*syscall.Stat_t)
		if stat.Uid != uint32(uid) || stat.Gid != uint32(gid) || info.Mode().Perm() != 0600 {
			t.Fatalf("owner/mode changed for %s: %d:%d %o", path, stat.Uid, stat.Gid, info.Mode().Perm())
		}
	}
	s := NewStore(path)
	for _, staleRootLock := range []bool{false, true} {
		if staleRootLock {
			if err := os.Chown(path+".lock", 0, 0); err != nil {
				t.Fatal(err)
			}
		}
		if err := s.Update(func(h *Hub) error { h.Listen = "127.0.0.1:8788"; return nil }); err != nil {
			t.Fatal(err)
		}
		assertOwner(path)
		assertOwner(path + ".lock")
		runAsOwner(path, "update")
		assertOwner(path)
		assertOwner(path + ".lock")
		updated, err := s.Snapshot()
		if err != nil || updated.Listen != "127.0.0.1:8899" {
			t.Fatalf("owner could not update after root: %+v %v", updated, err)
		}
	}
	// An unprivileged account may own the containing directory without owning a
	// particular file in it. Failed ownership preservation must not clobber it.
	rootFile := filepath.Join(dir, "root-owned")
	if err := WriteSecret(rootFile, []byte("original content"), false); err != nil {
		t.Fatal(err)
	}
	runAsOwner(rootFile, "replace-denied")
	b, err := os.ReadFile(rootFile)
	if err != nil || string(b) != "original content" {
		t.Fatalf("failed replacement changed the original: %q %v", b, err)
	}
	if info, err := os.Stat(rootFile); err != nil || info.Sys().(*syscall.Stat_t).Uid != 0 {
		t.Fatal("failed replacement changed the original owner")
	}
}
