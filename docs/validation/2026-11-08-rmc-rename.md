# RMC rename compatibility — 2026-11-08

Branch `rename/rmc`, macOS 26 arm64 (Mac mini), Go 1.27.3. All runs used
isolated temporary directories, loopback listeners and freshly generated
test-only credentials; nothing connected to a deployed Hub.

| Check | Result |
| --- | --- |
| `gofmt -l cmd internal`, `go vet ./...` | Pass |
| `go test -race -count=1 ./...` | Pass (all packages) |
| `make smoke` (`scripts/smoke.py --mode stdio`) | Pass: 9 tools, protocol 2025-11-25 |
| `bash scripts/release.sh v0.3.0 /tmp/rmc-dist` | Pass: 16 archives (12 `rmc*`, 4 legacy `hostmcp-agent_*`), `SHA256SUMS` lists all 16 |
| Legacy archive contents | `hostmcp-agent_0.3.0_darwin_arm64.tar.gz` contains `hostmcp-agent`, `LICENSE`, READMEs, docs, licenses |
| v0.2.4 updater (built from `41d4316`) + legacy archive | Pass: binary replaced, `version` reports `0.3.0`, `.previous` retained; `--rollback` restores `0.2.4` |
| v0.2.4 updater + `rmc-agent_*` archive | Refused (`agent executable is absent`), confirming the legacy archive is required |
| v0.2.4 Hub + RMC Agent (loopback) | Pass: enroll, `list_devices` online, `exec_command` output |
| RMC Hub + v0.2.4 Agent (loopback) | Pass: enroll, `list_devices` online, `exec_command` output |
| v0.2.4 `call` client + RMC Hub and Agent | Pass |

Not run here: `golangci-lint` (not installed on this host; covered by CI) and
service installation on real systemd/launchd hosts. The legacy service-name
paths are covered by unit tests in `internal/service/naming_test.go`.
