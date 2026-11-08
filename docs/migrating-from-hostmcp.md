# Migrating from HostMCP

RMC was called HostMCP up to v0.2.4. The rename changes names only: the
Hub↔Agent wire protocol is still version 1, the MCP tools and their arguments
are unchanged, and configuration file formats are the same. A v0.2.x Agent can
connect to an RMC Hub and an RMC Agent can connect to a v0.2.x Hub.

Apart from `RMC_DOMAIN` for the Caddy example (below), nothing has to change on
the day you upgrade. The table shows what was renamed and what RMC still
accepts from earlier installations.

| Item | HostMCP (≤ v0.2.4) | RMC | Still accepted by RMC |
| --- | --- | --- | --- |
| Repository | `github.com/soren-labs/hostmcp` | `github.com/soren-labs/rmc` | GitHub redirects the old URLs |
| Binaries | `hostmcp`, `hostmcp-hub`, `hostmcp-agent` | `rmc`, `rmc-hub`, `rmc-agent` | — |
| Default config directory | `~/.config/hostmcp/` | `~/.config/rmc/` | Used when a file exists only in the old directory |
| Client token variable | `HOSTMCP_TOKEN` | `RMC_TOKEN` | Read when `RMC_TOKEN` is unset |
| Service names | `hostmcp-…` | `rmc-…` | `service install` and `service uninstall` accept `hostmcp-…` names |
| launchd labels | `com.hostmcp.NAME` | `com.rmc.NAME` | Kept for `hostmcp-…` service names |
| macOS service logs | `Library/Logs/HostMCP` | `Library/Logs/RMC` | Kept for `hostmcp-…` service names |
| Agent update archive | `hostmcp-agent_V_OS_ARCH.tar.gz` | `rmc-agent_V_OS_ARCH.tar.gz` | Releases also ship the legacy name (below) |

Explicit `--config` paths are never rewritten, so services that already pass
`--config /var/lib/hostmcp/hub.json` or similar keep using those files.

## Upgrading an existing node

The least disruptive path keeps your service name and config path. The
installer never overwrites an existing unit or plist, so remove the old
service first and then install it again under the same name, scope and
account. Run both steps with the new binary you installed (`rmc-agent` for
an Agent, `rmc-hub` for a Hub, or the all-in-one `rmc`); every role binary
includes the `service` command.

1. Install the new binary, for example to `~/.local/bin/rmc-agent` with
   `scripts/install.sh --role agent`, or to `/usr/local/bin/rmc-agent`.
2. Remove the existing service. RMC recognises the `hostmcp-…` name and the
   `Managed by HostMCP` marker:
   `rmc-agent service uninstall --name hostmcp-agent-NAME`
   (add `--system` for a system service).
3. Install it again with the same name and the existing config file:
   `rmc-agent service install --role agent --name hostmcp-agent-NAME --config EXISTING.json`
   (add `--system --user USER` for a system service). On macOS the service keeps
   its `com.hostmcp.hostmcp-agent-NAME` label and `Library/Logs/HostMCP` log path.
4. Check `list_devices` from an MCP client: the node reports the new version.

Managed sessions on that node end when its Agent restarts. You can switch the
service to a `rmc-…` name and move the config to `~/.config/rmc/` later,
at your own pace.

## Environment variables in deployment files

| File | HostMCP | RMC | What to do |
| --- | --- | --- | --- |
| `deploy/Caddyfile` | `HOSTMCP_DOMAIN` | `RMC_DOMAIN` | Set `RMC_DOMAIN` before reloading Caddy with the new file; Caddy has no fallback between variables |
| `deploy/compose.yml` | `HOSTMCP_VERSION` | `RMC_VERSION` | Optional; `HOSTMCP_VERSION` is still read when `RMC_VERSION` is unset |

## Docker Compose Hub

`deploy/compose.yml` keeps the volume name `hostmcp-state`, so recreating the
container after the upgrade mounts the existing `/state/hub.json` and
credentials.

## Agents that self-update

The opt-in updater in v0.2.x downloads `hostmcp-agent_V_OS_ARCH.tar.gz` and
installs the `hostmcp-agent` file inside it. During the transition, every RMC
release also publishes an archive under that legacy name. It contains the RMC
Agent binary, named `hostmcp-agent`, and is listed in `SHA256SUMS`, so pin its
checksum in your update policy as usual. After that update the node runs RMC
at its existing path, and later updates use the `rmc-agent_…` archives.

## MCP client configuration

The Hub URL, bearer tokens, OAuth settings and Cloudflare Access settings are
unchanged. If you named the connector "HostMCP" in your client, renaming it is
optional; tool names do not depend on the server name.
