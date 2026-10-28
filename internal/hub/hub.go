// Package hub authenticates MCP clients and routes tools to outbound device
// connections. It never runs a shell or touches a client-supplied filesystem path.
package hub

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/soren-labs/hostmcp/internal/config"
	"github.com/soren-labs/hostmcp/internal/mcpserver"
	"github.com/soren-labs/hostmcp/internal/protocol"
)

type Hub struct {
	store           *config.Store
	handler         http.Handler
	ctx             context.Context
	cancel          context.CancelFunc
	mu              sync.RWMutex
	connections     map[string]*deviceConn
	known           map[string]protocol.Device
	requests        chan struct{}
	enrollments     chan struct{}
	handshakes      chan struct{}
	limitsMu        sync.Mutex
	clientSlots     map[string]*clientLimit
	lastDenied      atomic.Int64
	lastConfigError atomic.Int64
}
type clientLimit struct {
	slots chan struct{}
	refs  int
}
type deviceConn struct {
	id      string
	hash    string
	info    protocol.Device
	conn    *websocket.Conn
	done    chan struct{}
	once    sync.Once
	mu      sync.Mutex
	pending map[string]chan protocol.Message
}

func (d *deviceConn) close() { d.once.Do(func() { close(d.done); _ = d.conn.CloseNow() }) }

func New(store *config.Store, tools []protocol.Tool) (*Hub, error) {
	if store == nil {
		return nil, errors.New("configuration store is required")
	}
	if _, err := store.Snapshot(); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	h := &Hub{store: store, ctx: ctx, cancel: cancel, connections: map[string]*deviceConn{}, known: map[string]protocol.Device{}, requests: make(chan struct{}, 24), enrollments: make(chan struct{}, 2), handshakes: make(chan struct{}, 8), clientSlots: make(map[string]*clientLimit)}
	server := mcpserver.New(mcpserver.Options{Tools: tools, Remote: true, Call: h.Call, ListDevices: h.ListDevices})
	streamable := mcp.NewStreamableHTTPHandler(func(_ *http.Request) *mcp.Server { return server }, &mcp.StreamableHTTPOptions{
		Stateless: true, JSONResponse: true, MaxRequestBodyBytes: protocol.MaxMessageBytes,
		// This is replaced with an explicit configured-host allowlist below so a
		// localhost cloudflared/Caddy upstream can carry the real public Host.
		DisableLocalhostProtection: true, PropagateRequestCancellation: true,
	})
	authHandler := auth.RequireBearerToken(func(_ context.Context, token string, _ *http.Request) (*auth.TokenInfo, error) {
		client, ok := store.AuthClient(token)
		if !ok {
			return nil, auth.ErrInvalidToken
		}
		return &auth.TokenInfo{UserID: clientPrincipal(client)}, nil
	}, &auth.RequireBearerTokenOptions{AllowMissingExpiration: true})(streamable)
	mux := http.NewServeMux()
	mux.Handle("/mcp", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("WWW-Authenticate", "Bearer realm=\"hostmcp\"")
		if bearer(r) == "" {
			h.auditDenied("mcp", "missing_bearer")
			http.Error(w, "one bearer credential is required", http.StatusUnauthorized)
			return
		}
		h.boundedClient(authHandler).ServeHTTP(w, r)
	}))
	mux.HandleFunc("/agent/", h.acceptAgent)
	mux.Handle("/enroll", h.bounded(h.enrollments, http.HandlerFunc(h.enroll)))
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", "GET")
			http.Error(w, "method not allowed", 405)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "ok", "version": protocol.Version})
	})
	h.handler = h.protect(mux)
	go h.recheckDevices()
	return h, nil
}
func (h *Hub) Handler() http.Handler { return h.handler }
func (h *Hub) Close() error {
	h.cancel()
	h.mu.Lock()
	connections := h.connections
	h.connections = map[string]*deviceConn{}
	h.mu.Unlock()
	for _, d := range connections {
		d.close()
	}
	return nil
}

// Enrollment never competes with long-running MCP calls for capacity. Client
// calls also have a four-request per-token limit so one coding agent cannot
// starve every other principal, and a brief bounded queue absorbs bursts.
func (h *Hub) bounded(pool chan struct{}, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 1500*time.Millisecond)
		defer cancel()
		select {
		case pool <- struct{}{}:
			defer func() { <-pool }()
		case <-ctx.Done():
			w.Header().Set("Retry-After", "1")
			http.Error(w, "too many active requests", 503)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func clientPrincipal(c config.Client) string {
	// A new credential reusing the same display ID must never inherit the
	// previous credential's terminal sessions or deduplication records.
	return c.ID + "~" + c.TokenHash[:24]
}

func auditPrincipal(p string) string {
	if i := strings.IndexByte(p, '~'); i >= 0 {
		return p[:i]
	}
	return p
}

// Failed authentications are sampled so token sprays cannot create unbounded
// log volume. No token, hash, request body, or command arguments are logged.
func (h *Hub) auditDenied(endpoint, reason string) {
	now := time.Now().UnixNano()
	last := h.lastDenied.Load()
	if now-last > int64(5*time.Second) && h.lastDenied.CompareAndSwap(last, now) {
		log.Printf("event=auth_denied endpoint=%q reason=%q", endpoint, reason)
	}
}

func (h *Hub) boundedClient(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := bearer(r)
		if _, ok := h.store.AuthClient(token); !ok {
			h.auditDenied("mcp", "invalid_bearer")
			http.Error(w, "invalid bearer credential", http.StatusUnauthorized)
			return
		}
		key := config.Hash(token)
		h.limitsMu.Lock()
		limit := h.clientSlots[key]
		if limit == nil {
			limit = &clientLimit{slots: make(chan struct{}, 4)}
			h.clientSlots[key] = limit
		}
		limit.refs++
		h.limitsMu.Unlock()
		defer func() {
			h.limitsMu.Lock()
			limit.refs--
			if limit.refs == 0 {
				delete(h.clientSlots, key)
			}
			h.limitsMu.Unlock()
		}()
		ctx, cancel := context.WithTimeout(r.Context(), 1500*time.Millisecond)
		defer cancel()
		select {
		case limit.slots <- struct{}{}:
			defer func() { <-limit.slots }()
		case <-ctx.Done():
			w.Header().Set("Retry-After", "1")
			http.Error(w, "principal has too many active requests; retry", 503)
			return
		}
		select {
		case h.requests <- struct{}{}:
			defer func() { <-h.requests }()
		case <-ctx.Done():
			w.Header().Set("Retry-After", "1")
			http.Error(w, "hub has too many active requests; retry", 503)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (h *Hub) protect(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		cfg, err := h.store.Snapshot()
		if err != nil {
			h.auditDenied("http", "config_unavailable")
			http.Error(w, "configuration unavailable", 503)
			return
		}
		if !hostAllowed(r.Host, cfg) {
			h.auditDenied("http", "invalid_host")
			http.Error(w, "host not allowed", 403)
			return
		}
		origins := r.Header.Values("Origin")
		if len(origins) > 1 || (len(origins) == 1 && !originAllowed(origins[0], r, cfg)) {
			h.auditDenied("http", "invalid_origin")
			http.Error(w, "origin not allowed", 403)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func authority(value, defaultPort string) (string, string) {
	u, err := url.Parse("http://" + value)
	if err != nil || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
		return "", ""
	}
	port := u.Port()
	if port == "" {
		port = defaultPort
	}
	return strings.ToLower(strings.TrimSuffix(u.Hostname(), ".")), port
}
func hostAllowed(requestHost string, cfg config.Hub) bool {
	public, err := url.Parse(cfg.PublicURL)
	if err != nil {
		return false
	}
	defaultPort := "80"
	if public.Scheme == "https" {
		defaultPort = "443"
	}
	host, port := authority(requestHost, defaultPort)
	if host == "" {
		return false
	}
	publicHost, publicPort := authority(public.Host, defaultPort)
	if host == publicHost && port == publicPort {
		return true
	}
	bindHost, bindPort, bindErr := net.SplitHostPort(cfg.Listen)
	if bindErr == nil {
		// A :0 bind is only useful to tests/embedded hosts; the actual ephemeral
		// authority is not known until Listen has allocated it.
		loopback := host == "localhost" || host == "127.0.0.1" || host == "::1"
		if (port == bindPort || bindPort == "0") && (loopback || (bindHost != "0.0.0.0" && bindHost != "::" && strings.EqualFold(host, bindHost))) {
			return true
		}
	}
	for _, allowed := range cfg.AllowedHosts {
		allowedHost, allowedPort := authority(allowed, defaultPort)
		if host == allowedHost && port == allowedPort {
			return true
		}
	}
	return false
}
func originAllowed(origin string, r *http.Request, cfg config.Hub) bool {
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Path != "" || (u.Scheme != "https" && u.Scheme != "http") {
		return false
	}
	if origin == strings.TrimSuffix(cfg.PublicURL, "/") {
		return true
	}
	for _, allowed := range cfg.AllowedOrigins {
		if origin == strings.TrimSuffix(allowed, "/") {
			return true
		}
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	return origin == scheme+"://"+r.Host && hostAllowed(r.Host, cfg)
}
func bearer(r *http.Request) string {
	values := r.Header.Values("Authorization")
	if len(values) != 1 {
		return ""
	}
	fields := strings.Fields(values[0])
	if len(fields) != 2 || !strings.EqualFold(fields[0], "Bearer") {
		return ""
	}
	return fields[1]
}

func (h *Hub) enroll(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "method not allowed", 405)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 8192)
	var input struct {
		DeviceID string `json:"device_id"`
		Token    string `json:"token"`
	}
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&input); err != nil {
		http.Error(w, "invalid enrollment request", 400)
		return
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		http.Error(w, "invalid enrollment request", 400)
		return
	}
	agent, err := h.store.Redeem(input.DeviceID, input.Token)
	if err != nil {
		h.auditDenied("enroll", "invalid_or_expired_invitation")
		http.Error(w, "invalid, expired, or already used invitation", 401)
		return
	}
	log.Printf("event=device_enrolled device=%q", input.DeviceID)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(agent)
}

func (h *Hub) acceptAgent(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", 405)
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/agent/")
	token := bearer(r)
	if id == "" || strings.Contains(id, "/") || !h.store.AuthDevice(id, token) {
		h.auditDenied("agent", "invalid_credentials")
		w.Header().Set("WWW-Authenticate", "Bearer realm=\"hostmcp-device\"")
		http.Error(w, "invalid device credentials", 401)
		return
	}
	select {
	case h.handshakes <- struct{}{}:
	default:
		http.Error(w, "too many device handshakes", 503)
		return
	}
	releaseHandshake := sync.OnceFunc(func() { <-h.handshakes })
	defer releaseHandshake()
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		// h.protect has already validated any Origin against the exact allowlist.
		InsecureSkipVerify: true, CompressionMode: websocket.CompressionDisabled,
	})
	if err != nil {
		return
	}
	conn.SetReadLimit(protocol.MaxMessageBytes)
	handshakeCtx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	var hello protocol.Message
	err = wsjson.Read(handshakeCtx, conn, &hello)
	cancel()
	if err != nil || hello.Type != "hello" || hello.Version != protocol.WireVersion || hello.Device == nil || hello.Device.ID != id {
		log.Printf("event=device_rejected device=%q reason=invalid_handshake", id)
		_ = conn.Close(websocket.StatusPolicyViolation, "invalid handshake")
		return
	}
	if !validInfo(*hello.Device) {
		log.Printf("event=device_rejected device=%q reason=invalid_info", id)
		_ = conn.Close(websocket.StatusPolicyViolation, "invalid device info")
		return
	}
	if !h.store.DeviceValidForUser(id, config.Hash(token), hello.Device.Username) {
		log.Printf("event=device_rejected device=%q reason=account_binding", id)
		_ = conn.Close(websocket.StatusPolicyViolation, "device account does not match enrollment")
		return
	}
	info := *hello.Device
	info.ID = id
	info.Online = true
	info.ConnectedAt = time.Now().UTC()
	d := &deviceConn{id: id, hash: config.Hash(token), info: info, conn: conn, done: make(chan struct{}), pending: map[string]chan protocol.Message{}}
	h.mu.Lock()
	old := h.connections[id]
	h.connections[id] = d
	h.known[id] = info
	h.mu.Unlock()
	if old != nil {
		log.Printf("event=device_replaced device=%q user=%q", id, info.Username)
		old.close()
	}
	log.Printf("event=device_connected device=%q user=%q uid=%q", id, info.Username, info.EUID)
	defer func() {
		log.Printf("event=device_disconnected device=%q user=%q", id, info.Username)
		d.close()
		h.mu.Lock()
		if h.connections[id] == d {
			delete(h.connections, id)
			if info, ok := h.known[id]; ok {
				info.Online = false
				h.known[id] = info
			}
		}
		h.mu.Unlock()
	}()
	welcomeCtx, cancel := context.WithTimeout(h.ctx, 10*time.Second)
	err = wsjson.Write(welcomeCtx, conn, protocol.Message{Type: "welcome", Version: protocol.WireVersion})
	cancel()
	if err != nil {
		return
	}
	releaseHandshake()
	go h.heartbeat(d, 20*time.Second, 8*time.Second)
	for {
		var msg protocol.Message
		if err := wsjson.Read(h.ctx, conn, &msg); err != nil {
			return
		}
		if msg.Type != "result" || msg.ID == "" || len(msg.ID) > 128 {
			_ = conn.Close(websocket.StatusPolicyViolation, "invalid reply")
			return
		}
		d.mu.Lock()
		reply := d.pending[msg.ID]
		d.mu.Unlock()
		if reply != nil {
			select {
			case reply <- msg:
			default:
			}
		}
	}
}

func (h *Hub) heartbeat(d *deviceConn, interval, timeout time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-h.ctx.Done():
			return
		case <-d.done:
			return
		case <-ticker.C:
			ctx, cancel := context.WithTimeout(h.ctx, timeout)
			err := d.conn.Ping(ctx)
			cancel()
			if err != nil {
				d.close()
				return
			}
		}
	}
}
func validInfo(d protocol.Device) bool {
	return len(d.ID) <= 64 && len(d.Hostname) <= 255 && len(d.OS) <= 32 && len(d.Arch) <= 32 && len(d.Username) <= 256 && len(d.UID) <= 32 && len(d.EUID) <= 32 && len(d.ExpectedUser) <= 256 && len(d.Home) <= 4096 && len(d.Version) <= 64
}

func (h *Hub) recheckDevices() {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-h.ctx.Done():
			return
		case <-ticker.C:
			cfg, cfgErr := h.store.Snapshot()
			if cfgErr != nil {
				now := time.Now().UnixNano()
				last := h.lastConfigError.Load()
				if now-last > int64(30*time.Second) && h.lastConfigError.CompareAndSwap(last, now) {
					log.Printf("event=config_recheck_error action=retain_connections: %v", cfgErr)
				}
				continue
			}
			valid := make(map[string]config.Device)
			for _, entry := range cfg.Devices {
				valid[entry.ID] = entry
			}
			h.mu.Lock()
			for id := range h.known {
				if _, ok := valid[id]; !ok {
					delete(h.known, id)
				}
			}
			h.mu.Unlock()
			h.mu.RLock()
			list := make([]*deviceConn, 0, len(h.connections))
			for _, d := range h.connections {
				list = append(list, d)
			}
			h.mu.RUnlock()
			for _, d := range list {
				entry, ok := valid[d.id]
				if !ok || entry.TokenHash != d.hash || !entry.AllowsUser(d.info.Username) {
					d.close()
				}
			}
		}
	}
}
func (h *Hub) client(principal string) (config.Client, error) {
	cfg, err := h.store.Snapshot()
	if err != nil {
		return config.Client{}, errors.New("configuration unavailable")
	}
	for _, c := range cfg.Clients {
		if c.ID == principal || clientPrincipal(c) == principal {
			return c, nil
		}
	}
	return config.Client{}, errors.New("client has been revoked")
}
func (h *Hub) ListDevices(_ context.Context, principal string) ([]protocol.Device, error) {
	log.Printf("event=tool_call principal=%q tool=list_devices", auditPrincipal(principal))
	client, err := h.client(principal)
	if err != nil {
		return nil, err
	}
	cfg, err := h.store.Snapshot()
	if err != nil {
		return nil, err
	}
	h.mu.RLock()
	defer h.mu.RUnlock()
	devices := make([]protocol.Device, 0)
	for _, entry := range cfg.Devices {
		if !config.Allows(client, entry.ID) {
			continue
		}
		info, ok := h.known[entry.ID]
		if !ok {
			info = protocol.Device{ID: entry.ID}
		}
		info.ExpectedUser = entry.ExpectedUser
		_, info.Online = h.connections[entry.ID]
		devices = append(devices, info)
	}
	sort.Slice(devices, func(i, j int) bool { return devices[i].ID < devices[j].ID })
	return devices, nil
}
func (h *Hub) Call(ctx context.Context, principal, device, tool string, args json.RawMessage) (result protocol.Result, err error) {
	start := time.Now()
	requestID := "not_dispatched"
	defer func() {
		status := "ok"
		if err != nil || result.Error != "" {
			status = "error"
		}
		log.Printf("event=tool_call principal=%q device=%q tool=%q request_id=%q outcome=%q elapsed_ms=%d", auditPrincipal(principal), device, tool, requestID, status, time.Since(start).Milliseconds())
	}()
	client, err := h.client(principal)
	if err != nil {
		return protocol.Result{}, err
	}
	if !config.Allows(client, device) {
		return protocol.Result{}, errors.New("client is not authorized for this device")
	}
	h.mu.RLock()
	d := h.connections[device]
	h.mu.RUnlock()
	if d == nil {
		return protocol.Result{}, errors.New("device is offline; command was not dispatched")
	}
	if !h.store.DeviceValidForUser(d.id, d.hash, d.info.Username) {
		d.close()
		return protocol.Result{}, errors.New("device was revoked or its account binding changed; command was not dispatched")
	}
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return protocol.Result{}, err
	}
	id := hex.EncodeToString(b)
	requestID = id
	reply := make(chan protocol.Message, 1)
	d.mu.Lock()
	if len(d.pending) >= 16 {
		d.mu.Unlock()
		return protocol.Result{}, errors.New("device has too many active requests; command was not dispatched")
	}
	d.pending[id] = reply
	d.mu.Unlock()
	defer func() { d.mu.Lock(); delete(d.pending, id); d.mu.Unlock() }()
	callCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	writeCtx, writeCancel := context.WithTimeout(callCtx, 10*time.Second)
	err = wsjson.Write(writeCtx, d.conn, protocol.Message{Type: "request", ID: id, Principal: principal, Tool: tool, Args: args})
	writeCancel()
	unknown := func(reason string) (protocol.Result, error) {
		return protocol.Result{}, fmt.Errorf("%s; execution outcome unknown (device=%s, request=%s). Do not automatically retry side effects; reconnect and use list_processes/write_stdin to inspect any started command", reason, device, id)
	}
	if err != nil {
		d.close()
		return unknown("connection failed during dispatch")
	}
	select {
	case msg := <-reply:
		if msg.Error != "" {
			return protocol.Result{}, errors.New(msg.Error)
		}
		if msg.Result == nil {
			return protocol.Result{}, errors.New("device returned an empty result")
		}
		return *msg.Result, nil
	case <-d.done:
		return unknown("device disconnected after dispatch")
	case <-callCtx.Done():
		return unknown("tool response deadline or client cancellation")
	}
}
