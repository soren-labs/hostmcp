# Releasing RMC

Hubs and nodes update themselves to new releases (see
[automatic updates](components-updates.md#automatic-updates)), so publishing a
release is also a fleet rollout. Two rules keep that safe:

- **CI never publishes.** A pushed tag builds and tests the release, then creates
  a *draft*. Updaters ignore drafts.
- **Devices install only signed releases.** The owner signs `SHA256SUMS` with the
  offline release key on their own machine, then publishes. A release without a
  valid `SHA256SUMS.sig` is never installed, even if it is published.

Every version goes to a few canary nodes as a release candidate before it goes
to everyone.

## Versions

Tags are `vX.Y.Z` (stable) or `vX.Y.Z-rc.N` (release candidate, N ≥ 1). A release
candidate sorts before its final release: `0.3.0-rc.2` < `0.3.0`.
`scripts/release.sh` refuses any other form, and updaters ignore other tags.

| Channel | Installs | Set with |
| --- | --- | --- |
| `stable` (default) | Published stable releases only | `rmc update channel stable` |
| `canary` | Stable releases and published release candidates | `rmc update channel canary` |

## One-time setup: the release key

Generate the key on the machine that will sign releases. It never goes to GitHub
or CI.

```sh
rmc release keygen --output ~/.config/rmc/release-signing.key
```

The command writes the private key with mode `0600` and prints the public key
line (`rmc-release-ed25519:…`). Add that line to `trustedReleaseKeys` in
`internal/update/keys.go` and ship it in a release; only builds that contain the
public key trust releases signed with it.

**Back up the private key offline** (for example, on an encrypted USB drive kept
apart from the machine). If it is lost, no new release can be signed, and every
installation must be upgraded by hand to a build that trusts a new key. Key
rotation is described in the [security model](security.md#自动更新与发布签名).

## Release steps

1. **Prepare.** Merge everything for the release into `main`, with green CI. Keep
   the notes under `## Unreleased` in `CHANGELOG.md`; a release candidate uses
   that section when it has no section of its own.

2. **Tag a release candidate.**

   ```sh
   git tag v0.3.0-rc.1 && git push origin v0.3.0-rc.1
   ```

   The Release workflow runs vet, race tests and the stdio smoke test on Linux
   and macOS, builds the 12 archives with `SHA256SUMS`, runs the packaging smoke
   test, and creates a **draft prerelease**.

3. **Sign and publish the candidate** on the machine that holds the key:

   ```sh
   bash scripts/sign-release.sh v0.3.0-rc.1
   ```

   The script downloads the draft and checks that all 12 archives are present and
   match `SHA256SUMS`, runs this machine's archive to confirm it reports the
   tagged version, signs, verifies the signature against the compiled-in keys,
   asks for confirmation, uploads `SHA256SUMS.sig` and publishes the release as
   a prerelease. Options: `--key FILE` (default `$RMC_RELEASE_KEY` or
   `~/.config/rmc/release-signing.key`) and `--yes` to skip the prompt.

4. **Watch the canaries.** Only Hubs and nodes on the `canary` channel install a
   release candidate. Put two or three representative nodes on it once:

   ```sh
   rmc-node update channel canary --config /absolute/path/node.json
   ```

   The setting applies at the node's next check. A running service checks 2–9
   minutes after it starts and then about hourly (±10 minutes), so a canary
   installs the candidate within about 70 minutes. Then check:

   - `list_devices`: the canaries report the new version and are online;
   - `rmc update status --config …` on a canary: `last_update`, `rolled_back`
     and `last_result`;
   - the service logs: lines with `event=auto_update`.

   A version that does not become healthy is rolled back on that node
   automatically and listed under `rolled_back`. To fix a problem, tag
   `v0.3.0-rc.2` and repeat from step 2. Stable nodes never see release
   candidates.

5. **Release.** Move the notes from `## Unreleased` to a `## v0.3.0 — DATE`
   section, merge, then tag and sign the final version:

   ```sh
   git tag v0.3.0 && git push origin v0.3.0
   # after the Release workflow has created the draft:
   bash scripts/sign-release.sh v0.3.0
   ```

   For a stable tag, `sign-release.sh` refuses to continue unless a release
   candidate of the same version (`v0.3.0-rc.N`) was published. Pass
   `--skip-canary` only for an urgent fix you have tested another way.
   Publishing marks the release as latest; every Hub and node with automatic
   updates on installs it within about 70 minutes.

## Stopping a bad release

- **Before publishing:** delete the draft, or leave it; drafts are never
  installed.
- **After publishing:** turn the release back into a draft
  (`gh release edit v0.3.0 --draft`). Updaters skip it from their next check.
  Installations that already updated keep running it; a version that fails
  health checks rolls itself back, but a version that is healthy yet wrong does
  not. Publish a fixed, higher version (for example `v0.3.1`). Updaters never
  downgrade.
- **On one machine:** `rmc update disable --config …` stops automatic updates
  there; `rmc update enable` turns them back on.

## What CI and the signer each check

| Step | Where | Checks |
| --- | --- | --- |
| Release workflow | GitHub Actions | `go vet`, `go test -race`, stdio smoke test, 12 archives, `SHA256SUMS`, packaging smoke test; creates a draft |
| `scripts/sign-release.sh` | Key machine | Draft exists; every archive matches `SHA256SUMS`; this platform's archive reports the tag's version; a release candidate was published first; signature verifies |
| Updater on each device | Every Hub and node | Signature over the tag and `SHA256SUMS` from a trusted key; archive SHA-256; the extracted executable reports the expected version, component, platform and wire version |
