# Security policy

HostMCP gives MCP clients the native authority of the OS accounts that run its
Agents. Security bugs can therefore expose remote command execution, so please
report them privately.

## Supported versions

Security fixes are made on the latest minor release line. Older lines are not
patched; upgrade to the latest release.

| Version | Supported |
| --- | --- |
| 0.2.x (latest) | Yes |
| < 0.2 | No |

## Reporting a vulnerability

Use GitHub private vulnerability reporting:
<https://github.com/soren-labs/hostmcp/security/advisories/new>

Please include:

- the HostMCP version (`hostmcp version`) and component (hub, agent or all);
- the operating system, deployment shape (direct HTTPS, Cloudflare Tunnel,
  Docker Hub, stdio) and authentication mode (bearer, OAuth, Cloudflare Access);
- minimal reproduction steps.

Do not include live tokens, invitations, real configuration files or private
host names. Do not open a public issue for a suspected vulnerability.

This is a small project maintained on a best-effort basis. The maintainers aim to
acknowledge reports within 7 days and will coordinate a fix and disclosure date
with the reporter.

## Scope

In scope — for example:

- authentication bypass on `/mcp`, `/agent/` or `/enroll`;
- a client reaching a node outside its granted device list, or another
  principal's process sessions;
- the Hub executing commands or reading files itself;
- invitation, device or client credentials written to logs, URLs or
  world-readable files;
- Agent tools acting with more privilege than the Agent's OS account;
- installer, updater or release artifact integrity problems.

Out of scope — these are the documented design:

- an authorized client running any command or reading any file that the node's
  OS account can access. HostMCP intentionally has no directory or command
  allowlist; see [docs/security.md](docs/security.md);
- privileges the OS account already has (sudo, Docker socket, admin groups);
- denial of service by a client holding a valid credential, within the
  documented resource limits.

The full trust model is described in [docs/security.md](docs/security.md).
