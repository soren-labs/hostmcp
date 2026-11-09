package host

import (
	"context"
	"strings"
	"testing"
	"time"
)

// Few process spawns: some CI and macOS hosts start processes slowly.
const tickingCmd = "printf 'line-0\\n'; sleep 0.3; printf 'line-1\\n'; sleep 0.3; printf 'line-2\\n'"

func TestWriteStdinWaitForExitCollectsTheRest(t *testing.T) {
	for _, tty := range []bool{false, true} {
		r := newTestRuntime(t)
		out := yieldCall(t, r, "exec_command", map[string]any{"cmd": tickingCmd, "shell": "/bin/sh", "tty": tty, "yield_time_ms": 50})
		if out["running"] != true {
			t.Fatalf("tty=%v: command finished too early: %#v", tty, out)
		}
		start := time.Now()
		last := yieldCall(t, r, "write_stdin", map[string]any{"session_id": out["session_id"], "wait_for_exit": true, "yield_time_ms": 2500})
		if last["running"] != false || last["exit_code"] != float64(0) {
			t.Fatalf("tty=%v: wait_for_exit returned before exit after %v: %#v", tty, time.Since(start), last)
		}
		all := out["output"].(string) + last["output"].(string)
		if !strings.Contains(all, "line-0") || !strings.Contains(all, "line-2") {
			t.Fatalf("tty=%v: output incomplete: %q", tty, all)
		}
	}
}

func TestWriteStdinWithoutWaitForExitStillReturnsOnOutput(t *testing.T) {
	r := newTestRuntime(t)
	out := yieldCall(t, r, "exec_command", map[string]any{"cmd": tickingCmd, "shell": "/bin/sh", "yield_time_ms": 50})
	next := yieldCall(t, r, "write_stdin", map[string]any{"session_id": out["session_id"], "yield_time_ms": 2500})
	if next["running"] != true {
		t.Fatalf("default write_stdin waited for exit: %#v", next)
	}
	yieldCall(t, r, "terminate_process", map[string]any{"session_id": out["session_id"], "signal": "KILL"})
}

func TestWaitForExitDeadlineKeepsSessionRunning(t *testing.T) {
	r := newTestRuntime(t)
	out := yieldCall(t, r, "exec_command", map[string]any{"cmd": "printf a; sleep 5", "shell": "/bin/sh", "yield_time_ms": 0})
	next := yieldCall(t, r, "write_stdin", map[string]any{"session_id": out["session_id"], "wait_for_exit": true, "yield_time_ms": 300})
	if next["running"] != true || next["output"] != "a" {
		t.Fatalf("deadline result: %#v", next)
	}
	yieldCall(t, r, "terminate_process", map[string]any{"session_id": out["session_id"], "signal": "KILL"})
}

// About 250 KB of output whose last line is the one that matters.
const bigOutputCmd = "yes compiling-module | head -n 12000; echo 'error: link failed'"

func TestOutputModes(t *testing.T) {
	r := newTestRuntime(t)
	run := func(mode string) map[string]any {
		return yieldCall(t, r, "exec_command", map[string]any{"cmd": bigOutputCmd, "shell": "/bin/sh", "max_output_bytes": 4096, "output_mode": mode, "yield_time_ms": 2500})
	}
	stream := run("stream")
	if stream["has_more_output"] != true || strings.Contains(stream["output"].(string), "error:") {
		t.Fatalf("stream changed behavior: has_more=%v", stream["has_more_output"])
	}
	if _, ok := stream["skipped_bytes"]; ok {
		t.Fatal("stream mode reports skipped_bytes")
	}
	tail := run("tail")
	text := tail["output"].(string)
	if tail["running"] != false || tail["has_more_output"] != false || !strings.HasSuffix(text, "error: link failed\n") || !strings.HasPrefix(text, "[... ") {
		t.Fatalf("tail: running=%v has_more=%v output=%q", tail["running"], tail["has_more_output"], text[:min(len(text), 80)])
	}
	skipped := tail["skipped_bytes"].(float64)
	total := float64(12000*len("compiling-module\n") + len("error: link failed\n"))
	if returned := tail["output_bytes"].(float64); returned > 4096 || skipped+returned != total {
		t.Fatalf("tail accounting: returned=%v skipped=%v", returned, skipped)
	}
	headTail := run("head_tail")
	text = headTail["output"].(string)
	if !strings.HasPrefix(text, "compiling-module\n") || !strings.Contains(text, " bytes skipped ...]\n") || !strings.HasSuffix(text, "error: link failed\n") {
		t.Fatalf("head_tail output: %q ... %q", text[:min(len(text), 40)], text[max(0, len(text)-40):])
	}
	// The skipped bytes are discarded, not left for the next call.
	rest := yieldCall(t, r, "write_stdin", map[string]any{"session_id": headTail["session_id"], "yield_time_ms": 0})
	if rest["output"] != "" || rest["has_more_output"] != false {
		t.Fatalf("head_tail left output behind: %#v", rest)
	}
	// Output within budget is identical in every mode.
	for _, mode := range []string{"stream", "tail", "head_tail"} {
		small := yieldCall(t, r, "exec_command", map[string]any{"cmd": "printf 'one\\ntwo\\n'", "shell": "/bin/sh", "output_mode": mode})
		if small["output"] != "one\ntwo\n" {
			t.Fatalf("%s small output: %q", mode, small["output"])
		}
	}
}

func TestOutputModeRejectsUnknownValue(t *testing.T) {
	r := newTestRuntime(t)
	result, err := r.Call(context.Background(), "alice", "exec_command", raw(t, map[string]any{"cmd": "true", "output_mode": "middle"}))
	if err == nil && result.Error == "" {
		t.Fatal("unknown output_mode accepted")
	}
}

func TestCleanANSIOnPTYProgressBar(t *testing.T) {
	r := newTestRuntime(t)
	cmd := `for p in 10 50 100; do printf '\r\033[2K\033[32mdownloading %s%%\033[0m' $p; done; printf '\ndone\n'`
	out := yieldCall(t, r, "exec_command", map[string]any{"cmd": cmd, "shell": "/bin/sh", "tty": true, "clean_ansi": true, "yield_time_ms": 2000})
	text := out["output"].(string)
	for out["running"] == true {
		next := yieldCall(t, r, "write_stdin", map[string]any{"session_id": out["session_id"], "clean_ansi": true, "wait_for_exit": true, "yield_time_ms": 2000})
		text += next["output"].(string)
		out = next
	}
	// Each call cleans the chunk it returns, so a line redrawn across two
	// calls appears once per call: the split point depends on the OS and
	// timing. Whatever the split, no control bytes remain and the final
	// state of the bar ends the bar's line.
	if strings.ContainsAny(text, "\x1b\r") || !strings.HasSuffix(text, "downloading 100%\ndone\n") || strings.Count(text, "\n") != 2 {
		t.Fatalf("clean output %q", text)
	}
}

func TestCleanTerminalText(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"plain\ttext\n", "plain\ttext\n"},
		{"\x1b[31mred\x1b[0m and \x1b[1;4mbold\x1b[m", "red and bold"},
		{"\r\x1b[2K 10%\r\x1b[2K 50%\r\x1b[2K100%\ndone\n", "100%\ndone\n"},
		{"a\r\nb\r\n", "a\nb\n"},
		{"abc\b\bX", "aX"},
		{"Done\r", "Done"},
		{"\x1b]0;window title\x07after", "after"},
		{"\x1b]8;;https://example.com\x1b\\link\x1b]8;;\x1b\\", "link"},
		{"\x1b(Bascii\x1b=\x07bell", "asciibell"},
		{"中文\x1b[1m粗\x1b[0m", "中文粗"},
		{"\xc2\x9b31mc1\xc2\x9b0m", "c1"},
		{"cut \x1b[3", "cut "},
		{"cut \x1b]0;unterminated", "cut "},
	} {
		if got := cleanTerminalText(tc.in); got != tc.want {
			t.Errorf("cleanTerminalText(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
