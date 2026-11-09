package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/soren-labs/rmc/internal/config"
)

// setLogFormat switches the process log to one JSON object per line. Existing
// "event=name key=value" log lines become {"msg":"name","key":"value"}, so
// call sites keep using the log package.
func setLogFormat(format string) error {
	switch format {
	case "", "text":
		return nil
	case "json":
		log.SetFlags(0)
		log.SetOutput(&jsonLogWriter{logger: slog.New(slog.NewJSONHandler(os.Stderr, nil))})
		return nil
	}
	return errors.New("--log-format must be text or json")
}

type jsonLogWriter struct {
	mu     sync.Mutex
	logger *slog.Logger
}

func (w *jsonLogWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, line := range strings.Split(strings.TrimRight(string(p), "\n"), "\n") {
		msg, attrs := parseLogLine(line)
		w.logger.LogAttrs(context.Background(), slog.LevelInfo, msg, attrs...)
	}
	return len(p), nil
}

// parseLogLine splits "free text; key=value key2=\"quoted\"" into a message
// and attributes. An "event" value becomes the message when there is no
// leading text. Anything it cannot parse is kept verbatim in a "text" field.
func parseLogLine(line string) (string, []slog.Attr) {
	start := firstPair(line)
	msg := strings.TrimRight(strings.TrimSpace(line[:start]), ";:,")
	var attrs []slog.Attr
	rest := line[start:]
	for {
		rest = strings.TrimLeft(rest, " ")
		if rest == "" {
			break
		}
		eq := strings.IndexByte(rest, '=')
		if eq <= 0 || !isLogKey(rest[:eq]) {
			attrs = append(attrs, slog.String("text", rest))
			break
		}
		key := rest[:eq]
		rest = rest[eq+1:]
		var value string
		quoted := strings.HasPrefix(rest, `"`)
		if quoted {
			q, err := strconv.QuotedPrefix(rest)
			if err != nil {
				attrs = append(attrs, slog.String("text", key+"="+rest))
				break
			}
			value, _ = strconv.Unquote(q)
			rest = rest[len(q):]
		} else {
			end := strings.IndexByte(rest, ' ')
			if end < 0 {
				end = len(rest)
			}
			value, rest = rest[:end], rest[end:]
		}
		switch n, err := strconv.ParseInt(value, 10, 64); {
		case key == "event" && msg == "":
			msg = value
		case !quoted && err == nil:
			attrs = append(attrs, slog.Int64(key, n))
		default:
			attrs = append(attrs, slog.String(key, value))
		}
	}
	if msg == "" {
		msg = "log"
	}
	return msg, attrs
}

// firstPair returns the index of the first word that starts with "key=".
func firstPair(line string) int {
	for i := 0; i < len(line); i++ {
		if i > 0 && line[i-1] != ' ' {
			continue
		}
		if eq := strings.IndexByte(line[i:], '='); eq > 0 && isLogKey(line[i:i+eq]) {
			return i
		}
	}
	return len(line)
}

func isLogKey(s string) bool {
	if s == "" || len(s) > 64 {
		return false
	}
	for i, r := range s {
		if !(r == '_' || r >= 'a' && r <= 'z' || i > 0 && r >= '0' && r <= '9') {
			return false
		}
	}
	return true
}

// readInvite reads an invitation from a private file, or from standard input
// when path is "-" so it can be piped from the Hub without a temporary file.
func readInvite(path string, in io.Reader) (config.Invite, error) {
	var i config.Invite
	if path != "-" {
		return i, config.ReadJSON(path, &i)
	}
	b, err := io.ReadAll(io.LimitReader(in, 64<<10+1))
	if err != nil {
		return i, err
	}
	if len(b) > 64<<10 {
		return i, errors.New("invitation on stdin exceeds 64 KiB")
	}
	return i, config.ParseJSON(b, &i)
}

// isTerminal reports whether f is a character device such as a TTY.
func isTerminal(f *os.File) bool {
	info, err := f.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}

type check struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail"`
}

// hubChecks inspects a Hub configuration and, when url is set, a running Hub.
// It never prints secrets: only counts, addresses and versions.
func hubChecks(ctx context.Context, path, url string) []check {
	var checks []check
	add := func(name string, ok bool, format string, args ...any) {
		checks = append(checks, check{name, ok, fmt.Sprintf(format, args...)})
	}
	cfg, err := config.NewStore(path).Snapshot()
	if err != nil {
		add("config", false, "%s: %v", path, err)
	} else {
		add("config", true, "%s is private and valid", path)
		expired := 0
		for _, i := range cfg.Invitations {
			if time.Now().After(i.ExpiresAt) {
				expired++
			}
		}
		add("inventory", true, "%d clients, %d devices, %d pending invitations (%d expired)", len(cfg.Clients), len(cfg.Devices), len(cfg.Invitations), expired)
		identity := "bearer tokens"
		if cfg.OAuth != nil {
			identity += ", external OAuth issuer " + cfg.OAuth.Issuer
		}
		if cfg.CloudflareAccess != nil {
			identity += ", Cloudflare Access"
		}
		add("client_auth", true, "%s", identity)
		if _, _, err := net.SplitHostPort(cfg.Listen); err != nil {
			add("listen", false, "%s: %v", cfg.Listen, err)
		} else {
			add("listen", true, "%s; public MCP endpoint %s/mcp", cfg.Listen, strings.TrimSuffix(cfg.PublicURL, "/"))
		}
	}
	if url == "" {
		return checks
	}
	url = strings.TrimSuffix(strings.TrimSuffix(url, "/"), "/mcp")
	reqCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, url+"/healthz", nil)
	if err != nil {
		add("health", false, "%v", err)
		return checks
	}
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	sent := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		add("health", false, "%s/healthz: %v", url, err)
		return checks
	}
	defer resp.Body.Close()
	var health struct {
		Status      string `json:"status"`
		Version     string `json:"version"`
		Component   string `json:"component"`
		WireVersion int    `json:"wire_version"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&health); err != nil || resp.StatusCode != 200 || health.Status != "ok" {
		add("health", false, "%s/healthz: HTTP %d %s", url, resp.StatusCode, health.Status)
	} else {
		add("health", true, "%s %s, wire version %d", health.Component, health.Version, health.WireVersion)
	}
	// JWT validation (OAuth, Cloudflare Access) depends on agreeing clocks.
	if date, err := http.ParseTime(resp.Header.Get("Date")); err == nil {
		skew := date.Sub(sent.Add(time.Since(sent) / 2)).Round(time.Second)
		add("clock_skew", skew.Abs() <= 30*time.Second, "Hub clock differs from this machine by %v", skew)
	}
	return checks
}
