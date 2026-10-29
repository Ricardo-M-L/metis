#!/usr/bin/env python3
"""Opt-in real-model HOME checkpoint and durable Desktop Rewind acceptance.

Supply a newly compiled, source-frozen disposable CLI. No build, installation,
provider substitution, user application, or system input occurs here. Every
tool call is fenced by an exact fixture-only PreToolUse policy. Six short real
queries exercise Edit/Write, no-tool chat, and one explicitly authorized Bash
write. Rewind is then read and applied after the original workers have exited
and after the private Desktop backend has restarted.

--hold-ui-seconds permits a bounded parent-driven browser check after HTTP
acceptance. Touch OUTPUT/visual-done to complete, or OUTPUT/visual-failed to
fail and immediately destroy all owned processes, credentials and both CLIs.
--self-test reads no credentials and uses no network or product process.
"""
from __future__ import annotations

import argparse
import hashlib
import json
import os
from pathlib import Path
import re
import shlex
import shutil
import signal
import socket
import sys
import time
from typing import Any
from urllib.parse import quote
import uuid

from context_usage_live_check import ContextUsageLiveCheck, merged_route_header
from checkpoint_multirepo_live_check import guard_decision, run_guard
from persistent_agent_live_check import (atomic_json, digest, history_snapshots,
    json_sha, message_text, require, trace_proof, utc_now)


def rewind_oracle(value: dict[str, Any], sid: str) -> list[dict[str, Any]]:
    require(value.get("sessionId") == sid and isinstance(value.get("revision"), str) and value["revision"],
            "Rewind list lost its exact session owner or revision")
    points = value.get("points")
    require(isinstance(points, list), "Rewind points are missing")
    turns = [point.get("turn") for point in points]
    require(all(type(turn) is int and turn > 0 for turn in turns) and turns == sorted(set(turns), reverse=True),
            "Rewind timeline is not unique, positive and newest-first")
    for point in points:
        require(isinstance(point.get("prompt"), str) and type(point.get("codeAvailable")) is bool,
                "Rewind point lacks a prompt or explicit code capability")
        if not point["codeAvailable"]:
            require(isinstance(point.get("codeUnavailableReason"), str) and point["codeUnavailableReason"],
                    "Unavailable code restore lacks its reason")
    return points


def default_home_oracle(value: dict[str, Any], sid: str, workspace: Path) -> None:
    state = value.get("checkpoint", value)
    require(state.get("sessionId") == sid and state.get("enabled") is True and state.get("sparse") is True and
            state.get("roots") in (None, []) and Path(state.get("workspace", "")).resolve() == workspace,
            "HOME session is not automatically enabled with sparse targets and no broad directory roots")


def restore_oracle(mode: str, before: list[dict[str, Any]], after: list[dict[str, Any]],
                   actual_old: str, actual_created: str | None, expected_old: str, expected_new: str,
                   expected_created: str, response: dict[str, Any], prompt: str) -> None:
    require(response.get("changed") is True and response.get("mode") == mode, "Rewind response did not acknowledge the selected mode")
    result = response.get("result", {})
    code = mode in ("code", "both")
    conversation = mode in ("conversation", "both")
    require(result.get("codeRestored") is code and result.get("conversationRestored") is conversation,
            "Rewind mode/result flags disagree")
    require(actual_old == (expected_old if code else expected_new) and
            actual_created == (None if code else expected_created),
            "Host file bytes/creation state disagree with selected restore mode")
    if conversation:
        require(before and not after and response.get("prefill") == prompt,
                "Conversation rewind did not durably remove the selected first turn and return its prompt")
    else:
        require(json_sha(before) == json_sha(after) and response.get("prefill", "") == "",
                "Code-only rewind changed history or supplied a replacement draft")


class AutoCheckpointRewindLiveCheck(ContextUsageLiveCheck):
    def __init__(self, args: argparse.Namespace) -> None:
        super().__init__(args)
        # The literal private HOME is intentionally also the workspace. A
        # subdirectory would exercise ordinary whole-project checkpoints.
        self.workspace = self.user_home.resolve()
        self.home = self.workspace / ".metis"
        self.home.mkdir(mode=0o700)
        self.fixture = self.workspace / "rewind-fixture"
        self.fixture.mkdir(mode=0o700)
        self.plan_path = self.root / "fixture-policy.json"
        self.cases: dict[str, dict[str, Any]] = {}
        self.listener_ports: list[int] = []
        self.real_turns: list[dict[str, Any]] = []
        self.report["boundary"] = ("Six real gpt-6.1-sol/high queries on the original selected route, private literal HOME sparse "
            "checkpoint default, exact fixture-only tools, worker-exit and backend-restart durability, three Desktop Rewind modes, "
            "missing/incomplete code rejection and stale revision rejection. Optional parent-driven browser UI only; no native "
            "installation, OS permission changes, cross-platform signing, arbitrary Bash recovery or external exactly-once claim.")

    def prepare_route(self) -> None:
        self.load_route()
        require(self.provider == self.args.provider and self.model == self.args.model,
                "Original selected provider/model differ; no fallback is permitted")
        config = self.home / "config.toml"
        text = config.read_text().replace(
            'allowed = ["Read", "Write", "MessageTeammate", "Skill", "Workflow", "Agent"]',
            'allowed = ["Read", "Write", "Edit", "Bash"]')
        require('allowed = ["Read", "Write", "Edit", "Bash"]' in text, "Private tool-list replacement failed")
        text += '\n[[permission.allow]]\ntool = "Edit"\n'
        hook = " ".join(shlex.quote(value) for value in (sys.executable, "-B", str(Path(__file__).resolve()), "--guard", str(self.plan_path)))
        text += '\n[[hooks.pre_tool_use]]\ntype = "command"\ntimeout_seconds = 10\ncommand = ' + json.dumps(hook) + '\n'
        config.write_text(text)
        config.chmod(0o600)
        # Full request dumps remain private and are deleted; only the model,
        # effort, request digest and response status projection is retained.
        self.env["METIS_DUMP_PROMPTS"] = "1"
        self.sentinel = self.workspace / "unrelated-project" / "untouched.txt"
        self.sentinel.parent.mkdir(mode=0o700)
        self.sentinel.write_text("UNRELATED_MUST_REMAIN_UNCHANGED_" + uuid.uuid4().hex + "\n")
        self.sentinel_sha = digest(self.sentinel)

    def api(self, label: str, method: str, path: str, body: Any = None, *, expected: int = 200,
            timeout: float = 10, headers: dict[str, str] | None = None) -> Any:
        status, value = self.http(method, path, body, timeout=timeout, headers=headers)
        encoded = json.dumps(value)
        require(not self.key or self.key not in encoded, "Loopback API echoed the selected provider credential")
        safe = value
        if path.startswith(("/api/turns", "/api/sessions/")):
            safe = {"sessionId": value.get("sessionId"), "stopped": value.get("stopped"),
                    "response_sha256": hashlib.sha256(encoded.encode()).hexdigest()}
        self.api_index += 1
        atomic_json(self.output / (f"api-{self.api_index:02d}-" + label + ".json"),
            json.loads(self.redact(json.dumps({"method": method, "path": path, "status": status, "body": safe}))))
        error = value.get("error", "") if isinstance(value, dict) else ""
        require(status == expected, label + " returned HTTP " + str(status) + ": " + str(error)[:240])
        return value

    def start_headless_server(self) -> None:
        # Parent method's legacy string replacement only narrows its original
        # six-tool default, so our already fenced four-tool list is retained.
        super().start_headless_server()
        self.listener_ports.append(self.port)

    def stop_for_restart(self) -> None:
        require(self.server_child and self.server_child[0].poll() is None, "No private live service to restart")
        old_port = self.port
        self.api("private-restart-shutdown", "POST", "/api/shutdown", {}, expected=202,
                 headers={"X-Metis-Desktop-Token": self.shutdown_token})
        self.finish(self.server_child, "headless-desktop-service", json_output=False, operation_timeout=20)
        with socket.socket() as probe:
            probe.settimeout(1)
            require(probe.connect_ex(("127.0.0.1", old_port)) != 0, "Old private listener survived restart shutdown")
        self.server_child = None

    def history(self, sid: str) -> list[dict[str, Any]]:
        snapshots = history_snapshots(self.home / "sessions" / (sid + ".jsonl"))
        return snapshots[-1] if snapshots else []

    def wire_projection(self, sid: str) -> list[dict[str, Any]]:
        file = self.home / "dump-prompts" / (sid + ".jsonl")
        require(file.is_file(), "Real query has no dispatched wire evidence")
        rows = [json.loads(line) for line in file.read_text().splitlines() if line]
        transport = self.settings.get("transport", "anthropic_messages")
        require(transport in ("openai", "openai_chat", "openai_responses"),
                "This fixed high-effort wire oracle requires the original OpenAI-compatible route")
        endpoint = self.settings["base_url"].rstrip("/") + ("/responses" if transport == "openai_responses" else "/chat/completions")
        proof = []
        for row in rows:
            if row.get("type") != "request":
                continue
            body = row.get("body", {})
            require(body.get("model") == self.model and row.get("url") == endpoint and row.get("method") == "POST",
                    "Dispatched query used a different model, endpoint or method")
            effort = body.get("reasoning_effort")
            if transport == "openai_responses":
                effort = body.get("reasoning", {}).get("effort")
            require(effort == "high", "Dispatched reasoning effort differs from requested high")
            responses = [other for other in rows if other.get("type") == "response" and other.get("reqid") == row.get("reqid")]
            require(len(responses) == 1 and responses[0].get("status") == 200,
                    "Physical request lacks exactly one HTTP 200 response")
            proof.append({"request_id": row.get("reqid"), "model": body["model"], "reasoning_effort": effort,
                          "request_sha256": row.get("request_sha256"), "status": 200})
        require(proof, "No real request projection was collected")
        return proof

    def configure_case(self, mode: str) -> dict[str, Any]:
        sid = self.new_session("create-" + mode)
        self.api("activate-" + mode, "POST", "/api/sessions/activate", {"id": sid})
        effort = self.api("high-effort-" + mode, "POST", "/api/effort", {"effort": "high"})
        require(effort.get("effort") == "high" and effort.get("supported") is True, "Real route did not accept high effort")
        target = self.fixture / (mode + "-existing.txt")
        created = self.fixture / (mode + "-created.txt")
        nonce = uuid.uuid4().hex[:12]
        old, new, made = "BEFORE_" + nonce + "\n", "AFTER_" + nonce + "\n", "CREATED_" + nonce + "\n"
        target.write_text(old)
        plan = {"session_id": sid, "cwd": str(self.workspace), "fixture": str(self.fixture),
                "paths": [str(target), str(created)], "commands": [], "write_path": str(created), "write_content": made,
                "edit_path": str(target), "edit_old": old, "edit_new": new}
        if mode == "bash":
            # The exact command is explicitly host-authorized and fenced;
            # sparse protection still must not pretend its range is known.
            plan["commands"] = ["printf '%s\\n' " + shlex.quote(new.rstrip("\n")) + " > " + shlex.quote(str(target))]
        atomic_json(self.plan_path, plan)
        case = {"sid": sid, "mode": mode, "target": target, "created": created, "old": old, "new": new, "made": made, "plan": plan}
        self.cases[mode] = case
        return case

    def real_query(self, mode: str) -> None:
        case = self.configure_case(mode)
        sid = case["sid"]
        listing = self.api("default-home-" + mode, "GET", "/api/rewind?sessionId=" + quote(sid))
        default_home_oracle(listing, sid, self.workspace)
        marker = "AUTO_REWIND_" + mode.upper() + "_DONE"
        if mode == "no-snapshot":
            prompt = "Do not call any tools or modify any files. Reply only " + marker + "."
            required_tools: set[str] = set()
        elif mode == "bash":
            prompt = ("This is a disposable fixture test. Use only Bash exactly once with this exact command:\n" +
                      case["plan"]["commands"][0] + "\nDo not use any other tool, inspect anything else, or retry. After success reply only " + marker + ".")
            required_tools = {"Bash"}
        else:
            prompt = ("This is an authorized disposable fixture test. Use only Read, Edit and Write. First Read " + str(case["target"]) +
                      "; then Edit that file, replacing exactly " + json.dumps(case["old"]) + " with " + json.dumps(case["new"]) +
                      "; then Write " + str(case["created"]) + " with exactly " + json.dumps(case["made"]) +
                      ". Do not touch other paths, delegate, use Bash, or perform extra operations. After the tools succeed reply only " + marker + ".")
            required_tools = {"Read", "Edit", "Write"}
        case["prompt"] = prompt
        before = self.trace_snapshot()
        result = self.api("real-query-" + mode, "POST", "/api/turns", {"sessionId": sid, "input": prompt}, timeout=self.args.query_timeout)
        require(result.get("sessionId") == sid and not result.get("stopped"), "Real fixture query stopped or changed owner")
        events = self.trace_events(before)
        proof = trace_proof(events, self.provider, self.model)
        require(proof["model_responses"] > 0 and proof["session_ids"] == [sid] and not proof["errors"] and
                any(sum(row.get(key, 0) for key in ("input", "output", "cache_write", "cache_read")) > 0 for row in proof["usage"]),
                "Real query lacks owned successful provider token receipts")
        require(set(proof["tool_results"]) == required_tools, "Real model executed missing or unexpected tools")
        history = self.history(sid)
        route_rows = [json.loads(line) for line in (self.home / "sessions" / (sid + ".jsonl")).read_text().splitlines() if line]
        merged_route_header(route_rows, sid, self.provider, self.model)
        require(any(marker in message_text(message) for message in history if message.get("role") == "assistant"),
                "Real model did not return its fixture completion marker")
        if mode != "no-snapshot":
            require(case["target"].read_text() == case["new"], "Real model failed to modify the fixture's existing file")
            if mode != "bash":
                require(case["created"].read_text() == case["made"], "Real model failed to create the fixture file")
        require(digest(self.sentinel) == self.sentinel_sha, "Unrelated HOME project sentinel changed")
        wire = self.wire_projection(sid)
        self.real_turns.append({"mode": mode, "session_id": sid, "usage": proof, "wire": wire, "history_sha256": json_sha(history)})
        points = rewind_oracle(self.api("post-worker-rewind-" + mode, "GET", "/api/rewind?sessionId=" + quote(sid)), sid)
        require(len(points) == 1 and points[0]["codeAvailable"] is (mode not in ("bash", "no-snapshot")),
                "Worker-exit Rewind capability disagrees with actual captured mutation")
        self.record("real " + mode + " query and post-worker capability passed", session_id=sid,
                    physical_requests=len(wire), tools=proof["tool_results"], code_available=points[0]["codeAvailable"])

    def durable_restore_checks(self) -> None:
        self.stop_for_restart()
        self.start_headless_server()
        for mode, case in self.cases.items():
            sid = case["sid"]
            snapshot = self.api("post-restart-points-" + mode, "GET", "/api/rewind?sessionId=" + quote(sid))
            points = rewind_oracle(snapshot, sid)
            default_home_oracle(snapshot, sid, self.workspace)
            require(len(points) == 1 and points[0]["codeAvailable"] is (mode not in ("bash", "no-snapshot")),
                    "Restart lost or fabricated a durable code checkpoint")
            before = self.history(sid)
            files = {str(p): digest(p) if p.exists() else None for p in (case["target"], case["created"], self.sentinel)}
            if mode == "ui-edit":
                require(case["target"].read_text() == case["new"] and case["created"].read_text() == case["made"],
                        "Independent UI fixture changed before parent interaction")
                self.record("post-restart independent real-edit checkpoint retained for parent UI", session_id=sid,
                            turn=1, existing_path=str(case["target"]), created_path=str(case["created"]))
                continue
            payload = {"sessionId": sid, "turn": 1, "mode": mode if mode in ("code", "both", "conversation") else "code", "revision": snapshot["revision"]}
            if mode in ("bash", "no-snapshot"):
                self.api("reject-uncaptured-code-" + mode, "POST", "/api/rewind", payload, expected=409)
                require(json_sha(self.history(sid)) == json_sha(before) and all(
                    (digest(Path(p)) if Path(p).exists() else None) == sha for p, sha in files.items()),
                    "Rejected uncaptured code restore changed history or files")
                self.record("post-restart unavailable code restore rejected without mutation", mode=mode, session_id=sid)
                continue
            response = self.api("restore-" + mode, "POST", "/api/rewind", payload)
            restore_oracle(mode, before, self.history(sid), case["target"].read_text(),
                           case["created"].read_text() if case["created"].exists() else None,
                           case["old"], case["new"], case["made"], response, case["prompt"])
            require(digest(self.sentinel) == self.sentinel_sha, "Rewind modified an unrelated project")
            if mode == "conversation":
                after = self.history(sid)
                after_files = {str(p): digest(p) if p.exists() else None for p in (case["target"], case["created"])}
                self.api("reject-genuinely-stale-revision", "POST", "/api/rewind", {**payload, "mode": "both"}, expected=409)
                require(json_sha(self.history(sid)) == json_sha(after) and all(
                    (digest(Path(p)) if Path(p).exists() else None) == sha for p, sha in after_files.items()),
                    "Stale revision rejection changed files or durable history")
            self.record("post-restart " + mode + " restores exactly selected state", session_id=sid, result=response.get("result"))
        self.report["real_turns"] = self.real_turns
        self.report["physical_model_requests"] = sum(len(turn["wire"]) for turn in self.real_turns)

    def hold_ui(self) -> None:
        if not self.args.hold_ui_seconds:
            return
        # Keep code-only's real transcript and Bash/no-snapshot disabled points
        # available, plus create one unrun session to check the blank state.
        owner = self.cases["ui-edit"]["sid"]
        self.api("activate-ui-owner", "POST", "/api/sessions/activate", {"id": owner})
        ready = {"url": "http://127.0.0.1:" + str(self.port), "sessionId": owner,
                 "sessions": {mode: case["sid"] for mode, case in self.cases.items()},
                 "ui_fixture": {"existing_path": str(self.cases["ui-edit"]["target"]),
                                "created_path": str(self.cases["ui-edit"]["created"]),
                                "before": self.cases["ui-edit"]["old"],
                                "after": self.cases["ui-edit"]["new"],
                                "expected_on_visual_done": "both restore completed: original bytes, created file absent, history empty"},
                 "private_root": str(self.root), "maximum_seconds": self.args.hold_ui_seconds,
                 "done_marker": str(self.output / "visual-done"), "failure_marker": str(self.output / "visual-failed")}
        atomic_json(self.output / "visual-ready.json", ready)
        self.progress("HTTP passed; waiting for parent UI verification", port=self.port, session_id=owner)
        print(json.dumps({"ui_ready": str(self.output / "visual-ready.json"), "maximum_seconds": self.args.hold_ui_seconds}), flush=True)
        deadline = time.monotonic() + self.args.hold_ui_seconds
        while time.monotonic() < deadline:
            require(not (self.output / "visual-failed").exists(), "Parent UI verification failed; test runtime must be destroyed before repair")
            if (self.output / "visual-done").exists():
                case = self.cases["ui-edit"]
                require(case["target"].read_text() == case["old"] and not case["created"].exists() and not self.history(owner),
                        "Parent UI completion marker lacks actual both-mode file and durable history restoration")
                require(digest(self.sentinel) == self.sentinel_sha, "Parent UI restore modified an unrelated HOME project")
                self.report["parent_ui_verified"] = True
                self.record("parent UI both restore independently verified on real files and durable history", session_id=owner)
                return
            time.sleep(0.2)
        raise AssertionError("Parent UI hold expired without an explicit completion marker")

    def safe_evidence(self) -> None:
        # Retain fixture/model output transcripts and traces, never credentials,
        # assembled prompt dumps, shadow Git objects or the private config.
        for directory in ("sessions", "traces"):
            source_dir = self.home / directory
            if not source_dir.exists():
                continue
            for source in source_dir.rglob("*"):
                if source.is_file() and not source.is_symlink() and source.suffix in (".json", ".jsonl"):
                    destination = self.output / "evidence" / directory / source.relative_to(source_dir)
                    destination.parent.mkdir(parents=True, exist_ok=True, mode=0o700)
                    destination.write_text(self.redact(source.read_text(errors="replace")))
                    destination.chmod(0o600)
        if self.plan_path.with_suffix(".audit.jsonl").exists():
            shutil.copyfile(self.plan_path.with_suffix(".audit.jsonl"), self.output / "fixture-policy.audit.jsonl")

    def run_acceptance(self) -> int:
        def interrupted(_signum: int, _frame: Any) -> None:
            raise TimeoutError("Bounded automatic-checkpoint/Rewind acceptance interrupted")
        previous = {number: signal.signal(number, interrupted) for number in (signal.SIGINT, signal.SIGTERM, signal.SIGALRM)}
        signal.setitimer(signal.ITIMER_REAL, self.args.case_timeout)
        try:
            self.installed_and_original_guard()
            self.stage()
            self.prepare_route()
            self.start_headless_server()
            for mode in ("both", "code", "conversation", "no-snapshot", "bash", "ui-edit"):
                self.real_query(mode)
            self.durable_restore_checks()
            self.hold_ui()
            self.report["passed"] = True
        except BaseException as error:
            self.report.update(passed=False, failure_type=type(error).__name__, failure=self.redact(str(error)))
        finally:
            signal.setitimer(signal.ITIMER_REAL, 0)
            # Stop processes and destroy executable/credential copies first;
            # source repairs may begin only after this finally block completes.
            for label, action in (("cleanup", self.cleanup), ("safe_evidence", self.safe_evidence),
                                  ("source_freeze", self.verify_freeze), ("original_files", self.verify_originals)):
                try:
                    action()
                except BaseException as error:
                    self.report["passed"] = False
                    self.report[label + "_failure"] = self.redact(str(error))
            shutil.rmtree(self.root, ignore_errors=True)
            self.report.setdefault("cleanup", {})["temporary_root_destroyed"] = not self.root.exists()
            self.report["passed"] = self.report["passed"] and not self.root.exists()
            self.report.update(finished_utc=utc_now(), elapsed_seconds=round(time.monotonic() - self.started, 3))
            self.report = json.loads(self.redact(json.dumps(self.report)))
            self.secrets.clear()
            atomic_json(self.output / "verification-report.json", self.report)
            for number, handler in previous.items():
                signal.signal(number, handler)
        print(json.dumps({"passed": self.report["passed"], "failure": self.report.get("failure"),
                          "report": str(self.output / "verification-report.json")}), flush=True)
        return 0 if self.report["passed"] else 1


def self_test() -> dict[str, Any]:
    checks = 0
    def rejects(action: Any) -> None:
        nonlocal checks
        try:
            action()
        except (AssertionError, ValueError):
            checks += 1
            return
        raise AssertionError("Negative offline oracle fixture was accepted")
    snapshot = {"sessionId": "owner", "revision": "rev", "points": [{"turn": 1, "prompt": "edit", "codeAvailable": False, "codeUnavailableReason": "no snapshot"}]}
    rewind_oracle(snapshot, "owner")
    rejects(lambda: rewind_oracle(snapshot, "other"))
    rejects(lambda: rewind_oracle({**snapshot, "revision": ""}, "owner"))
    rejects(lambda: rewind_oracle({**snapshot, "points": [{"turn": 1, "prompt": "edit", "codeAvailable": False}]}, "owner"))
    rejects(lambda: rewind_oracle({**snapshot, "points": snapshot["points"] * 2}, "owner"))
    home = Path("/owned-private-home")
    state = {"checkpoint": {"sessionId": "owner", "workspace": str(home), "enabled": True, "sparse": True, "roots": []}}
    default_home_oracle(state, "owner", home)
    rejects(lambda: default_home_oracle({"checkpoint": {**state["checkpoint"], "roots": [str(home)]}}, "owner", home))
    rejects(lambda: default_home_oracle({"checkpoint": {**state["checkpoint"], "sparse": False}}, "owner", home))
    # Exercise the existing exact-path safety guard without reading any real
    # auth/config or spawning a tool. Every path here is an owned temp file.
    import tempfile
    with tempfile.TemporaryDirectory() as directory:
        fixture = Path(directory)
        check = object.__new__(AutoCheckpointRewindLiveCheck)
        check.key, check.secrets, check.api_index, check.output = "", [], 0, fixture
        check.http = lambda *_args, **_kwargs: (202, None)
        require(check.api("empty-shutdown", "POST", "/api/shutdown", {}, expected=202) is None,
                "Successful HTTP 202 with an empty JSON body was rejected")
        check.http = lambda *_args, **_kwargs: (409, None)
        rejects(lambda: check.api("empty-rejection", "POST", "/api/shutdown", {}, expected=202))
        target = fixture / "existing.txt"
        target.write_text("before\n")
        plan = {"session_id": "owner", "cwd": directory, "fixture": directory,
                "paths": [str(target)], "commands": [], "edit_path": str(target),
                "edit_old": "before\n", "edit_new": "after\n"}
        event = {"hook_event_name": "PreToolUse", "session_id": "owner", "tool_name": "Edit",
                 "tool_input": {"path": str(target), "old": "before\n", "new": "after\n"}}
        guard_decision(event, plan)
        rejects(lambda: guard_decision({**event, "session_id": "other"}, plan))
        rejects(lambda: guard_decision({**event, "tool_input": {**event["tool_input"], "path": "/unrelated/file"}}, plan))
        rejects(lambda: guard_decision({**event, "tool_input": {**event["tool_input"], "new": "unplanned"}}, plan))
    history = [{"role": "user", "content": [{"type": "text", "text": "edit"}]}]
    for mode in ("both", "code", "conversation"):
        code, conversation = mode in ("both", "code"), mode in ("both", "conversation")
        result = {"changed": True, "mode": mode, "prefill": "edit" if conversation else "", "result": {"codeRestored": code, "conversationRestored": conversation}}
        restore_oracle(mode, history, [] if conversation else history, "old" if code else "new", None if code else "created", "old", "new", "created", result, "edit")
        rejects(lambda mode=mode, result=result, code=code, conversation=conversation: restore_oracle(mode, history, [] if conversation else history,
            "wrong", None if code else "created", "old", "new", "created", result, "edit"))
    return {"passed": True, "negative_oracle_cases_rejected": checks, "real_model_requests": 0,
            "authentication_files_read": False, "network_used": False, "product_processes_started": False}


def main(argv: list[str] | None = None) -> int:
    if argv is None:
        argv = sys.argv[1:]
    if len(argv) == 2 and argv[0] == "--guard":
        return run_guard(Path(argv[1]))
    parser = argparse.ArgumentParser(description=__doc__)
    mode = parser.add_mutually_exclusive_group(required=True)
    mode.add_argument("--run-live", action="store_true")
    mode.add_argument("--self-test", action="store_true")
    parser.add_argument("--repo", type=Path, default=Path(__file__).resolve().parents[2])
    parser.add_argument("--binary", type=Path)
    parser.add_argument("--expected-sha256")
    parser.add_argument("--source-freeze", type=Path)
    parser.add_argument("--output", type=Path)
    parser.add_argument("--destroy-input", action="store_true")
    parser.add_argument("--provider", default="easycliproxyapi")
    parser.add_argument("--model", default="gpt-6.1-sol")
    parser.add_argument("--query-timeout", type=float, default=180)
    parser.add_argument("--case-timeout", type=float, default=1800)
    parser.add_argument("--hold-ui-seconds", type=int, default=0)
    args = parser.parse_args(argv)
    if args.self_test:
        print(json.dumps(self_test(), indent=2))
        return 0
    require(all((args.binary, args.expected_sha256, args.source_freeze, args.output, args.destroy_input)),
            "Live mode requires --binary --expected-sha256 --source-freeze --output --destroy-input")
    require(re.fullmatch(r"[a-f0-9]{64}", args.expected_sha256), "Supply exact SHA-256 of the fresh disposable CLI")
    require(0 < args.query_timeout <= 600 and 0 < args.case_timeout <= 3600 and 0 <= args.hold_ui_seconds <= 1200,
            "Query/case/UI hold must have explicit finite bounds")
    installed = shutil.which("metis")
    require(not installed or Path(installed).resolve() != args.binary.expanduser().resolve(), "Never destroy the installed CLI")
    os.umask(0o077)
    return AutoCheckpointRewindLiveCheck(args).run_acceptance()


if __name__ == "__main__":
    raise SystemExit(main())
