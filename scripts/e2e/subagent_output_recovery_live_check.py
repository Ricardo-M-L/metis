#!/usr/bin/env python3
"""Original-route real CLI/WebUI acceptance for bounded child output recovery.

Never builds, installs, mocks a completion, replays a side effect or changes the
user profile. A caller supplies a frozen disposable CLI. One actual CLI run
and bounded private WebUI turns exercise default budgets, child-only caps,
continuation success/exhaustion, startup rejection and cold history isolation.
The private server may be held for browser/native-shell inspection; this
script does not itself launch or claim native pixel acceptance. Finally always
destroys the supplied CLI, private copy, credentials and owned processes.
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
import shlex
import shutil
import signal
import subprocess
import sys
import time
from typing import Any
from urllib.parse import quote
import uuid

sys.dont_write_bytecode = True
from context_integrity_live_check import ContextIntegrityLiveCheck, disposable_binary, active_history, history_calls
from image_context_pressure_live_check import digest, load_selected_route
from persistent_agent_live_check import history_snapshots, message_text
from project_coordinator_live_check import atomic_json, require, safe_endpoint, toml_table, utc_now
from trace_presentation_live_check import stable_digest

MODEL = "deepseek-flash"
PROFILE = "output-recovery-parent"
KEY_ENV = "METIS_SUBAGENT_RECOVERY_ROUTE_KEY"
TOOLS = ["Agent", "SubAgentOutput", "SubAgentList", "Read", "Bash"]
DEFAULT_BUDGET = 65536


def safe_execution(execution: dict[str, Any]) -> dict[str, Any]:
    fields = ("version", "provider", "model", "effort", "fast", "maxOutputTokens", "profile", "taskPlanId",
              "tools", "permissionMode", "permissionRulesHash", "legacyMigration")
    value = {field: execution[field] for field in fields if field in execution}
    value["effectiveEffort"] = execution.get("effort", "default")
    value["effectiveMaxOutputTokens"] = execution.get("maxOutputTokens", 0)
    # Prompt and working directory bodies do not belong in retained reports.
    for field in ("system", "workDir"):
        if isinstance(execution.get(field), str):
            value[field + "Sha256"] = hashlib.sha256(execution[field].encode()).hexdigest()
    return value


def safe_agent_input(arguments: dict[str, Any]) -> dict[str, Any]:
    return {field: arguments[field] for field in ("name", "model", "max_output_tokens", "subagent_type") if field in arguments}


def assert_execution(execution: dict[str, Any], cap: int) -> None:
    require(execution.get("model") == MODEL,
            "Child execution model mismatch: actual=" + repr(execution.get("model")) + ", expected=" + MODEL)
    require(execution.get("effort", "default") == "high",
            "Child execution effort mismatch: actual=" + repr(execution.get("effort", "default")) + ", expected='high'")
    require(execution.get("maxOutputTokens", 0) == cap,
            "Child execution cap mismatch: actual=" + repr(execution.get("maxOutputTokens", 0)) + ", expected=" + str(cap))


def exhaustion_fixture(nonce: str) -> str:
    lines = [f"记录{index:03d}：{secrets.token_hex(20)}。随机核验记录必须逐字完整保留，不得删改。" for index in range(1, 513)]
    value = "EXHAUST_COPY_BEGIN_" + nonce + "\n" + "\n".join(lines) + "\nEXHAUST_COPY_END_" + nonce
    require(49152 <= len(value.encode()) <= 73728, "Exhaustion fixture must be about 64KiB and fit bounded native source pages")
    return value


def incomplete_prefix_oracle(output: str, source: str) -> dict[str, Any]:
    begin, end = source.splitlines()[0], source.splitlines()[-1]
    start = output.find(begin)
    require(0 <= start <= 256 and output.count(begin) == 1,
            "Exhausted copy lacks one unique source beginning after a bounded visible preface")
    body = output[start:]
    require(body and source.startswith(body), "Exhausted cumulative output is not an exact contiguous source prefix")
    require(len(body) < len(source) and end not in body, "Exhaustion fixture was completed instead of exhausting its output budget")
    return {"exact_contiguous_source_prefix": True, "visible_preface_characters": start, "source_characters": len(source),
            "copied_source_prefix_characters": len(body), "source_end_marker_absent": True,
            "source_body_sha256": hashlib.sha256(source.encode()).hexdigest()}


def run_once_command_oracle(command: str, script: Path) -> list[str]:
    try: tokens = shlex.split(command)
    except ValueError: raise AssertionError("Side-effect fixture command has invalid shell quoting") from None
    allowed = [[str(script)]] + [[shell, str(script)] for shell in ("sh", "/bin/sh", "bash", "/bin/bash")]
    require(tokens in allowed, "Side-effect fixture command is outside the one-script shell token whitelist")
    return tokens


def safe_tool_diagnostics(history: list[dict[str, Any]], script: Path | None, root: Path) -> list[dict[str, Any]]:
    calls = history_calls(history)
    observed = []
    for call in calls:
        entry = {"tool": call["tool"], "is_error": call["is_error"], "call_index": call["call_index"], "result_index": call["result_index"]}
        if call["tool"] == "Bash":
            command = call["input"].get("command", "")
            entry["command_sha256"] = hashlib.sha256(command.encode()).hexdigest()
            try:
                tokens = shlex.split(command)
                allowed = {"sh", "/bin/sh", "bash", "/bin/bash"}
                entry["command_tokens"] = [token if token in allowed else
                    token.replace(str(root), "<private-root>") if script is not None and token == str(script) else
                    "<redacted-token>" for token in tokens]
                entry["command_token_count"] = len(tokens)
            except ValueError: entry["invalid_shell_quoting"] = True
        elif call["tool"] == "Read":
            parameters = {}
            for field in ("path", "file_path", "offset", "limit", "byte_offset", "byte_limit"):
                if field not in call["input"]: continue
                value = call["input"][field]
                if field in ("path", "file_path"):
                    value = value.replace(str(root), "<private-root>") if isinstance(value, str) and value.startswith(str(root) + "/") else (
                        value if isinstance(value, str) and re.fullmatch(r"[\w .-]{1,100}", value) else "<redacted-path>")
                parameters[field] = value
            entry["read_parameters"] = parameters
            if call["is_error"]:
                error = call["result"].replace(str(root), "<private-root>")
                entry["read_error_context"] = "[non-filesystem error context omitted]" if re.search(
                    r'(?i)authorization|bearer |api[_-]?key|"error"\s*:|HTTP[/ ]', error) else error[:512]
        observed.append(entry)
    return observed


def fixture_read_path(value: Any, expected: Path) -> bool:
    return value == str(expected)


def recoverable_read_errors(calls: list[dict[str, Any]], workspace: Path) -> list[dict[str, Any]]:
    errors = []
    for call in calls:
        if not call["is_error"]: continue
        require(call["tool"] == "Read", "An executing or delegating tool failed; this is not recoverable Read input correction")
        arguments = call["input"]
        path = arguments.get("path")
        category = ""
        if not isinstance(path, str) or not path.strip():
            category = "missing_or_invalid_native_path_parameter"
        elif not Path(path).is_absolute():
            category = "nonabsolute_native_path_parameter"
        else:
            resolved = Path(path).resolve()
            require(resolved.is_relative_to(workspace.resolve()), "Failed Read attempted a path outside the private workspace")
            invalid_numbers = any(field in arguments and (type(arguments[field]) is not int or arguments[field] < minimum or
                maximum is not None and arguments[field] > maximum)
                for field, minimum, maximum in (("offset", 1, None), ("limit", 1, None), ("byte_offset", 0, None), ("byte_limit", 4, 8192)))
            mixed = any(field in arguments for field in ("offset", "limit")) and any(field in arguments for field in ("byte_offset", "byte_limit"))
            if invalid_numbers or mixed: category = "invalid_native_paging_parameters"
            elif not resolved.exists(): category = "nonexistent_private_fixture_path"
            else: require(False, "Valid Read to an existing private fixture failed; this cannot be classified as a model input error")
        errors.append({"call_index": call["call_index"], "category": category, "corrected_read_error": True})
    return errors


def copy_difference(output: str, source: str) -> dict[str, Any]:
    begin = source.splitlines()[0]
    start = output.find(begin)
    body = output[start:] if start >= 0 else output
    common = next((index for index, (left, right) in enumerate(zip(body, source)) if left != right), min(len(body), len(source)))
    return {"begin_marker_offset": start, "actual_characters": len(body), "expected_characters": len(source),
            "first_different_character_offset": common if body != source else None, "full_source_present": source in output}


def provider_text_oracle(events: list[dict[str, Any]], history: list[dict[str, Any]], terminal: dict[str, Any]) -> dict[str, Any]:
    require(events and any(row.get("kind") == "text" for row in events), "No independent emitted provider text trace is available")
    segments: list[list[dict[str, Any]]] = [[]]
    boundaries, stops = [], [[]]
    for row in events:
        if row.get("kind") == "text":
            require(not row.get("truncated") and isinstance(row.get("text"), str) and row.get("stream_id"),
                    "Provider text trace is truncated or lacks exact stream ownership")
            require("[REDACTED" not in row["text"], "Provider text trace is redacted; exact fidelity evidence is inconclusive")
            segments[-1].append(row)
        elif row.get("kind") == "context" and row.get("source") == "recovery":
            match = re.search(r'<subagent_output_continuation reason="([^"]+)" attempt="(\d+)" max_attempts="3"/>', row.get("text", ""))
            require(match is not None, "Provider trace recovery context lacks its exact host boundary")
            reason, attempt = match.groups()
            require(reason in ("max_tokens", "length", "max_output_tokens") and int(attempt) == len(boundaries) + 1,
                    "Provider text trace has a non-length or nonsequential continuation boundary")
            boundaries.append({"attempt": int(attempt), "reason": reason})
            segments.append([]); stops.append([])
        elif row.get("kind") == "loop_done":
            stops[-1].append(row.get("text"))
    require(len(boundaries) == terminal.get("continuationAttempts", 0) and all(len(value) == 1 for value in stops),
            "Independent provider trace lacks one terminal stop per claimed output segment")
    accepted, previous, normalized, raw = "", "", [], []
    for index, rows in enumerate(segments):
        value = "".join(row["text"] for row in rows)
        require(value, "Independent provider trace has a reasoning-only segment, not visible text recovery")
        raw.append(value)
        prefixes = [prefix for prefix in (accepted, previous) if prefix and value.startswith(prefix)] if index else []
        trim = max(map(len, prefixes), default=0)
        projected = value[trim:]
        normalized.append(projected); accepted += projected
        if index:
            previous = projected
        else:
            last_stream = rows[-1]["stream_id"]
            previous = "".join(row["text"] for row in rows if row["stream_id"] == last_stream)
        if index < len(boundaries):
            require(stops[index][0] == boundaries[index]["reason"], "Recovery boundary contradicts actual provider segment stop")
    require(stops[-1][0] == terminal.get("stopHint"), "Provider trace final stop contradicts durable child terminal")
    canonical = "".join(message_text(message) for message in history if message.get("role") == "assistant")
    require(accepted == canonical == terminal.get("output"),
            "Host text fidelity failed: normalized emitted provider text differs from canonical history or terminal")
    return {"emitted_provider_text_verified": True, "raw_http_bytes_captured": False,
            "trace_boundary": "Loop.stream EventTextDelta after initial text filter/redaction; before host continuation prefix projection",
            "provider_text_segment_count": len(raw), "provider_text_segment_sha256": [hashlib.sha256(value.encode()).hexdigest() for value in raw],
            "normalized_provider_text_sha256": hashlib.sha256(accepted.encode()).hexdigest(),
            "normalization": "strip once the longest fully matching cumulative or last-assistant prefix; preserve incomplete matches",
            "continuation_boundaries": boundaries, "actual_segment_stops": [value[0] for value in stops]}


def source_copy_quality(output: str, source: str, *, complete: bool) -> dict[str, Any]:
    begin, end = source.splitlines()[0], source.splitlines()[-1]
    start = output.find(begin)
    require(0 <= start <= 256 and output.count(begin) == 1, "Copy lacks a unique BEGIN after its bounded visible preface")
    core = output[start:]
    pattern = r"步骤(\d{3})" if "步骤001" in source else r"记录(\d{3})"
    numbers = [int(value) for value in re.findall(pattern, core)]
    require(numbers and numbers == list(range(1, len(numbers) + 1)), "Copied source numbered records are missing, duplicated or out of order")
    if complete:
        require(end in core and core.count(end) == 1 and len(numbers) == len(source.splitlines()) - 2,
                "Completed copy lacks its END or one of its original numbered records")
    else:
        require(end not in core and len(numbers) < len(source.splitlines()) - 2, "Exhaustion copy unexpectedly reached its complete END")
    exact = core == source if complete else source.startswith(core)
    return {"source_copy_exact": exact, "source_copy_quality_is_runtime_gate": False,
            "provider_authored_copy_difference": copy_difference(output, source), "ordered_numbered_records": len(numbers),
            "business_begin_end_and_records_verified": True, "source_end_marker_absent": not complete}


def complete_read_coverage_oracle(calls: list[dict[str, Any]], source: str) -> dict[str, Any]:
    raw, covered = source.encode(), 0
    spans = []
    for call in calls:
        match = re.search(r"\[Read source bytes (\d+)-(\d+) of (\d+); end exclusive\]", call["result"])
        require(match is not None, "Exhaustion fixture Read lacks its actual source-byte receipt")
        start, end, total = map(int, match.groups())
        require(total == len(raw) and 0 <= start < end <= total and raw[start:end].decode() in call["result"],
                "Exhaustion fixture source page was not completely visible in the actual Read result")
        spans.append((start, end))
    for start, end in sorted(spans):
        require(start <= covered, "Exhaustion fixture was copied with an unread source gap")
        covered = max(covered, end)
    require(covered == len(raw), "Exhaustion fixture was not fully read before its output")
    return {"complete_native_read_source_bytes": covered, "complete_native_read_pages": len(spans)}


def ledger_oracle(rows: list[dict[str, Any]], child_id: str, parent: str) -> dict[str, Any]:
    receipts: dict[str, dict[str, Any]] = {}
    for row in rows:
        require(row.get("agent_id") == child_id and row.get("parent_session_id") == parent,
                "Child usage journal changed its child/parent ownership")
        if row.get("receipt_id") and row.get("reported"):
            receipts[row["receipt_id"]] = row
    require(receipts, "Child has no real provider usage receipts")
    fields = ("input_tokens", "output_tokens", "cache_read_input_tokens", "cache_creation_input_tokens")
    require(all(type(row.get(field, 0)) is int and row.get(field, 0) >= 0 for row in receipts.values() for field in fields),
            "Child receipt contains invalid usage")
    totals = {field: sum(row.get(field, 0) for row in receipts.values()) for field in fields}
    require(sum(totals.values()) > 0, "Child provider receipts are empty")
    return {"unique_physical_stream_receipts": len(receipts), "raw_journal_records": len(rows),
            "usage_last_absolute_per_receipt": totals, "receipt_ids_sha256": stable_digest(sorted(receipts))}


def child_oracle(rows: list[dict[str, Any]], history: list[dict[str, Any]], usage: list[dict[str, Any]],
                 parent: str, name: str, *, cap: int, outcome: str, marker: str = "") -> dict[str, Any]:
    require(rows and rows[0].get("type") == "header", "Child journal has no immutable header")
    header = rows[0]["header"]
    require(header.get("sub_agent_of") == parent and header.get("teammate_name") == name and header.get("model") == MODEL,
            "Child did not retain its exact parent, Chinese name and model")
    executions = [row["execution"] for row in rows if row.get("type") == "execution"]
    require(executions and all(value == executions[0] for value in executions), "Child execution identity changed during recovery")
    execution = executions[0]
    assert_execution(execution, cap)
    terminals = [row["terminal"] for row in rows if row.get("type") == "terminal"]
    require(len(terminals) == 1, "One child output recovery produced zero or multiple durable terminals")
    terminal = terminals[0]
    attempts = terminal.get("continuationAttempts", 0)
    require(type(attempts) is int and 0 <= attempts <= 3, "Child exceeded its bounded output continuation count")
    text = terminal.get("output", "")
    require(isinstance(text, str) and bool(text.strip()),
            "Expected visible partial text was absent; reasoning-only length stop does not qualify for this recovery fixture")
    segments, triggers, part = [], [], []
    continuing = False
    for message in history:
        hints = [block.get("provider_hint", {}) for block in message.get("content", [])
                 if block.get("provider_hint", {}).get("metis.output_continuation")]
        if hints:
            require(message.get("role") == "user" and len(hints) == 1, "Output continuation lacks a unique host-owned user boundary")
            hint = hints[0]
            require(hint["metis.output_continuation"] in ("max_tokens", "length", "max_output_tokens"), "Automatic recovery was not caused by a real output-length stop")
            triggers.append(hint)
            require(hint.get("metis.output_continuation_attempt") == str(len(triggers)), "Persisted continuation attempts are not sequential")
            segments.append("".join(part)); part = []; continuing = True
        elif message.get("role") == "assistant":
            require(not continuing or not any(block.get("type") == "tool_use" for block in message.get("content", [])),
                    "A text-only output continuation replayed or executed a tool")
            part.append(message_text(message))
    segments.append("".join(part))
    require(len(triggers) == attempts and len(segments) == attempts + 1, "Durable history does not prove every claimed output-length continuation")
    # Canonical journal assistant blocks are already projected exactly as live
    # deltas (raw provider responses remain in trace). Never deduplicate again:
    # a legitimate new canonical segment may repeat some previous words.
    require(text == "".join(segments), "Cumulative terminal differs from canonical original/continued text deltas")
    if outcome == "default":
        require(terminal.get("status") == "completed" and attempts == 0 and terminal.get("stopHint") == "end_turn",
                "Default-budget child did not naturally finish without recovery")
    elif outcome == "recovered":
        require(terminal.get("status") == "completed" and 1 <= attempts <= 3 and terminal.get("stopHint") == "end_turn",
                "Low child cap did not genuinely recover to natural completion")
    else:
        require(outcome == "exhausted" and terminal.get("status") == "failed" and attempts == 3 and terminal.get("stopHint") in ("max_tokens", "length", "max_output_tokens"),
                "Long child did not honestly fail after exactly three output continuations")
    if marker:
        require(marker in text, "Completed child body lacks its actual final marker")
    receipt = ledger_oracle(usage, header["id"], parent)
    require(receipt["unique_physical_stream_receipts"] >= attempts + 1, "Recovery attempts lack independent real provider receipts")
    return {"agent_id": header["id"], "name": name, "status": terminal["status"], "stop_reason": terminal.get("stopHint"),
            "continuation_attempts": attempts, "explicit_child_output_cap": cap, "terminal_records": 1,
            "cumulative_output_characters": len(text), "cumulative_output_sha256": hashlib.sha256(text.encode()).hexdigest(),
            "assistant_text_segments": len(segments), "host_output_length_trigger_count": len(triggers),
            "actual_execution": safe_execution(execution), "partial_segments_preserved_in_order": True, **receipt}


def presentation_oracle(block: dict[str, Any], *, started: bool, status: str, attempts: int, reason: str) -> dict[str, Any]:
    view = block.get("presentation", {})
    child = view.get("subagent", {})
    require(view.get("metis.agent_started", False) is started, "Parent receipt misclassified startup versus an already-running child")
    require(child.get("status") == status and child.get("continuationAttempts", 0) == attempts and child.get("stopReason") == reason,
            "Parent receipt lost structured child terminal/recovery diagnostics")
    require(bool(block.get("is_error")) is (status == "failed"), "Parent receipt's error flag contradicts child status")
    if status == "failed":
        require(isinstance(child.get("diagnostic"), str) and child["diagnostic"], "Failed child receipt lacks a real diagnostic")
        if started:
            require(child.get("partialOutput") is True, "Started length-exhausted child lost its partial-output classification")
    return {"started": started, "status": status, "stop_reason": reason, "continuation_attempts": attempts,
            "partial_output": child.get("partialOutput", False), "diagnostic_present": bool(child.get("diagnostic"))}


def agent_result_blocks(history: list[dict[str, Any]]) -> list[tuple[dict[str, Any], dict[str, Any]]]:
    pending: dict[str, dict[str, Any]] = {}
    results = []
    for message in history:
        for block in message.get("content", []):
            if block.get("type") == "tool_use" and block.get("name") == "Agent":
                identity = block.get("trace_call_id") or block.get("tool_use_id") or block.get("id")
                pending[identity] = block.get("input", {})
            elif block.get("type") == "tool_result":
                identity = block.get("trace_call_id") or block.get("tool_use_id")
                if identity in pending:
                    results.append((pending.pop(identity), block))
    require(not pending, "Parent durable Agent call lacks its result")
    return results


def side_effect_verification_oracle(history: list[dict[str, Any]], verifier_history: list[dict[str, Any]],
                                    verifier_terminal: dict[str, Any], *, script: Path,
                                    effect: Path, source: Path) -> dict[str, Any]:
    calls = history_calls(history)
    read_errors = recoverable_read_errors(calls, source.parent)
    bash = [call for call in calls if call["tool"] == "Bash"]
    verify = [call for call in calls if call["tool"] == "Agent" and call["input"].get("subagent_type") == "verify"]
    require(len(bash) == 1 and not bash[0]["is_error"], "Side-effect fixture did not run exactly one successful Bash call")
    run_once_command_oracle(bash[0]["input"].get("command", ""), script)
    require(re.search(r"(?m)^ok[ \t]+fixture.local/effectcheck", bash[0]["result"]),
            "Fixture validation was not actually observed as a successful Go test")
    require(len(verify) == 1 and not verify[0]["is_error"], "Mutation fixture lacks exactly one successful independent verify Agent")
    require(verify[0]["result"].strip().splitlines()[-1] == "VERDICT: PASS", "Independent verifier did not return a real final PASS")
    boundaries = [index for index, message in enumerate(history) if any(
        block.get("provider_hint", {}).get("metis.output_continuation") for block in message.get("content", []))]
    require(boundaries and bash[0]["result_index"] < verify[0]["call_index"] and verify[0]["result_index"] < boundaries[0],
            "Required independent verification did not finish before text-only output recovery")
    require(verifier_terminal.get("status") == "completed" and verifier_terminal.get("stopHint") == "end_turn" and
            verifier_terminal.get("output", "").strip().splitlines()[-1] == "VERDICT: PASS",
            "Verifier journal contradicts the parent PASS receipt")
    verifier_calls = history_calls(verifier_history)
    verifier_errors = recoverable_read_errors(verifier_calls, source.parent)
    reads = [call for call in verifier_calls if not call["is_error"]]
    require(len(reads) == 2 and all(call["tool"] == "Read" and not call["is_error"] for call in reads) and
            all(any(fixture_read_path(call["input"].get("path", call["input"].get("file_path")), expected) for expected in (effect, source)) for call in reads) and
            all(any(fixture_read_path(call["input"].get("path", call["input"].get("file_path")), expected) for call in reads) for expected in (effect, source)),
            "Independent verifier did not actually read exactly the two private fixtures")
    require(all(call["tool"] in ("Bash", "Read", "Agent") and (not call["is_error"] or call["tool"] == "Read") for call in calls),
            "Copy fixture performed an unapproved or failed tool call")
    require(sum(call["tool"] == "Agent" for call in calls) == 1, "Copy fixture spawned an unrequested or duplicate verifier")
    source_reads = [call for call in calls if call["tool"] == "Read" and not call["is_error"]]
    require(source_reads and all(fixture_read_path(call["input"].get("path", call["input"].get("file_path")), source)
                for call in source_reads), "Copy child did not successfully Read its exact source")
    return {"append_bash_calls": 1, "actual_go_validation_pass": True, "independent_verify_calls": 1,
            "independent_verify_pass_before_recovery": True, "verifier_read_only_tool_calls": 2,
            "recoverable_read_errors": read_errors, "verifier_recoverable_read_errors": verifier_errors}


class SubagentOutputRecoveryLiveCheck(ContextIntegrityLiveCheck):
    def __init__(self, args: argparse.Namespace) -> None:
        super().__init__(args)
        self.children: dict[str, dict[str, Any]] = {}
        self.report.update({"mode": "subagent-output-recovery-live", "model": MODEL, "effort": "high",
            "boundary": "Real CLI run, private WebUI queries, terminal/usage/body/side-effect/cold-history checks. No raw upstream wire capture or native GUI launch; independent native acceptance must be recorded separately.",
            "real_cli_invocations": 0, "raw_upstream_wire_captured": False})

    def prepare(self) -> None:
        self.verify_freeze()
        require(self.input_sha == self.args.expected_sha256, "Supplied disposable CLI differs from source freeze")
        shutil.copyfile(self.source_binary, self.binary); self.binary.chmod(0o700)
        source = self.args.source_home.expanduser().resolve()
        route = load_selected_route(source, dict(os.environ))
        require(route["binding"][1] == "openai_chat", "Acceptance requires the existing OpenAI Chat route")
        self.source_config, self.source_config_sha = source / "config.toml", route["config_sha256"]
        self.source_auth = source / ".credentials/auth.json"
        self.source_auth_sha = digest(self.source_auth) if self.source_auth.is_file() else None
        self.key = route["key"]
        for path in (self.home, self.workspace, self.root / "user-home", self.home / "cache", self.home / "agents"):
            path.mkdir(mode=0o700)
        catalog = (source / "cache/models.json").read_bytes()
        (self.home / "cache/models.json").write_bytes(catalog)
        require(json.loads(catalog).get("deepseek", {}).get("models", {}).get(MODEL, {}).get("limit", {}).get("output", 0) >= DEFAULT_BUDGET,
                "Existing catalog lacks the exact DeepSeek model output capability")
        settings = dict(route["settings"])
        for field in ("api_key", "max_tokens", "models"):
            settings.pop(field, None)
        settings.update({"api_key_env": KEY_ENV, "model": MODEL, "timeout_seconds": self.args.query_timeout - 20})
        config = toml_table(["provider"], {"default": route["provider"]}) + "\n" + toml_table(["provider", "custom", route["provider"]], settings)
        config += "\n" + toml_table(["session"], {"dir": str(self.home / "sessions"), "skill_dir": str(self.home / "skills"),
            "max_iterations": 16, "auto_compact_threshold": route["threshold"], "auto_compact_minimum_tokens": 800000})
        config += "\n" + toml_table(["tools"], {"allowed": TOOLS}) + '\n[permission]\nmode = "fullAccess"\n'
        config += "\n" + toml_table(["agents"], {"max_agent_depth": 2, "max_concurrent_named": 8, "max_concurrent_anon": 8})
        (self.home / "config.toml").write_text(config); (self.home / "config.toml").chmod(0o600)
        def profile(name: str, tools: list[str], body: str, *, max_turns: int = 12) -> None:
            (self.home / "agents" / (name + ".md")).write_text("---\nname: " + name + "\nmodel: " + MODEL +
                "\neffort: high\ntools: [" + ", ".join(tools) + "]\npermission_mode: fullAccess\nmax_turns: " + str(max_turns) + "\nomit_claude_md: true\n---\n" + body + "\n")
        profile(PROFILE, TOOLS, "Execute only the explicitly requested disposable child/query test. Parent uses Agent/SubAgentOutput/SubAgentList only; never perform the child's filesystem tools yourself. Issue each requested call once, sequentially. After a failed child do not retry, resume, spawn a replacement or conceal its failure. Preserve the exact completion marker; never quote a child's long body in your final answer.")
        profile("plan", ["Read"], "You are the read-only Chinese plan child for an explicitly specified fixture. Produce the user's requested detailed complete plan; follow their section count and ending marker. Do not create tasks or delegate. Read only an exact supplied fixture when requested.")
        profile("output-copy", ["Read", "Bash", "Agent"], "You copy the exact supplied fixture, keeping every numbered line and ending marker. Before long output, perform the one authorized append-and-validation command, read the exact source completely, then dispatch the required independent verify child and wait for a genuine final VERDICT: PASS. Never bypass the contract or repeat a side effect. On an output continuation, continue from the first missing character/line; never restart an earlier block or add a summary.")
        profile("verify", ["Read"], "You are an independent read-only fixture verifier. You MUST actually Read both exact supplied private paths, check the effect file has exactly its one expected line and the copy source has its complete BEGIN/END and 64 numbered records. Never write, execute code, delegate or inspect another path. Report observed checks, then end with the exact line VERDICT: PASS only if all checks really passed; otherwise end VERDICT: FAIL.")
        profile("output-long", ["Read"], "Read the complete exact private source file in bounded source byte pages before answering. Then copy its exact bytes as text from BEGIN through END, preserving every random string and line break. Do not explain, summarize, use code fences or skip records. On output continuation resume at the first missing character, without repeating or using any tool.", max_turns=24)
        atomic_json(self.home / "desktop-preferences.json", {"defaultPreset": PROFILE, "rootTurnParallelism": 1,
                    "totalAgentParallelism": 4, "subagentParallelism": 3})
        self.environment = {name: os.environ[name] for name in ("PATH", "SHELL", "LANG", "LC_ALL") if name in os.environ}
        self.environment.update({"HOME": str(self.root / "user-home"), "METIS_HOME": str(self.home), "TMPDIR": str(self.root),
            "XDG_CACHE_HOME": str(self.root / "cache"), KEY_ENV: self.key, "METIS_AUTO_MEMORY": "0", "METIS_LEARNING_EVALUATION": "1",
            "METIS_NO_UPDATE_CHECK": "1", "METIS_RUN_CACHE": "0", "METIS_RUN_MAX_SECONDS": str(self.args.query_timeout),
            "METIS_TURN_MAX_SECONDS": str(self.args.query_timeout), "NO_PROXY": "127.0.0.1,localhost"})
        self.report.update({"binary_sha256": self.input_sha, "route": {"provider": route["provider"], "transport": "openai_chat",
            "endpoint": safe_endpoint(settings["base_url"]), "route_changed": False, "model": MODEL, "effort": "high"},
            "private_profile_overrides": {"max_tokens_configured": False, "per_model_output_budget_configured": False,
                "provider_timeout_seconds": settings["timeout_seconds"]}, "catalog_sha256": hashlib.sha256(catalog).hexdigest()})
        self.progress("private DeepSeek route prepared without max_tokens")

    def parent_turn(self, label: str, prompt: str, marker: str, *, sid: str | None = None, cli: bool = False) -> str:
        sid = sid or self.parent
        self.report["model_turn_submissions"] += 1
        started = time.monotonic()
        remaining = min(self.args.query_timeout, getattr(self, "case_deadline", float("inf")) - started)
        require(remaining > 0, "Private child case exceeded its finite time budget")
        if cli:
            self.report["real_cli_invocations"] += 1
            command = [str(self.binary), "run", "--bare", "--mode", "fullAccess", "--agent", PROFILE, "--tools", ",".join(TOOLS),
                "--model", MODEL, "--effort", "high", "--no-auth-wizard", "--no-markdown", "--max-iter", "8", "--session-id", sid, "--", prompt]
            out, err = self.root / "cli-parent.stdout", self.root / "cli-parent.stderr"
            with out.open("wb") as stdout, err.open("wb") as stderr:
                process = subprocess.Popen(command, cwd=self.workspace, env=self.environment, stdout=stdout, stderr=stderr, start_new_session=True)
                self.processes.append(process)
                try: code = process.wait(timeout=remaining)
                except subprocess.TimeoutExpired: raise AssertionError("Actual CLI query exceeded its bounded deadline") from None
            self.report["actual_cli"] = {"exit_code": code, "seconds": round(time.monotonic() - started, 3),
                "model": MODEL, "effort": "high", "interface": "run", "untrusted_help_fallback_used": False}
            require(code == 0, "Actual CLI query returned a nonzero exit status")
            text = out.read_text(errors="replace")
            receipt = {"transport": "actual CLI run", "exit_code": code, "argv_flags": "run --bare --model deepseek-flash --effort high --agent <private> --session-id <private>"}
        else:
            self.ensure_web_high(sid)
            code, body, _ = self.http("POST", "/api/turns", {"sessionId": sid, "input": prompt}, timeout=remaining)
            require(code == 200 and isinstance(body, dict) and body.get("sessionId") == sid and not body.get("stopped"), "Real parent turn failed or changed ownership")
            text = body.get("text", "")
            receipt = {"transport": "private WebUI backed by supplied CLI", "http_status": code}
        require(isinstance(text, str) and marker in text, "Real parent did not return its exact completion marker")
        self.report["checks"].append({"label": label, "session_id": sid, "seconds": round(time.monotonic() - started, 3),
            "response_sha256": hashlib.sha256(text.encode()).hexdigest(), **receipt})
        self.progress(label + " real parent query received")
        return text

    def ensure_web_high(self, sid: str) -> None:
        code, _, _ = self.http("POST", "/api/sessions/activate", {"id": sid})
        require(code == 200, "Private WebUI could not select the exact parent before setting effort")
        code, before, _ = self.http("GET", "/api/effort")
        require(code == 200 and before.get("supported") is True and before.get("model") == MODEL,
                "Private WebUI effort capability is unavailable or belongs to another model")
        code, selected, _ = self.http("POST", "/api/effort", {"effort": "high"})
        require(code == 200 and selected.get("effort") == "high", "Private WebUI did not accept explicit high effort")
        code, after, _ = self.http("GET", "/api/effort")
        require(code == 200 and after.get("effort") == "high" and after.get("model") == MODEL,
                "Private WebUI actual effort is not high after selection")
        code, canonical, _ = self.http("GET", "/api/sessions/" + quote(sid))
        require(code == 200 and canonical.get("session", {}).get("effort") == "high",
                "Canonical parent session did not persist the selected high effort")
        self.report.setdefault("web_effort_checks", []).append({"session_id": sid, "before": before.get("effort"),
            "explicit_selection": selected.get("effort"), "actual_after": after.get("effort"),
            "canonical_header_effort": canonical["session"]["effort"], "model": after.get("model")})

    def child(self, name: str, *, cap: int, outcome: str, marker: str = "") -> tuple[dict[str, Any], list[dict[str, Any]]]:
        matches = []
        for path in (self.home / "sessions/subagents").glob("*.jsonl"):
            rows = [json.loads(line) for line in path.read_text().splitlines() if line]
            header = rows[0].get("header", {}) if rows else {}
            if header.get("sub_agent_of") == self.parent and header.get("teammate_name") == name:
                matches.append((path, rows))
        require(len(matches) == 1, "Expected one durable child for the exact Chinese name and parent")
        path, rows = matches[0]
        usage_path = self.home / "sessions/subagent-usage" / (path.stem + ".jsonl")
        require(usage_path.is_file(), "Child usage journal is missing")
        usage = [json.loads(line) for line in usage_path.read_text().splitlines() if line]
        snapshots = history_snapshots(path)
        require(snapshots, "Child has no durable message checkpoint")
        proof = child_oracle(rows, snapshots[-1], usage, self.parent, name, cap=cap, outcome=outcome, marker=marker)
        terminal = next(row["terminal"] for row in rows if row.get("type") == "terminal")
        proof["provider_text_fidelity"] = self.provider_text_proof(path.stem, name, snapshots[-1], terminal)
        blocks = [(args, block) for args, block in agent_result_blocks(active_history(self.rows(self.parent))) if args.get("name") == name]
        require(len(blocks) == 1 and blocks[0][0].get("max_output_tokens", 0) == cap, "Parent did not make the requested exact Agent budget call")
        proof["parent_receipt"] = presentation_oracle(blocks[0][1], started=True, status=proof["status"],
            attempts=proof["continuation_attempts"], reason=proof["stop_reason"])
        proof["transcript_sha256"] = digest(path)
        proof["usage_journal_sha256"] = digest(usage_path)
        self.children[name] = proof
        return proof, snapshots[-1]

    def provider_text_proof(self, child_id: str, name: str, history: list[dict[str, Any]], terminal: dict[str, Any],
                            parent_history: list[dict[str, Any]] | None = None) -> dict[str, Any]:
        parent_history = parent_history if parent_history is not None else active_history(self.rows(self.parent))
        uses = [block for message in parent_history for block in message.get("content", [])
                if block.get("type") == "tool_use" and block.get("name") == "Agent" and block.get("input", {}).get("name") == name]
        require(len(uses) == 1, "Independent child text trace lacks its unique canonical parent Agent call")
        use = uses[0]
        path = self.home / "traces" / (self.parent + ".jsonl")
        require(path.is_file(), "Independent child emitted provider text trace file is absent")
        trace = [json.loads(line) for line in path.read_text().splitlines() if line]
        starts = [row for row in trace if row.get("kind") == "tool_start" and row.get("tool_name") == "Agent" and
            row.get("tool_use_id") == use.get("tool_use_id") and
            (not use.get("trace_call_id") or row.get("trace_call_id") == use["trace_call_id"])]
        require(len(starts) == 1 and starts[0].get("trace_invocation_id"), "Independent child trace has missing or ambiguous execution ownership")
        invocation = starts[0]["trace_invocation_id"]
        selected = []
        for row in sorted(trace, key=lambda value: value.get("sequence", 0)):
            if row.get("trace_invocation_id") != invocation: continue
            kind = row.get("kind")
            if kind == "text":
                selected.append({field: row[field] for field in ("kind", "sequence", "stream_id", "truncated", "text") if field in row})
            elif kind == "context" and row.get("source") == "recovery":
                match = re.search(r"<subagent_output_continuation [^\n]+?/>", row.get("text", ""))
                selected.append({"kind": kind, "sequence": row.get("sequence"), "source": "recovery", "text": match.group() if match else ""})
            elif kind == "loop_done":
                selected.append({"kind": kind, "sequence": row.get("sequence"), "text": row.get("text", "")})
        folder = self.output / "provider-text"
        folder.mkdir(exist_ok=True)
        retained = folder / (child_id + ".json")
        atomic_json(retained, {"agent_id": child_id, "trace_invocation_id": invocation,
            "contains_http_headers_or_credentials_or_thinking": False, "events": selected})
        proof = provider_text_oracle(selected, history, terminal)
        proof.update({"safe_emitted_text_evidence": str(retained), "safe_emitted_text_evidence_sha256": digest(retained)})
        return proof

    def default_case(self) -> None:
        self.parent = str(uuid.uuid4()); self.session_ids["recovery_cards"] = self.parent
        child_end, done = "DEFAULT_PLAN_END_" + secrets.token_hex(8), "DEFAULT_PARENT_DONE_" + secrets.token_hex(8)
        child_prompt = ("请只规划一套本地资料归档工具的完整设计，不执行也不读取其他文件。用中文给出80个编号模块，每个模块约100至120字，具体说明输入、输出、错误边界与验收方法；覆盖扫描、归档、版本、恢复、权限、事务、审计、CLI、桌面界面、测试。不要合并编号或省略模块，正文目标约9000字。最后单独输出 " + child_end)
        args = {"name": "中文长规划", "subagent_type": "plan", "model": MODEL, "timeout_seconds": self.args.query_timeout - 15, "prompt": child_prompt}
        prompt = "实际调用一次前台 Agent，参数为 " + json.dumps(args, ensure_ascii=False) + "。不要设置max_output_tokens，不得重新指派；子代理结束后仅报告其真实状态并输出 " + done
        self.parent_turn("default-budget-actual-cli", prompt, done, cli=True)
        proof, _ = self.child("中文长规划", cap=0, outcome="default", marker=child_end)
        require(proof["cumulative_output_characters"] >= 2000, "Default Chinese plan was not a real long response")
        proof["actual_output_tokens_exceeded_legacy_8192"] = proof["usage_last_absolute_per_receipt"]["output_tokens"] > 8192
        proof["budget_stress_boundary"] = "Actual output exceeded 8192" if proof["actual_output_tokens_exceeded_legacy_8192"] else "Natural long completion and default config only; upstream >8192 output not proven"
        self.ensure_web_high(self.parent)
        status = self.status(self.parent)
        require(status.get("contextDefaultOutputBudgetTokens") == DEFAULT_BUDGET, "Unconfigured DeepSeek route did not resolve the 64Ki default output budget")
        self.report["default_runtime_output_budget_tokens"] = status["contextDefaultOutputBudgetTokens"]
        self.report["default_child"] = proof

    def recovered_case(self) -> None:
        self.effect = self.workspace / "exactly-once.txt"
        self.effect_nonce = "EFFECT_" + secrets.token_hex(12)
        self.copy_end = "COPY_COMPLETE_" + secrets.token_hex(12)
        self.copy_start = "COPY_BEGIN_" + secrets.token_hex(12)
        lines = [f"步骤{index:03d}：保留核验记录。{secrets.token_hex(20)}" for index in range(1, 65)]
        self.copy_fixture = self.workspace / "long-copy.txt"
        self.copy_fixture.write_text(self.copy_start + "\n" + "\n".join(lines) + "\n" + self.copy_end)
        require(self.copy_fixture.stat().st_size < 8192, "Copy fixture must fit a complete native Read")
        validator = self.workspace / "effect-check"
        validator.mkdir()
        (validator / "go.mod").write_text("module fixture.local/effectcheck\n\ngo 1.25.0\n")
        (validator / "effect_test.go").write_text('package effectcheck\nimport("crypto/sha256";"fmt";"os";"testing")\n'
            'func TestExactlyOneAppendAndSourceIntegrity(t *testing.T){\n'
            'got,err:=os.ReadFile(' + json.dumps(str(self.effect)) + ');if err!=nil{t.Fatal(err)}\n'
            'if string(got)!=' + json.dumps(self.effect_nonce + "\n") + '{t.Fatal("effect is not exactly one expected line")}\n'
            'source,err:=os.ReadFile(' + json.dumps(str(self.copy_fixture)) + ');if err!=nil{t.Fatal(err)}\n'
            'if fmt.Sprintf("%x",sha256.Sum256(source))!=' + json.dumps(digest(self.copy_fixture)) + '{t.Fatal("copy source changed")}\n}\n')
        self.effect_script = self.workspace / "run-once.sh"
        self.effect_script.write_text("#!/bin/sh\nset -eu\nprintf '%s\\n' " + shlex.quote(self.effect_nonce) +
            " >> " + shlex.quote(str(self.effect)) + "\ncd " + shlex.quote(str(validator)) +
            "\nGOTOOLCHAIN=local GOWORK=off go test -count=1 -timeout=30s .\n")
        self.effect_script.chmod(0o700)
        self.effect_script_sha = digest(self.effect_script)
        command = shlex.quote(str(self.effect_script))
        verify_prompt = ('只读独立验收：实际调用Read，精确JSON为 ' + json.dumps({"path": str(self.effect)}) +
            "，确认只有一行且严格等于 " + self.effect_nonce + '；再实际调用Read，精确JSON为 ' +
            json.dumps({"path": str(self.copy_fixture)}) + '，确认BEGIN/END标记完整且步骤001至064均存在。'
            "原生Read的path必须为给定绝对路径，只使用path字段，不使用file_path或其他别名。不得执行代码或写文件。全部实际核对通过才以独立末行VERDICT: PASS结束，否则VERDICT: FAIL。")
        verifier_args = {"name": "独立副作用验收", "subagent_type": "verify", "model": MODEL,
                         "timeout_seconds": 120, "prompt": verify_prompt}
        child_prompt = ("这是已授权的一次性临时文件验收，必须按顺序完成所有前置真实验收，禁止跳过contract gate：先且仅先调用一次Bash执行精确命令：" + command +
            '；收到真实go test通过结果后只调用一次Read，精确JSON为 ' + json.dumps({"path": str(self.copy_fixture)}) +
            '。原生Read必须使用该绝对path，不使用相对路径、file_path或其他字段，不需要pwd或ls等额外Bash' +
            "；然后实际调用一次独立Agent，参数严格为 " + json.dumps(verifier_args, ensure_ascii=False) +
            "，等待它实际Read两文件并返回VERDICT: PASS。只有收到真实PASS后，才逐字输出完整copy文件，保留全部64个步骤及随机字符串，不加解释或代码围栏；输出续写时从缺失位置继续，绝不重新调用工具或重复副作用。")
        args = {"name": "有界续写完成", "subagent_type": "output-copy", "model": MODEL, "max_output_tokens": self.args.success_cap,
                "timeout_seconds": self.args.query_timeout - 15, "prompt": child_prompt}
        done = "RECOVERED_PARENT_DONE_" + secrets.token_hex(8)
        self.parent_turn("child-cap-recovery-with-one-side-effect", "实际仅调用一次前台Agent：" + json.dumps(args, ensure_ascii=False) + "。不替它执行工具、不重试；结束后报告真实状态并输出 " + done, done)
        proof, history = self.child("有界续写完成", cap=self.args.success_cap, outcome="recovered", marker=self.copy_end)
        require(self.effect.read_text().splitlines() == [self.effect_nonce], "Output continuation replayed or lost the actual physical side effect")
        calls = history_calls(history)
        nested = []
        for path in (self.home / "sessions/subagents").glob("*.jsonl"):
            rows = self.child_rows(path.stem)
            if rows and rows[0].get("header", {}).get("sub_agent_of") == proof["agent_id"]:
                nested.append((path, rows))
        require(len(nested) == 1, "Copy fixture lacks its unique independent verifier transcript")
        verify_path, verify_rows = nested[0]
        verify_history = history_snapshots(verify_path)[-1]
        verify_terminal = next(row["terminal"] for row in verify_rows if row.get("type") == "terminal")
        verify_usage = [json.loads(line) for line in (self.home / "sessions/subagent-usage" / (verify_path.stem + ".jsonl")).read_text().splitlines() if line]
        verify_proof = child_oracle(verify_rows, verify_history, verify_usage, proof["agent_id"], "独立副作用验收", cap=0, outcome="default", marker="VERDICT: PASS")
        verify_proof["provider_text_fidelity"] = self.provider_text_proof(verify_path.stem, "独立副作用验收", verify_history, verify_terminal, history)
        require(digest(self.effect_script) == self.effect_script_sha, "Authorized one-shot fixture script changed")
        verify_proof.update(side_effect_verification_oracle(history, verify_history, verify_terminal,
            script=self.effect_script, effect=self.effect, source=self.copy_fixture))
        verify_proof["one_shot_script_sha256"] = self.effect_script_sha
        self.report["independent_side_effect_verifier"] = verify_proof
        terminal = next(row["terminal"] for row in self.child_rows(proof["agent_id"]) if row.get("type") == "terminal")
        proof.update(source_copy_quality(terminal.get("output", ""), self.copy_fixture.read_text(), complete=True))
        proof.update({"side_effect_lines": 1, "physical_side_effect_sha256": digest(self.effect), "child_tool_calls": len(calls), "source_body_sha256": digest(self.copy_fixture)})
        self.report["recovered_child"] = proof

    def child_rows(self, child_id: str) -> list[dict[str, Any]]:
        path = self.home / "sessions/subagents" / (child_id + ".jsonl")
        return [json.loads(line) for line in path.read_text().splitlines() if line]

    def failure_snapshot(self) -> None:
        observed = []
        for path in (self.home / "sessions/subagents").glob("*.jsonl"):
            try:
                rows = [json.loads(line) for line in path.read_text().splitlines() if line]
                header = rows[0].get("header", {}) if rows else {}
                if header.get("sub_agent_of") not in self.session_ids.values(): continue
                terminals = [row["terminal"] for row in rows if row.get("type") == "terminal"]
                snapshots = history_snapshots(path)
                tools = safe_tool_diagnostics(snapshots[-1], getattr(self, "effect_script", None), self.root) if snapshots else []
                raw_proof: dict[str, Any] = {}
                if snapshots and len(terminals) == 1:
                    try: raw_proof = self.provider_text_proof(path.stem, header.get("teammate_name", ""), snapshots[-1], terminals[0])
                    except (AssertionError, OSError, ValueError, TypeError) as error:
                        raw_proof = {"evidence_failure_type": type(error).__name__,
                            "evidence_failure": str(error)[:300] if type(error) is AssertionError else "trace evidence unavailable"}
                observed.append({"agent_id": path.stem, "name": header.get("teammate_name"), "terminal_records": len(terminals),
                    "header": {field: header[field] for field in ("id", "sub_agent_of", "teammate_name", "model", "provider", "effort", "mode", "preset") if field in header},
                    "executions": [safe_execution(row["execution"]) for row in rows if row.get("type") == "execution"],
                    "actual_child_tools": tools,
                    "provider_text_fidelity": raw_proof,
                    "terminals": [{"status": t.get("status"), "stop_reason": t.get("stopHint"),
                        "continuation_attempts": t.get("continuationAttempts", 0), "partial_characters": len(t.get("output", "")),
                        "partial_sha256": hashlib.sha256(t.get("output", "").encode()).hexdigest(),
                        "error_present": bool(t.get("exitError")),
                        "copy_difference": copy_difference(t.get("output", ""), self.copy_fixture.read_text())
                            if header.get("teammate_name") == "有界续写完成" and hasattr(self, "copy_fixture") else None} for t in terminals]})
            except (OSError, ValueError, TypeError):
                observed.append({"agent_id": path.stem, "journal_unreadable": True})
        self.report["observed_child_terminal_snapshots"] = observed
        if hasattr(self, "effect_script"):
            self.report["observed_one_shot_fixture"] = {"expected_script_sha256": self.effect_script_sha,
                "actual_script_sha256": digest(self.effect_script) if self.effect_script.is_file() else None,
                "physical_effect_lines": len(self.effect.read_text().splitlines()) if self.effect.is_file() else 0}
        parent_inputs = []
        for parent in self.session_ids.values():
            try:
                for message in active_history(self.rows(parent)):
                    for block in message.get("content", []):
                        if block.get("type") == "tool_use" and block.get("name") == "Agent":
                            parent_inputs.append({"parent_session_id": parent, "input": safe_agent_input(block.get("input", {}))})
            except (AssertionError, OSError, ValueError, TypeError):
                parent_inputs.append({"parent_session_id": parent, "history_unreadable": True})
        self.report["observed_parent_agent_inputs"] = parent_inputs

    def exhausted_case(self) -> None:
        done = "EXHAUSTED_PARENT_DONE_" + secrets.token_hex(8)
        self.exhaust_fixture = self.workspace / "exhaustion-copy.txt"
        self.exhaust_source = exhaustion_fixture(secrets.token_hex(12))
        self.exhaust_fixture.write_text(self.exhaust_source)
        child_prompt = ('实际Read完整读取给定唯一绝对路径源文件，文件约64KiB。首次Read精确JSON为 ' +
            json.dumps({"path": str(self.exhaust_fixture), "byte_offset": 0, "byte_limit": 8192}) +
            '，然后严格按返回的next byte_offset继续Read同一绝对path，仅改变byte_offset，直到完整读到EOF。'
            "原生Read字段为path、byte_offset、byte_limit；path必须为给定绝对路径，不要使用相对路径或file_path，不要混用行offset/limit。全部Read完成后，从文件BEGIN标记开始逐字复制完整原文直到END标记，保留全部512条编号、中文、40位随机十六进制和换行。不得加标题、代码围栏、说明、省略号或提前结束；被自动续写时紧接最后一个已输出字符继续，不重复任何正文，不再次调用工具。")
        args = {"name": "续写额度耗尽", "subagent_type": "output-long", "model": MODEL, "max_output_tokens": self.args.exhaustion_cap,
                "timeout_seconds": self.args.query_timeout - 15, "prompt": child_prompt}
        self.parent_turn("length-budget-exhaustion", "实际调用一次前台Agent：" + json.dumps(args, ensure_ascii=False) + "。这是预期失败验收，禁止替换、恢复或重试；保留真实失败原因和partial，仅输出简短真实状态以及 " + done, done)
        proof, history = self.child("续写额度耗尽", cap=self.args.exhaustion_cap, outcome="exhausted")
        calls = history_calls(history)
        read_errors = recoverable_read_errors(calls, self.workspace)
        successful_reads = [call for call in calls if not call["is_error"]]
        require(successful_reads and all(call["tool"] == "Read" and
            fixture_read_path(call["input"].get("path", call["input"].get("file_path")), self.exhaust_fixture) for call in successful_reads),
            "Exhaustion copy did not actually Read only its private source")
        terminal = next(row["terminal"] for row in self.child_rows(proof["agent_id"]) if row.get("type") == "terminal")
        proof.update(complete_read_coverage_oracle(successful_reads, self.exhaust_source))
        first_copy = next(index for index, message in enumerate(history) if message.get("role") == "assistant" and self.exhaust_source.splitlines()[0] in message_text(message))
        require(all(call["result_index"] < first_copy for call in calls), "Exhaustion fixture began copying before all source pages were visible")
        proof.update(source_copy_quality(terminal["output"], self.exhaust_source, complete=False))
        require(self.exhaust_fixture.read_text() == self.exhaust_source, "Read-only exhaustion source changed")
        proof["read_only_source_tool_calls"] = len(calls)
        proof["recoverable_read_errors"] = read_errors
        self.report["exhausted_child"] = proof

    def startup_case(self) -> None:
        done = "STARTUP_REJECTION_DONE_" + secrets.token_hex(8)
        args = {"name": "真正启动失败", "subagent_type": "plan", "cwd": str(self.workspace / "intentionally-absent-directory"),
                "prompt": "Read-only fixture; reply ACK if started."}
        before = set((self.home / "sessions/subagents").glob("*.jsonl"))
        self.parent_turn("genuine-prelaunch-rejection", "仅调用一次Agent尝试 " + json.dumps(args, ensure_ascii=False) + "。目录故意不存在，错误是预期结果；不要创建目录、重试或改cwd。报告原始分类并输出 " + done, done)
        matches = [(args, block) for args, block in agent_result_blocks(active_history(self.rows(self.parent))) if args.get("name") == "真正启动失败"]
        require(len(matches) == 1 and matches[0][1].get("is_error"), "Expected actual prelaunch Agent rejection is absent")
        presentation = matches[0][1].get("presentation", {})
        require(presentation.get("metis.agent_started", False) is False, "Prelaunch rejection was falsely classified as a started child")
        require(set((self.home / "sessions/subagents").glob("*.jsonl")) == before, "Prelaunch rejection created an executing child transcript")
        self.report["startup_rejection"] = {"agent_started": False, "parent_error_receipt": True, "created_child_transcript": False,
            "presentation_sha256": stable_digest(presentation)}

    def history_case(self) -> None:
        baseline = {path: digest(path) for folder in ("subagents", "subagent-usage") for path in (self.home / "sessions" / folder).glob("*.jsonl")}
        self.stop_server(); self.start_server("cold-history")
        code, _, _ = self.http("POST", "/api/sessions/activate", {"id": self.parent}); require(code == 200, "Cold parent activation failed")
        child = self.children["有界续写完成"]
        done = "COLD_HISTORY_DONE_" + secrets.token_hex(8)
        prompt = f"只读调用SubAgentOutput两次：先agent_id={child['agent_id']}，再name=有界续写完成。不要Agent恢复、启动或写文件。返回真实status和load_state，最后输出 {done}"
        self.parent_turn("cold-history-id-and-chinese-name", prompt, done)
        calls = history_calls(active_history(self.rows(self.parent)))
        outputs = [call for call in calls if call["tool"] == "SubAgentOutput"][-2:]
        require(len(outputs) == 2 and all(not call["is_error"] and "load_state=not_loaded" in call["result"] and "status=completed" in call["result"] for call in outputs),
                "Restart/Roster-empty history lookup did not resolve both ID and Chinese name")
        foreign = self.new_session("foreign_parent")
        marker = "FOREIGN_QUERY_DONE_" + secrets.token_hex(8)
        self.parent_turn("cross-parent-history-denied", f"只调用一次SubAgentOutput(agent_id={child['agent_id']})。这是跨父会话隔离负例，错误为预期；不要重试、列出其他任务或调用Agent。报告真实拒绝并输出 {marker}", marker, sid=foreign)
        foreign_calls = history_calls(active_history(self.rows(foreign)))
        require(len(foreign_calls) == 1 and foreign_calls[0]["tool"] == "SubAgentOutput" and foreign_calls[0]["is_error"], "Cross-parent lookup was not rejected")
        body = foreign_calls[0]["result"]
        require(re.search(r"denied|not.found|not_found|归属|拒绝|不存在", body, re.I) and self.copy_end not in body and self.effect_nonce not in body,
                "Cross-parent denial leaked child output or lacked an ownership boundary")
        require(all(path.is_file() and digest(path) == sha for path, sha in baseline.items()), "Read-only cold lookup changed child transcripts or usage")
        code, _, _ = self.http("POST", "/api/sessions/activate", {"id": self.parent}); require(code == 200, "Fixture parent reactivation failed")
        self.session_id = self.parent
        self.report["cold_history"] = {"id_and_chinese_name_resolved": True, "roster_load_state": "not_loaded",
            "cross_parent_denied": True, "original_child_files_unchanged": True,
            "same_name_ambiguity_boundary": "Independent formal regression; this live flow intentionally uses distinct names."}

    def hold(self) -> None:
        if not self.args.hold_seconds: return
        stop = self.args.stop_file.resolve() if self.args.stop_file else self.output / "stop"
        require(not stop.exists(), "Hold stop file already exists")
        atomic_json(self.output / "live-server.json", {"url": "http://127.0.0.1:" + str(self.port), "port": self.port,
            "pid": self.server.pid, "sessionIds": self.session_ids, "parentSessionId": self.parent,
            "childIds": {name: child["agent_id"] for name, child in self.children.items()}, "privateHome": str(self.home),
            "workspace": str(self.workspace), "stop_file": str(stop), "hold_seconds": self.args.hold_seconds,
            "native_gui_started_by_harness": False, "contains_credentials": False,
            "inspection": "Completed, recovery-exhausted and genuine prelaunch failure cards; expand child details and reload. Native shell may read this private HOME, but this script does not claim pixel acceptance."})
        self.progress("verified private child history held for independent GUI acceptance")
        deadline = time.monotonic() + self.args.hold_seconds
        while time.monotonic() < deadline and not stop.exists():
            require(self.server.poll() is None, "Held private server exited")
            time.sleep(.25)
        self.report["hold"] = {"stop_file_observed": stop.exists(), "requested_seconds": self.args.hold_seconds}

    def run(self) -> int:
        try:
            self.prepare(); self.start_server("first")
            for name, case in (("default_cli", self.default_case), ("recovered_once_effect", self.recovered_case),
                               ("exhausted", self.exhausted_case), ("startup_rejection", self.startup_case), ("cold_history", self.history_case)):
                self.run_case(name, case)
            self.report["children"] = self.children
            self.hold(); self.verify_freeze(); self.report["passed"] = True
        except BaseException as error:
            self.report["failure_type"] = type(error).__name__
            if type(error) is AssertionError: self.report["failure"] = str(error)[:300]
        finally:
            try: self.failure_snapshot()
            except BaseException as error: self.report["failure_snapshot_error_type"] = type(error).__name__
            if hasattr(self, "source_auth"):
                try: self.report["original_auth_unchanged"] = (digest(self.source_auth) if self.source_auth.is_file() else None) == self.source_auth_sha
                except OSError: self.report["original_auth_unchanged"] = False
            try: self.cleanup()
            except BaseException as error: self.report["cleanup_failure_type"] = type(error).__name__; self.report["passed"] = False
            if self.report.get("original_config_unchanged") is False or self.report.get("original_auth_unchanged") is False: self.report["passed"] = False
            self.report.update({"children": self.children, "session_ids": self.session_ids, "finished_utc": utc_now()})
            atomic_json(self.output / "summary.json", self.report)
        print(json.dumps({"passed": self.report["passed"], "summary": str(self.output / "summary.json")}), flush=True)
        return 0 if self.report["passed"] else 1


def arguments(argv: list[str] | None = None) -> argparse.Namespace:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--binary", type=Path, required=True); parser.add_argument("--destroy-input", action="store_true", required=True)
    parser.add_argument("--expected-sha256", required=True); parser.add_argument("--source-freeze", type=Path, required=True)
    parser.add_argument("--repo", type=Path, default=Path(__file__).resolve().parents[2])
    parser.add_argument("--source-home", type=Path, default=Path(os.environ.get("METIS_HOME", str(Path.home() / ".metis"))))
    parser.add_argument("--output", type=Path, required=True); parser.add_argument("--query-timeout", type=int, default=360)
    parser.add_argument("--success-cap", type=int, default=2048); parser.add_argument("--exhaustion-cap", type=int, default=4096)
    parser.add_argument("--hold-seconds", type=int, default=0); parser.add_argument("--stop-file", type=Path)
    args = parser.parse_args(argv)
    parser.error("Invalid SHA256") if not re.fullmatch(r"[a-f0-9]{64}", args.expected_sha256) else None
    parser.error("query-timeout must be 60..360; child caps 256..4096; hold 0..3600") if not (60 <= args.query_timeout <= 360 and
        256 <= args.success_cap <= 4096 and 256 <= args.exhaustion_cap <= 4096 and 0 <= args.hold_seconds <= 3600) else None
    return args


def main(argv: list[str] | None = None) -> int:
    args = arguments(argv); binary = disposable_binary(args.binary); sha = digest(binary)
    try: return SubagentOutputRecoveryLiveCheck(args).run()
    except (AssertionError, OSError, ValueError) as error:
        print(json.dumps({"passed": False, "startup_failure_type": type(error).__name__}), file=sys.stderr); return 1
    finally:
        if binary.exists():
            require(digest(binary) == sha, "Refusing to destroy replaced disposable input CLI"); binary.unlink()


if __name__ == "__main__":
    def interrupted(_signum: int, _frame: Any) -> None: raise InterruptedError("Private subagent acceptance interrupted")
    signal.signal(signal.SIGTERM, interrupted)
    raise SystemExit(main())
