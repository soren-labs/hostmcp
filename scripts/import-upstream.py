#!/usr/bin/env python3
"""Import an explicit public-source allowlist without touching an installed app."""

import argparse
import hashlib
import json
from pathlib import Path
import subprocess

REPO = Path(__file__).resolve().parent.parent
COMMIT = "f51acbccdd734f524799ea92bb747be765fba1e4"
URL = "https://github.com/totec448-spec/chat-on-steroids"

CORE_FILES = [
    "src/main/" + name + ".ts"
    for name in ("fsops", "rawfs", "text-match", "diffstat", "exec", "env", "ripgrep", "search")
]
MCP_FILES = [
    "src/main/mcp/" + name + ".ts"
    for name in (
        "server", "tools", "tools-core", "kernel", "inbound", "call-context",
        "tool-declarations", "surfaces", "code-mode-runtime", "code-mode-worker",
        "code-mode-tool", "artifact-fetch", "artifact-target", "artifact-download",
    )
]
TEST_FILES = [
    "test/" + name + ".test.ts"
    for name in (
        "fsops", "text-match", "read-backend", "codex-runtime-parity",
        "codex-apply-patch-parity", "codex-apply-patch-invocation-parity",
        "codex-apply-patch-move-rollback", "codex-view-image-parity",
        "command-batch-display", "unified-exec-mutex", "exec-output-budget",
        "code-mode-runtime", "mcp-inbound",
    )
] + ["test/helpers.ts"]
LICENSE_FILES = ["LICENSE"] + [
    "docs/licenses/codex/" + name for name in ("LICENSE", "NOTICE", "README.md")
]


def git(root, *args):
    return subprocess.check_output(["git", "-C", str(root), *args], text=True).strip()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("upstream", type=Path)
    args = parser.parse_args()
    source = args.upstream.resolve(strict=True)
    if git(source, "rev-parse", "HEAD") != COMMIT:
        raise SystemExit("Refusing a checkout other than the pinned upstream commit")
    if git(source, "status", "--porcelain"):
        raise SystemExit("Refusing a dirty upstream checkout")
    tracked = set(git(source, "ls-files").splitlines())
    codex = sorted(
        path for path in tracked
        if path.startswith("src/main/codex/") and path.endswith(".ts")
        and path != "src/main/codex/ownership.ts"
    )
    groups = {
        "portable-core": sorted(CORE_FILES + codex),
        "mcp-reference": MCP_FILES,
        "regression-reference": TEST_FILES,
        "license": LICENSE_FILES,
    }
    prepared = []
    for group, paths in groups.items():
        for path in paths:
            if path not in tracked:
                raise SystemExit(f"Not a tracked upstream file: {path}")
            origin = source / path
            if origin.is_symlink():
                raise SystemExit(f"Symlink imports are not supported: {path}")
            data = origin.read_bytes()
            destination = "reference/chat-on-steroids/" + path
            target = REPO / destination
            if target.exists() and (target.is_symlink() or target.read_bytes() != data):
                raise SystemExit(f"Refusing to overwrite a changed reference: {destination}")
            prepared.append((path, destination, group, data))
    entries = [
        {"upstream_path": path, "path": dest, "group": group,
         "bytes": len(data), "sha256": hashlib.sha256(data).hexdigest()}
        for path, dest, group, data in prepared
    ]
    manifest = {
        "schema_version": 1,
        "repository": URL,
        "ref": "v2.1.11",
        "commit": COMMIT,
        "status": "Unmodified reference snapshot; not a standalone MCP runtime",
        "files": sorted(entries, key=lambda entry: entry["path"]),
    }
    for _, dest, _, data in prepared:
        target = REPO / dest
        target.parent.mkdir(parents=True, exist_ok=True)
        target.write_bytes(data)
    (REPO / "LICENSE").write_bytes((source / "LICENSE").read_bytes())
    output = REPO / "provenance/upstream.json"
    output.parent.mkdir(parents=True, exist_ok=True)
    output.write_text(json.dumps(manifest, indent=2) + "\n", encoding="utf-8")
    print(f"Imported {len(entries)} files, {sum(e['bytes'] for e in entries):,} bytes, from {COMMIT}")


if __name__ == "__main__":
    main()
