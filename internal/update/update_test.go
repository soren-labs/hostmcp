package update

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"github.com/soren-labs/hostmcp/internal/testutil"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func testBinary(version, role string, wire int) []byte {
	b, _ := json.Marshal(Identity{Version: version, Component: role, WireVersion: wire, OS: runtime.GOOS, Arch: runtime.GOARCH})
	return []byte("#!/bin/sh\nprintf '%s\\n' '" + string(b) + "'\n")
}
func testArchive(t *testing.T, binary []byte, entry string, kind byte) []byte {
	t.Helper()
	var b bytes.Buffer
	gz := gzip.NewWriter(&b)
	tw := tar.NewWriter(gz)
	size := int64(len(binary))
	if kind != tar.TypeReg {
		size = 0
	}
	if err := tw.WriteHeader(&tar.Header{Name: entry, Mode: 0755, Size: size, Typeflag: kind, Linkname: "/tmp/never-touch"}); err != nil {
		t.Fatal(err)
	}
	if size > 0 {
		if _, err := tw.Write(binary); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}
func updaterFixture(t *testing.T) (Policy, []byte) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("updater intentionally refuses root")
	}
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	old := testBinary("0.2.1", "agent", 1)
	binary := filepath.Join(dir, "hostmcp-agent")
	if err = os.WriteFile(binary, old, 0755); err != nil {
		t.Fatal(err)
	}
	return Policy{Version: 1, Enabled: true, Channel: "stable", TargetVersion: "0.2.2", Binary: binary}, old
}
func pin(p *Policy, b []byte) { sum := sha256.Sum256(b); p.SHA256 = hex.EncodeToString(sum[:]) }
func TestUpdaterAtomicInstallRollbackAndNoop(t *testing.T) {
	p, old := updaterFixture(t)
	next := testBinary("0.2.2", "agent", 1)
	archive := testArchive(t, next, "hostmcp-agent", tar.TypeReg)
	pin(&p, archive)
	changed, err := Apply(context.Background(), p, bytes.NewReader(archive))
	if err != nil || !changed {
		t.Fatalf("changed=%v error=%v", changed, err)
	}
	got, _ := os.ReadFile(p.Binary)
	backup, _ := os.ReadFile(p.Binary + ".previous")
	if !bytes.Equal(got, next) || !bytes.Equal(backup, old) {
		t.Fatal("incorrect install or backup")
	}
	if changed, err = Apply(context.Background(), p, bytes.NewReader(nil)); err != nil || changed {
		t.Fatalf("idempotent update: %v %v", changed, err)
	}
	if err = Rollback(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	got, _ = os.ReadFile(p.Binary)
	if !bytes.Equal(got, old) {
		t.Fatal("rollback did not restore original")
	}
	if _, err = os.Stat(p.Binary + ".previous"); !os.IsNotExist(err) {
		t.Fatal("rollback did not consume backup")
	}
}
func TestUpdaterFailuresLeaveBinaryUnchanged(t *testing.T) {
	cases := []struct {
		name          string
		version, role string
		wire          int
		entry         string
		kind          byte
		corrupt       bool
	}{
		{"checksum", "0.2.2", "agent", 1, "hostmcp-agent", tar.TypeReg, true},
		{"wrong-version", "0.2.3", "agent", 1, "hostmcp-agent", tar.TypeReg, false},
		{"wrong-component", "0.2.2", "hub", 1, "hostmcp-agent", tar.TypeReg, false},
		{"wrong-wire", "0.2.2", "agent", 2, "hostmcp-agent", tar.TypeReg, false},
		{"traversal", "0.2.2", "agent", 1, "../hostmcp-agent", tar.TypeReg, false},
		{"absolute", "0.2.2", "agent", 1, "/hostmcp-agent", tar.TypeReg, false},
		{"symlink", "0.2.2", "agent", 1, "hostmcp-agent", tar.TypeSymlink, false},
		{"hardlink", "0.2.2", "agent", 1, "hostmcp-agent", tar.TypeLink, false},
		{"missing-binary", "0.2.2", "agent", 1, "different", tar.TypeReg, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, old := updaterFixture(t)
			archive := testArchive(t, testBinary(tc.version, tc.role, tc.wire), tc.entry, tc.kind)
			pin(&p, archive)
			if tc.corrupt {
				archive = append(archive, 1)
			}
			changed, err := Apply(context.Background(), p, bytes.NewReader(archive))
			if err == nil || changed {
				t.Fatalf("failure changed=%v error=%v", changed, err)
			}
			got, _ := os.ReadFile(p.Binary)
			if !bytes.Equal(old, got) {
				t.Fatal("original modified on failure")
			}
			if _, err := os.Stat(p.Binary + ".previous"); !os.IsNotExist(err) {
				t.Fatal("failure left backup unexpectedly")
			}
		})
	}
}
func TestUpdaterPolicyLockAndBackupGuards(t *testing.T) {
	p, old := updaterFixture(t)
	archive := testArchive(t, testBinary("0.2.2", "agent", 1), "hostmcp-agent", tar.TypeReg)
	pin(&p, archive)
	for _, edit := range []func(*Policy){func(p *Policy) { p.Enabled = false }, func(p *Policy) { p.Channel = "latest" }, func(p *Policy) { p.TargetVersion = "0.2.3-rc.1" }, func(p *Policy) { p.TargetVersion = "../../evil" }, func(p *Policy) { p.SHA256 = "" }, func(p *Policy) { p.Binary = "relative" }} {
		invalid := p
		edit(&invalid)
		if _, err := Apply(context.Background(), invalid, bytes.NewReader(archive)); err == nil {
			t.Fatal("unsafe policy accepted")
		}
	}
	file := p.Binary + ".policy.json"
	if err := configWrite(file, p); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(file); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(file, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(file); err == nil {
		t.Fatal("public policy accepted")
	}
	// A busy update lock blocks concurrent replacers.
	guard, err := lock(p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = Apply(context.Background(), p, bytes.NewReader(archive)); err == nil {
		t.Fatal("concurrent update accepted")
	}
	guard.Close()
	if err = os.WriteFile(p.Binary+".previous", []byte("keep backup"), 0700); err != nil {
		t.Fatal(err)
	}
	if _, err = Apply(context.Background(), p, bytes.NewReader(archive)); err == nil {
		t.Fatal("backup overwritten")
	}
	got, _ := os.ReadFile(p.Binary)
	if !bytes.Equal(got, old) {
		t.Fatal("backup failure replaced binary")
	}
	backup, _ := os.ReadFile(p.Binary + ".previous")
	if string(backup) != "keep backup" {
		t.Fatal("backup altered")
	}
	if err = os.Remove(p.Binary + ".previous"); err != nil {
		t.Fatal(err)
	}
	if err = os.Chmod(p.Binary, 0775); err != nil {
		t.Fatal(err)
	}
	if _, err = Apply(context.Background(), p, bytes.NewReader(archive)); err == nil {
		t.Fatal("group-writable target accepted")
	}
	if err = os.Chmod(p.Binary, 0755); err != nil {
		t.Fatal(err)
	}
	downgrade := p
	downgrade.TargetVersion = "0.2.0"
	if _, err = Apply(context.Background(), downgrade, bytes.NewReader(archive)); err == nil {
		t.Fatal("downgrade accepted")
	}
}
func configWrite(file string, p Policy) error {
	b, err := json.Marshal(p)
	if err != nil {
		return err
	}
	return os.WriteFile(file, b, 0600)
}
func TestUpdaterRejectsSymlinkTargetAndLock(t *testing.T) {
	p, _ := updaterFixture(t)
	p.SHA256 = strings.Repeat("0", 64)
	alias := p.Binary + "-link"
	if err := os.Symlink(p.Binary, alias); err != nil {
		t.Fatal(err)
	}
	original := p.Binary
	p.Binary = alias
	if _, err := lock(p); err == nil {
		t.Fatal("symlink target accepted")
	}
	p.Binary = original
	if err := os.Symlink(original, p.Binary+".update.lock"); err != nil {
		t.Fatal(err)
	}
	if _, err := lock(p); err == nil {
		t.Fatal("symlink lock accepted")
	}
}
func TestUpdaterFixedSource(t *testing.T) {
	p := Policy{TargetVersion: "1.2.3"}
	if got := releaseURL(p); !strings.HasPrefix(got, "https://github.com/soren-labs/hostmcp/releases/download/v1.2.3/hostmcp-agent_1.2.3_") {
		t.Fatal(got)
	}
}

func TestUpdaterAuthenticatedSourceAndRedirects(t *testing.T) {
	p, _ := updaterFixture(t)
	p.SHA256 = strings.Repeat("0", 64)
	previous := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = previous })
	calls := 0
	http.DefaultTransport = testutil.HandlerTransport{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.String() != releaseURL(p) || r.URL.Scheme != "https" {
			t.Error("unexpected download source")
		}
		http.Redirect(w, r, "https://evil.example/agent", http.StatusFound)
	})}
	if body, err := Download(context.Background(), p); err == nil {
		body.Close()
		t.Fatal("arbitrary release redirect allowed")
	}
	if calls != 1 {
		t.Fatal("untrusted endpoint was contacted")
	}
	http.DefaultTransport = testutil.HandlerTransport{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "not published", 404) })}
	if body, err := Download(context.Background(), p); err == nil {
		body.Close()
		t.Fatal("failed release accepted")
	}
	file := p.Binary + ".policy.json"
	if err := os.WriteFile(file, []byte(`{"version":1,"enabled":true,"source":"https://evil.example/agent"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(file); err == nil {
		t.Fatal("custom update source accepted")
	}
}

type replacingReader struct {
	reader  *bytes.Reader
	replace func()
	done    bool
}

func (r *replacingReader) Read(b []byte) (int, error) {
	if !r.done {
		r.done = true
		r.replace()
	}
	return r.reader.Read(b)
}
func TestUpdaterConcurrentManualReplacementIsPreserved(t *testing.T) {
	p, _ := updaterFixture(t)
	archive := testArchive(t, testBinary("0.2.2", "agent", 1), "hostmcp-agent", tar.TypeReg)
	pin(&p, archive)
	manual := testBinary("0.2.3", "agent", 1)
	reader := &replacingReader{reader: bytes.NewReader(archive), replace: func() {
		temp := p.Binary + ".manual"
		if err := os.WriteFile(temp, manual, 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(temp, p.Binary); err != nil {
			t.Fatal(err)
		}
	}}
	if changed, err := Apply(context.Background(), p, reader); changed || err == nil {
		t.Fatal("concurrent manual replacement overwritten")
	}
	actual, _ := os.ReadFile(p.Binary)
	if !bytes.Equal(actual, manual) {
		t.Fatal("manual replacement lost")
	}
}
