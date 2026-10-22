#!/usr/bin/env bash
# Build four portable release archives and SHA256SUMS. Does not push tags or publish.
set -euo pipefail

release_version="${1:-}"
if [[ -z "$release_version" ]]; then
  printf '%s\n' 'Usage: bash scripts/release.sh v0.1.0 [OUTPUT_DIRECTORY]' >&2
  exit 2
fi
release_version="${release_version#v}"
if [[ ! "$release_version" =~ ^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z][0-9A-Za-z.-]*)?$ ]]; then
  printf '%s\n' 'Invalid release version' >&2
  exit 2
fi
repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
output_dir="${2:-$repo_root/dist}"
mkdir -p "$output_dir"
output_dir="$(cd "$output_dir" && pwd)"
cd "$repo_root"

for executable in go git python3 tar; do
  command -v "$executable" >/dev/null || { printf 'Missing command: %s\n' "$executable" >&2; exit 1; }
done
python3 scripts/verify-source-import.py
go mod download all
release_commit="$(git rev-parse --short=12 HEAD)"
release_time="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
temp_dir="$(mktemp -d "${TMPDIR:-/tmp}/hostmcp-release.XXXXXX")"
trap 'rm -rf "$temp_dir"' EXIT

# A statically linked binary still needs its dependency copyright/license notices.
go list -m -json all > "$temp_dir/modules.json"
python3 - "$temp_dir/modules.json" "$temp_dir/licenses" <<'PY'
import json
import pathlib
import shutil
import sys

text = pathlib.Path(sys.argv[1]).read_text()
destination = pathlib.Path(sys.argv[2])
destination.mkdir()
decoder = json.JSONDecoder()
modules = []
while text.strip():
    text = text.lstrip()
    module, length = decoder.raw_decode(text)
    text = text[length:]
    if module.get("Main"):
        continue
    actual = module.get("Replace", module)
    if "Dir" not in actual:
        raise SystemExit("Dependency source not available: " + module["Path"])
    source = pathlib.Path(actual["Dir"])
    folder = destination / (module["Path"].replace("/", "__") + "@" + module.get("Version", "local"))
    folder.mkdir()
    found = []
    for item in source.iterdir():
        if item.name.upper().startswith(("LICENSE", "LICENCE", "COPYING", "NOTICE", "PATENTS")):
            if item.is_dir():
                shutil.copytree(item, folder / item.name)
            else:
                shutil.copyfile(item, folder / item.name)
            found.append(item.name)
    if not found:
        raise SystemExit("No license notice found for " + module["Path"])
    modules.append({"module": module["Path"], "version": module.get("Version"), "notices": sorted(found)})
(destination / "modules.json").write_text(json.dumps(modules, indent=2) + "\n")
PY
cp -R reference/chat-on-steroids/docs/licenses/codex "$temp_dir/licenses/upstream-codex"

for target in linux/amd64 linux/arm64 darwin/amd64 darwin/arm64; do
  target_os="${target%/*}"
  target_arch="${target#*/}"
  package_dir="$temp_dir/${target_os}_${target_arch}"
  mkdir -p "$package_dir"
  CGO_ENABLED=0 GOOS="$target_os" GOARCH="$target_arch" go build -trimpath \
    -ldflags "-s -w -X main.version=$release_version -X main.commit=$release_commit -X main.buildTime=$release_time" \
    -o "$package_dir/hostmcp" ./cmd/hostmcp
  cp LICENSE NOTICE "$package_dir/"
  cp -R "$temp_dir/licenses" "$package_dir/licenses"
  COPYFILE_DISABLE=1 tar -C "$package_dir" -czf "$output_dir/hostmcp_${release_version}_${target_os}_${target_arch}.tar.gz" \
    hostmcp LICENSE NOTICE licenses
done

python3 - "$output_dir" "$release_version" <<'PY'
import hashlib
import pathlib
import sys

folder = pathlib.Path(sys.argv[1])
version = sys.argv[2]
names = [f"hostmcp_{version}_{os}_{arch}.tar.gz" for os in ("darwin", "linux") for arch in ("amd64", "arm64")]
lines = []
for name in names:
    digest = hashlib.sha256()
    with (folder / name).open("rb") as file:
        for chunk in iter(lambda: file.read(1024 * 1024), b""):
            digest.update(chunk)
    lines.append(f"{digest.hexdigest()}  {name}\n")
(folder / "SHA256SUMS").write_text("".join(lines))
PY
printf 'Built HostMCP %s (%s) in %s\n' "$release_version" "$release_commit" "$output_dir"
