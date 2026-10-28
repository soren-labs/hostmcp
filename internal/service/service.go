// Package service installs native user and system background services.
package service

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/soren-labs/hostmcp/internal/config"
)

const marker = "Managed by HostMCP"

func unitQuote(s string) string {
	s = strings.ReplaceAll(s, "%", "%%")
	s = strings.ReplaceAll(s, "\\", "\\\\")
	s = strings.ReplaceAll(s, "\"", "\\\"")
	return "\"" + s + "\""
}

// WorkingDirectory= accepts a path, not a quoted command-line word. Its parser
// expands percent specifiers but does not unquote or unescape backslashes. A /.
// suffix protects terminal whitespace or a backslash from INI trimming/continuation.
func unitDirectory(path string) string {
	value := strings.ReplaceAll(path, "%", "%%")
	if strings.TrimRight(value, " \t\v\f") != value || strings.HasSuffix(value, "\\") {
		value += "/."
	}
	return value
}

func xmlText(s string) string {
	var b bytes.Buffer
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}

func Render(goos, name, role, binary, configPath, home, path string) (string, error) {
	content, err := renderBase(goos, name, role, binary, configPath, home, path)
	if role == "agent" {
		content = strings.ReplaceAll(content, "UMask=0077\n", "")
	}
	return content, err
}

func renderBase(goos, name, role, binary, configPath, home, path string) (string, error) {
	if !config.ValidID(name) || !strings.HasPrefix(name, "hostmcp-") {
		return "", errors.New("service name must start with hostmcp- and use letters, digits, dots, underscores or hyphens")
	}
	if role != "agent" && role != "hub" {
		return "", errors.New("role must be hub or agent")
	}
	for _, value := range []string{binary, configPath, home, path} {
		if strings.ContainsAny(value, "\r\n\x00") {
			return "", errors.New("service paths and PATH cannot contain control characters")
		}
	}
	action := "run"
	if role == "hub" {
		action = "serve"
	}
	if goos == "linux" {
		if !filepath.IsAbs(home) {
			return "", errors.New("service working directory must be an absolute path")
		}
		// The agent intentionally has no filesystem/privilege sandbox: it acts as
		// this user's normal account. Only the optional Hub container is isolated.
		return fmt.Sprintf("# %s\n[Unit]\nDescription=HostMCP %s\nAfter=network-online.target\n\n[Service]\nType=exec\nExecStart=%s %s %s --config %s\nWorkingDirectory=%s\nEnvironment=%s\nEnvironment=%s\nRestart=on-failure\nRestartSec=3\nTimeoutStopSec=15\nKillMode=mixed\nUMask=0077\n\n[Install]\nWantedBy=default.target\n", marker, role, unitQuote(binary), role, action, unitQuote(configPath), unitDirectory(home), unitQuote("HOME="+home), unitQuote("PATH="+path)), nil
	}
	if goos == "darwin" {
		label := "com.hostmcp." + name
		logdir := filepath.Join(home, "Library", "Logs", "HostMCP")
		return fmt.Sprintf("<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n<!DOCTYPE plist PUBLIC \"-//Apple//DTD PLIST 1.0//EN\" \"http://www.apple.com/DTDs/PropertyList-1.0.dtd\">\n<!-- %s -->\n<plist version=\"1.0\"><dict>\n<key>Label</key><string>%s</string>\n<key>ProgramArguments</key><array><string>%s</string><string>%s</string><string>%s</string><string>--config</string><string>%s</string></array>\n<key>WorkingDirectory</key><string>%s</string>\n<key>EnvironmentVariables</key><dict><key>HOME</key><string>%s</string><key>PATH</key><string>%s</string></dict>\n<key>RunAtLoad</key><true/>\n<key>KeepAlive</key><dict><key>SuccessfulExit</key><false/></dict>\n<key>ThrottleInterval</key><integer>3</integer>\n<key>ExitTimeOut</key><integer>15</integer>\n<key>StandardOutPath</key><string>%s</string>\n<key>StandardErrorPath</key><string>%s</string>\n</dict></plist>\n", marker, xmlText(label), xmlText(binary), role, action, xmlText(configPath), xmlText(home), xmlText(home), xmlText(path), xmlText(filepath.Join(logdir, name+".log")), xmlText(filepath.Join(logdir, name+".log"))), nil
	}
	return "", errors.New("native services support Linux and macOS")
}

func unitPath(name string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	if !config.ValidID(name) || !strings.HasPrefix(name, "hostmcp-") {
		return "", errors.New("invalid HostMCP service name")
	}
	if runtime.GOOS == "linux" {
		base := os.Getenv("XDG_CONFIG_HOME")
		if base == "" {
			base = filepath.Join(home, ".config")
		}
		return filepath.Join(base, "systemd", "user", name+".service"), nil
	}
	if runtime.GOOS == "darwin" {
		return filepath.Join(home, "Library", "LaunchAgents", "com.hostmcp."+name+".plist"), nil
	}
	return "", errors.New("native services support Linux and macOS")
}

func withShell(goos, body, shell string) (string, error) {
	if shell == "" {
		shell = "/bin/sh"
	}
	if !filepath.IsAbs(shell) || strings.ContainsAny(shell, "\r\n\x00") {
		return "", errors.New("SHELL must name an absolute executable path")
	}
	if goos == "linux" {
		return strings.Replace(body, "\nRestart=", "\nEnvironment="+unitQuote("SHELL="+shell)+"\nRestart=", 1), nil
	}
	return strings.Replace(body, "<key>PATH</key>", "<key>SHELL</key><string>"+xmlText(shell)+"</string><key>PATH</key>", 1), nil
}

type commandRun func(string, ...string) error
type commandQuery func(string, ...string) ([]byte, error)

func query(name string, args ...string) ([]byte, error) {
	cmd := exec.Command(name, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return out, fmt.Errorf("%s: %w: %s", name, err, strings.TrimSpace(string(out)))
	}
	return out, nil
}

func run(name string, args ...string) error {
	out, err := query(name, args...)
	if err != nil {
		return err
	}
	if len(out) != 0 {
		fmt.Fprint(os.Stderr, string(out))
	}
	return nil
}

func startSystemd(name string, run commandRun, query commandQuery) error {
	return startSystemdScoped(name, false, run, query)
}

func systemdArgs(system bool, args ...string) []string {
	if system {
		return args
	}
	return append([]string{"--user"}, args...)
}

func startSystemdScoped(name string, system bool, run commandRun, query commandQuery) error {
	if err := run("systemctl", systemdArgs(system, "daemon-reload")...); err != nil {
		return err
	}
	if err := run("systemctl", systemdArgs(system, "enable", "--now", name+".service")...); err != nil {
		return err
	}
	// Type=exec waits for chdir/exec failures. Give immediate configuration or bind
	// failures a chance to settle before checking the actual manager state too.
	time.Sleep(150 * time.Millisecond)
	out, err := query("systemctl", systemdArgs(system, "show", "--no-pager", "--property=LoadState,ActiveState,SubState,Result", name+".service")...)
	if err != nil {
		return fmt.Errorf("cannot verify service startup: %w", err)
	}
	state := make(map[string]string)
	for _, line := range strings.Split(string(out), "\n") {
		key, value, ok := strings.Cut(line, "=")
		if ok {
			state[key] = value
		}
	}
	if state["LoadState"] != "loaded" || state["ActiveState"] != "active" || state["SubState"] != "running" {
		scope := "--user "
		if system {
			scope = ""
		}
		return fmt.Errorf("service did not start (load=%s, active=%s, sub=%s, result=%s); inspect journalctl %s-u %s.service", state["LoadState"], state["ActiveState"], state["SubState"], state["Result"], scope, name)
	}
	return nil
}

func launchdAbsent(err error) bool {
	var status interface{ ExitCode() int }
	if !errors.As(err, &status) {
		return false
	}
	// launchctl error 112/113: specified domain/service not found. In particular,
	// permission errors and an unavailable launchctl must never authorize deletion.
	return status.ExitCode() == 112 || status.ExitCode() == 113
}

func stopLaunchd(target string, run commandRun, query commandQuery) error {
	_, err := query("launchctl", "print", target)
	if launchdAbsent(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("cannot confirm whether the service is loaded: %w", err)
	}
	if err = run("launchctl", "bootout", target); err != nil {
		// A concurrent unload can race bootout; only an explicit absent result from
		// a fresh query makes it safe to remove the generated file after an error.
		_, checkErr := query("launchctl", "print", target)
		if launchdAbsent(checkErr) {
			return nil
		}
		return fmt.Errorf("service unload failed; keeping its plist: %w", err)
	}
	return nil
}

func Install(name, role, configPath string) error {
	if name == "" {
		name = "hostmcp-" + role
	}
	abs, err := filepath.Abs(configPath)
	if err != nil {
		return err
	}
	if role == "hub" {
		if _, err = config.NewStore(abs).Snapshot(); err != nil {
			return err
		}
	} else if role == "agent" {
		var a config.Agent
		if err = config.ReadJSON(abs, &a); err != nil {
			return err
		}
		if a.Version != 1 || !config.ValidID(a.DeviceID) || len(a.Token) < 32 {
			return errors.New("invalid agent configuration")
		}
		if _, err = config.CheckURL(a.HubURL, a.AllowInsecure); err != nil {
			return err
		}
	} else {
		return errors.New("role must be hub or agent")
	}
	binary, err := os.Executable()
	if err != nil {
		return err
	}
	binary, err = filepath.EvalSymlinks(binary)
	if err != nil {
		return err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	content, err := Render(runtime.GOOS, name, role, binary, abs, home, os.Getenv("PATH"))
	if err != nil {
		return err
	}
	content, err = withShell(runtime.GOOS, content, os.Getenv("SHELL"))
	if err != nil {
		return err
	}
	path, err := unitPath(name)
	if err != nil {
		return err
	}
	if err = config.WriteSecret(path, []byte(content), false); err != nil {
		return err
	}
	if runtime.GOOS == "linux" {
		err = startSystemd(name, run, query)
	} else {
		if err = os.MkdirAll(filepath.Join(home, "Library", "Logs", "HostMCP"), 0700); err != nil {
			return err
		}
		domain := "gui/" + strconv.Itoa(os.Getuid())
		if err = run("launchctl", "enable", domain+"/com.hostmcp."+name); err == nil {
			err = run("launchctl", "bootstrap", domain, path)
		}
	}
	if err != nil {
		return fmt.Errorf("service file saved at %s, but activation failed: %w", path, err)
	}
	fmt.Fprintf(os.Stderr, "Installed %s (%s).\n", name, path)
	if runtime.GOOS == "linux" {
		fmt.Fprintln(os.Stderr, "For operation after logout, enable this user's systemd linger if appropriate: loginctl enable-linger USER.")
	}
	return nil
}

func Uninstall(name string) error {
	path, err := unitPath(name)
	if err != nil {
		return err
	}
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if !strings.Contains(string(b), marker) {
		return errors.New("refusing to remove a service not generated by HostMCP")
	}
	if runtime.GOOS == "linux" {
		if err = run("systemctl", "--user", "disable", "--now", name+".service"); err != nil {
			return err
		}
	} else {
		target := "gui/" + strconv.Itoa(os.Getuid()) + "/com.hostmcp." + name
		if err = stopLaunchd(target, run, query); err != nil {
			return err
		}
	}
	if err = os.Remove(path); err != nil {
		return err
	}
	if runtime.GOOS == "linux" {
		return run("systemctl", "--user", "daemon-reload")
	}
	return nil
}
