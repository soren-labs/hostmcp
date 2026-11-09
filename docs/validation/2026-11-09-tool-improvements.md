# Tool improvements validation — 2026-11-09

Scope: PRs #13–#17 and #19 — tool capability checks, Hub configuration reads,
`wait_for_exit` / `output_mode` / `clean_ansi`, file `sha256` /
`expected_sha256`, `search`, and the ops CLI (piped enrollment, `doctor` Hub
checks, `--log-format json`).

Platform: Mac mini, macOS arm64 (Apple M6), Go 1.27.3, golangci-lint 2.15.0.
End-to-end checks used a loopback port and temporary configuration.

| Check | Result |
| --- | --- |
| GitHub Actions CI (ubuntu-latest and macos-latest runtime jobs, lint, govulncheck) | Passed on each pull request |
| Local, each branch: `gofmt -l`, `go vet ./...` | Pass |
| Local, each branch: `golangci-lint run` | 0 issues |
| Local, each branch: `go test -race -count=1` for the touched packages | Pass |
| `go test -race -count=1 ./...` on `main` after #13–#16 were merged | Pass |
| `scripts/smoke.py --mode stdio` | Pass: all ten native tools |

## Benchmarks

Apple M6, `-count=8`, 100 clients, 200 devices. The same benchmark file was run
on `main` before #14 and on #14.

| Benchmark | Before | After (#14) | Change |
| --- | --- | --- | --- |
| `ToolCallHTTP` time | 411.4 µs | 350.6 µs | −14.8% |
| `ToolCallHTTP` memory | 1211 KiB | 981 KiB | |
| `ToolCallHTTP` allocations | 1644 | 1101 | |
| `Authenticate` time | 31.8 µs | 24.9 µs | −21.6% |

## Polling experiment (#15)

A command printed one line every 100 ms for about one minute. A simulated client
polled it with a 2 s model turn between calls and `yield_time_ms` 30000.

| Setting | `write_stdin` calls | Output bytes |
| --- | --- | --- |
| Without `wait_for_exit` | 34 | 2590 |
| With `wait_for_exit` | 3 | 2590 |

## Search comparison (#17)

Repository with 121 files, measured through `Runtime.Call`, `-count=6`.

| Method | Time per call |
| --- | --- |
| `search` | 1.79 ms |
| `exec_command` `grep -rnI --exclude-dir=.git` | 5.61 ms |
| `exec_command` `rg -n` | 22.2 ms |

## End-to-end checks

Real binaries: a new Hub, a new Agent enrolled with
`rmc device invite --output - | rmc agent enroll --invite -`, and the installed
legacy `hostmcp` 0.2.4-dev Agent. All 12 checks passed:

1. Piped enrollment works.
2. Both agents are online.
3. `search` works on the new agent.
4. `search` is refused for the legacy agent before dispatch.
5. Legacy arguments still reach the legacy agent.
6. `clean_ansi` is refused for the legacy agent.
7. `wait_for_exit` returns in one call.
8. `output_mode=tail` with `clean_ansi` returns the last line without colors.
9. A stale `expected_sha256` is refused and the file is unchanged.
10. A fresh `expected_sha256` writes.
11. `doctor` passes 6 of 6 checks against the running Hub.
12. `hub serve --log-format json` writes only valid JSON.

Separately, `invite --output -` refuses to print in a PTY.

## Not tested

- Linux hosts other than CI runners.
- Real remote clients (Claude Code, ChatGPT, Claude.ai) through these features,
  including whether they auto-approve the read-only `search` tool.
- Windows (unsupported).
