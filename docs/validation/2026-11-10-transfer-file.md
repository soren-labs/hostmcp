# Tool ergonomics and transfer_file validation — 2026-11-10

Scope: #25 (`search.context_lines` and dependency-directory skips, anchored
`search` patterns fixed, `read.tail_lines`, Hub device hints, idle-safe restart
of an already-installed update) and this change (`transfer_file`, `move_path`,
`read.offset`, `write_file.append` with `expected_size`).

Platforms: Mac mini, macOS arm64, and the Ubuntu workstation, Linux x86_64;
Go 1.27.3 and golangci-lint 2.15.0 on both. The end-to-end check ran on the
Mac mini only, on a loopback port with temporary configuration and test-only
credentials, removed afterwards. No real Hub, node or user data was involved.

| Check | Result |
| --- | --- |
| Local run of every `ci.yml` step (script syntax, `make fmt`, vet, `go test -race ./...`, build, stdio smoke, version/doctor, golangci-lint; govulncheck v1.9.0 on macOS) for #25 at `ad34050` and this change at `5ead121`, Linux x86_64 and macOS arm64 | Pass on both |
| GitHub Actions CI on #25 (ubuntu-latest and macos-latest runtime, lint, govulncheck) | Pass |
| `scripts/smoke.py --mode stdio` | Pass: all eleven native tools, including a chunked base64 copy with `read.offset`, `write_file.append`/`expected_size` and `move_path` |
| `TestTransferFilePollCancelAndSourceChange`, 20 repeated runs | Pass |
| `go test -race -count=3 -run TestTransfer ./internal/hub` | Pass |
| Regression tests that fail on the previous code: `TestAutoUpdateAlreadyInstalledWaitsForIdle`, the anchored-pattern case of `TestSearchContent` | Fail before, pass after |

## End to end (Mac mini, real binary)

A temporary Hub (`hub init`, `hub serve` on `127.0.0.1`), two nodes enrolled
with piped invitations (`device invite --output - | node enroll --invite -`),
and `rmc call` with the owner token:

| Check | Result |
| --- | --- |
| `tools/list` | 13 tools, including `move_path` and `transfer_file` |
| `transfer_file` of a 40 MiB random file between the two nodes, `target_path` ending in `/` | `done` in 418 ms (about 90 MB/s over loopback), `verified: size`; sha256 of source, copy and result equal; mode 0750 kept; no `.rmc-transfer-*` file left |
| The same transfer again without `overwrite` | Tool error "target_path already exists", nothing written |
| A 14-byte file, then `read.tail_lines` on the copy | `verified: sha256`; tail returned the last two lines |
| Hub log | One `event=tool_call` line per chunk call and `event=transfer` start/finish lines with size and elapsed time; no paths or contents |

Not tested: a transfer between two different machines through a production
Hub. The in-process tests cover Linux and macOS nodes separately; the fleet
gets the feature with the next release.
