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
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/soren-labs/rmc/internal/cloudflareaccess"
	"github.com/soren-labs/rmc/internal/config"
	"github.com/soren-labs/rmc/internal/mcpserver"
	"github.com/soren-labs/rmc/internal/oauth"
	"github.com/soren-labs/rmc/internal/protocol"
)

type Hub struct {
	access          *cloudflareaccess.Verifier
	accessResource  string
	oauth           *oauth.Verifier
	oauthScopes     []string
	metadataURL     string
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
	lastConfigError atomic.Int64
	// requestDeadline bounds every admitted /mcp request; auditEvery is the
	// minimum interval between log lines for one audit event kind. Both are
	// fields so tests can shorten them.
	requestDeadline time.Duration
	auditEvery      time.Duration
	auditMu         sync.Mutex
	auditLast       map[string]time.Time
	auditSuppressed map[string]int64
	// toolArgs is the native tool surface this Hub advertises to MCP clients.
	toolArgs toolSet
}

// defaultRequestDeadline cancels admitted /mcp requests and interrupts their
// network I/O. Context-aware handlers such as SEP-2575 subscriptions/listen then
// release their concurrency slots. This cannot preempt arbitrary handler code
// that ignores cancellation. It exceeds Call's 45s dispatch deadline and stays
// inside the server's 50s WriteTimeout.
const defaultRequestDeadline = 48 * time.Second

// defaultAuditEvery limits each audit event kind to one log line per interval.
const defaultAuditEvery = 5 * time.Second

// The SDK detaches legacy tool-handler contexts from the HTTP context's Done
// and Deadline methods. Values survive that detachment, so keep the absolute
// admission deadline here and restore it at the Hub's dispatch boundary.
type requestDeadlineKey struct{}

type clientLimit struct {
	slots chan struct{}
	refs  int
}
type deviceConn struct {
	id      string
	hash    string
	info    protocol.Device
	tools   toolSet // nil when the Agent did not advertise tool_args
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
	cfg, err := store.Snapshot()
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	h := &Hub{store: store, ctx: ctx, cancel: cancel, connections: map[string]*deviceConn{}, known: map[string]protocol.Device{}, requests: make(chan struct{}, 24), enrollments: make(chan struct{}, 2), handshakes: make(chan struct{}, 8), clientSlots: make(map[string]*clientLimit), requestDeadline: defaultRequestDeadline, auditEvery: defaultAuditEvery}
	h.toolArgs = toolArgsOf(tools)
	if cfg.CloudflareAccess != nil {
		h.access, err = cloudflareaccess.New(ctx, *cfg.CloudflareAccess)
		if err != nil {
			cancel()
			return nil, err
		}
		h.accessResource = strings.TrimSuffix(cfg.PublicURL, "/") + "/mcp"
	}
	if cfg.OAuth != nil {
		discoveryCtx, stop := context.WithTimeout(ctx, 20*time.Second)
		h.oauth, err = oauth.New(discoveryCtx, cfg.OAuth.Issuer, strings.TrimSuffix(cfg.PublicURL, "/")+"/mcp")
		stop()
		if err != nil {
			cancel()
			return nil, err
		}
		h.oauthScopes = append([]string(nil), cfg.OAuth.Scopes...)
		h.metadataURL = strings.TrimSuffix(cfg.PublicURL, "/") + "/.well-known/oauth-protected-resource/mcp"
	}
	server := mcpserver.New(mcpserver.Options{Tools: tools, Remote: true, Call: h.Call, ListDevices: h.ListDevices})
	streamable := mcp.NewStreamableHTTPHandler(func(_ *http.Request) *mcp.Server { return server }, &mcp.StreamableHTTPOptions{
		Stateless: true, JSONResponse: true, MaxRequestBodyBytes: protocol.MaxMessageBytes,
		// This is replaced with an explicit configured-host allowlist below so a
		// localhost cloudflared/Caddy upstream can carry the real public Host.
		DisableLocalhostProtection: true, PropagateRequestCancellation: true,
	})
	authHandler := auth.RequireBearerToken(cachedTokenInfo, &auth.RequireBearerTokenOptions{AllowMissingExpiration: true, ResourceMetadataURL: h.metadataURL})(streamable)
	mux := http.NewServeMux()
	mux.Handle("/mcp", h.boundedClient(authHandler))
	if h.oauth != nil {
		mux.HandleFunc("/.well-known/oauth-protected-resource", h.metadata)
		mux.HandleFunc("/.well-known/oauth-protected-resource/mcp", h.metadata)
	}
	mux.HandleFunc("/agent/", h.acceptAgent)
	mux.Handle("/enroll", h.bounded(h.enrollments, http.HandlerFunc(h.enroll)))
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", "GET")
			http.Error(w, "method not allowed", 405)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "ok", "version": protocol.Version, "component": "hub", "wire_version": protocol.WireVersion})
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

// audit logs an operational event at most once per auditEvery for each
// event/endpoint/reason combination, and reports how many identical events
// were suppressed since the previous line. Sampling per kind means a noisy
// stale agent cannot hide MCP denials or capacity rejections. Keys are code
// constants, so the maps stay bounded. attrs are key/value pairs of
// non-secret values; never pass tokens, hashes, request bodies or arguments.
func (h *Hub) audit(event, endpoint, reason string, attrs ...string) {
	key := event + "\x00" + endpoint + "\x00" + reason
	now := time.Now()
	h.auditMu.Lock()
	if h.auditLast == nil {
		h.auditLast = map[string]time.Time{}
		h.auditSuppressed = map[string]int64{}
	}
	if last, ok := h.auditLast[key]; ok && now.Sub(last) < h.auditEvery {
		h.auditSuppressed[key]++
		h.auditMu.Unlock()
		return
	}
	suppressed := h.auditSuppressed[key]
	h.auditLast[key] = now
	delete(h.auditSuppressed, key)
	h.auditMu.Unlock()
	var extra strings.Builder
	for i := 0; i+1 < len(attrs); i += 2 {
		fmt.Fprintf(&extra, " %s=%q", attrs[i], attrs[i+1])
	}
	log.Printf("event=%s endpoint=%q reason=%q%s suppressed=%d", event, endpoint, reason, extra.String(), suppressed)
}

// auditDenied records a rejected request. No token, hash, request body, or
// command arguments are logged.
func (h *Hub) auditDenied(endpoint, reason string, attrs ...string) {
	h.audit("auth_denied", endpoint, reason, attrs...)
}

func (h *Hub) boundedClient(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Has("access_token") {
			http.Error(w, "tokens must use Authorization header", 400)
			return
		}
		info, code := h.authenticate(r)
		if code != 0 {
			h.challenge(w, code)
			h.auditDenied("mcp", "credential_rejected")
			http.Error(w, http.StatusText(code), code)
			return
		}
		ctxWithToken := context.WithValue(r.Context(), tokenInfoKey{}, info)
		if info.Extra["cloudflare_access"] == true {
			// Only authenticate can create this context after verifying the JWT and
			// policy. The SDK still requires a bearer header. This public sentinel
			// is never a credential and must not mutate the caller's request.
			r = r.Clone(ctxWithToken)
			r.Header.Set("Authorization", "Bearer access-assertion-validated")
			r.Header.Del(cloudflareaccess.Header)
		} else {
			r = r.WithContext(ctxWithToken)
		}
		key := info.UserID
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
			h.audit("mcp_busy", "mcp", "principal_busy")
			w.Header().Set("Retry-After", "1")
			http.Error(w, "principal has too many active requests; retry", 503)
			return
		}
		select {
		case h.requests <- struct{}{}:
			defer func() { <-h.requests }()
		case <-ctx.Done():
			h.audit("mcp_busy", "mcp", "hub_busy")
			w.Header().Set("Retry-After", "1")
			http.Error(w, "hub has too many active requests; retry", 503)
			return
		}
		requestCtx, cancelRequest := context.WithTimeout(r.Context(), h.requestDeadline)
		defer cancelRequest()
		deadline, _ := requestCtx.Deadline()
		requestCtx = context.WithValue(requestCtx, requestDeadlineKey{}, deadline)
		stopIO := interruptRequestIO(requestCtx, w)
		defer stopIO()
		next.ServeHTTP(w, r.WithContext(requestCtx))
		if errors.Is(requestCtx.Err(), context.DeadlineExceeded) {
			h.audit("mcp_deadline", "mcp", "request_deadline")
		}
	})
}

// Cancelling a context alone does not unblock net/http request-body reads or
// response writes. Expire the connection's I/O deadlines when cancellation
// happens, without extending the server's existing (possibly shorter) limits.
// The returned cleanup stops or joins the callback before ServeHTTP returns, so
// it cannot change deadlines on a connection already reused for another request.
func interruptRequestIO(ctx context.Context, w http.ResponseWriter) func() {
	done := make(chan struct{})
	stop := context.AfterFunc(ctx, func() {
		defer close(done)
		rc := http.NewResponseController(w)
		now := time.Now()
		// Plain net/http writers support these methods. Recorders or wrappers
		// without Unwrap may not; context cancellation still applies to them.
		_ = rc.SetReadDeadline(now)
		_ = rc.SetWriteDeadline(now)
	})
	return func() {
		if !stop() {
			<-done
		}
	}
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
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), requestSnapshotKey{}, cfg)))
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
		// A valid-looking node ID is not secret and identifies stale agents
		// that keep retrying with a revoked or renamed credential.
		device := "invalid"
		if config.ValidID(id) {
			device = id
		}
		h.auditDenied("agent", "invalid_credentials", "device", device)
		w.Header().Set("WWW-Authenticate", "Bearer realm=\"rmc-device\"")
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
	if !validInfo(*hello.Device) || (hello.Device.Component != "" && hello.Device.Component != "agent") || (hello.Device.WireVersion != 0 && hello.Device.WireVersion != hello.Version) {
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
	info.LastSeen = info.ConnectedAt
	info.DisconnectedAt = time.Time{}
	info.WireVersion = hello.Version
	info.Component = "agent"
	d := &deviceConn{id: id, hash: config.Hash(token), info: info, tools: advertisedTools(hello.ToolArgs), conn: conn, done: make(chan struct{}), pending: map[string]chan protocol.Message{}}
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
				info.DisconnectedAt = time.Now().UTC()
				h.known[id] = info
			}
		}
		h.mu.Unlock()
	}()
	welcomeCtx, cancel := context.WithTimeout(h.ctx, 10*time.Second)
	err = wsjson.Write(welcomeCtx, conn, protocol.Message{Type: "welcome", Version: protocol.WireVersion, Component: "hub", BuildVersion: protocol.Version})
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
			h.mu.Lock()
			if h.connections[d.id] == d {
				info := h.known[d.id]
				info.LastSeen = time.Now().UTC()
				h.known[d.id] = info
			}
			h.mu.Unlock()
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
	return h.clientIn(cfg, principal)
}

func (h *Hub) clientIn(cfg config.Hub, principal string) (config.Client, error) {
	if h.accessConfigured(cfg) {
		for _, p := range cfg.CloudflareAccess.Principals {
			if h.access.MatchesPrincipal(principal, h.accessResource, p.Email) {
				return config.Client{ID: principal, Devices: p.Devices}, nil
			}
		}
	}
	if cfg.OAuth != nil && h.oauth != nil && cfg.OAuth.Issuer == h.oauth.Issuer && strings.TrimSuffix(cfg.PublicURL, "/")+"/mcp" == h.oauth.Resource && slices.Equal(cfg.OAuth.Scopes, h.oauthScopes) {
		for _, p := range cfg.OAuth.Principals {
			if oauth.MatchesPrincipal(principal, h.oauth.Issuer, h.oauth.Resource, p.Subject) {
				return config.Client{ID: principal, Devices: p.Devices}, nil
			}
		}
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
	cfg, err := h.store.Snapshot()
	if err != nil {
		return nil, errors.New("configuration unavailable")
	}
	client, err := h.clientIn(cfg, principal)
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
	if deadline, ok := ctx.Value(requestDeadlineKey{}).(time.Time); ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithDeadline(ctx, deadline)
		defer cancel()
	}
	// One fresh read after admission queues serves both the client and the
	// device checks, so revocation still applies immediately before dispatch.
	cfg, err := h.store.Snapshot()
	if err != nil {
		return protocol.Result{}, errors.New("configuration unavailable")
	}
	client, err := h.clientIn(cfg, principal)
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
	if !cfg.DeviceValidForUser(d.id, d.hash, d.info.Username) {
		d.close()
		return protocol.Result{}, errors.New("device was revoked or its account binding changed; command was not dispatched")
	}
	if feature := h.unsupportedFeature(d, tool, args); feature != "" {
		return protocol.Result{}, fmt.Errorf("device %s runs RMC Agent %s, which does not support %s; upgrade that Agent. Command was not dispatched", device, agentVersion(d), feature)
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
	if err := callCtx.Err(); err != nil {
		requestID = "not_dispatched"
		return protocol.Result{}, fmt.Errorf("request canceled before dispatch; command was not dispatched: %w", err)
	}
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
