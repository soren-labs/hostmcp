package host

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func sha256Hex(b []byte) string { s := sha256.Sum256(b); return hex.EncodeToString(s[:]) }

func tryCall(t *testing.T, r *Runtime, tool string, args map[string]any) (map[string]any, string) {
	t.Helper()
	result, err := r.Call(context.Background(), "alice", tool, raw(t, args))
	if err != nil {
		return nil, err.Error()
	}
	if result.Error != "" {
		return nil, result.Error
	}
	var out map[string]any
	if err := json.Unmarshal(result.Data, &out); err != nil {
		t.Fatal(err)
	}
	return out, ""
}

func TestReadReportsWholeFileSHA256(t *testing.T) {
	r := newTestRuntime(t)
	dir := t.TempDir()
	text := []byte("one\ntwo\nthree\n")
	bin := []byte{0, 1, 2, 3, 0xff}
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), text, 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "b.bin"), bin, 0644); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		args map[string]any
		want string
	}{
		{"text", map[string]any{"paths": []string{"a.txt"}, "cwd": dir}, sha256Hex(text)},
		{"line range", map[string]any{"paths": []string{"a.txt"}, "cwd": dir, "start_line": 2, "end_line": 2}, sha256Hex(text)},
		{"base64", map[string]any{"paths": []string{"b.bin"}, "cwd": dir, "encoding": "base64"}, sha256Hex(bin)},
		{"binary detected", map[string]any{"paths": []string{"b.bin"}, "cwd": dir}, sha256Hex(bin)},
	} {
		out, errText := tryCall(t, r, "read", tc.args)
		if errText != "" {
			t.Fatalf("%s: %s", tc.name, errText)
		}
		entry := out["entries"].([]any)[0].(map[string]any)
		if entry["sha256"] != tc.want {
			t.Fatalf("%s: sha256=%v want %s", tc.name, entry["sha256"], tc.want)
		}
	}
	out, _ := tryCall(t, r, "read", map[string]any{"paths": []string{dir}})
	if _, ok := out["entries"].([]any)[0].(map[string]any)["sha256"]; ok {
		t.Fatal("directory entry has sha256")
	}
}

func TestWriteFileExpectedSHA256(t *testing.T) {
	r := newTestRuntime(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "config.txt")
	original := []byte("version 1\n")
	if err := os.WriteFile(path, original, 0600); err != nil {
		t.Fatal(err)
	}
	before, _ := os.Stat(path)

	// A stale hash writes nothing and reports the current one.
	_, errText := tryCall(t, r, "write_file", map[string]any{"path": path, "content": "stale", "overwrite": true, "expected_sha256": sha256Hex([]byte("something else"))})
	if !strings.Contains(errText, "has changed") || !strings.Contains(errText, sha256Hex(original)) {
		t.Fatalf("stale hash: %q", errText)
	}
	if b, _ := os.ReadFile(path); string(b) != string(original) {
		t.Fatalf("stale hash modified the file: %q", b)
	}

	// The matching hash (any case) replaces the file in place.
	out, errText := tryCall(t, r, "write_file", map[string]any{"path": path, "content": "version 2\n", "overwrite": true, "expected_sha256": strings.ToUpper(sha256Hex(original))})
	if errText != "" {
		t.Fatal(errText)
	}
	if out["previous_sha256"] != sha256Hex(original) || out["sha256"] != sha256Hex([]byte("version 2\n")) || out["written_bytes"] != float64(10) {
		t.Fatalf("result %#v", out)
	}
	after, _ := os.Stat(path)
	if b, _ := os.ReadFile(path); string(b) != "version 2\n" || !os.SameFile(before, after) || after.Mode().Perm() != 0600 {
		t.Fatalf("content=%q same=%v mode=%v", b, os.SameFile(before, after), after.Mode().Perm())
	}

	// A shorter replacement leaves no stale tail.
	if _, errText = tryCall(t, r, "write_file", map[string]any{"path": path, "content": "v3", "overwrite": true, "expected_sha256": sha256Hex([]byte("version 2\n"))}); errText != "" {
		t.Fatal(errText)
	}
	if b, _ := os.ReadFile(path); string(b) != "v3" {
		t.Fatalf("content=%q", b)
	}

	for _, tc := range []struct {
		name string
		args map[string]any
		want string
	}{
		{"needs overwrite", map[string]any{"path": path, "content": "x", "expected_sha256": sha256Hex([]byte("v3"))}, "requires overwrite=true"},
		{"missing file", map[string]any{"path": filepath.Join(dir, "missing"), "content": "x", "overwrite": true, "expected_sha256": sha256Hex(nil)}, "does not exist"},
		{"bad hash", map[string]any{"path": path, "content": "x", "overwrite": true, "expected_sha256": "abc"}, "64 hexadecimal"},
	} {
		if _, errText = tryCall(t, r, "write_file", tc.args); !strings.Contains(errText, tc.want) {
			t.Fatalf("%s: %q", tc.name, errText)
		}
	}
	if b, _ := os.ReadFile(path); string(b) != "v3" {
		t.Fatalf("failed calls modified the file: %q", b)
	}
}
