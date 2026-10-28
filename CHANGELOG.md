# Changelog

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

### Fixed

- Stdio clients receive the remote Hub's account and execution-lifecycle
  instructions through the bridge. Both 2025-11-25 and 2026-07-28 protocol paths
  have bridge tests for catalog, argument, result and error forwarding.

Deployment measurements and real agent-client evidence are recorded in
[docs/validation.md](docs/validation.md). Process sessions survive a connection
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
[docs/validation.md](docs/validation.md). Configuration templates alone do not
constitute evidence that every deployment option or client has been tested.
