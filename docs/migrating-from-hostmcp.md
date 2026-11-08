# Migrating from HostMCP

Roost was called HostMCP up to v0.2.4. The rename changes names only: the
Hub↔Agent wire protocol is still version 1, the MCP tools and their arguments
are unchanged, and configuration file formats are the same. A v0.2.x Agent can
connect to a Roost Hub and a Roost Agent can connect to a v0.2.x Hub.

Nothing has to change on the day you upgrade. The table shows what was renamed
and what Roost still accepts from earlier installations.

| Item | HostMCP (≤ v0.2.4) | Roost | Still accepted by Roost |
| --- | --- | --- | --- |
| Repository | `github.com/soren-labs/hostmcp` | `github.com/soren-labs/roost` | GitHub redirects the old URLs |
| Binaries | `hostmcp`, `hostmcp-hub`, `hostmcp-agent` | `roost`, `roost-hub`, `roost-agent` | — |
| Default config directory | `~/.config/hostmcp/` | `~/.config/roost/` | Used when a file exists only in the old directory |
| Client token variable | `HOSTMCP_TOKEN` | `ROOST_TOKEN` | Read when `ROOST_TOKEN` is unset |
| Service names | `hostmcp-…` | `roost-…` | `service install` and `service uninstall` accept `hostmcp-…` names |
| launchd labels | `com.hostmcp.NAME` | `com.roost.NAME` | Kept for `hostmcp-…` service names |
| macOS service logs | `Library/Logs/HostMCP` | `Library/Logs/Roost` | Kept for `hostmcp-…` service names |
| Agent update archive | `hostmcp-agent_V_OS_ARCH.tar.gz` | `roost-agent_V_OS_ARCH.tar.gz` | Releases also ship the legacy name (below) |

Explicit `--config` paths are never rewritten, so services that already pass
`--config /var/lib/hostmcp/hub.json` or similar keep using those files.

## Upgrading an existing node

The least disruptive path keeps your service name and config path:

1. Install the new binary next to the old one, for example
   `/usr/local/bin/roost-agent`.
2. Re-render the existing service with the same name so it points at the new
   binary: `roost service install --role agent --name hostmcp-agent-NAME --config EXISTING.json`
   (add `--system --user USER` for system services).
3. Check `list_devices` from an MCP client: the node reports the new version.

You can rename the service to `roost-…` and move the config to
`~/.config/roost/` later, at your own pace.

## Agents that self-update

The opt-in updater in v0.2.x downloads `hostmcp-agent_V_OS_ARCH.tar.gz` and
installs the `hostmcp-agent` file inside it. During the transition, every Roost
release also publishes an archive under that legacy name. It contains the Roost
Agent binary, named `hostmcp-agent`, and is listed in `SHA256SUMS`, so pin its
checksum in your update policy as usual. After that update the node runs Roost
at its existing path, and later updates use the `roost-agent_…` archives.

## MCP client configuration

The Hub URL, bearer tokens, OAuth settings and Cloudflare Access settings are
unchanged. If you named the connector "HostMCP" in your client, renaming it is
optional; tool names do not depend on the server name.
