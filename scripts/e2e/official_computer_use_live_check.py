#!/usr/bin/env python3
"""Opt-in acceptance of the published official Computer Use helper.

Uses two owned, opaque primary-display fixtures, two real Metis queries, and
local-only MCP screenshots. A test-only stdio proxy forwards the exact official
helper's replies and fences every input with independent foreground identity.
This does not upgrade the old helper to native AX or prove multi-monitor use.
--self-test reads no credentials, compiles nothing and performs no GUI actions.
"""
from __future__ import annotations
import argparse
import base64
import hashlib
import json
import os
from pathlib import Path
import plistlib
import selectors
import shutil
import signal
import struct
import subprocess
import sys
import time
import tomllib
from typing import Any
import uuid
import zlib

from persistent_agent_live_check import PersistentAgentLiveCheck, atomic_json, digest, require, toml_table, tool_calls, utc_now
from computer_use_cross_app_acceptance import CrossAppCheck, hydrate_native_results

PREFIX = "mcp__computer-use__"
MODEL_TOOLS = ("open_application", "screen_size", "left_click", "key", "type")
INITIAL = "fixture initial value"
BUILTINS = ("Safari", "Google Chrome", "Firefox", "Microsoft Edge", "Arc", "Brave Browser", "Terminal", "iTerm2",
            "Alacritty", "Visual Studio Code", "Code", "Cursor", "Xcode", "GoLand", "IntelliJ IDEA", "PyCharm", "WebStorm",
            "RubyMine", "Codex", "METIS", "Metis", "飞书", "Feishu", "Finder", "System Settings")


def screen_size_arguments() -> dict[str, str]:
    # The published helper requires a nonempty dummy field, even for local calls.
    return {"_": "noop"}


def configured_permission_mode(config: dict[str, Any], expected: str = "fullAccess") -> str:
    mode = config.get("permission", {}).get("mode")
    require(expected == "fullAccess" and mode == expected,
            "Desktop acceptance requires the existing configured fullAccess posture; never elevate or substitute a permission mode")
    return mode


def proxy_mcp_config(policy: Path, working_dir: Path) -> str:
    """Keep the ordinary generic MCP sandbox; grant only its private cwd."""
    working_dir = working_dir.resolve()
    require(working_dir != Path("/") and policy.resolve().parent == working_dir,
            "Proxy policy and writable evidence must remain in the exact private fixture cwd")
    return ('[[servers]]\nname="computer-use"\ncommand=' + json.dumps(sys.executable) +
            '\nargs=' + json.dumps([str(Path(__file__).resolve()), "--proxy", str(policy)]) +
            '\nworking_dir=' + json.dumps(str(working_dir)) +
            '\ndisabled=false\nenabled_tools=' + json.dumps(list(MODEL_TOOLS)) + '\n')


def fixture_query_prompt(plan: list[dict[str, Any]]) -> str:
    return ("Authorized acceptance in owned disposable applications. Execute the following exact six computer-use calls sequentially, one tool call per turn, in this order. "
            "Use the registered " + PREFIX + " tools and their supplied schemas. If ToolSearch is available and a schema is deferred, discover it first; otherwise invoke the already exposed computer-use tools directly. "
            "Do not invent success, skip or repeat calls. Stop immediately on any tool error or uncertainty; never retry an input. "
            "Coordinates are already the fixture's global logical primary-display coordinates. Every input has return_screenshot=false. "
            "Do not call any other tool, request_access, screenshots, clipboard, Bash or files. The test-only foreground fence independently verifies each real helper result. "
            "The required plan is " + json.dumps(plan) + ". After the sixth successful call briefly report completion.")


def state_identity(value: Any, fixture: dict[str, Any], *, active: bool = True) -> None:
    require(isinstance(value, dict) and value.get("schema_version") == 1 and
            all(value.get(k) == fixture[k] for k in ("nonce", "pid", "name", "bundle_id")) and
            type(value.get("ticks")) is int and value["ticks"] > 0 and value.get("opaque_full_display") is True,
            "Independent fixture identity/opaque display did not match")
    if active:
        require(value.get("active") is True and value.get("foreground_pid") == fixture["pid"] and
                value.get("foreground_bundle_id") == fixture["bundle_id"], "Foreground is not the exact owned fixture")
    screen = value.get("screen", {})
    require(screen.get("x") == 0 and screen.get("y") == 0 and
            type(screen.get("width")) is int and screen["width"] > 800 and
            type(screen.get("height")) is int and screen["height"] > 500,
            "This acceptance supports only an owned full primary display at origin zero")
    for key in ("input_point", "button_point"):
        point = value.get(key, {})
        require(set(point) == {"x", "y"} and all(type(point.get(k)) is int for k in ("x", "y")) and
                20 <= point["x"] <= screen["width"] - 20 and 20 <= point["y"] <= screen["height"] - 20,
                "Owned input coordinate lies outside the interior fixture display")
    require(value["input_point"] != value["button_point"], "Owned input and button coordinates overlap")


def read_state(fixture: dict[str, Any], *, active: bool = True) -> dict[str, Any]:
    path = Path(fixture["state_path"])
    require(not path.is_symlink() and path.is_file() and path.stat().st_size <= 65536,
            "Owned fixture state file is absent, replaced or oversized")
    require(-0.2 <= time.time() - path.stat().st_mtime <= 0.8, "Owned foreground observation is stale")
    state = json.loads(path.read_text())
    state_identity(state, fixture, active=active)
    return state


def wait_state(fixture: dict[str, Any], predicate=lambda _: True, *, active: bool = True, seconds: float = 3) -> dict[str, Any]:
    end = time.monotonic() + seconds
    last = "Owned fixture state is not ready"
    while time.monotonic() < end:
        try:
            state = read_state(fixture, active=active)
            require(predicate(state), "Owned fixture state has not reached the exact expected result")
            return state
        except (OSError, ValueError, AssertionError) as error:
            last = str(error)
            time.sleep(0.025)
    raise AssertionError(last)


def expected_steps(fixture: dict[str, Any], state: dict[str, Any], marker: str) -> list[dict[str, Any]]:
    require(marker.isascii() and 0 < len(marker) < 80, "Typing must remain short ASCII, without clipboard fallback")
    return [{"name": "open_application", "arguments": {"name": fixture["app_path"]}},
            {"name": "screen_size", "arguments": screen_size_arguments()},
            {"name": "left_click", "arguments": {**state["input_point"], "return_screenshot": False}},
            {"name": "key", "arguments": {"combo": "cmd+a", "return_screenshot": False}},
            {"name": "type", "arguments": {"text": marker, "return_screenshot": False}},
            {"name": "left_click", "arguments": {**state["button_point"], "return_screenshot": False}}]


def validate_plan_call(index: int, name: str, arguments: Any, plan: list[dict[str, Any]]) -> None:
    require(index < len(plan) and name == plan[index]["name"] and arguments == plan[index]["arguments"],
            "Call differs from the exact owned-app plan; duplicate, unexpected and retried mutations are forbidden")


def official_geometry(value: Any, state: dict[str, Any]) -> dict[str, Any]:
    require(isinstance(value, dict) and value.get("active") == 0, "Official helper did not select primary display zero")
    primary = next((d for d in value.get("displays", []) if d.get("index") == 0), {})
    require(all(primary.get(k) == state["screen"][k] for k in ("x", "y", "width", "height")),
            "Official display zero disagrees with the opaque fixture's logical coordinate bounds")
    return primary


def parse_png(data: bytes) -> tuple[int, int, bytes, int]:
    """Bounded, lossless RGB/RGBA PNG decode, enough for the helper's encoder."""
    require(data.startswith(b"\x89PNG\r\n\x1a\n") and len(data) <= 20 << 20, "Screenshot is not a bounded PNG")
    pos, compressed, size, channels, ended = 8, bytearray(), None, 0, False
    while pos + 12 <= len(data):
        count = struct.unpack_from(">I", data, pos)[0]
        kind = data[pos + 4:pos + 8]
        body = data[pos + 8:pos + 8 + count]
        require(pos + count + 12 <= len(data) and zlib.crc32(kind + body) & 0xffffffff == struct.unpack_from(">I", data, pos + 8 + count)[0],
                "Screenshot PNG chunk is truncated or corrupt")
        if kind == b"IHDR":
            w, h, depth, color, compression, filtering, interlace = struct.unpack(">IIBBBBB", body)
            require(depth == 8 and color in (2, 6) and compression == filtering == interlace == 0 and 0 < w * h <= 4_000_000,
                    "Unsupported screenshot PNG geometry/encoding")
            size, channels = (w, h), 3 if color == 2 else 4
        elif kind == b"IDAT":
            compressed.extend(body)
        elif kind == b"IEND":
            ended = True
            break
        pos += count + 12
    require(size is not None and ended, "Screenshot PNG has no dimensions or end marker")
    w, h = size
    rowbytes = w * channels
    decoder = zlib.decompressobj()
    raw = decoder.decompress(bytes(compressed), (rowbytes + 1) * h + 1)
    require(len(raw) == (rowbytes + 1) * h and decoder.eof and not decoder.unused_data, "Screenshot PNG pixels are incomplete or oversized")
    output, previous = bytearray(), bytearray(rowbytes)
    for y in range(h):
        method = raw[y * (rowbytes + 1)]
        row = bytearray(raw[y * (rowbytes + 1) + 1:(y + 1) * (rowbytes + 1)])
        require(method <= 4, "Invalid screenshot PNG row filter")
        for x in range(rowbytes):
            left = row[x - channels] if x >= channels else 0
            up, corner = previous[x], previous[x - channels] if x >= channels else 0
            predictor = 0
            if method == 1: predictor = left
            elif method == 2: predictor = up
            elif method == 3: predictor = (left + up) // 2
            elif method == 4:
                p = left + up - corner
                a, b, c = abs(p - left), abs(p - up), abs(p - corner)
                predictor = left if a <= b and a <= c else up if b <= c else corner
            row[x] = (row[x] + predictor) & 255
        output.extend(row); previous = row
    return w, h, bytes(output), channels


def screenshot_oracle(data: bytes, fixture: dict[str, Any], screen: dict[str, Any]) -> dict[str, Any]:
    w, h, pixels, channels = parse_png(data)
    require(abs(w / h - screen["width"] / screen["height"]) < 0.015 and w >= 600 and h >= 400,
            "Screenshot aspect/size disagrees with the owned primary-display fixture")
    bits = int(fixture["nonce"][:2], 16)
    samples = []
    for i in range(8):
        x, y = int(w * (0.1375 + i * 0.1)), int(h * 0.5)
        offset = (y * w + x) * channels
        red, green, blue = pixels[offset:offset + 3]
        require(blue < 160 and ((green > red + 60) if bits & (1 << i) else (red > green + 60)),
                "Screenshot does not contain this fixture's unique eight-bit visual nonce")
        samples.append([red, green, blue])
    return {"width": w, "height": h, "sha256": hashlib.sha256(data).hexdigest(), "bytes": len(data),
            "visual_nonce_verified": True, "samples": samples, "image_sent_to_model": False}


class MCP:
    def __init__(self, helper: str, env: dict[str, str], log: Path):
        self.err = log.open("ab")
        self.process = subprocess.Popen([helper], env=env, stdin=subprocess.PIPE, stdout=subprocess.PIPE,
            stderr=self.err, start_new_session=True)
        self.counter = 0
    def request(self, method: str, params: dict[str, Any] | None = None) -> dict[str, Any]:
        self.counter += 1
        value = {"jsonrpc": "2.0", "id": self.counter, "method": method, "params": params or {}}
        self.send(value)
        return self.receive(self.counter)
    def send(self, value: dict[str, Any]) -> None:
        self.process.stdin.write((json.dumps(value) + "\n").encode()); self.process.stdin.flush()
    def receive(self, request_id: Any) -> dict[str, Any]:
        deadline = time.monotonic() + 25
        selector = selectors.DefaultSelector(); selector.register(self.process.stdout, selectors.EVENT_READ)
        try:
            while time.monotonic() < deadline:
                require(selector.select(max(0, deadline - time.monotonic())), "Official helper MCP response timed out")
                line = self.process.stdout.readline(30 << 20)
                require(line and len(line) < 30 << 20, "Official helper MCP exited or emitted an oversized response")
                response = json.loads(line)
                if response.get("id") == request_id:
                    return response
            raise AssertionError("Official helper MCP response deadline exceeded")
        finally: selector.close()
    def initialize(self) -> dict[str, Any]:
        result = self.request("initialize", {"protocolVersion": "2024-11-05", "clientInfo": {"name": "metis-official-acceptance", "version": "1"}, "capabilities": {}})
        require("result" in result, "Official helper MCP initialization failed")
        self.send({"jsonrpc": "2.0", "method": "notifications/initialized"})
        tools = self.request("tools/list").get("result", {}).get("tools", [])
        names = {t.get("name"): t for t in tools}
        require(set(MODEL_TOOLS) | {"screenshot"} <= set(names), "Published helper omitted required real tools")
        for name in ("left_click", "key", "type"):
            require("return_screenshot" in names[name].get("inputSchema", {}).get("properties", {}), "Published input tool cannot suppress screenshots")
        screen_schema = names["screen_size"].get("inputSchema", {})
        require(screen_schema.get("required") == ["_"] and
                screen_schema.get("properties", {}).get("_", {}).get("type") == "string",
                "Published screen_size schema changed; stop before any model input")
        return names
    def close(self) -> None:
        try: self.process.stdin.close()
        except OSError: pass
        try: self.process.wait(timeout=3)
        except subprocess.TimeoutExpired:
            os.killpg(self.process.pid, signal.SIGTERM)
            try: self.process.wait(timeout=3)
            except subprocess.TimeoutExpired: os.killpg(self.process.pid, signal.SIGKILL); self.process.wait(timeout=3)
        self.err.close()


def helper_reply_diagnostic(response: dict[str, Any] | None) -> dict[str, Any] | None:
    """Bounded original error evidence; images and arbitrary payloads never leave locally."""
    if response is None: return None
    content = response.get("result", {}).get("content", [])
    text = "\n".join(str(block.get("text", "")) for block in content if block.get("type") == "text")
    error = response.get("error", {})
    return {"sha256": hashlib.sha256(json.dumps(response, sort_keys=True).encode()).hexdigest(),
            "jsonrpc_error_code": error.get("code"), "jsonrpc_error_message": str(error.get("message", ""))[:8192],
            "is_error": response.get("result", {}).get("isError", False), "text": text[:8192],
            "text_truncated": len(text) > 8192,
            "nontext_block_types": [block.get("type") for block in content if block.get("type") != "text"]}


class OfficialToolError(AssertionError): pass


def result_ok(response: dict[str, Any]) -> None:
    if "result" not in response or response["result"].get("isError"):
        evidence = helper_reply_diagnostic(response)
        raise OfficialToolError("Official helper error: " + (evidence["text"] or evidence["jsonrpc_error_message"] or "missing tool result"))


def stopped_reply(request_id: Any, error: BaseException, actual: dict[str, Any] | None) -> dict[str, Any]:
    # Preserve the exact helper's real text-only failure. A fence failure never
    # reuses a successful result, and image-bearing errors are never sent upstream.
    if isinstance(error, OfficialToolError) and actual is not None:
        blocks = actual.get("result", {}).get("content", [])
        if ("result" in actual and all(b.get("type") == "text" for b in blocks) and
                len(json.dumps(actual)) <= 32768): return actual
        if "error" in actual and set(actual["error"]) <= {"code", "message"}: return actual
    return {"jsonrpc": "2.0", "id": request_id, "result": {"isError": True, "content": [
            {"type": "text", "text": "Owned fixture safety fence stopped this acceptance: " + str(error)[:8192]}]}}


def run_proxy(policy_path: Path) -> int:
    """Test-only fence. Forward real replies unchanged; never invent success."""
    policy = json.loads(policy_path.read_text())
    require(digest(Path(policy["helper"])) == policy["helper_sha256"], "Official helper bytes changed before proxy start")
    client = MCP(policy["helper"], os.environ.copy(), policy_path.with_suffix(".helper-stderr.log"))
    index, blocked = 0, False
    fixture, plan = policy["fixture"], policy["plan"]
    log = Path(policy["guard_log"])
    atomic_json(policy_path.with_name(policy_path.stem + ".proxy-" + str(os.getpid()) + ".json"),
                {"pid": os.getpid(), "helper_pid": client.process.pid, "policy": str(policy_path), "helper": policy["helper"]})
    try:
        for line in sys.stdin.buffer:
            request = json.loads(line)
            reply, before = None, None
            if request.get("method") == "tools/call":
                try:
                    require(not blocked, "Foreground fence is latched after an earlier failure")
                    params = request.get("params", {})
                    validate_plan_call(index, params.get("name"), params.get("arguments", {}), plan)
                    if index == 0:
                        states = [read_state(f, active=False) for f in policy["fixtures"]]
                        active = [s for s, f in zip(states, policy["fixtures"]) if s.get("active") and s.get("foreground_pid") == f["pid"] and s.get("foreground_bundle_id") == f["bundle_id"]]
                        require(len(active) == 1, "Opening the target app requires another exact owned fixture in front")
                        before = active[0]
                    else: before = read_state(fixture)
                    client.send(request); reply = client.receive(request["id"]); result_ok(reply)
                    if index == 1:
                        geometry = json.loads("".join(b.get("text", "") for b in reply["result"].get("content", []) if b.get("type") == "text"))
                        official_geometry(geometry, before)
                    def done(s):
                        if index == 3: return s["selection_length"] == len(INITIAL) and s["input"] == INITIAL and s["counter"] == 0
                        if index == 4: return s["input"] == policy["marker"] and s["counter"] == 0
                        if index == 5: return s["input"] == policy["marker"] and s["counter"] == 1
                        return s["input"] == INITIAL and s["counter"] == 0
                    after = wait_state(fixture, done)
                    if index > 0: require(after["focus_serial"] == before["focus_serial"], "Focus changed during an input/read call")
                    with log.open("a") as stream: stream.write(json.dumps({"index": index, "name": params["name"], "arguments": params.get("arguments", {}), "before": before, "after": after, "actual_helper_reply_sha256": hashlib.sha256(json.dumps(reply, sort_keys=True).encode()).hexdigest(), "passed": True}) + "\n")
                    index += 1
                except BaseException as error:
                    blocked = True
                    with log.open("a") as stream: stream.write(json.dumps({"index": index, "name": request.get("params", {}).get("name"),
                        "passed": False, "failure": str(error), "before": before,
                        "actual_helper_reply": helper_reply_diagnostic(reply)}) + "\n")
                    reply = stopped_reply(request.get("id"), error, reply)
            else:
                client.send(request)
                if "id" in request:
                    reply = client.receive(request["id"])
                    if request.get("method") == "tools/list" and "result" in reply:
                        reply["result"]["tools"] = [t for t in reply["result"].get("tools", []) if t.get("name") in MODEL_TOOLS]
            if reply is not None:
                sys.stdout.write(json.dumps(reply) + "\n"); sys.stdout.flush()
    finally: client.close()
    return 1 if blocked else 0


class OfficialCheck(PersistentAgentLiveCheck):
    def __init__(self, args):
        super().__init__(args)
        self.fixtures = []
        self.report["boundary"] = "Published official helper, primary display only, two owned opaque apps, actual Metis queries and local-only screenshots. Test-only proxy forwards real official results and adds host foreground fences; no claim that these fences/native AX exist in helper 0.0.3. CLI inherits its launching host TCC responsibility; this does not establish native Desktop host permissions. No third-party apps, OS permission changes, clipboard or screenshots sent to model."
        paths = [Path.home() / ".metis-cu" / n for n in ("config.toml", "granted.json")]
        original = Path(os.environ.get("METIS_HOME", str(Path.home() / ".metis")))
        paths += [original / n for n in ("mcp.toml", ".credentials/auth.json", "components/computer-use/active.json")]
        self.original_guards = {str(p): digest(p) if p.exists() else None for p in paths}

    def prepare(self):
        require(sys.platform == "darwin", "Live acceptance requires native macOS")
        self.stage(); self.load_route()
        require((self.provider, self.model) == (self.args.expected_provider, self.args.expected_model), "Refusing a substitute provider/model")
        self.permission_mode = configured_permission_mode(tomllib.loads(self.original_config.read_text()), self.args.expected_permission_mode)
        self.record("private CLI preserves the existing configured Desktop permission posture",
                    original_mode=self.permission_mode, private_mode=self.permission_mode,
                    user_permission_configuration_changed=False)
        self.env["METIS_CU_BUNDLE_DIR"] = str(self.args.bundle_dir.resolve())
        installed, _ = self.command(["cu", "install", "--json"], "install-official-from-packaged-archives")
        require(installed.get("installed") and installed.get("source") == "official" and digest(Path(installed["path"])) == self.args.expected_helper_sha256,
                "Private official install does not match the authenticated published helper")
        self.helper = Path(installed["path"])
        require(self.helper.is_relative_to(self.home), "Official helper install escaped the private home")
        desc = subprocess.run([str(self.helper), "--describe", "--json"], env=self.env, capture_output=True, text=True, timeout=15, check=True)
        description = json.loads(desc.stdout)
        require(description.get("protocolVersion") == 1 and description.get("permissions", {}).get("accessibility") == "granted" and
                description.get("permissions", {}).get("screenRecording") == "granted", "Required existing macOS permissions are unavailable; no OS prompt was requested")
        self.record("official packaged helper installed with existing screen and input permissions", status=installed, description=description)
        if self.args.start_gate:
            self.record("preflight ready; waiting for explicitly scheduled foreground window")
            atomic_json(self.output / "preflight-ready.json", {"passed": True, "permissions": description["permissions"], "start_gate": str(self.args.start_gate)})
            end = time.monotonic() + self.args.start_wait_seconds
            while not self.args.start_gate.exists() and time.monotonic() < end: time.sleep(.1)
            require(self.args.start_gate.exists(), "No foreground-window start signal arrived; no GUI actions performed")

    def build_fixtures(self):
        source = Path(__file__).with_name("official-computer-use-fixture.swift")
        compiled = self.root / "fixture-program"
        result = subprocess.run(["swiftc", str(source), "-o", str(compiled)], env=self.env, capture_output=True, text=True, timeout=120)
        require(result.returncode == 0, "Owned fixture compile failed: " + result.stderr[-1500:])
        nonces = []
        for letter in ("A", "B"):
            nonce = uuid.uuid4().hex[:12]
            while nonce[:2] in [n[:2] for n in nonces]: nonce = uuid.uuid4().hex[:12]
            nonces.append(nonce)
            name, bundle = "Metis Official CU " + letter + " " + nonce, "dev.metis.officialcu." + letter.lower() + nonce
            app = self.root / (name + ".app"); exe = app / "Contents/MacOS" / name
            exe.parent.mkdir(parents=True); shutil.copyfile(compiled, exe); exe.chmod(0o700)
            (app / "Contents/Info.plist").write_bytes(plistlib.dumps({"CFBundleIdentifier": bundle, "CFBundleName": name,
                "CFBundleDisplayName": name, "CFBundleExecutable": name, "CFBundlePackageType": "APPL", "NSHighResolutionCapable": True}))
            subprocess.run(["codesign", "--force", "--sign", "-", str(app)], capture_output=True, check=True, timeout=15)
            state = self.root / ("fixture-" + letter + ".json")
            process = subprocess.Popen([str(exe), str(state), nonce], env=self.env, stdin=subprocess.DEVNULL,
                stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, start_new_session=True)
            self.processes.append(process)
            fixture = {"name": name, "bundle_id": bundle, "pid": process.pid, "nonce": nonce, "state_path": str(state), "app_path": str(app)}
            self.fixtures.append(fixture); wait_state(fixture, active=False)
        compiled.unlink()
        grants = {name: "read" for name in BUILTINS}; grants.update({f["name"]: "full" for f in self.fixtures})
        folder = self.user_home / ".metis-cu"; folder.mkdir(mode=0o700)
        atomic_json(folder / "granted.json", grants)
        (folder / "config.toml").write_text('[screenshot]\nformat="png"\nmax_width=1280\nmax_height=800\n[gate]\nfrontmost_timeout_ms=1500\nfrontmost_cache_ttl_ms=1\nhost_terminal_tier="disabled"\n[mouse]\nsettle_ms=100\n')
        (folder / "config.toml").chmod(0o600)
        # Actual tools/list from the exact installed executable, without input.
        client = MCP(str(self.helper), self.env, self.root / "mcp-preflight-stderr.log")
        try: tools = client.initialize()
        finally: client.close()
        self.record("published helper actual MCP tool registry and fixture-only input policy", tools={n: tools[n]["inputSchema"] for n in (*MODEL_TOOLS, "screenshot")},
            helper_sha256=digest(self.helper), fixtures=self.fixtures, model_screenshots_disabled=True,
            unknown_app_input_blocked_by_test_proxy=True)

    def query_fixture(self, fixture, letter):
        initial = read_state(fixture, active=False)
        marker = "metis-official-" + letter.lower() + "-" + uuid.uuid4().hex[:10]
        plan = expected_steps(fixture, initial, marker)
        policy = self.root / ("policy-" + letter + ".json")
        guard_log = self.root / ("guard-" + letter + ".jsonl")
        atomic_json(policy, {"helper": str(self.helper), "helper_sha256": self.args.expected_helper_sha256,
            "fixture": fixture, "fixtures": self.fixtures, "plan": plan, "marker": marker, "guard_log": str(guard_log)})
        text = toml_table(["provider"], {"default": self.provider}) + "\n" + toml_table(["provider", "custom", self.provider], self.settings)
        text += "\n" + toml_table(["session"], {"dir": str(self.home / "sessions"), "skill_dir": str(self.home / "skills"), "max_iterations": 16})
        text += "\n" + toml_table(["tools"], {"allowed": ["ToolSearch", *(PREFIX + n for n in MODEL_TOOLS)]})
        text += "\n" + toml_table(["permission"], {"mode": self.permission_mode})
        (self.home / "config.toml").write_text(text)
        (self.home / "mcp.toml").write_text(proxy_mcp_config(policy, self.root))
        prompt = fixture_query_prompt(plan)
        before = self.trace_snapshot()
        _, command = self.command(["run", "--no-auth-wizard", "--no-markdown", prompt], "real-official-app-" + letter, json_output=False)
        proof, events = self.real_proof(before, "real-official-app-" + letter, required_tools=tuple(PREFIX + n for n in MODEL_TOOLS))
        paths = {sid: next(p for p in self.home.rglob(sid + ".jsonl") if p.parent.name == "sessions") for sid in proof["session_ids"]}
        hydrate_native_results(events, paths)  # Includes a strict no-image durable-history assertion.
        guards = [json.loads(line) for line in guard_log.read_text().splitlines()]
        require(len(guards) == len(plan) and all(g.get("passed") for g in guards), "Actual proxy sequence or independent foreground proof failed")
        for index, row in enumerate(guards): validate_plan_call(index, row["name"], row["arguments"], plan)
        state = wait_state(fixture, lambda s: s["input"] == marker and s["counter"] == 1)
        fixture["expected_marker"] = marker
        atomic_json(self.output / ("fixture-" + letter + "-host-state.json"), state)
        atomic_json(self.output / ("fixture-" + letter + "-guard-proof.json"), guards)
        self.record("real Metis query completed official helper keyboard/mouse app " + letter, command=command, usage=proof, final_host_state=state,
            exact_guarded_helper_calls=len(guards), test_only_proxy_used=True, model_images=0)
        self.screenshot(fixture, letter)

    def screenshot(self, fixture, letter):
        before = read_state(fixture)
        client = MCP(str(self.helper), self.env, self.root / ("screenshot-" + letter + "-stderr.log"))
        try:
            client.initialize()
            geometry = client.request("tools/call", {"name": "screen_size", "arguments": screen_size_arguments()}); result_ok(geometry)
            geometry = json.loads("".join(b.get("text", "") for b in geometry["result"]["content"] if b.get("type") == "text"))
            official_geometry(geometry, before)
            read_state(fixture)  # Immediately before actual screenshot dispatch.
            result = client.request("tools/call", {"name": "screenshot", "arguments": {"display": 0}}); result_ok(result)
            after = read_state(fixture)
            require(before["focus_serial"] == after["focus_serial"], "Focus changed during local screenshot")
            images = [b for b in result["result"].get("content", []) if b.get("type") == "image"]
            require(len(images) == 1 and images[0].get("mimeType") == "image/png", "Official helper did not return one real lossless PNG")
            data = base64.b64decode(images[0]["data"], validate=True)
            proof = screenshot_oracle(data, fixture, before["screen"])
            path = self.output / ("official-fixture-" + letter + "-screenshot.png"); path.write_bytes(data)
            self.record("actual official MCP screenshot verified locally for app " + letter, screenshot=path.name, proof=proof,
                foreground_pid=fixture["pid"], foreground_bundle_id=fixture["bundle_id"])
        finally: client.close()

    def cleanup(self):
        errors = []
        for process in self.processes:
            try: self.stop(process)
            except BaseException as error: errors.append(str(error))
        # The proxy is a Python process, so bind its PID to this exact private policy argument before stopping it.
        for path in self.root.glob("policy-*.proxy-*.json"):
            record = json.loads(path.read_text())
            command = subprocess.run(["ps", "-p", str(record["pid"]), "-o", "command="], capture_output=True, text=True).stdout
            if str(Path(__file__).resolve()) in command and record["policy"] in command:
                try: os.kill(record["pid"], signal.SIGTERM)
                except ProcessLookupError: pass
        try: CrossAppCheck.stop_native_descendants(self)
        except BaseException as error: errors.append(str(error))
        super().cleanup()
        self.report["cleanup"]["official_native_cleanup_errors"] = errors
        require(not errors, "Owned helper/fixture cleanup failed")

    def run_official(self):
        def interrupted(signum, frame): raise TimeoutError("Bounded official acceptance interrupted")
        previous = {s: signal.signal(s, interrupted) for s in (signal.SIGALRM, signal.SIGTERM, signal.SIGINT)}
        signal.setitimer(signal.ITIMER_REAL, self.args.case_timeout)
        try:
            self.prepare(); self.build_fixtures()
            self.query_fixture(self.fixtures[0], "A"); self.query_fixture(self.fixtures[1], "B")
            a = read_state(self.fixtures[0], active=False)
            require(a["input"] == self.fixtures[0]["expected_marker"] and a["counter"] == 1, "App A was changed during app B execution")
            self.record("app A business result preserved after official cross-app B execution", host_state=a)
            self.report["passed"] = True
        except BaseException as error:
            self.report.update({"passed": False, "failure_type": type(error).__name__, "failure": self.redact(str(error))})
        finally:
            signal.setitimer(signal.ITIMER_REAL, 0)
            # Keep bounded owned-fixture guard diagnostics even if the model
            # aborts before normal per-query evidence collection.
            for path in self.root.glob("guard-*.jsonl"):
                if path.is_file() and not path.is_symlink() and path.stat().st_size <= 128 << 10:
                    (self.output / path.name).write_text(self.redact(path.read_text()))
            for fixture in self.fixtures:
                try: atomic_json(self.output / (fixture["name"] + "-last-state.json"), read_state(fixture, active=False))
                except BaseException: pass
            for label, action in (("cleanup", self.cleanup), ("evidence_copy", self.copy_evidence), ("source_freeze", self.verify_freeze)):
                try: action()
                except BaseException as error: self.report.update({"passed": False, label + "_failure": self.redact(str(error))})
            # Helpers are now stopped and their stderr streams flushed. Retain
            # only owned, bounded text logs before deleting the private workspace.
            retained = []
            for pattern in ("*.helper-stderr.log", "mcp-preflight-stderr.log", "screenshot-*-stderr.log"):
                for path in self.root.glob(pattern):
                    if path.is_file() and not path.is_symlink():
                        raw = path.read_bytes()
                        (self.output / path.name).write_text(self.redact(raw[-131072:].decode("utf-8", errors="replace")))
                        retained.append({"file": path.name, "original_bytes": len(raw), "truncated": len(raw) > 131072,
                            "original_sha256": hashlib.sha256(raw).hexdigest()})
            self.report["retained_official_helper_stderr"] = retained
            now = {p: digest(Path(p)) if Path(p).exists() else None for p in self.original_guards}
            if now != self.original_guards: self.report.update({"passed": False, "original_user_files_changed": True})
            else: self.report["original_user_files_unchanged"] = True
            shutil.rmtree(self.root, ignore_errors=True)
            self.report.setdefault("cleanup", {})["temporary_workspace_destroyed"] = not self.root.exists()
            self.report["passed"] = self.report["passed"] and not self.root.exists()
            self.report.update({"finished_utc": utc_now(), "elapsed_seconds": round(time.monotonic() - self.started, 3)})
            self.report = json.loads(self.redact(json.dumps(self.report))); self.secrets.clear()
            atomic_json(self.output / "verification-report.json", self.report)
            for number, handler in previous.items(): signal.signal(number, handler)
        print(json.dumps({"passed": self.report["passed"], "failure": self.report.get("failure"), "report": str(self.output / "verification-report.json")}), flush=True)
        return 0 if self.report["passed"] else 1


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    mode = parser.add_mutually_exclusive_group(required=True)
    mode.add_argument("--self-test", action="store_true")
    mode.add_argument("--proxy", type=Path)
    mode.add_argument("--run-live", action="store_true")
    parser.add_argument("--repo", type=Path, default=Path(__file__).resolve().parents[2])
    parser.add_argument("--binary", type=Path); parser.add_argument("--expected-sha256")
    parser.add_argument("--source-freeze", type=Path); parser.add_argument("--output", type=Path)
    parser.add_argument("--bundle-dir", type=Path); parser.add_argument("--expected-helper-sha256")
    parser.add_argument("--expected-provider", default="easycliproxyapi"); parser.add_argument("--expected-model", default="gpt-6.1-sol")
    parser.add_argument("--expected-permission-mode", choices=["fullAccess"], default="fullAccess")
    parser.add_argument("--destroy-input", action="store_true")
    parser.add_argument("--query-timeout", type=float, default=180); parser.add_argument("--case-timeout", type=float, default=600)
    parser.add_argument("--start-gate", type=Path); parser.add_argument("--start-wait-seconds", type=float, default=180)
    args = parser.parse_args(argv)
    if args.self_test:
        import unittest
        result = unittest.TextTestRunner(verbosity=2).run(unittest.defaultTestLoader.discover(str(Path(__file__).parent), pattern="test_official_computer_use_live_check.py"))
        return 0 if result.wasSuccessful() else 1
    if args.proxy: return run_proxy(args.proxy.resolve())
    require(all((args.binary, args.expected_sha256, args.source_freeze, args.output, args.bundle_dir, args.expected_helper_sha256, args.destroy_input)),
            "Live mode requires a disposable exact CLI, source freeze, packaged archive directory, authenticated helper SHA, output and --destroy-input")
    binary = args.binary.resolve()
    require(not any(p.suffix.lower() == ".app" for p in binary.parents),
            "Never give any app bundle's executable to a destructive acceptance; supply a disposable external copy")
    terminal_cli = shutil.which("metis")
    require(not terminal_cli or binary != Path(terminal_cli).resolve(),
            "Never give the installed terminal CLI to a destructive acceptance")
    return OfficialCheck(args).run_official()


if __name__ == "__main__": raise SystemExit(main())
