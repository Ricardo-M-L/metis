#!/usr/bin/env python3
"""Run the real Desktop backend against isolated state and a controlled local model.

No user configuration, sessions, or provider keys are loaded. The provider records
only test inputs, never request headers or system prompts. State is retained in a
new temporary directory for inspection. POST /fixture/stop shuts down all children.
"""

from __future__ import annotations

import argparse
import json
import os
from pathlib import Path
import re
import shlex
import signal
import socket
import subprocess
import tempfile
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from urllib.error import URLError
from urllib.request import Request, urlopen


def free_port() -> int:
    with socket.socket() as sock:
        sock.bind(("127.0.0.1", 0))
        return sock.getsockname()[1]


class Fixture:
    def __init__(self, root: Path):
        self.root = root
        self.stopped = threading.Event()
        self.lock = threading.Lock()
        self.calls: list[dict] = []
        self.gates: dict[str, threading.Event] = {}

    def record(self, entry: dict) -> None:
        with self.lock:
            with (self.root / "model-evidence.jsonl").open("a", encoding="utf-8") as stream:
                stream.write(json.dumps(entry, ensure_ascii=False) + "\n")

    def gate(self, name: str) -> threading.Event:
        with self.lock:
            return self.gates.setdefault(name, threading.Event())


def last_user_input(body: dict) -> str:
    items = body.get("input", body.get("messages", []))
    if isinstance(items, str):
        return items
    for item in reversed(items):
        if item.get("role") == "user":
            content = item.get("content", "")
            if isinstance(content, str):
                return content
            return "\n".join(part.get("text", "") for part in content if isinstance(part, dict))
    return "fixture request"


def handler_for(fixture: Fixture):
    class Handler(BaseHTTPRequestHandler):
        protocol_version = "HTTP/1.1"

        def log_message(self, *_args):
            pass

        def send_json(self, body: object, status: int = 200) -> None:
            data = json.dumps(body, ensure_ascii=False).encode()
            self.send_response(status)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(data)))
            self.end_headers()
            self.wfile.write(data)

        def do_HEAD(self):
            self.send_response(200)
            self.send_header("Content-Length", "0")
            self.end_headers()

        def do_GET(self):
            if self.path == "/fixture/state":
                with fixture.lock:
                    self.send_json({"calls": fixture.calls, "gates": list(fixture.gates)})
            elif self.path == "/v1/models":
                self.send_json({"object": "list", "data": [{"id": "desktop-fixture-model", "object": "model"}]})
            elif self.path == "/fixture/health":
                self.send_json({"ok": True})
            else:
                self.send_json({"error": "Unknown fixture route"}, 404)

        def do_POST(self):
            length = int(self.headers.get("Content-Length", "0"))
            if length > 8 * 1024 * 1024:
                self.send_json({"error": "Request too large"}, 413)
                return
            try:
                body = json.loads(self.rfile.read(length) or b"{}")
            except ValueError:
                self.send_json({"error": "Invalid JSON"}, 400)
                return
            if self.path == "/fixture/release":
                name = str(body.get("gate", ""))
                fixture.gate(name).set()
                self.send_json({"released": name})
                return
            if self.path == "/fixture/stop":
                self.send_json({"stopping": True})
                fixture.stopped.set()
                return
            if self.path != "/v1/responses":
                self.send_json({"error": {"message": "Unknown model endpoint"}}, 404)
                return
            user_input = last_user_input(body)
            memory_extraction = user_input.startswith("Extract any DURABLE facts from this user-assistant exchange")
            with fixture.lock:
                number = len(fixture.calls) + 1
                entry = {"number": number, "kind": "memory_extraction" if memory_extraction else "agent_turn", "input": user_input, "model": body.get("model"), "started": time.time(), "state": "streaming"}
                fixture.calls.append(entry)
            fixture.record({"event": "started", **entry})
            gate_match = None if memory_extraction else re.search(r"\[gate:([A-Za-z0-9_-]+)\]", user_input)
            delay_match = None if memory_extraction else re.search(r"\[slow:(\d+)\]", user_input)
            text = "[]" if memory_extraction else f"FIXTURE_REPLY_{number}: {user_input[-500:]}"
            response_id = f"resp_fixture_{number}"
            message_id = f"msg_fixture_{number}"
            output = [{"type": "message", "id": message_id, "role": "assistant", "status": "completed", "content": [{"type": "output_text", "text": text, "annotations": []}]}]
            envelope = {"id": response_id, "object": "response", "status": "completed", "model": body.get("model"), "output": output, "usage": {"input_tokens": 128, "output_tokens": 16, "total_tokens": 144, "input_tokens_details": {"cached_tokens": 64}}}
            if not body.get("stream"):
                self.send_json(envelope)
                entry["state"] = "completed"
                entry["completed"] = time.time()
                fixture.record({"event": "completed", **entry})
                return
            try:
                self.send_response(200)
                self.send_header("Content-Type", "text/event-stream")
                self.send_header("Cache-Control", "no-cache")
                self.send_header("Connection", "close")
                self.end_headers()
                self.close_connection = True
                sequence = 0

                def emit(kind: str, **payload):
                    nonlocal sequence
                    sequence += 1
                    self.wfile.write(("data: " + json.dumps({"type": kind, "sequence_number": sequence, **payload}, ensure_ascii=False) + "\n\n").encode())
                    self.wfile.flush()

                emit("response.created", response={**envelope, "status": "in_progress", "output": []})
                emit("response.output_item.added", output_index=0, item={"type": "message", "id": message_id, "role": "assistant", "status": "in_progress", "content": []})
                emit("response.output_text.delta", output_index=0, item_id=message_id, content_index=0, delta=f"FIXTURE_REPLY_{number}: ")
                deadline = time.monotonic() + (min(int(delay_match.group(1)), 120) if delay_match else 0)
                wait_gate = fixture.gate(gate_match.group(1)) if gate_match else None
                while not fixture.stopped.is_set() and (time.monotonic() < deadline or (wait_gate and not wait_gate.is_set())):
                    self.wfile.write(b": fixture waiting\n\n")
                    self.wfile.flush()
                    fixture.stopped.wait(0.15)
                if fixture.stopped.is_set():
                    return
                emit("response.output_text.delta", output_index=0, item_id=message_id, content_index=0, delta=user_input[-500:])
                emit("response.output_text.done", output_index=0, item_id=message_id, content_index=0, text=text)
                emit("response.output_item.done", output_index=0, item=output[0])
                emit("response.completed", response=envelope)
                self.wfile.write(b"data: [DONE]\n\n")
                self.wfile.flush()
                entry["state"] = "completed"
                entry["completed"] = time.time()
                fixture.record({"event": "completed", **entry})
            except (BrokenPipeError, ConnectionResetError, OSError):
                entry["state"] = "cancelled"
                fixture.record({"event": "cancelled", **entry})

    return Handler


def json_http(url: str, body: dict | None = None):
    request = Request(url, data=json.dumps(body).encode() if body is not None else None, headers={"Content-Type": "application/json"})
    with urlopen(request, timeout=30) as response:
        return json.load(response)


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--binary", type=Path, help="Use this CLI instead of compiling to the temporary fixture directory")
    parser.add_argument("--port", type=int, default=0, help="Browser backend port; defaults to an unused loopback port")
    parser.add_argument("--no-web", action="store_true", help="Only serve the model; use launch-native.sh for native verification")
    parser.add_argument("--duration", type=int, default=1800, help="Maximum fixture lifetime in seconds")
    parser.add_argument("--seed", action="store_true", help="Create two real completed sessions over HTTP")
    args = parser.parse_args()
    repo = Path(__file__).resolve().parents[2]
    root = Path(tempfile.mkdtemp(prefix="metis-desktop-e2e-"))
    home, workspace = root / "home", root / "workspace"
    for directory in (home, workspace, home / "skills"):
        directory.mkdir(mode=0o700)
    fixture = Fixture(root)
    provider = ThreadingHTTPServer(("127.0.0.1", 0), handler_for(fixture))
    provider.daemon_threads = True
    threading.Thread(target=provider.serve_forever, daemon=True).start()
    provider_url = f"http://127.0.0.1:{provider.server_port}"
    binary = args.binary.resolve() if args.binary else root / "metis"
    if not args.binary:
        with (root / "build.log").open("w") as log:
            subprocess.run(["go", "build", "-o", str(binary), "./cmd/metis"], cwd=repo, stdout=log, stderr=log, check=True)
    config = f'''[provider]
default = "desktop-fixture"
[provider.custom.desktop-fixture]
transport = "openai_responses"
base_url = "{provider_url}/v1"
api_key_env = "METIS_E2E_API_KEY"
model = "desktop-fixture-model"
max_tokens = 512
timeout_seconds = 180
[session]
dir = "{home}/sessions"
skill_dir = "{home}/skills"
max_iterations = 5
[permission]
mode = "default"
'''
    (home / "config.toml").write_text(config)
    (home / "config.toml").chmod(0o600)
    # A positive allowlist drops ambient OPENAI/ANTHROPIC/ARK keys, tokens,
    # provider selectors, proxies and service-specific overrides.
    environment = {key: os.environ[key] for key in ("PATH", "HOME", "TMPDIR", "SHELL", "LANG", "LC_ALL") if key in os.environ}
    environment.update({"METIS_HOME": str(home), "METIS_BIN": str(binary), "METIS_E2E_API_KEY": "fixture-only-not-a-real-key", "METIS_AUTO_MEMORY": "0", "METIS_NO_UPDATE_CHECK": "1", "NO_PROXY": "127.0.0.1,localhost"})
    (root / "environment.json").write_text(json.dumps(environment, indent=2))
    (root / "environment.json").chmod(0o600)
    native = repo / "metis-desktop/build/bin/METIS.app/Contents/MacOS/metis-desktop"
    launcher = root / "launch-native.sh"
    launcher.write_text("#!/bin/sh\nset -eu\ncd " + shlex.quote(str(workspace)) + "\nexec env -i " + " ".join(shlex.quote(f"{key}={value}") for key, value in environment.items()) + " " + shlex.quote(str(native)) + " --workspace " + shlex.quote(str(workspace)) + " --metis-bin " + shlex.quote(str(binary)) + "\n")
    launcher.chmod(0o700)
    children: list[subprocess.Popen] = []
    opened_logs = []
    for sig in (signal.SIGINT, signal.SIGTERM):
        signal.signal(sig, lambda _sig, _frame: fixture.stopped.set())
    try:
        web_url = None
        if not args.no_web:
            port = args.port or free_port()
            web_url = f"http://127.0.0.1:{port}"
            log = (root / "webui.log").open("w")
            opened_logs.append(log)
            child = subprocess.Popen([str(binary), "desktop", "--web", "--port", str(port)], cwd=workspace, env=environment, stdout=log, stderr=log, start_new_session=True)
            children.append(child)
            for _ in range(200):
                if child.poll() is not None:
                    raise RuntimeError(f"Desktop backend exited; inspect {root / 'webui.log'}")
                try:
                    json_http(web_url + "/api/health")
                    break
                except (URLError, TimeoutError):
                    time.sleep(0.1)
            else:
                raise RuntimeError("Desktop fixture did not become ready within 20 seconds")
        metadata = {"ready": True, "root": str(root), "web_url": web_url, "fixture_url": provider_url, "metis_home": str(home), "workspace": str(workspace), "metis_bin": str(binary), "native_launcher": str(launcher), "evidence": str(root / "model-evidence.jsonl")}
        if args.seed and web_url:
            metadata["sessions"] = []
            for label in ("Fixture Alpha", "Fixture Beta"):
                artifact = workspace / (label.replace(" ", "-").lower() + "-report.md")
                artifact.write_text(f"# {label} artifact\n\nUnique evidence for {label}; unrelated to the other session.\n", encoding="utf-8")
                session = json_http(web_url + "/api/sessions", {})
                identifier = session.get("id", session.get("sessionId"))
                json_http(web_url + "/api/turns", {"sessionId": identifier, "input": f"{label}. Report: [{label} report](<{artifact}>)"})
                json_http(web_url + "/api/sessions/rename", {"id": identifier, "title": label})
                metadata["sessions"].append({"id": identifier, "label": label, "artifact": str(artifact)})
        (root / "fixture.json").write_text(json.dumps(metadata, indent=2))
        print(json.dumps(metadata), flush=True)
        fixture.stopped.wait(max(1, min(args.duration, 7200)))
    finally:
        fixture.stopped.set()
        for child in children:
            if child.poll() is None:
                os.killpg(child.pid, signal.SIGINT)
                try:
                    child.wait(timeout=15)
                except subprocess.TimeoutExpired:
                    os.killpg(child.pid, signal.SIGKILL)
                    child.wait(timeout=5)
        provider.shutdown()
        provider.server_close()
        for log in opened_logs:
            log.close()
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
