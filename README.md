# HostMCP

[![CI](https://github.com/soren-labs/hostmcp/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/soren-labs/hostmcp/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/soren-labs/hostmcp)](https://github.com/soren-labs/hostmcp/releases/latest)
[![Go](https://img.shields.io/github/go-mod/go-version/soren-labs/hostmcp)](go.mod)
[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

**One MCP endpoint for all of your Linux and macOS machines.**

[简体中文](README.zh-CN.md)

HostMCP is a self-hosted [Model Context Protocol](https://modelcontextprotocol.io/)
server that lets AI agents such as Claude Code, Codex and ChatGPT run commands and
edit files on your own machines. A small Go **Hub** authenticates MCP clients and
routes each tool call to a native **Agent** on the chosen machine. Agents connect
out to the Hub, so your laptop, home server or VPS needs no public IP or open port.

Each Agent acts as the OS account that runs it: real paths, your shell and
environment, normal Unix permissions. No containers, no directory allowlists,
no Node.js or Electron on the devices.

## Features

- **Multi-device**: one Hub, many machines. Put the Hub on a VPS, or on a home
  machine published through Cloudflare Tunnel.
- **Per-account nodes**: a node is one machine plus one OS account. Run
  `build-box-worker` and `build-box-root` side by side and grant each AI client
  only the nodes it needs.
- **Native tools**: shell commands with PTY support, managed long-running
  processes, file read/write, Codex-style `apply_patch`, and image viewing.
- **Standard MCP**: Streamable HTTP for remote clients, stdio for local use, and a
  stdio bridge (`hostmcp connect`) for clients that only launch local commands.
  Built on the official MCP Go SDK.
- **Client authentication**: per-client bearer tokens, an external OAuth 2.1
  resource server, or Cloudflare Access Managed OAuth assertions.
- **Small footprint**: a single static binary for Linux and macOS (amd64, arm64).
  The Hub measured 12–16 MiB RSS on a 412 MiB VPS with three devices connected
  ([measurement](docs/validation/README.md)).
- **Service install**: generates systemd and launchd user or system services.

## How it works

```mermaid
flowchart LR
    C["MCP client<br/>Claude Code · Codex · ChatGPT"] -->|"HTTPS /mcp"| E["HTTPS proxy or<br/>Cloudflare Tunnel"]
    E --> H["HostMCP Hub<br/>auth + routing"]
    A1["Agent on Ubuntu<br/>(user dev)"] -->|"outbound WebSocket"| H
    A2["Agent on Ubuntu<br/>(root)"] -->|"outbound WebSocket"| H
    A3["Agent on Mac<br/>(user alice)"] -->|"outbound WebSocket"| H
```

The Hub never runs commands itself. To manage the Hub machine, run an Agent there
too. Builds and other heavy work happen on the device you pick.

## Tools

| Tool | What it does |
| --- | --- |
| `list_devices` | Nodes visible to this client, with online state, OS user and UID (remote Hub only) |
| `device_info` | OS account, UID/GID, groups, working directory and shell of a node |
| `exec_command` | Run a shell command, optionally in a PTY; returns a session for long-running work |
| `write_stdin` | Read more output from a session, send input or signals, resize a PTY |
| `list_processes` / `terminate_process` | List and stop the caller's own sessions |
| `read` | Read files (text line ranges or base64), list directories, show metadata |
| `write_file` | Create or overwrite a file (text or base64) |
| `apply_patch` | Apply a `*** Begin Patch` add/update/delete/move patch |
| `view_image` | Return a PNG, JPEG or GIF file as MCP image content |

On a remote Hub every device tool takes a `device` argument with the node ID from
`list_devices`. In local `stdio` mode the tools act on the current machine.

For non-PTY `exec_command`, `yield_time_ms` waits for the command to finish or for
that interval to elapse (default 1000 ms; zero returns immediately). PTYs and
`write_stdin` return when output is available; filling the output budget also
returns early. A still-running command keeps its session for later polling.
`timeout_seconds` controls the command's execution lifetime separately.

## Install

Pre-built binaries for Linux and macOS (amd64, arm64) are on the
[releases page](https://github.com/soren-labs/hostmcp/releases). The installer
picks the right archive, verifies its SHA-256 and installs to `~/.local/bin`:

```sh
curl -fsSLo hostmcp-install.sh \
  https://raw.githubusercontent.com/soren-labs/hostmcp/v0.2.3/scripts/install.sh
sh hostmcp-install.sh --version v0.2.3
export PATH="$HOME/.local/bin:$PATH"
hostmcp version
```

Use `--role hub` or `--role agent` to install the smaller single-role binaries
(`hostmcp-hub`, `hostmcp-agent`). The default `hostmcp` binary contains every command.

To build from source you need Go 1.25 or newer:

```sh
git clone https://github.com/soren-labs/hostmcp.git
cd hostmcp
make build            # writes ./build/hostmcp
```

`go install github.com/soren-labs/hostmcp/cmd/hostmcp@latest` also works; such
builds report a `-dev` version because release metadata is not stamped in.

## Quick start: one machine

No Hub is needed. Point your MCP client at `hostmcp stdio`; tools run as the user
who starts the client.

```sh
# Claude Code
claude mcp add --transport stdio hostmcp -- "$HOME/.local/bin/hostmcp" stdio
# Codex CLI
codex mcp add hostmcp -- "$HOME/.local/bin/hostmcp" stdio
```

Clients that use an `mcpServers` JSON file:

```json
{
  "mcpServers": {
    "hostmcp": { "command": "/absolute/path/to/hostmcp", "args": ["stdio"] }
  }
}
```

## Quick start: several machines

The example uses `https://mcp.example.com` as the Hub's public HTTPS address. See
[deployment](docs/deployment.md) for Cloudflare Tunnel, Caddy and Docker setups.

**1. Create and start the Hub** (on the Hub machine):

```sh
hostmcp hub init --public-url https://mcp.example.com \
  --client-token-file ~/.config/hostmcp/owner.token
hostmcp hub serve           # listens on 127.0.0.1:8787; put HTTPS in front of it
```

**2. Invite a device** (on the Hub machine). `--user` binds the node to the OS
account it must run as:

```sh
hostmcp device invite --id laptop --user alice --output laptop-invite.json
```

**3. Enroll and run the Agent** (on the device, as that user). Copy the invite
file over a channel you trust; it is single-use and expires after 15 minutes:

```sh
hostmcp agent enroll --invite laptop-invite.json
hostmcp agent run
```

**4. Connect a client.** Give each AI client its own token, scoped to the nodes it
needs:

```sh
# on the Hub machine
hostmcp token create --id claude --devices laptop --output claude.token
```

```sh
# on the client machine, with claude.token copied there (chmod 600)
claude mcp add --transport stdio hostmcp -- \
  "$HOME/.local/bin/hostmcp" connect \
  --url https://mcp.example.com/mcp --token-file /absolute/path/claude.token
```

Clients that support remote MCP with an `Authorization` header can use
`https://mcp.example.com/mcp` directly. Check the setup from a shell:

```sh
hostmcp call --url https://mcp.example.com/mcp \
  --token-file /absolute/path/claude.token --tool list_devices
```

**5. Keep it running.** Install the Hub and each Agent as a service:

```sh
hostmcp service install --role hub   --config ~/.config/hostmcp/hub.json   --name hostmcp-hub
hostmcp service install --role agent --config ~/.config/hostmcp/agent.json --name hostmcp-agent
```

User services are the default. For boot-time system services, root nodes and
several accounts on one machine, see [account nodes](docs/account-nodes.md).

## Security model

- **A token grants the full authority of the OS accounts behind its nodes.** An
  authorized client can run anything and read or write anything that account can.
  There is no directory or command allowlist by design. Give each client only the
  nodes it needs, and use dedicated low-privilege accounts where that matters.
- **Root is opt-in.** Agents never elevate. A root node is a separate Agent that an
  administrator deliberately runs as root.
- **Separate credentials** for clients, device invitations and device connections.
  The Hub stores only SHA-256 hashes. Tokens can be scoped, rotated and revoked
  without touching other clients or devices.
- **The Hub only routes.** It authenticates, checks the client's node grants and
  forwards the call. It has no command executor.
- **OS protections still apply**: file permissions, sudo policy, SELinux/AppArmor,
  and macOS TCC/SIP.

Read the full [security model](docs/security.md) before exposing a Hub to the
internet. Report vulnerabilities privately as described in [SECURITY.md](SECURITY.md).

## Status and limitations

The latest release is [v0.2.3](https://github.com/soren-labs/hostmcp/releases/latest).
HostMCP is pre-1.0: it serves one owner or one trust domain, and minor versions
may change behavior. Each [validation record](docs/validation/README.md) lists
exactly what was tested, on which machines and with which clients.

Not supported today:

- Windows, desktop control (mouse, keyboard, screenshots) and browser automation;
- multi-tenant isolation between untrusted users of one Hub;
- a built-in identity provider. OAuth uses your own provider or Cloudflare Access;
- resuming process sessions after an Agent restart. Sessions survive network
  reconnects, but not Agent restarts.

## Documentation

| Topic | Document | Language |
| --- | --- | --- |
| Architecture and design decisions | [docs/architecture.md](docs/architecture.md) | 中文 |
| Deployment: Cloudflare Tunnel, Caddy, Docker, services | [docs/deployment.md](docs/deployment.md) | 中文 |
| Client setup: Claude Code, Codex, stdio bridge | [docs/clients.md](docs/clients.md) | 中文 |
| Account nodes: ordinary user and root on one machine | [docs/account-nodes.md](docs/account-nodes.md) | 中文 |
| Security model | [docs/security.md](docs/security.md) | 中文 |
| External OAuth | [docs/oauth.md](docs/oauth.md) | English |
| Cloudflare Access Managed OAuth | [docs/cloudflare-access.md](docs/cloudflare-access.md) | English |
| Hub/Agent components, compatibility and updates | [docs/components-updates.md](docs/components-updates.md) | English |
| Validation records | [docs/validation/](docs/validation/README.md) | 中文 / English |

Translations of the Chinese documents are welcome.

## Contributing

Bug reports, documentation fixes and pull requests are welcome. Start with
[CONTRIBUTING.md](CONTRIBUTING.md); `make check` runs the same checks as CI.
Participation is covered by the [code of conduct](CODE_OF_CONDUCT.md).

## License

HostMCP is released under the [MIT License](LICENSE). Release archives and the
container image include the license notices of the Go modules they are built from.
