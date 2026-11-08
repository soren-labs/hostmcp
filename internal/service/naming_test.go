package service

import (
	"strings"
	"testing"
)

func TestLegacyServiceNamesKeepTheirLaunchdIdentity(t *testing.T) {
	cases := []struct {
		name, label, logs string
		valid             bool
	}{
		{"roost-agent", "com.roost.roost-agent", "Roost", true},
		{"hostmcp-agent-gcp-hub", "com.hostmcp.hostmcp-agent-gcp-hub", "HostMCP", true},
		{"agent", "", "", false},
		{"roost-", "", "", true},
		{"other-agent", "", "", false},
		{"roost-../x", "", "", false},
	}
	for _, c := range cases {
		if got := validName(c.name); got != c.valid {
			t.Errorf("validName(%q) = %v, want %v", c.name, got, c.valid)
		}
		if c.label == "" {
			continue
		}
		if got := launchdLabel(c.name); got != c.label {
			t.Errorf("launchdLabel(%q) = %q, want %q", c.name, got, c.label)
		}
		if got := logDirName(c.name); got != c.logs {
			t.Errorf("logDirName(%q) = %q, want %q", c.name, got, c.logs)
		}
	}
}

func TestRenderLegacyLaunchdServiceKeepsLabelAndLogs(t *testing.T) {
	body, err := renderBase("darwin", "hostmcp-agent", "agent", "/usr/local/bin/roost-agent", "/Users/a/.config/hostmcp/agent.json", "/Users/a", "/usr/bin:/bin")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"<string>com.hostmcp.hostmcp-agent</string>", "/Users/a/Library/Logs/HostMCP/hostmcp-agent.log", marker} {
		if !strings.Contains(body, want) {
			t.Errorf("legacy plist missing %q:\n%s", want, body)
		}
	}
	body, err = renderBase("darwin", "roost-agent", "agent", "/usr/local/bin/roost-agent", "/Users/a/.config/roost/agent.json", "/Users/a", "/usr/bin:/bin")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"<string>com.roost.roost-agent</string>", "/Users/a/Library/Logs/Roost/roost-agent.log"} {
		if !strings.Contains(body, want) {
			t.Errorf("plist missing %q:\n%s", want, body)
		}
	}
}

func TestGeneratedRecognisesBothMarkers(t *testing.T) {
	if !generated("# Managed by Roost\n") || !generated("<!-- Managed by HostMCP -->") {
		t.Fatal("generated() must accept current and legacy markers")
	}
	if generated("[Unit]\nDescription=someone else\n") {
		t.Fatal("generated() accepted a foreign unit")
	}
}
