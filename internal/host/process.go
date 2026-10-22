package host

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/creack/pty"
	"github.com/soren-labs/hostmcp/internal/protocol"
)

type ExecInput struct {
	Cmd            string            `json:"cmd"`
	Cwd            string            `json:"cwd,omitempty"`
	Env            map[string]string `json:"env,omitempty"`
	Shell          string            `json:"shell,omitempty"`
	Login          bool              `json:"login,omitempty"`
	TTY            bool              `json:"tty,omitempty"`
	YieldTimeMS    *int              `json:"yield_time_ms,omitempty"`
	TimeoutSeconds *int              `json:"timeout_seconds,omitempty"`
	MaxOutputBytes *int              `json:"max_output_bytes,omitempty"`
	RequestID      string            `json:"request_id,omitempty"`
}

type StdinInput struct {
	SessionID      string `json:"session_id"`
	Chars          string `json:"chars,omitempty"`
	Signal         string `json:"signal,omitempty"`
	CloseStdin     bool   `json:"close_stdin,omitempty"`
	YieldTimeMS    *int   `json:"yield_time_ms,omitempty"`
	MaxOutputBytes *int   `json:"max_output_bytes,omitempty"`
	Rows           int    `json:"rows,omitempty"`
	Cols           int    `json:"cols,omitempty"`
}
type TerminateInput struct {
	SessionID string `json:"session_id"`
	Signal    string `json:"signal,omitempty"`
}

type session struct {
	mu                          sync.Mutex
	interact                    chan struct{}
	id, principal, command, cwd string
	cmd                         *exec.Cmd
	pid                         int
	tty                         bool
	input                       *os.File
	output                      *os.File
	inputClosed                 bool
	startedAt, finishedAt       time.Time
	exitCode                    *int
	signal                      string
	waitError                   string
	timedOut                    bool
	pending                     []byte
	dropped                     int64
	totalDropped                int64
	notify                      chan struct{}
	done                        chan struct{}
	readDone                    chan struct{}
	killOnce                    sync.Once
	killDone                    chan struct{}
}

func bounds(value *int, def, min, max int, name string) (int, error) {
	n := def
	if value != nil {
		n = *value
	}
	if n < min || n > max {
		return 0, fmt.Errorf("%s must be between %d and %d", name, min, max)
	}
	return n, nil
}
func outputOptions(yield, max *int) (time.Duration, int, error) {
	y, e := bounds(yield, 1000, 0, 30000, "yield_time_ms")
	if e != nil {
		return 0, 0, e
	}
	m, e := bounds(max, 65536, 1024, 262144, "max_output_bytes")
	return time.Duration(y) * time.Millisecond, m, e
}

func (r *Runtime) execCommand(ctx context.Context, principal string, in ExecInput) (protocol.Result, error) {
	yield, maxOut, err := outputOptions(in.YieldTimeMS, in.MaxOutputBytes)
	if err != nil {
		return protocol.Result{}, err
	}
	timeout, err := bounds(in.TimeoutSeconds, 3600, 0, 86400, "timeout_seconds")
	if err != nil {
		return protocol.Result{}, err
	}
	if strings.TrimSpace(in.Cmd) == "" || len(in.Cmd) > 128<<10 {
		return protocol.Result{}, errors.New("cmd must contain 1 to 131072 bytes")
	}
	if len(in.RequestID) > 128 {
		return protocol.Result{}, errors.New("request_id exceeds 128 bytes")
	}
	cwd := in.Cwd
	if cwd == "" {
		cwd = r.cwd
	}
	cwd, err = r.path(cwd, "")
	if err != nil {
		return protocol.Result{}, err
	}
	info, err := os.Stat(cwd)
	if err != nil {
		return protocol.Result{}, err
	}
	if !info.IsDir() {
		return protocol.Result{}, fmt.Errorf("working directory %s is not a directory", cwd)
	}
	shell := in.Shell
	if shell == "" {
		shell = r.defaultShell()
	}
	if strings.Contains(shell, "/") && !filepath.IsAbs(shell) {
		shell, err = r.path(shell, cwd)
		if err != nil {
			return protocol.Result{}, err
		}
	}
	if len(in.Env) > 128 {
		return protocol.Result{}, errors.New("at most 128 environment overrides are allowed")
	}
	for k, v := range in.Env {
		if k == "" || strings.ContainsAny(k, "=\x00") || strings.ContainsRune(v, 0) {
			return protocol.Result{}, errors.New("invalid environment variable")
		}
	}
	fingerprintBytes, _ := json.Marshal(struct {
		Cmd, Cwd, Shell string
		Env             map[string]string
		Login, TTY      bool
		Timeout         int
	}{in.Cmd, cwd, shell, in.Env, in.Login, in.TTY, timeout})
	fingerprint := fmt.Sprintf("%x", sha256.Sum256(fingerprintBytes))
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return protocol.Result{}, errors.New("device runtime is stopping")
	}
	r.pruneLocked(time.Now())
	if in.RequestID != "" {
		if rec, ok := r.requests[principal+"\x00"+in.RequestID]; ok {
			if rec.Fingerprint != fingerprint {
				r.mu.Unlock()
				return protocol.Result{}, errors.New("request_id was already used with different command arguments")
			}
			s := r.sessions[rec.SessionID]
			r.mu.Unlock()
			if s == nil {
				return protocol.Result{}, fmt.Errorf("request_id already executed as session %s; its output session was recycled and the command will not be replayed", rec.SessionID)
			}
			out, e := s.poll(ctx, yield, maxOut)
			if e != nil {
				return protocol.Result{}, e
			}
			out["replayed"] = true
			return protocol.JSONResult(out), nil
		}
		if len(r.requests) >= maxRequestRecords {
			r.mu.Unlock()
			return protocol.Result{}, errors.New("idempotency record capacity reached; retained keys expire 24 hours after their session is recycled")
		}
	}
	r.recycleCompletedLocked()
	if len(r.sessions) >= maxSessions {
		r.mu.Unlock()
		return protocol.Result{}, fmt.Errorf("session capacity reached (%d retained sessions); completed sessions expire after 30 minutes", maxSessions)
	}
	running, owned := 0, 0
	for _, s := range r.sessions {
		s.mu.Lock()
		live := s.finishedAt.IsZero()
		s.mu.Unlock()
		if live {
			running++
			if s.principal == principal {
				owned++
			}
		}
	}
	if running >= maxRunning || owned >= maxRunningPerPrincipal {
		r.mu.Unlock()
		return protocol.Result{}, errors.New("running session limit reached (16 per device, 8 per principal)")
	}
	idBytes := make([]byte, 16)
	if _, err = rand.Read(idBytes); err != nil {
		r.mu.Unlock()
		return protocol.Result{}, err
	}
	flag := "-c"
	if in.Login {
		flag = "-lc"
	}
	cmd := exec.Command(shell, flag, in.Cmd)
	cmd.Dir = cwd
	env := map[string]string{}
	for _, entry := range os.Environ() {
		k, v, ok := strings.Cut(entry, "=")
		if ok {
			env[k] = v
		}
	}
	env["PWD"] = cwd
	for k, v := range in.Env {
		env[k] = v
	}
	if in.TTY && env["TERM"] == "" {
		env["TERM"] = "xterm-256color"
	}
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	sort.Strings(cmd.Env)
	s := &session{id: hex.EncodeToString(idBytes), principal: principal, command: in.Cmd, cwd: cwd, cmd: cmd, tty: in.TTY, startedAt: time.Now().UTC(), interact: make(chan struct{}, 1), notify: make(chan struct{}, 1), done: make(chan struct{}), readDone: make(chan struct{}), killDone: make(chan struct{})}
	if err := ctx.Err(); err != nil {
		r.mu.Unlock()
		return protocol.Result{}, err
	}
	if in.TTY {
		f, startErr := pty.StartWithSize(cmd, &pty.Winsize{Rows: 40, Cols: 120})
		if startErr != nil {
			r.mu.Unlock()
			return protocol.Result{}, fmt.Errorf("start PTY: %w", startErr)
		}
		s.input = f
		s.output = f
	} else {
		inRead, inWrite, e := os.Pipe()
		if e != nil {
			r.mu.Unlock()
			return protocol.Result{}, e
		}
		outRead, outWrite, e := os.Pipe()
		if e != nil {
			inRead.Close()
			inWrite.Close()
			r.mu.Unlock()
			return protocol.Result{}, e
		}
		cmd.Stdin = inRead
		cmd.Stdout = outWrite
		cmd.Stderr = outWrite
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		err = cmd.Start()
		inRead.Close()
		outWrite.Close()
		if err != nil {
			inWrite.Close()
			outRead.Close()
			r.mu.Unlock()
			return protocol.Result{}, fmt.Errorf("start shell: %w", err)
		}
		s.input = inWrite
		s.output = outRead
	}
	s.pid = cmd.Process.Pid
	r.sessions[s.id] = s
	if in.RequestID != "" {
		r.requests[principal+"\x00"+in.RequestID] = requestRecord{SessionID: s.id, Fingerprint: fingerprint, ExpiresAt: time.Now().Add(requestRetention)}
	}
	r.mu.Unlock()
	go s.readOutput()
	go s.wait()
	if timeout > 0 {
		go func() {
			t := time.NewTimer(time.Duration(timeout) * time.Second)
			defer t.Stop()
			select {
			case <-t.C:
				s.stop("TERM", true)
			case <-s.done:
			}
		}()
	}
	out, err := s.poll(ctx, yield, maxOut)
	if err != nil {
		return protocol.Result{}, err
	}
	return protocol.JSONResult(out), nil
}

func (s *session) wake() {
	select {
	case s.notify <- struct{}{}:
	default:
	}
}
func (s *session) appendOutput(p []byte) {
	s.mu.Lock()
	if len(p) >= maxBufferBytes {
		s.dropped += int64(len(s.pending) + len(p) - maxBufferBytes)
		s.pending = append(s.pending[:0], p[len(p)-maxBufferBytes:]...)
	} else {
		overflow := len(s.pending) + len(p) - maxBufferBytes
		if overflow > 0 {
			s.dropped += int64(overflow)
			copy(s.pending, s.pending[overflow:])
			s.pending = s.pending[:len(s.pending)-overflow]
		}
		s.pending = append(s.pending, p...)
	}
	// Never leave a cut UTF-8 continuation byte at the start after dropping bytes.
	if s.dropped > 0 {
		n := 0
		for n < len(s.pending) && s.pending[n]&0xc0 == 0x80 {
			n++
		}
		if n > 0 {
			s.dropped += int64(n)
			s.pending = s.pending[n:]
		}
	}
	s.mu.Unlock()
	s.wake()
}
func (s *session) readOutput() {
	defer close(s.readDone)
	buf := make([]byte, 32768)
	for {
		n, err := s.output.Read(buf)
		if n > 0 {
			s.appendOutput(buf[:n])
		}
		if err != nil {
			return
		}
	}
}

func (s *session) stop(signal string, timedOut bool) {
	s.mu.Lock()
	if !s.finishedAt.IsZero() {
		s.mu.Unlock()
		return
	}
	if timedOut {
		s.timedOut = true
	}
	s.mu.Unlock()
	if signal == "KILL" {
		_ = syscall.Kill(-s.pid, syscall.SIGKILL)
	}
	s.killOnce.Do(func() {
		if signal == "KILL" {
			close(s.killDone)
			return
		}
		err := syscall.Kill(-s.pid, syscall.SIGTERM)
		if errors.Is(err, syscall.ESRCH) {
			close(s.killDone)
			return
		}
		go func() {
			defer close(s.killDone)
			deadline := time.Now().Add(500 * time.Millisecond)
			for time.Now().Before(deadline) {
				if errors.Is(syscall.Kill(-s.pid, 0), syscall.ESRCH) {
					return
				}
				time.Sleep(20 * time.Millisecond)
			}
			_ = syscall.Kill(-s.pid, syscall.SIGKILL)
		}()
	})
}

func (s *session) wait() {
	err := s.cmd.Wait()
	// A shell that exits must not leave its ordinary process-group children owned
	// by nobody. Deliberately setsid-detached daemons require their own service lifecycle.
	s.stop("TERM", false)
	<-s.killDone
	if !s.tty {
		s.mu.Lock()
		if !s.inputClosed {
			s.inputClosed = true
			_ = s.input.Close()
		}
		s.mu.Unlock()
	}
	select {
	case <-s.readDone:
	case <-time.After(150 * time.Millisecond):
		_ = s.output.Close()
		<-s.readDone
	}
	_ = s.output.Close()
	s.mu.Lock()
	code := s.cmd.ProcessState.ExitCode()
	s.exitCode = &code
	if status, ok := s.cmd.ProcessState.Sys().(syscall.WaitStatus); ok && status.Signaled() {
		s.signal = status.Signal().String()
	}
	if err != nil {
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) {
			s.waitError = err.Error()
		}
	}
	s.finishedAt = time.Now().UTC()
	s.mu.Unlock()
	close(s.done)
	s.wake()
}

func (s *session) acquire(ctx context.Context) error {
	select {
	case s.interact <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (s *session) release() { <-s.interact }

func completeUTF8Prefix(p []byte) int {
	n := len(p)
	if n == 0 {
		return 0
	}
	start := n - 1
	for start > 0 && start >= n-4 && p[start]&0xc0 == 0x80 {
		start--
	}
	if !utf8.FullRune(p[start:]) {
		return start
	}
	return n
}

func (s *session) snapshotLocked() map[string]any {
	// Command metadata must stay small when all 64 retained sessions are listed.
	// Keep the execution command and its idempotency fingerprint unchanged.
	const maxCommandPreviewBytes = 4 << 10
	command := s.command
	commandTruncated := len(command) > maxCommandPreviewBytes
	if commandTruncated {
		prefix := []byte(command[:maxCommandPreviewBytes])
		command = string(prefix[:completeUTF8Prefix(prefix)])
	}
	o := map[string]any{"session_id": s.id, "pid": s.pid, "running": s.finishedAt.IsZero(), "tty": s.tty, "started_at": s.startedAt, "cwd": s.cwd, "command": command, "command_truncated": commandTruncated, "command_bytes": len(s.command), "buffered_bytes": len(s.pending), "dropped_bytes_total": s.totalDropped + s.dropped, "timed_out": s.timedOut}
	if s.exitCode != nil {
		o["exit_code"] = *s.exitCode
	}
	if s.signal != "" {
		o["signal"] = s.signal
	}
	if s.waitError != "" {
		o["wait_error"] = s.waitError
	}
	if !s.finishedAt.IsZero() {
		o["finished_at"] = s.finishedAt
	}
	return o
}
func (s *session) drainLocked(max int) map[string]any {
	n := len(s.pending)
	if n > max {
		n = max
	}
	if n < len(s.pending) || s.finishedAt.IsZero() {
		n = completeUTF8Prefix(s.pending[:n])
	}
	output := strings.ToValidUTF8(string(s.pending[:n]), "\uFFFD")
	s.pending = s.pending[n:]
	out := s.snapshotLocked()
	out["output"] = output
	out["output_bytes"] = n
	out["dropped_bytes"] = s.dropped
	out["truncated"] = s.dropped > 0
	out["has_more_output"] = len(s.pending) > 0
	s.totalDropped += s.dropped
	s.dropped = 0
	return out
}
func (s *session) poll(ctx context.Context, yield time.Duration, max int) (map[string]any, error) {
	if err := s.acquire(ctx); err != nil {
		return nil, fmt.Errorf("session %s remains available: %w", s.id, err)
	}
	defer s.release()
	return s.pollLocked(ctx, yield, max)
}
func (s *session) pollLocked(ctx context.Context, yield time.Duration, max int) (map[string]any, error) {
	t := time.NewTimer(yield)
	defer t.Stop()
	for {
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("session %s remains available; call cancellation does not stop the command: %w", s.id, err)
		}
		s.mu.Lock()
		ready := len(s.pending) > 0 && completeUTF8Prefix(s.pending) > 0 || !s.finishedAt.IsZero()
		if ready || yield == 0 {
			out := s.drainLocked(max)
			s.mu.Unlock()
			return out, nil
		}
		s.mu.Unlock()
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("session %s remains available; call cancellation does not stop the command: %w", s.id, ctx.Err())
		case <-s.notify:
		case <-s.done:
		case <-t.C:
			s.mu.Lock()
			out := s.drainLocked(max)
			s.mu.Unlock()
			return out, nil
		}
	}
}

func (r *Runtime) owned(principal, id string) (*session, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s, ok := r.sessions[id]
	if !ok || s.principal != principal {
		return nil, errors.New("unknown session for this principal")
	}
	return s, nil
}

func (r *Runtime) writeStdin(ctx context.Context, principal string, in StdinInput) (protocol.Result, error) {
	yield, max, err := outputOptions(in.YieldTimeMS, in.MaxOutputBytes)
	if err != nil {
		return protocol.Result{}, err
	}
	if len(in.Chars) > 65536 {
		return protocol.Result{}, errors.New("stdin input exceeds 64 KiB")
	}
	if in.Signal != "" && in.Signal != "INT" && in.Signal != "TERM" && in.Signal != "KILL" {
		return protocol.Result{}, errors.New("signal must be INT, TERM or KILL")
	}
	if (in.Rows == 0) != (in.Cols == 0) || in.Rows < 0 || in.Rows > 500 || in.Cols < 0 || in.Cols > 500 {
		return protocol.Result{}, errors.New("rows and cols must both be between 1 and 500")
	}
	s, err := r.owned(principal, in.SessionID)
	if err != nil {
		return protocol.Result{}, err
	}
	if err = s.acquire(ctx); err != nil {
		return protocol.Result{}, err
	}
	defer s.release()
	s.mu.Lock()
	finished := !s.finishedAt.IsZero()
	inputClosed := s.inputClosed
	s.mu.Unlock()
	if (in.Chars != "" || in.Signal != "" || in.CloseStdin || in.Rows > 0) && finished {
		return protocol.Result{}, errors.New("session has already exited; poll without input to read its remaining output")
	}
	if in.Rows > 0 {
		if !s.tty {
			return protocol.Result{}, errors.New("cannot resize a piped session")
		}
		if err = pty.Setsize(s.output, &pty.Winsize{Rows: uint16(in.Rows), Cols: uint16(in.Cols)}); err != nil {
			return protocol.Result{}, err
		}
	}
	if in.Signal != "" {
		switch in.Signal {
		case "INT":
			err = syscall.Kill(-s.pid, syscall.SIGINT)
		case "TERM", "KILL":
			s.stop(in.Signal, false)
		}
		if err != nil && !errors.Is(err, syscall.ESRCH) {
			return protocol.Result{}, err
		}
	}
	if in.Chars != "" || in.CloseStdin {
		if inputClosed {
			return protocol.Result{}, errors.New("stdin is closed")
		}
		chars := in.Chars
		if in.CloseStdin && s.tty {
			chars += "\x04"
		}
		if chars != "" {
			if err = s.writeInput(ctx, []byte(chars)); err != nil {
				return protocol.Result{}, err
			}
		}
		if in.CloseStdin && !s.tty {
			s.mu.Lock()
			s.inputClosed = true
			err = s.input.Close()
			s.mu.Unlock()
			if err != nil && !errors.Is(err, os.ErrClosed) {
				return protocol.Result{}, err
			}
		}
	}
	out, err := s.pollLocked(ctx, yield, max)
	if err != nil {
		return protocol.Result{}, err
	}
	return protocol.JSONResult(out), nil
}

func (s *session) writeInput(ctx context.Context, b []byte) error {
	// A non-reading child must not pin a tool call indefinitely. Close its input
	// (or PTY) and stop its owned process group if a write cannot complete.
	finished := make(chan error, 1)
	go func() { _, err := s.input.Write(b); finished <- err }()
	t := time.NewTimer(5 * time.Second)
	defer t.Stop()
	select {
	case err := <-finished:
		return err
	case <-ctx.Done():
		_ = s.input.Close()
		s.stop("TERM", false)
		return fmt.Errorf("stdin write cancelled; session %s was stopped to release blocked input: %w", s.id, ctx.Err())
	case <-t.C:
		_ = s.input.Close()
		s.stop("TERM", false)
		return errors.New("stdin did not accept input within 5 seconds; session stopped")
	}
}

func (r *Runtime) listProcesses(principal string) protocol.Result {
	r.mu.Lock()
	ss := make([]*session, 0, len(r.sessions))
	for _, s := range r.sessions {
		if s.principal == principal {
			ss = append(ss, s)
		}
	}
	r.mu.Unlock()
	sort.Slice(ss, func(i, j int) bool { return ss[i].startedAt.Before(ss[j].startedAt) })
	items := make([]map[string]any, 0, len(ss))
	for _, s := range ss {
		s.mu.Lock()
		items = append(items, s.snapshotLocked())
		s.mu.Unlock()
	}
	return protocol.JSONResult(map[string]any{"sessions": items, "retention_seconds": int(retention.Seconds())})
}
func (r *Runtime) terminate(ctx context.Context, principal string, in TerminateInput) (protocol.Result, error) {
	if in.Signal == "" {
		in.Signal = "TERM"
	}
	if in.Signal != "TERM" && in.Signal != "KILL" {
		return protocol.Result{}, errors.New("signal must be TERM or KILL")
	}
	s, err := r.owned(principal, in.SessionID)
	if err != nil {
		return protocol.Result{}, err
	}
	s.stop(in.Signal, false)
	select {
	case <-s.done:
	case <-ctx.Done():
		return protocol.Result{}, fmt.Errorf("stop requested for session %s: %w", s.id, ctx.Err())
	case <-time.After(2 * time.Second):
	}
	out, err := s.poll(ctx, 0, 65536)
	if err != nil {
		return protocol.Result{}, err
	}
	return protocol.JSONResult(out), nil
}
