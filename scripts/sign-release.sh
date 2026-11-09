#!/usr/bin/env bash
# Sign a draft GitHub release with the offline release key, then publish it.
# Devices install only releases whose SHA256SUMS.sig verifies, so nothing
# reaches them until this script runs on the machine that holds the key.
#
# Usage: bash scripts/sign-release.sh vX.Y.Z[-rc.N] [--key FILE] [--yes] [--skip-canary]
#   --key FILE      private key (default: $RMC_RELEASE_KEY or ~/.config/rmc/release-signing.key)
#   --yes           publish without asking
#   --skip-canary   publish a stable release that had no published release candidate
set -euo pipefail

tag="${1:-}"
shift || true
key="${RMC_RELEASE_KEY:-$HOME/.config/rmc/release-signing.key}"
assume_yes=false
skip_canary=false
while [[ $# -gt 0 ]]; do
  case "$1" in
    --key) key="${2:?--key needs a file}"; shift 2 ;;
    --yes) assume_yes=true; shift ;;
    --skip-canary) skip_canary=true; shift ;;
    *) printf 'Unknown option: %s\n' "$1" >&2; exit 2 ;;
  esac
done
if [[ ! "$tag" =~ ^v[0-9]+\.[0-9]+\.[0-9]+(-rc\.[1-9][0-9]*)?$ ]]; then
  printf '%s\n' 'Usage: bash scripts/sign-release.sh vX.Y.Z[-rc.N] [--key FILE] [--yes] [--skip-canary]' >&2
  exit 2
fi
version="${tag#v}"
repo="${GH_REPO:-soren-labs/rmc}"
repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
for executable in gh go python3 tar; do
  command -v "$executable" >/dev/null || { printf 'Missing command: %s\n' "$executable" >&2; exit 1; }
done
[[ -f "$key" ]] || { printf 'No release key at %s\n' "$key" >&2; exit 1; }

state="$(gh release view "$tag" --repo "$repo" --json isDraft,isPrerelease --jq '"\(.isDraft) \(.isPrerelease)"')"
if [[ "$state" != "true "* ]]; then
  printf '%s is not a draft release; nothing to sign.\n' "$tag" >&2
  exit 1
fi
if [[ "$tag" != *-rc.* && "$skip_canary" != true ]]; then
  published_rc="$(gh release list --repo "$repo" --limit 100 --json tagName,isDraft \
    --jq "[.[] | select(.isDraft | not) | select(.tagName | startswith(\"$tag-rc.\"))] | length")"
  if [[ "$published_rc" == 0 ]]; then
    printf 'No published release candidate %s-rc.N: canary nodes have not run this version.\n' "$tag" >&2
    printf 'Publish an -rc first, or pass --skip-canary.\n' >&2
    exit 1
  fi
fi

work="$(mktemp -d "${TMPDIR:-/tmp}/rmc-sign.XXXXXX")"
trap 'rm -rf "$work"' EXIT
mkdir "$work/assets" "$work/bin"
gh release download "$tag" --repo "$repo" --dir "$work/assets" --skip-existing
rm -f "$work/assets/SHA256SUMS.sig"

# Every asset must be listed, every listed archive present and matching, and
# the twelve component archives must all exist.
python3 - "$work/assets" "$version" <<'PY'
import hashlib, pathlib, sys
folder, version = pathlib.Path(sys.argv[1]), sys.argv[2]
sums = {}
for line in (folder / "SHA256SUMS").read_text().splitlines():
    digest, name = line.split()
    sums[name.lstrip("*")] = digest
expected = {f"{c}_{version}_{o}_{a}.tar.gz" for c in ("rmc", "rmc-hub", "rmc-node") for o in ("darwin", "linux") for a in ("amd64", "arm64")}
present = {p.name for p in folder.iterdir()} - {"SHA256SUMS"}
problems = sorted(expected - set(sums)) + sorted(present - set(sums)) + sorted(set(sums) - present)
for name in sorted(set(sums) & present):
    if hashlib.sha256((folder / name).read_bytes()).hexdigest() != sums[name]:
        problems.append("checksum mismatch: " + name)
if problems:
    sys.exit("release assets do not match SHA256SUMS: " + ", ".join(problems))
print(f"{len(sums)} assets match SHA256SUMS")
PY

# The archive for this machine must run and report the tagged version.
os_name="$(uname -s | tr '[:upper:]' '[:lower:]')"
case "$(uname -m)" in x86_64|amd64) arch=amd64 ;; arm64|aarch64) arch=arm64 ;; *) arch="$(uname -m)" ;; esac
tar -xzf "$work/assets/rmc_${version}_${os_name}_${arch}.tar.gz" -C "$work/bin" rmc
reported="$("$work/bin/rmc" version | python3 -c 'import json,sys; print(json.load(sys.stdin)["version"])')"
[[ "$reported" == "$version" ]] || { printf 'Archive reports version %s, not %s\n' "$reported" "$version" >&2; exit 1; }

# Sign with this checkout's code, then verify against the compiled-in keys.
(cd "$repo_root" && go build -o "$work/signer" ./cmd/rmc)
"$work/signer" release sign --key "$key" --tag "$tag" --sums "$work/assets/SHA256SUMS" --output "$work/SHA256SUMS.sig"
"$work/signer" release verify --tag "$tag" --sums "$work/assets/SHA256SUMS" --sig "$work/SHA256SUMS.sig"

if [[ "$tag" == *-rc.* ]]; then
  audience='canary nodes (channel canary) only'
else
  audience='EVERY Hub and node with automatic updates on'
fi
if [[ "$assume_yes" != true ]]; then
  printf 'Publish %s? Within about an hour it reaches %s. [y/N] ' "$tag" "$audience"
  read -r answer
  [[ "$answer" == y || "$answer" == Y ]] || { printf 'Not published; the release stays a draft.\n'; exit 1; }
fi
gh release upload "$tag" "$work/SHA256SUMS.sig" --repo "$repo" --clobber
if [[ "$tag" == *-rc.* ]]; then
  gh release edit "$tag" --repo "$repo" --draft=false --prerelease
else
  gh release edit "$tag" --repo "$repo" --draft=false --latest
fi
printf 'Published %s; it reaches %s.\n' "$tag" "$audience"
