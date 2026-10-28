package service

import (
	"encoding/xml"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestRenderPreservesLiteralPaths(t *testing.T) {
	x, err := Render("linux", "hostmcp-agent", "agent", "/home/test/My tools/hostmcp", "/home/test/100% safe/config.json", "/home/test", "/bin:/opt/a b/bin")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(x, `"/home/test/100%% safe/config.json"`) {
		t.Fatal("systemd percent was not escaped")
	}
	if !strings.Contains(x, "\nWorkingDirectory=/home/test\n") || !strings.Contains(x, "\nType=exec\n") {
		t.Fatal("working directory was quoted or exec failure detection was disabled")
	}
	if strings.Contains(x, "NoNewPrivileges") || strings.Contains(x, "ProtectHome") || strings.Contains(x, "UMask=0077") {
		t.Fatal("agent's normal user privileges restricted")
	}
	p, err := Render("darwin", "hostmcp-agent", "agent", "/Users/test/A & B/hostmcp", "/Users/test/cfg.json", "/Users/test", "/bin")
	if err != nil {
		t.Fatal(err)
	}
	d := xml.NewDecoder(strings.NewReader(p))
	for {
		_, err := d.Token()
		if err != nil {
			if err.Error() != "EOF" {
				t.Fatal(err)
			}
			break
		}
	}
	if !strings.Contains(p, "A &amp; B") {
		t.Fatal("plist path not escaped")
	}
	if _, err := Render("linux", "sshd", "hub", "/bin/hostmcp", "/tmp/a", "/home/test", "/bin"); err == nil {
		t.Fatal("unrelated service name accepted")
	}
}

func TestReviewSystemdExecEscapesDollarLiterals(t *testing.T) {
	unit, err := Render("linux", "hostmcp-audit", "agent", "/bin/$BIN", "/tmp/${HOME} $$ spaced%path.json", "/home/a", "/bin")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(unit, `ExecStart="/bin/$$BIN" agent run --config "/tmp/$${HOME} $$$$ spaced%%path.json"`) {
		t.Fatalf("systemd ExecStart would expand a literal configuration filename:\n%s", unit)
	}
	if !strings.Contains(unit, `Environment="HOME=/home/a"`) {
		t.Fatal("normal Environment= syntax was corrupted")
	}
}

func TestServiceCarriesLoginShell(t *testing.T) {
	unit, _ := Render("linux", "hostmcp-agent", "agent", "/bin/hostmcp", "/tmp/cfg", "/home/test", "/bin")
	unit, err := withShell("linux", unit, "/bin/bash")
	if err != nil || !strings.Contains(unit, `Environment="SHELL=/bin/bash"`) {
		t.Fatal("service lost SHELL")
	}
	plist, _ := Render("darwin", "hostmcp-agent", "agent", "/bin/hostmcp", "/tmp/cfg", "/Users/test", "/bin")
	plist, err = withShell("darwin", plist, "/bin/zsh")
	if err != nil || !strings.Contains(plist, "<key>SHELL</key><string>/bin/zsh</string>") {
		t.Fatal("LaunchAgent lost SHELL")
	}
}

// This invokes systemd's real parser without loading or executing a service.
// The debug dump lets us assert the resulting path, not just accepted syntax.
func TestSystemdParsesLiteralWorkingDirectory(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("requires Linux systemd parser")
	}
	analyze, err := exec.LookPath("systemd-analyze")
	if err != nil {
		t.Skip("systemd-analyze is not installed")
	}
	binary, err := exec.LookPath("true")
	if err != nil {
		t.Fatal(err)
	}
	for _, suffix := range []string{"plain", "space 100% 'quote' \"double\" \\literal", "trailing space ", "trailing\\"} {
		t.Run(suffix, func(t *testing.T) {
			dir := t.TempDir()
			home := filepath.Join(dir, suffix)
			if err := os.Mkdir(home, 0700); err != nil {
				t.Fatal(err)
			}
			body, err := Render("linux", "hostmcp-service-audit", "agent", binary, filepath.Join(home, "config.json"), home, "/usr/bin:/bin")
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, "hostmcp-service-audit.service")
			if err := os.WriteFile(path, []byte(body), 0600); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command(analyze, "--user", "verify", "--man=no", path)
			cmd.Env = append(os.Environ(), "SYSTEMD_LOG_LEVEL=debug")
			if os.Getenv("XDG_RUNTIME_DIR") == "" {
				cmd.Env = append(cmd.Env, "XDG_RUNTIME_DIR="+dir)
			}
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("systemd rejected generated unit: %v\n%s", err, out)
			}
			for _, line := range strings.Split(string(out), "\n") {
				if strings.TrimLeft(line, " \t") == "WorkingDirectory: "+home {
					return
				}
			}
			t.Fatalf("systemd did not preserve the literal working directory %q\n%s", home, out)
		})
	}
}

func TestSystemdStartupChecksManagerState(t *testing.T) {
	for _, tc := range []struct {
		name, state string
		queryErr    error
		wantError   bool
	}{
		{"running", "LoadState=loaded\nActiveState=active\nSubState=running\nResult=success\n", nil, false},
		{"invalid-unit-after-successful-enable", "LoadState=bad-setting\nActiveState=inactive\nSubState=dead\nResult=success\n", nil, true},
		{"failed-process", "LoadState=loaded\nActiveState=failed\nSubState=failed\nResult=exit-code\n", nil, true},
		{"restart-loop", "LoadState=loaded\nActiveState=activating\nSubState=auto-restart\nResult=exit-code\n", nil, true},
		{"missing-state", "", nil, true},
		{"manager-unavailable", "", errors.New("user manager unavailable"), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			commands := 0
			err := startSystemd("hostmcp-audit", func(string, ...string) error {
				commands++
				return nil // enable --now can exit zero despite a failed unit.
			}, func(string, ...string) ([]byte, error) {
				return []byte(tc.state), tc.queryErr
			})
			if (err != nil) != tc.wantError || commands != 2 {
				t.Fatalf("commands=%d, startup error=%v, want error=%v", commands, err, tc.wantError)
			}
		})
	}
}

type testExitStatus int

func (s testExitStatus) Error() string { return fmt.Sprintf("exit status %d", s) }
func (s testExitStatus) ExitCode() int { return int(s) }

func TestLaunchdCleanupRequiresConfirmedAbsence(t *testing.T) {
	for _, tc := range []struct {
		name      string
		queries   []error
		bootErr   error
		wantBoot  bool
		wantError bool
	}{
		{"never-loaded", []error{testExitStatus(113)}, nil, false, false},
		{"no-gui-domain", []error{testExitStatus(112)}, nil, false, false},
		{"permission-denied", []error{testExitStatus(1)}, nil, false, true},
		{"unknown-query-failure", []error{testExitStatus(5)}, nil, false, true},
		{"loaded", []error{nil}, nil, true, false},
		{"unload-failed-still-loaded", []error{nil, nil}, testExitStatus(5), true, true},
		{"unload-failed-state-unknown", []error{nil, testExitStatus(1)}, testExitStatus(5), true, true},
		{"concurrently-unloaded", []error{nil, testExitStatus(113)}, testExitStatus(5), true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			queried, booted := 0, false
			err := stopLaunchd("gui/501/com.hostmcp.hostmcp-audit", func(_ string, args ...string) error {
				if len(args) != 2 || args[0] != "bootout" {
					t.Fatal("unexpected launchctl mutation", args)
				}
				booted = true
				return tc.bootErr
			}, func(_ string, args ...string) ([]byte, error) {
				if len(args) != 2 || args[0] != "print" || queried >= len(tc.queries) {
					t.Fatal("unexpected launchctl query", args)
				}
				err := tc.queries[queried]
				queried++
				return nil, err
			})
			if (err != nil) != tc.wantError || booted != tc.wantBoot {
				t.Fatalf("booted=%v, error=%v; want boot=%v, error=%v", booted, err, tc.wantBoot, tc.wantError)
			}
		})
	}
}

func TestUninstallNeverLoadedLaunchAgent(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("requires macOS launchctl")
	}
	// Only a generated file below the private test HOME is created. This service
	// is never bootstrapped; print only confirms this unique label is absent.
	t.Setenv("HOME", t.TempDir())
	name := fmt.Sprintf("hostmcp-unloaded-audit-%d", time.Now().UnixNano())
	path, err := unitPath(name)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("<!-- "+marker+" -->\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := Uninstall(name); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("unloaded generated plist was retained: %v", err)
	}
}
