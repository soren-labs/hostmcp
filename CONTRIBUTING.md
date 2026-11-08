# Contributing to RMC

RMC provides native Linux/macOS user capabilities through MCP. Read AGENTS.md
before changing the runtime.

## Development

Use Go 1.25 or a newer compatible toolchain, Python 3 for the smoke and packaging
checks, and [golangci-lint](https://golangci-lint.run/welcome/install/) v2.
The runtime is built from `cmd/rmc` and the `internal/` packages.

```sh
make check    # gofmt, vet, golangci-lint, race tests, stdio smoke test
make build    # ./build/rmc
make vuln     # govulncheck
```

`make check` runs the same checks as CI. The individual commands are in the
`Makefile` if you prefer to run them directly.

Use isolated temporary directories, test-only credentials, and dedicated Hub/Agent
processes. Never connect tests to a production Hub or read real user
data. Keep production configuration, real tokens and device details out of
fixtures and logs.

## Pull requests

- Open an issue first for larger changes, especially anything that touches the
  permission model, authentication or the Hub↔Agent wire protocol.
- Keep each pull request focused. Fill in the template, including the platforms
  you actually tested on.
- Add user-visible changes to the `Unreleased` section of `CHANGELOG.md`.
- CI must pass. `main` is protected; changes land through pull requests.

### Optional 200-node connection/routing check

Normal CI skips this test. Point it at an already built executable to start a
separate temporary Hub and 200 simulated WebSocket peers:

```sh
RMC_SCALE_BINARY="$PWD/build/rmc" \
  go test ./internal/hub -run '^TestScaleAccountNodes$' -count=1 -v -timeout=2m
```

The check verifies all 200 online entries, 200 correctly routed authenticated MCP
calls with eight requests in flight, and 20 disconnected/reconnected peers followed
by another route check. It respects the Hub's current eight-request/handshake
resource bound. It never uses an existing Hub, credentials, inventory or endpoint.
Temporary state and processes are cleaned up by the test.

On Linux, its JSON `RMC_SCALE_REPORT` includes observed RSS and CPU samples
for only the separate Hub process. The simulated peers run in the test process.
These are bounded loopback protocol/load measurements, not 200 physical hosts,
200 simultaneous native workloads, TLS proxy resource measurements or a long-term
availability guarantee. Test actual OS-account execution separately with real nodes.

To run on a low-memory Linux VPS without compiling there, build the test executable
on a development machine and copy it alongside the matching RMC binary:

```sh
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go test -c \
  -o build/rmc-hub-scale.test ./internal/hub
RMC_SCALE_BINARY=/absolute/path/rmc \
  ./rmc-hub-scale.test -test.run '^TestScaleAccountNodes$' -test.v -test.timeout 2m
```

## Changes and tests

- Preserve normal OS user permissions and real path semantics. Authentication,
  session ownership, input validation and bounded resources remain separate concerns.
- Keep Hub routing separate from native command execution.
- Add regression tests for meaningful failure modes, especially authentication,
  cross-device/session mixups, output limits and process cleanup.
- Test protocol behavior through an MCP client when changing the transport.
- Report the platform actually tested. A cross-compiled Darwin executable does not
  prove macOS runtime behavior, and a checked-in workflow is not evidence of a CI run.
- Record reproducible results and limitations under docs/validation/, without secrets.

## Third-party code

Write new code rather than copying it. If you must include code from another
project, keep its copyright and license notices, check that its license is
compatible with MIT, and say so in the pull request. Design decisions belong in
docs/architecture.md.

## Releasing

1. Move the `Unreleased` entries in `CHANGELOG.md` under a new `## vX.Y.Z — YYYY-MM-DD`
   heading. The release workflow publishes that section as the GitHub release notes
   and fails if it is missing.
2. Update the version in the install examples of `README.md` and `README.zh-CN.md`.
3. Merge to `main`, then push the tag: `git tag vX.Y.Z && git push origin vX.Y.Z`.
4. After publishing, set the source version to the next development version
   (`version` in `cmd/rmc/main.go` and `Version` in `internal/protocol/types.go`,
   for example `0.2.5-dev`).

## Release packaging

The release workflow pins Go 1.27.3 for validation and archive builds. Runtime CI
uses the minimum Go version declared in go.mod to check baseline compatibility.

The packaging script builds four archives and a SHA256SUMS file without publishing:

```sh
bash scripts/release.sh v0.2.3
```

Archive names are rmc_VERSION_OS_ARCH.tar.gz for linux/darwin and amd64/arm64.
Each contains the binary, LICENSE and dependency license notices. The
installer verifies the selected archive checksum and writes to a user-owned prefix.
It can be tested without a network download:

```sh
sh scripts/install.sh --version v0.2.3 \
  --archive dist/rmc_0.2.3_linux_amd64.tar.gz \
  --checksums dist/SHA256SUMS --prefix /tmp/rmc-install-check
```

Use the archive for the test machine's OS/architecture. The release workflow runs
Linux/macOS tests before building artifacts; a version tag triggers GitHub release
publication. A manual workflow on a branch builds reviewable artifacts only.
Publishing a tag/release is a separate maintainer action. Do not mark a version
released or a validation passed before the relevant operation succeeds.

Release artifacts have SHA-256 integrity checks. These checks are not a claim of
code signing, notarization, independent supply-chain attestation, or reproducible
build bytes.
