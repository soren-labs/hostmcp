package host

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"os/signal"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func readText(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func inode(t *testing.T, path string) fileIdentity {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return identityOf(info)
}

func patchError(t *testing.T, r *Runtime, patch, cwd string) string {
	t.Helper()
	res, err := r.Call(context.Background(), "alice", "apply_patch", raw(t, PatchInput{Patch: patch, Cwd: cwd}))
	if err != nil {
		return err.Error()
	}
	return res.Error
}

// symlinkTree builds root/a/link -> root/b/sub. Natively, a/link/../file is
// b/file; lexical cleaning would wrongly select a/file.
func symlinkTree(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for _, dir := range []string{"a", "b/sub"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(filepath.Join(root, "b", "sub"), filepath.Join(root, "a", "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "a", "file"), []byte("lexical-file\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "b", "file"), []byte("native-file\n"), 0600); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestPathsKeepNativeSymlinkDotDotSemantics(t *testing.T) {
	r := newTestRuntime(t)
	root := symlinkTree(t)
	lexical, native := filepath.Join(root, "a", "file"), filepath.Join(root, "b", "file")
	spellings := map[string]ReadInput{
		"absolute": {Paths: []string{root + "/a/link/../file"}},
		"relative": {Paths: []string{"link/../file"}, Cwd: root + "/a"},
		"cwd":      {Paths: []string{"../file"}, Cwd: root + "/a/link"},
	}
	home := *r.user
	home.HomeDir = root
	r.user = &home
	spellings["tilde"] = ReadInput{Paths: []string{"~/a/link/../file"}}
	for name, in := range spellings {
		t.Run("read-"+name, func(t *testing.T) {
			out := call(t, r, "alice", "read", in)
			entry := out["entries"].([]any)[0].(map[string]any)
			if entry["content"] != "native-file\n" {
				t.Fatalf("read selected the lexical file: %#v", entry)
			}
		})
	}
	call(t, r, "alice", "write_file", WriteInput{Path: "link/../file", Cwd: root + "/a", Content: "written\n", Overwrite: true})
	if readText(t, native) != "written\n" || readText(t, lexical) != "lexical-file\n" {
		t.Fatal("write_file changed the lexical file instead of the native one")
	}
	call(t, r, "alice", "apply_patch", PatchInput{Patch: "*** Begin Patch\n*** Update File: a/link/../file\n@@\n-written\n+patched\n*** End Patch", Cwd: root})
	if readText(t, native) != "patched\n" || readText(t, lexical) != "lexical-file\n" {
		t.Fatal("apply_patch changed the lexical file instead of the native one")
	}
	call(t, r, "alice", "apply_patch", PatchInput{Patch: "*** Begin Patch\n*** Add File: a/link/../new/added.txt\n+added\n*** End Patch", Cwd: root})
	if readText(t, filepath.Join(root, "b", "new", "added.txt")) != "added\n" {
		t.Fatal("Add File did not create its parent through the symlink")
	}
	if _, err := os.Stat(filepath.Join(root, "a", "new")); !os.IsNotExist(err) {
		t.Fatal("Add File created a lexical parent directory")
	}
	call(t, r, "alice", "write_file", WriteInput{Path: root + "/a/link/../made/w.txt", Content: "w", CreateParents: true})
	if readText(t, filepath.Join(root, "b", "made", "w.txt")) != "w" {
		t.Fatal("create_parents did not follow the symlink")
	}
	first := call(t, r, "alice", "exec_command", ExecInput{Cmd: "pwd -P; printf '%s\\n' \"$PWD\"; cat file", Cwd: root + "/a/link/.."})
	_, output := untilDone(t, r, "alice", first)
	physical, err := filepath.EvalSymlinks(filepath.Join(root, "b"))
	if err != nil {
		t.Fatal(err)
	}
	if output != physical+"\n"+physical+"\npatched\n" {
		t.Fatalf("command cwd did not follow native traversal: %q", output)
	}
}

func TestTidyPathNeverResolvesParents(t *testing.T) {
	for in, want := range map[string]string{
		"/a//b/./c":    "/a/b/c",
		"/a/link/../b": "/a/link/../b",
		"/a/./":        "/a/",
		"/a/.":         "/a/.",
		"/":            "/",
		"//":           "/",
		"/a/b/":        "/a/b/",
	} {
		if got := tidyPath(in); got != want {
			t.Errorf("tidyPath(%q) = %q, want %q", in, got, want)
		}
	}
	if got := nativeDir("/a/link/../new"); got != "/a/link/.." {
		t.Fatalf("nativeDir cleaned its input: %q", got)
	}
}

func TestPatchEditsFileInPlace(t *testing.T) {
	r := newTestRuntime(t)
	dir := t.TempDir()
	locked := filepath.Join(dir, "locked")
	if err := os.Mkdir(locked, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(locked, "config.txt")
	if err := os.WriteFile(path, []byte("keep\nold\n"), 0600); err != nil {
		t.Fatal(err)
	}
	// Set-group-ID on a file of our own primary group is permitted to its owner.
	if err := os.Chmod(path, 0640|os.ModeSetgid); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "hardlink.txt")
	if err := os.Link(path, link); err != nil {
		t.Fatal(err)
	}
	before := inode(t, path)
	modeBefore, _ := os.Stat(path)
	// The account may write the file but not its directory, as with shared
	// group-writable configuration or another account's file in sticky /tmp.
	if err := os.Chmod(locked, 0500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0700) })
	call(t, r, "alice", "apply_patch", PatchInput{Patch: "*** Begin Patch\n*** Update File: config.txt\n@@\n keep\n-old\n+new and longer than before\n*** End Patch", Cwd: locked})
	if readText(t, path) != "keep\nnew and longer than before\n" {
		t.Fatal(readText(t, path))
	}
	if inode(t, path) != before {
		t.Fatal("patch replaced the inode instead of editing it")
	}
	if readText(t, link) != readText(t, path) {
		t.Fatal("hard link no longer shares the patched content")
	}
	info, _ := os.Stat(path)
	if info.Mode() != modeBefore.Mode() {
		t.Fatalf("mode changed from %v to %v", modeBefore.Mode(), info.Mode())
	}
	call(t, r, "alice", "apply_patch", PatchInput{Patch: "*** Begin Patch\n*** Update File: config.txt\n@@\n-new and longer than before\n+x\n*** End Patch", Cwd: locked})
	if readText(t, link) != "keep\nx\n" || inode(t, path) != before {
		t.Fatal("shrinking edit was not applied in place")
	}
	// A hard link and its other name are the same file: patching both in one
	// patch is refused before anything is written.
	if err := os.Chmod(locked, 0700); err != nil {
		t.Fatal(err)
	}
	msg := patchError(t, r, "*** Begin Patch\n*** Update File: "+path+"\n@@\n-x\n+y\n*** Update File: "+link+"\n@@\n-x\n+z\n*** End Patch", dir)
	if !strings.Contains(msg, "same file") || readText(t, path) != "keep\nx\n" {
		t.Fatalf("aliased update was not refused: %q", msg)
	}
}

// The root-only check runs where the test process is root (EC2 acceptance and
// the privileged CI step). It reproduces the review: root patching a file
// owned by an ordinary account must not take that file away from its owner.
func TestRootPatchKeepsOwner(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires root")
	}
	r := newTestRuntime(t)
	dir := t.TempDir()
	if err := os.Chmod(dir, 0755); err != nil {
		t.Fatal(err)
	}
	const uid, gid = 65534, 65534
	path := filepath.Join(dir, "owned.txt")
	if err := os.WriteFile(path, []byte("line\n"), 0664); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0664); err != nil {
		t.Fatal(err)
	}
	if err := os.Chown(path, uid, gid); err != nil {
		t.Fatal(err)
	}
	call(t, r, "root", "apply_patch", PatchInput{Patch: "*** Begin Patch\n*** Update File: owned.txt\n@@\n-line\n+changed by root\n*** End Patch", Cwd: dir})
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	st := info.Sys().(*syscall.Stat_t)
	if st.Uid != uid || st.Gid != gid || info.Mode().Perm() != 0664 || readText(t, path) != "changed by root\n" {
		t.Fatalf("root patch did not preserve other account's file ownership: uid=%d gid=%d mode=%v", st.Uid, st.Gid, info.Mode())
	}
}

func TestRootPatchKeepsOwnerAndMoveKeepsOwner(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires root")
	}
	r := newTestRuntime(t)
	dir := t.TempDir()
	if err := os.Chmod(dir, 0755); err != nil {
		t.Fatal(err)
	}
	const uid, gid = 65534, 65534
	for _, name := range []string{"owned.txt", "moved.txt"} {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte("line\n"), 0664); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, 0664); err != nil {
			t.Fatal(err)
		}
		if err := os.Chown(path, uid, gid); err != nil {
			t.Fatal(err)
		}
	}
	call(t, r, "root", "apply_patch", PatchInput{Patch: "*** Begin Patch\n*** Update File: owned.txt\n@@\n-line\n+changed by root\n*** Update File: moved.txt\n*** Move to: sub/renamed.txt\n@@\n-line\n+moved by root\n*** End Patch", Cwd: dir})
	for _, name := range []string{"owned.txt", "sub/renamed.txt"} {
		info, err := os.Stat(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		st := info.Sys().(*syscall.Stat_t)
		if st.Uid != uid || st.Gid != gid || info.Mode().Perm() != 0664 {
			t.Fatalf("%s owner/mode changed to %d:%d %v", name, st.Uid, st.Gid, info.Mode())
		}
	}
}

func TestPatchWriteFailureRestoresOriginal(t *testing.T) {
	const childFlag = "HOSTMCP_TEST_FSIZE_CHILD_FILE"
	if path := os.Getenv(childFlag); path != "" {
		// Separate process: the file-size limit must not affect the parent tests.
		signal.Ignore(syscall.SIGXFSZ)
		limit := syscall.Rlimit{Cur: 200, Max: 200}
		if err := syscall.Setrlimit(syscall.RLIMIT_FSIZE, &limit); err != nil {
			t.Fatal(err)
		}
		r := newTestRuntime(t)
		long := strings.Repeat("y", 400)
		msg := patchError(t, r, "*** Begin Patch\n*** Update File: "+path+"\n@@\n-short\n+"+long+"\n*** End Patch", "/")
		if msg == "" {
			t.Fatal("write beyond the file-size limit succeeded")
		}
		if got := readText(t, path); got != "keep\nshort\n" {
			t.Fatalf("failed patch left corrupted content %q (%s)", got, msg)
		}
		return
	}
	path := filepath.Join(t.TempDir(), "limited.txt")
	if err := os.WriteFile(path, []byte("keep\nshort\n"), 0600); err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, executable, "-test.run=^TestPatchWriteFailureRestoresOriginal$", "-test.v")
	cmd.Env = append(os.Environ(), childFlag+"="+path)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("isolated write-failure fixture failed: %v\n%s", err, output)
	}
}

func TestPatchEdgeSemantics(t *testing.T) {
	r := newTestRuntime(t)
	dir := t.TempDir()
	write := func(name, content string) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	// Mixed endings: untouched lines keep theirs; inserted lines use the first one.
	mixed := write("mixed.txt", "a\r\nb\nc\r\n")
	call(t, r, "alice", "apply_patch", PatchInput{Patch: "*** Begin Patch\n*** Update File: mixed.txt\n@@\n a\n-b\n+B\n c\n*** End Patch", Cwd: dir})
	if got := readText(t, mixed); got != "a\r\nB\r\nc\r\n" {
		t.Fatalf("mixed endings: %q", got)
	}
	keep := write("keep.txt", "one\ntwo\r\nthree\n")
	call(t, r, "alice", "apply_patch", PatchInput{Patch: "*** Begin Patch\n*** Update File: keep.txt\n@@\n-one\n+uno\n*** End Patch", Cwd: dir})
	if got := readText(t, keep); got != "uno\ntwo\r\nthree\n" {
		t.Fatalf("untouched CRLF line changed: %q", got)
	}
	// Pure additions append at end of file without an anchor, or after it with one.
	add := write("add.txt", "class A:\n    pass\nclass B:\n    pass\n")
	call(t, r, "alice", "apply_patch", PatchInput{Patch: "*** Begin Patch\n*** Update File: add.txt\n@@\n+# end\n*** End Patch", Cwd: dir})
	call(t, r, "alice", "apply_patch", PatchInput{Patch: "*** Begin Patch\n*** Update File: add.txt\n@@ class B:\n+    x = 1\n*** End Patch", Cwd: dir})
	if got := readText(t, add); got != "class A:\n    pass\nclass B:\n    x = 1\n    pass\n# end\n" {
		t.Fatalf("pure additions: %q", got)
	}
	// An anchor selects its first match; the context after it must be unique.
	anchors := write("anchors.py", "def f():\n    return 1\ndef f():\n    return 2\n")
	call(t, r, "alice", "apply_patch", PatchInput{Patch: "*** Begin Patch\n*** Update File: anchors.py\n@@ def f():\n-    return 2\n+    return 3\n*** End Patch", Cwd: dir})
	if got := readText(t, anchors); got != "def f():\n    return 1\ndef f():\n    return 3\n" {
		t.Fatalf("anchor: %q", got)
	}
	// Upstream format: the first hunk may omit @@ and an empty line is empty context.
	blank := write("blank.txt", "first\n\nlast\n")
	call(t, r, "alice", "apply_patch", PatchInput{Patch: "*** Begin Patch\n*** Update File: blank.txt\n first\n\n-last\n+final\n*** End Patch", Cwd: dir})
	if got := readText(t, blank); got != "first\n\nfinal\n" {
		t.Fatalf("empty context: %q", got)
	}
	noNewline := write("no-newline.txt", "a\nb")
	call(t, r, "alice", "apply_patch", PatchInput{Patch: "*** Begin Patch\n*** Update File: no-newline.txt\n@@\n-b\n+c\n+d\n*** End Patch", Cwd: dir})
	if got := readText(t, noNewline); got != "a\nc\nd" {
		t.Fatalf("missing final newline was not preserved: %q", got)
	}
	// Delete File does not read content: large and binary files can be removed.
	big := write("big.bin", strings.Repeat("\x00", 5<<20))
	call(t, r, "alice", "apply_patch", PatchInput{Patch: "*** Begin Patch\n*** Delete File: big.bin\n*** End Patch", Cwd: dir})
	if _, err := os.Lstat(big); !os.IsNotExist(err) {
		t.Fatal("large binary file was not deleted")
	}
	if err := os.Mkdir(filepath.Join(dir, "folder"), 0700); err != nil {
		t.Fatal(err)
	}
	if msg := patchError(t, r, "*** Begin Patch\n*** Delete File: folder\n*** End Patch", dir); !strings.Contains(msg, "not a regular file") {
		t.Fatalf("directory delete: %q", msg)
	}
	// Two spellings of one new file are refused before anything is written.
	msg := patchError(t, r, "*** Begin Patch\n*** Add File: twin.txt\n+1\n*** Add File: folder/../twin.txt\n+2\n*** End Patch", dir)
	if !strings.Contains(msg, "same file") {
		t.Fatalf("aliased add: %q", msg)
	}
	if _, err := os.Lstat(filepath.Join(dir, "twin.txt")); !os.IsNotExist(err) {
		t.Fatal("refused patch wrote a file")
	}
	// Results report the real number of added and removed lines.
	result := call(t, r, "alice", "apply_patch", PatchInput{Patch: "*** Begin Patch\n*** Update File: keep.txt\n@@\n uno\n-two\n+2\n+II\n*** End Patch", Cwd: dir})
	applied := result["applied"].([]any)[0].(map[string]any)
	if applied["lines_added"] != float64(2) || applied["lines_removed"] != float64(1) {
		t.Fatalf("line counts: %#v", applied)
	}
}

func TestStdinBacklogKeepsProcessRunning(t *testing.T) {
	previous := stdinWait
	stdinWait = 300 * time.Millisecond
	t.Cleanup(func() { stdinWait = previous })
	r := newTestRuntime(t)
	gate := filepath.Join(t.TempDir(), "gate")
	// The process does not read until the gate file exists.
	first := call(t, r, "alice", "exec_command", ExecInput{Cmd: "while [ ! -e " + shellQuote(gate) + " ]; do sleep 0.05; done; wc -c", YieldTimeMS: intPtr(0)})
	id := first["session_id"]
	chunk := strings.Repeat("x", 65536)
	total := 0
	queued := false
	for attempt := 0; attempt < 8 && !queued; attempt++ {
		out := call(t, r, "alice", "write_stdin", map[string]any{"session_id": id, "chars": chunk, "yield_time_ms": 0})
		total += len(chunk)
		if out["running"] != true {
			t.Fatalf("process stopped after stdin backlog: %#v", out)
		}
		queued = out["stdin_pending"] == true
	}
	if !queued {
		t.Fatal("pipe never filled; cannot exercise the stdin backlog")
	}
	if _, err := r.Call(context.Background(), "alice", "write_stdin", raw(t, map[string]any{"session_id": id, "chars": "more"})); err == nil || !strings.Contains(err.Error(), "still running") {
		t.Fatalf("second write while input is queued: %v", err)
	}
	if err := os.WriteFile(gate, nil, 0600); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		out := call(t, r, "alice", "write_stdin", map[string]any{"session_id": id, "yield_time_ms": 100})
		if out["stdin_pending"] != true {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("queued input was never delivered")
		}
	}
	out := call(t, r, "alice", "write_stdin", map[string]any{"session_id": id, "close_stdin": true, "yield_time_ms": 1000})
	final, text := untilDone(t, r, "alice", out)
	if final["exit_code"] != float64(0) || strings.TrimSpace(text) != strconv.Itoa(total) {
		t.Fatalf("queued input lost or process killed: %#v %q (want %d bytes)", final, text, total)
	}
}

func intPtr(n int) *int { return &n }

// Helper process for TestProcessLifetimeSemantics. It detaches into its own
// session, as a daemon does, and records its PID.
func TestHelperDetachedProcess(t *testing.T) {
	pidFile := os.Getenv("HOSTMCP_TEST_DETACHED_PIDFILE")
	if pidFile == "" {
		t.Skip("helper process")
	}
	if os.Getenv("HOSTMCP_TEST_DETACHED_CHILD") != "1" {
		cmd := exec.Command(os.Args[0], "-test.run=^TestHelperDetachedProcess$")
		cmd.Env = append(os.Environ(), "HOSTMCP_TEST_DETACHED_CHILD=1")
		cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		return
	}
	if err := os.WriteFile(pidFile, []byte(strconv.Itoa(os.Getpid())), 0600); err != nil {
		t.Fatal(err)
	}
	time.Sleep(30 * time.Second)
}

func alive(pid int) bool { return !errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) }

func TestProcessLifetimeSemantics(t *testing.T) {
	r := newTestRuntime(t)
	dir := t.TempDir()
	// Documented: a background job in the command's process group ends with its shell.
	first := call(t, r, "alice", "exec_command", ExecInput{Cmd: "sleep 30 & echo $!"})
	_, output := untilDone(t, r, "alice", first)
	background, err := strconv.Atoi(strings.TrimSpace(output))
	if err != nil {
		t.Fatalf("background PID: %q", output)
	}
	deadline := time.Now().Add(3 * time.Second)
	for alive(background) && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if alive(background) {
		_ = syscall.Kill(background, syscall.SIGKILL)
		t.Fatal("background job in the session's process group outlived its shell")
	}
	// A process that detaches into its own session is outside that lifetime.
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	pidFile := filepath.Join(dir, "daemon.pid")
	// Give the helper time to spawn its new session before the command shell
	// exits and its original process group is terminated.
	cmd := "HOSTMCP_TEST_DETACHED_PIDFILE=" + shellQuote(pidFile) + " " + shellQuote(executable) + " -test.run='^TestHelperDetachedProcess$' >/dev/null 2>&1 & sleep 0.5"
	_, _ = untilDone(t, r, "alice", call(t, r, "alice", "exec_command", ExecInput{Cmd: cmd}))
	var daemon int
	for deadline = time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if b, err := os.ReadFile(pidFile); err == nil && len(bytes.TrimSpace(b)) > 0 {
			daemon, _ = strconv.Atoi(string(b))
			break
		}
	}
	if daemon == 0 {
		t.Fatal("detached helper did not start")
	}
	t.Cleanup(func() { _ = syscall.Kill(daemon, syscall.SIGKILL) })
	time.Sleep(700 * time.Millisecond)
	if !alive(daemon) {
		t.Fatal("a process detached with setsid was stopped with the session")
	}
}

func TestRuntimeUsesHomeForTilde(t *testing.T) {
	r := newTestRuntime(t)
	u := &user.User{Username: r.user.Username, Uid: r.user.Uid, HomeDir: "/home/example/"}
	r.user = u
	got, err := r.path("~/x/../y", "")
	if err != nil || got != "/home/example/x/../y" {
		t.Fatalf("tilde path %q %v", got, err)
	}
}
