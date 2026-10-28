// HostMCP is a native, self-hosted MCP gateway and device executor.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/soren-labs/hostmcp/internal/agent"
	"github.com/soren-labs/hostmcp/internal/client"
	"github.com/soren-labs/hostmcp/internal/config"
	"github.com/soren-labs/hostmcp/internal/host"
	"github.com/soren-labs/hostmcp/internal/hub"
	"github.com/soren-labs/hostmcp/internal/mcpserver"
	"github.com/soren-labs/hostmcp/internal/protocol"
	"github.com/soren-labs/hostmcp/internal/service"
)

var version = "0.2.2-dev"
var component = "all"
var commit = "development"
var buildTime = "unknown"

const usage = `HostMCP — native Linux/macOS tools, locally or across your devices

  hostmcp stdio
  hostmcp hub init [--config FILE] [--listen HOST:PORT] [--public-url URL]
  hostmcp hub serve [--config FILE]
  hostmcp hub configure [--config FILE] [--public-url URL] [--listen HOST:PORT]
  hostmcp device invite --id NAME --output INVITE.json [--user OS_USER] [--config HUB.json]
  hostmcp device revoke --id NAME [--config HUB.json]
  hostmcp device list [--config HUB.json]
  hostmcp agent enroll --invite INVITE.json [--config AGENT.json]
  hostmcp agent run [--config AGENT.json]
  hostmcp agent update --policy POLICY.json [--archive RELEASE.tar.gz | --rollback]
  hostmcp token create --id NAME --devices '*' --output TOKEN [--config HUB.json]
  hostmcp token list [--config HUB.json]
  hostmcp token update --id NAME --devices NODE1,NODE2 [--config HUB.json]
  hostmcp token rotate --id NAME --output NEW_TOKEN [--config HUB.json]
  hostmcp token revoke --id NAME [--config HUB.json]
  hostmcp connect --url https://host.example/mcp --token-file TOKEN
  hostmcp call --url URL --token-file TOKEN --tool list_devices --args '{}'
  hostmcp service install --role hub|agent --config FILE [--name hostmcp-NAME] [--system --user OS_USER]
  hostmcp service uninstall --name hostmcp-NAME [--system]
  hostmcp doctor
  hostmcp version

Run a subcommand with --help for flags. Secrets are written to private files.
HTTP access requires bearer authentication. Native devices use outbound connections.
`

func defaultFile(name string) string {
	h, err := os.UserHomeDir()
	if err != nil {
		return name
	}
	return filepath.Join(h, ".config", "hostmcp", name)
}
func flags(name string) *flag.FlagSet {
	f := flag.NewFlagSet(name, flag.ContinueOnError)
	f.SetOutput(os.Stderr)
	return f
}
func parse(f *flag.FlagSet, args []string) error {
	if err := f.Parse(args); err != nil {
		return err
	}
	if f.NArg() != 0 {
		return fmt.Errorf("unexpected arguments: %s", strings.Join(f.Args(), " "))
	}
	return nil
}
func printJSON(value any) error {
	e := json.NewEncoder(os.Stdout)
	e.SetIndent("", "  ")
	return e.Encode(value)
}
func absent(path string) error {
	if path == "" {
		return errors.New("an output file is required")
	}
	_, err := os.Lstat(path)
	if err == nil {
		return fmt.Errorf("refusing to overwrite %s", path)
	}
	if !os.IsNotExist(err) {
		return err
	}
	return nil
}

func main() {
	log.SetFlags(log.LstdFlags | log.LUTC)
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := run(ctx, os.Args[1:]); err != nil && !errors.Is(err, flag.ErrHelp) && !errors.Is(err, context.Canceled) {
		fmt.Fprintln(os.Stderr, "hostmcp:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string) error {
	if len(args) == 0 {
		fmt.Print(roleUsage(component))
		return nil
	}
	if err := checkRole(component, args[0]); err != nil {
		return err
	}
	switch args[0] {
	case "help", "--help", "-h":
		fmt.Print(roleUsage(component))
		return nil
	case "version", "--version":
		return printJSON(map[string]any{"version": version, "commit": commit, "build_time": buildTime, "os": runtime.GOOS, "arch": runtime.GOARCH, "component": component, "wire_version": protocol.WireVersion, "config_version": config.FormatVersion})
	case "hub":
		return hubCommand(ctx, args[1:])
	case "device":
		return deviceCommand(args[1:])
	case "token":
		return tokenCommand(args[1:])
	case "agent":
		return agentCommand(ctx, args[1:])
	case "connect", "call":
		return clientCommand(ctx, args[0], args[1:])
	case "service":
		return serviceCommand(args[1:])
	case "doctor":
		return doctor(ctx, args[1:])
	case "stdio":
		f := flags("stdio")
		if err := parse(f, args[1:]); err != nil {
			return err
		}
		rt, err := host.New()
		if err != nil {
			return err
		}
		defer rt.Close()
		s := mcpserver.New(mcpserver.Options{Tools: host.Tools(), Principal: "local", Call: func(c context.Context, p, device, tool string, a json.RawMessage) (protocol.Result, error) {
			return rt.Call(c, p, tool, a)
		}})
		return s.Run(ctx, &mcp.StdioTransport{MaxLineLength: protocol.MaxMessageBytes})
	default:
		return fmt.Errorf("unknown command %q; use hostmcp --help", args[0])
	}
}

func hubCommand(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errors.New("use hub init, hub serve or hub configure")
	}
	f := flags("hub " + args[0])
	path := f.String("config", defaultFile("hub.json"), "hub configuration path")
	switch args[0] {
	case "init":
		listen := f.String("listen", "127.0.0.1:8787", "HTTP listen address")
		public := f.String("public-url", "http://127.0.0.1:8787", "external HTTPS origin, without /mcp")
		tokenPath := f.String("client-token-file", "", "owner token output (default CONFIG.owner-token)")
		insecure := f.Bool("allow-insecure", false, "explicitly allow HTTP on a trusted private test network")
		allowedHosts := f.String("allow-hosts", "", "additional HTTP host[:port] values, comma-separated")
		origins := f.String("allow-origins", "", "additional explicit Origin values, comma-separated")
		if err := parse(f, args[1:]); err != nil {
			return err
		}
		if err := absent(*path); err != nil {
			return err
		}
		url, err := config.CheckURL(*public, *insecure)
		if err != nil {
			return err
		}
		if *tokenPath == "" {
			*tokenPath = *path + ".owner-token"
		}
		if err := absent(*tokenPath); err != nil {
			return err
		}
		token, err := config.NewToken()
		if err != nil {
			return err
		}
		h := config.Hub{Version: 1, Listen: *listen, PublicURL: url.String(), AllowedHosts: commaList(*allowedHosts), AllowedOrigins: commaList(*origins), Clients: []config.Client{{ID: "owner", TokenHash: config.Hash(token), Devices: []string{"*"}}}, Devices: []config.Device{}}
		if err = config.Validate(h); err != nil {
			return err
		}
		if err = config.WriteSecret(*tokenPath, []byte(token+"\n"), false); err != nil {
			return err
		}
		if err = config.WriteJSON(*path, h, false); err != nil {
			_ = os.Remove(*tokenPath)
			return err
		}
		fmt.Fprintf(os.Stderr, "Created hub configuration: %s\nOwner client token: %s (private file)\nMCP endpoint: %s/mcp\n", *path, *tokenPath, h.PublicURL)
		return nil
	case "configure":
		public := f.String("public-url", "", "replace public origin")
		listen := f.String("listen", "", "replace listen address")
		insecure := f.Bool("allow-insecure", false, "allow HTTP for a trusted private test network")
		allowedHosts := f.String("allow-hosts", "", "replace extra allowed hosts, comma-separated")
		origins := f.String("allow-origins", "", "replace extra origins, comma-separated")
		oauthFile := f.String("oauth-file", "", "private JSON external issuer policy; restart Hub after changing discovery settings")
		disableOAuth := f.Bool("disable-oauth", false, "remove OAuth policy; legacy credentials remain valid")
		if err := parse(f, args[1:]); err != nil {
			return err
		}
		if *oauthFile != "" && *disableOAuth {
			return errors.New("choose --oauth-file or --disable-oauth")
		}
		var oauthPolicy *config.OAuth
		if *oauthFile != "" {
			oauthPolicy = new(config.OAuth)
			if err := config.ReadJSON(*oauthFile, oauthPolicy); err != nil {
				return err
			}
		}
		return config.NewStore(*path).Update(func(h *config.Hub) error {
			if oauthPolicy != nil {
				h.OAuth = oauthPolicy
			}
			if *disableOAuth {
				h.OAuth = nil
			}
			if *public != "" {
				u, err := config.CheckURL(*public, *insecure)
				if err != nil {
					return err
				}
				h.PublicURL = u.String()
			}
			if *listen != "" {
				h.Listen = *listen
			}
			setHosts, setOrigins := false, false
			f.Visit(func(v *flag.Flag) {
				if v.Name == "allow-hosts" {
					setHosts = true
				}
				if v.Name == "allow-origins" {
					setOrigins = true
				}
			})
			if setHosts {
				h.AllowedHosts = commaList(*allowedHosts)
			}
			if setOrigins {
				h.AllowedOrigins = commaList(*origins)
			}
			return nil
		})
	case "serve":
		if err := parse(f, args[1:]); err != nil {
			return err
		}
		store := config.NewStore(*path)
		cfg, err := store.Snapshot()
		if err != nil {
			return err
		}
		h, err := hub.New(store, host.Tools())
		if err != nil {
			return err
		}
		defer h.Close()
		srv := &http.Server{Addr: cfg.Listen, Handler: h.Handler(), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 50 * time.Second, IdleTimeout: 90 * time.Second, MaxHeaderBytes: 16 << 10}
		errCh := make(chan error, 1)
		go func() { errCh <- srv.ListenAndServe() }()
		log.Printf("HostMCP hub listening on %s; public MCP %s/mcp", cfg.Listen, cfg.PublicURL)
		select {
		case err := <-errCh:
			if errors.Is(err, http.ErrServerClosed) {
				return nil
			}
			return err
		case <-ctx.Done():
		}
		_ = h.Close()
		shutdown, stop := context.WithTimeout(context.Background(), 10*time.Second)
		defer stop()
		return srv.Shutdown(shutdown)
	default:
		return errors.New("use hub init, hub serve or hub configure")
	}
}

func commaList(raw string) []string {
	var out []string
	for _, s := range strings.Split(raw, ",") {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

func deviceCommand(args []string) error {
	if len(args) == 0 {
		return errors.New("use device invite, device revoke or device list")
	}
	f := flags("device " + args[0])
	path := f.String("config", defaultFile("hub.json"), "hub configuration path")
	id := f.String("id", "", "device ID")
	expectedUser := f.String("user", "", "bind this node to an expected OS username (optional)")
	output := f.String("output", "", "private invitation JSON output file")
	ttl := f.Duration("ttl", 15*time.Minute, "invitation lifetime (maximum 24h)")
	if err := parse(f, args[1:]); err != nil {
		return err
	}
	store := config.NewStore(*path)
	switch args[0] {
	case "invite":
		if err := absent(*output); err != nil {
			return err
		}
		i, err := store.InviteDeviceForUser(*id, *expectedUser, *ttl)
		if err != nil {
			return err
		}
		if err = config.WriteJSON(*output, i, false); err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "Created single-use invitation for %s in %s; expires %s. Transfer it securely to that device.\n", *id, *output, i.ExpiresAt.Format(time.RFC3339))
		return nil
	case "revoke":
		if !config.ValidID(*id) {
			return errors.New("valid --id is required")
		}
		return store.Update(func(h *config.Hub) error {
			found := false
			ds := h.Devices[:0]
			for _, d := range h.Devices {
				if d.ID == *id {
					found = true
				} else {
					ds = append(ds, d)
				}
			}
			h.Devices = ds
			is := h.Invitations[:0]
			for _, i := range h.Invitations {
				if i.DeviceID == *id {
					found = true
				} else {
					is = append(is, i)
				}
			}
			h.Invitations = is
			if !found {
				return errors.New("device or invitation not found")
			}
			return nil
		})
	case "list":
		h, err := store.Snapshot()
		if err != nil {
			return err
		}
		devices := []string{}
		for _, d := range h.Devices {
			devices = append(devices, d.ID)
		}
		invites := []map[string]any{}
		for _, i := range h.Invitations {
			invites = append(invites, map[string]any{"device_id": i.DeviceID, "expires_at": i.ExpiresAt})
		}
		return printJSON(map[string]any{"devices": devices, "invitations": invites})
	default:
		return errors.New("use device invite, device revoke or device list")
	}
}

func tokenCommand(args []string) error {
	if len(args) == 0 {
		return errors.New("use token create, list, update, rotate or revoke")
	}
	f := flags("token " + args[0])
	path := f.String("config", defaultFile("hub.json"), "hub configuration path")
	id := f.String("id", "", "stable client identity")
	devices := f.String("devices", "", "comma-separated device IDs, or '*' for all devices")
	output := f.String("output", "", "private token output file")
	if err := parse(f, args[1:]); err != nil {
		return err
	}
	if args[0] != "list" && !config.ValidID(*id) {
		return errors.New("valid --id is required")
	}
	store := config.NewStore(*path)
	switch args[0] {
	case "list":
		h, err := store.Snapshot()
		if err != nil {
			return err
		}
		type clientInfo struct {
			ID      string   `json:"id"`
			Devices []string `json:"devices"`
		}
		clients := make([]clientInfo, 0, len(h.Clients))
		for _, c := range h.Clients {
			clients = append(clients, clientInfo{ID: c.ID, Devices: c.Devices})
		}
		return printJSON(map[string]any{"clients": clients})
	case "update":
		scopes := commaList(*devices)
		if len(scopes) == 0 {
			return errors.New("--devices is required")
		}
		return store.Update(func(h *config.Hub) error {
			for n := range h.Clients {
				if h.Clients[n].ID == *id {
					h.Clients[n].Devices = scopes
					return nil
				}
			}
			return errors.New("client not found")
		})
	case "rotate":
		if *devices != "" {
			return errors.New("token rotate preserves device access; use token update to change it")
		}
		if err := absent(*output); err != nil {
			return err
		}
		token, err := config.NewToken()
		if err != nil {
			return err
		}
		if err = config.WriteSecret(*output, []byte(token+"\n"), false); err != nil {
			return err
		}
		err = store.Update(func(h *config.Hub) error {
			for n := range h.Clients {
				if h.Clients[n].ID == *id {
					h.Clients[n].TokenHash = config.Hash(token)
					return nil
				}
			}
			return errors.New("client not found")
		})
		if err != nil {
			_ = os.Remove(*output)
			return err
		}
		fmt.Fprintf(os.Stderr, "Rotated client %s; old credential is revoked; new private token file: %s\n", *id, *output)
		return nil
	case "create":
		if err := absent(*output); err != nil {
			return err
		}
		scopes := commaList(*devices)
		if len(scopes) == 0 {
			return errors.New("--devices is required")
		}
		token, err := config.NewToken()
		if err != nil {
			return err
		}
		if err = config.WriteSecret(*output, []byte(token+"\n"), false); err != nil {
			return err
		}
		err = store.Update(func(h *config.Hub) error {
			for _, c := range h.Clients {
				if c.ID == *id {
					return errors.New("client already exists; use token rotate to replace its credential")
				}
			}
			h.Clients = append(h.Clients, config.Client{ID: *id, TokenHash: config.Hash(token), Devices: scopes})
			return nil
		})
		if err != nil {
			_ = os.Remove(*output)
			return err
		}
		fmt.Fprintf(os.Stderr, "Created client %s; private token file: %s\n", *id, *output)
		return nil
	case "revoke":
		return store.Update(func(h *config.Hub) error {
			found := false
			cs := h.Clients[:0]
			for _, c := range h.Clients {
				if c.ID == *id {
					found = true
				} else {
					cs = append(cs, c)
				}
			}
			h.Clients = cs
			if !found {
				return errors.New("client not found")
			}
			return nil
		})
	default:
		return errors.New("use token create, list, update, rotate or revoke")
	}
}

func agentCommand(ctx context.Context, args []string) error {
	if len(args) > 0 && args[0] == "update" {
		return updateCommand(ctx, args[1:])
	}
	if len(args) == 0 {
		return errors.New("use agent enroll, agent run or agent update")
	}
	f := flags("agent " + args[0])
	path := f.String("config", defaultFile("agent.json"), "device agent configuration path")
	invitePath := f.String("invite", "", "single-use invitation JSON")
	if err := parse(f, args[1:]); err != nil {
		return err
	}
	switch args[0] {
	case "enroll":
		if err := absent(*path); err != nil {
			return err
		}
		if *invitePath == "" {
			return errors.New("--invite is required")
		}
		var i config.Invite
		if err := config.ReadJSON(*invitePath, &i); err != nil {
			return err
		}
		if i.Version != 1 || !config.ValidID(i.DeviceID) || len(i.Token) < 32 {
			return errors.New("invalid invitation")
		}
		if time.Now().After(i.ExpiresAt) {
			return errors.New("invitation has expired")
		}
		u, err := config.CheckURL(i.HubURL, i.AllowInsecure)
		if err != nil {
			return err
		}
		body, _ := json.Marshal(map[string]string{"device_id": i.DeviceID, "token": i.Token})
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String()+"/enroll", bytes.NewReader(body))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")
		httpClient := &http.Client{Timeout: 20 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("enrollment redirects are refused") }}
		resp, err := httpClient.Do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			if resp.StatusCode == http.StatusServiceUnavailable {
				return errors.New("enrollment not dispatched: Hub is busy (HTTP 503); retry this invitation after a short delay")
			}
			return fmt.Errorf("enrollment rejected (HTTP %d); check invitation, expiry and hub URL", resp.StatusCode)
		}
		var a config.Agent
		if err = json.NewDecoder(io.LimitReader(resp.Body, 8192)).Decode(&a); err != nil {
			return err
		}
		if a.Version != 1 || a.DeviceID != i.DeviceID || a.HubURL != i.HubURL || a.ExpectedUser != i.ExpectedUser || len(a.Token) < 32 {
			return errors.New("hub returned inconsistent device credentials")
		}
		if err = config.WriteJSON(*path, a, false); err != nil {
			return fmt.Errorf("enrollment completed but saving credentials failed; revoke and invite this device again: %w", err)
		}
		fmt.Fprintf(os.Stderr, "Enrolled %s; credentials saved privately in %s.\n", a.DeviceID, *path)
		return nil
	case "run":
		var a config.Agent
		if err := config.ReadJSON(*path, &a); err != nil {
			return err
		}
		if a.Version != 1 {
			return errors.New("unsupported agent configuration version")
		}
		if _, err := config.CheckURL(a.HubURL, a.AllowInsecure); err != nil {
			return err
		}
		rt, err := host.NewForDevice(a.DeviceID)
		if err != nil {
			return err
		}
		defer rt.Close()
		return agent.Run(ctx, a, rt, rt.Info(a.DeviceID))
	default:
		return errors.New("use agent enroll, agent run or agent update")
	}
}

func clientCommand(ctx context.Context, command string, args []string) error {
	f := flags(command)
	url := f.String("url", "", "remote MCP endpoint")
	tokenFile := f.String("token-file", "", "owner-only bearer file (or HOSTMCP_TOKEN)")
	insecure := f.Bool("allow-insecure", false, "explicitly allow HTTP over a trusted private test network")
	protocolVersion := f.String("protocol-version", "", "MCP protocol version to request (default latest SDK version)")
	tool := f.String("tool", "list_devices", "tool name (call command)")
	arguments := f.String("args", "{}", "tool arguments as a JSON object (call command)")
	list := f.Bool("list-tools", false, "print tool catalog instead of invoking a tool")
	if err := parse(f, args); err != nil {
		return err
	}
	token, err := client.Token(*tokenFile)
	if err != nil {
		return err
	}
	s, err := client.Connect(ctx, *url, token, *protocolVersion, *insecure)
	if err != nil {
		return err
	}
	defer s.Close()
	if command == "connect" {
		return client.Bridge(ctx, s)
	}
	if *list {
		r, err := s.ListTools(ctx, nil)
		if err != nil {
			return err
		}
		return printJSON(r)
	}
	if len(*arguments) > protocol.MaxMessageBytes-4096 {
		return errors.New("arguments exceed size limit")
	}
	var obj map[string]any
	if err = json.Unmarshal([]byte(*arguments), &obj); err != nil || obj == nil {
		return errors.New("--args must be a JSON object")
	}
	r, err := s.CallTool(ctx, &mcp.CallToolParams{Name: *tool, Arguments: obj})
	if err != nil {
		return err
	}
	if err = printJSON(r); err != nil {
		return err
	}
	if r.IsError {
		return errors.New("tool returned an error (see JSON output)")
	}
	return nil
}

func serviceCommand(args []string) error {
	if len(args) == 0 {
		return errors.New("use service install or service uninstall")
	}
	f := flags("service " + args[0])
	role := f.String("role", "", "hub or agent")
	path := f.String("config", "", "absolute configuration path")
	name := f.String("name", "", "service name starting with hostmcp-")
	system := f.Bool("system", false, "install or remove a system service (requires root)")
	user := f.String("user", "", "OS account for a system service; requires --system")
	if err := parse(f, args[1:]); err != nil {
		return err
	}
	switch args[0] {
	case "install":
		if component != "all" && component != *role {
			return errors.New("service role is not supported by this component")
		}
		if *path == "" {
			return errors.New("--config is required")
		}
		return service.InstallWithOptions(*name, *role, *path, service.Options{System: *system, User: *user})
	case "uninstall":
		if *name == "" {
			return errors.New("--name is required")
		}
		return service.UninstallWithOptions(*name, service.Options{System: *system, User: *user})
	default:
		return errors.New("use service install or service uninstall")
	}
}

func doctor(ctx context.Context, args []string) error {
	f := flags("doctor")
	if err := parse(f, args); err != nil {
		return err
	}
	if component == "hub" {
		return printJSON(map[string]any{"version": version, "component": component, "wire_version": protocol.WireVersion, "transport_auth": "legacy bearer and optional external OAuth JWT resource server"})
	}
	rt, err := host.New()
	if err != nil {
		return err
	}
	defer rt.Close()
	cwd, _ := os.Getwd()
	binary, _ := os.Executable()
	timeout, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	sudo := exec.CommandContext(timeout, "sudo", "-n", "true").Run() == nil
	return printJSON(map[string]any{"version": version, "device": rt.Info("local"), "cwd": cwd, "executable": binary, "shell": os.Getenv("SHELL"), "passwordless_sudo_available": sudo, "permission_model": "native OS user; no automatic elevation; macOS privacy and SIP still apply", "component": component, "wire_version": protocol.WireVersion, "transport_auth": "legacy bearer and optional external OAuth JWT resource server; no built-in authorization server"})
}
