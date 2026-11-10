package host

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A file larger than one response can be read in base64 chunks with offset
// and rebuilt with write_file append, guarded by expected_size.
func TestChunkedReadAndAppend(t *testing.T) {
	r := newTestRuntime(t)
	dir := t.TempDir()
	data := make([]byte, 2<<20+12345)
	if _, err := rand.Read(data); err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(dir, "src.bin")
	if err := os.WriteFile(src, data, 0644); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(dir, "dst.bin")
	var size int64
	for offset := int64(0); ; {
		out, errText := tryCall(t, r, "read", map[string]any{"paths": []string{src}, "encoding": "base64", "offset": offset, "max_bytes": 1 << 20})
		if errText != "" {
			t.Fatal(errText)
		}
		e := out["entries"].([]any)[0].(map[string]any)
		chunk, _ := base64.StdEncoding.DecodeString(e["content"].(string))
		if e["offset"] != float64(offset) || !bytes.Equal(chunk, data[offset:offset+int64(len(chunk))]) {
			t.Fatalf("chunk at %d: offset=%v", offset, e["offset"])
		}
		args := map[string]any{"path": dst, "content": e["content"], "encoding": "base64"}
		if offset > 0 {
			args["append"], args["expected_size"] = true, offset
		}
		w, errText := tryCall(t, r, "write_file", args)
		if errText != "" {
			t.Fatal(errText)
		}
		offset += int64(len(chunk))
		if offset > 1<<20 && w["size"] != float64(offset) {
			t.Fatalf("append size %v, want %d", w["size"], offset)
		}
		size = offset
		if e["truncated"] == false {
			break
		}
	}
	got, err := os.ReadFile(dst)
	if err != nil || size != int64(len(data)) || !bytes.Equal(got, data) {
		t.Fatalf("rebuilt %d of %d bytes, equal=%v: %v", len(got), len(data), bytes.Equal(got, data), err)
	}

	// A repeated chunk is refused and writes nothing.
	_, errText := tryCall(t, r, "write_file", map[string]any{"path": dst, "content": "x", "append": true, "expected_size": 0})
	if !strings.Contains(errText, "not the expected_size 0") {
		t.Fatalf("stale expected_size: %s", errText)
	}
	if info, _ := os.Stat(dst); info.Size() != int64(len(data)) {
		t.Fatal("refused append changed the file")
	}
	// Small appends report the whole file's sha256.
	small := filepath.Join(dir, "small.txt")
	if err := os.WriteFile(small, []byte("one\n"), 0644); err != nil {
		t.Fatal(err)
	}
	w, errText := tryCall(t, r, "write_file", map[string]any{"path": small, "content": "two\n", "append": true})
	if errText != "" || w["sha256"] != sha256Hex([]byte("one\ntwo\n")) || w["size"] != float64(8) || w["written_bytes"] != float64(4) {
		t.Fatalf("small append: %v %s", w, errText)
	}

	for _, tc := range []struct {
		tool string
		args map[string]any
		want string
	}{
		{"write_file", map[string]any{"path": filepath.Join(dir, "missing"), "content": "x", "append": true}, "does not exist"},
		{"write_file", map[string]any{"path": small, "content": "x", "append": true, "overwrite": true}, "cannot be combined"},
		{"write_file", map[string]any{"path": small, "content": "x", "append": true, "mode": "0600"}, "cannot be combined"},
		{"write_file", map[string]any{"path": small, "content": "x", "overwrite": true, "expected_size": 8}, "only with append"},
		{"read", map[string]any{"paths": []string{small}, "offset": 2}, "only to encoding=base64"},
		{"read", map[string]any{"paths": []string{small}, "encoding": "base64", "offset": 100}, "beyond the end"},
	} {
		if _, errText := tryCall(t, r, tc.tool, tc.args); !strings.Contains(errText, tc.want) {
			t.Fatalf("%s %v: %q, want %q", tc.tool, tc.args, errText, tc.want)
		}
	}
	// Reading at the end returns nothing and is not truncated.
	out, _ := tryCall(t, r, "read", map[string]any{"paths": []string{small}, "encoding": "base64", "offset": 8})
	if e := out["entries"].([]any)[0].(map[string]any); e["content"] != "" || e["truncated"] != false {
		t.Fatalf("read at end: %v", e)
	}
}

func TestMovePath(t *testing.T) {
	r := newTestRuntime(t)
	dir := t.TempDir()
	write := func(name, content string, mode os.FileMode) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(content), mode); err != nil {
			t.Fatal(err)
		}
		return p
	}
	a := write("a", "A", 0750)
	b := write("b", "B", 0644)
	before, _ := os.Stat(a)

	// No replacement without overwrite; nothing moves.
	if _, errText := tryCall(t, r, "move_path", map[string]any{"from": a, "to": b}); !strings.Contains(errText, "already exists") {
		t.Fatalf("no-clobber: %s", errText)
	}
	if got, _ := os.ReadFile(b); string(got) != "B" {
		t.Fatal("destination replaced")
	}
	// A plain rename keeps the inode and mode.
	c := filepath.Join(dir, "c")
	out, errText := tryCall(t, r, "move_path", map[string]any{"from": "a", "to": "c", "cwd": dir})
	after, err := os.Stat(c)
	if errText != "" || err != nil || !os.SameFile(before, after) || after.Mode().Perm() != 0750 || out["replaced"] != false {
		t.Fatalf("rename: %v %s %v", out, errText, err)
	}
	if _, err := os.Lstat(a); !os.IsNotExist(err) {
		t.Fatal("source still exists")
	}
	// overwrite replaces atomically.
	out, errText = tryCall(t, r, "move_path", map[string]any{"from": c, "to": b, "overwrite": true})
	if got, _ := os.ReadFile(b); errText != "" || string(got) != "A" || out["replaced"] != true {
		t.Fatalf("overwrite: %v %s", out, errText)
	}
	// A final symlink is renamed, not followed; directories move too.
	link := filepath.Join(dir, "link")
	if err := os.Symlink(b, link); err != nil {
		t.Fatal(err)
	}
	if _, errText = tryCall(t, r, "move_path", map[string]any{"from": link, "to": filepath.Join(dir, "link2")}); errText != "" {
		t.Fatal(errText)
	}
	if target, err := os.Readlink(filepath.Join(dir, "link2")); err != nil || target != b {
		t.Fatalf("symlink: %q %v", target, err)
	}
	sub := filepath.Join(dir, "sub")
	if err := os.MkdirAll(filepath.Join(sub, "x"), 0755); err != nil {
		t.Fatal(err)
	}
	if _, errText = tryCall(t, r, "move_path", map[string]any{"from": sub, "to": filepath.Join(dir, "sub2")}); errText != "" {
		t.Fatal(errText)
	}
	if _, err := os.Stat(filepath.Join(dir, "sub2", "x")); err != nil {
		t.Fatal(err)
	}
	for _, args := range []map[string]any{
		{"from": filepath.Join(dir, "missing"), "to": filepath.Join(dir, "z")},
		{"from": b, "to": filepath.Join(dir, "no", "such", "dir")},
		{"from": b},
	} {
		if _, errText := tryCall(t, r, "move_path", args); errText == "" {
			t.Fatalf("accepted %v", args)
		}
	}
}

func TestLinkRenameFallback(t *testing.T) {
	dir := t.TempDir()
	from, to := filepath.Join(dir, "f"), filepath.Join(dir, "g")
	if err := os.WriteFile(from, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(to, []byte("y"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := linkRename(from, to, nil); !os.IsExist(err) {
		t.Fatalf("fallback replaced or failed oddly: %v", err)
	}
	if err := os.Remove(to); err != nil {
		t.Fatal(err)
	}
	if err := linkRename(from, to, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(from); !os.IsNotExist(err) {
		t.Fatal("fallback kept the source")
	}
	if err := linkRename(dir, filepath.Join(t.TempDir(), "d"), nil); err == nil {
		t.Fatal("fallback moved a directory")
	}
}
