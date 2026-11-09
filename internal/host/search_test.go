package host

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"syscall"
	"testing"
	"time"
)

func searchTree(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	files := map[string]string{
		"main.go":                 "package main\n\nfunc main() { needle() }\n",
		"pkg/util.go":             "package pkg\n// Needle in a comment\nvar a_b = 1\n",
		"pkg/deep/more/x_test.go": "package more\nfunc TestNeedle() {}\n",
		".hidden/secret.go":       "needle\n",
		"node_modules/m/index.js": "needle\n",
		"README.md":               "no match here\r\nneedle with CRLF\r\n",
	}
	for name, content := range files {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "blob.bin"), []byte("needle\x00binary"), 0644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func searchCall(t *testing.T, r *Runtime, args map[string]any) map[string]any {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	res, err := r.Call(ctx, "alice", "search", raw(t, args))
	if err != nil || res.Error != "" {
		t.Fatalf("search %v: %v %s", args, err, res.Error)
	}
	var out map[string]any
	if err := json.Unmarshal(res.Data, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func matchedLines(out map[string]any) []string {
	var got []string
	for _, m := range out["matches"].([]any) {
		item := m.(map[string]any)
		got = append(got, fmt.Sprintf("%s:%d:%s", item["path"], int(item["line"].(float64)), item["text"]))
	}
	return got
}

func entryPaths(out map[string]any) []string {
	var got []string
	for _, e := range out["entries"].([]any) {
		got = append(got, e.(map[string]any)["path"].(string))
	}
	return got
}

func TestSearchContent(t *testing.T) {
	r := newTestRuntime(t)
	dir := searchTree(t)
	out := searchCall(t, r, map[string]any{"path": dir, "pattern": "needle"})
	want := []string{"README.md:2:needle with CRLF", "main.go:3:func main() { needle() }", "node_modules/m/index.js:1:needle"}
	if got := matchedLines(out); !slices.Equal(got, want) {
		t.Fatalf("matches %q, want %q", got, want)
	}
	skipped := out["skipped"].(map[string]any)
	if out["mode"] != "content" || skipped["binary"] != float64(1) || out["truncated"] != false {
		t.Fatalf("result %#v", out)
	}

	out = searchCall(t, r, map[string]any{"path": dir, "pattern": "NEEDLE", "case_insensitive": true, "glob": "*.go", "include_hidden": true})
	want = []string{".hidden/secret.go:1:needle", "main.go:3:func main() { needle() }", "pkg/deep/more/x_test.go:2:func TestNeedle() {}", "pkg/util.go:2:// Needle in a comment"}
	if got := matchedLines(out); !slices.Equal(got, want) {
		t.Fatalf("case-insensitive *.go: %q", got)
	}

	out = searchCall(t, r, map[string]any{"path": dir, "pattern": "needle", "exclude": []string{"node_modules"}, "glob": "**/*.md"})
	if got := matchedLines(out); !slices.Equal(got, []string{"README.md:2:needle with CRLF"}) {
		t.Fatalf("exclude + **: %q", got)
	}

	// literal treats metacharacters as text; the regex form also matches "a_b".
	lit := searchCall(t, r, map[string]any{"path": dir, "pattern": "a.b", "literal": true})
	re := searchCall(t, r, map[string]any{"path": dir, "pattern": "a.b"})
	if len(lit["matches"].([]any)) != 0 || len(re["matches"].([]any)) != 1 {
		t.Fatalf("literal=%v regex=%v", lit["matches"], re["matches"])
	}

	one := searchCall(t, r, map[string]any{"path": dir, "pattern": "needle", "max_results": 1})
	if len(one["matches"].([]any)) != 1 || one["truncated"] != true || one["truncated_reason"] != "max_results" {
		t.Fatalf("max_results: %#v", one)
	}

	file := searchCall(t, r, map[string]any{"path": filepath.Join(dir, "pkg", "util.go"), "pattern": "var"})
	if got := matchedLines(file); !slices.Equal(got, []string{"util.go:3:var a_b = 1"}) {
		t.Fatalf("file root: %q", got)
	}
}

func TestSearchListsFiles(t *testing.T) {
	r := newTestRuntime(t)
	dir := searchTree(t)
	out := searchCall(t, r, map[string]any{"path": dir, "glob": "pkg/**/*.go"})
	if got := entryPaths(out); !slices.Equal(got, []string{"pkg/deep/more/x_test.go", "pkg/util.go"}) {
		t.Fatalf("glob listing %q", got)
	}
	top := searchCall(t, r, map[string]any{"path": dir, "max_depth": 1})
	if got := entryPaths(top); !slices.Equal(got, []string{"README.md", "blob.bin", "main.go", "node_modules", "pkg"}) {
		t.Fatalf("max_depth 1: %q", got)
	}
	for _, e := range top["entries"].([]any) {
		item := e.(map[string]any)
		if item["path"] == "main.go" && (item["type"] != "file" || item["size"] == nil) || item["path"] == "pkg" && item["type"] != "directory" {
			t.Fatalf("entry %#v", item)
		}
	}
}

func TestSearchSkipsSpecialFilesAndLoops(t *testing.T) {
	r := newTestRuntime(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "ok.txt"), []byte("needle\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(dir, "pipe"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(dir, filepath.Join(dir, "loop")); err != nil {
		t.Fatal(err)
	}
	big, err := os.Create(filepath.Join(dir, "big.log"))
	if err != nil {
		t.Fatal(err)
	}
	if err = big.Truncate(searchFileLimit + 1); err != nil {
		t.Fatal(err)
	}
	big.Close()
	out := searchCall(t, r, map[string]any{"path": dir, "pattern": "needle"})
	skipped := out["skipped"].(map[string]any)
	if got := matchedLines(out); !slices.Equal(got, []string{"ok.txt:1:needle"}) || skipped["too_large"] != float64(1) || skipped["symlinks"] != float64(1) {
		t.Fatalf("matches %q skipped %v", got, skipped)
	}
}

func TestSearchRejectsBadInput(t *testing.T) {
	r := newTestRuntime(t)
	for _, args := range []map[string]any{
		{"pattern": "("},
		{"glob": "[a"},
		{"max_results": 0},
		{"max_depth": 65},
	} {
		res, err := r.Call(context.Background(), "alice", "search", raw(t, args))
		if err == nil && res.Error == "" {
			t.Fatalf("accepted %v", args)
		}
	}
}

func TestMatchGlob(t *testing.T) {
	for _, tc := range []struct {
		glob, rel string
		want      bool
	}{
		{"*.go", "a/b/c.go", true},
		{"*.go", "a/b/c.js", false},
		{"**/*.go", "c.go", true},
		{"**/*.go", "a/b/c.go", true},
		{"a/**", "a/b/c", true},
		{"a/**/c", "a/c", true},
		{"a/**/c", "a/x/y/c", true},
		{"a/*/c", "a/x/y/c", false},
		{"/a/b", "a/b", true},
		{"node_modules", "x/node_modules", true},
	} {
		if got := matchGlob(tc.glob, tc.rel); got != tc.want {
			t.Errorf("matchGlob(%q, %q) = %v", tc.glob, tc.rel, got)
		}
	}
}
