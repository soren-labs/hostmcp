# Hub and node lifecycle

RMC ships `rmc` as the all-in-one compatibility binary plus separate `rmc-hub`
and `rmc-node` artifacts (`rmc-agent` before v0.3; introduced in HostMCP v0.2.2 as `hostmcp`,
`hostmcp-hub` and `hostmcp-agent`). All variants share Go source and may share a
release tag; each has a separate archive, destination path and deployment lifecycle.

| Artifact | Commands | Upgrade when |
| --- | --- | --- |
| `rmc-hub` | `hub`, `device`, `token`, `connect`, `call`, service/diagnostics | Auth, MCP presentation, routing, metadata or Hub policy changes |
| `rmc-node` | `node enroll/run/update` (`agent` is a legacy alias), service/diagnostics | Native executor fixes/features or deliberate wire changes |
| `rmc` | All existing commands, including local `stdio` | Compatibility installations, bridges and local use |

Role-specific binaries reject unsupported commands before reading configs or
starting services. `service install --role` must match the binary. Service uninstall
remains a local management operation under the calling OS account. Roles are CLI
restrictions, not distinct OS sandboxes. They do not change the node user's normal
filesystem, process, sudo or macOS privacy permissions.

Build release archives with `bash scripts/release.sh vX.Y.Z`: three variants times
four targets (Linux/macOS, amd64/arm64), with SHA256SUMS and license notices. No
publishing occurs. Install a verified release with `scripts/install.sh --version
vX.Y.Z --role hub` or `--role node` (`agent` is still accepted); omit `--role` for the original binary. For local
archives add `--archive ... --checksums ...`. Service generation uses the executable
actually invoked; new node services can use:

```sh
/absolute/path/rmc-node service install --role node \
  --config /absolute/path/node.json --name rmc-node
```

Existing services using `rmc agent run` (the legacy form of `rmc node run`) keep working. Migrating to a new path
requires updating/reinstalling that service deliberately, preserving its config,
name and OS account. An existing `rmc` service does not start using a new
`rmc-node` file automatically. A node restart ends its managed process
sessions; [automatic updates](#automatic-updates) wait until the node is idle.

## Compatibility contract

Config format, private wire version and component build version are separate:

- Config format remains **1**. Existing fields/tokens/nodes remain valid. Optional
  Hub OAuth/Cloudflare Access fields require a supporting Hub; older Hub binaries require removing
  that optional policy before rollback. No new node config fields are required.
- Wire version remains **1**, independently of the release tag. Both sides require
  exactly version 1 in hello/welcome. A mismatch closes the connection before any
  tool request. A Hub-only upgrade never requires node changes when this contract
  and the requested native operations remain unchanged.
- JSON object fields added to v1 must be optional; missing fields have old behavior
  and unknown fields are ignored. Existing request/result meanings, tool names,
  input fields, principal size limit and replay/ownership rules cannot be changed
  incompatibly within v1. New required messages/operations need explicit negotiation
  or a new wire version, plus mixed-version tests before release. Do not silently
  dispatch new native features to older nodes; unsupported native operations
  require a node upgrade. There is no arbitrary remote code/plugin delivery.
- Build versions are diagnostic, not compatibility gates. `version` reports the
  component, build/commit, platform, wire and config versions. node hello reports
  its build version; welcome optionally reports the Hub build version/component.
  Old peers ignore these new fields. Release builds inject the same version into
  CLI, MCP server, health endpoint and device inventory. Unstamped source builds
  report the next development version with a `-dev` suffix (for example
  `0.2.4-dev` after v0.2.3). The v0.2.3 Cloudflare Access identity mode is
  [Hub-only](cloudflare-access.md); existing nodes require no replacement.

A node may send an optional `tool_args` map in hello, listing each native tool
it runs and that tool's top-level argument names. Before dispatch, a Hub refuses
a native tool or argument the target node does not support, with an "upgrade
that node" error, so the command is not sent. Nodes without the field (v0.2.4
and older) are checked against a fixed table of their tools and arguments.
Tools and arguments the Hub does not know are passed through unchanged. Wire
version stays 1, and older Hubs ignore the field.

`TestCompatibilityLegacyAgentWithOAuthHub` sends the old v0.2.1 hello field set to
the new Hub and dispatches an OAuth-authorized tool over wire v1. The reverse test
sends new node traffic to a minimal legacy-v1 Hub fixture. These are in-process
protocol tests, not tests of deployed v0.2.1 binaries or external clients.

## Automatic updates

From v0.3, a running Hub (`hub serve`) or node (`node run`) keeps itself on the
newest signed release. This applies to `rmc-hub`, `rmc-node` and the combined
`rmc` binary; each installs its own archive.

**On by default, stable channel.** Settings live in `updates.json` beside the
Hub or node configuration, shared by the services configured in that directory.
A missing file means "on, stable".

```sh
rmc update status  [--config FILE]   # settings, mode, last check, rollbacks
rmc update disable [--config FILE]   # turn automatic updates off
rmc update enable  [--config FILE]
rmc update channel stable|canary [--config FILE]
rmc update check   [--config FILE]   # look for a release now; installs nothing
```

`RMC_AUTO_UPDATE=off` in the service environment also turns updates off. The
container image sets it: containers are updated by replacing the image.
Changes take effect at the service's next check, without a restart.

`list_devices` shows each node's `auto_update` mode as of its last start:

| Mode | Meaning |
| --- | --- |
| `stable` / `canary` | Automatic updates on, from that channel |
| `off` | Turned off with `rmc update disable` or `RMC_AUTO_UPDATE` |
| `manual` | This installation cannot replace itself (see below) |

### When an installation is manual

The updater leaves the executable alone, and reports why in `rmc update status`
(`manual_reason`) and in the service log, when:

- it is a development build (a version such as `0.3.0-dev`);
- the service runs as root. Root installations are always updated by an
  administrator;
- the executable's directory is not writable by the service account, or any
  directory above it is owned by another user or writable by others. Packages
  installed root-owned in `/usr/local/bin` are manual for this reason. To
  enable automatic updates for such a node, move its binary to a directory the
  service account owns (for example `~/.rmc/bin`, mode `0755`) and point the
  service at it. On Ubuntu, `~/.local` is usually group-writable (the default
  umask is `002`), so binaries under `~/.local/bin` are manual there too;
  `rmc update status` shows the reason.

### What a check does

A service checks 2–9 minutes after it starts, then about hourly (±10 minutes).
Each check:

1. reads the release list from `api.github.com` for `soren-labs/rmc`. Downloads
   are accepted only from GitHub's release hosts. `RMC_UPDATE_API` may point
   at an HTTPS mirror of the releases API instead; signatures are still
   required;
2. picks the newest published release above the running version for its
   channel, skipping drafts and versions that were rolled back on this machine;
3. requires `SHA256SUMS` and `SHA256SUMS.sig`, and verifies the signature
   against the release keys compiled into the running binary. Unsigned or
   wrongly signed releases are never installed;
4. downloads only this component's archive for this platform and checks its
   SHA-256 against the signed `SHA256SUMS`;
5. extracts only the executable, and runs its `version` command: the version,
   component, platform and wire version must match.

### Installing and restarting

The updater waits until the service is idle: for a node, no tool call or managed
process is running; for a Hub, no MCP request is in progress. A node that stays
busy is restarted after 24 hours anyway. It refuses to continue if someone
replaced the executable on disk since the service started.

It then keeps the current executable as `NAME.previous`, renames the new one
into place, and re-executes it in the same process. The PID and arguments stay
the same, so systemd and launchd see one continuous service; this was checked
under launchd on macOS (see the [validation record](validation/2026-11-09-auto-update.md)).
If the re-exec fails, the service exits with an error and its service manager
starts the new executable.

A restart ends the node's managed process sessions, which is why the updater
waits for idle. Run long-lived work as its own systemd or launchd service.

### Health checks and rollback

A new version must confirm that it is healthy:

- a node confirms on its first successful connection to its Hub;
- a Hub confirms after serving for 60 seconds.

A version that has not confirmed after 3 starts is rolled back on the 4th
start: `NAME.previous` is restored and executed. A node that has not reached its
Hub within 10 minutes is also rolled back, but only while the Hub itself answers
`/healthz`, so a Hub outage does not roll back healthy nodes. A rolled-back
version is recorded in `CONFIG-STEM.update-state.json` (for example
`node.update-state.json`) and never retried on that machine; the next higher
release is installed normally.

Rollback cannot catch a version that crashes before its start is counted; the
`version` check before installing guards against that.

### Upgrading from v0.2

The v0.2 updater accepts only the old `agent` component, so moving to v0.3 is a
one-time manual upgrade: install the v0.3 binary in a directory the service
account owns, point the service at it, and restart. From then on the
installation updates itself. See [Migrating from HostMCP](migrating-from-hostmcp.md).

Release owners: see [Releasing RMC](releasing.md) for signing and canary
rollout.

## Pinned manual updates

For a node where automatic updates are off, `node update` installs one exact
version from a private local policy. It never runs on its own; schedule it with a
systemd timer or launchd job if needed. It accepts only the fixed GitHub source
`soren-labs/rmc`, an exact stable version, this platform, and a SHA-256 that you
pin in the policy. That pin is the trust anchor, so obtain it through your own
review channel, not from the `SHA256SUMS` downloaded beside the archive. Policy
files and executables must belong to the calling user, policy files must be
private, and the updater refuses to run as root.

```json
{
  "version": 1,
  "enabled": true,
  "channel": "stable",
  "target_version": "0.3.0",
  "sha256": "REPLACE_WITH_64_HEX_DIGITS_FOR_THE_NODE_ARCHIVE",
  "binary": "/home/worker/.local/bin/rmc-node"
}
```

Use the real absolute path without symlinked parents (on macOS, `/private/var`
rather than `/var`). The target must already be a node-only binary reporting a
stable version.

```sh
rmc-node node update --policy /absolute/path/node-update.json
# Or install a reviewed archive without network access:
rmc-node node update --policy /absolute/path/node-update.json \
  --archive /absolute/path/rmc-node_X.Y.Z_linux_amd64.tar.gz
# Restore the previous executable:
rmc-node node update --policy /absolute/path/node-update.json --rollback
```

It verifies the pinned SHA-256 before extracting, extracts only the executable,
checks its `version`, keeps `.previous` and renames atomically, like the
automatic updater. Unlike it, it does not restart the service, check health or
roll back by itself: restart the service in a maintenance window, then check
`list_devices` and `device_info`. An existing `.previous` blocks the next pinned
upgrade so a rollback copy is never overwritten; remove it once the rollout is
confirmed.

## Health and restart operations

Unauthenticated `/healthz` reports only Hub liveness, build component/version and
wire version. It does not reveal nodes or prove they are online. Authenticated
`list_devices` returns only allowed nodes with running node version, component,
observed wire version, `online`, `connected_at`, `last_seen` (successful connection
or heartbeat), and `disconnected_at`. Heartbeats run every 20 seconds with an
8-second timeout; an unresponsive connection is normally detected by the next
heartbeat plus timeout. Known timestamps are in memory and reset on Hub restart;
never-seen nodes have no timestamps. A successful ping does not attest that an OS
shell, disk or external network is healthy. Use `device_info` for an active check.

| Service | Status/logs | Planned node restart |
| --- | --- | --- |
| Linux user | `systemctl --user status NAME`; `journalctl --user -u NAME` | `systemctl --user restart NAME` |
| Linux system | `systemctl status NAME`; `journalctl -u NAME` | Administrator runs `systemctl restart NAME` |
| macOS user | `launchctl print gui/UID/com.rmc.NAME`; configured log file | `launchctl kickstart -k gui/UID/com.rmc.NAME` in the correct login session |
| macOS system | `launchctl print system/com.rmc.NAME`; configured log file | Administrator runs `launchctl kickstart -k system/com.rmc.NAME` |

Use the actual service label; manual templates may have different labels. A Hub
restart reconnects nodes; do not replay uncertain side effects. A sleeping,
powered-off, physically disconnected or failed machine cannot be repaired by this
software. Diagnose power, network and OS startup separately; user services also
require the appropriate user manager/login and macOS FileVault/TCC state.

## Local verification

```sh
go test ./internal/update -count=1
bash scripts/release.sh v0.3.0 ./build/release-smoke
python3 scripts/packaging-smoke.py --dist ./build/release-smoke --version v0.3.0
```

The packaging test builds a temporary older native node, installs each native
role, rejects unsupported commands and corrupted archives, performs a pinned
offline upgrade and rollback, and checks all 12 archive digests/notices. Tests use
temporary files and no service manager. Cross-compilation verifies buildability;
it does not establish native Linux execution on a Mac or vice versa.
