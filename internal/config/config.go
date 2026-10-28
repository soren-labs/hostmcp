// Package config stores a single owner's credentials and device inventory.
// Hub files hold SHA-256 digests, never the client/device bearer credentials.
package config

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"
	"unicode"
)

const FormatVersion = 1
const maxConfigBytes = 1 << 20

type Hub struct {
	Version        int          `json:"version"`
	Listen         string       `json:"listen"`
	PublicURL      string       `json:"public_url"`
	AllowedOrigins []string     `json:"allowed_origins,omitempty"`
	AllowedHosts   []string     `json:"allowed_hosts,omitempty"`
	Clients        []Client     `json:"clients"`
	Devices        []Device     `json:"devices"`
	Invitations    []Invitation `json:"invitations,omitempty"`
}

type Client struct {
	ID        string   `json:"id"`
	TokenHash string   `json:"token_hash"`
	Devices   []string `json:"devices"`
}

// A Device is one node process running as one OS account. A physical machine can
// register multiple devices (for example, workstation-dev and workstation-root).
// ExpectedUser is a deployment guard, not remote attestation of a trusted node.
type Device struct {
	ID           string `json:"id"`
	TokenHash    string `json:"token_hash"`
	ExpectedUser string `json:"expected_user,omitempty"`
}

func (d Device) AllowsUser(username string) bool {
	return d.ExpectedUser == "" || d.ExpectedUser == username
}

type Invitation struct {
	ExpectedUser string    `json:"expected_user,omitempty"`
	DeviceID     string    `json:"device_id"`
	TokenHash    string    `json:"token_hash"`
	ExpiresAt    time.Time `json:"expires_at"`
}

type Agent struct {
	ExpectedUser  string `json:"expected_user,omitempty"`
	Version       int    `json:"version"`
	HubURL        string `json:"hub_url"`
	DeviceID      string `json:"device_id"`
	Token         string `json:"token"`
	AllowInsecure bool   `json:"allow_insecure,omitempty"`
}

type Invite struct {
	ExpectedUser  string    `json:"expected_user,omitempty"`
	Version       int       `json:"version"`
	HubURL        string    `json:"hub_url"`
	DeviceID      string    `json:"device_id"`
	Token         string    `json:"token"`
	AllowInsecure bool      `json:"allow_insecure,omitempty"`
	ExpiresAt     time.Time `json:"expires_at"`
}

type Store struct{ Path string }

func NewStore(path string) *Store { return &Store{Path: path} }

func NewToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func Hash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

var idPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

func ValidID(id string) bool { return idPattern.MatchString(id) }

// ValidUser checks the portable account name carried in an invitation. The
// account is resolved by the node's OS, never looked up on the Hub machine.
func ValidUser(name string) bool {
	if name == "" || len(name) > 256 {
		return false
	}
	for _, r := range name {
		if unicode.IsControl(r) || unicode.IsSpace(r) || strings.ContainsRune("/:\\", r) {
			return false
		}
	}
	return true
}

func Allows(client Client, device string) bool {
	for _, id := range client.Devices {
		if id == "*" || id == device {
			return true
		}
	}
	return false
}

// CheckURL accepts a base URL, not an MCP endpoint. Clear-text remote transport
// requires an explicit deployment choice. Loopback is allowed for local tests.
func CheckURL(raw string, allowInsecure bool) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return nil, errors.New("hub URL must be an absolute http(s) origin without credentials, query or path")
	}
	if u.Scheme != "https" && u.Scheme != "http" {
		return nil, errors.New("hub URL must use https")
	}
	h := u.Hostname()
	ip := net.ParseIP(h)
	loopback := h == "localhost" || (ip != nil && ip.IsLoopback())
	if u.Scheme == "http" && !loopback && !allowInsecure {
		return nil, errors.New("remote hub requires HTTPS (explicit --allow-insecure is only for a trusted private test network)")
	}
	u.Path = ""
	return u, nil
}

func ReadJSON(path string, out any) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return err
	}
	if !st.Mode().IsRegular() {
		return errors.New("configuration must be a regular file")
	}
	if st.Mode().Perm()&0077 != 0 {
		return fmt.Errorf("configuration %s must be private: chmod 600 it", path)
	}
	if st.Size() > maxConfigBytes {
		return errors.New("configuration exceeds 1 MiB")
	}
	d := json.NewDecoder(io.LimitReader(f, maxConfigBytes+1))
	d.DisallowUnknownFields()
	if err = d.Decode(out); err != nil {
		return fmt.Errorf("invalid configuration: %w", err)
	}
	var extra any
	if err = d.Decode(&extra); err != io.EOF {
		return errors.New("configuration contains trailing JSON")
	}
	return nil
}

func WriteJSON(path string, value any, replace bool) error {
	b, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	return WriteSecret(path, append(b, '\n'), replace)
}

// WriteSecret creates an owner-only file atomically without following a target
// symlink. Existing configuration updates use the stable companion lock file.
func WriteSecret(path string, data []byte, replace bool) error {
	if len(data) > maxConfigBytes {
		return errors.New("configuration exceeds 1 MiB")
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	var original os.FileInfo
	if st, err := os.Lstat(path); err == nil {
		if !replace {
			return fmt.Errorf("refusing to overwrite %s", path)
		}
		if !st.Mode().IsRegular() {
			return errors.New("refusing to replace a non-regular configuration file")
		}
		original = st
	} else if !os.IsNotExist(err) {
		return err
	}
	f, err := os.CreateTemp(dir, ".hostmcp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if original != nil {
		err = preserveOwner(f, original)
	}
	if err == nil {
		err = f.Chmod(0600)
	}
	if err == nil {
		_, err = f.Write(data)
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if !replace {
		// Hard-link is an atomic no-clobber installation on the same filesystem.
		if err = os.Link(tmp, path); err != nil {
			return err
		}
	} else if err = os.Rename(tmp, path); err != nil {
		return err
	}
	if d, e := os.Open(dir); e == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}

// preserveOwner keeps an administrator's atomic replacement readable and
// writable by the service account. Do this on the temporary file before rename:
// if the caller cannot preserve ownership, the original stays untouched.
func preserveOwner(f *os.File, original os.FileInfo) error {
	want, ok := original.Sys().(*syscall.Stat_t)
	if !ok {
		return errors.New("cannot determine configuration ownership")
	}
	info, err := f.Stat()
	if err != nil {
		return err
	}
	got, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return errors.New("cannot determine replacement ownership")
	}
	if got.Uid == want.Uid && got.Gid == want.Gid {
		return nil
	}
	if err := f.Chown(int(want.Uid), int(want.Gid)); err != nil {
		return fmt.Errorf("cannot preserve configuration owner %d:%d: %w", want.Uid, want.Gid, err)
	}
	return nil
}

func (s *Store) Snapshot() (Hub, error) {
	var h Hub
	if err := ReadJSON(s.Path, &h); err != nil {
		return h, err
	}
	if err := validate(h); err != nil {
		return h, err
	}
	return h, nil
}

func validate(h Hub) error {
	if h.Version != FormatVersion {
		return errors.New("unsupported hub configuration version")
	}
	if _, _, err := net.SplitHostPort(h.Listen); err != nil {
		return fmt.Errorf("invalid listen address: %w", err)
	}
	if _, err := CheckURL(h.PublicURL, true); err != nil {
		return err
	}
	ids := make(map[string]bool)
	check := func(kind, id, hash string) error {
		if !ValidID(id) {
			return fmt.Errorf("invalid %s id", kind)
		}
		key := kind + ":" + id
		if ids[key] {
			return fmt.Errorf("duplicate %s id", kind)
		}
		ids[key] = true
		b, err := hex.DecodeString(hash)
		if err != nil || len(b) != 32 {
			return fmt.Errorf("invalid %s credential hash", kind)
		}
		return nil
	}
	if len(h.Clients) > 256 || len(h.Devices) > 1024 || len(h.Invitations) > 1024 {
		return errors.New("inventory limit exceeded")
	}
	for _, c := range h.Clients {
		if err := check("client", c.ID, c.TokenHash); err != nil {
			return err
		}
		if len(c.Devices) == 0 {
			return errors.New("client must have at least one device scope")
		}
		for _, d := range c.Devices {
			if d != "*" && !ValidID(d) {
				return errors.New("invalid device scope")
			}
		}
	}
	for _, d := range h.Devices {
		if d.ExpectedUser != "" && !ValidUser(d.ExpectedUser) {
			return errors.New("invalid device account name")
		}
		if err := check("device", d.ID, d.TokenHash); err != nil {
			return err
		}
	}
	for _, i := range h.Invitations {
		if i.ExpectedUser != "" && !ValidUser(i.ExpectedUser) {
			return errors.New("invalid invitation account name")
		}
		if err := check("invitation", i.DeviceID, i.TokenHash); err != nil {
			return err
		}
	}
	for _, origin := range h.AllowedOrigins {
		if _, err := CheckURL(origin, true); err != nil {
			return errors.New("allowed origins must be explicit http(s) origins")
		}
	}
	for _, host := range h.AllowedHosts {
		if host == "" || strings.ContainsAny(host, "/\\\r\n* @") {
			return errors.New("allowed hosts must be explicit host[:port] values")
		}
	}
	return nil
}

// Validate checks a new hub configuration before any credential is written.
func Validate(h Hub) error { return validate(h) }

func (s *Store) Update(fn func(*Hub) error) error {
	configInfo, err := os.Lstat(s.Path)
	if err != nil {
		return err
	}
	if !configInfo.Mode().IsRegular() {
		return errors.New("configuration must be a regular file")
	}
	if owner := configInfo.Sys().(*syscall.Stat_t).Uid; os.Geteuid() != 0 && uint32(os.Geteuid()) != owner {
		return errors.New("configuration must be updated by its owner or an administrator")
	}
	lockPath := s.Path + ".lock"
	lock, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return fmt.Errorf("open configuration lock %s (use the configuration owner or an administrator): %w", lockPath, err)
	}
	defer lock.Close()
	lockInfo, err := lock.Stat()
	if err != nil {
		return err
	}
	if !lockInfo.Mode().IsRegular() || lockInfo.Sys().(*syscall.Stat_t).Nlink != 1 {
		return errors.New("configuration lock must be a regular file with one link")
	}
	if err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	configInfo, err = os.Lstat(s.Path)
	if err != nil {
		return err
	}
	if !configInfo.Mode().IsRegular() {
		return errors.New("configuration must be a regular file")
	}
	if err = preserveOwner(lock, configInfo); err != nil {
		return fmt.Errorf("configuration lock: %w", err)
	}
	if err = lock.Chmod(0600); err != nil {
		return fmt.Errorf("configuration lock permissions: %w", err)
	}
	h, err := s.Snapshot()
	if err != nil {
		return err
	}
	if err = fn(&h); err != nil {
		return err
	}
	if err = validate(h); err != nil {
		return err
	}
	return WriteJSON(s.Path, h, true)
}

func matches(token, hash string) bool {
	if len(token) < 32 || len(token) > 1024 {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(Hash(token)), []byte(hash)) == 1
}

func (s *Store) AuthClient(token string) (Client, bool) {
	h, err := s.Snapshot()
	if err != nil {
		return Client{}, false
	}
	for _, c := range h.Clients {
		if matches(token, c.TokenHash) {
			return c, true
		}
	}
	return Client{}, false
}

func (s *Store) AuthDevice(id, token string) bool {
	if len(token) < 32 || len(token) > 1024 {
		return false
	}
	return s.DeviceValid(id, Hash(token))
}

func (s *Store) DeviceValid(id, hash string) bool {
	h, err := s.Snapshot()
	if err != nil {
		return false
	}
	for _, d := range h.Devices {
		if d.ID == id && subtle.ConstantTimeCompare([]byte(d.TokenHash), []byte(hash)) == 1 {
			return true
		}
	}
	return false
}

// DeviceValidForUser checks both the node credential and its configured account
// binding. It must be rechecked at dispatch, so an edited binding takes effect
// immediately even before the background connection revocation pass.
func (s *Store) DeviceValidForUser(id, hash, username string) bool {
	h, err := s.Snapshot()
	if err != nil {
		return false
	}
	for _, d := range h.Devices {
		if d.ID == id && subtle.ConstantTimeCompare([]byte(d.TokenHash), []byte(hash)) == 1 {
			return d.AllowsUser(username)
		}
	}
	return false
}

func (s *Store) Redeem(id, token string) (Agent, error) {
	var result Agent
	err := s.Update(func(h *Hub) error {
		index := -1
		for n, i := range h.Invitations {
			if i.DeviceID == id && time.Now().Before(i.ExpiresAt) && matches(token, i.TokenHash) {
				index = n
				break
			}
		}
		if index < 0 {
			return errors.New("invalid or expired invitation")
		}
		for _, d := range h.Devices {
			if d.ID == id {
				return errors.New("device already enrolled; revoke it before enrolling again")
			}
		}
		secret, err := NewToken()
		if err != nil {
			return err
		}
		expectedUser := h.Invitations[index].ExpectedUser
		h.Devices = append(h.Devices, Device{ID: id, TokenHash: Hash(secret), ExpectedUser: expectedUser})
		h.Invitations = append(h.Invitations[:index], h.Invitations[index+1:]...)
		result = Agent{Version: FormatVersion, HubURL: h.PublicURL, DeviceID: id, Token: secret, ExpectedUser: expectedUser, AllowInsecure: strings.HasPrefix(h.PublicURL, "http://")}
		return nil
	})
	return result, err
}

func (s *Store) InviteDevice(id string, ttl time.Duration) (Invite, error) {
	return s.InviteDeviceForUser(id, "", ttl)
}

// InviteDeviceForUser binds the resulting node to a named OS account. Empty user
// preserves the original invitation behavior for existing deployments.
func (s *Store) InviteDeviceForUser(id, username string, ttl time.Duration) (Invite, error) {
	if username != "" && !ValidUser(username) {
		return Invite{}, errors.New("user must be a valid OS account name")
	}
	if !ValidID(id) {
		return Invite{}, errors.New("device id must be 1-64 letters, digits, dots, underscores or hyphens")
	}
	if ttl <= 0 || ttl > 24*time.Hour {
		return Invite{}, errors.New("invitation TTL must be between zero and 24h")
	}
	secret, err := NewToken()
	if err != nil {
		return Invite{}, err
	}
	var out Invite
	err = s.Update(func(h *Hub) error {
		for _, d := range h.Devices {
			if d.ID == id {
				return errors.New("device already enrolled; revoke it before enrolling again")
			}
		}
		active := make([]Invitation, 0, len(h.Invitations)+1)
		for _, i := range h.Invitations {
			if i.DeviceID != id && time.Now().Before(i.ExpiresAt) {
				active = append(active, i)
			}
		}
		expires := time.Now().Add(ttl).UTC()
		h.Invitations = append(active, Invitation{DeviceID: id, TokenHash: Hash(secret), ExpectedUser: username, ExpiresAt: expires})
		out = Invite{Version: FormatVersion, HubURL: h.PublicURL, DeviceID: id, Token: secret, ExpectedUser: username, AllowInsecure: strings.HasPrefix(h.PublicURL, "http://"), ExpiresAt: expires}
		return nil
	})
	return out, err
}
