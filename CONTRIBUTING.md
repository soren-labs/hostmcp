# Contributing to HostMCP

HostMCP provides native Linux/macOS user capabilities through MCP. Read AGENTS.md
before changing the runtime. The upstream reference tree is immutable evidence,
not the active implementation.

## Development

Use Go 1.25 or a newer compatible toolchain and Python 3 for source verification.
The runtime is built from cmd/hostmcp and internal packages.

```sh
python3 scripts/verify-source-import.py
go test -race ./...
go vet ./...
go build -trimpath -o build/hostmcp ./cmd/hostmcp
```

Use isolated temporary directories, test-only credentials, and dedicated Hub/Agent
processes. Never connect tests to an existing Chat On Steroids installation or read
its user data. Keep production configuration, real tokens and device details out of
fixtures and logs.

## Changes and tests

- Preserve normal OS user permissions and real path semantics. Authentication,
  session ownership, input validation and bounded resources remain separate concerns.
- Keep Hub routing separate from native command execution.
- Add regression tests for meaningful failure modes, especially authentication,
  cross-device/session mixups, output limits and process cleanup.
- Test protocol behavior through an MCP client when changing the transport.
- Report the platform actually tested. A cross-compiled Darwin executable does not
  prove macOS runtime behavior, and a checked-in workflow is not evidence of a CI run.
- Update docs/validation.md with reproducible results and limitations, without secrets.

## Reference source

Do not edit reference/chat-on-steroids/, provenance hashes, upstream notices, or
the existing source-baseline workflow to make a check pass. Adapt ideas into the
new runtime. Preserve any applicable copyright/license attribution when porting
source. The historical extraction plan records the earlier design; current choices
belong in docs/architecture.md.

## Release packaging

The packaging script builds four archives and a SHA256SUMS file without publishing:

```sh
bash scripts/release.sh v0.1.0
```

Archive names are hostmcp_VERSION_OS_ARCH.tar.gz for linux/darwin and amd64/arm64.
Each contains the binary, LICENSE, NOTICE and dependency license notices. The
installer verifies the selected archive checksum and writes to a user-owned prefix.
It can be tested without a network download:

```sh
sh scripts/install.sh --version v0.1.0 \
  --archive dist/hostmcp_0.1.0_linux_amd64.tar.gz \
  --checksums dist/SHA256SUMS --prefix /tmp/hostmcp-install-check
```

Use the archive for the test machine's OS/architecture. The release workflow runs
Linux/macOS tests before building artifacts; a version tag triggers GitHub release
publication. A manual workflow on a branch builds reviewable artifacts only.
Publishing a tag/release is a separate maintainer action. Do not mark a version
released or a validation passed before the relevant operation succeeds.

Release artifacts have SHA-256 integrity checks. These checks are not a claim of
code signing, notarization, independent supply-chain attestation, or reproducible
build bytes.
