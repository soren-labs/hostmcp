package host

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/soren-labs/hostmcp/internal/protocol"
)

func newTestRuntime(t *testing.T) *Runtime {
	t.Helper()
	r, err := New()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := r.Close(); err != nil {
			t.Error(err)
		}
	})
	return r
}
func raw(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
func call(t *testing.T, r *Runtime, principal, name string, args any) map[string]any {
	t.Helper()
	res, err := r.Call(context.Background(), principal, name, raw(t, args))
	if err != nil {
		t.Fatal(err)
	}
	if res.Error != "" {
		t.Fatal(res.Error)
	}
	var out map[string]any
	if err = json.Unmarshal(res.Data, &out); err != nil {
		t.Fatal(err)
	}
	return out
}
func untilDone(t *testing.T, r *Runtime, principal string, first map[string]any) (map[string]any, string) {
	t.Helper()
	out := first
	text, _ := out["output"].(string)
	deadline := time.Now().Add(12 * time.Second)
	for out["running"] == true || out["has_more_output"] == true {
		if time.Now().After(deadline) {
			t.Fatalf("session did not exit: %#v", out)
		}
		out = call(t, r, principal, "write_stdin", map[string]any{"session_id": out["session_id"], "yield_time_ms": 100})
		chunk, _ := out["output"].(string)
		text += chunk
	}
	return out, text
}
func shellQuote(value string) string { return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'" }

func TestNativeIdentityCwdAndEnvironment(t *testing.T) {
	r := newTestRuntime(t)
	dir := filepath.Join(t.TempDir(), "outside repo with spaces")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	in := map[string]any{"cmd": "printf '%s|%s|%s' \"$PWD\" \"$HOSTMCP_TEST_VALUE\" \"$(id -u)\"", "cwd": dir, "env": map[string]string{"HOSTMCP_TEST_VALUE": "你好 world"}, "yield_time_ms": 1000}
	out, text := untilDone(t, r, "alice", call(t, r, "alice", "exec_command", in))
	if text != dir+"|你好 world|"+strconv.Itoa(os.Getuid()) {
		t.Fatalf("unexpected native execution: %q", text)
	}
	if out["exit_code"] != float64(0) {
		t.Fatalf("unexpected exit: %#v", out)
	}
	info := call(t, r, "alice", "device_info", map[string]any{})
	if info["uid"] != float64(os.Getuid()) {
		t.Fatal(info)
	}
}

func TestSessionOwnershipAndTermination(t *testing.T) {
	r := newTestRuntime(t)
	out := call(t, r, "alice", "exec_command", map[string]any{"cmd": "sleep 30", "yield_time_ms": 0})
	id := out["session_id"]
	_, err := r.Call(context.Background(), "bob", "write_stdin", raw(t, map[string]any{"session_id": id, "chars": "oops"}))
	if err == nil {
		t.Fatal("another principal obtained session access")
	}
	listed := call(t, r, "bob", "list_processes", map[string]any{})
	if len(listed["sessions"].([]any)) != 0 {
		t.Fatal("another principal can see sessions")
	}
	out = call(t, r, "alice", "terminate_process", map[string]any{"session_id": id})
	if out["running"] != false {
		t.Fatalf("process still running: %#v", out)
	}
	if out["signal"] == nil {
		t.Fatalf("missing real termination signal: %#v", out)
	}
}

func TestPTYInputAndResize(t *testing.T) {
	r := newTestRuntime(t)
	first := call(t, r, "alice", "exec_command", map[string]any{"cmd": "printf 'READY\\n'; read -r line; printf 'GOT:%s\\n' \"$line\"", "tty": true, "yield_time_ms": 1000})
	if !strings.Contains(first["output"].(string), "READY") {
		t.Fatalf("missing ready: %#v", first)
	}
	second := call(t, r, "alice", "write_stdin", map[string]any{"session_id": first["session_id"], "chars": "你好终端\n", "rows": 30, "cols": 100, "yield_time_ms": 1000})
	out, text := untilDone(t, r, "alice", second)
	if !strings.Contains(text, "GOT:你好终端") {
		t.Fatalf("PTY output: %q", text)
	}
	if out["exit_code"] != float64(0) {
		t.Fatal(out)
	}
}

func TestExecutionTimeoutKillsProcessGroup(t *testing.T) {
	r := newTestRuntime(t)
	out := call(t, r, "alice", "exec_command", map[string]any{"cmd": "trap '' TERM; sleep 30 & printf '%s\\n' \"$!\"; wait", "timeout_seconds": 1, "yield_time_ms": 1000})
	last, text := untilDone(t, r, "alice", out)
	if last["timed_out"] != true || last["running"] != false {
		t.Fatalf("deadline status incorrect: %#v", last)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(text))
	if err != nil {
		t.Fatalf("child pid: %q: %v", text, err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if err = syscall.Kill(pid, 0); err == syscall.ESRCH {
			return
		}
		stat, _ := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
		if strings.Contains(string(stat), ") Z ") {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("child %d remains alive after process-group deadline", pid)
}

func TestCallCancellationKeepsStartedCommandDiscoverable(t *testing.T) {
	r := newTestRuntime(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	_, err := r.Call(ctx, "alice", "exec_command", raw(t, map[string]any{"cmd": "sleep 1; printf completed", "yield_time_ms": 2000}))
	if err == nil || !strings.Contains(err.Error(), "session ") {
		t.Fatalf("expected discoverable cancelled wait: %v", err)
	}
	list := call(t, r, "alice", "list_processes", map[string]any{})
	items := list["sessions"].([]any)
	if len(items) != 1 {
		t.Fatalf("started command was lost: %#v", list)
	}
	id := items[0].(map[string]any)["session_id"]
	last, text := untilDone(t, r, "alice", call(t, r, "alice", "write_stdin", map[string]any{"session_id": id, "yield_time_ms": 2000}))
	if text != "completed" || last["exit_code"] != float64(0) {
		t.Fatalf("command did not survive call cancellation: %q %#v", text, last)
	}
}

func TestIdempotencyDoesNotRepeatSideEffects(t *testing.T) {
	r := newTestRuntime(t)
	path := filepath.Join(t.TempDir(), "count")
	in := map[string]any{"cmd": "printf 'once\\n' >> " + shellQuote(path), "request_id": "stable-retry", "yield_time_ms": 0}
	one := call(t, r, "alice", "exec_command", in)
	two := call(t, r, "alice", "exec_command", in)
	if one["session_id"] != two["session_id"] || two["replayed"] != true {
		t.Fatalf("idempotency failed: %#v %#v", one, two)
	}
	untilDone(t, r, "alice", two)
	b, err := os.ReadFile(path)
	if err != nil || string(b) != "once\n" {
		t.Fatalf("side effect repeated: %q %v", b, err)
	}
	in["cmd"] = "printf wrong"
	if _, err = r.Call(context.Background(), "alice", "exec_command", raw(t, in)); err == nil {
		t.Fatal("conflicting request_id accepted")
	}
}

func TestOutputBoundAndIncrementalUTF8(t *testing.T) {
	r := newTestRuntime(t)
	out := call(t, r, "alice", "exec_command", map[string]any{"cmd": "head -c 2097152 /dev/zero | tr '\\000' x; printf '终结\\n'", "yield_time_ms": 0})
	s, err := r.owned("alice", out["session_id"].(string))
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-s.done:
	case <-time.After(8 * time.Second):
		t.Fatal("output fixture timeout")
	}
	var all strings.Builder
	var dropped int64
	for {
		item := call(t, r, "alice", "write_stdin", map[string]any{"session_id": s.id, "yield_time_ms": 0, "max_output_bytes": 1024})
		chunk := item["output"].(string)
		if !utf8.ValidString(chunk) {
			t.Fatal("UTF-8 split across polls")
		}
		if item["output_bytes"].(float64) > 1024 {
			t.Fatal("output limit exceeded")
		}
		all.WriteString(chunk)
		dropped += int64(item["dropped_bytes"].(float64))
		if item["has_more_output"] == false {
			break
		}
	}
	if dropped == 0 || all.Len() > maxBufferBytes || !strings.HasSuffix(all.String(), "终结\n") {
		t.Fatalf("bounded tail failed: bytes=%d dropped=%d", all.Len(), dropped)
	}
}

func TestFileRoundTripOverwriteAndSymlink(t *testing.T) {
	r := newTestRuntime(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "folder with spaces", "你好.txt")
	call(t, r, "alice", "write_file", map[string]any{"path": path, "content": "first\n第二行\nthird\n", "create_parents": true})
	if _, err := r.Call(context.Background(), "alice", "write_file", raw(t, map[string]any{"path": path, "content": "wrong"})); err == nil {
		t.Fatal("write_file overwrote without authorization")
	}
	read := call(t, r, "alice", "read", map[string]any{"paths": []string{path}, "start_line": 2, "end_line": 2})
	entry := read["entries"].([]any)[0].(map[string]any)
	if entry["content"] != "第二行\n" {
		t.Fatal(entry)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	call(t, r, "alice", "write_file", map[string]any{"path": link, "content": "through link", "overwrite": true})
	b, _ := os.ReadFile(path)
	if string(b) != "through link" {
		t.Fatal("normal symlink semantics not preserved")
	}
	bin := []byte{0, 1, 255, 128, 10}
	binPath := filepath.Join(dir, "binary")
	call(t, r, "alice", "write_file", map[string]any{"path": binPath, "content": base64.StdEncoding.EncodeToString(bin), "encoding": "base64"})
	read = call(t, r, "alice", "read", map[string]any{"paths": []string{binPath}, "encoding": "base64"})
	entry = read["entries"].([]any)[0].(map[string]any)
	if entry["content"] != base64.StdEncoding.EncodeToString(bin) {
		t.Fatal(entry)
	}
}

func TestPatchPreflightMoveAndSymlink(t *testing.T) {
	r := newTestRuntime(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "old.txt")
	if err := os.WriteFile(path, []byte("one\r\ntwo\r\n"), 0640); err != nil {
		t.Fatal(err)
	}
	bad := "*** Begin Patch\n*** Add File: should-not-exist\n+hello\n*** Update File: old.txt\n@@\n-missing\n+replacement\n*** End Patch\n"
	if _, err := r.Call(context.Background(), "alice", "apply_patch", raw(t, PatchInput{Patch: bad, Cwd: dir})); err == nil {
		t.Fatal("invalid context accepted")
	}
	if _, err := os.Stat(filepath.Join(dir, "should-not-exist")); !os.IsNotExist(err) {
		t.Fatal("preflight failure mutated an earlier file")
	}
	valid := "*** Begin Patch\n*** Update File: old.txt\n*** Move to: nested/new.txt\n@@\n one\n-two\n+三\n*** End Patch\n"
	call(t, r, "alice", "apply_patch", PatchInput{Patch: valid, Cwd: dir})
	newPath := filepath.Join(dir, "nested/new.txt")
	b, err := os.ReadFile(newPath)
	if err != nil || string(b) != "one\r\n三\r\n" {
		t.Fatalf("CRLF move failed: %q %v", b, err)
	}
	if _, err = os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("move source remains")
	}
	info, _ := os.Stat(newPath)
	if info.Mode().Perm() != 0640 {
		t.Fatalf("mode lost: %v", info.Mode())
	}
	link := filepath.Join(dir, "alias")
	if err = os.Symlink(newPath, link); err != nil {
		t.Fatal(err)
	}
	call(t, r, "alice", "apply_patch", PatchInput{Patch: "*** Begin Patch\n*** Update File: alias\n@@\n-one\n+first\n 三\n*** End Patch\n", Cwd: dir})
	if info, err = os.Lstat(link); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatal("patch replaced a symlink instead of its target")
	}
	b, _ = os.ReadFile(newPath)
	if string(b) != "first\r\n三\r\n" {
		t.Fatal(string(b))
	}
}

func TestPatchRejectsAmbiguousContextAndExistingDestination(t *testing.T) {
	r := newTestRuntime(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "repeat"), []byte("x\nx\n"), 0600); err != nil {
		t.Fatal(err)
	}
	_, err := r.Call(context.Background(), "alice", "apply_patch", raw(t, PatchInput{Patch: "*** Begin Patch\n*** Update File: repeat\n@@\n-x\n+y\n*** End Patch", Cwd: dir}))
	if err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("ambiguous context: %v", err)
	}
	_, err = r.Call(context.Background(), "alice", "apply_patch", raw(t, PatchInput{Patch: "*** Begin Patch\n*** Add File: repeat\n+new\n*** End Patch", Cwd: dir}))
	if err == nil {
		t.Fatal("add clobbered an existing file")
	}
	b, _ := os.ReadFile(filepath.Join(dir, "repeat"))
	if string(b) != "x\nx\n" {
		t.Fatal(string(b))
	}
}

func TestImageAndValidation(t *testing.T) {
	r := newTestRuntime(t)
	path := filepath.Join(t.TempDir(), "pixel.png")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	im := image.NewRGBA(image.Rect(0, 0, 2, 2))
	im.Set(0, 0, color.RGBA{255, 0, 0, 255})
	if err = png.Encode(f, im); err != nil {
		t.Fatal(err)
	}
	f.Close()
	res, err := r.Call(context.Background(), "alice", "view_image", raw(t, ImageInput{Path: path}))
	if err != nil || res.Image == nil || res.Image.MIMEType != "image/png" {
		t.Fatalf("image result: %+v %v", res, err)
	}
	if _, err = r.Call(context.Background(), "alice", "exec_command", json.RawMessage(`{"cmd":"echo no","yield_time_ms":30001}`)); err == nil {
		t.Fatal("unbounded wait accepted")
	}
	if _, err = r.Call(context.Background(), "alice", "exec_command", json.RawMessage(`{"cmd":"echo no","unknown":"field"}`)); err == nil {
		t.Fatal("unknown execution fields accepted")
	}
	if _, err = r.Call(context.Background(), "", "device_info", json.RawMessage(`{}`)); err == nil {
		t.Fatal("empty principal accepted")
	}
}

func TestCompletedSessionsRecycleWithoutReplayingRequest(t *testing.T) {
	r := newTestRuntime(t)
	path := filepath.Join(t.TempDir(), "once")
	input := map[string]any{"cmd": "printf x >> " + shellQuote(path), "shell": "/bin/sh", "request_id": "do-once", "yield_time_ms": 1000}
	first, _ := untilDone(t, r, "alice", call(t, r, "alice", "exec_command", input))
	for i := 0; i < maxSessions+5; i++ {
		untilDone(t, r, "alice", call(t, r, "alice", "exec_command", map[string]any{"cmd": ":", "shell": "/bin/sh", "yield_time_ms": 1000}))
	}
	if _, err := r.owned("alice", first["session_id"].(string)); err == nil {
		t.Fatal("old fully consumed session was not recycled")
	}
	if _, err := r.Call(context.Background(), "alice", "exec_command", raw(t, input)); err == nil || !strings.Contains(err.Error(), "will not be replayed") {
		t.Fatalf("recycled idempotency key lost its tombstone: %v", err)
	}
	b, err := os.ReadFile(path)
	if err != nil || string(b) != "x" {
		t.Fatalf("recycled request repeated side effects: %q %v", b, err)
	}
}

func TestNativeFilePermissionErrorsArePreserved(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root is permitted to read a mode-000 file")
	}
	r := newTestRuntime(t)
	path := filepath.Join(t.TempDir(), "unreadable")
	if err := os.WriteFile(path, []byte("private"), 0000); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Call(context.Background(), "alice", "read", raw(t, ReadInput{Paths: []string{path}})); !os.IsPermission(err) {
		t.Fatalf("native read permission error was not preserved: %v", err)
	}
	if _, err := r.Call(context.Background(), "alice", "write_file", raw(t, WriteInput{Path: path, Content: "replacement", Overwrite: true})); !os.IsPermission(err) {
		t.Fatalf("native write permission error was not preserved: %v", err)
	}
}

func TestSpecialFilesCannotBlockFileTools(t *testing.T) {
	r := newTestRuntime(t)
	dir := t.TempDir()
	fifo := filepath.Join(dir, "no-writer.fifo")
	if err := syscall.Mkfifo(fifo, 0600); err != nil {
		t.Fatal(err)
	}
	checks := []struct {
		name string
		args any
	}{
		{"view_image", ImageInput{Path: fifo}},
		{"write_file", WriteInput{Path: fifo, Content: "never written", Overwrite: true}},
		{"apply_patch", PatchInput{Patch: "*** Begin Patch\n*** Update File: no-writer.fifo\n@@\n-before\n+after\n*** End Patch", Cwd: dir}},
	}
	for _, check := range checks {
		t.Run(check.name, func(t *testing.T) {
			result := make(chan error, 1)
			args := raw(t, check.args)
			go func() { _, err := r.Call(context.Background(), "alice", check.name, args); result <- err }()
			select {
			case err := <-result:
				if err == nil || !strings.Contains(err.Error(), "regular") {
					t.Fatalf("expected special-file refusal: %v", err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("file tool blocked while opening FIFO")
			}
		})
	}
	meta := call(t, r, "alice", "read", ReadInput{Paths: []string{fifo}})
	entry := meta["entries"].([]any)[0].(map[string]any)
	if entry["type"] != "special" {
		t.Fatalf("read should return FIFO metadata: %#v", entry)
	}
	if err := atomicWrite(fifo, []byte("do not replace FIFO"), 0600, false, false); err == nil {
		t.Fatal("atomic patch write replaced a special-file target")
	}
	info, err := os.Lstat(fifo)
	if err != nil || info.Mode()&os.ModeNamedPipe == 0 {
		t.Fatal("FIFO was modified by rejected file operations")
	}
	// An apply_patch failure must release filesMu; otherwise all future writes hang.
	result := make(chan error, 1)
	args := raw(t, WriteInput{Path: filepath.Join(dir, "after-rejection"), Content: "ok"})
	go func() { _, err := r.Call(context.Background(), "alice", "write_file", args); result <- err }()
	select {
	case err := <-result:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("special-file patch failure left all file writes blocked")
	}
}

func TestSessionInventoryStaysWithinTransportFrame(t *testing.T) {
	r := newTestRuntime(t)
	// Control bytes require six bytes each in JSON. A multibyte rune straddles
	// the 4 KiB preview boundary, exercising truncation and worst-case escaping.
	command := strings.Repeat("\x01", 4095) + "你" + strings.Repeat("\x01", (128<<10)-4098)
	now := time.Now().UTC()
	r.mu.Lock()
	for i := 0; i < maxSessions; i++ {
		done := make(chan struct{})
		close(done)
		id := fmt.Sprintf("retained-%03d", i)
		r.sessions[id] = &session{id: id, principal: "alice", command: command, startedAt: now.Add(-time.Duration(i) * time.Second), finishedAt: now, done: done}
	}
	r.mu.Unlock()
	result, err := r.Call(context.Background(), "alice", "list_processes", json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	frame, err := json.Marshal(protocol.Message{Type: "result", ID: strings.Repeat("r", 128), Result: &result})
	if err != nil {
		t.Fatal(err)
	}
	if len(frame) >= protocol.MaxMessageBytes {
		t.Fatalf("64-session inventory exceeds the device transport frame: %d bytes", len(frame))
	}
	var inventory struct {
		Sessions []struct {
			Command          string `json:"command"`
			CommandTruncated bool   `json:"command_truncated"`
			CommandBytes     int    `json:"command_bytes"`
		} `json:"sessions"`
	}
	if err := json.Unmarshal(result.Data, &inventory); err != nil {
		t.Fatal(err)
	}
	if len(inventory.Sessions) != maxSessions {
		t.Fatalf("lost sessions: %d", len(inventory.Sessions))
	}
	for _, item := range inventory.Sessions {
		if !item.CommandTruncated || item.CommandBytes != len(command) || len(item.Command) != 4095 || !utf8.ValidString(item.Command) {
			t.Fatalf("invalid command preview metadata: bytes=%d original=%d truncated=%t", len(item.Command), item.CommandBytes, item.CommandTruncated)
		}
	}
	r.mu.Lock()
	for _, s := range r.sessions {
		if s.command != command {
			r.mu.Unlock()
			t.Fatal("metadata truncation changed the execution command")
		}
	}
	r.mu.Unlock()
	t.Logf("64-session inventory, maximum JSON escaping: %d bytes (limit %d)", len(frame), protocol.MaxMessageBytes)
}

func TestPatchCreationHonorsUmask(t *testing.T) {
	const childFlag = "HOSTMCP_TEST_UMASK_CHILD_DIRECTORY"
	if dir := os.Getenv(childFlag); dir != "" {
		// This branch runs in a separate test process selected by an exact -test.run.
		// Changing umask here cannot race with or affect the parent test process.
		syscall.Umask(0077)
		r := newTestRuntime(t)
		for name, mode := range map[string]os.FileMode{"update.txt": 0640, "move.txt": 0751} {
			path := filepath.Join(dir, name)
			if err := os.WriteFile(path, []byte("before\n"), 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(path, mode); err != nil {
				t.Fatal(err)
			}
		}
		patch := "*** Begin Patch\n*** Add File: new.txt\n+new\n*** Update File: update.txt\n@@\n-before\n+updated\n*** Update File: move.txt\n*** Move to: moved.txt\n@@\n-before\n+moved\n*** End Patch\n"
		call(t, r, "alice", "apply_patch", PatchInput{Patch: patch, Cwd: dir})
		for name, want := range map[string]os.FileMode{"new.txt": 0600, "update.txt": 0640, "moved.txt": 0751} {
			info, err := os.Stat(filepath.Join(dir, name))
			if err != nil {
				t.Fatal(err)
			}
			if got := info.Mode().Perm(); got != want {
				t.Fatalf("umask077: %s mode=%04o, want %04o", name, got, want)
			}
		}
		return
	}
	dir := t.TempDir()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, executable, "-test.run=^TestPatchCreationHonorsUmask$", "-test.v")
	cmd.Env = append(os.Environ(), childFlag+"="+dir)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("isolated umask fixture failed: %v\n%s", err, output)
	}
	t.Logf("isolated umask077 fixture passed: new=0600, update=0640, move=0751")
}

var _ protocol.Executor = (*Runtime)(nil)
