#!/usr/bin/env python3
"""Real original-route context/tool/task regression acceptance.

Accept a frozen disposable CLI; never build, install, retry a model turn or
touch real sessions. Three private sessions cover visible Read pages and Write
guards, main-step/execution-task grouping, and stable single-item Todo history.
Only sanitized aggregate evidence survives. A bounded hold exposes the private
WebUI for an independent browser check; it is not native Desktop acceptance.
"""
from __future__ import annotations

import argparse
from collections import Counter
import hashlib
import json
import os
from pathlib import Path
import re
import secrets
import shutil
import signal
import sys
import time
from typing import Any
from urllib.parse import quote

sys.dont_write_bytecode = True
from image_context_pressure_live_check import TARGET_MODEL, TARGET_PROVIDER, catalog_proof, digest, load_selected_route
from project_coordinator_live_check import atomic_json, require, safe_endpoint, toml_table, utc_now
from trace_presentation_live_check import TracePresentationLiveCheck, stable_digest

PROFILE = "context-integrity-probe"
KEY_ENV = "METIS_CONTEXT_INTEGRITY_ROUTE_KEY"
TOOLS = ["Read", "Write", "TodoWrite", "TodoRead", "TaskCreate", "TaskUpdate", "TaskList"]
PARTIAL_ERROR = "file was Read with offset/limit (partial view)"


def disposable_binary(path: Path) -> Path:
    raw = path.expanduser().absolute()
    require(raw.is_file() and not raw.is_symlink(), "Supply a regular disposable CLI, not a symlink")
    resolved = raw.resolve()
    for installed in (Path("/Applications"), Path("/System/Applications"), Path.home() / "Applications",
                      Path.home() / ".local/share/metis/versions"):
        require(not resolved.is_relative_to(installed), "Refusing an installed CLI/application path")
    installed_cli = shutil.which("metis")
    require(not installed_cli or Path(installed_cli).resolve() != resolved, "Refusing the installed CLI")
    return resolved


def active_history(rows: list[dict[str, Any]]) -> list[dict[str, Any]]:
    history: list[dict[str, Any]] = []
    for row in rows:
        if row.get("type") == "history_replace":
            history = list(row.get("messages", []))
        elif row.get("type") == "message":
            history.append(row["message"])
    return history


def result_text(block: dict[str, Any]) -> str:
    content = block.get("content", "")
    if isinstance(content, str):
        return content
    if isinstance(content, list):
        return "\n".join(part.get("text", "") for part in content if isinstance(part, dict))
    return ""


def history_calls(history: list[dict[str, Any]]) -> list[dict[str, Any]]:
    calls: list[dict[str, Any]] = []
    pending: dict[str, dict[str, Any]] = {}
    for index, message in enumerate(history):
        for block in message.get("content", []):
            if not isinstance(block, dict):
                continue
            identity = block.get("trace_call_id") or block.get("id") or block.get("tool_use_id")
            if block.get("type") == "tool_use":
                call = {"tool": block.get("name"), "input": block.get("input", {}), "call_index": index,
                        "call_id": identity, "result": None, "is_error": False}
                calls.append(call)
                pending[identity] = call
                # Legacy blocks may provide trace identity on only one side.
                if block.get("id"):
                    pending[block["id"]] = call
            elif block.get("type") == "tool_result":
                call = pending.get(identity) or pending.get(block.get("tool_use_id"))
                require(call is not None, "Durable tool result has no preceding exact call")
                require(call["result"] is None, "Durable tool call has duplicate results")
                call.update({"result": result_text(block), "is_error": bool(block.get("is_error")), "result_index": index})
    require(all(call["result"] is not None for call in calls), "Durable tool call has no result")
    return calls


def read_write_oracle(history: list[dict[str, Any]], target: Path, control: Path,
                      tail_canary: str, original_sha: str) -> dict[str, Any]:
    calls = history_calls(history)
    require(calls and all(call["tool"] in ("Read", "Write") for call in calls), "Read case used an unexpected tool")
    for call in calls:
        require(call["input"].get("path", call["input"].get("file_path")) in (str(target), str(control)),
                "Read/Write case accessed a non-fixture path")
    reads = [call for call in calls if call["tool"] == "Read" and call["input"].get("path", call["input"].get("file_path")) == str(target)]
    writes = [call for call in calls if call["tool"] == "Write" and call["input"].get("path", call["input"].get("file_path")) == str(target)]
    require(len(reads) >= 2 and len(writes) == 1, "Read case did not exercise initial/tail pages and one guard Write")
    first, guard = reads[0], writes[0]
    require(not first["is_error"] and 0 < len(first["result"].encode()) <= 16_384,
            "First real Read was not a bounded successful page")
    require("Full output saved to:" not in first["result"] and tail_canary not in first["result"],
            "First Read still spilled or exposed the deliberately distant tail")
    require(first["result_index"] < guard["call_index"], "Guard Write was emitted before the model received its Read result")
    require(guard["is_error"] and PARTIAL_ERROR in guard["result"], "Whole-file Write after partial visible Read was not rejected by the partial-view guard")
    require(digest(target) == original_sha, "Rejected guard Write still changed the physical long file")
    tail_reads = [call for call in reads[1:] if type(call["input"].get("byte_offset")) is int and call["input"]["byte_offset"] > 0]
    require(tail_reads and any(not call["is_error"] and tail_canary in call["result"] for call in tail_reads),
            "Native source-byte pagination did not reach the hidden tail")
    require(all("Full output saved to:" not in call["result"] for call in reads), "A native Read page was recursively spilled")
    control_reads = [call for call in calls if call["tool"] == "Read" and call["input"].get("path", call["input"].get("file_path")) == str(control)]
    control_writes = [call for call in calls if call["tool"] == "Write" and call["input"].get("path", call["input"].get("file_path")) == str(control)]
    require(control_reads and len(control_writes) == 1, "Normal small-file Read/Write control was not exercised")
    write = control_writes[0]
    require(not write["is_error"] and any(not call["is_error"] and call["result_index"] < write["call_index"] for call in control_reads),
            "Normal complete small-file Read did not authorize a later Write")
    require(control.read_bytes() == write["input"]["content"].encode(), "Successful small-file Write differs from actual disk bytes")
    errors = [call for call in calls if call["is_error"]]
    require(errors == [guard], "Read/Write case contains an error other than the expected guard rejection")
    return {"tool_calls": len(calls), "bounded_first_page_bytes": len(first["result"].encode()),
            "hidden_tail_reached": True, "whole_write_rejected": True, "long_file_unchanged": True,
            "small_file_write_verified": True, "expected_tool_errors": 1,
            "long_file_sha256": original_sha, "small_file_sha256": digest(control),
            "first_read_result_sha256": hashlib.sha256(first["result"].encode()).hexdigest()}


def plan_oracle(status: dict[str, Any], main_steps: list[str], execution: str) -> dict[str, Any]:
    steps, extra, canonical = status.get("planSteps"), status.get("executionItems"), status.get("planItems")
    require(all(isinstance(value, list) for value in (steps, extra, canonical)), "Status lacks main-step/execution/canonical arrays")
    require([row.get("content") for row in steps] == main_steps, "Main plan steps changed or include an execution task")
    require(len(extra) == 1 and extra[0].get("content") == execution, "Execution task was lost, duplicated or flattened into main steps")
    require(len(canonical) == 4 and {row.get("content") for row in canonical} == set(main_steps + [execution]),
            "Business canonical plan no longer contains all four real records")
    require(all(row.get("status") == "completed" for row in [*steps, *extra, *canonical]), "Completed task fixture has unfinished records")
    return {"main_steps": 3, "execution_items": 1, "canonical_items": 4,
            "main_contents": main_steps, "execution_content": execution, "projection_sha256": stable_digest([steps, extra, canonical])}


def single_todo_event(history: list[dict[str, Any]], content: str) -> dict[str, Any]:
    matches = [call for call in history_calls(history) if call["tool"] == "TodoWrite" and
               call["input"].get("content") == content and "todos" not in call["input"]]
    require(len(matches) == 1 and not matches[0]["is_error"], "Expected one real single-item TodoWrite event")
    call = matches[0]
    require(call["input"].get("status") == "completed" and content in call["result"], "Single-item Todo event lost completed content")
    return {"input": call["input"], "result": call["result"], "call_id": call["call_id"]}


class ContextIntegrityLiveCheck(TracePresentationLiveCheck):
    def __init__(self, args: argparse.Namespace) -> None:
        disposable_binary(args.binary)
        super().__init__(args)
        self.session_ids: dict[str, str] = {}
        self.report.update({"mode": "context-integrity-live", "checks": [], "model_turn_submissions": 0,
                            "boundary": "Original-route real WebUI/CLI queries and durable model-visible result checks. Native pixels, provider-ID collision, summary revocation and Responses-only metering require independent acceptance."})

    def progress(self, phase: str) -> None:
        value = {"phase": phase, "utc": utc_now(), "model_turn_submissions": self.report["model_turn_submissions"],
                 "sessionIds": self.session_ids}
        if hasattr(self, "port"):
            value.update({"url": "http://127.0.0.1:" + str(self.port), "port": self.port})
        atomic_json(self.output / "progress.json", value)
        print(json.dumps({"phase": phase}), flush=True)

    def verify_freeze(self) -> None:
        raw = self.args.source_freeze.read_bytes()
        frozen = json.loads(raw)
        require(isinstance(frozen.get("files"), dict) and frozen["files"], "Source freeze lacks file digests")
        repo = self.args.repo.resolve()
        for name, sha in frozen["files"].items():
            path = repo / name
            require(not Path(name).is_absolute() and path.resolve().is_relative_to(repo), "Source freeze path leaves checkout")
            require(path.is_file() and digest(path) == sha, "Source changed after the frozen build")
        sha = hashlib.sha256(raw).hexdigest()
        require(not hasattr(self, "freeze_sha") or sha == self.freeze_sha, "Source freeze manifest changed during acceptance")
        self.freeze_sha = sha
        self.report["source_freeze"] = {"sha256": sha, "files_checked": len(frozen["files"])}

    def prepare(self) -> None:
        self.verify_freeze()
        require(self.input_sha == self.args.expected_sha256, "Supplied CLI differs from frozen build digest")
        shutil.copyfile(self.source_binary, self.binary)
        self.binary.chmod(0o700)
        source_home = self.args.source_home.expanduser().resolve()
        route = load_selected_route(source_home, dict(os.environ))
        self.source_config, self.source_config_sha = source_home / "config.toml", route["config_sha256"]
        self.source_auth = source_home / ".credentials/auth.json"
        self.source_auth_sha = digest(self.source_auth) if self.source_auth.is_file() else None
        self.key = route["key"]
        user_home = self.root / "user-home"
        for path in (self.home, self.workspace, user_home, self.home / "cache", self.home / "agents"):
            path.mkdir(mode=0o700)
        catalog, proof = catalog_proof(source_home / "cache/models.json")
        (self.home / "cache/models.json").write_bytes(catalog)
        settings = dict(route["settings"])
        settings.pop("api_key", None)
        settings.update({"api_key_env": KEY_ENV, "model": TARGET_MODEL, "max_tokens": self.args.max_output_tokens})
        config = toml_table(["provider"], {"default": TARGET_PROVIDER}) + "\n" + toml_table(["provider", "custom", TARGET_PROVIDER], settings)
        config += "\n" + toml_table(["session"], {"dir": str(self.home / "sessions"), "skill_dir": str(self.home / "skills"),
            "max_iterations": 16, "auto_compact_threshold": route["threshold"], "auto_compact_minimum_tokens": 800000})
        config += "\n" + toml_table(["tools"], {"allowed": TOOLS}) + '\n[permission]\nmode = "fullAccess"\n'
        (self.home / "config.toml").write_text(config)
        (self.home / "config.toml").chmod(0o600)
        (self.home / "agents" / (PROFILE + ".md")).write_text("---\nname: " + PROFILE + "\nmodel: " + TARGET_MODEL +
            "\neffort: high\ntools: [" + ", ".join(TOOLS) + "]\npermission_mode: fullAccess\nmax_turns: 16\nomit_claude_md: true\n---\n"
            "You execute an authorized disposable fixture acceptance. Obey only the explicit current user test steps. "
            "Issue each tool sequentially, waiting for its actual result before the next call; never parallelize these steps. "
            "An intentionally rejected partial-file Write is an expected negative test, not a reason to bypass the guard. "
            "Never read other files, change configuration, delegate, use shell, recover archived tasks or add unrequested tasks. "
            "Return the exact completion marker and any explicitly requested hidden canary; never claim a tool succeeded if its receipt rejected it.\n")
        atomic_json(self.home / "desktop-preferences.json", {"defaultPreset": PROFILE, "rootTurnParallelism": 1,
                    "totalAgentParallelism": 1, "subagentParallelism": 1})
        self.environment = {name: os.environ[name] for name in ("PATH", "SHELL", "LANG", "LC_ALL") if name in os.environ}
        self.environment.update({"HOME": str(user_home), "METIS_HOME": str(self.home), "TMPDIR": str(self.root),
            "XDG_CACHE_HOME": str(self.root / "cache"), KEY_ENV: self.key, "METIS_AUTO_MEMORY": "0",
            "METIS_LEARNING_EVALUATION": "1", "METIS_NO_UPDATE_CHECK": "1", "METIS_RUN_CACHE": "0",
            "METIS_RUN_MAX_SECONDS": str(self.args.query_timeout), "METIS_TURN_MAX_SECONDS": str(self.args.query_timeout),
            "METIS_DESKTOP_MAX_PARALLEL_TURNS": "1", "NO_PROXY": "127.0.0.1,localhost"})
        self.report.update({"binary_sha256": self.input_sha, "catalog_reference": proof,
            "route": {"provider": TARGET_PROVIDER, "model": TARGET_MODEL, "effort": "high", "transport": route["binding"][1],
                      "endpoint": safe_endpoint(settings["base_url"]), "route_changed": False},
            "startup": {"private_home_and_profile_only": True, "tools": TOOLS, "native_desktop_launched": False}})
        self.progress("private original-route runtime prepared")

    def new_session(self, label: str) -> str:
        code, body, _ = self.http("POST", "/api/sessions", {})
        sid = body.get("id") if isinstance(body, dict) else None
        require(code == 201 and isinstance(sid, str) and re.fullmatch(r"[A-Za-z0-9_.-]+", sid), "Private session creation failed")
        self.session_ids[label] = sid
        return sid

    def rows(self, sid: str) -> list[dict[str, Any]]:
        path = self.home / "sessions" / (sid + ".jsonl")
        require(path.is_file(), "Private durable session is absent")
        return [json.loads(line) for line in path.read_text().splitlines() if line]

    def traces(self, sid: str) -> list[dict[str, Any]]:
        return [row for path in (self.home / "traces").glob("*.jsonl") for row in
                (json.loads(line) for line in path.read_text().splitlines() if line) if row.get("session_id") == sid]

    def turn(self, label: str, sid: str, prompt: str, marker: str) -> str:
        before = len(self.traces(sid))
        self.report["model_turn_submissions"] += 1
        started = time.monotonic()
        remaining = min(self.args.query_timeout, getattr(self, "case_deadline", float("inf")) - time.monotonic())
        require(remaining > 0, "Private real case exceeded its finite time budget")
        code, result, _ = self.http("POST", "/api/turns", {"sessionId": sid, "input": prompt}, timeout=remaining)
        require(code == 200 and isinstance(result, dict) and result.get("sessionId") == sid and not result.get("stopped"),
                "Private real turn failed or changed session identity")
        text = result.get("text", "")
        require(isinstance(text, str) and marker in text, "Real turn did not return its exact bounded completion marker")
        events = self.traces(sid)[before:]
        require(events and not any(row.get("kind") == "error" for row in events), "Real turn recorded an agent/provider error")
        usage: Counter[str] = Counter()
        tokens = [row for row in events if row.get("kind") == "tokens"]
        for row in tokens:
            for field, number in re.findall(r"(input|output|cache_write|cache_read)=(\d+)", row.get("text", "")):
                usage[field] += int(number)
        require(tokens and sum(usage.values()) > 0, "Private turn has no real provider usage receipts")
        require(any(row.get("kind") == "loop_done" and row.get("text") == "end_turn" for row in events), "Real turn did not end naturally")
        self.report["checks"].append({"label": label, "session_id": sid, "http_status": code,
            "seconds": round(time.monotonic() - started, 3), "real_provider_responses": len(tokens), "usage": dict(usage),
            "response_sha256": hashlib.sha256(text.encode()).hexdigest(), "event_type_counts": dict(Counter(row.get("kind") for row in events))})
        self.progress(label + " real turn received")
        return text

    def status(self, sid: str) -> dict[str, Any]:
        # Plan status is deliberately scoped to the selected conversation.
        # A sessionId query parameter selects context/worker diagnostics but
        # must not silently activate another user's conversation.
        code, _, _ = self.http("POST", "/api/sessions/activate", {"id": sid})
        require(code == 200, "Private status session selection failed")
        code, body, _ = self.http("GET", "/api/status?sessionId=" + quote(sid))
        require(code == 200 and isinstance(body, dict) and body.get("activeSessionId") == sid,
                "Private session status failed or belongs to another selected conversation")
        return body

    def run_case(self, label: str, case: Any) -> None:
        started = time.monotonic()
        self.case_deadline = started + self.args.query_timeout
        case()
        require(time.monotonic() <= self.case_deadline, "Private real case exceeded its finite time budget")
        self.report.setdefault("case_durations_seconds", {})[label] = round(time.monotonic() - started, 3)
        self.case_deadline = float("inf")

    def case_read(self) -> None:
        sid = self.new_session("read_visibility")
        target, control = self.workspace / "long-single-line.txt", self.workspace / "small-control.txt"
        self.tail_canary = "HIDDEN_TAIL_" + secrets.token_hex(20)
        raw = b"LONG_PAGE_BEGIN_" + b"Q" * (60000 - 16 - len(self.tail_canary)) + self.tail_canary.encode()
        require(len(raw) == 60000, "Synthetic long file has wrong size")
        target.write_bytes(raw)
        control.write_text("FIRST_CONTROL\nSECOND_CONTROL\nLAST_CONTROL\n")
        original = digest(target)
        marker = "READ_VISIBILITY_DONE_" + secrets.token_hex(8)
        prompt = ("This is an authorized disposable Read visibility negative test. Call each tool separately and wait for its result. "
            f"1. Read(path={str(target)!r},offset=1,limit=1). "
            f"2. After receiving that result, deliberately attempt Write(path={str(target)!r},content='GUARD_MUST_REJECT'). "
            "This partial-read overwrite must be rejected by the host; do not skip the attempt, repeat it, read the whole file first or bypass it. "
            f"3. Read(path={str(target)!r},byte_offset=59872,byte_limit=128). "
            "4. Return the complete HIDDEN_TAIL_ canary actually observed in this last page. "
            f"5. Read(path={str(control)!r}) with default parameters. After its result, Write(path={str(control)!r},content='SMALL_CONTROL_WRITTEN'). "
            "Only these two disposable file paths are authorized; use no other tool. End with " + marker)
        text = self.turn("read-visible-pages-and-write-guard", sid, prompt, marker)
        require(self.tail_canary in text, "Real model did not observe the hidden tail through the native byte page")
        proof = read_write_oracle(active_history(self.rows(sid)), target, control, self.tail_canary, original)
        self.report["read_visibility"] = proof
        self.progress("Read page progress, expected guard rejection and physical hashes verified")

    def case_plan(self) -> None:
        sid = self.new_session("task_grouping")
        self.main_steps = ["核对资料边界", "整理主计划方案", "复核主计划结论"]
        self.execution = "执行独立资料交叉检查"
        marker = "TASK_GROUPS_DONE_" + secrets.token_hex(8)
        prompt = ("This is a disposable task presentation test. Use only these three sequential tool calls: "
            "1. TodoWrite with new_plan=true and complete todos array " + json.dumps([{"content": content, "status": "completed"} for content in self.main_steps], ensure_ascii=False) + ". "
            "2. TaskCreate with subject=" + json.dumps(self.execution, ensure_ascii=False) + ",description='Synthetic independent execution work already checked in this test',owner='fixture-worker'. "
            "3. TaskUpdate the returned task ID with status='completed'. This extra Task is intentionally separate execution work, not a fourth main step. "
            "Do not rename, add, restore or delete any other task. End with " + marker)
        self.turn("main-steps-and-execution-task", sid, prompt, marker)
        calls = history_calls(active_history(self.rows(sid)))
        require([call["tool"] for call in calls] == ["TodoWrite", "TaskCreate", "TaskUpdate"] and not any(call["is_error"] for call in calls),
                "Task grouping turn did not execute exactly the required successful tool calls")
        self.report["task_grouping"] = plan_oracle(self.status(sid), self.main_steps, self.execution)
        self.progress("Three main steps and one separate execution task verified")

    def case_history(self) -> None:
        sid = self.new_session("single_todo_history")
        self.alpha, self.beta = "ALPHA 历史单项卡片", "BETA 当前单项卡片"
        for name, content in (("alpha", self.alpha), ("beta", self.beta)):
            marker = "SINGLE_TODO_" + name.upper() + "_" + secrets.token_hex(8)
            prompt = ("This is a new human task for historical-card isolation. Call TodoWrite exactly once using the single-item form "
                + json.dumps({"content": content, "status": "completed"}, ensure_ascii=False)
                + ". Do not use a todos array, include another task, restore an archived plan or invoke another tool. End with " + marker)
            self.turn("single-todo-" + name, sid, prompt, marker)
            history = active_history(self.rows(sid))
            event = single_todo_event(history, self.alpha)
            if name == "alpha":
                self.alpha_event_sha = stable_digest(event)
            else:
                require(stable_digest(event) == self.alpha_event_sha, "BETA rewrote the durable ALPHA historical event")
                single_todo_event(history, self.beta)
        status = self.status(sid)
        require([row.get("content") for row in status.get("planSteps", [])] == [self.beta] and
                [row.get("content") for row in status.get("planItems", [])] == [self.beta], "Current BETA plan includes archived ALPHA")
        self.report["single_todo_history"] = {"alpha_original_event_sha256": self.alpha_event_sha,
            "beta_did_not_rewrite_alpha_event": True, "current_plan_only_beta": True,
            "ui_expected_alpha_summary": "已完成 · " + self.alpha,
            "ui_pixel_acceptance": "requires independent browser expansion/reload/resize; not claimed by this API check"}
        self.progress("Original single-item ALPHA event preserved independently of current BETA")

    def restart_and_verify(self) -> None:
        self.stop_server()
        self.start_server("restarted")
        require(plan_oracle(self.status(self.session_ids["task_grouping"]), self.main_steps, self.execution) == self.report["task_grouping"],
                "Server restart changed the main/execution presentation projection")
        sid = self.session_ids["single_todo_history"]
        require(stable_digest(single_todo_event(active_history(self.rows(sid)), self.alpha)) == self.alpha_event_sha,
                "Server restart changed the original single TodoWrite event")
        require([row.get("content") for row in self.status(sid).get("planSteps", [])] == [self.beta], "Restart restored the wrong current task plan")
        code, _, _ = self.http("POST", "/api/sessions/activate", {"id": sid})
        require(code == 200, "Private historical session activation failed")
        self.session_id = sid
        self.report["restart_preserved_task_projections_and_original_event"] = True
        self.progress("Private server restart and historical task restoration verified")

    def hold(self) -> None:
        if not self.args.hold_seconds:
            return
        stop = self.args.stop_file.expanduser().resolve() if self.args.stop_file else self.output / "stop"
        require(not stop.exists(), "Hold stop-file already exists")
        atomic_json(self.output / "live-server.json", {"url": "http://127.0.0.1:" + str(self.port), "port": self.port,
            "pid": self.server.pid, "sessionIds": self.session_ids, "stop_file": str(stop), "hold_seconds": self.args.hold_seconds,
            "read_only_browser_checks": ["task_grouping has three main steps plus one separately labeled execution task",
                "single_todo_history ALPHA summary stays its own completed item after expanding, resizing and reloading; BETA is current"]})
        self.progress("Verified private WebUI held for independent browser inspection")
        deadline = time.monotonic() + self.args.hold_seconds
        while time.monotonic() < deadline and not stop.exists():
            require(self.server.poll() is None, "Held private WebUI exited unexpectedly")
            time.sleep(.25)
        self.report["hold"] = {"requested_seconds": self.args.hold_seconds, "stop_file_observed": stop.exists()}

    def run(self) -> int:
        try:
            self.prepare()
            self.start_server("first")
            self.run_case("read_visibility", self.case_read)
            self.run_case("task_grouping", self.case_plan)
            self.run_case("single_todo_history", self.case_history)
            self.restart_and_verify()
            self.hold()
            self.verify_freeze()
            self.report["passed"] = True
        except BaseException as error:
            self.report["failure_type"] = type(error).__name__
            # Only our fixed assertions contain safe text; arbitrary HTTP, auth,
            # model and parser exception payloads are never retained.
            if type(error) is AssertionError:
                self.report["failure"] = str(error)[:300]
        finally:
            if hasattr(self, "source_auth"):
                try:
                    current = digest(self.source_auth) if self.source_auth.is_file() else None
                    self.report["original_auth_unchanged"] = current == self.source_auth_sha
                except OSError:
                    self.report["original_auth_unchanged"] = False
            try:
                self.cleanup()
            except BaseException as error:
                self.report["cleanup_failure_type"] = type(error).__name__
                self.report["passed"] = False
            if self.report.get("original_config_unchanged") is False or self.report.get("original_auth_unchanged") is False:
                self.report["passed"] = False
            self.report["session_ids"] = self.session_ids
            self.report["finished_utc"] = utc_now()
            atomic_json(self.output / "summary.json", self.report)
        print(json.dumps({"passed": self.report["passed"], "summary": str(self.output / "summary.json")}), flush=True)
        return 0 if self.report["passed"] else 1


def arguments(argv: list[str] | None = None) -> argparse.Namespace:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--binary", type=Path, required=True)
    parser.add_argument("--destroy-input", action="store_true", required=True)
    parser.add_argument("--expected-sha256", required=True)
    parser.add_argument("--source-freeze", type=Path, required=True)
    parser.add_argument("--repo", type=Path, default=Path(__file__).resolve().parents[2])
    parser.add_argument("--source-home", type=Path, default=Path(os.environ.get("METIS_HOME", str(Path.home() / ".metis"))))
    parser.add_argument("--output", type=Path, required=True)
    parser.add_argument("--query-timeout", type=int, default=240)
    parser.add_argument("--max-output-tokens", type=int, default=4096)
    parser.add_argument("--hold-seconds", type=int, default=0)
    parser.add_argument("--stop-file", type=Path)
    args = parser.parse_args(argv)
    if not re.fullmatch(r"[a-f0-9]{64}", args.expected_sha256):
        parser.error("--expected-sha256 must be SHA256 hex")
    if not 30 <= args.query_timeout <= 240 or not 2048 <= args.max_output_tokens <= 8192 or not 0 <= args.hold_seconds <= 3600:
        parser.error("query-timeout must be 30..240, output 2048..8192, hold 0..3600")
    return args


def main(argv: list[str] | None = None) -> int:
    args = arguments(argv)
    # A constructor failure must not strand the specifically supplied test CLI.
    binary = disposable_binary(args.binary)
    original_sha = digest(binary)
    try:
        return ContextIntegrityLiveCheck(args).run()
    except (AssertionError, OSError, ValueError) as error:
        print(json.dumps({"passed": False, "startup_failure_type": type(error).__name__}), file=sys.stderr)
        return 1
    finally:
        if binary.exists():
            require(digest(binary) == original_sha, "Refusing to destroy a replaced disposable input CLI")
            binary.unlink()


if __name__ == "__main__":
    def interrupted(_signum: int, _frame: Any) -> None:
        raise InterruptedError("Private context acceptance interrupted")
    signal.signal(signal.SIGTERM, interrupted)
    raise SystemExit(main())
