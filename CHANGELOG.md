# Changelog

All notable changes to RMC (named HostMCP up to v0.2.4) are recorded here. Versions follow
[Semantic Versioning](https://semver.org/); before 1.0, minor versions may
include breaking changes and are called out explicitly. Release notes on GitHub
are generated from the matching section of this file.

## Unreleased

### Automatic updates

- Hubs and nodes now keep themselves on the newest release, **on by default**.
  A running `hub serve` or `node run` checks GitHub releases about hourly,
  installs the newest release of its channel, waits until it is idle (no tool
  call, managed process or MCP request in progress; at most 24 hours), keeps
  the previous executable and restarts in place.
- Releases are installed only when `SHA256SUMS.sig` verifies against a release
  key compiled into the running binary. The key is held offline by the owner:
  CI now creates **draft** releases only, and `scripts/sign-release.sh` checks,
  signs and publishes them. See [Releasing RMC](docs/releasing.md).
- A new version that is not healthy is rolled back and never retried on that
  machine: after 3 starts without confirming health, or for a node, after 10
  minutes without reaching a Hub that is itself up.
- Two channels: `stable` (default) and `canary`, which also installs release
  candidates (`vX.Y.Z-rc.N`). Release candidates go to canary nodes before the
  final release goes to everyone.
- New commands: `rmc update status|enable|disable|check` and
  `rmc update channel stable|canary`, with settings in `updates.json` beside the
  configuration; `rmc release keygen|sign|verify` for release owners.
  `RMC_AUTO_UPDATE=off` also turns updates off; the container image sets it.
- `list_devices` reports each node's `auto_update` mode: `stable`, `canary`,
  `off` or `manual`. Development builds, root services and executables in
  directories the service account cannot write are `manual` and never replaced.
- Release tags must be `vX.Y.Z` or `vX.Y.Z-rc.N`.
- Upgrading from v0.2.x is a one-time manual step; see
  [automatic updates](docs/components-updates.md#automatic-updates).

### Renamed: Agent is now Node

- The device-side component is now a **node**, because "agent" also names the AI
  clients that drive RMC. Use `rmc node enroll`, `rmc node run` and `rmc node update`;
  the `rmc agent …` commands still work.
- The binary and release archive are now `rmc-node` and `rmc-node_V_OS_ARCH.tar.gz`.
  `--role node` replaces `--role agent` in service install and `scripts/install.sh`;
  `agent` is still accepted.
- The default config is `~/.config/rmc/node.json`. An existing `agent.json` is used
  when `node.json` is absent. The example service is `rmc-node`, with a
  `deploy/launchd/io.rmc.node.plist` template; existing installed service names keep working.
- `list_devices` reports the component as `node`.
- Nothing changes on the wire: nodes still connect on `/agent/<id>` and send `agent`
  as their hello component, so old and new versions interoperate.
- The legacy `hostmcp-agent_…` release archives are no longer published. The v0.2.x
  updater accepts only the `agent` component, so move those nodes to this release once
  by hand. See [Migrating from HostMCP](docs/migrating-from-hostmcp.md).

### Renamed: HostMCP is now RMC (Remote Machine Control)

- The project, repository, Go module and binaries are now `rmc`,
  `rmc-hub` and `rmc-node` (`github.com/soren-labs/rmc`). The README
  leads with what RMC is for: letting web, mobile, desktop and terminal AI
  clients work on your own machines through one self-hosted endpoint.
- Nothing changes on the wire: Hub↔node wire version 1, MCP tools, arguments
  and configuration formats are the same, and RMC and HostMCP v0.2.x Hubs and
  agents interoperate.
- Existing installations keep working: the default config falls back to
  `~/.config/hostmcp/` when only that file exists, `HOSTMCP_TOKEN` is read when
  `RMC_TOKEN` is unset, and `service install`/`uninstall` accept existing
  `hostmcp-…` services with their original launchd labels and log directories.
- See [Migrating from HostMCP](docs/migrating-from-hostmcp.md).

### Added

- New read-only native tool `search`: lists files matching a `glob`, or
  searches file contents with an RE2 or literal `pattern`. It skips hidden
  entries unless `include_hidden` is set, binary files and files over 16 MiB,
  never follows symlinks, and is bounded by `max_results`, `max_bytes`,
  200,000 visited entries and 20 seconds. Native tools are now ten.
- `write_stdin.wait_for_exit` keeps waiting up to `yield_time_ms` for the
  process to exit, instead of returning on the first new output.
- `output_mode` on `exec_command` and `write_stdin`: `stream` (default, as
  before), `tail` (newest bytes) or `head_tail` (oldest quarter of the budget
  and newest bytes). The skip modes mark the gap and report `skipped_bytes`.
- `clean_ansi` on `exec_command` and `write_stdin` strips ANSI escape sequences
  and keeps only the final state of progress bars. `output_bytes` still counts
  raw bytes.
- `read` reports the `sha256` of the whole file for regular files up to 16 MiB.
- `write_file.expected_sha256` (with `overwrite: true`) replaces a file only if
  its current content still has that hash. Otherwise nothing is written, and
  the error reports the current hash.
- All new arguments are optional; omitting them keeps the previous behavior.
- Piped enrollment: `rmc device invite --output -` writes the invitation to
  stdout, and `rmc node enroll --invite -` reads it from stdin. A device can be
  added in one pipeline without leaving a credential file on either machine.
  `--output -` refuses to print to a terminal.
- `rmc doctor --hub-config` runs Hub checks: configuration privacy and validity,
  inventory including expired invitations, client authentication modes, listen
  address and public endpoint. With `--hub-url`, it also checks `/healthz` and
  clock skew from the HTTP `Date` header. It exits non-zero if any check fails
  and prints no credentials. Plain `rmc doctor` is unchanged.
- `--log-format json` for `hub serve` and `node run` writes each log line as one
  JSON object. The default stays text, and log call sites are unchanged.

### Changed

- Nodes advertise their native tools and argument names in an optional
  `tool_args` hello field. Before dispatch, a Hub refuses a native tool or
  argument that the target node does not support, with an "upgrade that node.
  Command was not dispatched" error. Nodes that do not advertise (v0.2.4 and
  older) are checked against a fixed table. Wire version stays 1, and older
  Hubs ignore the field. Using the new tools or arguments through a new Hub
  requires upgrading that node.

### Performance

- Each tool call through the Hub reads its configuration twice instead of four
  times, and hashes the bearer token once. Revocation semantics are unchanged.
  A tool call through the Hub takes 351 µs instead of 411 µs (−15%), with 33%
  fewer allocations (Apple M6, 100 clients, 200 devices).

## v0.2.4 — 2026-11-04

### Repository

- English README with a separate Chinese README; validation records moved under
  `docs/validation/`.
- Community files: security policy, code of conduct, issue and pull request
  templates, Dependabot, and a CODEOWNERS file.
- CI adds golangci-lint and govulncheck. Release notes come from this changelog.
- Unstamped source builds report `0.2.4-dev`. The Hub container build uses the
  source version unless `VERSION` is supplied, instead of a stale default.
- The deployment guide notes that the Hub image needs BuildKit (`docker buildx`).

### Removed

- The Chat On Steroids reference snapshot (`reference/`), its provenance manifest
  and import/verification scripts, the source-baseline workflow, `NOTICE`,
  `ATTRIBUTION.md` and the historical extraction plan. The Go runtime never
  imported or built any of them.
- Release archives and the container image no longer ship `NOTICE` or the
  upstream Codex license. `LICENSE` now names only the HostMCP contributors.
  Dependency license notices are still bundled.

### Fixed

- Updater tests no longer fail when the developer's umask is `002`, which is
  common on Ubuntu. The updater itself still rejects group-writable directories.
- Static MCP tool capability (`tools.listChanged: false`) so SEP-2575 (`2026-07-28`)
  clients such as Claude Web do not hold open long-lived `subscriptions/listen`
  streams and exhaust per-principal concurrency slots; sampled `mcp_busy` events
  (`principal_busy` / `hub_busy`) on HTTP 503 and sanitized Caddy access log template.
- Admitted `/mcp` requests have a 48-second context deadline that also interrupts
  stalled request-body reads and response writes. Context-aware handlers and
  parked subscription streams release their concurrency slots on expiry, logged
  as `event=mcp_deadline`. Shorter server I/O limits still apply; completed
  requests cannot leave a timeout callback affecting a reused connection.
- The same absolute request deadline reaches tool dispatch for legacy MCP
  protocols whose SDK contexts detach HTTP cancellation. Expired requests are
  rejected before dispatch; already-dispatched operations keep their unknown
  outcome semantics and do not lose the Agent connection or running command.
- Audit log sampling is per event kind and reports `suppressed=N`. A stale agent
  retrying a revoked credential no longer hides MCP denials or capacity
  rejections, and agent denials name the (validated) node ID. Capacity
  rejections are logged as `event=mcp_busy` instead of `event=auth_denied`.
- Agent heartbeat diagnostics retain ping write/response timeout causes, even
  when the socket closes before Ping returns. Normal Hub shutdowns keep their
  original close reason, and heartbeat goroutines finish before reconnecting.
- Non-PTY `exec_command` waits for command completion or `yield_time_ms`, instead
  of returning on an early output banner. PTYs and `write_stdin` remain responsive
  to the first output; a full output budget also returns early. Cancelling a call
  preserves its already-started command and unread output for later polling.

### Performance and compatibility

- Hub configuration parsing and validation are reused for byte-identical files.
  Each check still reads the current private file, so there is no TTL or metadata
  cache delaying revocation. Invalid or unreadable files fail closed, and returned
  snapshots own their nested grants independently.
- MCP results intentionally retain both structured JSON and the JSON text
  fallback recommended by the protocol. Regression tests cover both client
  representations, partial failures, Unicode, and large integer precision.

## v0.2.3 — 2026-10-28

- Optional Hub-only Cloudflare Access Managed OAuth assertion authentication,
  separate from external OAuth JWT access tokens and static bearers. Validates
  RS256, exact issuer/application AUD, times and email/subject using bounded,
  redirect-denying HTTPS JWKS fetches and a bounded key cache; startup fails closed.
- Explicit email-to-device grants, isolated session principals, SDK header/context
  integration, dynamic grant revocation and independent configure/disable flags.
  Agent/enrollment credentials, config format 1 and wire version 1 are unchanged.
- Ephemeral signed-JWT and official MCP SDK integration tests; production setup
  instructions in [Cloudflare Access](docs/cloudflare-access.md). Actual ChatGPT
  login through a deployed Cloudflare Access application remains untested.

## v0.2.2 — 2026-10-28

- Optional external OAuth resource server with RFC 9728 metadata/challenges, strict
  JWT/JWKS validation, explicit subject-to-node grants and client session isolation.
  Existing client/device credentials and wire version 1 remain supported.
- Separate Hub and Agent role artifacts alongside the original compatibility
  binary; component/wire identification and mixed-generation protocol tests.
- Opt-in local Agent updater pinned to a stable version, trusted archive SHA-256
  and fixed HTTPS release source, with staging, atomic replace and local rollback.
  Root updates, service restarts and post-restart recovery remain manual.
- Scoped node heartbeat/disconnect timestamps and offline packaging/updater tests.


## v0.2.1 — 2026-10-28

Repair release after two independent acceptance reviews of v0.2.0. The native
machine/account permission model, wire version, and existing credentials remain
compatible. No extra command or directory restrictions are introduced.

### Fixed

- Native path traversal preserves symlink components followed by `..`, avoiding
  accidental reads and writes to a different existing file.
- `apply_patch` updates existing inodes without replacing their owner, hard
  links, attributes or permission bits; adds safer error handling, source
  verification and tests for root editing files owned by another account.
- Stdin backpressure no longer kills a session; queued input remains ordered
  and visible to callers. Process-lifetime limitations are documented.
- The Hub reserves capacity for distinct client credentials and device
  enrollment. One client running long polls no longer monopolizes all requests.
- Authentication attempts, device connection lifecycle and tool dispatch
  outcomes produce token-free structured logs.
- Temporary Hub configuration read errors no longer disconnect known nodes;
  dispatch still fails closed until configuration is readable again.
- Credential recreation under the same client display ID does not inherit
  the previous credential's session identity. Revoked device agents back off
  instead of reconnecting every three seconds.
- systemd `ExecStart` correctly escapes dollar characters in literal paths;
  Compose images default to the correct release version.
- Added cross-platform regression tests for these scenarios and clarified
  enrollment error diagnostics, empty inventories, host allowlist clearing,
  and re-enrollment instructions.

### Limits

- A patch spanning multiple files is not a distributed transaction. The
  repository does not promise durable Agent sessions after process restarts.
- High availability, OAuth, Windows and multi-tenant authorization remain out
  of scope. Audit logs are not immutable or centrally retained by default.

## v0.2.0 — 2026-10-28

Account-node deployment for Linux and macOS. One registered node represents one
machine plus its actual OS account; clients receive separate credentials scoped to
node IDs. Root and ordinary accounts on the same host run separate named agents.
No directory policy, command allowlist, Windows support, or extra role hierarchy
is introduced.

### Added

- Optional expected-account binding on device invitations to catch accidental
  deployment under a different OS user.
- Client credential inventory, node-scope updates, and atomic credential rotation
  without changing the client's stable identity.
- Named Linux/macOS system services alongside existing user services, including
  explicit ordinary-user and root execution identities.
- Developer recipes for Codex and Claude Code, independent client credentials,
  and same-host root/ordinary account nodes.
- `scripts/verify-access.py` for actual MCP credential scopes, execution UID,
  root-only file permissions, and fixture cleanup.
- An opt-in 200-node routing/reconnect test with isolated Hub resource sampling,
  plus redacted evidence from real Claude Code, Codex, cloud and native-service runs.

### Fixed

- Stdio clients receive the remote Hub's account and execution-lifecycle
  instructions through the bridge. Both 2025-11-25 and 2026-07-28 protocol paths
  have bridge tests for catalog, argument, result and error forwarding.
- Atomic configuration updates preserve service-account ownership of existing
  files and locks, including administration performed by root.
- Device identity reports use the enrolled node ID and omit unknown connection
  timestamps instead of emitting a zero timestamp.
- macOS system-service access checks use native account switching semantics,
  avoiding invalid supplementary-group credentials during installation preflight.

Deployment measurements and real agent-client evidence are recorded in
[docs/validation/](docs/validation/README.md). Process sessions survive a connection
interruption while the node process remains alive; node-process restart recovery
and durable exactly-once command execution are not part of this release.

## v0.1.0 — 2026-10-22

First usable native HostMCP runtime, replacing the source-only extraction baseline
with an independently implemented Go application. The pinned upstream reference
files and their copyright/license notices remain unchanged.

### Added

- One executable for a multi-device Hub, outbound Linux/macOS device Agents, local
  stdio MCP, and a stdio bridge to the remote Streamable HTTP endpoint.
- Device inventory plus nine native tools: identity, shell execution, terminal
  input/output, managed process inventory/termination, file reads/writes, textual
  patches, and MCP image content from existing image files.
- Native OS-account authority, real filesystem paths, user shell/environment,
  POSIX PTYs, bounded output, execution deadlines, and process-group cleanup.
- Single-use, expiring enrollment invitations; independent device and client
  credentials; per-device client scopes; live revocation; private atomic state.
- Outbound WebSocket connections with heartbeats and reconnect backoff. Device
  sessions remain available across Hub/network interruptions while the Agent
  process stays running. Commands are not automatically replayed after a failure.
- Optional in-memory request IDs to prevent an explicit retry from executing the
  same command twice while its retained record exists.
- Native user services for systemd and launchd, a diagnostic command, install and
  release scripts, four platform archives, and dependency license bundles.
- Cloudflare Tunnel, Caddy, and Hub-only Docker deployment examples; architecture,
  client, security, contribution, and evidence-based validation documentation.
- Linux/macOS race tests and real CLI stdio smoke tests in CI; release publishing
  gated on tests, four-platform builds, and archive checksum/install verification.

### Scope

This release serves one owner or one trust domain. HTTP authentication uses static
bearer tokens. It does not implement OAuth login/refresh, untrusted tenant isolation,
GUI control, or recovery of terminal sessions after a device Agent restarts.

Actual deployment and client compatibility results are in
[docs/validation/](docs/validation/README.md). Configuration templates alone do not
constitute evidence that every deployment option or client has been tested.
