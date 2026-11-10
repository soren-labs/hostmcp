# Automatic updates validation — 2026-11-09

Scope: the `feat/auto-update` branch — signed automatic updates for Hub and
nodes, rollback of unhealthy versions, and the `rmc update` off switch.

Platform: Mac mini, macOS arm64, Go 1.27.3. Run on 2026-11-10 01:57–02:02 UTC
(2026-11-09 local time).

## Setup

Everything ran in a private temporary directory and on loopback ports; the real
Hub, nodes and configuration were not touched.

- A test Hub on `127.0.0.1:18787` (`hub init --allow-insecure`), supervised by a
  bash `while` loop.
- A test node enrolled to it with a piped invitation and run **under launchd**
  as a temporary LaunchAgent (`KeepAlive`, `ThrottleInterval` 5 s), so restarts
  and the in-place re-exec happen under the real macOS service manager.
- A fake GitHub releases API on `127.0.0.1:18790` (Python): each published
  release is a directory of assets. Hub and node used
  `RMC_UPDATE_API=http://127.0.0.1:18790` and `RMC_UPDATE_INTERVAL=10s`.
- Releases, all signed with the real release key `b01f7233bca74eac` and verified
  against the compiled-in key before publishing. Only `0.0.x` versions were used,
  which no real installation can ever select:

| Version | Built with | Contents |
| --- | --- | --- |
| 0.0.1 | release ldflags | Installed Hub and node binaries |
| 0.0.2 | `scripts/release.sh v0.0.2` | All 12 archives and `SHA256SUMS` |
| 0.0.3 | patched copy of the tree | Node: `node run` returns an error right after `Startup()`. Hub: unchanged |
| 0.0.4 | release ldflags | Good Hub and node archives |

## Results

| Check | Result |
| --- | --- |
| Both start in mode `stable`; first check 5 s after start: "up to date: 0.0.1" | Pass |
| `list_devices` shows the node with `auto_update: stable` | Pass |
| 0.0.2 published: node and Hub each verify the signature, download only their own archive, install and re-exec within about 6 s | Pass |
| Node re-exec under launchd keeps the same PID; launchd records 1 run and no exit, so `syscall.Exec` works under launchd | Pass |
| Hub re-exec under the bash loop: no exit seen by the supervisor | Pass |
| Node confirms health on its first Hub connection; Hub confirms after 60 s of serving | Pass |
| `list_devices` then shows the node at 0.0.2, `auto_update: stable` | Pass |
| 0.0.3 published: Hub updates to 0.0.3 and confirms health | Pass |
| Node installs 0.0.3, which fails on 3 starts; on the 4th start it restores 0.0.2, records `rolled_back: ["0.0.3"]` and reconnects. launchd: 4 runs, last exit code 1 | Pass |
| 0.0.3 is never retried: its node archive was downloaded once, and the node reported "up to date: 0.0.2" at each of about 7 further checks until updates were disabled | Pass |
| `rmc update disable` on the node, then 0.0.4 published: node logs "automatic updates are off" and downloads nothing, while the Hub updates to 0.0.4 | Pass |
| `rmc update status` reports `mode: off`, the rollback history and the last check | Pass |
| `rmc update enable`: at the next check the node goes from 0.0.2 straight to 0.0.4 (skipping the rolled-back 0.0.3), same PID, healthy | Pass |

## Defects found and fixed

1. **Busy and idle were inverted for nodes.** `node run` passed `rt.Busy` where
   the updater expects a function that reports idle. An idle node would have
   waited the full 24 hours before restarting, and a busy node would have
   restarted at once, interrupting tool calls and processes. Found while
   preparing this run and fixed before it ("node: give the updater an idle
   check, not Busy"); the run above used
   the fixed code, and the node updated within seconds while idle.
2. **Every update left its download behind.** A successful update ends in
   `syscall.Exec`, so the deferred cleanup never ran and each update left a
   `.rmc-archive-*` file of about 4 MB beside the binary: 6 files after 3
   updates each of the Hub and the node. Fixed in "update: remove the downloaded
   archive before restarting", which
   removes the archive before the restart. A unit test now checks the directory
   at the moment of restart; it fails without the fix. This fix was not
   re-run end to end.

## Not covered here

- Rollback of a node that starts but never connects within 10 minutes while the
  Hub answers `/healthz` (covered by `TestConfirmAndUnconfirmedRollback`).
- Linux and systemd. The re-exec path is the same code; Linux runs were unit
  and integration tests only.
- The real GitHub API, draft releases and `scripts/sign-release.sh` against a
  real draft.
- Upgrading existing v0.2.x installations, which is a manual one-time step.

The temporary directory, binaries, archives and the 0.0.x signatures were
deleted after the run, and the LaunchAgent was removed with `launchctl bootout`.
