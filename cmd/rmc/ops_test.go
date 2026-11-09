package main

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseLogLine(t *testing.T) {
	for _, tc := range []struct {
		line, msg string
		attrs     map[string]any
	}{
		{`event=tool_call principal="alice" device="dev 1" tool="read" request_id="ab" outcome="ok" elapsed_ms=12`, "tool_call",
			map[string]any{"principal": "alice", "device": "dev 1", "tool": "read", "request_id": "ab", "outcome": "ok", "elapsed_ms": int64(12)}},
		{`device dev1 connected; hub_version="0.3.0" wire_version=1`, "device dev1 connected", map[string]any{"hub_version": "0.3.0", "wire_version": int64(1)}},
		{`RMC hub listening on 127.0.0.1:8787; public MCP http://x/mcp`, "RMC hub listening on 127.0.0.1:8787; public MCP http://x/mcp", map[string]any{}},
		{`event=x reason="unterminated`, "x", map[string]any{"text": `reason="unterminated`}},
	} {
		msg, attrs := parseLogLine(tc.line)
		got := map[string]any{}
		for _, a := range attrs {
			got[a.Key] = a.Value.Any()
		}
		if msg != tc.msg || len(got) != len(tc.attrs) {
			t.Fatalf("%q: msg=%q attrs=%v", tc.line, msg, got)
		}
		for k, v := range tc.attrs {
			if got[k] != v {
				t.Fatalf("%q: %s=%#v want %#v", tc.line, k, got[k], v)
			}
		}
	}
}

func TestJSONLogWriterEmitsOneObjectPerLine(t *testing.T) {
	var buf bytes.Buffer
	w := &jsonLogWriter{logger: slog.New(slog.NewJSONHandler(&buf, nil))}
	if _, err := w.Write([]byte("event=device_connected device=\"d1\"\n")); err != nil {
		t.Fatal(err)
	}
	var rec map[string]any
	if err := json.Unmarshal(buf.Bytes(), &rec); err != nil || rec["msg"] != "device_connected" || rec["device"] != "d1" || rec["level"] != "INFO" {
		t.Fatalf("record %s: %v", buf.String(), err)
	}
	if setLogFormat("xml") == nil {
		t.Fatal("unknown log format accepted")
	}
}

func TestInviteToStdoutAndEnrollFromStdin(t *testing.T) {
	path, store := testHub(t)
	out, err := os.Create(filepath.Join(t.TempDir(), "stdout"))
	if err != nil {
		t.Fatal(err)
	}
	original := os.Stdout
	os.Stdout = out
	err = deviceCommand([]string{"invite", "--config", path, "--id", "piped", "--output", "-"})
	os.Stdout = original
	if err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(out.Name())
	invite, err := readInvite("-", bytes.NewReader(b))
	if err != nil || invite.DeviceID != "piped" || len(invite.Token) < 32 {
		t.Fatalf("invite %+v: %v", invite, err)
	}
	if _, err := store.Redeem(invite.DeviceID, invite.Token); err != nil {
		t.Fatalf("piped invitation does not redeem: %v", err)
	}
	if _, err := readInvite("-", strings.NewReader(`{"version":1,"unknown":true}`)); err == nil {
		t.Fatal("stdin invitation with unknown fields accepted")
	}
	if _, err := readInvite("-", strings.NewReader(strings.Repeat(" ", 64<<10+1))); err == nil {
		t.Fatal("oversized stdin invitation accepted")
	}
}

func TestHubChecks(t *testing.T) {
	path, _ := testHub(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "ok", "version": "test", "component": "hub", "wire_version": 1})
	}))
	defer srv.Close()
	checks := hubChecks(context.Background(), path, srv.URL+"/mcp")
	names := map[string]bool{}
	for _, c := range checks {
		names[c.Name] = c.OK
		if !c.OK {
			t.Fatalf("check failed: %+v", c)
		}
		if strings.Contains(c.Detail, "token") && strings.Contains(c.Detail, "hash") {
			t.Fatalf("check leaks credential material: %+v", c)
		}
	}
	for _, want := range []string{"config", "inventory", "client_auth", "listen", "health", "clock_skew"} {
		if _, ok := names[want]; !ok {
			t.Fatalf("missing check %s in %+v", want, checks)
		}
	}
	if err := os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	checks = hubChecks(context.Background(), path, "")
	if checks[0].Name != "config" || checks[0].OK {
		t.Fatalf("world-readable config passed: %+v", checks[0])
	}
}
