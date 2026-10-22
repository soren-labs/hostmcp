// Package host executes tools with the ordinary privileges of its OS account.
// The process/session semantics follow the retained Chat On Steroids / Codex
// references. This implementation is independent Go code; reference/ is immutable.
package host

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/soren-labs/hostmcp/internal/protocol"
)

const (
	maxSessions            = 64
	maxRunning             = 16
	maxRunningPerPrincipal = 8
	maxBufferBytes         = 1 << 20
	maxFileBytes           = 1 << 20
	retention              = 30 * time.Minute
	requestRetention       = 24 * time.Hour
	maxRequestRecords      = 4096
)

type Runtime struct {
	mu       sync.Mutex
	sessions map[string]*session
	requests map[string]requestRecord
	closed   bool
	cwd      string
	user     *user.User
	hostname string
	filesMu  sync.Mutex
	calls    chan struct{}
	stop     chan struct{}
}

type requestRecord struct {
	SessionID, Fingerprint string
	ExpiresAt              time.Time
}

func New() (*Runtime, error) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		return nil, errors.New("HostMCP supports Linux and macOS")
	}
	cwd, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	u, err := user.Current()
	if err != nil {
		return nil, fmt.Errorf("current OS account: %w", err)
	}
	hostname, err := os.Hostname()
	if err != nil {
		return nil, err
	}
	r := &Runtime{sessions: map[string]*session{}, requests: map[string]requestRecord{}, cwd: cwd, user: u, hostname: hostname, calls: make(chan struct{}, 32), stop: make(chan struct{})}
	go r.reap()
	return r, nil
}

func (r *Runtime) Info(deviceID string) protocol.Device {
	return protocol.Device{ID: deviceID, Hostname: r.hostname, OS: runtime.GOOS, Arch: runtime.GOARCH, Username: r.user.Username, UID: r.user.Uid, Home: r.user.HomeDir, Version: protocol.Version, Online: true}
}

func (r *Runtime) Close() error {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil
	}
	r.closed = true
	close(r.stop)
	ss := make([]*session, 0, len(r.sessions))
	for _, s := range r.sessions {
		ss = append(ss, s)
	}
	r.mu.Unlock()
	for _, s := range ss {
		s.stop("TERM", false)
	}
	for _, s := range ss {
		select {
		case <-s.done:
		case <-time.After(3 * time.Second):
			s.stop("KILL", false)
		}
	}
	return nil
}

func (r *Runtime) reap() {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-r.stop:
			return
		case now := <-t.C:
			r.mu.Lock()
			r.pruneLocked(now)
			r.mu.Unlock()
		}
	}
}

func (r *Runtime) pruneLocked(now time.Time) {
	for id, s := range r.sessions {
		s.mu.Lock()
		expired := !s.finishedAt.IsZero() && now.Sub(s.finishedAt) > retention
		s.mu.Unlock()
		if expired {
			r.forgetLocked(id, now)
		}
	}
	for id, rec := range r.requests {
		if _, ok := r.sessions[rec.SessionID]; !ok && now.After(rec.ExpiresAt) {
			delete(r.requests, id)
		}
	}
}

// Keep request tombstones after recycling sessions: an old retry must not silently
// execute the command again simply because its output has already been consumed.
func (r *Runtime) forgetLocked(id string, now time.Time) {
	delete(r.sessions, id)
	for key, rec := range r.requests {
		if rec.SessionID == id {
			rec.ExpiresAt = now.Add(requestRetention)
			r.requests[key] = rec
		}
	}
}

func (r *Runtime) recycleCompletedLocked() {
	if len(r.sessions) < maxSessions {
		return
	}
	var oldest *session
	for _, s := range r.sessions {
		s.mu.Lock()
		available := !s.finishedAt.IsZero() && len(s.pending) == 0
		s.mu.Unlock()
		if available && (oldest == nil || s.startedAt.Before(oldest.startedAt)) {
			oldest = s
		}
	}
	if oldest != nil {
		r.forgetLocked(oldest.id, time.Now())
	}
}

func decodeArgs(raw json.RawMessage, out any) error {
	if len(raw) == 0 {
		raw = []byte("{}")
	}
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return errors.New("tool arguments must be a JSON object")
	}
	if len(raw) > 3<<20 {
		return errors.New("tool arguments exceed 3 MiB")
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(out); err != nil {
		return fmt.Errorf("invalid arguments: %w", err)
	}
	var extra any
	if err := d.Decode(&extra); err != io.EOF {
		return errors.New("expected one JSON argument object")
	}
	return nil
}

func (r *Runtime) Call(ctx context.Context, principal, name string, raw json.RawMessage) (protocol.Result, error) {
	if principal == "" || len(principal) > 256 {
		return protocol.Result{}, errors.New("a nonempty authenticated principal is required")
	}
	if err := ctx.Err(); err != nil {
		return protocol.Result{}, err
	}
	select {
	case r.calls <- struct{}{}:
		defer func() { <-r.calls }()
	default:
		return protocol.Result{}, errors.New("device is busy; maximum concurrent tool calls reached")
	}
	r.mu.Lock()
	closed := r.closed
	r.mu.Unlock()
	if closed {
		return protocol.Result{}, errors.New("device runtime is stopping")
	}
	switch name {
	case "device_info":
		var in struct{}
		if err := decodeArgs(raw, &in); err != nil {
			return protocol.Result{}, err
		}
		groups, _ := os.Getgroups()
		return protocol.JSONResult(map[string]any{"device": r.Info(""), "uid": os.Getuid(), "euid": os.Geteuid(), "gid": os.Getgid(), "groups": groups, "cwd": r.cwd, "shell": r.defaultShell(), "authority": "native OS account; no additional filesystem roots; OS permissions and macOS privacy controls still apply"}), nil
	case "exec_command":
		var in ExecInput
		if err := decodeArgs(raw, &in); err != nil {
			return protocol.Result{}, err
		}
		return r.execCommand(ctx, principal, in)
	case "write_stdin":
		var in StdinInput
		if err := decodeArgs(raw, &in); err != nil {
			return protocol.Result{}, err
		}
		return r.writeStdin(ctx, principal, in)
	case "list_processes":
		var in struct{}
		if err := decodeArgs(raw, &in); err != nil {
			return protocol.Result{}, err
		}
		return r.listProcesses(principal), nil
	case "terminate_process":
		var in TerminateInput
		if err := decodeArgs(raw, &in); err != nil {
			return protocol.Result{}, err
		}
		return r.terminate(ctx, principal, in)
	case "read":
		var in ReadInput
		if err := decodeArgs(raw, &in); err != nil {
			return protocol.Result{}, err
		}
		return r.read(in)
	case "write_file":
		var in WriteInput
		if err := decodeArgs(raw, &in); err != nil {
			return protocol.Result{}, err
		}
		r.filesMu.Lock()
		defer r.filesMu.Unlock()
		if err := ctx.Err(); err != nil {
			return protocol.Result{}, err
		}
		return r.writeFile(in)
	case "apply_patch":
		var in PatchInput
		if err := decodeArgs(raw, &in); err != nil {
			return protocol.Result{}, err
		}
		r.filesMu.Lock()
		defer r.filesMu.Unlock()
		if err := ctx.Err(); err != nil {
			return protocol.Result{}, err
		}
		return r.applyPatch(in)
	case "view_image":
		var in ImageInput
		if err := decodeArgs(raw, &in); err != nil {
			return protocol.Result{}, err
		}
		return r.viewImage(in)
	default:
		return protocol.Result{}, fmt.Errorf("unknown device tool %q", name)
	}
}

func (r *Runtime) defaultShell() string {
	if s := os.Getenv("SHELL"); s != "" {
		return s
	}
	return "/bin/sh"
}

func (r *Runtime) path(value, cwd string) (string, error) {
	if value == "" {
		return "", errors.New("path must not be empty")
	}
	if strings.IndexByte(value, 0) >= 0 || strings.IndexByte(cwd, 0) >= 0 {
		return "", errors.New("path contains NUL")
	}
	expand := func(p string) string {
		if p == "~" {
			return r.user.HomeDir
		}
		if strings.HasPrefix(p, "~/") {
			return filepath.Join(r.user.HomeDir, p[2:])
		}
		return p
	}
	value = expand(value)
	cwd = expand(cwd)
	if cwd == "" {
		cwd = r.cwd
	}
	if !filepath.IsAbs(cwd) {
		cwd = filepath.Join(r.cwd, cwd)
	}
	if !filepath.IsAbs(value) {
		value = filepath.Join(cwd, value)
	}
	return filepath.Clean(value), nil
}

func obj(props map[string]any, required ...string) map[string]any {
	m := map[string]any{"type": "object", "properties": props, "additionalProperties": false}
	if len(required) > 0 {
		m["required"] = required
	}
	return m
}
func str(description string) map[string]any {
	return map[string]any{"type": "string", "description": description}
}
func integer(description string, min, max int) map[string]any {
	return map[string]any{"type": "integer", "description": description, "minimum": min, "maximum": max}
}
func boolean(description string) map[string]any {
	return map[string]any{"type": "boolean", "description": description}
}

func Tools() []protocol.Tool {
	return []protocol.Tool{
		{Name: "device_info", Description: "Report the native OS account, device, working directory and effective UID. No secrets or environment variables are dumped.", ReadOnly: true, InputSchema: obj(map[string]any{})},
		{Name: "exec_command", Description: "Run a shell command as the device's OS account. Returns a durable-in-agent-memory session_id, bounded combined stdout/stderr and actual status. Poll with write_stdin. Network disconnect does not undo execution. Existing OS permissions and sudo policy apply. Sessions survive network reconnects, but not device-agent restarts.", InputSchema: obj(map[string]any{
			"cmd": str("Shell command to execute"), "cwd": str("Native working directory; defaults to agent startup directory"), "env": map[string]any{"type": "object", "additionalProperties": map[string]any{"type": "string"}, "description": "Overrides merged over the agent's inherited environment"}, "shell": str("Shell executable; default SHELL or /bin/sh"), "login": boolean("Use a login shell; defaults to false so explicit cwd/env are preserved"), "tty": boolean("Allocate an interactive POSIX PTY"), "yield_time_ms": integer("Wait for initial output, up to this deadline; default 1000", 0, 30000), "timeout_seconds": integer("Kill the command's process group after this duration; default 3600; zero disables the execution deadline", 0, 86400), "max_output_bytes": integer("Output bytes returned per call; default 65536; unread bytes remain buffered", 1024, 262144), "request_id": str("Optional idempotency key, max 128 characters, scoped to this principal. Tombstones remain for 24 hours after session recycling, with a 4096-key limit. Not durable across agent restarts")}, "cmd")},
		{Name: "write_stdin", Description: "Read new output or send input to an owned session. Reads drain only returned bytes. Completed sessions expire after 30 minutes; completed sessions with fully consumed output may be recycled sooner under capacity pressure. Use signal for INT/TERM/KILL; close_stdin sends EOF.", InputSchema: obj(map[string]any{"session_id": str("Session returned by exec_command"), "chars": str("Input text, at most 64 KiB"), "signal": map[string]any{"type": "string", "enum": []string{"INT", "TERM", "KILL"}}, "close_stdin": boolean("Close piped stdin, or send Ctrl-D to a PTY"), "yield_time_ms": integer("Wait for new output; default 1000", 0, 30000), "max_output_bytes": integer("Maximum bytes to drain; default 65536", 1024, 262144), "rows": integer("Resize PTY rows (provide with cols)", 1, 500), "cols": integer("Resize PTY columns (provide with rows)", 1, 500)}, "session_id")},
		{Name: "list_processes", Description: "List only the calling principal's managed command sessions and real running/exit state. This is session ownership, not isolation between principals sharing the same OS account.", ReadOnly: true, InputSchema: obj(map[string]any{})},
		{Name: "terminate_process", Description: "Stop an owned command's POSIX process group with TERM followed by KILL after a grace period, or KILL immediately. Daemons which deliberately detach into another session are outside this process group.", InputSchema: obj(map[string]any{"session_id": str("Owned session identifier"), "signal": map[string]any{"type": "string", "enum": []string{"TERM", "KILL"}}}, "session_id")},
		{Name: "read", Description: "Read real OS paths as bounded text, base64 bytes, directory entries or metadata. Follows normal symlink/permission semantics. Text line ranges scan at most 16 MiB; results report truncation. No approved-directory list.", ReadOnly: true, InputSchema: obj(map[string]any{"paths": map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "minItems": 1, "maxItems": 16}, "cwd": str("Base for relative paths"), "start_line": integer("First text line, inclusive; default 1", 1, 10000000), "end_line": integer("Last text line, inclusive; zero means no line limit", 0, 10000000), "max_bytes": integer("Total returned content budget, default 65536; maximum 1 MiB", 1024, 1048576), "encoding": map[string]any{"type": "string", "enum": []string{"utf8", "base64"}}}, "paths")},
		{Name: "write_file", Description: "Write UTF-8 text or base64-decoded bytes to a native file. Existing files require overwrite=true. Writes follow symlinks and OS permissions; failures report bytes actually written. Use apply_patch for contextual changes.", InputSchema: obj(map[string]any{"path": str("Native destination path"), "content": str("UTF-8 text or base64-encoded bytes, up to 1 MiB decoded"), "cwd": str("Base for relative paths"), "encoding": map[string]any{"type": "string", "enum": []string{"utf8", "base64"}}, "overwrite": boolean("Explicitly allow replacing the contents of an existing file"), "create_parents": boolean("Create missing parent directories"), "mode": str("Octal permissions for a new file, e.g. 0600; default 0644 before umask")}, "path", "content")},
		{Name: "apply_patch", Description: "Apply a *** Begin Patch / *** End Patch textual patch with Add File, Update File, Delete File, optional Move to and @@ hunks. Context must match exactly and unambiguously. Validate all edits before writing; a filesystem failure during commit reports completed paths. Individual writes are atomic, the entire multi-file patch is not a filesystem transaction.", InputSchema: obj(map[string]any{"patch": str("Patch text, up to 1 MiB"), "cwd": str("Base for relative patch paths")}, "patch")},
		{Name: "view_image", Description: "Return a PNG/JPEG/GIF image as MCP image content. Maximum 2 MiB file and 40 million pixels; no resizing. Uses normal OS file permissions.", ReadOnly: true, InputSchema: obj(map[string]any{"path": str("Native image path"), "cwd": str("Base for relative paths")}, "path")},
	}
}
