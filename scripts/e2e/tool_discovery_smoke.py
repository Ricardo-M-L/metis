#!/usr/bin/env python3
"""Run real Metis + stdio MCP discovery and cache introspection, with local SSE."""

import argparse
import json
import os
from pathlib import Path
import shlex
import subprocess
import sys
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

from cache_prefix_smoke import quit_tmux, respond_auxiliary_request, tmux_capture, verify_auxiliary_requests, wait_for


def serve_mcp():
    for line in sys.stdin:
        request = json.loads(line)
        if "id" not in request:
            continue
        method = request.get("method")
        if method == "initialize":
            result = {"protocolVersion": request["params"]["protocolVersion"], "capabilities": {"tools": {}}, "serverInfo": {"name": "fixture", "version": "1"}}
        elif method == "tools/list":
            result = {"tools": [{"name": "echo", "description": "Echo a synthetic value.", "inputSchema": {"type": "object", "properties": {"value": {"type": "string"}}, "required": ["value"]}, "annotations": {"readOnlyHint": True, "destructiveHint": False}}]}
        elif method == "tools/call":
            result = {"content": [{"type": "text", "text": "ECHO_OK:" + request["params"]["arguments"]["value"]}]}
        else:
            result = {}
        print(json.dumps({"jsonrpc": "2.0", "id": request["id"], "result": result}), flush=True)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--mcp-server", action="store_true")
    parser.add_argument("--binary", type=Path)
    parser.add_argument("--out", type=Path)
    parser.add_argument("--mode", choices=("default", "always"), default="default")
    parser.add_argument("--tmux", action="store_true")
    args = parser.parse_args()
    if args.mcp_server:
        serve_mcp()
        return
    if not args.binary or not args.out:
        parser.error("--binary and --out are required for the app test")
    binary, out = args.binary.resolve(strict=True), args.out.resolve()
    out.mkdir(parents=True, exist_ok=False)
    home, workspace = out / "home", out / "workspace"
    metis_home = home / ".metis"
    metis_home.mkdir(parents=True, exist_ok=True)
    workspace.mkdir(parents=True, exist_ok=True)
    requests = []
    auxiliary_requests = []
    lock = threading.Lock()
    tool_name = "mcp__fixture__echo"

    class Handler(BaseHTTPRequestHandler):
        def do_POST(self):
            body = json.loads(self.rfile.read(int(self.headers["Content-Length"])))
            if respond_auxiliary_request(self, body, auxiliary_requests):
                return
            with lock:
                requests.append(body)
                index = len(requests)
            offset = 1 if args.mode == "always" else 0
            if index == 1 and offset:
                name, arguments = "ToolSearch", {"query": "select:" + tool_name}
            elif index == 1 + offset:
                name, arguments = tool_name, {"value": "fixture"}
            elif index == 2 + offset:
                name, arguments = "MetisInfo", {"section": "cache"}
            else:
                name, arguments = None, None
            content = ({"type": "tool_use", "id": f"tool-{index}", "name": name, "input": {}}
                       if name else {"type": "text", "text": ""})
            delta = ({"type": "input_json_delta", "partial_json": json.dumps(arguments)}
                     if name else {"type": "text_delta", "text": "TOOL_CACHE_OK"})
            events = [
                {"type": "message_start", "message": {"id": f"fixture-{index}", "type": "message", "role": "assistant", "model": "fixture", "content": [], "usage": {"input_tokens": 100, "output_tokens": 0}}},
                {"type": "content_block_start", "index": 0, "content_block": content},
                {"type": "content_block_delta", "index": 0, "delta": delta},
                {"type": "content_block_stop", "index": 0},
                {"type": "message_delta", "delta": {"stop_reason": "tool_use" if name else "end_turn"}, "usage": {"output_tokens": 10}},
                {"type": "message_stop"},
            ]
            self.send_response(200)
            self.send_header("Content-Type", "text/event-stream")
            self.end_headers()
            for event in events:
                self.wfile.write(("event: " + event["type"] + "\ndata: " + json.dumps(event) + "\n\n").encode())
                self.wfile.flush()

        def log_message(self, *_):
            pass

    server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
    threading.Thread(target=server.serve_forever, daemon=True).start()
    (metis_home / "config.toml").write_text(f'''[provider]
default = "tool-fixture"
[provider.custom.tool-fixture]
transport = "anthropic_messages"
base_url = "http://127.0.0.1:{server.server_port}"
api_key_env = "TOOL_FIXTURE_KEY"
model = "claude-sonnet-4-6"
max_tokens = 1024
context_window = 200000
timeout_seconds = 15
[permission]
mode = "dontAsk"
[[permission.allow]]
tool = "{tool_name}"
[ui]
prompt_suggestions = false
''')
    (metis_home / "mcp.toml").write_text("[[servers]]\nname = \"fixture\"\ncommand = " + json.dumps(sys.executable) + "\nargs = " + json.dumps(["-B", str(Path(__file__).resolve()), "--mcp-server"]) + "\n")
    env = {"PATH": "/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin", "HOME": str(home),
           "METIS_HOME": str(metis_home), "TOOL_FIXTURE_KEY": "synthetic-key", "LANG": "en_US.UTF-8",
           "TERM": "xterm-256color", "METIS_AUTO_MEMORY": "0", "METIS_CATALOG_DISABLE": "1",
           "METIS_NO_UPDATE_CHECK": "1", "METIS_LAZY_MCP": "never", "PYTHONDONTWRITEBYTECODE": "1"}
    if args.mode == "always":
        env["ENABLE_TOOL_SEARCH"] = "true"
    session, started = f"metis-tools-{os.getpid()}", False
    try:
        if args.tmux:
            launch = "cd " + shlex.quote(str(workspace)) + " && exec env -i " + " ".join(shlex.quote(f"{k}={v}") for k, v in env.items())
            launch += " " + shlex.join([str(binary), "chat", "--provider", "tool-fixture", "--no-auth-wizard", "--max-iter", "8"])
            launcher = out / "launch.sh"
            launcher.write_text("#!/bin/sh\n" + launch + "\n")
            command = "/bin/sh " + shlex.quote(str(launcher)) + "; exit_code=$?; printf '\\nMETIS_PROCESS_EXIT=%s\\n' \"$exit_code\""
            subprocess.run(["tmux", "new-session", "-d", "-s", session, "-x", "160", "-y", "45", "/bin/bash --noprofile --norc"], check=True)
            started = True
            subprocess.run(["tmux", "send-keys", "-t", session, "-l", command], check=True)
            subprocess.run(["tmux", "send-keys", "-t", session, "Enter"], check=True)
            ready = wait_for(session, lambda text: "Type a message to start" in text or "Yes, I trust this folder" in text)
            if "Yes, I trust this folder" in ready:
                subprocess.run(["tmux", "send-keys", "-t", session, "Enter"], check=True)
                wait_for(session, lambda text: "Type a message to start" in text)
            subprocess.run(["tmux", "send-keys", "-t", session, "-l", "Call the fixture echo tool, then inspect cache observations."], check=True)
            subprocess.run(["tmux", "send-keys", "-t", session, "Enter"], check=True)
            pane = wait_for(session, lambda text: "TOOL_CACHE_OK" in text)
            (out / "pane.txt").write_text(pane)
            (out / "pane-exited.txt").write_text(quit_tmux(session))
        else:
            result = subprocess.run([str(binary), "run", "--provider", "tool-fixture", "--max-iter", "8", "--no-markdown", "Call the fixture echo tool, then inspect cache observations."], cwd=workspace, env=env, text=True, capture_output=True, timeout=60)
            (out / "stdout.txt").write_text(result.stdout)
            (out / "stderr.txt").write_text(result.stderr)
            assert result.returncode == 0, f"CLI failed: {result.stderr[-1500:]}"
            assert "TOOL_CACHE_OK" in result.stdout, "missing final response"
        assert len(requests) == (4 if args.mode == "always" else 3), "unexpected discovery/tool round count"
        verify_auxiliary_requests(auxiliary_requests)
        first_tools = {tool["name"]: tool for tool in requests[0]["tools"]}
        if args.mode == "always":
            assert tool_name not in first_tools, "unselected Deferred tool leaked into the provider tools array"
            assert "ToolSearch" in first_tools, "explicit Always omitted discovery"
            loaded = next(tool for tool in requests[1]["tools"] if tool["name"] == tool_name)
            assert "value" in loaded["input_schema"].get("properties", {}), "discovered tool was not hydrated"
        else:
            first = first_tools[tool_name]
            assert "value" in first["input_schema"].get("properties", {}), "small default tool still requires discovery"
        results = [block for message in requests[-1]["messages"] for block in message.get("content", []) if isinstance(block, dict) and block.get("type") == "tool_result"]
        assert all(not result.get("is_error", False) for result in results), f"tool execution failed: {results}"
        assert any("ECHO_OK:fixture" in str(result.get("content")) for result in results), "real stdio MCP tool did not execute"
        cache = next(str(result["content"]) for result in results if "[cache]" in str(result.get("content")))
        assert "cache_miss = unknown" in cache and "reported_hit_rate = unknown" in cache, "missing accounting was misreported as a cache miss"
        (out / "result.json").write_text(json.dumps({"passed": True, "interactive": args.tmux, "discovery_mode": args.mode, "request_count": len(requests)}, indent=2))
        print(f"PASS: real Metis {'TUI' if args.tmux else 'CLI'}, stdio MCP echo, {args.mode} discovery, and cache introspection")
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


if __name__ == "__main__":
    main()
