#!/usr/bin/env python3
"""Exercise durable Responses context with a real Metis process (synthetic SSE)."""

import argparse
import json
import os
from pathlib import Path
import shlex
import subprocess
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

from cache_prefix_smoke import quit_tmux, respond_auxiliary_request, tmux_capture, verify_auxiliary_requests, wait_for


def persisted_context_texts(transcript):
    """Replay active history, including atomic checkpoint replacements."""
    messages = []
    lines = transcript.read_text().splitlines()
    for index, line in enumerate(lines):
        try:
            entry = json.loads(line)
        except json.JSONDecodeError:
            # Match Store.Load's tolerance for a torn final append only.
            if index == len(lines) - 1:
                break
            raise
        if entry.get("type") == "history_replace":
            messages = entry.get("messages") or []
        elif entry.get("type") == "message" and entry.get("message"):
            messages.append(entry["message"])
    return {block.get("text", "") for message in messages for block in message.get("content", []) if block.get("type") == "context_update"}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--binary", type=Path, required=True)
    parser.add_argument("--out", type=Path, required=True)
    parser.add_argument("--tmux", action="store_true")
    args = parser.parse_args()
    binary, out = args.binary.resolve(strict=True), args.out.resolve()
    out.mkdir(parents=True, exist_ok=False)
    home, workspace = out / "home", out / "workspace"
    metis_home = home / ".metis"
    metis_home.mkdir(parents=True, exist_ok=True)
    workspace.mkdir(parents=True, exist_ok=True)
    requests = []
    auxiliary_requests = []
    durability = []
    lock = threading.Lock()

    class Handler(BaseHTTPRequestHandler):
        def do_POST(self):
            body = json.loads(self.rfile.read(int(self.headers["Content-Length"])))
            if respond_auxiliary_request(self, body, auxiliary_requests):
                return
            expected_context = [part.get("text", "") for item in body.get("input", []) if item.get("role") == "developer" for part in item.get("content", [])]
            persisted = set()
            for transcript in (metis_home / "sessions").glob("*.jsonl"):
                persisted.update(persisted_context_texts(transcript))
            with lock:
                requests.append(body)
                durability.append(bool(expected_context) and all(text in persisted for text in expected_context))
                n = len(requests)
            self.send_response(200)
            self.send_header("Content-Type", "text/event-stream")
            self.end_headers()
            events = [
                {"type": "response.output_text.delta", "delta": f"CTX_RESPONSE_{n}"},
                {"type": "response.completed", "response": {"id": f"fixture-{n}", "status": "completed", "output": []}},
            ]
            for event in events:
                self.wfile.write(("data: " + json.dumps(event) + "\n\n").encode())
                self.wfile.flush()

        def log_message(self, *_):
            pass

    server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
    threading.Thread(target=server.serve_forever, daemon=True).start()
    (metis_home / "config.toml").write_text(f'''[provider]
default = "context-fixture"
[provider.custom.context-fixture]
transport = "openai_responses"
responses_profile = "openai"
responses_state_mode = "local"
base_url = "http://127.0.0.1:{server.server_port}"
api_key_env = "CONTEXT_FIXTURE_KEY"
model = "fixture-model"
max_tokens = 256
context_window = 200000
timeout_seconds = 15
[permission]
mode = "dontAsk"
[ui]
prompt_suggestions = false
''')
    env = {"PATH": "/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin", "HOME": str(home),
           "METIS_HOME": str(metis_home), "CONTEXT_FIXTURE_KEY": "synthetic-key", "LANG": "en_US.UTF-8",
           "TERM": "xterm-256color", "METIS_AUTO_MEMORY": "0", "METIS_CATALOG_DISABLE": "1",
           "METIS_NO_UPDATE_CHECK": "1", "ENABLE_TOOL_SEARCH": "false"}
    session, started = f"metis-context-{os.getpid()}", False
    try:
        if args.tmux:
            launch = "cd " + shlex.quote(str(workspace)) + " && exec env -i " + " ".join(shlex.quote(f"{k}={v}") for k, v in env.items())
            launch += " " + shlex.join([str(binary), "chat", "--provider", "context-fixture", "--no-auth-wizard", "--max-iter", "4"])
            launcher = out / "launch.sh"
            launcher.write_text("#!/bin/sh\n" + launch + "\n")
            command = "/bin/sh " + shlex.quote(str(launcher)) + "; exit_code=$?; printf '\\nMETIS_PROCESS_EXIT=%s\\n' \"$exit_code\""
            subprocess.run(["tmux", "new-session", "-d", "-s", session, "-x", "160", "-y", "45", "/bin/bash --noprofile --norc"], check=True)
            started = True
            subprocess.run(["tmux", "send-keys", "-t", session, "-l", command], check=True)
            subprocess.run(["tmux", "send-keys", "-t", session, "Enter"], check=True)
            ready = wait_for(session, lambda text: "Type a message to start" in text or "Yes, I trust this folder" in text)
            if "Yes, I trust this folder" in ready:
                # This is the newly created, empty synthetic workspace only.
                subprocess.run(["tmux", "send-keys", "-t", session, "Enter"], check=True)
                wait_for(session, lambda text: "Type a message to start" in text)
            for n in (1, 2):
                subprocess.run(["tmux", "send-keys", "-t", session, "-l", f"Summarize fixture turn {n}."], check=True)
                subprocess.run(["tmux", "send-keys", "-t", session, "Enter"], check=True)
                pane = wait_for(session, lambda text: f"CTX_RESPONSE_{n}" in text)
                (out / f"pane-{n}.txt").write_text(pane)
            (out / "pane-exited.txt").write_text(quit_tmux(session))
        else:
            for n in (1, 2):
                command = [str(binary), "run", "--provider", "context-fixture", "--max-iter", "4", "--no-markdown"]
                if n == 2:
                    command.append("--continue")
                command.append(f"Summarize fixture turn {n}.")
                result = subprocess.run(command, cwd=workspace, env=env, capture_output=True, text=True, timeout=45)
                (out / f"stdout-{n}.txt").write_text(result.stdout)
                (out / f"stderr-{n}.txt").write_text(result.stderr)
                assert result.returncode == 0, f"CLI turn {n} failed: {result.stderr[-1500:]}"
                assert f"CTX_RESPONSE_{n}" in result.stdout, f"missing turn {n} response"
        assert len(requests) == 2, f"expected two requests, got {len(requests)}"
        verify_auxiliary_requests(auxiliary_requests)
        assert all(durability), f"context was not on disk before provider request: {durability}"
        first, second = requests
        assert first["instructions"] == second["instructions"], "stable instructions changed"
        assert first["tools"] == second["tools"], "stable tool definitions changed"
        assert first["prompt_cache_key"] == second["prompt_cache_key"], "cache affinity changed"
        assert second["input"][:len(first["input"])] == first["input"], "previous complete wire prefix was rewritten"
        assert any(item.get("role") == "developer" for item in first["input"]), "fixture did not exercise runtime context"
        (out / "result.json").write_text(json.dumps({"passed": True, "interactive": args.tmux, "request_count": len(requests), "persisted_before_request": durability}, indent=2))
        print(f"PASS: real Metis {'TUI multi-turn' if args.tmux else 'CLI process resume'} preserves the complete prior Responses input prefix")
        print(f"Evidence: {out}")
    finally:
        if started:
            if subprocess.run(["tmux", "has-session", "-t", session], capture_output=True).returncode == 0:
                (out / "pane-final.txt").write_text(tmux_capture(session))
            subprocess.run(["tmux", "kill-session", "-t", session], capture_output=True)
        server.shutdown()
        server.server_close()
        (out / "requests.json").write_text(json.dumps(requests, indent=2))
        (out / "auxiliary-requests.json").write_text(json.dumps(auxiliary_requests, indent=2))
        (out / "durability.json").write_text(json.dumps({"persisted_before_request": durability}, indent=2))


if __name__ == "__main__":
    main()
