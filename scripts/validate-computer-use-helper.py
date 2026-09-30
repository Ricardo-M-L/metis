#!/usr/bin/env python3
"""Validate the managed METIS Computer Use helper contract.

The release builder can verify hashes and archive layout, but that is not
enough to prove that the executable can be managed by METIS. This gate checks
the native descriptor and then uses read-only MCP resource requests to confirm
that its advertised status capability actually works before packaging.
"""

import argparse
import json
import os
from pathlib import Path
import re
import selectors
import signal
import subprocess
import tempfile
import time


PROTOCOL_VERSION = 1
MAX_OUTPUT_BYTES = 64 << 10
MCP_TIMEOUT_SECONDS = 10
MAX_RESOURCE_URI_BYTES = 2048
VERSION_RE = re.compile(r"[A-Za-z0-9][A-Za-z0-9._+~-]{0,255}\Z")
REQUIRED_CAPABILITIES = {
    "status",
    "stop",
    "end-turn",
    "serialized-input",
    "input-ownership",
}
REQUIRED_PERMISSIONS = {"accessibility", "screenRecording"}
ACTIVE_STATES = {"idle", "running"}


class HelperValidationError(ValueError):
    """The helper does not satisfy the managed METIS contract."""


def checked_executable(path):
    path = Path(path).absolute()
    if not path.exists():
        raise HelperValidationError(f"helper executable does not exist: {path}")
    if path.is_symlink() or not path.is_file():
        raise HelperValidationError(f"helper must be a regular non-symlink file: {path}")
    if os.name != "nt" and not path.stat().st_mode & 0o111:
        raise HelperValidationError(f"helper is not executable: {path}")
    return path


def run_helper(path, *args, timeout=10):
    """Run a metadata-only command with a minimal, deterministic environment."""
    with tempfile.TemporaryDirectory(prefix="metis-cu-probe-") as cwd:
        env = {
            "PATH": "/usr/bin:/bin:/usr/sbin:/sbin",
            "HOME": cwd,
            "PWD": cwd,
            "LANG": "C",
            "LC_ALL": "C",
            "TZ": "UTC",
        }
        process = subprocess.Popen(
            [str(path), *args], cwd=cwd, env=env, stdin=subprocess.DEVNULL,
            stdout=subprocess.PIPE, stderr=subprocess.PIPE, bufsize=0,
            start_new_session=os.name == "posix",
        )
        try:
            deadline = time.monotonic() + timeout
            output = {"stdout": bytearray(), "stderr": bytearray()}
            with selectors.DefaultSelector() as selector:
                selector.register(process.stdout, selectors.EVENT_READ, "stdout")
                selector.register(process.stderr, selectors.EVENT_READ, "stderr")
                while selector.get_map():
                    remaining = deadline - time.monotonic()
                    if remaining <= 0:
                        raise HelperValidationError(f"helper metadata command timed out: {' '.join(args)}")
                    events = selector.select(remaining)
                    if not events:
                        raise HelperValidationError(f"helper metadata command timed out: {' '.join(args)}")
                    for key, _ in events:
                        chunk = os.read(key.fd, 4096)
                        if not chunk:
                            selector.unregister(key.fileobj)
                            continue
                        buffer = output[key.data]
                        if len(buffer) + len(chunk) > MAX_OUTPUT_BYTES:
                            raise HelperValidationError("helper metadata command exceeded output limit")
                        buffer.extend(chunk)
            try:
                returncode = process.wait(timeout=max(0, deadline - time.monotonic()))
            except subprocess.TimeoutExpired as error:
                raise HelperValidationError(f"helper metadata command timed out: {' '.join(args)}") from error
            if returncode != 0:
                detail = output["stderr"].decode("utf-8", "replace").strip()
                suffix = f": {detail}" if detail else ""
                raise HelperValidationError(
                    f"helper metadata command failed ({returncode}){suffix}"
                )
            return bytes(output["stdout"])
        finally:
            _stop_process(process)


def _mcp_response(process, selector, pending, totals, request_id, method, deadline):
    """Read one JSON-RPC response while draining both bounded output pipes."""
    while True:
        while b"\n" in pending:
            line, _, remainder = pending.partition(b"\n")
            pending[:] = remainder
            try:
                message = json.loads(line.decode("utf-8"))
            except (UnicodeDecodeError, json.JSONDecodeError) as error:
                raise HelperValidationError(f"helper MCP returned invalid JSON: {error}") from error
            if not isinstance(message, dict) or message.get("jsonrpc") != "2.0":
                raise HelperValidationError("helper MCP returned an invalid JSON-RPC message")
            if "id" not in message and isinstance(message.get("method"), str):
                continue  # An unsolicited notification, not the requested result.
            if type(message.get("id")) is not int or message["id"] != request_id:
                raise HelperValidationError(f"helper MCP {method} returned an unexpected response id")
            if "error" in message:
                error = message["error"]
                code = error.get("code") if isinstance(error, dict) else None
                raise HelperValidationError(f"helper MCP {method} failed with error {code!r}")
            if "result" not in message or not isinstance(message["result"], dict):
                raise HelperValidationError(f"helper MCP {method} returned no object result")
            return message["result"]

        remaining = deadline - time.monotonic()
        if remaining <= 0:
            raise HelperValidationError(f"helper MCP {method} timed out")
        events = selector.select(remaining)
        if not events:
            raise HelperValidationError(f"helper MCP {method} timed out")
        for key, _ in events:
            chunk = os.read(key.fd, 4096)
            if not chunk:
                selector.unregister(key.fileobj)
                if key.data == "stdout":
                    raise HelperValidationError(f"helper MCP closed before {method} responded")
                continue
            totals[key.data] += len(chunk)
            if totals[key.data] > MAX_OUTPUT_BYTES:
                raise HelperValidationError("helper MCP exceeded output limit")
            if key.data == "stdout":
                pending.extend(chunk)


def _mcp_request(process, selector, pending, totals, request_id, method, deadline, params=None):
    request = {"jsonrpc": "2.0", "id": request_id, "method": method}
    if params is not None:
        request["params"] = params
    encoded = (json.dumps(request, separators=(",", ":")) + "\n").encode("utf-8")
    if len(encoded) > 4096:
        raise HelperValidationError(f"helper MCP {method} request exceeded size limit")
    try:
        process.stdin.write(encoded)
    except (BrokenPipeError, OSError) as error:
        raise HelperValidationError(f"helper MCP closed before {method} request") from error
    return _mcp_response(process, selector, pending, totals, request_id, method, deadline)


def _stop_process(process):
    # The helper runs in a new process group, so children receive termination.
    def signal_process(sig):
        if process.poll() is not None:
            return
        if os.name == "posix":
            try:
                os.killpg(process.pid, sig)
                return
            except OSError:
                # A process can exit between poll and killpg; macOS can also
                # deny signaling its group after the leader has exited.
                pass
        try:
            if sig == signal.SIGKILL:
                process.kill()
            else:
                process.terminate()
        except ProcessLookupError:
            pass

    signal_process(signal.SIGTERM)
    try:
        process.wait(timeout=0.5)
    except subprocess.TimeoutExpired:
        signal_process(signal.SIGKILL)
        try:
            process.wait(timeout=0.5)
        except subprocess.TimeoutExpired:
            pass
    finally:
        if process.stdin is not None:
            process.stdin.close()
        process.stdout.close()
        process.stderr.close()


def probe_status_resource(path, descriptor, timeout=MCP_TIMEOUT_SECONDS):
    """Exercise only initialize, resources/list and resources/read over stdio."""
    with tempfile.TemporaryDirectory(prefix="metis-cu-mcp-probe-") as cwd:
        env = {
            "PATH": "/usr/bin:/bin:/usr/sbin:/sbin",
            "HOME": cwd,
            "PWD": cwd,
            "LANG": "C",
            "LC_ALL": "C",
            "TZ": "UTC",
        }
        process = subprocess.Popen(
            [str(path)], cwd=cwd, env=env, stdin=subprocess.PIPE,
            stdout=subprocess.PIPE, stderr=subprocess.PIPE, bufsize=0,
            start_new_session=os.name == "posix",
        )
        try:
            deadline = time.monotonic() + timeout
            pending = bytearray()
            totals = {"stdout": 0, "stderr": 0}
            with selectors.DefaultSelector() as selector:
                selector.register(process.stdout, selectors.EVENT_READ, "stdout")
                selector.register(process.stderr, selectors.EVENT_READ, "stderr")
                _mcp_request(process, selector, pending, totals, 1, "initialize", deadline, {
                    "protocolVersion": "2024-11-05",
                    "capabilities": {"roots": {"listChanged": False}},
                    "clientInfo": {"name": "metis", "version": "0.1"},
                })
                # Match METIS' handshake; no tool invocation or permission request.
                try:
                    process.stdin.write(b'{"jsonrpc":"2.0","method":"notifications/initialized"}\n')
                except (BrokenPipeError, OSError) as error:
                    raise HelperValidationError("helper MCP closed during initialize") from error
                listing = _mcp_request(
                    process, selector, pending, totals, 2, "resources/list", deadline
                )
                resources = listing.get("resources")
                if not isinstance(resources, list):
                    raise HelperValidationError("helper MCP resources/list returned no resource array")
                status_resource = next(
                    (item for item in resources if isinstance(item, dict)
                     and item.get("name") == "Computer use status"), None
                )
                if status_resource is None:
                    raise HelperValidationError("helper MCP has no Computer use status resource")
                uri = status_resource.get("uri")
                if not isinstance(uri, str) or not uri or len(uri.encode("utf-8")) > MAX_RESOURCE_URI_BYTES:
                    raise HelperValidationError("helper MCP status resource has an invalid URI")
                result = _mcp_request(
                    process, selector, pending, totals, 3, "resources/read", deadline,
                    {"uri": uri},
                )
                contents = result.get("contents")
                if not isinstance(contents, list) or not contents:
                    raise HelperValidationError("helper MCP status resource has no contents")
                for content in contents:
                    if not isinstance(content, dict) or not isinstance(content.get("text"), str):
                        continue
                    try:
                        status = json.loads(content["text"])
                    except json.JSONDecodeError:
                        continue
                    if (
                        isinstance(status, dict)
                        and status.get("name") == descriptor["name"]
                        and status.get("protocolVersion") == descriptor["protocolVersion"]
                        and status.get("version") == descriptor["version"]
                        and valid_permissions(status.get("permissions"))
                        and isinstance(status.get("lifecycle"), dict)
                        and isinstance(status["lifecycle"].get("state"), str)
                        and status["lifecycle"].get("state") in ACTIVE_STATES
                    ):
                        return
                raise HelperValidationError("helper MCP status resource disagrees with descriptor")
        finally:
            _stop_process(process)


def decode_one_json(raw):
    try:
        text = raw.decode("utf-8")
        decoder = json.JSONDecoder()
        value, end = decoder.raw_decode(text)
    except (UnicodeDecodeError, json.JSONDecodeError) as error:
        raise HelperValidationError(f"helper returned invalid metadata JSON: {error}") from error
    if text[end:].strip():
        raise HelperValidationError("helper metadata contains trailing data")
    if not isinstance(value, dict):
        raise HelperValidationError("helper metadata must be a JSON object")
    return value


def valid_permissions(value):
    return isinstance(value, dict) and all(
        isinstance(value.get(key), str) and bool(value[key].strip())
        for key in REQUIRED_PERMISSIONS
    )


def validate(path, target, expected_version=None):
    path = checked_executable(path)
    descriptor = decode_one_json(run_helper(path, "--describe", "--json"))
    if descriptor.get("name") != "metis-cu":
        raise HelperValidationError(f"unexpected helper name: {descriptor.get('name')!r}")
    if descriptor.get("protocolVersion") != PROTOCOL_VERSION:
        raise HelperValidationError(
            f"incompatible helper protocol {descriptor.get('protocolVersion')!r}; "
            f"expected {PROTOCOL_VERSION}"
        )
    actual_target = f"{descriptor.get('platform')}-{descriptor.get('arch')}"
    if actual_target != target:
        raise HelperValidationError(f"helper target {actual_target!r} does not match {target!r}")
    version = descriptor.get("version")
    if not isinstance(version, str) or not VERSION_RE.fullmatch(version):
        raise HelperValidationError("helper reported an invalid version")
    if expected_version is not None and version != expected_version:
        raise HelperValidationError(
            f"helper descriptor version {version!r} does not match {expected_version!r}"
        )
    capabilities = descriptor.get("capabilities")
    if not isinstance(capabilities, list):
        raise HelperValidationError("helper capabilities must be a JSON array")
    missing = sorted(REQUIRED_CAPABILITIES - {item for item in capabilities if isinstance(item, str)})
    if missing:
        raise HelperValidationError("helper is missing required capabilities: " + ", ".join(missing))
    if not valid_permissions(descriptor.get("permissions")):
        raise HelperValidationError("helper descriptor is missing required permission statuses")

    version_line = run_helper(path, "--version").decode("utf-8", "replace").strip().splitlines()
    if version_line != [f"metis-cu {version}"]:
        raise HelperValidationError(f"helper --version output is not stable: {version_line!r}")
    probe_status_resource(path, descriptor)
    return descriptor


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--binary", type=Path, required=True)
    parser.add_argument("--target", required=True, help="expected GOOS-GOARCH target")
    parser.add_argument("--version")
    args = parser.parse_args(argv)
    try:
        descriptor = validate(args.binary, args.target, args.version)
    except (OSError, HelperValidationError) as error:
        parser.error(str(error))
    print(
        "computer-use helper compatible: "
        f"{descriptor['version']} {descriptor['platform']}-{descriptor['arch']}"
    )
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
