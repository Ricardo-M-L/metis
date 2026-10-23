#!/usr/bin/env python3
"""Opt-in headless Desktop context/usage acceptance with real selected-route queries.

Caller supplies one newly built, source-frozen disposable CLI. Live mode uses
only a private HOME/workspace, the original selected provider/model credential,
and a loopback HTTP API. It never opens a browser/native application, sends a
message to another person, requests OS permission, or substitutes a provider.
Both caller and private CLI copies, credentials, workers and listener are
destroyed on success or failure before a source repair may begin. --self-test
uses only in-memory oracle fixtures: no network, authentication or GUI access.
"""
from __future__ import annotations

import argparse
import http.client
import json
import os
from pathlib import Path
import re
import secrets
import shutil
import signal
import socket
import subprocess
import sys
import tempfile
import time
from typing import Any
from urllib.parse import quote
import uuid

from persistent_agent_live_check import (PersistentAgentLiveCheck, atomic_json, digest,
    history_snapshots, message_text, require, trace_proof, utc_now)


def context_oracle(value: dict[str, Any], sid: str) -> dict[str, Any]:
    require(value.get("contextSessionId") == sid, "Context pressure belongs to a different session")
    require(all(type(value.get(key)) is int and value[key] >= 0 for key in ("contextUsed", "contextWindow")) and
            value["contextUsed"] > 0 and value["contextWindow"] > 0, "Context pressure/window is missing, negative or empty after a real turn")
    parts = value.get("contextBreakdown")
    require(isinstance(parts, dict) and parts.get("estimated") is True and
            all(type(parts.get(key)) is int and parts[key] >= 0 for key in ("systemTokens", "toolTokens", "messageTokens")),
            "Current-session context categories are missing, negative or not identified as estimates")
    # The prepared estimate categories and measured provider context are
    # different observations. Never normalize categories to cumulative usage.
    return {"session_id": sid, "contextUsed": value["contextUsed"], "contextWindow": value["contextWindow"],
            "contextBreakdown": parts, "estimated_category_sum": sum(parts[key] for key in ("systemTokens", "toolTokens", "messageTokens")),
            "cumulative_usage_not_used_as_context": True}


def isolation_oracle(value: dict[str, Any], owner: str) -> None:
    require("contextBreakdown" not in value and value.get("contextSessionId") != owner,
            "Another/empty/unknown session leaked the original session context categories")


def restored_context_oracle(value: dict[str, Any], sid: str) -> dict[str, Any]:
    # activateSession -> RebindProviderRuntime/ResetSession invalidates the
    # prepared prefix, then installs ownership of the restored transcript.
    # Status must not assemble a fresh request or borrow the old worker prefix.
    require(value.get("contextSessionId") == sid and all(type(value.get(key)) is int and value[key] >= 0
            for key in ("contextUsed", "contextWindow")) and value["contextWindow"] > 0,
            "Restored context pressure/window lost its session ownership or valid total")
    if "contextBreakdown" in value:
        return {**context_oracle(value, sid), "restored_prepared_categories_present": True}
    return {"session_id": sid, "contextUsed": value["contextUsed"], "contextWindow": value["contextWindow"],
            "restored_prepared_categories_present": False, "categories_omitted_until_next_request_or_preflight": True,
            "old_prepared_prefix_not_reused": True}


def merged_route_header(records: list[dict[str, Any]], sid: str, provider: str, model: str) -> dict[str, Any]:
    """Project only route fields using Store.Load/mergeHeader semantics.

    Desktop appends ID/status/title-only headers. Empty provider/model fields
    leave the established route intact. This test requires its initial full
    owner header and rejects every explicit route change, even if changed back.
    """
    headers = [row.get("header") for row in records if row.get("type") == "header"]
    require(headers and isinstance(headers[0], dict) and headers[0].get("id") == sid and
            headers[0].get("provider") == provider and headers[0].get("model") == model and provider and model,
            "Initial session header lacks the exact complete owner/provider/model route")
    merged = {"id": sid, "provider": provider, "model": model}
    for header in headers:
        require(isinstance(header, dict) and header.get("id") == sid, "A session header patch belongs to another owner")
        for field, expected in (("provider", provider), ("model", model)):
            value = header.get(field)
            require(value is None or isinstance(value, str), "Session route field has an invalid type")
            if value:
                require(value == expected, "Session header explicitly changed its original selected " + field)
                merged[field] = value
    return {**merged, "header_entries": len(headers), "partial_header_entries": sum(
        not header.get("provider") or not header.get("model") for header in headers),
        "route_projection_contract": "Store.Load/mergeHeader nonempty provider/model; explicit changes rejected"}


def usage_oracle(value: dict[str, Any], sid: str, events: list[dict[str, Any]], cost: dict[str, Any], turns: int) -> dict[str, Any]:
    require(value.get("enabled") is True and value.get("sessionId") == sid and value.get("source") == "live",
            "Trace API does not describe the requested live session")
    stats = value.get("stats", {})
    require(all(type(stats.get(key)) is int and stats[key] >= 0 for key in
                ("inputTokens", "outputTokens", "cacheReadTokens", "cacheWriteTokens")) and stats.get("turns") == turns,
            "Cumulative trace statistics lack exact nonnegative token buckets/turn ownership")
    require(events and all(row.get("session_id") == sid for row in events), "Persisted trace contains another session")
    raw = {"input": 0, "output": 0, "cache_write": 0, "cache_read": 0}
    replies = 0
    for row in events:
        require(not row.get("is_error") and row.get("kind") != "error", "Real query recorded a provider/tool error")
        require(row.get("kind") not in ("tool_args", "tool_start", "tool_result"), "Short context-only query executed an unexpected tool")
        if row.get("kind") != "tokens":
            continue
        require(not row.get("subagent_of") and not row.get("trace_parent_invocation_id") and row.get("trace_invocation_id"),
                "Usage is not owned by the original root query invocation")
        values = {key: int(number) for key, number in re.findall(r"(input|output|cache_write|cache_read)=(\d+)", row.get("text", ""))}
        require(set(values) == set(raw), "Real provider usage receipt is incomplete")
        for key in raw:
            raw[key] += values[key]
        replies += 1
    require(replies >= turns and sum(raw.values()) > 0, "Two real turns lack nonzero provider usage receipts")
    inclusive_input = raw["input"] + raw["cache_write"] + raw["cache_read"]
    require(stats["inputTokens"] == inclusive_input and stats["outputTokens"] == raw["output"] and
            stats["cacheReadTokens"] == raw["cache_read"] and stats["cacheWriteTokens"] == raw["cache_write"],
            "Trace cumulative input omitted cache buckets or differs from absolute real receipts")
    expected_cost = {"input_tokens": raw["input"], "output_tokens": raw["output"],
                     "cache_read_tokens": raw["cache_read"], "cache_create_tokens": raw["cache_write"]}
    require(all(type(cost.get(key)) is int and cost[key] == amount for key, amount in expected_cost.items()),
            "Persisted session usage ledger differs from real trace receipts")
    total = inclusive_input + raw["output"]
    # Current API exposes input/output buckets; the UI computes their total.
    # Validate a total if added later, while labeling today's sum as derived.
    require("totalTokens" not in stats or stats["totalTokens"] == total, "Trace total differs from input plus output")
    return {"session_id": sid, "turns": turns, "real_provider_responses": replies, "raw_usage_receipts": raw,
            "api_input_tokens_including_cache": inclusive_input, "api_output_tokens": raw["output"],
            "derivedTotalTokens": total, "total_formula": "inputTokens + outputTokens", "total_is_host_derived": "totalTokens" not in stats,
            "real_cache_receipt_observed": raw["cache_read"] + raw["cache_write"] > 0,
            "contextUsed_not_used_for_accounting": True}


class ContextUsageLiveCheck(PersistentAgentLiveCheck):
    def __init__(self, args: argparse.Namespace) -> None:
        super().__init__(args)
        self.port: int | None = None
        self.server_child = None
        self.shutdown_token = ""
        self.active_http: set[http.client.HTTPConnection] = set()
        self.api_index = 0
        self.originals: dict[str, str | None] = {}
        self.report["boundary"] = ("Two real short API turns in one private headless Desktop session, live context estimate categories, "
            "cache-inclusive cumulative usage and cross-session isolation. No browser/native UI, OS input, OS permission changes, "
            "external messaging, supplier invoice reconciliation or cross-platform native acceptance.")

    def http(self, method: str, path: str, body: Any = None, *, timeout: float = 10,
             headers: dict[str, str] | None = None) -> tuple[int, Any]:
        require(self.port is not None, "Loopback service is not configured")
        connection = http.client.HTTPConnection("127.0.0.1", self.port, timeout=timeout)
        self.active_http.add(connection)
        try:
            connection.request(method, path, None if body is None else json.dumps(body).encode(),
                {"Content-Type": "application/json", "Connection": "close", **(headers or {})})
            response = connection.getresponse()
            data = response.read(8 * 1024 * 1024 + 1)
            require(len(data) <= 8 * 1024 * 1024, "Loopback API response exceeded its bound")
            return response.status, json.loads(data) if data else None
        finally:
            connection.close()
            self.active_http.discard(connection)

    def api(self, label: str, method: str, path: str, body: Any = None, *, expected: int = 200,
            timeout: float = 10, headers: dict[str, str] | None = None) -> Any:
        status, value = self.http(method, path, body, timeout=timeout, headers=headers)
        self.api_index += 1
        evidence = {"method": method, "path": path, "status": status, "body": value}
        atomic_json(self.output / (f"api-{self.api_index:02d}-" + label + ".json"), json.loads(self.redact(json.dumps(evidence))))
        require(status == expected, label + " returned HTTP " + str(status))
        return value

    def installed_and_original_guard(self) -> None:
        original_home = Path(os.environ.get("METIS_HOME", str(Path.home() / ".metis"))).expanduser().resolve()
        paths = [original_home / "config.toml", original_home / ".credentials" / "auth.json"]
        installed = shutil.which("metis")
        if installed:
            installed_path = Path(installed).resolve()
            require(installed_path != self.source_binary, "Supply a disposable build, never the installed CLI")
            paths.append(installed_path)
        self.originals = {str(path): digest(path) if path.is_file() else None for path in paths}
        self.report["original_files_before"] = self.originals

    def verify_originals(self) -> None:
        after = {name: digest(Path(name)) if Path(name).is_file() else None for name in self.originals}
        require(after == self.originals, "Original provider config/auth or installed CLI changed")
        self.report["original_files_unchanged"] = True

    def start_headless_server(self) -> None:
        # Reviewed cmdDesktop --web only logs a URL and calls srv.Run. Native
        # launching is behind !opts.web; this checker invokes no open-* API.
        self.report["headless_entrypoint"] = {"command": "desktop --web --port PORT", "loopback_only": True,
            "automatically_opens_browser": False, "source_sha256": digest(self.repo / "cmd/metis/desktop.go")}
        with socket.socket() as candidate:
            candidate.bind(("127.0.0.1", 0))
            self.port = candidate.getsockname()[1]
        self.shutdown_token = secrets.token_hex(32)
        self.secrets.append(self.shutdown_token)
        self.env["METIS_DESKTOP_FRAME_TOKEN"] = self.shutdown_token
        # Private runtime has no MCP registrations, channels, plugins or cron
        # state. Read is the only available tool and prompts require no tools.
        config_path = self.home / "config.toml"
        text = config_path.read_text().replace('allowed = ["Read", "Write", "MessageTeammate", "Skill", "Workflow", "Agent"]', 'allowed = ["Read"]')
        config_path.write_text(text)
        self.server_child = self.spawn(["desktop", "--web", "--port", str(self.port)], "headless-desktop-service")
        process = self.server_child[0]
        deadline = time.monotonic() + 40
        while time.monotonic() < deadline:
            require(process.poll() is None, "Private Desktop backend exited before becoming ready")
            try:
                status, health = self.http("GET", "/api/health", timeout=1)
                if status == 200 and health.get("status") == "ok":
                    break
            except (OSError, ValueError, http.client.HTTPException):
                pass
            time.sleep(0.1)
        else:
            raise AssertionError("Private Desktop backend did not become healthy")
        lsof = shutil.which("lsof")
        require(lsof, "Listener ownership check requires lsof")
        check = subprocess.run([lsof, "-nP", "-a", "-iTCP:" + str(self.port), "-sTCP:LISTEN", "-Fp"], capture_output=True, text=True, timeout=10)
        owners = {int(line[1:]) for line in check.stdout.splitlines() if re.fullmatch(r"p\d+", line)}
        require(check.returncode == 0 and owners == {process.pid}, "Selected loopback port is not owned exclusively by the exact private CLI PID")
        self.record("headless loopback Desktop server owns its temporary listener", pid=process.pid, port=self.port,
                    browser_or_native_app_launched=False, provider=self.provider, model=self.model)

    def new_session(self, label: str) -> str:
        created = self.api(label, "POST", "/api/sessions", {}, expected=201)
        sid = created.get("id", "")
        require(re.fullmatch(r"[A-Za-z0-9_-]+", sid) and created.get("model") == self.model and
                Path(created.get("workDir", created.get("work_dir", ""))).resolve() == self.workspace,
                "New private session changed its model/workspace identity")
        return sid

    def real_proof(self, before: dict[str, int], label: str, *, required_tools: tuple[str, ...] = ()) -> tuple[dict[str, Any], list[dict[str, Any]]]:
        # The shared checker predates Desktop's incremental lifecycle headers
        # and treats the final raw header as complete. Keep its real usage and
        # occurrence evidence while using this transport's formal merge rules.
        rows = self.trace_events(before)
        proof = trace_proof(rows, self.provider, self.model)
        require(proof["model_responses"] > 0 and any(sum(row.get(field, 0) for field in
            ("input", "output", "cache_write", "cache_read")) > 0 for row in proof["usage"]), label + " has no real provider usage")
        require(not proof["errors"], label + " has unexpected provider/tool errors: " + json.dumps(proof["errors"]))
        for tool in required_tools:
            require(proof["tool_results"].get(tool, 0) > 0, label + " did not actually execute " + tool)
        headers = []
        for sid in proof["session_ids"]:
            paths = [path for path in self.home.rglob(sid + ".jsonl") if path.parent.name == "sessions"]
            require(len(paths) == 1, label + " session identity is absent or ambiguous on disk")
            records = [json.loads(line) for line in paths[0].read_text().splitlines() if line]
            route = merged_route_header(records, sid, self.provider, self.model)
            canonical = self.api(label + "-canonical-session-route", "GET", "/api/sessions/" + quote(sid))
            require(canonical.get("session", {}).get("id") == sid and canonical["session"].get("model") == route["model"],
                    "Formal Store.Load session API disagrees with the merged owner/model route")
            headers.append({"session_id": sid, **route, "transcript_sha256": digest(paths[0]),
                            "canonical_session_api_model_matched": True})
        proof["session_headers"] = headers
        atomic_json(self.output / (label + "-trace-proof.json"), proof)
        return proof, rows

    def check_two_turns(self) -> str:
        sid = self.new_session("create-context-owner")
        empty = self.new_session("create-empty-isolation-session")
        unknown = "absent-" + uuid.uuid4().hex
        previous = None
        prompts = []
        for turn in (1, 2):
            prompt = "Do not use any tools. Reply with only CONTEXT-" + uuid.uuid4().hex[:12] + "-" + str(turn) + "."
            prompts.append(prompt)
            before = self.trace_snapshot()
            response = self.api("real-turn-" + str(turn), "POST", "/api/turns", {"sessionId": sid, "input": prompt}, timeout=self.args.query_timeout)
            require(response.get("sessionId") == sid and not response.get("stopped"), "Real HTTP turn was interrupted or rebound to another session")
            measured, _ = self.real_proof(before, "real-turn-" + str(turn))
            require(measured["session_ids"] == [sid], "Real query usage crossed a session boundary")
            status = self.api("context-after-turn-" + str(turn), "GET", "/api/status?sessionId=" + quote(sid))
            context = context_oracle(status, sid)
            trace = self.api("trace-after-turn-" + str(turn), "GET", "/api/trace?sessionId=" + quote(sid) + "&limit=2000")
            events = [json.loads(line) for line in (self.home / "traces" / (sid + ".jsonl")).read_text().splitlines() if line]
            cost = json.loads((self.home / "sessions" / (sid + ".cost.json")).read_text())
            usage = usage_oracle(trace, sid, events, cost, turn)
            if previous:
                require(usage["api_input_tokens_including_cache"] > previous["api_input_tokens_including_cache"] and
                        usage["api_output_tokens"] > previous["api_output_tokens"], "Second real query did not accumulate actual input/output usage")
            previous = usage
            self.record("real turn " + str(turn) + " has session-owned estimated context and independent cumulative usage",
                        context=context, cumulative_usage=usage, new_turn_usage=measured)
            for label, other in (("empty", empty), ("unknown", unknown)):
                other_status = self.api(label + "-isolation-after-" + str(turn), "GET", "/api/status?sessionId=" + quote(other))
                isolation_oracle(other_status, sid)
        transcript = self.home / "sessions" / (sid + ".jsonl")
        records = [json.loads(line) for line in transcript.read_text().splitlines() if line]
        route = merged_route_header(records, sid, self.provider, self.model)
        history = history_snapshots(transcript)[-1]
        require(all(sum(message.get("role") == "user" and message_text(message) == prompt for message in history) == 1 for prompt in prompts),
                "Durable session does not own exactly the two real query prompts")
        self.api("activate-empty-session", "POST", "/api/sessions/activate", {"id": empty})
        isolation_oracle(self.api("active-empty-status", "GET", "/api/status"), sid)
        blank_trace = self.api("empty-session-trace", "GET", "/api/trace?sessionId=" + quote(empty))
        require(blank_trace.get("sessionId") == empty and blank_trace.get("stats", {}).get("inputTokens") == 0 and
                blank_trace.get("stats", {}).get("outputTokens") == 0, "Cumulative usage leaked into an empty session")
        self.api("reactivate-owner-session", "POST", "/api/sessions/activate", {"id": sid})
        restored = restored_context_oracle(self.api("reactivated-owner-status", "GET", "/api/status?sessionId=" + quote(sid)), sid)
        restored_trace = self.api("reactivated-owner-trace", "GET", "/api/trace?sessionId=" + quote(sid) + "&limit=2000")
        restored_usage = usage_oracle(restored_trace, sid, events, json.loads((self.home / "sessions" / (sid + ".cost.json")).read_text()), 2)
        require(previous is not None and restored_usage["raw_usage_receipts"] == previous["raw_usage_receipts"],
                "Session navigation changed cumulative real usage")
        invalid = self.api("invalid-session-id", "GET", "/api/status?sessionId=bad%2Fid", expected=400)
        require("contextBreakdown" not in invalid, "Invalid session request disclosed context categories")
        self.record("navigation and explicit viewed-session queries keep estimates and cumulative totals isolated",
            owner_session_id=sid, empty_session_id=empty, transcript_sha256=digest(transcript), restored_context=restored,
            restored_cumulative_usage=restored_usage, merged_session_route=route, additional_model_requests_for_navigation=0)
        return sid

    def visual_window(self, sid: str) -> None:
        if not self.args.visual_wait_seconds:
            return
        atomic_json(self.output / "visual-ready.json", {"url": "http://127.0.0.1:" + str(self.port), "sessionId": sid})
        started = time.monotonic()
        deadline = started + self.args.visual_wait_seconds
        done = self.output / "visual-done"
        while time.monotonic() < deadline and not done.is_file():
            time.sleep(0.25)
        self.report["optional_visual_window"] = {"maximum_seconds": self.args.visual_wait_seconds,
            "elapsed_seconds": round(time.monotonic() - started, 3), "ended_by_visual_done": done.is_file(),
            "harness_opened_browser_or_GUI": False}

    def cleanup(self) -> None:
        for connection in list(self.active_http):
            connection.close()
        self.active_http.clear()
        shutdown_error = None
        if self.server_child and self.server_child[0].poll() is None and self.shutdown_token:
            try:
                status, _ = self.http("POST", "/api/shutdown", {}, headers={"X-Metis-Desktop-Token": self.shutdown_token}, timeout=5)
                require(status == 202, "Private backend did not acknowledge its own shutdown token")
                self.finish(self.server_child, "headless-desktop-service", json_output=False, operation_timeout=20)
            except Exception as error:
                shutdown_error = type(error).__name__
        try:
            super().cleanup()
        finally:
            # A process-inspection error must never skip executable/auth cleanup.
            self.binary.unlink(missing_ok=True)
            shutil.rmtree(self.home / ".credentials", ignore_errors=True)
            self.key = ""
            if self.source_binary.exists():
                require(digest(self.source_binary) == getattr(self, "input_sha", self.args.expected_sha256), "Refusing to destroy a replaced disposable CLI input")
                self.source_binary.unlink()
        released = True
        if self.port is not None:
            with socket.socket() as probe:
                probe.settimeout(1)
                released = probe.connect_ex(("127.0.0.1", self.port)) != 0
        self.report.setdefault("cleanup", {}).update({"loopback_listener_released": released,
            "shutdown_failure_class": shutdown_error, "all_http_connections_closed": not self.active_http})
        require(released, "Temporary listener is still reachable after owned process cleanup")

    def run_context(self) -> int:
        def interrupted(_signum: int, _frame: Any) -> None:
            raise TimeoutError("Bounded headless context acceptance interrupted or timed out")
        previous = {number: signal.signal(number, interrupted) for number in (signal.SIGINT, signal.SIGTERM, signal.SIGALRM)}
        signal.setitimer(signal.ITIMER_REAL, self.args.case_timeout)
        try:
            self.installed_and_original_guard()
            self.stage()
            self.load_route()
            self.start_headless_server()
            sid = self.check_two_turns()
            self.visual_window(sid)
            self.report["passed"] = True
        except BaseException as error:
            self.report.update({"passed": False, "failure_type": type(error).__name__, "failure": self.redact(str(error))})
        finally:
            signal.setitimer(signal.ITIMER_REAL, 0)
            for label, action in (("cleanup", self.cleanup), ("evidence_copy", self.copy_evidence),
                                  ("source_freeze", self.verify_freeze), ("original_files", self.verify_originals)):
                try:
                    action()
                except BaseException as error:
                    self.report["passed"] = False
                    self.report[label + "_failure"] = self.redact(str(error))
            shutil.rmtree(self.root, ignore_errors=True)
            self.report.setdefault("cleanup", {})["temporary_workspace_destroyed"] = not self.root.exists()
            self.report["passed"] = self.report["passed"] and not self.root.exists()
            self.report.update({"finished_utc": utc_now(), "elapsed_seconds": round(time.monotonic() - self.started, 3)})
            self.report = json.loads(self.redact(json.dumps(self.report)))
            self.secrets.clear()
            self.report["cleanup"]["redaction_secret_values_cleared_after_evidence"] = not self.secrets
            atomic_json(self.output / "verification-report.json", self.report)
            for number, handler in previous.items():
                signal.signal(number, handler)
        print(json.dumps({"passed": self.report["passed"], "failure": self.report.get("failure"),
                          "report": str(self.output / "verification-report.json")}), flush=True)
        return 0 if self.report["passed"] else 1


def self_test() -> dict[str, Any]:
    context = {"contextSessionId": "owner", "contextUsed": 100, "contextWindow": 1000000,
               "contextBreakdown": {"systemTokens": 80, "toolTokens": 30, "messageTokens": 20, "estimated": True}}
    require(context_oracle(context, "owner")["estimated_category_sum"] == 130, "Context estimate was normalized to pressure")
    rejected = 0
    def rejects(action: Any) -> None:
        nonlocal rejected
        try:
            action()
        except AssertionError:
            rejected += 1
            return
        raise AssertionError("Offline negative oracle fixture was accepted")
    rejects(lambda: context_oracle(context, "other"))
    rejects(lambda: context_oracle({**context, "contextBreakdown": {**context["contextBreakdown"], "estimated": False}}, "owner"))
    rejects(lambda: context_oracle({**context, "contextBreakdown": {**context["contextBreakdown"], "messageTokens": -1}}, "owner"))
    restored = {key: value for key, value in context.items() if key != "contextBreakdown"}
    restored_proof = restored_context_oracle(restored, "owner")
    require(restored_proof["old_prepared_prefix_not_reused"] and restored_proof["categories_omitted_until_next_request_or_preflight"],
            "Restored history without a prepared request incorrectly requires or fabricates categories")
    require(restored_context_oracle(context, "owner")["restored_prepared_categories_present"],
            "Valid same-session prepared categories were not verified")
    rejects(lambda: context_oracle(restored, "owner"))  # Post-query checks remain strict.
    rejects(lambda: restored_context_oracle({**restored, "contextSessionId": "other"}, "owner"))
    rejects(lambda: restored_context_oracle({**context, "contextBreakdown": {**context["contextBreakdown"], "estimated": False}}, "owner"))
    isolation_oracle({"contextUsed": 0}, "owner")
    rejects(lambda: isolation_oracle(context, "owner"))
    rows = [{"session_id": "owner", "trace_invocation_id": "inv-1", "kind": "tokens", "text": "input=100 output=10 cache_write=20 cache_read=80"},
            {"session_id": "owner", "trace_invocation_id": "inv-2", "kind": "tokens", "text": "input=50 output=5 cache_write=0 cache_read=150"}]
    api = {"enabled": True, "source": "live", "sessionId": "owner", "stats": {"turns": 2,
           "inputTokens": 400, "outputTokens": 15, "cacheReadTokens": 230, "cacheWriteTokens": 20}}
    cost = {"input_tokens": 150, "output_tokens": 15, "cache_read_tokens": 230, "cache_create_tokens": 20}
    totals = usage_oracle(api, "owner", rows, cost, 2)
    require(totals["derivedTotalTokens"] == 415 and totals["real_cache_receipt_observed"], "Cache-inclusive cumulative oracle failed")
    rejects(lambda: usage_oracle({**api, "stats": {**api["stats"], "inputTokens": 150}}, "owner", rows, cost, 2))
    rejects(lambda: usage_oracle({**api, "stats": {**api["stats"], "totalTokens": 100}}, "owner", rows, cost, 2))
    rejects(lambda: usage_oracle(api, "other", rows, cost, 2))
    rejects(lambda: usage_oracle(api, "owner", rows, {**cost, "cache_read_tokens": 0}, 2))
    rejects(lambda: usage_oracle(api, "owner", [{**row, "session_id": "other"} for row in rows], cost, 2))
    headers = [{"type": "header", "header": {"id": "owner", "provider": "selected", "model": "selected-model"}},
               {"type": "header", "header": {"id": "owner", "model": "", "status": "running"}},
               {"type": "header", "header": {"id": "owner", "status": "completed"}}]
    route = merged_route_header(headers, "owner", "selected", "selected-model")
    require(route["model"] == "selected-model" and route["partial_header_entries"] == 2,
            "Lifecycle-only headers erased the formally merged route")
    rejects(lambda: merged_route_header(headers[1:], "owner", "selected", "selected-model"))
    rejects(lambda: merged_route_header([*headers, {"type": "header", "header": {"id": "owner", "model": "different"}}], "owner", "selected", "selected-model"))
    rejects(lambda: merged_route_header([*headers, {"type": "header", "header": {"id": "owner", "provider": "different"}}], "owner", "selected", "selected-model"))
    rejects(lambda: merged_route_header([*headers, {"type": "header", "header": {"id": "other", "status": "completed"}}], "owner", "selected", "selected-model"))
    rejects(lambda: merged_route_header(headers, "other", "selected", "selected-model"))
    # Simulated process-inspection failure still destroys staged inert files
    # and a fake credential directory; no product process/network is started.
    from unittest.mock import patch
    with tempfile.TemporaryDirectory() as directory:
        root = Path(directory)
        check = object.__new__(ContextUsageLiveCheck)
        check.active_http, check.server_child, check.shutdown_token, check.port = set(), None, "", None
        check.binary, check.source_binary, check.home = root / "private-cli", root / "caller-cli", root / "private-home"
        check.binary.write_text("inert private executable fixture")
        check.source_binary.write_text("inert caller executable fixture")
        (check.home / ".credentials").mkdir(parents=True)
        check.key = "fake-offline-key"
        check.args = argparse.Namespace(expected_sha256=digest(check.source_binary))
        with patch.object(PersistentAgentLiveCheck, "cleanup", side_effect=OSError("offline process-inspection failure")):
            try:
                check.cleanup()
            except OSError:
                pass
        require(not check.binary.exists() and not check.source_binary.exists() and not (check.home / ".credentials").exists() and not check.key,
                "Failure cleanup left a disposable CLI or private credential directory")
    return {"passed": True, "offline_oracles_only": True, "negative_cases_rejected": rejected,
            "real_provider_requests": 0, "authentication_files_read": False, "network_used": False, "GUI_used": False}


def main(argv: list[str] | None = None) -> int:
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
    parser.add_argument("--query-timeout", type=float, default=180)
    parser.add_argument("--case-timeout", type=float, default=600)
    parser.add_argument("--visual-wait-seconds", type=int, default=0,
        help="optional 0..60-second passive window for the parent to inspect the same real session; never opens a browser")
    args = parser.parse_args(argv)
    if args.self_test:
        print(json.dumps(self_test(), indent=2))
        return 0
    require(all((args.binary, args.expected_sha256, args.source_freeze, args.output, args.destroy_input)),
            "Live mode requires --binary --expected-sha256 --source-freeze --output --destroy-input")
    require(re.fullmatch(r"[a-f0-9]{64}", args.expected_sha256), "Supply the exact fresh CLI SHA-256")
    require(0 < args.query_timeout <= 600 and 0 < args.case_timeout <= 1800, "Live timeouts must be bounded")
    require(0 <= args.visual_wait_seconds <= 60, "Visual wait must be between 0 and 60 seconds")
    installed = shutil.which("metis")
    require(not installed or Path(installed).resolve() != args.binary.expanduser().resolve(), "Never pass the installed CLI as a disposable build")
    os.umask(0o077)
    return ContextUsageLiveCheck(args).run_context()


if __name__ == "__main__":
    raise SystemExit(main())
