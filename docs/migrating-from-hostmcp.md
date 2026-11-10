# Migrating from HostMCP

RMC was called HostMCP up to v0.2.4. The rename changes names only: the
Hub↔node wire protocol is still version 1, the MCP tools and their arguments
are unchanged, and configuration file formats are the same. A v0.2.x node can
connect to an RMC Hub and an RMC node can connect to a v0.2.x Hub.

Apart from `RMC_DOMAIN` for the Caddy example (below), nothing has to change on
the day you upgrade. The table shows what was renamed and what RMC still
accepts from earlier installations.

| Item | HostMCP (≤ v0.2.4) | RMC | Still accepted by RMC |
| --- | --- | --- | --- |
| Repository | `github.com/soren-labs/hostmcp` | `github.com/soren-labs/rmc` | GitHub redirects the old URLs |
| Binaries | `hostmcp`, `hostmcp-hub`, `hostmcp-agent` | `rmc`, `rmc-hub`, `rmc-node` | — |
| Default config directory | `~/.config/hostmcp/` | `~/.config/rmc/` | Used when a file exists only in the old directory |
| Client token variable | `HOSTMCP_TOKEN` | `RMC_TOKEN` | Read when `RMC_TOKEN` is unset |
| Service names | `hostmcp-…` | `rmc-…` | `service install` and `service uninstall` accept `hostmcp-…` names |
| launchd labels | `com.hostmcp.NAME` | `com.rmc.NAME` | Kept for `hostmcp-…` service names |
| macOS service logs | `Library/Logs/HostMCP` | `Library/Logs/RMC` | Kept for `hostmcp-…` service names |
| Node update archive | `hostmcp-agent_V_OS_ARCH.tar.gz` | `rmc-node_V_OS_ARCH.tar.gz` | Legacy name no longer published (see below) |

Explicit `--config` paths are never rewritten, so services that already pass
`--config /var/lib/hostmcp/hub.json` or similar keep using those files.

## Agent → Node (v0.3)

The device-side component was called **Agent** before v0.3. It is now a **node**,
because "agent" also names the AI clients that drive RMC. The rename changes names only:

- `rmc node enroll`, `rmc node run` and `rmc node update` replace `rmc agent …`.
- The binary and release archive are `rmc-node` and `rmc-node_V_OS_ARCH.tar.gz`.
- `--role node` replaces `--role agent` in `service install` and `scripts/install.sh`.
- The default config is `~/.config/rmc/node.json`. If that file is absent and
  `~/.config/rmc/agent.json` exists, RMC uses the old file.
- The example service name is `rmc-node`, and the hand-written launchd template is
  `deploy/launchd/io.rmc.node.plist`. Existing installed service names keep working.
- `list_devices` reports the component as `node`.

Still accepted under the old names: `rmc agent …` commands, `--role agent`,
`agent.json`, and services installed under their old names.

Nothing changes on the wire. Nodes still connect to the Hub on `/agent/<id>` and send
`agent` as their hello component, so old and new nodes work with old and new Hubs.
The legacy `hostmcp-agent_…` archives are no longer published; see
[Nodes that self-update](#nodes-that-self-update).

## Upgrading an existing node

The least disruptive path keeps your service name and config path. The
installer never overwrites an existing unit or plist, so remove the old
service first and then install it again under the same name, scope and
account. Run both steps with the new binary you installed (`rmc-node` for
a node, `rmc-hub` for a Hub, or the all-in-one `rmc`); every role binary
includes the `service` command.

1. Install the new binary, for example to `~/.local/bin/rmc-node` with
   `scripts/install.sh --role node`, or to `/usr/local/bin/rmc-node`.
2. Remove the existing service. RMC recognises the `hostmcp-…` name and the
   `Managed by HostMCP` marker:
   `rmc-node service uninstall --name hostmcp-agent-NAME`
   (add `--system` for a system service).
3. Install it again with the same name and the existing config file:
   `rmc-node service install --role node --name hostmcp-agent-NAME --config EXISTING.json`
   (add `--system --user USER` for a system service). On macOS the service keeps
   its `com.hostmcp.hostmcp-agent-NAME` label and `Library/Logs/HostMCP` log path.
4. Check `list_devices` from an MCP client: the node reports the new version.

Managed sessions on that node end when its node restarts. You can switch the
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

## Nodes that self-update

The opt-in updater in v0.2.x downloads `hostmcp-agent_V_OS_ARCH.tar.gz` and
installs the `hostmcp-agent` file inside it. Releases from v0.3.0 do not publish
that legacy archive, and the v0.2.x updater accepts only builds whose component is
`agent`, so it cannot install a v0.3 node. Upgrade each such node once, by hand,
with [Upgrading an existing node](#upgrading-an-existing-node). The `rmc node update`
command in [Hub and node lifecycle](components-updates.md) uses the `rmc-node_…` archives.

## MCP client configuration

The Hub URL, bearer tokens, OAuth settings and Cloudflare Access settings are
unchanged. If you named the connector "HostMCP" in your client, renaming it is
optional; tool names do not depend on the server name.
