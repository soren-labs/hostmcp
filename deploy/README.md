# Native background services

Install the binary once and use a separate configuration and service name for
each node. A node is one machine **and one OS account**: for example,
`workstation-alice` and `workstation-root` are separate registrations, credentials,
and processes on the same workstation. Client credentials grant access to node
IDs; HostMCP does not filter that account's commands or filesystem paths.

The generated services are the recommended deployment path. The adjacent
systemd/launchd files are examples for administrators who manage units themselves.
The generator records the executable's absolute path; put the binary in its final
location before installing the service.

## Current user's service

Run enrollment and installation as the account that should execute tools:

```sh
hostmcp agent enroll --invite invitation.json --config "$HOME/.config/hostmcp/agent.json"
hostmcp service install --role agent --name hostmcp-my-node \
  --config "$HOME/.config/hostmcp/agent.json"
```

Linux uses the user's systemd manager. Enable that user's linger if the service
must run without an active login: `sudo loginctl enable-linger USER`.
macOS uses a LaunchAgent in the current GUI login session. For a machine that
must run before login, use a system service instead.

## Linux system service under an ordinary account

This example uses an existing account called `worker`. Its native groups and
privileges remain in effect. If it has sudo access, the node also has that access.

```sh
sudo install -m 0755 ./hostmcp /usr/local/bin/hostmcp
sudo -u worker /usr/local/bin/hostmcp agent enroll \
  --invite /path/readable-by-worker/invitation.json \
  --config /home/worker/.config/hostmcp/agent.json
sudo /usr/local/bin/hostmcp service install --system --user worker \
  --role agent --name hostmcp-workstation-worker \
  --config /home/worker/.config/hostmcp/agent.json
```

The service is enabled at boot, independent of the account's login session.
Its HOME, login shell and primary/supplementary groups belong to `worker`.
The configuration must be owned by `worker`, private (mode 0600), and reachable
through its parent directories. No directory allowlist, command allowlist,
`NoNewPrivileges`, or filesystem sandbox is added to node services.

## Root node

Enroll a separate root node with its own invitation, configuration and name:

```sh
sudo /usr/local/bin/hostmcp agent enroll --invite /root/root-invitation.json \
  --config /etc/hostmcp/root.json
sudo /usr/local/bin/hostmcp service install --system --user root \
  --role agent --name hostmcp-workstation-root --config /etc/hostmcp/root.json
```

A root service requires root-owned binary/configuration paths whose parents
cannot be replaced by another account. Use `/usr/local/bin` and `/etc/hostmcp`,
not a developer-owned checkout. Root access is granted by registering this node
and issuing a client credential that includes it; installation never elevates an
ordinary running node automatically.

## Hub as a Linux system service

A Hub routes operations; it does not need root. Use a dedicated existing account
and a private writable state directory:

```sh
sudo install -d -m 0700 -o hostmcp -g hostmcp /var/lib/hostmcp
sudo -u hostmcp /usr/local/bin/hostmcp hub init \
  --config /var/lib/hostmcp/hub.json \
  --listen 127.0.0.1:8787 --public-url https://mcp.example.com
sudo /usr/local/bin/hostmcp service install --system --user hostmcp \
  --role hub --name hostmcp-hub --config /var/lib/hostmcp/hub.json
```

The state directory must be writable by the Hub account because device
enrollment and credential updates replace configuration files atomically.
Put HTTPS in front of the Hub using the supplied Caddy or Cloudflare examples.

## macOS system service

The same `service install --system --user ACCOUNT` command installs a LaunchDaemon
in `/Library/LaunchDaemons`. Run it with `sudo`; use the account's real home path
when enrolling. For root on macOS, use a root-owned configuration under
`/var/root` or `/etc/hostmcp` and a root-owned binary under `/usr/local/bin`.
System service logs go to `/Library/Logs/HostMCP/SERVICE_NAME/service.log`.

System services run without a GUI login. Normal macOS privacy permissions and
SIP still apply, and an existing user's GUI/application permissions are not
automatically transferred to a background daemon.

## Inspect, upgrade and remove

On Linux:

```sh
sudo systemctl status hostmcp-workstation-worker
sudo journalctl -u hostmcp-workstation-worker -n 50
```

For user units, replace `sudo systemctl`/`sudo journalctl` with
`systemctl --user`/`journalctl --user`.

On macOS, use `sudo launchctl print system/com.hostmcp.SERVICE_NAME` for system
services, or `launchctl print gui/$(id -u)/com.hostmcp.SERVICE_NAME` for user agents.

To upgrade, install the verified release binary at the same path and restart
the named service (`systemctl restart` or `launchctl kickstart -k`). Existing
configuration and credentials stay in place. Installation refuses to overwrite
an existing unit; uninstall/reinstall when changing its account or configuration.

```sh
sudo /usr/local/bin/hostmcp service uninstall --system \
  --name hostmcp-workstation-worker
```

Use the corresponding scope when uninstalling. Removal is idempotent and stops
only the named HostMCP service; it leaves credentials, logs and user files intact.
Revoke the node/client credential in the Hub separately when retiring access.
