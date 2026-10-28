// Package update performs operator-pinned, agent-only binary updates. There is
// deliberately no Hub update message or URL supplied by an MCP client.
package update

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/soren-labs/hostmcp/internal/config"
	"github.com/soren-labs/hostmcp/internal/protocol"
)

const releaseSource = "https://github.com/soren-labs/hostmcp/releases/download/"
const maxArchive = 128 << 20
const maxBinary = 64 << 20

type Policy struct {
	Version       int    `json:"version"`
	Enabled       bool   `json:"enabled"`
	Channel       string `json:"channel"`
	TargetVersion string `json:"target_version"`
	SHA256        string `json:"sha256"`
	Binary        string `json:"binary"`
}
type Identity struct {
	Version     string `json:"version"`
	Component   string `json:"component"`
	WireVersion int    `json:"wire_version"`
	OS          string `json:"os"`
	Arch        string `json:"arch"`
}

var stableVersion = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)

func versionParts(s string) ([3]uint64, error) {
	var v [3]uint64
	if !stableVersion.MatchString(s) {
		return v, errors.New("updates require an exact stable version (without v prefix)")
	}
	for i, p := range strings.Split(s, ".") {
		n, err := strconv.ParseUint(p, 10, 64)
		if err != nil {
			return v, err
		}
		v[i] = n
	}
	return v, nil
}
func (p Policy) Validate() error {
	if p.Version != 1 || !p.Enabled || p.Channel != "stable" {
		return errors.New("updates require explicit enabled policy version 1, channel stable")
	}
	if _, err := versionParts(p.TargetVersion); err != nil {
		return err
	}
	b, err := hex.DecodeString(p.SHA256)
	if err != nil || len(b) != 32 {
		return errors.New("policy requires a trusted archive SHA-256 pin")
	}
	if !filepath.IsAbs(p.Binary) || filepath.Clean(p.Binary) != p.Binary {
		return errors.New("update binary must be a clean absolute path")
	}
	return nil
}
func Load(file string) (Policy, error) {
	var p Policy
	if _, err := safeFile(file); err != nil {
		return p, err
	}
	err := config.ReadJSON(file, &p)
	if err == nil {
		err = p.Validate()
	}
	return p, err
}
func Asset(version string) string {
	return fmt.Sprintf("hostmcp-agent_%s_%s_%s.tar.gz", version, runtime.GOOS, runtime.GOARCH)
}
func releaseURL(p Policy) string {
	return releaseSource + "v" + p.TargetVersion + "/" + Asset(p.TargetVersion)
}

// Download authenticates the fixed release origin using system HTTPS trust.
// The independently provisioned policy checksum pins the actual artifact.
func Download(ctx context.Context, p Policy) (io.ReadCloser, error) {
	if err := p.Validate(); err != nil {
		return nil, err
	}
	client := &http.Client{Timeout: 2 * time.Minute, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		host := req.URL.Hostname()
		if len(via) > 5 || req.URL.Scheme != "https" || req.URL.User != nil || req.URL.Port() != "" || (host != "github.com" && host != "release-assets.githubusercontent.com" && host != "objects.githubusercontent.com") {
			return errors.New("release redirect outside trusted source")
		}
		return nil
	}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, releaseURL(p), nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, errors.New("release download failed")
	}
	if resp.StatusCode != 200 {
		resp.Body.Close()
		return nil, fmt.Errorf("release unavailable (HTTP %d)", resp.StatusCode)
	}
	if resp.ContentLength > maxArchive {
		resp.Body.Close()
		return nil, errors.New("release archive exceeds limit")
	}
	return resp.Body, nil
}
func safeFile(file string) (os.FileInfo, error) {
	st, err := os.Lstat(file)
	if err != nil {
		return nil, err
	}
	owner, ok := st.Sys().(*syscall.Stat_t)
	if !ok || !st.Mode().IsRegular() || owner.Uid != uint32(os.Geteuid()) || st.Mode().Perm()&0022 != 0 || st.Mode()&(os.ModeSetuid|os.ModeSetgid) != 0 {
		return nil, errors.New("update files must be regular, owned by the calling user, without group/other write or set-ID bits")
	}
	return st, nil
}
func safeParents(file string) error {
	for dir := filepath.Dir(file); ; dir = filepath.Dir(dir) {
		st, err := os.Lstat(dir)
		if err != nil {
			return err
		}
		owner, ok := st.Sys().(*syscall.Stat_t)
		if !ok || !st.IsDir() || (owner.Uid != 0 && owner.Uid != uint32(os.Geteuid())) || (st.Mode().Perm()&0022 != 0 && !(owner.Uid == 0 && st.Mode()&os.ModeSticky != 0)) {
			return errors.New("update directory chain must be owned by the caller/root and not writable by others (root sticky temporary directories are allowed)")
		}
		if dir == filepath.Dir(dir) {
			return nil
		}
	}
}
func lock(p Policy) (*os.File, error) {
	if err := p.Validate(); err != nil {
		return nil, err
	}
	if os.Geteuid() == 0 {
		return nil, errors.New("root agent updates are manual; updater refuses to run as root")
	}
	if err := safeParents(p.Binary); err != nil {
		return nil, err
	}
	if _, err := safeFile(p.Binary); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(p.Binary+".update.lock", os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, err
	}
	st, err := f.Stat()
	if err == nil && (st.Sys().(*syscall.Stat_t).Uid != uint32(os.Geteuid()) || !st.Mode().IsRegular() || st.Mode().Perm() != 0600 || st.Sys().(*syscall.Stat_t).Nlink != 1) {
		err = errors.New("unsafe update lock")
	}
	if err == nil {
		err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	}
	if err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

// limitedOutput prevents an invalid candidate from filling memory while probing it.
type limitedOutput struct{ data []byte }

func (b *limitedOutput) Write(p []byte) (int, error) {
	if len(p)+len(b.data) > 8192 {
		return 0, errors.New("version output exceeds limit")
	}
	b.data = append(b.data, p...)
	return len(p), nil
}
func probe(ctx context.Context, file string) (Identity, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, file, "version")
	cmd.WaitDelay = time.Second
	out := new(limitedOutput)
	cmd.Stdout = out
	cmd.Stderr = io.Discard
	if err := cmd.Run(); err != nil {
		return Identity{}, errors.New("candidate version check failed")
	}
	var id Identity
	if err := json.Unmarshal(out.data, &id); err != nil {
		return id, errors.New("candidate returned invalid version identity")
	}
	if id.Component != "agent" || id.WireVersion != protocol.WireVersion || id.OS != runtime.GOOS || id.Arch != runtime.GOARCH {
		return id, errors.New("candidate component, platform or wire version mismatch")
	}
	return id, nil
}
func extract(archive *os.File, out *os.File) error {
	if _, err := archive.Seek(0, io.SeekStart); err != nil {
		return err
	}
	gz, err := gzip.NewReader(archive)
	if err != nil {
		return err
	}
	defer gz.Close()
	reader := tar.NewReader(gz)
	found := false
	var total int64
	for count := 0; ; count++ {
		header, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		if count > 10000 || header.Size < 0 || header.Size > maxBinary {
			return errors.New("invalid archive entry size/count")
		}
		total += header.Size
		name := strings.TrimSuffix(header.Name, "/")
		if total > maxArchive || name == "" || path.IsAbs(name) || path.Clean(name) != name || name == ".." || strings.HasPrefix(name, "../") || strings.Contains(name, "\\") {
			return errors.New("unsafe archive path or size")
		}
		if header.Typeflag != tar.TypeReg && header.Typeflag != tar.TypeDir {
			return errors.New("archive links and special files are refused")
		}
		if name != "hostmcp-agent" {
			continue
		}
		if found || header.Typeflag != tar.TypeReg {
			return errors.New("archive must contain exactly one regular hostmcp-agent")
		}
		found = true
		if _, err = io.Copy(out, reader); err != nil {
			return err
		}
	}
	// Read through the gzip trailer so truncated/corrupt streams fail before replacement.
	remaining, err := io.Copy(io.Discard, io.LimitReader(gz, maxArchive+1))
	if err != nil {
		return err
	}
	if remaining > maxArchive {
		return errors.New("archive trailing data exceeds limit")
	}
	if !found {
		return errors.New("agent executable is absent")
	}
	return nil
}
func syncDir(file string) error {
	d, err := os.Open(filepath.Dir(file))
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

// Inspect checks local update eligibility without downloading or replacing files.
func Inspect(ctx context.Context, p Policy) (Identity, error) {
	guard, err := lock(p)
	if err != nil {
		return Identity{}, err
	}
	defer guard.Close()
	return probe(ctx, p.Binary)
}

// Apply stages on the target filesystem, verifies the archive and executable,
// retains one local backup, then atomically renames. It never restarts services.
// The bool reports whether replacement occurred, including directory-sync errors.
func Apply(ctx context.Context, p Policy, archive io.Reader) (bool, error) {
	guard, err := lock(p)
	if err != nil {
		return false, err
	}
	defer guard.Close()
	original, err := safeFile(p.Binary)
	if err != nil {
		return false, err
	}
	current, err := probe(ctx, p.Binary)
	if err != nil {
		return false, fmt.Errorf("installed agent: %w", err)
	}
	old, _ := versionParts(current.Version)
	target, _ := versionParts(p.TargetVersion)
	if _, err = versionParts(current.Version); err != nil {
		return false, err
	}
	for i := range old {
		if target[i] > old[i] {
			break
		}
		if target[i] < old[i] {
			return false, errors.New("automatic downgrade refused; use local rollback")
		}
	}
	if current.Version == p.TargetVersion {
		return false, nil
	}
	dir := filepath.Dir(p.Binary)
	compressed, err := os.CreateTemp(dir, ".hostmcp-archive-*")
	if err != nil {
		return false, err
	}
	defer os.Remove(compressed.Name())
	defer compressed.Close()
	digest := sha256.New()
	n, err := io.Copy(io.MultiWriter(compressed, digest), io.LimitReader(archive, maxArchive+1))
	if err != nil {
		return false, err
	}
	if n > maxArchive || !strings.EqualFold(hex.EncodeToString(digest.Sum(nil)), p.SHA256) {
		return false, errors.New("archive SHA-256 mismatch or size exceeded; binary unchanged")
	}
	staged, err := os.CreateTemp(dir, ".hostmcp-agent-*")
	if err != nil {
		return false, err
	}
	defer os.Remove(staged.Name())
	defer staged.Close()
	if err = extract(compressed, staged); err != nil {
		return false, err
	}
	if err = staged.Chmod(original.Mode().Perm()); err != nil {
		return false, err
	}
	if err = staged.Sync(); err != nil {
		return false, err
	}
	if err = staged.Close(); err != nil {
		return false, err
	}
	candidate, err := probe(ctx, staged.Name())
	if err != nil {
		return false, err
	}
	if candidate.Version != p.TargetVersion {
		return false, errors.New("candidate version does not match policy")
	}
	if err = ctx.Err(); err != nil {
		return false, err
	}
	latest, err := safeFile(p.Binary)
	if err != nil {
		return false, err
	}
	if !os.SameFile(original, latest) || original.Size() != latest.Size() || !original.ModTime().Equal(latest.ModTime()) {
		return false, errors.New("installed agent changed during staging; replacement refused")
	}
	// An existing backup blocks further replacement, so rollback is never overwritten.
	backup := p.Binary + ".previous"
	if err = os.Link(p.Binary, backup); err != nil {
		return false, fmt.Errorf("preserve rollback binary (archive/remove a previous backup before next upgrade): %w", err)
	}
	if err = syncDir(p.Binary); err != nil {
		return false, err
	}
	if err = os.Rename(staged.Name(), p.Binary); err != nil {
		return false, err
	}
	return true, syncDir(p.Binary)
}

// Rollback restores only the locally preserved executable. No URL is accepted.
func Rollback(ctx context.Context, p Policy) error {
	guard, err := lock(p)
	if err != nil {
		return err
	}
	defer guard.Close()
	backup := p.Binary + ".previous"
	if _, err = safeFile(backup); err != nil {
		return err
	}
	if _, err = probe(ctx, backup); err != nil {
		return err
	}
	if err = os.Rename(backup, p.Binary); err != nil {
		return err
	}
	return syncDir(p.Binary)
}
