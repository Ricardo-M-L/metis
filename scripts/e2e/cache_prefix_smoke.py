#!/usr/bin/env python3
"""Drive a newly built Metis through real Fork HTTP requests and optional tmux.

The local SSE server is scripted, not a real model or cache performance probe.
All prompts, credentials and homes are isolated synthetic fixtures.
"""

import argparse
import json
import os
from pathlib import Path
import re
import shlex
import subprocess
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer


class Fixture:
    def __init__(self):
        self.requests = []
        self.auxiliary_requests = []
        self.lock = threading.Lock()

    def events(self, body):
        with self.lock:
            self.requests.append(body)
            index = len(self.requests)
        tool = index == 1
        if "input" in body:
            if tool:
                item = {"type": "function_call", "id": "item-fork", "call_id": "fork-fixture", "name": "Fork", "arguments": ""}
                complete = dict(item, arguments=json.dumps({"directive": "Return CACHE_CHILD_OK only."}))
                return [
                    {"type": "response.output_item.added", "output_index": 0, "item": item},
                    {"type": "response.function_call_arguments.delta", "item_id": "item-fork", "output_index": 0, "delta": complete["arguments"]},
                    {"type": "response.output_item.done", "output_index": 0, "item": complete},
                    {"type": "response.completed", "response": {"id": "fixture-parent", "status": "completed", "output": [complete]}},
                ]
            return [
                {"type": "response.output_text.delta", "delta": "CACHE_CHILD_OK" if index == 2 else "CACHE_PARENT_OK"},
                {"type": "response.completed", "response": {"id": f"fixture-{index}", "status": "completed", "output": []}},
            ]
        block = ({"type": "tool_use", "id": "fork-fixture", "name": "Fork", "input": {}}
                 if tool else {"type": "text", "text": ""})
        delta = ({"type": "input_json_delta", "partial_json": json.dumps({"directive": "Return CACHE_CHILD_OK only."})}
                 if tool else {"type": "text_delta", "text": "CACHE_CHILD_OK" if index == 2 else "CACHE_PARENT_OK"})
        return [
            {"type": "message_start", "message": {"id": f"fixture-{index}", "type": "message", "role": "assistant", "model": "claude-sonnet-4-6", "content": [], "usage": {"input_tokens": 100, "output_tokens": 0}}},
            {"type": "content_block_start", "index": 0, "content_block": block},
            {"type": "content_block_delta", "index": 0, "delta": delta},
            {"type": "content_block_stop", "index": 0},
            {"type": "message_delta", "delta": {"stop_reason": "tool_use" if tool else "end_turn"}, "usage": {"output_tokens": 10}},
            {"type": "message_stop"},
        ]


def strip_cache_markers(value):
    if isinstance(value, dict):
        return {key: strip_cache_markers(item) for key, item in value.items() if key != "cache_control"}
    if isinstance(value, list):
        return [strip_cache_markers(item) for item in value]
    return value


def verify_fork_tools(parent, child):
    # Root permission transitions capture the parent Gate and must never be
    # advertised to children. Retained tools preserve their definitions/order;
    # this fixture does not claim an identical complete tool-array prefix.
    root_only = {"EnterPlanMode", "ExitPlanMode"}
    expected = [tool for tool in parent if tool["name"] not in root_only]
    assert strip_cache_markers(child) == strip_cache_markers(expected), "Fork changed allowed tool definitions or ordering"


def auxiliary_kind(body):
    instructions = str(body.get("instructions", body.get("system", ""))).lower()
    if "predict the short message" in instructions:
        return "suggestion"
    if "extract durable facts" in instructions:
        return "memory_distillation"
    return "unknown"


def respond_auxiliary_request(handler, body, requests):
    """Serve current-main nonstream enrichment without shifting dialogue steps."""
    if body.get("stream"):
        return False
    requests.append(body)
    text = "NONE" if auxiliary_kind(body) == "suggestion" else "[]"
    if "input" in body:
        response = {"id": "fixture-aux", "object": "response", "status": "completed", "output": [{"id": "fixture-aux-text", "type": "message", "role": "assistant", "status": "completed", "content": [{"type": "output_text", "text": text, "annotations": []}]}], "usage": {"input_tokens": 1, "output_tokens": 1, "total_tokens": 2}}
    else:
        response = {"id": "fixture-aux", "type": "message", "role": "assistant", "model": body["model"], "content": [{"type": "text", "text": text}], "stop_reason": "end_turn", "usage": {"input_tokens": 1, "output_tokens": 1}}
    encoded = json.dumps(response).encode()
    handler.send_response(200)
    handler.send_header("Content-Type", "application/json")
    handler.send_header("Content-Length", str(len(encoded)))
    handler.end_headers()
    handler.wfile.write(encoded)
    return True


def verify_auxiliary_requests(requests):
    assert all(not body.get("tools") and auxiliary_kind(body) != "unknown" for body in requests), "unexpected auxiliary provider request"


def verify_requests(requests):
    assert len(requests) == 3, f"expected parent/Fork/parent, got {len(requests)} requests"
    parent, child, final = requests
    if "input" in parent:
        for field in ("instructions", "model", "reasoning", "prompt_cache_key", "max_output_tokens"):
            assert child.get(field) == parent.get(field), f"Fork changed {field}"
        verify_fork_tools(parent["tools"], child["tools"])
        assert child["input"][:len(parent["input"])] == parent["input"], "Fork rewrote the Responses history prefix"
        calls = [item for item in child["input"] if item.get("type") == "function_call"]
        results = [item for item in child["input"] if item.get("type") == "function_call_output"]
        assert len(calls) == 1 and len(results) == 1 and calls[0]["call_id"] == results[0]["call_id"], "invalid Responses Fork tool pairing"
        assert any("CACHE_CHILD_OK" in str(item) for item in final["input"]), "parent did not receive child result"
        return
    for field in ("system", "model", "thinking", "output_config", "max_tokens"):
        assert strip_cache_markers(child.get(field)) == strip_cache_markers(parent.get(field)), f"Fork changed {field}"
    verify_fork_tools(parent["tools"], child["tools"])
    prefix = strip_cache_markers(parent["messages"])
    actual = strip_cache_markers(child["messages"])
    assert actual[:len(prefix)] == prefix, "Fork rewrote the parent history"
    calls = [block for msg in actual for block in msg.get("content", []) if isinstance(block, dict) and block.get("type") == "tool_use"]
    results = [block for msg in actual for block in msg.get("content", []) if isinstance(block, dict) and block.get("type") == "tool_result"]
    assert len(calls) == 1 and len(results) == 1, "Fork duplicated an outstanding tool result"
    assert results[0]["tool_use_id"] == calls[0]["id"], "Fork left an orphaned call"
    assert any("CACHE_CHILD_OK" in str(msg) for msg in final["messages"]), "parent did not receive child result"


def tmux_capture(session):
    return subprocess.check_output(["tmux", "capture-pane", "-p", "-S", "-", "-t", session], text=True)


def tmux_exit_code(text):
    for line in text.splitlines():
        match = re.match(r"^METIS_PROCESS_EXIT=(\d+)", line.strip())
        if match:
            return int(match.group(1))
    return None


def wait_for(session, predicate, timeout=30):
    end = time.monotonic() + timeout
    while time.monotonic() < end:
        if subprocess.run(["tmux", "has-session", "-t", session], capture_output=True).returncode:
            raise RuntimeError("Metis tmux session exited before the expected state")
        text = tmux_capture(session)
        if predicate(text):
            return text
        if tmux_exit_code(text) is not None:
            raise RuntimeError("Metis exited:\n" + text[-3000:])
        time.sleep(0.1)
    raise TimeoutError("Metis did not reach the expected terminal state")


def quit_tmux(session):
    # A visible final text delta can precede EventDone/checkpoint completion.
    # If the app explicitly refuses /quit while that turn drains, retry only
    # after observing a new refusal; do not cancel the turn to end the test.
    refusal = "can't /quit mid-turn"
    refused = tmux_capture(session).count(refusal)
    def submit():
        subprocess.run(["tmux", "send-keys", "-t", session, "-l", "/quit"], check=True)
        subprocess.run(["tmux", "send-keys", "-t", session, "Enter"], check=True)
    submit()
    end = time.monotonic() + 15
    while time.monotonic() < end:
        pane = tmux_capture(session)
        code = tmux_exit_code(pane)
        if code is not None:
            assert code == 0, f"Metis exited {code}: {pane[-3000:]}"
            return pane
        current = pane.count(refusal)
        if current > refused:
            refused = current
            submit()
        time.sleep(0.1)
    raise TimeoutError("Metis did not finish a graceful /quit: " + tmux_capture(session)[-3000:])


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--binary", type=Path, required=True)
    parser.add_argument("--out", type=Path, required=True)
    parser.add_argument("--tmux", action="store_true")
    parser.add_argument("--transport", choices=("anthropic_messages", "openai_responses"), default="anthropic_messages")
    args = parser.parse_args()
    binary = args.binary.resolve(strict=True)
    out = args.out.resolve()
    out.mkdir(parents=True, exist_ok=False)
    home, workspace = out / "home", out / "workspace"
    metis_home = home / ".metis"
    metis_home.mkdir(parents=True, exist_ok=True)
    workspace.mkdir(exist_ok=True)
    fixture = Fixture()

    class Handler(BaseHTTPRequestHandler):
        def do_POST(self):
            body = json.loads(self.rfile.read(int(self.headers["Content-Length"])))
            if respond_auxiliary_request(self, body, fixture.auxiliary_requests):
                return
            self.send_response(200)
            self.send_header("Content-Type", "text/event-stream")
            self.end_headers()
            for event in fixture.events(body):
                self.wfile.write(("event: " + event["type"] + "\ndata: " + json.dumps(event) + "\n\n").encode())
                self.wfile.flush()

        def log_message(self, *_):
            pass

    server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
    threading.Thread(target=server.serve_forever, daemon=True).start()
    config = f'''[provider]
default = "cache-fixture"
[provider.custom.cache-fixture]
transport = "{args.transport}"
responses_profile = "openai"
responses_state_mode = "local"
base_url = "http://127.0.0.1:{server.server_port}"
api_key_env = "CACHE_FIXTURE_KEY"
model = "claude-sonnet-4-6"
max_tokens = 1024
context_window = 200000
timeout_seconds = 15
[permission]
mode = "dontAsk"
[[permission.allow]]
tool = "Fork"
[ui]
prompt_suggestions = false
'''
    (metis_home / "config.toml").write_text(config)
    env = {"PATH": "/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin", "HOME": str(home), "METIS_HOME": str(metis_home),
           "LANG": "en_US.UTF-8", "TERM": "xterm-256color", "CACHE_FIXTURE_KEY": "synthetic-key",
           "METIS_AUTO_MEMORY": "0", "METIS_CATALOG_DISABLE": "1", "METIS_NO_UPDATE_CHECK": "1",
           "ENABLE_TOOL_SEARCH": "false"}
    session = f"metis-cache-{os.getpid()}"
    started = False
    try:
        if args.tmux:
            launch = "cd " + shlex.quote(str(workspace)) + " && exec env -i " + " ".join(shlex.quote(f"{key}={value}") for key, value in env.items())
            launch += " " + shlex.join([str(binary), "chat", "--provider", "cache-fixture", "--no-auth-wizard", "--max-iter", "6"])
            launcher = out / "launch.sh"
            launcher.write_text("#!/bin/sh\n" + launch + "\n")
            # Keep the typed line below the macOS PTY canonical-input limit.
            command = "/bin/sh " + shlex.quote(str(launcher))
            command += "; exit_code=$?; printf '\\nMETIS_PROCESS_EXIT=%s\\n' \"$exit_code\""
            subprocess.run(["tmux", "new-session", "-d", "-s", session, "-x", "160", "-y", "45", "/bin/bash --noprofile --norc"], check=True)
            started = True
            subprocess.run(["tmux", "send-keys", "-t", session, "-l", command], check=True)
            subprocess.run(["tmux", "send-keys", "-t", session, "Enter"], check=True)
            ready = wait_for(session, lambda text: "Type a message to start" in text or "Yes, I trust this folder" in text)
            if "Yes, I trust this folder" in ready:
                # Approve only this invocation's newly created empty fixture workspace.
                subprocess.run(["tmux", "send-keys", "-t", session, "Enter"], check=True)
                wait_for(session, lambda text: "Type a message to start" in text)
            subprocess.run(["tmux", "send-keys", "-t", session, "-l", "Use Fork to return CACHE_CHILD_OK, then summarize."], check=True)
            subprocess.run(["tmux", "send-keys", "-t", session, "Enter"], check=True)
            pane = wait_for(session, lambda text: "CACHE_PARENT_OK" in text)
            (out / "pane.txt").write_text(pane)
            (out / "pane-exited.txt").write_text(quit_tmux(session))
        else:
            result = subprocess.run([str(binary), "run", "--provider", "cache-fixture", "--max-iter", "6", "--no-markdown", "Use Fork to return CACHE_CHILD_OK, then summarize."], cwd=workspace, env=env, text=True, capture_output=True, timeout=45)
            (out / "stdout.txt").write_text(result.stdout)
            (out / "stderr.txt").write_text(result.stderr)
            assert result.returncode == 0, f"CLI exited {result.returncode}: {result.stderr[-1500:]}"
            assert "CACHE_PARENT_OK" in result.stdout, "CLI did not render the final response"
        verify_requests(fixture.requests)
        verify_auxiliary_requests(fixture.auxiliary_requests)
        (out / "requests.json").write_text(json.dumps(fixture.requests, indent=2))
        (out / "result.json").write_text(json.dumps({"passed": True, "interactive": args.tmux, "transport": args.transport, "request_count": len(fixture.requests)}, indent=2))
        print(f"PASS: real Metis {'TUI' if args.tmux else 'CLI'} completed Fork with unchanged history and allowed tool definitions; root permission controls excluded; 3 synthetic HTTP requests")
        print(f"Evidence: {out}")
    finally:
        if started:
            # This invocation exclusively owns this PID-scoped test session.
            if subprocess.run(["tmux", "has-session", "-t", session], capture_output=True).returncode == 0:
                (out / "pane-final.txt").write_text(tmux_capture(session))
            subprocess.run(["tmux", "kill-session", "-t", session], capture_output=True)
        server.shutdown()
        server.server_close()
        if fixture.requests:
            (out / "requests.json").write_text(json.dumps(fixture.requests, indent=2))
        (out / "auxiliary-requests.json").write_text(json.dumps(fixture.auxiliary_requests, indent=2))


if __name__ == "__main__":
    main()
