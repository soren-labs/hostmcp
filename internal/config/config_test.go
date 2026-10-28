package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
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
