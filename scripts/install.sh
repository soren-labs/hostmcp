#!/bin/sh
# Install a released HostMCP binary into a user-owned directory.
set -eu

usage() {
  cat <<'EOF'
Usage: sh install.sh --version v0.2.0 [--prefix DIR] [--repository OWNER/REPO]
       sh install.sh --version v0.2.0 --archive PATH --checksums PATH [--prefix DIR]

Downloads the requested GitHub release, verifies SHA-256, and installs hostmcp.
Default destination: $HOME/.local/bin. No privilege escalation or service changes.
The local archive mode also verifies the matching entry in SHA256SUMS.
EOF
}

fail() { printf '%s\n' "hostmcp installer: $*" >&2; exit 1; }
need_value() { [ "$#" -ge 2 ] && [ -n "$2" ] || fail "missing value for $1"; }

version=''
prefix="${HOME:?HOME must be set}/.local/bin"
repository='soren-labs/hostmcp'
local_archive=''
local_checksums=''
while [ "$#" -gt 0 ]; do
  case "$1" in
    --version) need_value "$@"; version="$2"; shift 2 ;;
    --prefix) need_value "$@"; prefix="$2"; shift 2 ;;
    --repository) need_value "$@"; repository="$2"; shift 2 ;;
    --archive) need_value "$@"; local_archive="$2"; shift 2 ;;
    --checksums) need_value "$@"; local_checksums="$2"; shift 2 ;;
    -h|--help) usage; exit 0 ;;
    *) fail "unknown argument: $1" ;;
  esac
done

[ -n "$version" ] || { usage >&2; fail '--version is required'; }
version="${version#v}"
printf '%s\n' "$version" | LC_ALL=C grep -Eq '^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z][0-9A-Za-z.-]*)?$' || fail 'invalid version'
printf '%s\n' "$repository" | LC_ALL=C grep -Eq '^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$' || fail 'invalid repository'
case "$(uname -s)" in
  Linux) target_os=linux ;;
  Darwin) target_os=darwin ;;
  *) fail 'supported systems are Linux and macOS' ;;
esac
case "$(uname -m)" in
  x86_64|amd64) target_arch=amd64 ;;
  arm64|aarch64) target_arch=arm64 ;;
  *) fail 'supported architectures are amd64 and arm64' ;;
esac
asset="hostmcp_${version}_${target_os}_${target_arch}.tar.gz"

for executable in tar awk mktemp; do
  command -v "$executable" >/dev/null 2>&1 || fail "required command is missing: $executable"
done
if command -v sha256sum >/dev/null 2>&1; then
  hash_command=sha256sum
elif command -v shasum >/dev/null 2>&1; then
  hash_command=shasum
else
  fail 'sha256sum or shasum is required'
fi

temp_dir=$(mktemp -d "${TMPDIR:-/tmp}/hostmcp-install.XXXXXX")
staged_binary=''
cleanup() {
  [ -z "$staged_binary" ] || rm -f "$staged_binary"
  rm -rf "$temp_dir"
}
trap cleanup EXIT HUP INT TERM

if [ -n "$local_archive" ] || [ -n "$local_checksums" ]; then
  [ -n "$local_archive" ] && [ -n "$local_checksums" ] || fail '--archive and --checksums must be supplied together'
  [ -f "$local_archive" ] || fail 'local archive does not exist'
  [ -f "$local_checksums" ] || fail 'local checksums file does not exist'
  cp "$local_archive" "$temp_dir/$asset"
  cp "$local_checksums" "$temp_dir/SHA256SUMS"
else
  command -v curl >/dev/null 2>&1 || fail 'curl is required for downloads'
  release_url="https://github.com/$repository/releases/download/v$version"
  curl --fail --location --silent --show-error --proto '=https' --tlsv1.2 \
    "$release_url/$asset" -o "$temp_dir/$asset" || fail "could not download $asset; check that v$version has been released"
  curl --fail --location --silent --show-error --proto '=https' --tlsv1.2 \
    "$release_url/SHA256SUMS" -o "$temp_dir/SHA256SUMS" || fail 'could not download SHA256SUMS'
fi

expected=$(awk -v name="$asset" '$2 == name { print $1; found++ } END { if (found != 1) exit 1 }' "$temp_dir/SHA256SUMS") || fail 'checksums must contain exactly one matching archive'
printf '%s\n' "$expected" | LC_ALL=C grep -Eq '^[0-9a-fA-F]{64}$' || fail 'invalid SHA-256 entry'
if [ "$hash_command" = sha256sum ]; then
  actual=$(sha256sum "$temp_dir/$asset" | awk '{print $1}')
else
  actual=$(shasum -a 256 "$temp_dir/$asset" | awk '{print $1}')
fi
expected=$(printf '%s' "$expected" | tr 'ABCDEF' 'abcdef')
[ "$actual" = "$expected" ] || fail 'SHA-256 mismatch; nothing was installed'

mkdir "$temp_dir/extract"
tar -xzf "$temp_dir/$asset" -C "$temp_dir/extract" hostmcp
[ -f "$temp_dir/extract/hostmcp" ] && [ ! -L "$temp_dir/extract/hostmcp" ] || fail 'archive does not contain a regular hostmcp binary'
mkdir -p "$prefix"
staged_binary=$(mktemp "$prefix/.hostmcp.XXXXXX")
cp "$temp_dir/extract/hostmcp" "$staged_binary"
chmod 0755 "$staged_binary"
mv -f "$staged_binary" "$prefix/hostmcp"
staged_binary=''
printf '%s\n' "Installed HostMCP $version to $prefix/hostmcp" "Add $prefix to PATH if needed. Services were not started or changed."
