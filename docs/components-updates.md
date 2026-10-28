# Hub and Agent lifecycle

This source tree keeps `hostmcp` as the original compatibility binary and adds
`hostmcp-hub` and `hostmcp-agent` artifacts. These new artifacts are not present in
the already published v0.2.1 release. All variants share Go source and may share a
release tag; each has a separate archive, destination path and deployment lifecycle.

| Artifact | Commands | Upgrade when |
| --- | --- | --- |
| `hostmcp-hub` | `hub`, `device`, `token`, `connect`, `call`, service/diagnostics | Auth, MCP presentation, routing, metadata or Hub policy changes |
| `hostmcp-agent` | `agent enroll/run/update`, service/diagnostics | Native executor fixes/features or deliberate wire changes |
| `hostmcp` | All existing commands, including local `stdio` | Compatibility installations, bridges and local use |

Role-specific binaries reject unsupported commands before reading configs or
starting services. `service install --role` must match the binary. Service uninstall
remains a local management operation under the calling OS account. Roles are CLI
restrictions, not distinct OS sandboxes. They do not change the Agent user's normal
filesystem, process, sudo or macOS privacy permissions.

Build release archives with `bash scripts/release.sh vX.Y.Z`: three variants times
four targets (Linux/macOS, amd64/arm64), with SHA256SUMS and license notices. No
publishing occurs. Install a verified release with `scripts/install.sh --version
vX.Y.Z --role hub` or `--role agent`; omit `--role` for the original binary. For local
archives add `--archive ... --checksums ...`. Service generation uses the executable
actually invoked; new agent services can use:

```sh
/absolute/path/hostmcp-agent service install --role agent \
  --config /absolute/path/agent.json --name hostmcp-agent
```

Existing services using `hostmcp agent run` keep working. Migrating to a new path
requires updating/reinstalling that service deliberately, preserving its config,
name and OS account. An existing `hostmcp` service does not start using a new
`hostmcp-agent` file automatically. Agent updates can end managed process sessions
when the service restarts; schedule maintenance around important work.

## Compatibility contract

Config format, private wire version and component build version are separate:

- Config format remains **1**. Existing fields/tokens/nodes remain valid. Optional
  Hub OAuth fields require this Hub version; older Hub binaries require removing
  that optional policy before rollback. No new Agent config fields are required.
- Wire version remains **1**, independently of the release tag. Both sides require
  exactly version 1 in hello/welcome. A mismatch closes the connection before any
  tool request. A Hub-only upgrade never requires Agent changes when this contract
  and the requested native operations remain unchanged.
- JSON object fields added to v1 must be optional; missing fields have old behavior
  and unknown fields are ignored. Existing request/result meanings, tool names,
  input fields, principal size limit and replay/ownership rules cannot be changed
  incompatibly within v1. New required messages/operations need explicit negotiation
  or a new wire version, plus mixed-version tests before release. Do not silently
  dispatch new native features to older Agents; unsupported native operations
  require an Agent upgrade. There is no arbitrary remote code/plugin delivery.
- Build versions are diagnostic, not compatibility gates. `version` reports the
  component, build/commit, platform, wire and config versions. Agent hello reports
  its build version; welcome optionally reports the Hub build version/component.
  Old peers ignore these new fields. Release builds inject the same version into
  CLI, MCP server, health endpoint and device inventory. Unstamped source builds
  identify themselves as `0.2.2-dev`.

`TestCompatibilityLegacyAgentWithOAuthHub` sends the old v0.2.1 hello field set to
the new Hub and dispatches an OAuth-authorized tool over wire v1. The reverse test
sends new Agent traffic to a minimal legacy-v1 Hub fixture. These are in-process
protocol tests, not tests of deployed v0.2.1 binaries or external clients.

## Optional pinned node updates

Updates are disabled unless a local operator provisions a private policy and runs
or schedules `agent update`. The agent channel has no update API or update URL message from the Hub.
This constrains the updater mechanism: a client with native execution authority
on a node can still edit its own files or run software as that OS account; this
is not an additional OS permission boundary. The updater accepts only the fixed HTTPS GitHub release source
`soren-labs/hostmcp`, exact stable versions, this node's OS/architecture and a
locally pinned SHA-256. There is no `latest`, prerelease channel, alternate repository
or custom URL field. Policy files and executables must be owned by the calling
user; policy files are private. The updater refuses root entirely. Root nodes and
root-owned executables require a separate, deliberate administrator-managed upgrade.
It does not use sudo or change service identities.

Example **template**, replacing the version/checksum/path with an available Agent
release and its digest for this OS/architecture:

```json
{
  "version": 1,
  "enabled": true,
  "channel": "stable",
  "target_version": "0.2.2",
  "sha256": "REPLACE_WITH_64_HEX_DIGITS_FOR_THE_AGENT_ARCHIVE",
  "binary": "/home/worker/.local/bin/hostmcp-agent"
}
```

Place this in a `0600` file in the operator's private configuration directory. Use
the actual, absolute path without symlinked parent directories (resolve `/var` to
`/private/var` on macOS if necessary). The target must already be an Agent-only
binary reporting a stable build version. Migrate legacy combined binaries manually
once; this updater will not overwrite a Hub or compatibility binary. Do not deploy
user-writable executables as root services.

Obtain the expected digest through your trusted release-review/configuration
channel, independently of the artifact being downloaded. SHA256SUMS downloaded
beside a binary alone does not authenticate a compromised publisher. The fixed
source is authenticated by normal HTTPS certificate validation; this version does
not implement release signing, Sigstore/TUF, a private GitHub credential flow, or
key rotation for signed release manifests. The local checksum pin is the content
trust anchor; protecting its distribution is the operator's responsibility.

```sh
hostmcp-agent agent update --policy /absolute/path/agent-update.json
# Or transfer a reviewed archive without giving the node network access:
hostmcp-agent agent update --policy /absolute/path/agent-update.json \
  --archive /absolute/path/hostmcp-agent_X.Y.Z_linux_amd64.tar.gz
```

The updater locks the target, refuses automatic downgrade, bounds download and
extraction sizes, verifies SHA-256 **before extraction/execution**, extracts only
the expected regular executable (rejecting archive links and unsafe paths), stages
on the same filesystem, and executes its `version` command with bounded output and
a deadline. The candidate must match the pinned version, agent role, platform and
wire v1. It syncs staging, preserves the original executable at `.previous`, then
atomically renames the candidate. A directory-sync error after rename explicitly
reports that replacement occurred; inspect version before deciding next steps.
Failures before replacement leave the executable unchanged. An existing `.previous`
blocks a later upgrade so rollback is never overwritten. After confirming a rollout,
archive/remove that backup locally before pinning the next version.

A process already running continues using its old executable. The updater does
**not** restart services, interrupt sessions, test the new process's Hub connection
or automatically roll back a process that fails after restart. Plan an explicit
restart and check the running version/online state. To restore the retained binary:

```sh
hostmcp-agent agent update --policy /absolute/path/agent-update.json --rollback
# Restart the same service, then verify list_devices and device_info.
```

If the new executable cannot start at all, use another verified HostMCP updater
or restore the local `.previous` file to the original path with a same-directory
rename before restarting. Rollback consumes the local backup and never downloads a URL. Revoke or change the
target policy before the next scheduled update if you want the node to stay on the
rolled-back version. `.update.lock` is a persistent coordination file; process exit
releases its OS lock. A crash after backup creation may leave `.previous` while the
original remains active; inspect versions before clearing that backup.

For many nodes, distribute one reviewed desired version with per-platform digest
pins using your existing configuration management. Roll out to a canary, verify
online version and a harmless identity call, then expand in batches. No Hub/Agent
release-tag equality is required. For optional periodic replacement, a user-level
systemd timer or launchd job can run the above fixed-policy command at a chosen
interval; it is a no-op without a download once the installed version matches.
Keep service restart as an explicit maintenance operation. This is a functional
pinned updater, not an automatic latest-version discovery or fleet orchestrator.

## Health and restart operations

Unauthenticated `/healthz` reports only Hub liveness, build component/version and
wire version. It does not reveal nodes or prove they are online. Authenticated
`list_devices` returns only allowed nodes with running Agent version, component,
observed wire version, `online`, `connected_at`, `last_seen` (successful connection
or heartbeat), and `disconnected_at`. Heartbeats run every 20 seconds with an
8-second timeout; an unresponsive connection is normally detected by the next
heartbeat plus timeout. Known timestamps are in memory and reset on Hub restart;
never-seen nodes have no timestamps. A successful ping does not attest that an OS
shell, disk or external network is healthy. Use `device_info` for an active check.

| Service | Status/logs | Planned Agent restart |
| --- | --- | --- |
| Linux user | `systemctl --user status NAME`; `journalctl --user -u NAME` | `systemctl --user restart NAME` |
| Linux system | `systemctl status NAME`; `journalctl -u NAME` | Administrator runs `systemctl restart NAME` |
| macOS user | `launchctl print gui/UID/com.hostmcp.NAME`; configured log file | `launchctl kickstart -k gui/UID/com.hostmcp.NAME` in the correct login session |
| macOS system | `launchctl print system/com.hostmcp.NAME`; configured log file | Administrator runs `launchctl kickstart -k system/com.hostmcp.NAME` |

Use the actual service label; manual templates may have different labels. A Hub
restart reconnects Agents; do not replay uncertain side effects. A sleeping,
powered-off, physically disconnected or failed machine cannot be repaired by this
software. Diagnose power, network and OS startup separately; user agents also
require the appropriate user manager/login and macOS FileVault/TCC state.

## Local verification

```sh
go test ./internal/update -count=1
bash scripts/release.sh v0.2.2 ./build/release-smoke
python3 scripts/packaging-smoke.py --dist ./build/release-smoke --version v0.2.2
```

The packaging test builds a temporary older native Agent, installs each native
role, rejects unsupported commands and corrupted archives, performs a pinned
offline upgrade and rollback, and checks all 12 archive digests/notices. Tests use
temporary files and no service manager. Cross-compilation verifies buildability;
it does not establish native Linux execution on a Mac or vice versa.
