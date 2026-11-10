package host

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func tailEntry(t *testing.T, r *Runtime, args map[string]any) map[string]any {
	t.Helper()
	out, errText := tryCall(t, r, "read", args)
	if errText != "" {
		t.Fatalf("read %v: %s", args, errText)
	}
	return out["entries"].([]any)[0].(map[string]any)
}

func TestReadTailLines(t *testing.T) {
	r := newTestRuntime(t)
	dir := t.TempDir()
	write := func(name, content string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	for _, tc := range []struct {
		name, content string
		n             int
		want          string
		lines         int
	}{
		{"final newline", "a\nb\nc\n", 2, "b\nc\n", 2},
		{"no final newline", "a\nb\nc", 2, "b\nc", 2},
		{"fewer lines than asked", "a\nb\n", 5, "a\nb\n", 2},
		{"empty", "", 3, "", 0},
		{"blank last lines", "a\n\n\n", 2, "\n\n", 2},
		{"crlf", "a\r\nb\r\nc\r\n", 1, "c\r\n", 1},
	} {
		e := tailEntry(t, r, map[string]any{"paths": []string{write(tc.name, tc.content)}, "tail_lines": tc.n})
		if e["content"] != tc.want || e["lines"] != float64(tc.lines) || e["truncated"] != false {
			t.Fatalf("%s: %#v", tc.name, e)
		}
		if want := float64(len(tc.content) - len(tc.want)); e["start_offset"] != want {
			t.Fatalf("%s: start_offset %v want %v", tc.name, e["start_offset"], want)
		}
	}
}

// A file far larger than the 16 MiB forward scan and than one read block can
// still be tailed, and lines spanning block boundaries come back whole.
func TestReadTailLinesLargeFile(t *testing.T) {
	r := newTestRuntime(t)
	path := filepath.Join(t.TempDir(), "big.log")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	pad := strings.Repeat("x", 1000)
	const total = 20000 // about 20 MB
	for i := 1; i <= total; i++ {
		fmt.Fprintf(f, "%06d %s\n", i, pad)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	e := tailEntry(t, r, map[string]any{"paths": []string{path}, "tail_lines": 100, "max_bytes": 1 << 20})
	content := e["content"].(string)
	got := strings.Split(strings.TrimSuffix(content, "\n"), "\n")
	if len(got) != 100 || !strings.HasPrefix(got[0], fmt.Sprintf("%06d ", total-99)) || !strings.HasPrefix(got[99], fmt.Sprintf("%06d ", total)) || e["truncated"] != false {
		t.Fatalf("tail of big file: %d lines, first %.10q, truncated %v", len(got), got[0], e["truncated"])
	}
	if _, ok := e["sha256"]; ok {
		t.Fatal("sha256 reported for a file over the hash limit")
	}

	// Asking for more than max_bytes returns the newest whole lines that fit.
	e = tailEntry(t, r, map[string]any{"paths": []string{path}, "tail_lines": 1000, "max_bytes": 10000})
	content = e["content"].(string)
	lines := strings.Split(strings.TrimSuffix(content, "\n"), "\n")
	if e["truncated"] != true || len(content) > 10000 || len(lines) != 9 || !strings.HasPrefix(lines[8], fmt.Sprintf("%06d ", total)) || len(lines[0]) != 1007 {
		t.Fatalf("budget: truncated=%v bytes=%d lines=%d", e["truncated"], len(content), len(lines))
	}
}

func TestReadTailLinesLongLineAndBinary(t *testing.T) {
	r := newTestRuntime(t)
	dir := t.TempDir()
	long := filepath.Join(dir, "long.txt")
	if err := os.WriteFile(long, []byte("first\n"+strings.Repeat("é", 5000)+"\n"), 0644); err != nil {
		t.Fatal(err)
	}
	e := tailEntry(t, r, map[string]any{"paths": []string{long}, "tail_lines": 1, "max_bytes": 1024})
	content := e["content"].(string)
	if e["truncated"] != true || e["invalid_utf8_replaced"] != false || !strings.HasSuffix(content, "é\n") || strings.Contains(content, "first") || len(content) > 1024 {
		t.Fatalf("long line: %#v", e)
	}
	bin := filepath.Join(dir, "b.bin")
	if err := os.WriteFile(bin, []byte("a\nb\x00c\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if e := tailEntry(t, r, map[string]any{"paths": []string{bin}, "tail_lines": 1}); e["binary"] != true {
		t.Fatalf("binary: %#v", e)
	}
	for _, args := range []map[string]any{
		{"paths": []string{long}, "tail_lines": 1, "start_line": 2},
		{"paths": []string{long}, "tail_lines": 1, "end_line": 2},
		{"paths": []string{long}, "tail_lines": 1, "encoding": "base64"},
		{"paths": []string{long}, "tail_lines": maxTailLines + 1},
	} {
		if _, errText := tryCall(t, r, "read", args); errText == "" {
			t.Fatalf("accepted %v", args)
		}
	}
}
