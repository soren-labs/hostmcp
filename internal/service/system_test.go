package service

import (
	"encoding/xml"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestSystemUnitUsesExplicitAccount(t *testing.T) {
	a := account{name: "developer", home: "/home/developer", shell: "/bin/bash", uid: 1234, gid: 1235}
	body, err := renderSystem("linux", "hostmcp-dev", "agent", "/usr/local/bin/hostmcp", "/home/developer/.config/hostmcp/agent.json", a)
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{"User=1234\n", "Group=1235\n", "WantedBy=multi-user.target", `Environment="HOME=/home/developer"`, `Environment="SHELL=/bin/bash"`, `Environment="USER=developer"`} {
		if !strings.Contains(body, expected) {
			t.Fatalf("system service lost account context %q:\n%s", expected, body)
		}
	}
	for _, restriction := range []string{"NoNewPrivileges", "ProtectHome", "ProtectSystem", "CapabilityBoundingSet", "UMask=0077"} {
		if strings.Contains(body, restriction) {
			t.Fatalf("agent received an extra privilege restriction: %s", restriction)
		}
	}
	rootBody, err := renderSystem("linux", "hostmcp-root", "agent", "/usr/local/bin/hostmcp", "/etc/hostmcp/root.json", account{name: "root", home: "/root", shell: "/bin/bash"})
	if err != nil || !strings.Contains(rootBody, "User=0\nGroup=0\n") {
		t.Fatalf("root account not explicit: %v\n%s", err, rootBody)
	}
}

func TestLaunchDaemonCarriesAccountAndEscapesPaths(t *testing.T) {
	a := account{name: "developer", home: "/Users/developer", shell: "/bin/zsh", uid: 501, gid: 20}
	body, err := renderSystem("darwin", "hostmcp-dev", "agent", "/Applications/A & B/hostmcp", "/Users/developer/config.json", a)
	if err != nil {
		t.Fatal(err)
	}
	decoder := xml.NewDecoder(strings.NewReader(body))
	for {
		_, err := decoder.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	for _, expected := range []string{"A &amp; B", "<key>UserName</key><string>developer</string>", "/Library/Logs/HostMCP/hostmcp-dev/service.log", "<key>SHELL</key><string>/bin/zsh</string>"} {
		if !strings.Contains(body, expected) {
			t.Fatalf("launch daemon lost %q", expected)
		}
	}
}

func TestSystemServiceValidationBeforeMutation(t *testing.T) {
	if err := InstallWithOptions("hostmcp-test", "agent", "missing", Options{User: "root"}); err == nil {
		t.Fatal("--user without --system accepted")
	}
	if err := UninstallWithOptions("hostmcp-test", Options{User: "root"}); err == nil {
		t.Fatal("uninstall accepted irrelevant --user")
	}
	if os.Geteuid() != 0 {
		if err := InstallWithOptions("hostmcp-test", "agent", "missing", Options{System: true}); err == nil || !strings.Contains(err.Error(), "requires root") {
			t.Fatalf("system install did not reject unelevated caller: %v", err)
		}
	}
	for _, name := range []string{"sshd", "../hostmcp-agent", "hostmcp-bad\nUser=root"} {
		if _, err := systemUnitPath("linux", name); err == nil {
			t.Fatalf("accepted non-HostMCP unit name %q", name)
		}
	}
}

func TestSystemdSystemScope(t *testing.T) {
	var calls [][]string
	err := startSystemdScoped("hostmcp-system-test", true, func(_ string, args ...string) error {
		calls = append(calls, args)
		return nil
	}, func(_ string, args ...string) ([]byte, error) {
		calls = append(calls, args)
		return []byte("LoadState=loaded\nActiveState=active\nSubState=running\nResult=success\n"), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, args := range calls {
		if strings.Contains(strings.Join(args, " "), "--user") {
			t.Fatalf("system service used the per-user manager: %v", args)
		}
	}
}

func TestSystemdParsesSystemAccountUnit(t *testing.T) {
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
	dir := t.TempDir()
	body, err := renderSystem("linux", "hostmcp-system-parser", "agent", binary, filepath.Join(dir, "config.json"), account{name: "test", home: dir, shell: "/bin/sh", uid: uint32(os.Getuid()), gid: uint32(os.Getgid())})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "hostmcp-system-parser.service")
	if err = os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(analyze, "verify", "--man=no", path)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("systemd rejected system unit: %v\n%s", err, out)
	}
}

func TestUninstallMissingUserServiceIsIdempotent(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	name := fmt.Sprintf("hostmcp-not-installed-%d", os.Getpid())
	if err := Uninstall(name); err != nil {
		t.Fatalf("uninstall of missing service failed: %v", err)
	}
}

func TestRootServiceRejectsUserOwnedPaths(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "binary")
	if err := os.WriteFile(file, []byte("fixture"), 0777); err != nil {
		t.Fatal(err)
	}
	if err := requireRootOwned(file); err == nil {
		t.Fatal("root service accepted a replaceable executable")
	}
}

func TestLookupRootAccount(t *testing.T) {
	a, err := lookupAccount("root")
	if err != nil {
		t.Fatal(err)
	}
	if a.uid != 0 || a.name != "root" || !filepath.IsAbs(a.home) || !filepath.IsAbs(a.shell) {
		t.Fatalf("invalid native root account: %+v", a)
	}
}

func TestAccountAccessPreservesCurrentCredentials(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "private")
	if err := os.WriteFile(file, []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	a := account{name: "current", uid: uint32(os.Geteuid()), gid: uint32(os.Getegid())}
	// Directory services can report more memberships than Darwin setgroups
	// accepts. No group mutation is needed when checking the current identity.
	for n := 0; n < 64; n++ {
		a.groups = append(a.groups, uint32(n))
	}
	if err := checkAccountAccess(a, "-r", file); err != nil {
		t.Fatal(err)
	}
	if err := checkAccountAccess(a, "-r", filepath.Join(dir, "missing")); err == nil {
		t.Fatal("missing file was reported readable")
	}
}

func TestDarwinRootChecksTargetAccountPermissions(t *testing.T) {
	if runtime.GOOS != "darwin" || os.Geteuid() != 0 || os.Getenv("SUDO_USER") == "" || os.Getenv("SUDO_USER") == "root" {
		t.Skip("requires a macOS administrator running this fixture with sudo")
	}
	a, err := lookupAccount(os.Getenv("SUDO_USER"))
	if err != nil {
		t.Fatal(err)
	}
	// t.TempDir nests under a root-only test parent, which would hide the
	// account-owned fixture before the intended file permission check.
	dir, err := os.MkdirTemp("/private/tmp", "hostmcp-account-access-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	if err = os.Chmod(dir, 0755); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "account-private")
	if err = os.WriteFile(file, []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.Chown(file, int(a.uid), int(a.gid)); err != nil {
		t.Fatal(err)
	}
	if err = checkAccountAccess(a, "-r", file); err != nil {
		t.Fatal(err)
	}
	if err = os.Chown(file, 0, 0); err != nil {
		t.Fatal(err)
	}
	if err = checkAccountAccess(a, "-r", file); err == nil {
		t.Fatal("target user inherited root's access to a root-only file")
	}
}
