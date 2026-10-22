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

type Device struct {
	ID        string `json:"id"`
	TokenHash string `json:"token_hash"`
}

type Invitation struct {
	DeviceID  string    `json:"device_id"`
	TokenHash string    `json:"token_hash"`
	ExpiresAt time.Time `json:"expires_at"`
}

type Agent struct {
	Version       int    `json:"version"`
	HubURL        string `json:"hub_url"`
	DeviceID      string `json:"device_id"`
	Token         string `json:"token"`
	AllowInsecure bool   `json:"allow_insecure,omitempty"`
}

type Invite struct {
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
	if st, err := os.Lstat(path); err == nil {
		if !replace {
			return fmt.Errorf("refusing to overwrite %s", path)
		}
		if !st.Mode().IsRegular() {
			return errors.New("refusing to replace a non-regular configuration file")
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	f, err := os.CreateTemp(dir, ".hostmcp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if err = f.Chmod(0600); err == nil {
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
		if err := check("device", d.ID, d.TokenHash); err != nil {
			return err
		}
	}
	for _, i := range h.Invitations {
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
	lock, err := os.OpenFile(s.Path+".lock", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
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
		h.Devices = append(h.Devices, Device{ID: id, TokenHash: Hash(secret)})
		h.Invitations = append(h.Invitations[:index], h.Invitations[index+1:]...)
		result = Agent{Version: FormatVersion, HubURL: h.PublicURL, DeviceID: id, Token: secret, AllowInsecure: strings.HasPrefix(h.PublicURL, "http://")}
		return nil
	})
	return result, err
}

func (s *Store) InviteDevice(id string, ttl time.Duration) (Invite, error) {
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
		h.Invitations = append(active, Invitation{DeviceID: id, TokenHash: Hash(secret), ExpiresAt: expires})
		out = Invite{Version: FormatVersion, HubURL: h.PublicURL, DeviceID: id, Token: secret, AllowInsecure: strings.HasPrefix(h.PublicURL, "http://"), ExpiresAt: expires}
		return nil
	})
	return out, err
}
