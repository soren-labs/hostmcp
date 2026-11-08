# Roost — notes for coding agents

Roost is a self-hosted MCP gateway for Linux and macOS, written in Go. A Hub
authenticates MCP clients and routes tool calls over outbound WebSocket
connections to native device Agents. Each Agent runs as one OS account and its
tools act with that account's ordinary privileges.

## Layout

| Path | Purpose |
| --- | --- |
| `cmd/roost` | CLI entry point: `hub`, `device`, `token`, `agent`, `connect`, `call`, `service`, `doctor`, `stdio`. The `component` ldflag builds the `all`, `hub` or `agent` variant. |
| `internal/hub` | MCP Streamable HTTP endpoint, client authentication (bearer, external OAuth, Cloudflare Access), device routing. Never executes commands. |
| `internal/agent` | Outbound Agent connection loop with heartbeats and reconnect backoff. |
| `internal/host` | Native tools: `exec_command`, `write_stdin`, processes, `read`, `write_file`, `apply_patch`, `view_image`. |
| `internal/mcpserver` | Maps native tools onto the official MCP Go SDK. |
| `internal/protocol` | Private Hub↔Agent wire protocol (wire version 1). |
| `internal/config` | Hub/Agent configuration files and the locked configuration store. |
| `internal/oauth`, `internal/cloudflareaccess` | JWT access-token and Access-assertion verifiers. |
| `internal/service` | systemd and launchd user/system service installers. |
| `internal/update` | Opt-in, checksum-pinned Agent updater. |
| `scripts/` | Smoke tests, packaging, installer and release helpers. |
| `docs/` | User documentation. `docs/validation/` holds test evidence. |
| `deploy/` | Docker, Caddy, Cloudflare Tunnel and hand-written service examples. |

## Commands

```sh
make check        # gofmt, vet, lint, race tests, stdio smoke test
make build        # ./build/roost
```

`make lint` needs [golangci-lint](https://golangci-lint.run/) v2. The underlying
commands are listed in `Makefile` and `CONTRIBUTING.md`.

## Rules

- Use real OS paths. Do not add approved-folder policies, command allowlists or
  browser conversation identity in front of ordinary file and process operations.
- Full user authority does not mean automatic root elevation or bypassing macOS
  privacy permissions. Preserve OS errors and normal user environment semantics.
- Keep transport authentication, input validation, bounded output and process
  ownership separate from OS filesystem permissions.
- The Hub routes; it never runs device commands. Keep that separation.
- Wire version 1 is a compatibility contract: new message fields must be
  optional, and existing request/result meanings must not change. See
  `docs/components-updates.md`.
- Target standard MCP clients over stdio and Streamable HTTP. No Electron,
  renderer, tray, browser extension, ChatGPT DOM automation, model API or GUI.
- Tests use isolated temporary directories and test-only credentials. Never
  connect tests to a real Hub, real credentials or real user data.
- Only claim a platform, deployment or client as tested when it was actually
  tested. Record evidence under `docs/validation/` without secrets.
- Add user-visible changes to the `Unreleased` section of `CHANGELOG.md`.
