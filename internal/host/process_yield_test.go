package host

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

func yieldCall(t *testing.T, r *Runtime, name string, args map[string]any) map[string]any {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	result, err := r.Call(ctx, "alice", name, raw(t, args))
	if err != nil {
		t.Fatal(err)
	}
	if result.Error != "" {
		t.Fatal(result.Error)
	}
	var out map[string]any
	if err := json.Unmarshal(result.Data, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestExecYieldWaitsPastInitialOutputForExit(t *testing.T) {
	for _, tc := range []struct {
		name  string
		yield int
		code  int
	}{
		{name: "default"},
		{name: "explicit", yield: 2000, code: 7},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newTestRuntime(t)
			args := map[string]any{
				"cmd":   fmt.Sprintf("printf 'starting\\n'; sleep 0.05; printf 'finished\\n' >&2; exit %d", tc.code),
				"shell": "/bin/sh",
			}
			if tc.yield != 0 {
				args["yield_time_ms"] = tc.yield
			}
			out := yieldCall(t, r, "exec_command", args)
			if out["running"] != false || out["exit_code"] != float64(tc.code) || out["output"] != "starting\nfinished\n" {
				t.Fatalf("initial output ended the wait before the final result: %#v", out)
			}
			last := yieldCall(t, r, "write_stdin", map[string]any{"session_id": out["session_id"], "yield_time_ms": 0})
			if last["output"] != "" || last["has_more_output"] != false {
				t.Fatalf("returned output was not drained exactly once: %#v", last)
			}
		})
	}
}

func TestExecYieldDeadlineKeepsCommandRunning(t *testing.T) {
	r := newTestRuntime(t)
	start := time.Now()
	out := yieldCall(t, r, "exec_command", map[string]any{
		"cmd": "printf 'READY\\n'; read -r line; printf '%s\\n' \"$line\"", "shell": "/bin/sh", "yield_time_ms": 150,
	})
	if elapsed := time.Since(start); elapsed < 150*time.Millisecond {
		t.Fatalf("exec returned on output after %s instead of waiting for its yield deadline", elapsed)
	}
	if out["running"] != true || out["output"] != "READY\n" {
		t.Fatalf("yield deadline lost output or stopped the command: %#v", out)
	}
	last, text := untilDone(t, r, "alice", yieldCall(t, r, "write_stdin", map[string]any{"session_id": out["session_id"], "chars": "done\n"}))
	if last["exit_code"] != float64(0) || text != "done\n" {
		t.Fatalf("command did not continue after yielding: %#v %q", last, text)
	}
}

func TestExecYieldZeroAndIdempotentRetry(t *testing.T) {
	r := newTestRuntime(t)
	args := map[string]any{
		"cmd": "printf 'READY\\n'; read -r line; sleep 0.05; printf '%s\\n' \"$line\"", "shell": "/bin/sh",
		"request_id": "yield-retry", "yield_time_ms": 0,
	}
	first := yieldCall(t, r, "exec_command", args)
	if first["running"] != true {
		t.Fatalf("zero yield did not return the live command: %#v", first)
	}
	input := yieldCall(t, r, "write_stdin", map[string]any{"session_id": first["session_id"], "chars": "done\n", "yield_time_ms": 0})
	args["yield_time_ms"] = 2000
	retry := yieldCall(t, r, "exec_command", args)
	if retry["session_id"] != first["session_id"] || retry["replayed"] != true || retry["running"] != false || retry["exit_code"] != float64(0) {
		t.Fatalf("idempotent retry did not wait for the original command: %#v", retry)
	}
	if output := first["output"].(string) + input["output"].(string) + retry["output"].(string); output != "READY\ndone\n" {
		t.Fatalf("retry lost or duplicated output: %q", output)
	}
}

func TestInteractiveWaitReturnsAvailableOutput(t *testing.T) {
	for _, tty := range []bool{false, true} {
		t.Run(fmt.Sprintf("tty=%v", tty), func(t *testing.T) {
			r := newTestRuntime(t)
			yield := 0
			command := "read -r start; printf 'READY\\n'; read -r line; printf 'GOT:%s\\n' \"$line\""
			if tty {
				yield = 30000
				command = "printf 'READY\\n'; read -r line; printf 'GOT:%s\\n' \"$line\""
			}
			out := yieldCall(t, r, "exec_command", map[string]any{
				"cmd": command, "shell": "/bin/sh", "tty": tty, "yield_time_ms": yield,
			})
			if !tty {
				out = yieldCall(t, r, "write_stdin", map[string]any{"session_id": out["session_id"], "chars": "start\n", "yield_time_ms": 30000})
			}
			if out["running"] != true || !strings.Contains(out["output"].(string), "READY") {
				t.Fatalf("interactive prompt was not available before the wait deadline: %#v", out)
			}
			last, text := untilDone(t, r, "alice", yieldCall(t, r, "write_stdin", map[string]any{"session_id": out["session_id"], "chars": "done\n"}))
			if last["exit_code"] != float64(0) || !strings.Contains(text, "GOT:done") {
				t.Fatalf("interactive response was lost: %#v %q", last, text)
			}
		})
	}
}

func TestExecYieldReturnsWhenOutputBudgetFills(t *testing.T) {
	for _, max := range []int{0, 1024} {
		t.Run(fmt.Sprintf("max=%d", max), func(t *testing.T) {
			r := newTestRuntime(t)
			budget := max
			if budget == 0 {
				budget = 65536 // Omitted max_output_bytes uses the existing default.
			}
			args := map[string]any{
				"cmd":   fmt.Sprintf("head -c %d /dev/zero | tr '\\000' x; read -r line", budget+512),
				"shell": "/bin/sh", "yield_time_ms": 30000,
			}
			if max != 0 {
				args["max_output_bytes"] = max
			}
			out := yieldCall(t, r, "exec_command", args)
			if out["running"] != true || out["output"] != strings.Repeat("x", budget) || out["truncated"] != false {
				t.Fatalf("full output budget did not end the wait cleanly: running=%v output_bytes=%v truncated=%v", out["running"], out["output_bytes"], out["truncated"])
			}
			last, text := untilDone(t, r, "alice", yieldCall(t, r, "write_stdin", map[string]any{"session_id": out["session_id"], "chars": "done\n"}))
			if last["exit_code"] != float64(0) || text != strings.Repeat("x", 512) {
				t.Fatalf("output after the first budget was lost: %#v bytes=%d", last, len(text))
			}
		})
	}
	r := newTestRuntime(t)
	if _, err := r.Call(context.Background(), "alice", "exec_command", raw(t, map[string]any{"cmd": ":", "max_output_bytes": 0})); err == nil {
		t.Fatal("explicit zero max_output_bytes bypassed the existing 1024-byte minimum")
	}
}

func TestExecYieldCancellationPreservesBufferedOutput(t *testing.T) {
	r := newTestRuntime(t)
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	_, err := r.Call(ctx, "alice", "exec_command", raw(t, map[string]any{
		"cmd": "printf 'READY\\n'; read -r line; printf '%s\\n' \"$line\"", "shell": "/bin/sh", "yield_time_ms": 30000,
	}))
	if err == nil || !strings.Contains(err.Error(), "session ") {
		t.Fatalf("expected cancelled wait to identify the surviving session: %v", err)
	}
	items := call(t, r, "alice", "list_processes", map[string]any{})["sessions"].([]any)
	if len(items) != 1 {
		t.Fatalf("cancelled wait lost its command: %#v", items)
	}
	id := items[0].(map[string]any)["session_id"]
	out := yieldCall(t, r, "write_stdin", map[string]any{"session_id": id, "yield_time_ms": 0})
	if out["running"] != true || out["output"] != "READY\n" {
		t.Fatalf("cancelled wait consumed output or killed the command: %#v", out)
	}
	last, text := untilDone(t, r, "alice", yieldCall(t, r, "write_stdin", map[string]any{"session_id": id, "chars": "done\n"}))
	if last["exit_code"] != float64(0) || text != "done\n" {
		t.Fatalf("command did not survive cancellation: %#v %q", last, text)
	}
}
