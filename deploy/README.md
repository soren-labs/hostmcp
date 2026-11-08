# Native background services

Install the binary once and use a separate configuration and service name for
each node. A node is one machine **and one OS account**: for example,
`workstation-alice` and `workstation-root` are separate registrations, credentials,
and processes on the same workstation. Client credentials grant access to node
IDs; Roost does not filter that account's commands or filesystem paths.

The generated services are the recommended deployment path. The adjacent
systemd/launchd files are examples for administrators who manage units themselves.
The generator records the executable's absolute path; put the binary in its final
location before installing the service.

## Current user's service

Run enrollment and installation as the account that should execute tools:

```sh
roost agent enroll --invite invitation.json --config "$HOME/.config/roost/agent.json"
roost service install --role agent --name roost-my-node \
  --config "$HOME/.config/roost/agent.json"
```

Linux uses the user's systemd manager. Enable that user's linger if the service
must run without an active login: `sudo loginctl enable-linger USER`.
macOS uses a LaunchAgent in the current GUI login session. For a machine that
must run before login, use a system service instead.

## Linux system service under an ordinary account

This example uses an existing account called `worker`. Its native groups and
privileges remain in effect. If it has sudo access, the node also has that access.

```sh
sudo install -m 0755 ./roost /usr/local/bin/roost
sudo -u worker /usr/local/bin/roost agent enroll \
  --invite /path/readable-by-worker/invitation.json \
  --config /home/worker/.config/roost/agent.json
sudo /usr/local/bin/roost service install --system --user worker \
  --role agent --name roost-workstation-worker \
  --config /home/worker/.config/roost/agent.json
```

The service is enabled at boot, independent of the account's login session.
Its HOME, login shell and primary/supplementary groups belong to `worker`.
The configuration must be owned by `worker`, private (mode 0600), and reachable
through its parent directories. No directory allowlist, command allowlist,
`NoNewPrivileges`, or filesystem sandbox is added to node services.

## Root node

Enroll a separate root node with its own invitation, configuration and name:

```sh
sudo /usr/local/bin/roost agent enroll --invite /root/root-invitation.json \
  --config /var/lib/roost/root.json
sudo /usr/local/bin/roost service install --system --user root \
  --role agent --name roost-workstation-root --config /var/lib/roost/root.json
```

A root service requires root-owned binary/configuration paths whose parents
cannot be replaced by another account. Use `/usr/local/bin` and `/etc/roost`,
not a developer-owned checkout. Root access is granted by registering this node
and issuing a client credential that includes it; installation never elevates an
ordinary running node automatically.

## Hub as a Linux system service

A Hub routes operations; it does not need root. Use a dedicated existing account
and a private writable state directory:

```sh
sudo install -d -m 0700 -o roost -g roost /var/lib/roost
sudo -u roost /usr/local/bin/roost hub init \
  --config /var/lib/roost/hub.json \
  --listen 127.0.0.1:8787 --public-url https://mcp.example.com
sudo /usr/local/bin/roost service install --system --user roost \
  --role hub --name roost-hub --config /var/lib/roost/hub.json
```

The state directory must be writable by the Hub account because device
enrollment and credential updates replace configuration files atomically.
Put HTTPS in front of the Hub using the supplied Caddy or Cloudflare examples.

## macOS system service

The same `service install --system --user ACCOUNT` command installs a LaunchDaemon
in `/Library/LaunchDaemons`. Run it with `sudo`; use the account's real home path
when enrolling. For root on macOS, use a root-owned configuration under
`/var/root` or `/etc/roost` and a root-owned binary under `/usr/local/bin`.
System service logs go to `/Library/Logs/Roost/SERVICE_NAME/service.log`.

System services run without a GUI login. Normal macOS privacy permissions and
SIP still apply, and an existing user's GUI/application permissions are not
automatically transferred to a background daemon.

## Inspect, upgrade and remove

On Linux:

```sh
sudo systemctl status roost-workstation-worker
sudo journalctl -u roost-workstation-worker -n 50
```

For user units, replace `sudo systemctl`/`sudo journalctl` with
`systemctl --user`/`journalctl --user`.

On macOS, use `sudo launchctl print system/com.roost.SERVICE_NAME` for system
services, or `launchctl print gui/$(id -u)/com.roost.SERVICE_NAME` for user agents.

To upgrade, install the verified release binary at the same path and restart
the named service (`systemctl restart` or `launchctl kickstart -k`). Existing
configuration and credentials stay in place. Installation refuses to overwrite
an existing unit; uninstall/reinstall when changing its account or configuration.

```sh
sudo /usr/local/bin/roost service uninstall --system \
  --name roost-workstation-worker
```

Use the corresponding scope when uninstalling. Removal is idempotent and stops
only the named Roost service; it leaves credentials, logs and user files intact.
Revoke the node/client credential in the Hub separately when retiring access.
