#!/usr/bin/env python3
"""Opt-in plugin validation acceptance using the caller's real selected model.

The caller supplies a source-frozen disposable CLI and a minimal private
profile. The model route is never replaced by a fixture: only the MCP service
is local. Three real queries must invoke the installed plugin's short, long
and Unicode remote tools. Random receipts exist only inside the MCP service
until a real tools/call returns them. Native Skill invocation is also checked.

Unsupported declarations are checked separately by actual CLI installation
and manifest inspection, then left in the private plugin tree for the third
query to prove runtime preflight skips them before any MCP startup. This does
not establish working scheduler, theme, hook or arbitrary plugin execution.

All copied credentials, supplied/private CLIs, prompts, wire dumps and owned
listeners/processes are destroyed on every outcome. Retained evidence contains
only safe projections, hashes and host oracle results. --self-test uses no
network, authentication, model or product executable.
"""
from __future__ import annotations

import argparse
from collections import Counter
import hashlib
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import json
import os
from pathlib import Path
import re
import secrets
import shutil
import signal
import socket
import subprocess
import threading
import time
import tomllib
from typing import Any
import uuid

from persistent_agent_live_check import (PersistentAgentLiveCheck, assert_skill_invoked,
    atomic_json, digest, final_assistant, history_snapshots, require, trace_proof, utc_now)
from project_coordinator_live_check import toml_table


PLUGIN = "live-validation"
SKILL = PLUGIN + ":receipt-guide"
MARKER = "PLUGIN_RECEIPT_GUIDE_V1"
REMOTE = {"short": "probe", "long": "long-remote-name-" * 8, "unicode": "搜索/文件?"}
DISABLED = "disabled"
FUNCTION = re.compile(r"[A-Za-z0-9_-]{1,64}\Z")
DISTILL_SYSTEM = ("You extract durable facts from agent conversations for long-term memory. "
    "Be conservative: most turns yield ZERO durable facts. Reply with ONLY a JSON array; never prose.")
INVALID = {
    "schedule": '[[schedules]]\ncron = "0 9 * * *"\nprompt = "fixture"\n',
    "theme": '[themes]\nname = "fixture"\n',
    "hooks": '[[hooks]]\nevents = ["SessionStart"]\ncommand = "not-started"\n',
    "missing-skill": 'skills = ["missing.md"]\n',
    "unknown-field": 'unknown_capability = true\n',
}


def sha_text(value: str) -> str:
    return hashlib.sha256(value.encode()).hexdigest()


def server_name(plugin: str = PLUGIN) -> str:
    return "plugin-" + hashlib.sha256((plugin + "\0default").encode()).hexdigest()[:24]


def wire_name(remote: str, plugin: str = PLUGIN) -> str:
    prefix = "mcp__" + server_name(plugin) + "__"
    suffix = remote if FUNCTION.fullmatch(remote) and len(prefix + remote) <= 64 else "tool-" + sha_text(remote)[:20]
    return prefix + suffix


class MCPFixture:
    """Only fixture tools are deterministic; model responses remain upstream."""
    def __init__(self) -> None:
        self.lock = threading.Lock()
        self.events: list[dict[str, Any]] = []
        self.receipts = {kind: "opaque-" + secrets.token_hex(24) for kind in REMOTE}
        self.challenges: dict[str, str] = {}
        self.server: ThreadingHTTPServer | None = None
        self.thread: threading.Thread | None = None
        self.port: int | None = None

    def snapshot(self) -> int:
        with self.lock:
            return len(self.events)

    def rows(self, start: int = 0) -> list[dict[str, Any]]:
        with self.lock:
            return [dict(row) for row in self.events[start:]]

    def handle(self, path: str, request: dict[str, Any]) -> tuple[int, Any]:
        method = request.get("method", "")
        event: dict[str, Any] = {"path": path, "method": method}
        result: dict[str, Any] = {}
        if method == "initialize":
            result = {"protocolVersion": "2024-11-05", "capabilities": {"tools": {}},
                      "serverInfo": {"name": "metis-private-plugin-fixture", "version": "1"}}
        elif method == "tools/list":
            result = {"tools": [{"name": name, "description": "Return the private opaque receipt for " + kind + " acceptance. Call once with this query's challenge.",
                "inputSchema": {"type": "object", "properties": {"challenge": {"type": "string"}},
                    "required": ["challenge"], "additionalProperties": False}}
                for kind, name in [*REMOTE.items(), ("disabled", DISABLED)]]}
        elif method == "tools/call":
            params = request.get("params", {})
            name, arguments = params.get("name"), params.get("arguments", {})
            kind = next((kind for kind, remote in REMOTE.items() if remote == name), "")
            accepted = bool(path == "/good" and kind and arguments == {"challenge": self.challenges.get(kind)} and kind in self.challenges)
            event.update({"remote_name": name, "arguments_valid": accepted, "argument_keys": sorted(arguments) if isinstance(arguments, dict) else [],
                          "challenge_sha256": sha_text(str(arguments.get("challenge", ""))) if isinstance(arguments, dict) else ""})
            if accepted:
                text = json.dumps({"kind": kind, "receipt": self.receipts[kind]}, separators=(",", ":"))
                event["result_sha256"] = sha_text(text)
                result = {"content": [{"type": "text", "text": text}]}
            else:
                result = {"isError": True, "content": [{"type": "text", "text": "Fixture rejected remote name, endpoint or query challenge"}]}
        with self.lock:
            self.events.append(event)
        if request.get("id") is None:
            return 202, None
        return 200, {"jsonrpc": "2.0", "id": request["id"], "result": result}

    def start(self) -> None:
        fixture = self
        class Handler(BaseHTTPRequestHandler):
            def log_message(self, *_args: Any) -> None:
                pass

            def do_GET(self) -> None:
                with fixture.lock:
                    fixture.events.append({"path": self.path, "method": "HTTP_GET"})
                self.send_response(200)
                self.send_header("Content-Type", "text/event-stream")
                self.send_header("Content-Length", "0")
                self.end_headers()

            def do_POST(self) -> None:
                size = int(self.headers.get("Content-Length", "0"))
                if not 0 < size <= 1024 * 1024:
                    self.send_error(400)
                    return
                try:
                    request = json.loads(self.rfile.read(size))
                    status, response = fixture.handle(self.path, request)
                except (ValueError, TypeError, AttributeError):
                    self.send_error(400)
                    return
                body = json.dumps(response).encode() if response is not None else b""
                self.send_response(status)
                self.send_header("Content-Type", "application/json")
                self.send_header("Content-Length", str(len(body)))
                self.end_headers()
                self.wfile.write(body)
        self.server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        self.server.daemon_threads = True
        self.port = self.server.server_address[1]
        self.thread = threading.Thread(target=self.server.serve_forever, daemon=True)
        self.thread.start()

    def stop(self) -> None:
        if self.server:
            self.server.shutdown()
            self.server.server_close()
        if self.thread:
            self.thread.join(timeout=5)
            require(not self.thread.is_alive(), "Owned MCP fixture thread survived cleanup")
        if self.port:
            with socket.socket() as probe:
                probe.settimeout(1)
                require(probe.connect_ex(("127.0.0.1", self.port)) != 0, "Owned MCP listener survived cleanup")


def mcp_oracle(rows: list[dict[str, Any]], kind: str, challenge: str) -> dict[str, Any]:
    counts = Counter(row["method"] for row in rows if row["path"] == "/good")
    require(counts["initialize"] >= 1 and counts["tools/list"] >= 1, "Real MCP initialize/tools/list handshake absent")
    calls = [row for row in rows if row["method"] == "tools/call"]
    require(len(calls) == 1 and calls[0].get("remote_name") == REMOTE[kind] and calls[0].get("arguments_valid") is True and
            calls[0].get("challenge_sha256") == sha_text(challenge), "Actual MCP dispatch did not call exactly the intended original remote name and arguments")
    require(all(row["path"] == "/good" for row in rows), "Invalid plugin attempted MCP startup")
    return {"initialize": counts["initialize"], "tools_list": counts["tools/list"], "tools_call": 1,
            "remote_name": REMOTE[kind], "wire_name": wire_name(REMOTE[kind]), "arguments_valid": True,
            "tool_result_sha256": calls[0]["result_sha256"], "invalid_plugin_MCP_requests": 0}


def wire_oracle(entries: list[dict[str, Any]], sid: str, provider: str, model: str, endpoint: str) -> list[dict[str, Any]]:
    requests = [row for row in entries if row.get("type") == "request" and row.get("_session_id") == sid]
    require(requests, "No physical upstream model request evidence")
    expected = {"Skill", *(wire_name(name) for name in REMOTE.values())}
    proofs = []
    business = 0
    for request in requests:
        body = request.get("body", {})
        require(request.get("method") == "POST" and request.get("url") == endpoint.rstrip("/") + "/chat/completions" and
                body.get("model") == model, "Physical model request changed the selected route or model")
        tool_names = [row.get("function", {}).get("name", "") for row in body.get("tools", [])]
        require(len(tool_names) == len(set(tool_names)) and all(FUNCTION.fullmatch(name) for name in tool_names),
                "Actual upstream tools include a duplicate or illegal function name")
        if tool_names:
            require(expected <= set(tool_names) <= expected | {"ToolSearch"},
                    "Business upstream tools include a disabled/unexpected name or omit a valid plugin tool")
            require(body.get("reasoning_effort") == "high" and body.get("max_tokens") == 8192,
                    "Business model request changed the confirmed high effort or output limit")
            role = "business_plugin_query"
            business += 1
        else:
            # Ordinary runtime performs one optional archival-memory extraction
            # with a separate fixed prompt/budget (memory/manager.go). Do not
            # mistake it for the main turn, and never accept arbitrary tool-less
            # calls as auxiliary: that would hide a broken main tool registry.
            messages = body.get("messages", [])
            require(body.get("max_tokens") == 400 and body.get("reasoning_effort") is None and len(messages) == 2 and
                    messages[0].get("role") == "system" and messages[0].get("content") == DISTILL_SYSTEM and
                    messages[1].get("role") == "user", "Unrecognized tool-less model request cannot be classified as background memory distillation")
            role = "background_memory_distillation"
        related = [row for row in entries if row.get("_session_id") == sid and row.get("reqid") == request.get("reqid")]
        responses = [row for row in related if row.get("type") == "response"]
        require(sum(row.get("type") == "request" for row in related) == 1 and len(responses) == 1 and responses[0].get("status") == 200 and not any(row.get("type") == "error" for row in related),
                "Physical upstream request lacks one successful HTTP 200 response")
        require(isinstance(request.get("request_sha256"), str) and re.fullmatch(r"[a-f0-9]{64}", request["request_sha256"]) and
                type(request.get("request_bytes")) is int and request["request_bytes"] > 0, "Physical request has no exact transport hash/byte evidence")
        require(FUNCTION.fullmatch(wire_name(DISABLED)) and wire_name(DISABLED) not in tool_names, "Disabled tool was exposed")
        proofs.append({"session_id": sid, "request_id": request.get("reqid"), "provider": provider, "model": model,
            "role": role, "reasoning_effort": body.get("reasoning_effort"), "max_tokens": body.get("max_tokens"), "status": 200, "tool_names": tool_names,
            "request_sha256": request.get("request_sha256"), "request_bytes": request.get("request_bytes"),
            "full_prompt_or_headers_retained": False})
    require(business > 0, "Background-only requests cannot establish a real business plugin query")
    return proofs


def validate_inputs(binary: Path, profile: Path) -> None:
    require(binary.is_file() and not binary.is_symlink(), "Supply a regular disposable CLI")
    protected = (Path("/Applications"), Path("/usr"), Path("/opt/homebrew"), Path.home() / ".local")
    resolved = binary.resolve()
    require(not any(root == resolved or root in resolved.parents for root in protected), "Refusing installed/protected CLI")
    require(profile.is_dir() and not profile.is_symlink() and profile.resolve() != (Path.home() / ".metis").resolve(), "Supply an isolated consumable profile")
    require((profile / "config.toml").is_file() and not (profile / "config.toml").is_symlink(), "Private profile has no regular config")


class PluginValidationLiveCheck(PersistentAgentLiveCheck):
    def __init__(self, args: argparse.Namespace) -> None:
        validate_inputs(args.binary.expanduser(), args.profile_home.expanduser())
        super().__init__(args)
        self.fixture = MCPFixture()
        self.private_logs = self.root / "runtime-logs"
        self.private_logs.mkdir(mode=0o700)
        self.sessions: list[str] = []
        self.report["boundary"] = ("Three real fixed-route high-effort CLI queries of installed Skill/MCP plugins; local preflight rejection of unsupported manifests. "
            "No mocked model, scheduler/theme/hook runtime, Desktop/native UI, production data or arbitrary third-party plugin claim.")
        self.report.update({"MCP_fixture_only": True, "model_response_forcing": False, "prompts_prescribe_workflow": True,
                            "raw_prompts_or_headers_retained": False})

    def load_route(self) -> None:
        supplied = self.args.profile_home.expanduser().resolve()
        config = tomllib.loads((supplied / "config.toml").read_text())
        route = config.get("provider", {}).get("default", "")
        selected = config.get("provider", {}).get("custom", {}).get(route, {})
        require(route == self.args.provider and selected.get("model") == self.args.model and selected.get("transport") == "openai_chat" and
                selected.get("base_url", "").rstrip("/") == self.args.endpoint.rstrip("/") and selected.get("max_tokens") == 8192,
                "Caller profile differs from the explicitly confirmed original route")
        previous = os.environ.get("METIS_HOME")
        os.environ["METIS_HOME"] = str(supplied)
        try:
            super().load_route()
        finally:
            if previous is None:
                os.environ.pop("METIS_HOME", None)
            else:
                os.environ["METIS_HOME"] = previous
        self.settings["max_tokens"] = 8192
        text = toml_table(["provider"], {"default": self.provider}) + "\n" + toml_table(["provider", "custom", self.provider], self.settings)
        text += "\n" + toml_table(["session"], {"dir": str(self.home / "sessions"), "skill_dir": str(self.home / "skills"), "max_iterations": 8})
        text += '\n[tools]\nallowed = ["Skill", "mcp__"]\n\n[permission]\nmode = "dontAsk"\n'
        # Visibility supports MCP prefixes; the permission gate uses exact
        # tool identities. Grant only the three owned fixture aliases.
        for tool in ("Skill", *(wire_name(remote) for remote in REMOTE.values())):
            text += '\n[[permission.allow]]\ntool = "' + tool + '"\n'
        (self.home / "config.toml").write_text(text)
        (self.home / "config.toml").chmod(0o600)
        self.env.update({"METIS_NO_BUNDLED_PLUGINS": "1", "METIS_DUMP_PROMPTS": "1",
            "CODEX_HOME": str(self.root / "codex-home"), "DSH_HOME": str(self.root / "dsh-home")})
        self.report["original_route"]["private_parameter_overrides"] = {"max_tokens": 8192, "max_iterations": 8, "tools": ["Skill", "mcp__"]}

    def spawn(self, arguments: list[str], label: str) -> tuple[subprocess.Popen, Path, Path]:
        # Parent helper writes logs to output; our commands and model replies
        # stay private instead. Only their exit statuses and hashes survive.
        self.index += 1
        out = self.private_logs / (f"{self.index:03d}-{label}.stdout.log")
        err = self.private_logs / (f"{self.index:03d}-{label}.stderr.log")
        handles = out.open("w"), err.open("w")
        try:
            process = subprocess.Popen([str(self.binary), *arguments], cwd=self.workspace, env=self.env,
                stdin=subprocess.DEVNULL, stdout=handles[0], stderr=handles[1], start_new_session=True)
        except BaseException:
            for handle in handles:
                handle.close()
            raise
        self.processes.append(process)
        self.handles[process.pid] = handles
        self.report["commands"].append({"label": label, "pid": process.pid, "subcommand": arguments[0], "raw_arguments_retained": False})
        self.progress(label, owned_pid=process.pid)
        return process, out, err

    def fixtures(self) -> None:
        self.fixture.start()
        market = self.home / "plugins/marketplaces/fixture"
        (market / ".claude-plugin").mkdir(parents=True, mode=0o700)
        atomic_json(self.home / "marketplaces.json", {"fixture": {"source": {"source": "github", "repo": "fixture/not-fetched"}}})
        candidates = []
        for name in (PLUGIN, *("invalid-" + kind for kind in INVALID)):
            source = market / name
            source.mkdir(mode=0o700)
            if name == PLUGIN:
                content = 'manifest_version = 1\nname = "' + PLUGIN + '"\nversion = "0.0.1"\nskills = ["SKILL.md"]\n'
                (source / "SKILL.md").write_text('---\nname: receipt-guide\ndescription: Read the private plugin receipt guide\n---\n' + MARKER + '\nCall only the user-selected receipt tool with the supplied challenge. Return its opaque receipt exactly. Do not run commands or access files.\n')
                path = "/good"
            else:
                kind = name.removeprefix("invalid-")
                content = 'name = "' + name + '"\nversion = "0.0.1"\n' + INVALID[kind]
                path = "/invalid/" + kind
            content += '\n[mcp_server]\nurl = "http://127.0.0.1:' + str(self.fixture.port) + path + '"\n'
            if name == PLUGIN:
                content += "enabled_tools = " + json.dumps([*REMOTE.values(), DISABLED], ensure_ascii=False) + '\ndisabled_tools = ["disabled"]\n'
            (source / "plugin.toml").write_text(content)
            candidates.append({"name": name, "description": "Private host acceptance fixture", "source": "./" + name})
        atomic_json(market / ".claude-plugin/marketplace.json", {"name": "fixture", "plugins": candidates})
        before = self.fixture.snapshot()
        _, command = self.command(["plugin", "install", PLUGIN + "@fixture"], "valid-plugin-install", json_output=False, operation_timeout=30)
        installed = self.home / "plugins" / PLUGIN
        require((installed / "plugin.toml").is_file() and (installed / "SKILL.md").is_file(), "Valid skills+MCP plugin was not installed")
        self.command(["plugin", "info", PLUGIN], "valid-plugin-info", json_output=False, operation_timeout=30)
        require(self.fixture.snapshot() == before, "Static plugin install/info unexpectedly started MCP")
        self.record("valid native Skill and MCP plugin installed and statically validated without starting MCP", command=command,
                    manifest_sha256=digest(installed / "plugin.toml"), skill_sha256=digest(installed / "SKILL.md"))

    def invalid_preflight(self) -> None:
        market = self.home / "plugins/marketplaces/fixture"
        reasons = {"schedule": "schedules", "theme": "themes", "hooks": "hooks are not supported",
                   "missing-skill": "missing.md", "unknown-field": "unknown_capability"}
        for kind in INVALID:
            name = "invalid-" + kind
            before = self.fixture.snapshot()
            _, command = self.command(["plugin", "install", name + "@fixture"], "reject-install-" + kind,
                expect_success=False, json_output=False, operation_timeout=30, rejection_contains=reasons[kind])
            destination = self.home / "plugins" / name
            require(not destination.exists() and not list((self.home / "plugins").glob(".install-*")), "Rejected native plugin or install staging was published")
            # Simulate a manually copied/corrupt installed plugin. Runtime must
            # reject the same declaration, before contacting its MCP endpoint.
            shutil.copytree(market / name, destination)
            self.command(["plugin", "info", name], "reject-info-" + kind,
                expect_success=False, json_output=False, operation_timeout=30, rejection_contains=reasons[kind])
            require(self.fixture.snapshot() == before, "Invalid plugin manifest triggered an MCP request during preflight")
            self.record("unsupported native plugin rejected before install commit and static MCP startup: " + kind,
                        command=command, MCP_requests=0, invalid_install_destination_published=False)

    def dumps(self) -> list[dict[str, Any]]:
        rows = []
        for path in (self.home / "dump-prompts").glob("*.jsonl"):
            require(path.is_file() and not path.is_symlink() and path.stat().st_size < 32 * 1024 * 1024, "Unsafe or oversized private wire dump")
            for line in path.read_text().splitlines():
                if line:
                    rows.append({**json.loads(line), "_session_id": path.stem})
        return rows

    def query(self, kind: str, number: int) -> None:
        sid = str(uuid.uuid4())
        self.sessions.append(sid)
        challenge = "query-" + secrets.token_hex(12)
        self.fixture.challenges[kind] = challenge
        before, mcp_before = self.trace_snapshot(), self.fixture.snapshot()
        prompt = ("This is a finite private plugin acceptance. " +
            ("First invoke the installed plugin Skill '" + SKILL + "'. " if kind == "short" else "") +
            "Then call exactly once the plugin MCP tool whose description identifies the '" + kind +
            "' receipt acceptance, with argument challenge='" + challenge + "'. " +
            "Use only that receipt tool" + (" and Skill" if kind == "short" else "") +
            ". Do not access other files, execute commands, install anything or call disabled tools. " +
            "Finish by replying only the opaque receipt returned by the tool; obtain it from the actual tool result.")
        _, command = self.command(["run", "--no-auth-wizard", "--mode", "dontAsk", "--effort", "high",
            "--tools", "Skill,mcp__", "--provider", self.provider, "--model", self.model,
            "--max-iter", "8", "--session-id", sid, prompt], f"real-query-{number}-{kind}", json_output=False)
        events = self.trace_events(before)
        proof = trace_proof(events, self.provider, self.model)
        # Retain only scalar usage/tool identities even if the subsequent host
        # oracle fails. Full messages, input arguments and returned values stay
        # private and are destroyed before any retry or code edit.
        atomic_json(self.output / (f"real-query-{number}-{kind}-trace-projection.json"),
            {**proof, "errors": [{key: error.get(key) for key in ("kind", "tool")} for error in proof["errors"]]})
        require(proof["session_ids"] == [sid] and proof["model_responses"] > 0 and not proof["errors"] and
            any(sum(row.get(field, 0) for field in ("input", "output", "cache_write", "cache_read")) > 0 for row in proof["usage"]),
            "Actual provider token receipt missing or tool/provider error occurred")
        target = wire_name(REMOTE[kind])
        require(proof["tool_results"].get(target) == 1 and set(proof["tool_results"]) <= ({target, "Skill", "ToolSearch"} if kind == "short" else {target, "ToolSearch"}),
                "Real model skipped/repeated the selected plugin tool or executed an unexpected tool")
        if kind == "short":
            skill = assert_skill_invoked(events, SKILL, MARKER)
            self.record("real upstream model invoked the installed namespaced plugin Skill", matching_invocations=len(skill["matching_invokes"]))
        histories = history_snapshots(self.home / "sessions" / (sid + ".jsonl"))
        require(histories and final_assistant(histories[-1]).strip() == self.fixture.receipts[kind],
                "Final assistant reply does not match the opaque receipt known only to the MCP service")
        deadline = time.monotonic() + 5
        while True:
            try:
                wire = wire_oracle(self.dumps(), sid, self.provider, self.model, self.settings["base_url"])
                break
            except AssertionError:
                if time.monotonic() >= deadline:
                    raise
                time.sleep(0.1)  # Wait only for asynchronous dump flush, never retry a model query.
        mcp = mcp_oracle(self.fixture.rows(mcp_before), kind, challenge)
        proof["errors"] = []
        safe = {"session_id": sid, "kind": kind, "command": command, "usage": proof, "wire": wire, "MCP": mcp,
                "receipt_sha256": sha_text(self.fixture.receipts[kind]), "receipt_matched_host_oracle": True,
                "raw_transcript_retained": False}
        atomic_json(self.output / (f"real-query-{number}-{kind}-proof.json"), safe)
        self.record("real upstream plugin tool dispatch and host receipt verified: " + kind,
                    session_id=sid, physical_HTTP_200=len(wire), business_HTTP_200=sum(row["role"] == "business_plugin_query" for row in wire),
                    background_memory_HTTP_200=sum(row["role"] == "background_memory_distillation" for row in wire),
                    wire_tool=target, actual_remote_tool=REMOTE[kind])

    def cleanup(self) -> None:
        failures = []
        try:
            super().cleanup()
        except BaseException as error:
            failures.append(self.redact(str(error)))
        try:
            self.fixture.stop()
        except BaseException as error:
            failures.append(str(error))
        supplied = self.args.profile_home.expanduser().resolve()
        shutil.rmtree(supplied / ".credentials", ignore_errors=True)
        self.report.setdefault("cleanup", {}).update({"MCP_listener_stopped": not self.fixture.thread or not self.fixture.thread.is_alive(),
            "supplied_profile_credentials_removed": not (supplied / ".credentials").exists()})
        require(not failures, "Cleanup failures: " + "; ".join(failures))

    def copy_evidence(self) -> None:
        # No raw logs, transcripts, MCP bodies, full prompts or headers leave
        # the private directory, including on a failed run.
        wire = []
        for row in self.dumps():
            safe = {key: row.get(key) for key in ("type", "_session_id", "reqid", "status", "request_sha256", "request_bytes")}
            if row.get("type") == "request" and isinstance(row.get("body"), dict):
                body = row["body"]
                safe["parameters"] = {key: body.get(key) for key in ("model", "reasoning_effort", "max_tokens")}
                safe["tool_names"] = [entry.get("function", {}).get("name") for entry in body.get("tools", [])]
            wire.append(safe)
        atomic_json(self.output / "safe-physical-wire-evidence.json", wire)
        atomic_json(self.output / "safe-MCP-events.json", self.fixture.rows())
        atomic_json(self.output / "private-artifact-hashes.json", [{"kind": kind, "name": path.name, "sha256": digest(path), "raw_file_retained": False}
            for kind, paths in (("wire", (self.home / "dump-prompts").glob("*.jsonl")),
                                ("transcript", (self.home / "sessions").glob("*.jsonl")),
                                ("runtime-log", self.private_logs.glob("*.log"))) for path in paths if path.is_file()])

    def run(self) -> int:
        signal.setitimer(signal.ITIMER_REAL, self.args.case_timeout)
        try:
            self.stage()
            self.load_route()
            self.fixtures()
            self.query("short", 1)
            self.query("long", 2)
            self.invalid_preflight()
            self.query("unicode", 3)
            require(all(row["path"] == "/good" for row in self.fixture.rows()), "Invalid plugin contacted MCP at runtime")
            self.record("invalid installed plugins skipped before MCP startup while valid plugin remains callable", invalid_plugins=len(INVALID), MCP_invalid_requests=0)
            self.report["passed"] = True
        except BaseException as error:
            self.report.update({"failure_type": type(error).__name__, "failure": self.redact(str(error))})
        finally:
            signal.setitimer(signal.ITIMER_REAL, 0)
            for label, action in (("cleanup", self.cleanup), ("safe_evidence", self.copy_evidence), ("source_freeze", self.verify_freeze)):
                try:
                    action()
                except BaseException as error:
                    self.report["passed"] = False
                    self.report[label + "_failure"] = self.redact(str(error))
            shutil.rmtree(self.args.profile_home.expanduser().resolve(), ignore_errors=True)
            shutil.rmtree(self.root, ignore_errors=True)
            self.report.setdefault("cleanup", {}).update({"temporary_workspace_destroyed": not self.root.exists(),
                "supplied_private_profile_destroyed": not self.args.profile_home.expanduser().exists(),
                "raw_wire_and_prompts_destroyed": not self.root.exists()})
            self.report.update({"finished_utc": utc_now(), "elapsed_seconds": round(time.monotonic() - self.started, 3)})
            atomic_json(self.output / "final-verification.json", json.loads(self.redact(json.dumps(self.report))))
            self.fixture.receipts.clear()
            self.secrets.clear()
        print(json.dumps({"passed": self.report["passed"], "evidence": str(self.output), "cleanup": self.report.get("cleanup")}), flush=True)
        return 0 if self.report["passed"] else 1


def self_test() -> int:
    fixture = MCPFixture()
    fixture.challenges["unicode"] = "offline-challenge"
    fixture.handle("/good", {"id": 1, "method": "initialize"})
    fixture.handle("/good", {"id": 2, "method": "tools/list"})
    status, response = fixture.handle("/good", {"id": 3, "method": "tools/call", "params": {
        "name": REMOTE["unicode"], "arguments": {"challenge": "offline-challenge"}}})
    require(status == 200 and fixture.receipts["unicode"] in json.dumps(response), "Offline fixture receipt missing")
    mcp_oracle(fixture.rows(), "unicode", "offline-challenge")
    tools = [{"type": "function", "function": {"name": name}} for name in ["Skill", *(wire_name(name) for name in REMOTE.values())]]
    requests = [{"type": "request", "_session_id": "offline", "reqid": "offline-request", "method": "POST", "url": "http://127.0.0.1:8317/v1/chat/completions",
        "request_sha256": "a" * 64, "request_bytes": 1000,
        "body": {"model": "gpt-6.1-sol", "reasoning_effort": "high", "max_tokens": 8192, "tools": tools,
                 "messages": [{"role": "system", "content": "offline business query"}, {"role": "user", "content": "offline"}]}},
        {"type": "response", "_session_id": "offline", "reqid": "offline-request", "status": 200}]
    wire_oracle(requests, "offline", "easycliproxyapi", "gpt-6.1-sol", "http://127.0.0.1:8317/v1")
    background = json.loads(json.dumps(requests))
    for entry in background:
        entry["reqid"] = "offline-background"
    background[0]["body"] = {"model": "gpt-6.1-sol", "max_tokens": 400,
        "messages": [{"role": "system", "content": DISTILL_SYSTEM}, {"role": "user", "content": "offline distilled exchange"}]}
    with_search = json.loads(json.dumps(requests))
    with_search[0]["body"]["tools"].append({"type": "function", "function": {"name": "ToolSearch"}})
    classified = wire_oracle(with_search + background, "offline", "easycliproxyapi", "gpt-6.1-sol", "http://127.0.0.1:8317/v1")
    require([row["role"] for row in classified] == ["business_plugin_query", "background_memory_distillation"],
            "Offline wire classifier mixed business and memory extraction calls")
    rejections = 0
    for mutation in ("disabled", "bad-name", "wrong-model", "wrong-effort", "bad-status", "removed-tools",
                     "only-background", "low-effort-plus-background", "fake-title", "background-wrong-model"):
        copied = json.loads(json.dumps(requests))
        if mutation == "disabled":
            copied[0]["body"]["tools"].append({"type": "function", "function": {"name": wire_name(DISABLED)}})
        elif mutation == "bad-name":
            copied[0]["body"]["tools"][1]["function"]["name"] = REMOTE["unicode"]
        elif mutation == "wrong-model":
            copied[0]["body"]["model"] = "other"
        elif mutation == "wrong-effort":
            copied[0]["body"]["reasoning_effort"] = "low"
        elif mutation == "bad-status":
            copied[1]["status"] = 401
        elif mutation == "removed-tools":
            copied[0]["body"].pop("tools")
            copied += json.loads(json.dumps(background))
        elif mutation == "only-background":
            copied = json.loads(json.dumps(background))
        elif mutation == "low-effort-plus-background":
            copied[0]["body"]["reasoning_effort"] = "low"
            copied += json.loads(json.dumps(background))
        elif mutation == "fake-title":
            extra = json.loads(json.dumps(background))
            extra[0]["body"]["messages"][0]["content"] = "Generate a title"
            copied += extra
        elif mutation == "background-wrong-model":
            extra = json.loads(json.dumps(background))
            extra[0]["body"]["model"] = "other"
            copied += extra
        try:
            wire_oracle(copied, "offline", "easycliproxyapi", "gpt-6.1-sol", "http://127.0.0.1:8317/v1")
        except AssertionError:
            rejections += 1
    require(rejections == 10 and all(FUNCTION.fullmatch(wire_name(name)) for name in REMOTE.values()), "Offline wire oracle accepted a mutated failure")
    fixture.handle("/invalid/hooks", {"id": 4, "method": "initialize"})
    try:
        mcp_oracle(fixture.rows(), "unicode", "offline-challenge")
    except AssertionError:
        rejections += 1
    require(rejections == 11, "Offline MCP oracle accepted invalid plugin startup")
    print(json.dumps({"self_test": "passed", "negative_oracles": rejections, "network": False, "auth": False,
        "model": False, "product_CLI": False, "bound_listener": False}))
    return 0


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--self-test", action="store_true")
    parser.add_argument("--run-live", action="store_true")
    parser.add_argument("--repo", type=Path, default=Path(__file__).resolve().parents[2])
    parser.add_argument("--binary", type=Path)
    parser.add_argument("--expected-sha256")
    parser.add_argument("--source-freeze", type=Path)
    parser.add_argument("--profile-home", type=Path)
    parser.add_argument("--output", type=Path)
    parser.add_argument("--destroy-input", action="store_true")
    parser.add_argument("--provider", default="easycliproxyapi")
    parser.add_argument("--model", default="gpt-6.1-sol")
    parser.add_argument("--endpoint", default="http://127.0.0.1:8317/v1")
    parser.add_argument("--query-timeout", type=float, default=360)
    parser.add_argument("--case-timeout", type=float, default=1800)
    args = parser.parse_args()
    if args.self_test:
        require(not args.run_live, "Choose offline self-test or live mode")
        return self_test()
    require(args.run_live and args.destroy_input and all(getattr(args, key) for key in
        ("binary", "expected_sha256", "source_freeze", "profile_home", "output")), "Live mode requires explicit disposable CLI/profile/freeze/hash/output/destruction inputs")
    require(re.fullmatch(r"[a-f0-9]{64}", args.expected_sha256), "Expected CLI SHA-256 invalid")
    require(0 < args.query_timeout <= 600 and 0 < args.case_timeout <= 3600, "Live timeouts must be bounded")
    os.umask(0o077)
    def interrupted(signum: int, _frame: Any) -> None:
        raise KeyboardInterrupt("Plugin acceptance interrupted by " + signal.Signals(signum).name)
    def timed_out(_signum: int, _frame: Any) -> None:
        raise TimeoutError("Plugin acceptance exceeded its bounded deadline")
    signal.signal(signal.SIGINT, interrupted)
    signal.signal(signal.SIGTERM, interrupted)
    signal.signal(signal.SIGALRM, timed_out)
    return PluginValidationLiveCheck(args).run()


if __name__ == "__main__":
    raise SystemExit(main())
