# OAuth, component and updater validation — 2026-10-28

Local source-worktree validation on macOS arm64, Go 1.27.3. No deployment, live
service restart, real identity-provider login or existing application data access
was performed. The release tag `v0.2.2` below is a local packaging test label, not a
published release. Prior validation records describe earlier releases separately.

| Check | Result |
| --- | --- |
| `gofmt` on modified/new Go files; `git diff --check` | Pass |
| `go vet ./...` | Pass |
| `python3 scripts/verify-source-import.py` | Pass: 62 unchanged upstream files, provenance retained |
| `sh -n scripts/install.sh`, `bash -n scripts/release.sh` | Pass; shellcheck is unavailable in this environment |
| Python packaging script syntax | Pass |
| Native `scripts/smoke.py --mode stdio` | Pass: all nine native tools, isolated files and owned process cleanup |
| `go test ./...` | Attempted; blocked in existing agent/client/hub tests by denied TCP localhost binds |
| `go test -race ./...` | Attempted; same sandbox listener restriction |
| Full cmd/config/host/oauth/update/service package suites, normal and race | Pass |
| New Hub/Agent OAuth and compatibility tests, normal and race | Pass using in-memory HTTP and websocket framing over `net.Pipe` |
| `bash scripts/release.sh v0.2.2 build/release-smoke` | Pass: 12 archives, four cross-build targets, dependency/license notices |
| `python3 scripts/packaging-smoke.py --dist build/release-smoke --version v0.2.2` | Pass on native macOS arm64: role installs/rejections, checksum rejection, pinned offline upgrade and rollback |

Go's default build cache is outside the sandbox's write roots. Checks use a cache
under `/private/tmp`; release builds also use a temporary module cache populated
from cached dependency archives, with `GOPROXY=off`. Native process tests use
`SHELL=/bin/bash` because the inherited zsh attempts a background `nice` operation
that this sandbox denies. These test-only environment choices do not change
HostMCP's runtime environment semantics.

Reproduce the passing package subsets:

```sh
SHELL=/bin/bash go test -race ./cmd/hostmcp ./internal/config ./internal/host \
  ./internal/oauth ./internal/update ./internal/service -count=1
go test -race ./internal/hub ./internal/agent \
  -run '^(TestOAuth|TestCompatibility)' -count=1
```

OAuth negatives include wrong signature/issuer/audience, expired or absent expiry,
future nbf/iat, disallowed algorithms, missing/conflicting OAuth client identity,
invalid signing keys, discovery/key refresh failure, missing scope/unmapped subject,
owner-name privilege confusion, duplicate Authorization headers, Origin rejection
and rejection on the agent channel. An official Go MCP client makes an authenticated
scoped `list_devices` call against an in-memory resource server. Legacy bearer is
accepted in the same configuration. Client IDs separate session ownership and token
refresh preserves the same identity. Policy removal/reconfiguration fails closed.

Compatibility checks cover old hello/new welcome fields, new Agent/legacy welcome,
version mismatch rejection before dispatch, OAuth dispatch to a simulated old
wire-v1 node, running build/wire identification, heartbeat timestamp advancement
and disconnect state. These use legacy field fixtures, not installed v0.2.1 binaries.

Updater negatives cover corrupted SHA-256, wrong candidate version/component/wire,
missing executable, archive traversal/absolute paths/symlinks/hard links, disabled or
invalid policies, unsupported channel/version, unsafe target/lock permissions,
symlinked paths, concurrent update lock, backup preservation, downgrade rejection,
concurrent manual replacement, arbitrary source redirects and unknown policy URL
fields. Tests verify originals remain unchanged on pre-replacement failure and
exercise retained-binary rollback. The native packaging test uses actual built
executables in a temporary directory; unit tests also use small executable fixtures.

Remaining acceptance work: run the unmodified full suite on an unrestricted Mac
and Linux host/CI, native Linux packaging/updater smoke, real HTTPS proxy and chosen
OAuth provider/client registration/login/refresh flows, and intentional service
restart after an upgrade. Docker build and actual systemd/launchd installation were
not run. Root updates and post-restart automatic rollback are intentionally absent.
No new product-specific Codex/Claude/ChatGPT OAuth compatibility claim is made.
