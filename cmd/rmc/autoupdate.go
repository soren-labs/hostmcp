package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/soren-labs/rmc/internal/config"
	"github.com/soren-labs/rmc/internal/update"
)

// nodeConfirmWait is how long a freshly updated node may take to reach its
// Hub before the update is rolled back (only while the Hub itself answers).
const nodeConfirmWait = 10 * time.Minute

// hubConfirmAfter is how long a freshly updated Hub must keep serving before
// the update counts as healthy.
const hubConfirmAfter = time.Minute

// selfUpdater runs automatic updates for one long-running service. A nil
// selfUpdater means this process cannot update itself; every method is a no-op.
type selfUpdater struct {
	up      *update.Updater
	pending chan chan error // a restart the serving loop must complete
}

func executablePath() (string, error) {
	binary, err := os.Executable()
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(binary)
}

func newSelfUpdater(configFile string) *selfUpdater {
	binary, err := executablePath()
	if err != nil {
		log.Printf("event=auto_update mode=manual reason=%q", "cannot locate executable: "+err.Error())
		return nil
	}
	up, err := update.New(normalizeRole(component), binary, version, configFile)
	if err != nil {
		log.Printf("event=auto_update mode=manual reason=%q", err.Error())
		return nil
	}
	up.Logf = log.Printf
	if reason := up.Unsupported(); reason != "" {
		log.Printf("event=auto_update mode=manual reason=%q", reason)
		return nil
	}
	// Until the service is serving, nothing needs shutting down first.
	up.Restart = func() error { return reexec(binary) }
	log.Printf("event=auto_update mode=%s settings=%q", up.Mode(), up.SettingsFile)
	return &selfUpdater{up: up}
}

// reexec replaces this process with binary, keeping arguments, environment
// and PID, so systemd and launchd see the same running service.
func reexec(binary string) error {
	return syscall.Exec(binary, append([]string{binary}, os.Args[1:]...), os.Environ())
}

func (s *selfUpdater) mode() string {
	if s == nil {
		return "manual"
	}
	return s.up.Mode()
}

// startup counts the start of a just-installed version and rolls back one
// that keeps failing; it returns the function that confirms health.
func (s *selfUpdater) startup() func() {
	if s == nil {
		return func() {}
	}
	return s.up.Startup()
}

// serve starts the periodic checks. The returned context ends with ctx, or
// when an installed update needs the service to stop for a restart; the
// service then returns through finish.
func (s *selfUpdater) serve(ctx context.Context, idle func() bool) context.Context {
	if s == nil {
		return ctx
	}
	runCtx, cancel := context.WithCancel(ctx)
	requests := make(chan chan error)
	s.pending = make(chan chan error, 1)
	s.up.Idle = idle
	s.up.Restart = func() error {
		reply := make(chan error, 1)
		select {
		case requests <- reply:
			return <-reply
		case <-runCtx.Done():
			return errors.New("service is stopping")
		}
	}
	go func() {
		select {
		case reply := <-requests:
			s.pending <- reply
			cancel()
		case <-runCtx.Done():
		}
	}()
	go s.up.Run(runCtx)
	return runCtx
}

// rollbackUnlessConfirmed watches a just-installed node version.
func (s *selfUpdater) rollbackUnlessConfirmed(ctx context.Context, wait time.Duration, healthy func(context.Context) bool) {
	if s != nil {
		go s.up.RollbackUnlessConfirmed(ctx, wait, healthy)
	}
}

// finish completes a requested restart after the service stopped serving;
// otherwise it returns err unchanged.
func (s *selfUpdater) finish(ctx context.Context, err error, stop func()) error {
	if s == nil || s.pending == nil {
		return err
	}
	select {
	case reply := <-s.pending:
		if ctx.Err() != nil {
			reply <- ctx.Err()
			return err
		}
		stop()
		execErr := reexec(s.up.Binary)
		reply <- execErr
		// Exiting non-zero lets systemd or launchd start the new executable.
		return fmt.Errorf("restart into the updated executable: %w", execErr)
	default:
		return err
	}
}

// hubReachable reports whether the Hub answers /healthz, which tells a node
// that cannot connect whether the problem is its own version.
func hubReachable(a config.Node) func(context.Context) bool {
	return func(ctx context.Context) bool {
		u, err := config.CheckURL(a.HubURL, a.AllowInsecure)
		if err != nil {
			return false
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimSuffix(u.String(), "/")+"/healthz", nil)
		if err != nil {
			return false
		}
		c := &http.Client{Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
		resp, err := c.Do(req)
		if err != nil {
			return false
		}
		resp.Body.Close()
		return resp.StatusCode == http.StatusOK
	}
}

// updateConfigFile is the configuration whose directory holds updates.json.
func updateConfigFile() string {
	switch normalizeRole(component) {
	case "hub":
		return defaultFile("hub.json")
	case "node":
		return nodeConfigFile()
	}
	node := nodeConfigFile()
	if _, err := os.Lstat(node); err != nil {
		if hub := defaultFile("hub.json"); hub != "" {
			if _, err := os.Lstat(hub); err == nil {
				return hub
			}
		}
	}
	return node
}

func autoUpdateCommand(ctx context.Context, args []string) error {
	const help = "use update status, enable, disable, check or channel stable|canary"
	if len(args) == 0 {
		return errors.New(help)
	}
	sub, rest := args[0], args[1:]
	var channel string
	if sub == "channel" {
		if len(rest) == 0 || strings.HasPrefix(rest[0], "-") {
			return errors.New("use update channel stable|canary")
		}
		channel, rest = rest[0], rest[1:]
	}
	f := flags("update " + sub)
	path := f.String("config", updateConfigFile(), "Hub or node configuration; updates.json is kept beside it")
	if err := parse(f, rest); err != nil {
		return err
	}
	settingsFile, stateFile := update.Paths(*path)
	switch sub {
	case "status":
		s, err := update.LoadSettings(settingsFile)
		if err != nil {
			return err
		}
		st, err := update.LoadState(stateFile)
		if err != nil {
			return err
		}
		out := map[string]any{"version": version, "component": normalizeRole(component), "settings_file": settingsFile, "state_file": stateFile, "auto": s.Enabled(), "channel": s.ChannelName(), "state": st}
		if v := os.Getenv("RMC_AUTO_UPDATE"); v != "" {
			out["environment"] = "RMC_AUTO_UPDATE=" + v
		}
		if binary, err := executablePath(); err == nil {
			out["executable"] = binary
			if up, err := update.New(normalizeRole(component), binary, version, *path); err == nil {
				out["mode"] = up.Mode()
				if reason := up.Unsupported(); reason != "" {
					out["manual_reason"] = reason
				}
			}
		}
		return printJSON(out)
	case "enable", "disable", "channel":
		s, err := update.LoadSettings(settingsFile)
		if err != nil {
			return err
		}
		if sub == "channel" {
			s.Channel = channel
		} else {
			on := sub == "enable"
			s.Auto = &on
		}
		if err = update.SaveSettings(settingsFile, s); err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "Automatic updates: %s, saved in %s. Running services apply it at their next check; list_devices shows it after the node restarts.\n", s.Describe(), settingsFile)
		return nil
	case "check":
		binary, err := executablePath()
		if err != nil {
			return err
		}
		up, err := update.New(normalizeRole(component), binary, version, *path)
		if err != nil {
			return err
		}
		res, err := up.Check(ctx, false)
		if err != nil {
			return err
		}
		return printJSON(res)
	default:
		return errors.New(help)
	}
}

func releaseCommand(args []string) error {
	const help = "use release keygen, sign or verify"
	if len(args) == 0 {
		return errors.New(help)
	}
	f := flags("release " + args[0])
	output := f.String("output", "", "file to create")
	keyFile := f.String("key", "", "private release signing key file")
	tag := f.String("tag", "", "release tag, for example v0.3.0")
	sumsFile := f.String("sums", "SHA256SUMS", "SHA256SUMS of the release")
	sigFile := f.String("sig", "SHA256SUMS.sig", "signature to verify")
	publicKey := f.String("public-key", "", "verify with this public key instead of the compiled-in keys")
	if err := parse(f, args[1:]); err != nil {
		return err
	}
	switch args[0] {
	case "keygen":
		if err := absent(*output); err != nil {
			return err
		}
		pub, priv, err := update.GenerateKey()
		if err != nil {
			return err
		}
		file, err := os.OpenFile(*output, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err != nil {
			return err
		}
		if _, err = file.WriteString(priv + "\n"); err == nil {
			err = file.Sync()
		}
		if closeErr := file.Close(); err == nil {
			err = closeErr
		}
		if err != nil {
			return err
		}
		fmt.Println(pub)
		fmt.Fprintf(os.Stderr, "Private key saved in %s; keep an offline backup. Add the public key above to internal/update/keys.go.\n", *output)
		return nil
	case "sign":
		if *keyFile == "" || *tag == "" {
			return errors.New("--key and --tag are required")
		}
		if err := absent(*output); err != nil {
			return err
		}
		st, err := os.Stat(*keyFile)
		if err != nil {
			return err
		}
		if st.Mode().Perm()&0077 != 0 {
			return fmt.Errorf("%s must not be readable by group or others (chmod 600)", *keyFile)
		}
		raw, err := os.ReadFile(*keyFile)
		if err != nil {
			return err
		}
		priv, err := update.ParsePrivateKey(strings.TrimSpace(string(raw)))
		if err != nil {
			return err
		}
		sums, err := os.ReadFile(*sumsFile)
		if err != nil {
			return err
		}
		sig, err := update.Sign(priv, *tag, sums)
		if err != nil {
			return err
		}
		return os.WriteFile(*output, sig, 0644)
	case "verify":
		if *tag == "" {
			return errors.New("--tag is required")
		}
		keys, err := update.TrustedKeys()
		if *publicKey != "" {
			var k update.PublicKey
			k, err = update.ParsePublicKey(*publicKey)
			keys = []update.PublicKey{k}
		}
		if err != nil {
			return err
		}
		sums, err := os.ReadFile(*sumsFile)
		if err != nil {
			return err
		}
		sig, err := os.ReadFile(*sigFile)
		if err != nil {
			return err
		}
		id, err := update.Verify(keys, *tag, sums, sig)
		if err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "%s is signed by release key %s.\n", *tag, id)
		return nil
	default:
		return errors.New(help)
	}
}
