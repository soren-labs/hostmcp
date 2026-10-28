#!/usr/bin/env python3
"""Verify two MCP credentials against root and ordinary nodes on the same host.

This uses actual MCP calls through the hostmcp CLI, never SSH. Both nodes must
already be online. The admin token must reach the root node; the ordinary token
must reach only the ordinary node. A root-owned temporary fixture is created and
removed through MCP. No token contents are read or printed by this script.
"""

import argparse
import json
import posixpath
import shlex
import subprocess
import sys
import time

from smoke import check, unpack


def fixture_path(path):
    return (posixpath.normpath(path) == path and posixpath.dirname(path) == "/tmp"
            and posixpath.basename(path).startswith("hostmcp-access."))


class AccessClient:
    def __init__(self, args, token_file):
        self.args = args
        self.flags = [args.binary, "call", "--url", args.url,
                      "--token-file", token_file]
        if args.allow_insecure:
            self.flags.append("--allow-insecure")

    def raw(self, name, arguments):
        try:
            result = subprocess.run(self.flags + ["--tool", name, "--args", json.dumps(arguments)],
                                    capture_output=True, text=True, encoding="utf-8",
                                    timeout=self.args.call_timeout, check=False)
        except subprocess.TimeoutExpired:
            raise AssertionError("MCP call timed out: " + name) from None
        # A network/authentication error is not evidence of a device-scope denial.
        try:
            parsed = json.loads(result.stdout)
        except ValueError:
            raise AssertionError("MCP call produced no result: " + name) from None
        check(isinstance(parsed, dict), "MCP result must be an object")
        check(result.returncode == 0 or parsed.get("isError"),
              "CLI failed without an MCP tool error: " + name)
        return parsed

    def call(self, name, **arguments):
        return unpack(self.raw(name, arguments))

    def execute(self, node, command):
        data = self.call("exec_command", device=node, cmd=command, shell="/bin/sh",
                         yield_time_ms=1000, timeout_seconds=15)
        output = data.get("output", "")
        deadline = time.monotonic() + 20
        while data.get("running") or data.get("has_more_output"):
            check(time.monotonic() < deadline, "identity/fixture command did not finish")
            data = self.call("write_stdin", device=node, session_id=data["session_id"],
                             yield_time_ms=500)
            output += data.get("output", "")
        check(data.get("exit_code") == 0, "identity/fixture command failed")
        return output.strip()


def run(args):
    admin = AccessClient(args, args.admin_token_file)
    ordinary = AccessClient(args, args.ordinary_token_file)
    report = {"status": "failed", "checks": [], "root_node": args.root_node,
              "ordinary_node": args.ordinary_node, "transport": "MCP HTTP"}
    fixture = None
    try:
        visible = ordinary.call("list_devices")["devices"]
        visible_ids = {node["id"] for node in visible}
        check(args.ordinary_node in visible_ids, "ordinary node is not visible to ordinary token")
        check(args.root_node not in visible_ids, "root node leaked into ordinary token inventory")
        report["checks"].append("credential_scoped_inventory")

        root_info = admin.call("device_info", device=args.root_node)
        ordinary_info = ordinary.call("device_info", device=args.ordinary_node)
        root_host = root_info.get("device", {}).get("hostname")
        ordinary_host = ordinary_info.get("device", {}).get("hostname")
        check(isinstance(root_host, str) and bool(root_host)
              and isinstance(ordinary_host, str) and bool(ordinary_host)
              and root_host == ordinary_host,
              "root and ordinary nodes must report the same nonempty hostname")
        check(int(root_info["uid"]) == 0 and int(root_info["euid"]) == 0, "admin node is not root")
        check(int(ordinary_info["uid"]) != 0 and int(ordinary_info["euid"]) != 0,
              "ordinary node has root UID or effective UID")
        if args.expected_ordinary_uid is not None:
            check(int(ordinary_info["uid"]) == args.expected_ordinary_uid, "ordinary UID differs from expected")
        check(admin.execute(args.root_node, "id -u") == "0", "actual root command UID differs")
        actual_uid = ordinary.execute(args.ordinary_node, "id -u")
        check(actual_uid == str(ordinary_info["euid"]), "actual ordinary command effective UID differs")
        report.update(root_uid="0", root_euid="0", ordinary_uid=str(ordinary_info["uid"]),
                      ordinary_euid=actual_uid)
        report["checks"].append("native_execution_uids")

        denied = ordinary.raw("exec_command", {"device": args.root_node, "cmd": "id -u",
                                               "shell": "/bin/sh", "yield_time_ms": 1000})
        check(denied.get("isError") is True, "ordinary credential could execute on root node")
        report["checks"].append("cross_node_execution_denied")

        fixture = admin.execute(args.root_node, "mktemp -d /tmp/hostmcp-access.XXXXXXXX")
        check(fixture_path(fixture), "unexpected fixture directory")
        quoted_dir = shlex.quote(fixture)
        path = fixture + "/root-only.txt"
        quoted_path = shlex.quote(path)
        admin.execute(args.root_node, "chmod 755 " + quoted_dir + "; printf hostmcp-native-root > "
                      + quoted_path + "; chmod 600 " + quoted_path)
        root_read = admin.call("read", device=args.root_node, paths=[path])
        check(root_read["entries"][0].get("content") == "hostmcp-native-root", "root fixture read failed")
        user_read = ordinary.raw("read", {"device": args.ordinary_node, "paths": [path]})
        error_text = " ".join(item.get("text", "") for item in user_read.get("content", [])
                              if item.get("type") == "text").lower()
        check(user_read.get("isError") is True and "permission denied" in error_text,
              "ordinary file tool did not preserve native permission denial")
        user_exec = ordinary.execute(args.ordinary_node, "if cat " + quoted_path
                                     + " >/dev/null 2>&1; then printf READABLE; else printf DENIED; fi")
        check(user_exec == "DENIED", "ordinary shell could read root-only fixture")
        report["checks"].append("native_file_and_shell_permissions")
        report["status"] = "passed"
    except Exception as exc:
        report["error"] = str(exc)
    finally:
        if fixture and fixture_path(fixture):
            try:
                path = shlex.quote(fixture)
                admin.execute(args.root_node, "rm -rf -- " + path + "; test ! -e " + path + " && test ! -L " + path)
                report["cleanup"] = "exclusive root-owned fixture removed"
            except Exception:
                report["status"] = "failed"
                report["cleanup_error"] = "could not remove fixture " + fixture
    return report


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--binary", default="./build/hostmcp")
    parser.add_argument("--url", required=True)
    parser.add_argument("--admin-token-file", required=True)
    parser.add_argument("--ordinary-token-file", required=True)
    parser.add_argument("--root-node", required=True)
    parser.add_argument("--ordinary-node", required=True)
    parser.add_argument("--expected-ordinary-uid", type=int)
    parser.add_argument("--allow-insecure", action="store_true")
    parser.add_argument("--call-timeout", type=float, default=45)
    args = parser.parse_args()
    if args.root_node == args.ordinary_node or args.call_timeout <= 0:
        parser.error("use distinct root/ordinary nodes and a positive timeout")
    started = time.monotonic()
    report = run(args)
    report["seconds"] = round(time.monotonic() - started, 3)
    print(json.dumps(report, ensure_ascii=False), flush=True)
    return 0 if report["status"] == "passed" else 1


if __name__ == "__main__":
    sys.exit(main())
