package update

import (
	"archive/tar"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestVersionOrder(t *testing.T) {
	order := []string{"0.2.4", "0.3.0-rc.1", "0.3.0-rc.2", "0.3.0-rc.10", "0.3.0", "0.3.1", "1.0.0"}
	for i := range order {
		for j := range order {
			a, _ := ParseVersion(order[i])
			b, err := ParseVersion(order[j])
			if err != nil {
				t.Fatal(err)
			}
			if a.Less(b) != (i < j) {
				t.Fatalf("%s < %s = %v", order[i], order[j], a.Less(b))
			}
		}
	}
	for _, bad := range []string{"0.3.0-dev", "v0.3.0", "0.3", "0.3.0-rc.0", "0.3.0-beta.1", "01.2.3"} {
		if _, err := ParseVersion(bad); err == nil {
			t.Fatalf("accepted %q", bad)
		}
	}
}

func testKey(t *testing.T) (ed25519.PrivateKey, PublicKey) {
	t.Helper()
	pub, priv, err := GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	k, err := ParsePrivateKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	p, err := ParsePublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	return k, p
}

func TestSignatureBindsTagAndSums(t *testing.T) {
	priv, pub := testKey(t)
	_, other := testKey(t)
	sums := []byte("abc  rmc-node_0.3.0_linux_amd64.tar.gz\n")
	sig, err := Sign(priv, "v0.3.0", sums)
	if err != nil {
		t.Fatal(err)
	}
	if id, err := Verify([]PublicKey{other, pub}, "v0.3.0", sums, sig); err != nil || id != pub.ID {
		t.Fatalf("valid signature: %v", err)
	}
	for name, tc := range map[string]struct {
		keys []PublicKey
		tag  string
		sums []byte
		sig  []byte
	}{
		"other tag":     {[]PublicKey{pub}, "v0.3.1", sums, sig},
		"changed sums":  {[]PublicKey{pub}, "v0.3.0", append([]byte("x"), sums...), sig},
		"untrusted key": {[]PublicKey{other}, "v0.3.0", sums, sig},
		"malformed":     {[]PublicKey{pub}, "v0.3.0", sums, []byte("rmc-release-signature v1\nkey: x\n")},
		"bad tag":       {[]PublicKey{pub}, "0.3.0", sums, sig},
	} {
		if _, err := Verify(tc.keys, tc.tag, tc.sums, tc.sig); err == nil {
			t.Fatalf("%s: verified", name)
		}
	}
	if keys, err := TrustedKeys(); err != nil || len(keys) == 0 {
		t.Fatalf("compiled-in keys: %v %v", keys, err)
	}
}

func TestChooseRespectsChannelDraftsAndRollbacks(t *testing.T) {
	rel := func(tag string, draft, pre bool) ghRelease { return ghRelease{Tag: tag, Draft: draft, Prerelease: pre} }
	releases := []ghRelease{rel("v0.4.0", true, false), rel("v0.3.1-rc.1", false, true), rel("v0.3.0", false, false), rel("v0.2.9", false, false), rel("v0.3.2", false, true), rel("nonsense", false, false)}
	cur, _ := ParseVersion("0.2.4")
	for _, tc := range []struct {
		channel string
		skip    []string
		want    string
	}{
		{"stable", nil, "0.3.0"},
		{"canary", nil, "0.3.2"},
		{"stable", []string{"0.3.0"}, "0.2.9"},
	} {
		c, ok := choose(releases, cur, tc.channel, tc.skip)
		if !ok || c.version.String() != tc.want {
			t.Fatalf("%s %v: got %v %v", tc.channel, tc.skip, c.version, ok)
		}
	}
	newest, _ := ParseVersion("0.3.0")
	if _, ok := choose(releases, newest, "stable", nil); ok {
		t.Fatal("offered a version that is not newer")
	}
}

type fakeRelease struct {
	tag, version string
	pre          bool
	files        map[string][]byte
}

// autoFixture serves a GitHub-like release API over TLS and returns an updater
// whose executable is an installed fake node of version current.
func autoFixture(t *testing.T, current string, signer ed25519.PrivateKey, trusted PublicKey, releases ...fakeRelease) (*Updater, *atomic.Int32) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("updater intentionally refuses root")
	}
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(dir, "rmc-node")
	if err = os.WriteFile(binary, testBinary(current, "node", 1), 0700); err != nil {
		t.Fatal(err)
	}
	var srv *httptest.Server
	srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/releases" {
			var list []map[string]any
			for _, rel := range releases {
				var assets []map[string]string
				for name := range rel.files {
					assets = append(assets, map[string]string{"name": name, "browser_download_url": srv.URL + "/download/" + rel.tag + "/" + name})
				}
				list = append(list, map[string]any{"tag_name": rel.tag, "draft": false, "prerelease": rel.pre, "assets": assets})
			}
			_ = json.NewEncoder(w).Encode(list)
			return
		}
		parts := strings.SplitN(strings.TrimPrefix(r.URL.Path, "/download/"), "/", 2)
		for _, rel := range releases {
			if len(parts) == 2 && rel.tag == parts[0] {
				if b, ok := rel.files[parts[1]]; ok {
					_, _ = w.Write(b)
					return
				}
			}
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)
	host, _ := url.Parse(srv.URL)
	restarts := new(atomic.Int32)
	u := &Updater{
		Component: "node", Binary: binary, Current: current,
		SettingsFile: filepath.Join(dir, settingsFileName), StateFile: filepath.Join(dir, stateFileName),
		MaxIdleWait: time.Minute, Restart: func() error { restarts.Add(1); return nil }, Logf: t.Logf,
		api: srv.URL, client: srv.Client(), keys: []PublicKey{trusted},
		hosts: map[string]bool{host.Hostname(): true}, idlePoll: 10 * time.Millisecond,
	}
	return u, restarts
}

// signedRelease builds a release whose node archive contains a fake binary.
func signedRelease(t *testing.T, signer ed25519.PrivateKey, version string, pre bool) fakeRelease {
	t.Helper()
	tag := "v" + version
	name := AssetName("node", version)
	archive := testArchive(t, testBinary(version, "node", 1), "rmc-node", tar.TypeReg)
	d := sha256.Sum256(archive)
	sums := []byte(fmt.Sprintf("%s  %s\n%s  rmc-hub_%s_x_y.tar.gz\n", hex.EncodeToString(d[:]), name, strings.Repeat("0", 64), version))
	files := map[string][]byte{name: archive, "SHA256SUMS": sums}
	if signer != nil {
		sig, err := Sign(signer, tag, sums)
		if err != nil {
			t.Fatal(err)
		}
		files["SHA256SUMS.sig"] = sig
	}
	return fakeRelease{tag: tag, version: version, pre: pre, files: files}
}

func installedVersion(t *testing.T, binary string) string {
	t.Helper()
	id, err := probeAs(context.Background(), binary, "node")
	if err != nil {
		t.Fatal(err)
	}
	return id.Version
}

func TestAutoUpdateInstallsSignedReleaseAndRestarts(t *testing.T) {
	priv, pub := testKey(t)
	u, restarts := autoFixture(t, "0.3.0", priv, pub, signedRelease(t, priv, "0.3.1", false), signedRelease(t, priv, "0.4.0-rc.1", true))
	busy := atomic.Bool{}
	busy.Store(true)
	u.Idle = func() bool { return !busy.Load() }
	// A real restart is syscall.Exec, so deferred cleanup never runs: no
	// staging file may be left beside the binary by the time it restarts.
	leftovers := []string{"not restarted"}
	u.Restart = func() error {
		restarts.Add(1)
		leftovers, _ = filepath.Glob(filepath.Join(filepath.Dir(u.Binary), ".rmc-*"))
		return nil
	}
	go func() { time.Sleep(50 * time.Millisecond); busy.Store(false) }()
	res, err := u.Check(context.Background(), true)
	if err != nil || res.Available != "0.3.1" {
		t.Fatalf("check: %+v %v", res, err)
	}
	if installedVersion(t, u.Binary) != "0.3.1" || installedVersion(t, u.Binary+".previous") != "0.3.0" || restarts.Load() != 1 {
		t.Fatalf("install: binary=%s restarts=%d", installedVersion(t, u.Binary), restarts.Load())
	}
	if len(leftovers) != 0 {
		t.Fatalf("staging files left at restart: %v", leftovers)
	}
	st, _ := LoadState(u.StateFile)
	if st.Pending == nil || st.Pending.From != "0.3.0" || st.Pending.To != "0.3.1" || st.LastResult == "" {
		t.Fatalf("state %+v", st)
	}
}

// When the candidate is already on disk (another process sharing the
// executable installed it, or an earlier restart failed), only the restart is
// missing; it must still wait until the service is idle.
func TestAutoUpdateAlreadyInstalledWaitsForIdle(t *testing.T) {
	priv, pub := testKey(t)
	u, restarts := autoFixture(t, "0.3.0", priv, pub, signedRelease(t, priv, "0.3.1", false))
	if err := os.WriteFile(u.Binary, testBinary("0.3.1", "node", 1), 0700); err != nil {
		t.Fatal(err)
	}
	busy := atomic.Bool{}
	busy.Store(true)
	u.Idle = func() bool { return !busy.Load() }
	var busyAtRestart atomic.Bool
	u.Restart = func() error { busyAtRestart.Store(busy.Load()); restarts.Add(1); return nil }
	go func() { time.Sleep(80 * time.Millisecond); busy.Store(false) }()
	if _, err := u.Check(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	if restarts.Load() != 1 || busyAtRestart.Load() {
		t.Fatalf("restarts=%d busy at restart=%v", restarts.Load(), busyAtRestart.Load())
	}

	// A service that stops while waiting does not restart.
	u2, restarts2 := autoFixture(t, "0.3.0", priv, pub, signedRelease(t, priv, "0.3.1", false))
	if err := os.WriteFile(u2.Binary, testBinary("0.3.1", "node", 1), 0700); err != nil {
		t.Fatal(err)
	}
	u2.Idle = func() bool { return false }
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()
	if _, err := u2.Check(ctx, true); err == nil || restarts2.Load() != 0 {
		t.Fatalf("canceled wait: err=%v restarts=%d", err, restarts2.Load())
	}
}

func TestAutoUpdateRefusesUnsignedTamperedAndUntrusted(t *testing.T) {
	priv, pub := testKey(t)
	otherPriv, _ := testKey(t)
	tampered := signedRelease(t, priv, "0.3.1", false)
	name := AssetName("node", "0.3.1")
	tampered.files[name] = testArchive(t, testBinary("0.3.1", "node", 1), "rmc-node-evil", tar.TypeReg)
	for label, rel := range map[string]fakeRelease{
		"unsigned":  signedRelease(t, nil, "0.3.1", false),
		"untrusted": signedRelease(t, otherPriv, "0.3.1", false),
		"tampered":  tampered,
	} {
		u, restarts := autoFixture(t, "0.3.0", priv, pub, rel)
		if _, err := u.Check(context.Background(), true); err == nil {
			t.Fatalf("%s: installed", label)
		}
		if installedVersion(t, u.Binary) != "0.3.0" || restarts.Load() != 0 {
			t.Fatalf("%s: binary changed", label)
		}
	}
}

func TestAutoUpdateSettings(t *testing.T) {
	priv, pub := testKey(t)
	u, restarts := autoFixture(t, "0.3.0", priv, pub, signedRelease(t, priv, "0.3.1-rc.1", true))
	if res, err := u.Check(context.Background(), true); err != nil || res.Available != "" {
		t.Fatalf("stable channel took a release candidate: %+v %v", res, err)
	}
	if err := SaveSettings(u.SettingsFile, Settings{Channel: "canary"}); err != nil {
		t.Fatal(err)
	}
	off := false
	if err := SaveSettings(u.SettingsFile, Settings{Auto: &off, Channel: "canary"}); err != nil {
		t.Fatal(err)
	}
	if res, _ := u.Check(context.Background(), true); res.Message != "automatic updates are off" || restarts.Load() != 0 {
		t.Fatalf("disabled: %+v", res)
	}
	if res, err := u.Check(context.Background(), false); err != nil || res.Available != "0.3.1-rc.1" {
		t.Fatalf("manual check while disabled: %+v %v", res, err)
	}
	if err := SaveSettings(u.SettingsFile, Settings{Channel: "canary"}); err != nil {
		t.Fatal(err)
	}
	if _, err := u.Check(context.Background(), true); err != nil || installedVersion(t, u.Binary) != "0.3.1-rc.1" {
		t.Fatalf("canary: %v", err)
	}
	dev, _ := autoFixture(t, "0.3.0-dev", priv, pub, signedRelease(t, priv, "0.3.1", false))
	if res, _ := dev.Check(context.Background(), true); !strings.Contains(res.Message, "development build") {
		t.Fatalf("dev build: %+v", res)
	}
}

func TestStartupRollsBackAfterRepeatedFailedStarts(t *testing.T) {
	priv, pub := testKey(t)
	u, restarts := autoFixture(t, "0.3.0", priv, pub, signedRelease(t, priv, "0.3.1", false))
	if _, err := u.Check(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	// The re-executed process is now 0.3.1 and never confirms health.
	for i := 1; i <= maxStartAttempts; i++ {
		restarted(u, "0.3.1").Startup()
		if st, _ := LoadState(u.StateFile); st.Pending == nil || st.Pending.Attempts != i {
			t.Fatalf("attempt %d: %+v", i, st.Pending)
		}
	}
	restarted(u, "0.3.1").Startup()
	st, _ := LoadState(u.StateFile)
	if installedVersion(t, u.Binary) != "0.3.0" || !slices.Contains(st.RolledBack, "0.3.1") || st.Pending != nil || restarts.Load() != 2 {
		t.Fatalf("rollback: binary=%s state=%+v restarts=%d", installedVersion(t, u.Binary), st, restarts.Load())
	}
	// A rolled-back version is never offered again.
	if res, err := u.Check(context.Background(), false); err != nil || res.Available != "" {
		t.Fatalf("rolled-back version offered again: %+v %v", res, err)
	}
}

func TestConfirmAndUnconfirmedRollback(t *testing.T) {
	priv, pub := testKey(t)
	u, _ := autoFixture(t, "0.3.0", priv, pub, signedRelease(t, priv, "0.3.1", false))
	if _, err := u.Check(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	restarted(u, "0.3.1").Startup()()
	st, _ := LoadState(u.StateFile)
	if st.Pending != nil || st.Last == nil || st.Last.To != "0.3.1" {
		t.Fatalf("confirm: %+v", st)
	}

	u2, restarts := autoFixture(t, "0.3.0", priv, pub, signedRelease(t, priv, "0.3.1", false))
	if _, err := u2.Check(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	stuck := restarted(u2, "0.3.1")
	stuck.Startup()
	// The Hub is unreachable as well: keep the new version.
	stuck.RollbackUnlessConfirmed(context.Background(), 10*time.Millisecond, func(context.Context) bool { return false })
	if installedVersion(t, u2.Binary) != "0.3.1" {
		t.Fatal("rolled back although the Hub was unreachable")
	}
	// The Hub answers but this version never connects: roll back.
	stuck.RollbackUnlessConfirmed(context.Background(), 10*time.Millisecond, func(context.Context) bool { return true })
	if installedVersion(t, u2.Binary) != "0.3.0" || restarts.Load() != 2 {
		t.Fatalf("unconfirmed rollback: %s restarts=%d", installedVersion(t, u2.Binary), restarts.Load())
	}
}

func TestSourceAllowlist(t *testing.T) {
	u := &Updater{hosts: map[string]bool{"github.com": true}}
	for raw, want := range map[string]bool{
		"https://github.com/x": true, "http://github.com/x": false, "https://evil.example/x": false, "https://user@github.com/x": false,
	} {
		x, _ := url.Parse(raw)
		if u.allowed(x) != want {
			t.Fatalf("%s allowed=%v", raw, !want)
		}
	}
	t.Setenv("RMC_UPDATE_API", "http://evil.example")
	if _, err := New("node", "/x", "0.3.0", "/tmp/node.json"); err == nil {
		t.Fatal("plain-HTTP non-loopback source accepted")
	}
}

// restarted is the updater of the process that replaced u after an install.
func restarted(u *Updater, current string) *Updater {
	return &Updater{
		Component: u.Component, Binary: u.Binary, Current: current,
		SettingsFile: u.SettingsFile, StateFile: u.StateFile,
		Idle: u.Idle, MaxIdleWait: u.MaxIdleWait, Restart: u.Restart, Logf: u.Logf,
		api: u.api, client: u.client, keys: u.keys, hosts: u.hosts, plainHTTP: u.plainHTTP, idlePoll: u.idlePoll,
	}
}
