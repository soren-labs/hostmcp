# Cloudflare Access validation — v0.2.3 source, 2026-10-28

Validated locally on macOS arm64 with Go 1.27.3, on
`feat/cloudflare-access-oauth` from main/v0.2.2 commit `469d98ac3856`.
No production config, deployed service, secret, other worktree's source or imported
snapshot was changed. No real Cloudflare team, ChatGPT login, GUI or installed
application was contacted. Local `v0.2.3` archives are packaging fixtures, not a
published release or tag.

| Check | Result |
| --- | --- |
| `gofmt` on changed/new Go files; `git diff --check` | Pass |
| `go vet ./...` | Pass |
| `python3 scripts/verify-source-import.py` | Pass: 62 unchanged upstream files, provenance hashes retained |
| `go test ./...` | Attempted; existing agent/client/Hub TCP-listener tests fail because sandbox denies localhost binds |
| `go test -race ./...` | Attempted; same existing TCP-listener restriction; no race reported |
| Full cloudflareaccess/config/cmd/host/oauth/service/update suites, normal and race | Pass |
| Hub/Agent `TestAccess`, `TestOAuth`, `TestCompatibility`, normal and race | Pass using in-memory HTTP and WebSockets over `net.Pipe` |
| Final Hub `TestAccess` suite including SDK negative matrix, normal and race | Pass |
| `bash scripts/release.sh v0.2.3 build/cloudflare-access-release` | Pass: 12 archives, Linux/macOS amd64/arm64 cross-builds and notices |
| `python3 scripts/packaging-smoke.py --dist build/cloudflare-access-release --version v0.2.3` | Pass on native macOS arm64: archive checksums, component roles, isolated installs, checksum rejection, offline update/rollback |

The sandbox prevents writes to Go's default cache. Checks used a worktree-local
`GOCACHE`; packaging used a local `GOMODCACHE` populated only from cached archives
of declared dependencies, with `GOPROXY=off GOSUMDB=off`. Nothing was downloaded.
Native process tests used `SHELL=/bin/bash`: inherited zsh background jobs attempt
`nice`, which this sandbox denies. Full runs with Bash still fail only at the
existing TCP bind restriction. These are test environments, with no change to
HostMCP's runtime environment semantics.

Reproduce the passing suites in an ordinary development checkout:

```sh
SHELL=/bin/bash go test -race ./internal/cloudflareaccess ./internal/config \
  ./cmd/hostmcp ./internal/host ./internal/oauth ./internal/service ./internal/update -count=1
go test -race ./internal/hub ./internal/agent \
  -run '^(TestAccess|TestOAuth|TestCompatibility)' -count=1
```

The Access tests generate ephemeral RSA signing keys and bounded JWKS fixtures.
They verify signature, RS256-only algorithm selection, exact issuer/application
audience, mandatory expiry, future nbf/iat rejection, normalized email/nonempty
subject, unknown/missing keys, forged/unsigned/malformed/oversized assertions,
duplicate assertion/Authorization headers and ambiguous JWT fields. Key tests cover
rotation/removal, concurrent refresh, refresh throttling, bounded cache expiry,
malformed/empty/oversized/duplicate key sets, unusable RSA keys, redirects, timeout
and startup failure. No insecure production transport option was added.

The official Go MCP SDK initializes and calls `list_devices` through in-memory
HTTP. The SDK matrix covers missing, forged, expired, wrong-audience, wrong-issuer,
unknown-email and duplicate assertions, the untrusted synthetic bearer, legacy
bearers, and valid assertions alongside opaque/legacy bearers. Access sees only
its explicit node even when a legacy owner bearer is present. A simulated legacy
wire-v1 Agent receives the validated synthetic principal, while unauthorized-node
dispatch is denied. Actual temporary process sessions reject unrelated principals
and retain ownership across assertion refresh. Host/Origin rejection, policy
revocation at dispatch/on existing MCP connections, enabled-after-start rejection,
incompatible settings/restart isolation, mode coexistence, request cloning,
token-free logging and agent/enrollment credential separation are covered.

Remaining acceptance: run the unmodified full suites on unrestricted macOS and
Linux; execute native Linux packaging tests; verify the real HTTPS/Tunnel/Access
application and ChatGPT authorization-code/PKCE login, refresh, mapped node calls,
revocation and deliberate Hub restart. Cross-compilation is not native Linux
runtime acceptance. No ChatGPT/Cloudflare production compatibility claim is made.
