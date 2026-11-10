package update

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/soren-labs/rmc/internal/config"
)

// Automatic updates: a running Hub or node checks GitHub releases about once an
// hour, installs the newest release of its channel that is signed by a trusted
// release key, waits until it is idle, replaces its own executable and
// re-executes it. A version that does not become healthy is rolled back to the
// previous executable and never retried.

const (
	defaultReleaseAPI = "https://api.github.com/repos/soren-labs/rmc"
	settingsFileName  = "updates.json"
	stateFileName     = "update-state.json"
	maxSums           = 64 << 10
	maxReleaseList    = 4 << 20
	maxStartAttempts  = 3
	maxRolledBack     = 20
)

// Settings is the optional updates.json beside a node or Hub configuration. A
// missing file means automatic updates from the stable channel.
type Settings struct {
	Version int    `json:"version"`
	Auto    *bool  `json:"auto,omitempty"`
	Channel string `json:"channel,omitempty"`
}

func (s Settings) Enabled() bool { return s.Auto == nil || *s.Auto }

// ChannelName is "stable" (releases) or "canary" (release candidates too).
func (s Settings) ChannelName() string {
	if s.Channel == "" {
		return "stable"
	}
	return s.Channel
}

// Describe summarizes the settings for list_devices and status output.
func (s Settings) Describe() string {
	if !s.Enabled() {
		return "off"
	}
	return "on (" + s.ChannelName() + ")"
}

func (s Settings) validate() error {
	if s.Version != 1 {
		return errors.New("updates.json version must be 1")
	}
	if s.Channel != "" && s.Channel != "stable" && s.Channel != "canary" {
		return errors.New("update channel must be stable or canary")
	}
	return nil
}

// Paths returns the settings and state files beside a configuration file. The
// settings are shared by the services configured in one directory; the state
// belongs to one service, for example node.update-state.json.
func Paths(configFile string) (settings, state string) {
	dir := filepath.Dir(configFile)
	stem := strings.TrimSuffix(filepath.Base(configFile), filepath.Ext(configFile))
	return filepath.Join(dir, settingsFileName), filepath.Join(dir, stem+"."+stateFileName)
}

// disabledByEnvironment reports RMC_AUTO_UPDATE=off, which packaged
// installations such as container images use: they are updated by replacing
// the image, never in place.
func disabledByEnvironment() bool {
	v := strings.ToLower(os.Getenv("RMC_AUTO_UPDATE"))
	return v == "off" || v == "0" || v == "false"
}

// LoadSettings reads updates.json; a missing file yields the defaults.
func LoadSettings(file string) (Settings, error) {
	if _, err := os.Lstat(file); errors.Is(err, os.ErrNotExist) {
		return Settings{Version: 1}, nil
	}
	var s Settings
	if err := config.ReadJSON(file, &s); err != nil {
		return s, err
	}
	return s, s.validate()
}

// SaveSettings writes updates.json privately.
func SaveSettings(file string, s Settings) error {
	s.Version = 1
	if err := s.validate(); err != nil {
		return err
	}
	return config.WriteJSON(file, s, true)
}

// Transition is one installed update.
type Transition struct {
	From     string    `json:"from"`
	To       string    `json:"to"`
	At       time.Time `json:"at"`
	Attempts int       `json:"attempts,omitempty"`
}

// State is update-state.json, written by the running service.
type State struct {
	Version    int         `json:"version"`
	Pending    *Transition `json:"pending,omitempty"`
	Last       *Transition `json:"last_update,omitempty"`
	RolledBack []string    `json:"rolled_back,omitempty"`
	LastCheck  time.Time   `json:"last_check,omitzero"`
	LastResult string      `json:"last_result,omitempty"`
}

// LoadState reads update-state.json; a missing file yields an empty state.
func LoadState(file string) (State, error) {
	if _, err := os.Lstat(file); errors.Is(err, os.ErrNotExist) {
		return State{Version: 1}, nil
	}
	var st State
	err := config.ReadJSON(file, &st)
	return st, err
}

func saveState(file string, st State) error {
	st.Version = 1
	return config.WriteJSON(file, st, true)
}

// Result describes one update check.
type Result struct {
	Available string `json:"available,omitempty"`
	Message   string `json:"message"`
}

// Updater keeps one running Hub or node executable up to date.
type Updater struct {
	Component    string // "all", "hub" or "node": the running build
	Binary       string // the running executable, symlinks resolved
	Current      string // the running build version
	SettingsFile string
	StateFile    string
	// Idle reports whether replacing the process now interrupts nothing.
	Idle        func() bool
	MaxIdleWait time.Duration
	// Restart replaces the process with Binary; it returns only on failure.
	Restart func() error
	Logf    func(format string, args ...any)

	api        string
	client     *http.Client
	keys       []PublicKey
	hosts      map[string]bool
	plainHTTP  bool // only for a loopback RMC_UPDATE_API test source
	firstDelay time.Duration
	interval   time.Duration
	idlePoll   time.Duration
	mu         sync.Mutex
	confirmed  atomic.Bool
	lastResult string
}

// New creates the updater for a running service. RMC_UPDATE_API may point at a
// mirror of the GitHub releases API (signatures are still required), and
// RMC_UPDATE_INTERVAL shortens the hourly check, mainly for tests.
func New(component, binary, current, configFile string) (*Updater, error) {
	keys, err := TrustedKeys()
	if err != nil {
		return nil, err
	}
	settings, state := Paths(configFile)
	u := &Updater{
		Component: component, Binary: binary, Current: current, SettingsFile: settings, StateFile: state,
		MaxIdleWait: 24 * time.Hour, Logf: func(string, ...any) {},
		api: defaultReleaseAPI, keys: keys,
		hosts:      map[string]bool{"api.github.com": true, "github.com": true, "objects.githubusercontent.com": true, "release-assets.githubusercontent.com": true},
		firstDelay: time.Duration(2+rand.IntN(8)) * time.Minute, interval: time.Hour, idlePoll: 30 * time.Second,
	}
	if source := os.Getenv("RMC_UPDATE_API"); source != "" {
		x, err := url.Parse(source)
		if err != nil || x.User != nil || x.Host == "" || (x.Scheme != "https" && !(x.Scheme == "http" && isLoopback(x.Hostname()))) {
			return nil, errors.New("RMC_UPDATE_API must be an https URL (or http on loopback)")
		}
		u.api = strings.TrimSuffix(source, "/")
		u.hosts[x.Hostname()] = true
		u.plainHTTP = x.Scheme == "http"
	}
	if raw := os.Getenv("RMC_UPDATE_INTERVAL"); raw != "" {
		d, err := time.ParseDuration(raw)
		if err != nil || d < 10*time.Second {
			return nil, errors.New("RMC_UPDATE_INTERVAL must be a duration of at least 10s")
		}
		u.interval, u.firstDelay = d, 5*time.Second
	}
	u.client = &http.Client{Timeout: 5 * time.Minute, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) > 5 || !u.allowed(req.URL) {
			return errors.New("release redirect outside trusted sources")
		}
		return nil
	}}
	return u, nil
}

func isLoopback(host string) bool { return host == "127.0.0.1" || host == "localhost" || host == "::1" }

func (u *Updater) allowed(x *url.URL) bool {
	if x.User != nil || !u.hosts[x.Hostname()] {
		return false
	}
	switch x.Scheme {
	case "https":
		return true
	case "http":
		return u.plainHTTP && isLoopback(x.Hostname())
	}
	return false
}

func (u *Updater) logf(format string, args ...any) { u.Logf(format, args...) }

// Unsupported explains why this process cannot replace its own executable,
// or returns "" when it can.
func (u *Updater) Unsupported() string {
	if _, err := ParseVersion(u.Current); err != nil {
		return "development build " + u.Current
	}
	if os.Geteuid() == 0 {
		return "runs as root; root installations are updated manually"
	}
	if err := safeParents(u.Binary); err != nil {
		return u.Binary + ": " + err.Error()
	}
	if _, err := safeFile(u.Binary); err != nil {
		return u.Binary + ": " + err.Error()
	}
	if err := syscall.Access(filepath.Dir(u.Binary), 2 /* W_OK */); err != nil {
		return filepath.Dir(u.Binary) + " is not writable by this account"
	}
	return ""
}

// Mode is how this installation is updated: "stable" or "canary" while
// automatic updates run, "off" when they were turned off, and "manual" when
// this build or installation cannot replace itself.
func (u *Updater) Mode() string {
	if u.Unsupported() != "" {
		return "manual"
	}
	s, err := LoadSettings(u.SettingsFile)
	if err != nil || !s.Enabled() || disabledByEnvironment() {
		return "off"
	}
	return s.ChannelName()
}

// Describe summarizes the current settings, for example "on (stable)".
func (u *Updater) Describe() string {
	s, err := LoadSettings(u.SettingsFile)
	if err != nil {
		return "error: " + err.Error()
	}
	return s.Describe()
}

func (u *Updater) get(ctx context.Context, raw string, limit int64, w io.Writer, api bool) error {
	x, err := url.Parse(raw)
	if err != nil || !u.allowed(x) {
		return fmt.Errorf("refusing download from %q: not a trusted release source", raw)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, raw, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "rmc-updater/"+u.Current)
	if api {
		req.Header.Set("Accept", "application/vnd.github+json")
	}
	resp, err := u.client.Do(req)
	if err != nil {
		return fmt.Errorf("download %s: %w", x.Path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download %s: HTTP %d", x.Path, resp.StatusCode)
	}
	if resp.ContentLength > limit {
		return fmt.Errorf("download %s exceeds %d bytes", x.Path, limit)
	}
	n, err := io.Copy(w, io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return err
	}
	if n > limit {
		return fmt.Errorf("download %s exceeds %d bytes", x.Path, limit)
	}
	return nil
}

type ghAsset struct {
	Name string `json:"name"`
	URL  string `json:"browser_download_url"`
}

type ghRelease struct {
	Tag        string    `json:"tag_name"`
	Draft      bool      `json:"draft"`
	Prerelease bool      `json:"prerelease"`
	Assets     []ghAsset `json:"assets"`
}

type candidate struct {
	version Version
	tag     string
	assets  map[string]ghAsset
}

// choose returns the newest published release above current for the channel,
// skipping drafts, malformed tags and versions that were rolled back here.
func choose(releases []ghRelease, current Version, channel string, skip []string) (candidate, bool) {
	var best candidate
	found := false
	for _, r := range releases {
		if r.Draft || !strings.HasPrefix(r.Tag, "v") {
			continue
		}
		v, err := ParseVersion(strings.TrimPrefix(r.Tag, "v"))
		if err != nil || (channel != "canary" && (!v.Stable() || r.Prerelease)) {
			continue
		}
		if !current.Less(v) || slices.Contains(skip, v.String()) || (found && !best.version.Less(v)) {
			continue
		}
		assets := map[string]ghAsset{}
		for _, a := range r.Assets {
			assets[a.Name] = a
		}
		best, found = candidate{version: v, tag: r.Tag, assets: assets}, true
	}
	return best, found
}

func executableName(component string) string {
	switch component {
	case "hub":
		return "rmc-hub"
	case "node":
		return "rmc-node"
	}
	return "rmc"
}

// AssetName is the release archive for a component on this platform.
func AssetName(component, version string) string {
	return fmt.Sprintf("%s_%s_%s_%s.tar.gz", executableName(component), version, runtime.GOOS, runtime.GOARCH)
}

func sumFor(sums []byte, name string) (string, error) {
	for _, line := range strings.Split(string(sums), "\n") {
		f := strings.Fields(line)
		if len(f) != 2 || strings.TrimPrefix(f[1], "*") != name {
			continue
		}
		if b, err := hex.DecodeString(f[0]); err != nil || len(b) != sha256.Size {
			return "", fmt.Errorf("SHA256SUMS entry for %s is malformed", name)
		}
		return strings.ToLower(f[0]), nil
	}
	return "", fmt.Errorf("SHA256SUMS has no entry for %s", name)
}

// Check looks for a newer signed release. With install, it also downloads,
// verifies and installs it, then restarts; on success it does not return.
func (u *Updater) Check(ctx context.Context, install bool) (Result, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	res, err := u.check(ctx, install)
	u.record(res, err)
	return res, err
}

func (u *Updater) record(res Result, err error) {
	text := res.Message
	if err != nil {
		text = "error: " + err.Error()
	}
	if text != u.lastResult {
		if err != nil {
			u.logf("event=auto_update outcome=error error=%q", err.Error())
		} else {
			u.logf("event=auto_update outcome=checked result=%q", text)
		}
		u.lastResult = text
	}
	st, loadErr := LoadState(u.StateFile)
	if loadErr != nil {
		st = State{}
	}
	st.LastCheck, st.LastResult = time.Now().UTC(), text
	_ = saveState(u.StateFile, st)
}

func (u *Updater) check(ctx context.Context, install bool) (Result, error) {
	settings, err := LoadSettings(u.SettingsFile)
	if err != nil {
		return Result{}, err
	}
	if install && !settings.Enabled() {
		return Result{Message: "automatic updates are off"}, nil
	}
	if install && disabledByEnvironment() {
		return Result{Message: "automatic updates are off (RMC_AUTO_UPDATE)"}, nil
	}
	current, err := ParseVersion(u.Current)
	if err != nil {
		return Result{Message: "development build " + u.Current + "; automatic updates apply only to release builds"}, nil
	}
	if install && os.Geteuid() == 0 {
		return Result{Message: "running as root; root installations are updated manually"}, nil
	}
	st, _ := LoadState(u.StateFile)
	var list bytes.Buffer
	if err = u.get(ctx, u.api+"/releases?per_page=30", maxReleaseList, &list, true); err != nil {
		return Result{}, err
	}
	var releases []ghRelease
	if err = json.Unmarshal(list.Bytes(), &releases); err != nil {
		return Result{}, errors.New("release list is not valid JSON")
	}
	c, ok := choose(releases, current, settings.ChannelName(), st.RolledBack)
	if !ok {
		return Result{Message: fmt.Sprintf("up to date: %s (%s channel)", u.Current, settings.ChannelName())}, nil
	}
	sumsAsset, okSums := c.assets["SHA256SUMS"]
	sigAsset, okSig := c.assets["SHA256SUMS.sig"]
	if !okSums || !okSig {
		return Result{}, fmt.Errorf("release %s has no SHA256SUMS.sig; unsigned releases are never installed", c.tag)
	}
	var sums, sig bytes.Buffer
	if err = u.get(ctx, sumsAsset.URL, maxSums, &sums, false); err != nil {
		return Result{}, err
	}
	if err = u.get(ctx, sigAsset.URL, maxSignature, &sig, false); err != nil {
		return Result{}, err
	}
	keyID, err := Verify(u.keys, c.tag, sums.Bytes(), sig.Bytes())
	if err != nil {
		return Result{}, err
	}
	name := AssetName(u.Component, c.version.String())
	digest, err := sumFor(sums.Bytes(), name)
	if err != nil {
		return Result{}, err
	}
	asset, ok := c.assets[name]
	if !ok {
		return Result{}, fmt.Errorf("release %s has no %s", c.tag, name)
	}
	res := Result{Available: c.version.String(), Message: fmt.Sprintf("%s is available (signed by %s)", c.tag, keyID)}
	if !install {
		return res, nil
	}
	return res, u.install(ctx, c, asset, digest)
}

func (u *Updater) install(ctx context.Context, c candidate, asset ghAsset, digest string) error {
	guard, err := lockBinary(u.Binary)
	if err != nil {
		return fmt.Errorf("cannot replace %s: %w", u.Binary, err)
	}
	defer guard.Close()
	original, err := safeFile(u.Binary)
	if err != nil {
		return fmt.Errorf("cannot replace %s: %w", u.Binary, err)
	}
	// Never overwrite an executable someone replaced since this process
	// started. If it already is the candidate, only the restart is missing.
	onDisk, err := probeAs(ctx, u.Binary, u.Component)
	if err != nil {
		return fmt.Errorf("inspect installed %s: %w", u.Binary, err)
	}
	switch onDisk.Version {
	case u.Current:
	case c.version.String():
		u.logf("event=auto_update outcome=already_installed version=%q action=restart", onDisk.Version)
		guard.Close()
		return u.Restart()
	default:
		return fmt.Errorf("%s on disk is version %s but %s is running; restart the service first", u.Binary, onDisk.Version, u.Current)
	}
	dir := filepath.Dir(u.Binary)
	compressed, err := os.CreateTemp(dir, ".rmc-archive-*")
	if err != nil {
		return fmt.Errorf("cannot stage beside %s (the directory must be writable by this account): %w", u.Binary, err)
	}
	defer os.Remove(compressed.Name())
	defer compressed.Close()
	h := sha256.New()
	if err = u.get(ctx, asset.URL, maxArchive, io.MultiWriter(compressed, h), false); err != nil {
		return err
	}
	if hex.EncodeToString(h.Sum(nil)) != digest {
		return fmt.Errorf("%s does not match its signed SHA-256; not installed", asset.Name)
	}
	staged, err := os.CreateTemp(dir, ".rmc-update-*")
	if err != nil {
		return err
	}
	defer os.Remove(staged.Name())
	defer staged.Close()
	if err = extractNamed(compressed, staged, executableName(u.Component)); err != nil {
		return err
	}
	// A successful restart replaces this process, so the deferred cleanup
	// never runs: remove the downloaded archive before going further.
	_ = compressed.Close()
	if err = os.Remove(compressed.Name()); err != nil {
		return err
	}
	if err = staged.Chmod(original.Mode().Perm()); err != nil {
		return err
	}
	if err = staged.Sync(); err != nil {
		return err
	}
	if err = staged.Close(); err != nil {
		return err
	}
	id, err := probeAs(ctx, staged.Name(), u.Component)
	if err != nil {
		return err
	}
	if id.Version != c.version.String() {
		return fmt.Errorf("%s reports version %s, not %s", asset.Name, id.Version, c.version)
	}
	u.waitIdle(ctx)
	if err = ctx.Err(); err != nil {
		return err
	}
	latest, err := safeFile(u.Binary)
	if err != nil {
		return err
	}
	if !os.SameFile(original, latest) || original.Size() != latest.Size() || !original.ModTime().Equal(latest.ModTime()) {
		return errors.New("installed executable changed while the update was staged; not replaced")
	}
	backup := u.Binary + ".previous"
	if err = os.Remove(backup); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err = os.Link(u.Binary, backup); err != nil {
		return fmt.Errorf("keep the previous executable for rollback: %w", err)
	}
	if err = syncDir(u.Binary); err != nil {
		return err
	}
	if err = os.Rename(staged.Name(), u.Binary); err != nil {
		return err
	}
	if err = syncDir(u.Binary); err != nil {
		return err
	}
	st, _ := LoadState(u.StateFile)
	st.Pending = &Transition{From: u.Current, To: c.version.String(), At: time.Now().UTC()}
	if err = saveState(u.StateFile, st); err != nil {
		return err
	}
	u.logf("event=auto_update outcome=installed from=%q to=%q action=restart", u.Current, c.version.String())
	guard.Close()
	if err = u.Restart(); err != nil {
		return fmt.Errorf("installed %s but could not restart; it starts with the next service restart: %w", c.version, err)
	}
	return nil
}

// waitIdle waits until Idle reports true, up to MaxIdleWait.
func (u *Updater) waitIdle(ctx context.Context) {
	if u.Idle == nil || u.Idle() {
		return
	}
	u.logf("event=auto_update action=wait_idle max_wait=%q", u.MaxIdleWait.String())
	deadline := time.Now().Add(u.MaxIdleWait)
	t := time.NewTicker(u.idlePoll)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if u.Idle() {
				return
			}
			if time.Now().After(deadline) {
				u.logf("event=auto_update action=restart_busy waited=%q", u.MaxIdleWait.String())
				return
			}
		}
	}
}

// Run checks periodically until ctx ends. Errors are logged and retried at
// the next interval.
func (u *Updater) Run(ctx context.Context) {
	delay := u.firstDelay
	for {
		t := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
		_, _ = u.Check(ctx, true)
		jitter := time.Duration(0)
		if span := int64(u.interval / 6); span > 0 {
			jitter = time.Duration(rand.Int64N(2*span) - span)
		}
		delay = u.interval + jitter
	}
}

// Startup runs before a freshly started service does any work. When the
// running version was just installed, it counts the start; after repeated
// starts that never confirmed health it rolls back to the previous executable.
// The returned confirm records the update as healthy.
func (u *Updater) Startup() (confirm func()) {
	confirm = func() {
		if !u.confirmed.CompareAndSwap(false, true) {
			return
		}
		u.mu.Lock()
		defer u.mu.Unlock()
		st, err := LoadState(u.StateFile)
		if err != nil || st.Pending == nil || st.Pending.To != u.Current {
			return
		}
		done := *st.Pending
		done.Attempts = 0
		st.Last, st.Pending = &done, nil
		if saveState(u.StateFile, st) == nil {
			u.logf("event=auto_update outcome=healthy from=%q to=%q", done.From, done.To)
		}
	}
	st, err := LoadState(u.StateFile)
	if err != nil {
		u.logf("event=auto_update outcome=error error=%q", "unreadable update state: "+err.Error())
		return confirm
	}
	if st.Pending == nil {
		return confirm
	}
	if st.Pending.To != u.Current {
		st.Pending = nil
		_ = saveState(u.StateFile, st)
		return confirm
	}
	st.Pending.Attempts++
	if st.Pending.Attempts > maxStartAttempts {
		u.rollback(&st, fmt.Sprintf("not healthy after %d starts", maxStartAttempts))
		return confirm
	}
	_ = saveState(u.StateFile, st)
	return confirm
}

// RollbackUnlessConfirmed rolls back a just-installed version that has not
// confirmed health after wait, but only while healthy reports that the
// problem is not elsewhere (for a node: the Hub itself answers /healthz).
func (u *Updater) RollbackUnlessConfirmed(ctx context.Context, wait time.Duration, healthy func(context.Context) bool) {
	if st, err := LoadState(u.StateFile); err != nil || st.Pending == nil || st.Pending.To != u.Current {
		return
	}
	t := time.NewTimer(wait)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return
	case <-t.C:
	}
	if u.confirmed.Load() {
		return
	}
	if healthy != nil && !healthy(ctx) {
		u.logf("event=auto_update action=keep reason=%q", "not confirmed, but the Hub is unreachable too")
		return
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	st, err := LoadState(u.StateFile)
	if err != nil || st.Pending == nil || st.Pending.To != u.Current {
		return
	}
	u.rollback(&st, "no healthy connection within "+wait.String())
}

// rollback restores Binary.previous, records the version as rolled back and
// restarts into the restored executable.
func (u *Updater) rollback(st *State, reason string) {
	backup := u.Binary + ".previous"
	st.Pending = nil
	if _, err := safeFile(backup); err != nil {
		st.LastResult = "cannot roll back " + u.Current + ": " + err.Error()
		_ = saveState(u.StateFile, *st)
		u.logf("event=auto_update outcome=error error=%q", st.LastResult)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	id, err := probeAs(ctx, backup, u.Component)
	cancel()
	if err != nil && !(u.Component == "node" && id.Component == "agent" && id.OS == runtime.GOOS && id.Arch == runtime.GOARCH) {
		st.LastResult = "cannot roll back " + u.Current + ": previous executable is unusable"
		_ = saveState(u.StateFile, *st)
		u.logf("event=auto_update outcome=error error=%q", st.LastResult)
		return
	}
	if err = os.Rename(backup, u.Binary); err == nil {
		err = syncDir(u.Binary)
	}
	if err != nil {
		st.LastResult = "cannot roll back " + u.Current + ": " + err.Error()
		_ = saveState(u.StateFile, *st)
		return
	}
	if !slices.Contains(st.RolledBack, u.Current) {
		st.RolledBack = append(st.RolledBack, u.Current)
		if len(st.RolledBack) > maxRolledBack {
			st.RolledBack = st.RolledBack[len(st.RolledBack)-maxRolledBack:]
		}
	}
	st.LastResult = "rolled back " + u.Current + " to " + id.Version + ": " + reason
	_ = saveState(u.StateFile, *st)
	u.logf("event=auto_update outcome=rolled_back version=%q restored=%q reason=%q", u.Current, id.Version, reason)
	if err = u.Restart(); err != nil {
		u.logf("event=auto_update outcome=error error=%q", "restart after rollback failed: "+err.Error())
	}
}
