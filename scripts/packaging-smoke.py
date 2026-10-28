#!/usr/bin/env python3
"""Offline native installer/component/updater checks in an isolated temporary tree."""
import argparse
import hashlib
import json
import os
from pathlib import Path
import platform
import subprocess
import tarfile
import tempfile

ROOT = Path(__file__).resolve().parent.parent


def run(*args, ok=True):
    result = subprocess.run([str(a) for a in args], cwd=ROOT, capture_output=True, text=True)
    if (result.returncode == 0) != ok:
        raise AssertionError(f"command {args[0]}: exit {result.returncode}\n{result.stderr}")
    return result


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--dist", type=Path, required=True)
    parser.add_argument("--version", required=True)
    args = parser.parse_args()
    version = args.version.removeprefix("v")
    dist = args.dist.resolve()
    goos = {"Darwin": "darwin", "Linux": "linux"}[platform.system()]
    arch = {"arm64": "arm64", "aarch64": "arm64", "x86_64": "amd64"}[platform.machine()]
    entries = dict(line.split()[::-1] for line in (dist / "SHA256SUMS").read_text().splitlines())
    assert len(entries) == 12
    for name, checksum in entries.items():
        assert hashlib.sha256((dist / name).read_bytes()).hexdigest() == checksum
        with tarfile.open(dist / name) as archive:
            names = archive.getnames()
            assert "NOTICE" in names and "LICENSE" in names and "licenses" in names
    with tempfile.TemporaryDirectory(prefix="hostmcp-packaging-") as tmp:
        folder = Path(tmp).resolve()
        prefix = folder / "bin"
        for role in ("all", "hub", "agent"):
            binary = "hostmcp" if role == "all" else f"hostmcp-{role}"
            asset = dist / f"{binary}_{version}_{goos}_{arch}.tar.gz"
            run("sh", "scripts/install.sh", "--role", role, "--version", version,
                "--archive", asset, "--checksums", dist / "SHA256SUMS", "--prefix", prefix)
            identity = json.loads(run(prefix / binary, "version").stdout)
            assert identity["component"] == role and identity["version"] == version
            assert identity["wire_version"] == 1
            if role == "hub":
                run(prefix / binary, "agent", "run", ok=False)
                run(prefix / binary, "stdio", ok=False)
            if role == "agent":
                run(prefix / binary, "hub", "serve", ok=False)
                run(prefix / binary, "stdio", ok=False)
        bad = folder / "bad.tar.gz"
        bad.write_bytes(asset.read_bytes() + b"corrupted")
        original = (prefix / "hostmcp-agent").read_bytes()
        run("sh", "scripts/install.sh", "--role", "agent", "--version", version,
            "--archive", bad, "--checksums", dist / "SHA256SUMS", "--prefix", prefix, ok=False)
        assert original == (prefix / "hostmcp-agent").read_bytes()
        # Build an older native agent as the target; no service is installed or started.
        target = prefix / "hostmcp-agent"
        run("go", "build", "-trimpath", "-ldflags",
            "-X main.component=agent -X main.version=0.0.0 -X github.com/soren-labs/hostmcp/internal/protocol.Version=0.0.0",
            "-o", target, "./cmd/hostmcp")
        old = target.read_bytes()
        policy = folder / "update.json"
        policy.write_text(json.dumps({"version": 1, "enabled": True, "channel": "stable",
                                     "target_version": version, "sha256": hashlib.sha256(asset.read_bytes()).hexdigest(),
                                     "binary": str(target)}))
        policy.chmod(0o600)
        if os.geteuid() == 0:
            run(target, "agent", "update", "--policy", policy, "--archive", asset, ok=False)
            assert target.read_bytes() == old
            print("Root replacement refusal verified; non-root updater smoke requires a user account.")
        else:
            run(target, "agent", "update", "--policy", policy, "--archive", bad, ok=False)
            assert target.read_bytes() == old
            run(target, "agent", "update", "--policy", policy, "--archive", asset)
            assert json.loads(run(target, "version").stdout)["version"] == version
            assert (prefix / "hostmcp-agent.previous").read_bytes() == old
            run(target, "agent", "update", "--policy", policy, "--rollback")
            assert target.read_bytes() == old
        print("Verified 12 archives, native component roles, offline installs, checksum rejection, and updater safeguards.")


if __name__ == "__main__":
    main()
