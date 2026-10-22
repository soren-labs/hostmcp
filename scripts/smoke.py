#!/usr/bin/env python3
"""Real HostMCP smoke test: Python stdlib + an existing hostmcp binary.

Examples:
  python3 scripts/smoke.py --binary ./bin/hostmcp --mode stdio
  python3 scripts/smoke.py --binary ./bin/hostmcp --url https://host.example/mcp \
      --token-file ~/.config/hostmcp/client.token --devices ubuntu,mac,vps
  python3 scripts/smoke.py --binary ./bin/hostmcp --mode bridge \
      --url https://host.example/mcp --token-file TOKEN --devices ubuntu,mac

bridge runs `hostmcp connect` and drives its real newline-delimited JSON-RPC.
Each selected device receives temporary files and short-lived test processes.
Only this run's sessions and its exclusively created directory are cleaned up.
No token contents are read by Python or printed. No deployment is performed.
"""

import argparse
import base64
import json
import os
import posixpath
import queue
import shlex
import struct
import subprocess
import sys
import threading
import time
import uuid
import zlib

PROTOCOL = "2025-11-25"
TOOLS = {"device_info", "exec_command", "write_stdin", "list_processes",
         "terminate_process", "read", "write_file", "apply_patch", "view_image"}


def check(condition, message):
    if not condition:
        raise AssertionError(message)


def unpack(result):
    check(isinstance(result, dict), "MCP tool result is not an object")
    if result.get("isError"):
        messages = [c.get("text", "") for c in result.get("content", []) if c.get("type") == "text"]
        raise AssertionError("MCP tool error: " + " ".join(messages)[:600])
    structured = result.get("structuredContent")
    if isinstance(structured, str):
        structured = json.loads(structured)
    if structured is not None:
        return structured
    for content in result.get("content", []):
        if content.get("type") == "text":
            try:
                return json.loads(content["text"])
            except (ValueError, TypeError):
                continue
    raise AssertionError("MCP tool result has neither structuredContent nor JSON text")


class Client:
    def __init__(self, args):
        self.args = args
        self.proc = None
        self.counter = 0
        self.responses = queue.Queue()
        self.remote_flags = ["--url", args.url or "", "--token-file", args.token_file or "",
                             "--protocol-version", PROTOCOL]
        if args.allow_insecure:
            self.remote_flags.append("--allow-insecure")
        if args.mode != "http":
            command = [args.binary, "stdio"] if args.mode == "stdio" else [args.binary, "connect"] + self.remote_flags
            self.proc = subprocess.Popen(command, stdin=subprocess.PIPE, stdout=subprocess.PIPE,
                                         stderr=subprocess.DEVNULL, text=True, encoding="utf-8", bufsize=1)
            threading.Thread(target=self._reader, daemon=True).start()
            try:
                init = self.rpc("initialize", {"protocolVersion": PROTOCOL, "capabilities": {},
                                               "clientInfo": {"name": "hostmcp-smoke", "version": "0.1.0"}})
                check(init.get("protocolVersion") == PROTOCOL, "server did not negotiate requested MCP protocol")
                self._send({"jsonrpc": "2.0", "method": "notifications/initialized"})
            except Exception:
                self.close()
                raise

    def _reader(self):
        try:
            while True:
                line = self.proc.stdout.readline((8 << 20) + 1)
                if not line:
                    raise RuntimeError("MCP subprocess closed stdout")
                if len(line) > 8 << 20:
                    raise RuntimeError("MCP subprocess emitted an oversized JSON-RPC line")
                if line.strip():
                    self.responses.put(json.loads(line))
        except Exception as exc:
            self.responses.put(exc)

    def _send(self, message):
        self.proc.stdin.write(json.dumps(message, ensure_ascii=False, separators=(",", ":")) + "\n")
        self.proc.stdin.flush()

    def rpc(self, method, params):
        self.counter += 1
        request_id = self.counter
        self._send({"jsonrpc": "2.0", "id": request_id, "method": method, "params": params})
        deadline = time.monotonic() + self.args.call_timeout
        while True:
            remaining = deadline - time.monotonic()
            if remaining <= 0:
                raise TimeoutError("JSON-RPC response timed out for " + method)
            try:
                message = self.responses.get(timeout=remaining)
            except queue.Empty:
                raise TimeoutError("JSON-RPC response timed out for " + method) from None
            if isinstance(message, Exception):
                raise message
            if message.get("id") != request_id:
                continue  # Notifications are permitted between responses.
            if "error" in message:
                raise AssertionError("JSON-RPC " + method + " failed: " + str(message["error"].get("message", "error"))[:600])
            return message["result"]

    def cli(self, flags):
        try:
            result = subprocess.run([self.args.binary, "call"] + self.remote_flags + flags,
                                    stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True,
                                    encoding="utf-8", timeout=self.args.call_timeout, check=False)
        except subprocess.TimeoutExpired:
            raise TimeoutError("hostmcp call subprocess timed out") from None
        # Do not echo CLI arguments/stderr: a caller may have configured credentials there.
        check(result.returncode == 0, "hostmcp call failed with exit code " + str(result.returncode))
        return json.loads(result.stdout)

    def catalog(self):
        result = self.cli(["--list-tools"]) if self.args.mode == "http" else self.rpc("tools/list", {})
        return {tool["name"] for tool in result["tools"]}

    def tool(self, name, arguments):
        if self.args.mode == "http":
            return self.cli(["--tool", name, "--args", json.dumps(arguments, ensure_ascii=False)])
        return self.rpc("tools/call", {"name": name, "arguments": arguments})

    def close(self):
        if self.proc is None:
            return
        try:
            self.proc.stdin.close()
        except (BrokenPipeError, OSError):
            pass
        try:
            self.proc.wait(timeout=5)
        except subprocess.TimeoutExpired:
            self.proc.terminate()
            try:
                self.proc.wait(timeout=2)
            except subprocess.TimeoutExpired:
                self.proc.kill()
                self.proc.wait(timeout=2)


def tiny_png():
    def chunk(kind, data):
        return struct.pack(">I", len(data)) + kind + data + struct.pack(">I", zlib.crc32(kind + data) & 0xffffffff)
    return (b"\x89PNG\r\n\x1a\n" + chunk(b"IHDR", struct.pack(">IIBBBBB", 1, 1, 8, 6, 0, 0, 0))
            + chunk(b"IDAT", zlib.compress(b"\x00\x20\x80\xee\xff")) + chunk(b"IEND", b""))


class DeviceSmoke:
    def __init__(self, client, device):
        self.client = client
        self.device = device
        self.directory = None
        self.sessions = set()
        self.tools = set()
        self.checks = []

    def invoke(self, name, **arguments):
        if self.client.args.mode != "stdio":
            arguments["device"] = self.device
        self.tools.add(name)
        raw = self.client.tool(name, arguments)
        data = unpack(raw)
        if name == "exec_command" and data.get("session_id"):
            self.sessions.add(data["session_id"])
        return data, raw

    def call(self, name, **arguments):
        return self.invoke(name, **arguments)[0]

    def finish(self, first, timeout=15):
        result = first
        text = result.get("output", "")
        deadline = time.monotonic() + timeout
        while result.get("running") or result.get("has_more_output"):
            check(time.monotonic() < deadline, "managed command did not finish in time")
            result = self.call("write_stdin", session_id=result["session_id"], yield_time_ms=500)
            text += result.get("output", "")
        return result, text

    def execute(self, command, **arguments):
        arguments.setdefault("shell", "/bin/sh")
        arguments.setdefault("yield_time_ms", 1000)
        result, text = self.finish(self.call("exec_command", cmd=command, **arguments))
        check(result.get("exit_code") == 0, "fixture command failed with exit " + str(result.get("exit_code")))
        return text

    def read_text(self, path):
        return self.call("read", paths=[path])["entries"][0]["content"]

    def run(self):
        identity = self.call("device_info")
        uid = int(identity["uid"])
        directory = posixpath.normpath(self.execute('mktemp -d "${TMPDIR:-/tmp}/hostmcp-smoke.XXXXXXXX"').strip())
        check(directory.startswith("/") and posixpath.basename(directory).startswith("hostmcp-smoke."),
              "mktemp did not return an exclusive absolute smoke directory")
        self.directory = directory
        lines = self.execute('printf "%s\\n" "$PWD" "$HOSTMCP_SMOKE_VALUE"; id -u', cwd=directory,
                             env={"HOSTMCP_SMOKE_VALUE": "中文 env value"}).splitlines()
        check(len(lines) == 3 and lines[0] == directory and lines[1] == "中文 env value" and int(lines[2]) == uid,
              "cwd, environment or actual OS UID did not match device_info")
        self.checks.append("native_uid_cwd_env")

        path = directory + "/中文 目录/测试 文件.txt"
        self.call("write_file", path=path, content="第一行\nsecond\n", create_parents=True)
        check(self.read_text(path) == "第一行\nsecond\n", "UTF-8 file roundtrip failed")
        patch = "*** Begin Patch\n*** Update File: " + path + "\n@@\n 第一行\n-second\n+第三行\n*** End Patch\n"
        self.call("apply_patch", patch=patch)
        check(self.read_text(path) == "第一行\n第三行\n", "context patch on a Unicode/spaced path failed")
        binary = b"\x00\x01\x7f\xffbinary\r\n"
        binary_path = directory + "/binary.bin"
        self.call("write_file", path=binary_path, encoding="base64", content=base64.b64encode(binary).decode("ascii"))
        encoded = self.call("read", paths=[binary_path], encoding="base64")["entries"][0]["content"]
        check(base64.b64decode(encoded) == binary, "binary file roundtrip failed")
        self.checks.append("unicode_paths_patch_binary")

        png = tiny_png()
        image_path = directory + "/pixel.png"
        self.call("write_file", path=image_path, encoding="base64", content=base64.b64encode(png).decode("ascii"))
        image_info, image_raw = self.invoke("view_image", path=image_path)
        images = [block for block in image_raw.get("content", []) if block.get("type") == "image"]
        check(image_info.get("width") == 1 and image_info.get("height") == 1 and len(images) == 1
              and images[0].get("mimeType") == "image/png" and base64.b64decode(images[0]["data"]) == png,
              "view_image did not return genuine MCP PNG image content")
        self.checks.append("mcp_image_content")

        tty = self.call("exec_command", cmd='printf "READY\\n"; read -r reply; printf "TTY:%s\\n" "$reply"',
                        shell="/bin/sh", tty=True, yield_time_ms=1000, timeout_seconds=10)
        sent = self.call("write_stdin", session_id=tty["session_id"], chars="你好 PTY\n", rows=30, cols=100,
                         yield_time_ms=1000)
        final, output = self.finish(sent)
        check(final.get("exit_code") == 0 and "TTY:你好 PTY" in output, "interactive PTY roundtrip failed")
        nonzero, _ = self.finish(self.call("exec_command", cmd="exit 7", shell="/bin/sh", yield_time_ms=1000))
        check(nonzero.get("exit_code") == 7, "nonzero exit status was not preserved")
        self.checks.append("pty_and_nonzero_exit")

        long_job = self.call("exec_command", cmd="sleep 1; printf long-complete", shell="/bin/sh", yield_time_ms=0)
        inventory = self.call("list_processes")["sessions"]
        check(any(item["session_id"] == long_job["session_id"] for item in inventory), "started session missing from inventory")
        final, output = self.finish(long_job)
        check(final.get("exit_code") == 0 and output == "long-complete", "long session polling lost output")
        deadline_job = self.call("exec_command", cmd="sleep 20", shell="/bin/sh", yield_time_ms=0, timeout_seconds=1)
        final, _ = self.finish(deadline_job)
        check(final.get("timed_out") is True and final.get("running") is False, "execution deadline did not stop session")
        stop_job = self.call("exec_command", cmd="sleep 20", shell="/bin/sh", yield_time_ms=0, timeout_seconds=0)
        stopped = self.call("terminate_process", session_id=stop_job["session_id"])
        check(stopped.get("running") is False, "explicit termination did not stop session")
        self.checks.append("inventory_poll_timeout_terminate")

        marker = directory + "/once.txt"
        command = "printf 'once\\n' >> " + shlex.quote(marker)
        arguments = {"cmd": command, "shell": "/bin/sh", "yield_time_ms": 0, "request_id": "smoke-" + uuid.uuid4().hex}
        first = self.call("exec_command", **arguments)
        repeated = self.call("exec_command", **arguments)
        check(first["session_id"] == repeated["session_id"] and repeated.get("replayed") is True,
              "request_id did not reuse the original execution")
        self.finish(repeated)
        check(self.read_text(marker) == "once\n", "idempotent request repeated a filesystem side effect")
        self.checks.append("idempotent_side_effect")
        check(TOOLS <= self.tools, "not all nine native tools were exercised")
        return uid

    def cleanup(self):
        errors = []
        try:
            inventory = self.call("list_processes")["sessions"]
            for item in inventory:
                if item["session_id"] in self.sessions and item.get("running"):
                    self.call("terminate_process", session_id=item["session_id"])
        except Exception as exc:
            errors.append("owned process cleanup: " + str(exc))
        if self.directory:
            path = self.directory
            if not (path.startswith("/") and posixpath.basename(path).startswith("hostmcp-smoke.")):
                errors.append("refused cleanup of an unexpected directory")
            else:
                try:
                    quoted = shlex.quote(path)
                    self.execute("rm -rf -- " + quoted + "; test ! -e " + quoted + " && test ! -L " + quoted,
                                 timeout_seconds=10)
                except Exception as exc:
                    errors.append("temporary directory cleanup: " + str(exc))
        return errors


def main():
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("--binary", default="./bin/hostmcp")
    parser.add_argument("--mode", choices=("http", "stdio", "bridge"), default="http")
    parser.add_argument("--url")
    parser.add_argument("--token-file")
    parser.add_argument("--devices", help="Comma-separated registered IDs; ignored for local stdio")
    parser.add_argument("--allow-insecure", action="store_true", help="Explicit HTTP opt-in for trusted private test networks")
    parser.add_argument("--call-timeout", type=float, default=45, help="Per-subprocess/JSON-RPC timeout in seconds")
    args = parser.parse_args()
    if args.mode != "stdio" and (not args.url or not args.token_file or not args.devices):
        parser.error("http/bridge require --url, --token-file and --devices")
    if args.call_timeout <= 0:
        parser.error("--call-timeout must be positive")
    devices = [None] if args.mode == "stdio" else [item.strip() for item in args.devices.split(",") if item.strip()]
    check(bool(devices), "no device IDs were provided")
    client = None
    failed = False
    try:
        client = Client(args)
        check(TOOLS <= client.catalog(), "server catalog is missing native tools")
        if args.mode != "stdio":
            visible = unpack(client.tool("list_devices", {}))["devices"]
            online = {item["id"] for item in visible if item.get("online")}
            check(set(devices) <= online, "one or more requested devices are not visible and online")
        for device in devices:
            smoke = DeviceSmoke(client, device)
            started = time.monotonic()
            report = {"mode": args.mode, "protocol": PROTOCOL, "device": device or "local"}
            try:
                report["uid"] = smoke.run()
                report["status"] = "passed"
            except Exception as exc:
                failed = True
                report.update(status="failed", error=str(exc))
            finally:
                cleanup_errors = smoke.cleanup()
                if cleanup_errors:
                    failed = True
                    report.update(status="failed", cleanup_errors=cleanup_errors)
                else:
                    report["cleanup"] = "exclusive temporary directory removed; owned sessions stopped"
            report.update(checks=smoke.checks, tools_tested=sorted(smoke.tools), seconds=round(time.monotonic() - started, 3))
            print(json.dumps(report, ensure_ascii=False), flush=True)
    except Exception as exc:
        failed = True
        print(json.dumps({"mode": args.mode, "status": "failed", "error": str(exc)}, ensure_ascii=False), flush=True)
    finally:
        if client is not None:
            client.close()
    return 1 if failed else 0


if __name__ == "__main__":
    sys.exit(main())
