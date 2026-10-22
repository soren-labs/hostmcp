#!/usr/bin/env python3
"""Verify the immutable source snapshot; does not execute any upstream code."""

import argparse
from collections import Counter
import hashlib
import json
from pathlib import Path
import subprocess

REPO = Path(__file__).resolve().parent.parent
COMMIT = "f51acbccdd734f524799ea92bb747be765fba1e4"


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--upstream", type=Path)
    args = parser.parse_args()
    manifest = json.loads((REPO / "provenance/upstream.json").read_text(encoding="utf-8"))
    if manifest.get("commit") != COMMIT or manifest.get("schema_version") != 1:
        raise SystemExit("Unexpected provenance version or upstream commit")
    if args.upstream:
        revision = subprocess.check_output(
            ["git", "-C", str(args.upstream), "rev-parse", "HEAD"], text=True
        ).strip()
        if revision != COMMIT:
            raise SystemExit("Comparison checkout is not the pinned upstream commit")
    expected = set()
    counts = Counter()
    total = 0
    for entry in manifest["files"]:
        relative = Path(entry["path"])
        if relative.is_absolute() or ".." in relative.parts:
            raise SystemExit("Invalid manifest path")
        if relative.parts[:2] != ("reference", "chat-on-steroids"):
            raise SystemExit("Reference file is outside its snapshot")
        if entry["path"] in expected:
            raise SystemExit("Duplicate manifest path")
        expected.add(entry["path"])
        target = REPO / relative
        if target.is_symlink() or not target.is_file():
            raise SystemExit(f"Missing file or unexpected symlink: {relative}")
        data = target.read_bytes()
        if len(data) != entry["bytes"] or hashlib.sha256(data).hexdigest() != entry["sha256"]:
            raise SystemExit(f"Source checksum mismatch: {relative}")
        if args.upstream and data != (args.upstream / entry["upstream_path"]).read_bytes():
            raise SystemExit(f"Source differs from upstream: {relative}")
        counts[entry["group"]] += 1
        total += len(data)
    actual = {
        str(path.relative_to(REPO))
        for path in (REPO / "reference/chat-on-steroids").rglob("*")
        if path.is_file() or path.is_symlink()
    }
    if expected != actual:
        raise SystemExit(f"Reference inventory differs: {sorted(expected ^ actual)}")
    if (REPO / "LICENSE").read_bytes() != (REPO / "reference/chat-on-steroids/LICENSE").read_bytes():
        raise SystemExit("Root MIT license differs from upstream")
    for path in expected:
        if any(part in {"renderer", "preload", "extension", "node_modules"} for part in Path(path).parts):
            raise SystemExit(f"Unexpected GUI or dependency tree: {path}")
    print(f"Verified {len(expected)} unchanged files ({total:,} bytes) at {COMMIT}")
    for group, count in sorted(counts.items()):
        print(f"  {group}: {count}")
    print("No installed-app state was accessed. Runtime behavior was not tested.")


if __name__ == "__main__":
    main()
