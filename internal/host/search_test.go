package host

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
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
	want := []string{"README.md:2:needle with CRLF", "main.go:3:func main() { needle() }"}
	if got := matchedLines(out); !slices.Equal(got, want) {
		t.Fatalf("matches %q, want %q", got, want)
	}
	skipped := out["skipped"].(map[string]any)
	if out["mode"] != "content" || skipped["binary"] != float64(1) || skipped["dependency_dirs"] != float64(1) || out["truncated"] != false {
		t.Fatalf("result %#v", out)
	}
	if fmt.Sprint(out["skipped_dependency_dirs"]) != "[node_modules]" {
		t.Fatalf("skipped dirs %#v", out["skipped_dependency_dirs"])
	}
	deps := searchCall(t, r, map[string]any{"path": dir, "pattern": "needle", "include_dependencies": true})
	if got := matchedLines(deps); !slices.Contains(got, "node_modules/m/index.js:1:needle") || deps["skipped_dependency_dirs"] != nil {
		t.Fatalf("include_dependencies: %q", got)
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

	// Anchors apply to each line, not to the whole file.
	anchored := searchCall(t, r, map[string]any{"path": dir, "pattern": `^func|CRLF$|\Avar`})
	if got := matchedLines(anchored); !slices.Equal(got, []string{"README.md:2:needle with CRLF", "main.go:3:func main() { needle() }", "pkg/deep/more/x_test.go:2:func TestNeedle() {}", "pkg/util.go:3:var a_b = 1"}) {
		t.Fatalf("anchored: %q", got)
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
		if item["path"] == "main.go" && (item["type"] != "file" || item["size"] == nil) || item["path"] == "pkg" && (item["type"] != "directory" || item["contents_skipped"] != nil) {
			t.Fatalf("entry %#v", item)
		}
		if item["path"] == "node_modules" && (item["type"] != "directory" || item["contents_skipped"] != true) {
			t.Fatalf("dependency entry %#v", item)
		}
	}
}

func TestSearchSkipsDependencyAndCacheDirs(t *testing.T) {
	r := newTestRuntime(t)
	dir := t.TempDir()
	files := map[string]string{
		"src/lib.rs":                      "needle\n",
		"target/CACHEDIR.TAG":             cacheDirSignature + "\n# cargo\n",
		"target/debug/out.rs":             "needle\n",
		"fake/CACHEDIR.TAG":               "not a signature\n",
		"fake/kept.txt":                   "needle\n",
		"env/pyvenv.cfg":                  "home = /usr/bin\n",
		"env/lib/site.py":                 "needle\n",
		"app/__pycache__/x.py":            "needle\n",
		"web/bower_components/b/index.js": "needle\n",
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
	out := searchCall(t, r, map[string]any{"path": dir, "pattern": "needle"})
	if got := matchedLines(out); !slices.Equal(got, []string{"fake/kept.txt:1:needle", "src/lib.rs:1:needle"}) {
		t.Fatalf("matches %q", got)
	}
	if got := fmt.Sprint(out["skipped_dependency_dirs"]); got != "[app/__pycache__ env target web/bower_components]" {
		t.Fatalf("skipped %s", got)
	}
	// Naming a skipped directory as the root searches it.
	inside := searchCall(t, r, map[string]any{"path": filepath.Join(dir, "target"), "pattern": "needle"})
	if got := matchedLines(inside); !slices.Equal(got, []string{"debug/out.rs:1:needle"}) {
		t.Fatalf("root inside target: %q", got)
	}
}

func TestSearchContextLines(t *testing.T) {
	r := newTestRuntime(t)
	dir := t.TempDir()
	var b strings.Builder
	for i := 1; i <= 20; i++ {
		fmt.Fprintf(&b, "line %d", i)
		if i == 5 || i == 7 || i == 15 {
			b.WriteString(" hit")
		}
		b.WriteString("\n")
	}
	file := filepath.Join(dir, "f.txt")
	if err := os.WriteFile(file, []byte(b.String()), 0644); err != nil {
		t.Fatal(err)
	}
	out := searchCall(t, r, map[string]any{"path": file, "pattern": "hit", "context_lines": 3})
	type m struct {
		Line          int
		Before, After []string
	}
	var got []m
	for _, x := range out["matches"].([]any) {
		item := x.(map[string]any)
		var e m
		e.Line = int(item["line"].(float64))
		for _, s := range item["before"].([]any) {
			e.Before = append(e.Before, s.(string))
		}
		for _, s := range item["after"].([]any) {
			e.After = append(e.After, s.(string))
		}
		got = append(got, e)
	}
	want := []m{
		{5, []string{"line 2", "line 3", "line 4"}, []string{"line 6"}},
		{7, nil, []string{"line 8", "line 9", "line 10"}},
		{15, []string{"line 12", "line 13", "line 14"}, []string{"line 16", "line 17", "line 18"}},
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("context\n got %v\nwant %v", got, want)
	}
	// Context at the start and end of a file, and none by default.
	edge := searchCall(t, r, map[string]any{"path": file, "pattern": "^line (1|20)$", "context_lines": 2})
	items := edge["matches"].([]any)
	first, last := items[0].(map[string]any), items[len(items)-1].(map[string]any)
	if len(first["before"].([]any)) != 0 || len(last["after"].([]any)) != 0 || len(last["before"].([]any)) != 2 {
		t.Fatalf("edges %#v", edge)
	}
	plain := searchCall(t, r, map[string]any{"path": file, "pattern": "hit"})
	if _, ok := plain["matches"].([]any)[0].(map[string]any)["before"]; ok {
		t.Fatalf("context without context_lines: %#v", plain)
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
