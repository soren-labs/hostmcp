package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestSnapshotSeesRevocationWithPreservedMetadata(t *testing.T) {
	s, oldToken := fixture(t)
	if _, ok := s.AuthClient(oldToken); !ok {
		t.Fatal("initial credential rejected")
	}
	before, err := os.Stat(s.Path)
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(s.Path)
	if err != nil {
		t.Fatal(err)
	}
	newToken := strings.Repeat("n", 43)
	changed := strings.Replace(string(b), Hash(oldToken), Hash(newToken), 1)
	if len(changed) != len(b) || changed == string(b) {
		t.Fatal("fixture must change only the equal-length credential digest")
	}
	if err := os.WriteFile(s.Path, []byte(changed), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(s.Path, before.ModTime(), before.ModTime()); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.AuthClient(oldToken); ok {
		t.Fatal("cached policy accepted the revoked credential")
	}
	if _, ok := s.AuthClient(newToken); !ok {
		t.Fatal("updated credential was not immediately accepted")
	}
	// A separate CLI process uses its own Store and replaces the file atomically.
	other := NewStore(s.Path)
	if err := other.Update(func(h *Hub) error { h.Clients = nil; return nil }); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.AuthClient(newToken); ok {
		t.Fatal("cached policy missed another store's atomic revocation")
	}
}

func TestSnapshotCacheFailsClosed(t *testing.T) {
	changes := map[string]func(string) error{
		"permissions":    func(p string) error { return os.Chmod(p, 0644) },
		"missing":        os.Remove,
		"malformed":      func(p string) error { return os.WriteFile(p, []byte("{"), 0600) },
		"invalid_policy": func(p string) error { return os.WriteFile(p, []byte(`{"version":99}`), 0600) },
		"oversized":      func(p string) error { return os.WriteFile(p, []byte(strings.Repeat(" ", maxConfigBytes+1)), 0600) },
	}
	for name, change := range changes {
		t.Run(name, func(t *testing.T) {
			s, token := fixture(t)
			if _, ok := s.AuthClient(token); !ok {
				t.Fatal("initial credential rejected")
			}
			if err := change(s.Path); err != nil {
				t.Fatal(err)
			}
			if _, err := s.Snapshot(); err == nil {
				t.Fatal("invalid current file was hidden by cached policy")
			}
			if _, ok := s.AuthClient(token); ok {
				t.Fatal("read/validation failure reused stale credentials")
			}
		})
	}
}

func TestSnapshotOwnsNestedPolicy(t *testing.T) {
	s, _ := fixture(t)
	h, err := s.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	h.AllowedOrigins = []string{"https://client.example"}
	h.AllowedHosts = []string{"localhost:8787"}
	h.Devices = []Device{{ID: "node", TokenHash: Hash("test-device-credential")}}
	h.Invitations = []Invitation{{DeviceID: "invite", TokenHash: Hash("test-invite-credential"), ExpiresAt: time.Now().Add(time.Hour).UTC()}}
	h.OAuth = &OAuth{Issuer: "https://issuer.example", Scopes: []string{"tools"}, Principals: []OAuthPrincipal{{Subject: "test-subject", Devices: []string{"node"}}}}
	h.CloudflareAccess = &CloudflareAccess{TeamDomain: "https://example.cloudflareaccess.com", Audience: "test-audience", Principals: []CloudflareAccessPrincipal{{Email: "test@example.com", Devices: []string{"node"}}}}
	if err := WriteJSON(s.Path, h, true); err != nil {
		t.Fatal(err)
	}
	want, err := s.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	change := func(got *Hub) {
		got.AllowedOrigins[0] = "https://changed.example"
		got.AllowedHosts[0] = "changed.example"
		got.Clients[0].TokenHash = Hash("changed")
		got.Clients[0].Devices[0] = "changed"
		got.Devices[0].ExpectedUser = "changed"
		got.Invitations[0].DeviceID = "changed"
		got.OAuth.Issuer = "https://changed.example"
		got.OAuth.Scopes[0] = "changed"
		got.OAuth.Principals[0].Subject = "changed"
		got.OAuth.Principals[0].Devices[0] = "changed"
		got.CloudflareAccess.Audience = "changed"
		got.CloudflareAccess.Principals[0].Email = "changed@example.com"
		got.CloudflareAccess.Principals[0].Devices[0] = "changed"
	}
	var workers sync.WaitGroup
	for range 8 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for range 10 {
				got, err := s.Snapshot()
				if err != nil {
					t.Error(err)
					return
				}
				if !reflect.DeepEqual(got, want) {
					t.Error("a caller mutated another caller's policy")
					return
				}
				change(&got)
			}
		}()
	}
	workers.Wait()
	aborted := errors.New("abort update")
	if err := s.Update(func(got *Hub) error { change(got); return aborted }); !errors.Is(err, aborted) {
		t.Fatalf("Update returned %v", err)
	}
	got, err := s.Snapshot()
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatal("an aborted update changed cached authorization policy")
	}
}

func BenchmarkSnapshot(b *testing.B) {
	for _, large := range []bool{false, true} {
		name := "small"
		if large {
			name = "large"
		}
		b.Run(name, func(b *testing.B) {
			h := Hub{Version: FormatVersion, Listen: "127.0.0.1:8787", PublicURL: "http://localhost:8787", Clients: []Client{{ID: "test-client", TokenHash: Hash("test-credential"), Devices: []string{"*"}}}}
			if large {
				h.Clients = nil
				for i := range 1024 {
					h.Devices = append(h.Devices, Device{ID: fmt.Sprintf("node-%04d", i), TokenHash: Hash(fmt.Sprint(i))})
					h.Invitations = append(h.Invitations, Invitation{DeviceID: fmt.Sprintf("invite-%04d", i), TokenHash: Hash(fmt.Sprint(i)), ExpiresAt: time.Now().Add(time.Hour)})
				}
				for i := range 256 {
					c := Client{ID: fmt.Sprintf("client-%03d", i), TokenHash: Hash(fmt.Sprint(i))}
					for j := range 96 {
						c.Devices = append(c.Devices, h.Devices[j].ID)
					}
					h.Clients = append(h.Clients, c)
				}
			}
			path := filepath.Join(b.TempDir(), "hub.json")
			if err := WriteJSON(path, h, false); err != nil {
				b.Fatal(err)
			}
			store := NewStore(path)
			if _, err := store.Snapshot(); err != nil {
				b.Fatal(err)
			}
			for _, cached := range []bool{false, true} {
				mode := "parse_each_time"
				if cached {
					mode = "reuse_parsed_policy"
				}
				b.Run(mode, func(b *testing.B) {
					b.ReportAllocs()
					for b.Loop() {
						var err error
						if cached {
							_, err = store.Snapshot()
						} else {
							var snapshot Hub
							err = ReadJSON(path, &snapshot)
							if err == nil {
								err = validate(snapshot)
							}
						}
						if err != nil {
							b.Fatal(err)
						}
					}
				})
			}
		})
	}
}
