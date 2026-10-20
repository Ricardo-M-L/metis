#!/usr/bin/env python3
"""Fresh-CLI acceptance for queue visibility, host actions and quality rollback.

Only an explicitly supplied disposable CLI is executed. The original selected
provider/model/endpoint is retained. SQLite is opened read-only and queried only
by exact reserved physical-request markers and gateway IDs; missing receipts or
charges are never invented.
Every live outcome destroys owned processes, private credentials and both CLI
executables before retaining sanitized evidence. --self-test is entirely offline.
"""
from __future__ import annotations

import argparse
from collections import Counter
import hashlib
import ipaddress
import json
import os
from pathlib import Path
import re
import shutil
import signal
import sqlite3
import tempfile
import time
from typing import Any
from urllib.parse import quote, urlsplit, urlunsplit
import uuid

from job_handoff_dispatch_live_check import (JobHandoffDispatchLiveCheck,
    assert_frozen_binding, assert_no_manual_dispatch, business_receipt, go_json_sha)
from persistent_agent_live_check import (assert_skill_invoked, atomic_json,
    digest, json_sha, require, tool_calls, trace_proof, utc_now)


AUTHORITY = "cliproxy-gui/persisted-usage"
VERIFIER = "coordinator-host-checks"
NATIVE_GATEWAY_FIELDS = ("event_key", "request_id", "provider", "model", "alias", "user_agent", "failed", "canceled",
    "input_tokens", "output_tokens", "cache_read_tokens", "cache_creation_tokens")


def waiting_view(record: dict[str, Any], reason: str) -> dict[str, Any]:
    require(record.get("state") == "pending" and record.get("attempts") == 0 and not record.get("business"),
        "Waiting message was claimed/reserved or given a fabricated business outcome")
    value = record.get("admission", {})
    require(value.get("state") == "waiting" and value.get("waiting_reason") == reason and
        value.get("last_checked_at") and value.get("recovery_hint"), "Missing durable queue reason, timestamp or recovery hint")
    return {key: value[key] for key in ("state", "waiting_reason", "last_checked_at", "recovery_hint")}


def physical_usage(run: dict[str, Any], item_ids: set[str], provider: str, model: str) -> dict[str, Any]:
    rows = [row for row in run.get("usage_requests", []) if row.get("item_id") in item_ids]
    require(rows and len({row.get("client_request_id") for row in rows}) == len(rows), "Missing/duplicate physical request ledger")
    total: Counter = Counter()
    for row in rows:
        require(row.get("provider") == provider and row.get("model") == model and row.get("client_request_id"), "Physical request changed its route")
        require(row.get("status") in ("completed", "reconciled") and row.get("usage_reported") is True and
            row.get("response_status_code") == 200, "Physical provider request failed or has unresolved usage")
        usage = row.get("usage", {})
        require(set(usage) == {"input", "output", "cache_read", "cache_write"} and
            all(type(number) is int and number >= 0 for number in usage.values()), "Physical usage is not complete absolute token data")
        total.update(usage)
    require(sum(total.values()) > 0, "Real provider requests reported no tokens")
    return {"physical_requests": len(rows), "tokens": dict(total), "client_request_ids": [row["client_request_id"] for row in rows]}


def exact_gateway_row(connection: sqlite3.Connection, request: dict[str, Any]) -> dict[str, Any] | None:
    identifier = request.get("provider_request_id", "")
    client_id = request.get("client_request_id", "")
    if not re.fullmatch(r"[a-f0-9]{32}", client_id):
        return None
    require(isinstance(identifier, str) and len(identifier) <= 256 and not any(ord(c) < 32 for c in identifier), "Invalid persisted gateway identifier")
    marker = "metis-usage/" + client_id
    deadline = time.monotonic() + 5
    connection.set_progress_handler(lambda: int(time.monotonic() > deadline), 10000)
    rows = connection.execute("SELECT event_key,request_id,provider,model,alias,user_agent,failed,canceled,input_tokens,output_tokens,cache_read_tokens,cache_creation_tokens FROM usage_events WHERE instr(' ' || coalesce(user_agent,'') || ' ', ' ' || ? || ' ') > 0 LIMIT 2", (marker,)).fetchall()
    require(len(rows) <= 1, "Physical request marker is ambiguous in the gateway")
    if not rows:
        return None
    row = dict(rows[0])
    require(row["request_id"] and (not identifier or row["request_id"] == identifier) and
        row["user_agent"].split().count(marker) == 1 and request["model"] in (row["model"], row["alias"]), "Gateway receipt belongs to another exact physical request/model")
    if row["failed"] != 0 or row["canceled"] != 0:
        return None
    require(row["provider"] in ("codex", "openai") and row["event_key"], "Unknown native token semantics")
    values = [row[name] for name in ("input_tokens", "output_tokens", "cache_read_tokens", "cache_creation_tokens")]
    require(all(type(value) is int and value >= 0 for value in values) and values[0] >= values[2] + values[3], "Invalid native token counts")
    row["exclusive_usage"] = {"input": values[0] - values[2] - values[3], "output": values[1], "cache_read": values[2], "cache_write": values[3]}
    return row


def inference_endpoint(base: str, endpoint_sha: str) -> str:
    candidates = [base, *[base.rstrip("/") + suffix for suffix in
        ("/responses", "/chat/completions", "/messages", "/v1/responses", "/v1/chat/completions", "/v1/messages")]]
    matches = []
    for candidate in candidates:
        parts = urlsplit(candidate)
        canonical = urlunsplit((parts.scheme.lower(), parts.netloc.lower(), parts.path, "", ""))
        if hashlib.sha256(canonical.encode()).hexdigest() == endpoint_sha:
            matches.append(candidate)
    require(len(set(matches)) == 1, "Cannot match actual ledger endpoint to the unchanged selected route")
    return matches[0]


def quality_observation(state: dict[str, Any], candidate: str, baseline: str, *, failed: bool) -> dict[str, Any]:
    quality = state.get("quality", {})
    records = [row for row in quality.get("records", []) if row.get("binding", {}).get("version_sha256") == candidate and row.get("outcome")]
    require(records, "Managed artifact has no host-derived business outcome")
    for row in records:
        outcome = row["outcome"]
        require(outcome.get("verifier") == VERIFIER and re.fullmatch(r"[a-f0-9]{64}", outcome.get("invocation_sha256", "")) and
            re.fullmatch(r"[a-f0-9]{64}", outcome.get("evidence_sha256", "")), "Quality record lacks independent host evidence")
    if failed:
        decisions = [row for row in records if row["outcome"].get("passed") is False and row.get("decision", {}).get("action") == "rolled_back"]
        require(len(decisions) == 1 and state.get("active_version") == baseline and quality.get("paused") is True,
            "Host-rejected actual managed artifact did not automatically roll back")
        decision = decisions[0]["decision"]
        require(decision.get("from_version") == candidate and decision.get("to_version") == baseline and decision.get("failures") == 1,
            "Rollback was not tied to the invoked candidate and its previous activation")
    else:
        require(all(row["outcome"].get("passed") is True and row.get("decision", {}).get("action") == "observed" for row in records) and
            state.get("active_version") == candidate and not quality.get("paused") and not state.get("suspended"),
            "Successful independent host checks caused a spurious rollback/pause")
    return {"active_version": state["active_version"], "activation_epoch": state["activation_epoch"], "quality": quality}


class DurableOperationsLiveCheck(JobHandoffDispatchLiveCheck):
    def __init__(self, args: argparse.Namespace) -> None:
        super().__init__(args)
        self.skill_name = "durable-business-quality"
        self.report["actual_supplier_charge_known"] = False
        self.report["supplier_charge_boundary"] = "Existing collector records tokens only; actual supplier invoice/charges require a separate authoritative API."
        self.report["boundary"] = ("Real selected-route requests, resident job pickup, same-sender queue status, Desktop management CLI actions and independent host-check quality rollback. "
            "Native gateway tokens require exact request-ID matches; gateway token records never establish supplier USD charges. No OS reboot, live-kill, multi-hour soak or arbitrary business API exactly-once claim.")

    def spawn(self, args: list[str], label: str):
        if args[:2] == ["coordinator", "serve"] and hasattr(self, "billing_path"):
            args = [*args, "--usage-billing-config", str(self.billing_path)]
        return super().spawn(args, label)

    def prepare(self) -> None:
        super().prepare()
        self.profile("dispatch-orders", ["Read", "Write", "Bash", "Skill"], "Follow only the current durable assignment. "
            "Invoke Skill action invoke with name " + self.skill_name + " exactly once before reading/writing its task files. "
            "Then Read the named input and existing output, run the supplied readonly calculator exactly once, Write its exact JSON output, and Read it back. "
            "For the explicitly negative fixture Write its supplied intentionally wrong JSON unchanged. End with DONE.")

    def configure_managed_skill(self) -> None:
        nonce = uuid.uuid4().hex[:12]
        self.skill_marker = "MANAGED_CANDIDATE_" + nonce
        expected = ["EVALUATION_ONE_" + nonce, "EVALUATION_TWO_" + nonce]
        def body(marker: str) -> str:
            return ("---\nname: " + self.skill_name + "\ndescription: Host-managed finite business acceptance fixture.\n---\n" + marker + "\n"
                "For EVAL_ONE return exactly " + expected[0] + ". For EVAL_TWO return exactly " + expected[1] + ". "
                "For BUSINESS tasks follow the current durable assignment's Read/calculator/Write/Read instructions. "
                "The explicitly negative fixture must retain its intentionally wrong JSON so independent host acceptance can reject it. "
                "Do not alter the host policy, checker, input data or other tasks.\n")
        active = self.home / "skills" / (self.skill_name + ".md")
        active.write_text(body("MANAGED_BASELINE_" + nonce))
        self.baseline = digest(active)
        candidate_file = self.root / "managed-candidate.md"
        candidate_file.write_text(body(self.skill_marker))
        suite_file = self.root / "managed-promotion-suite.json"
        atomic_json(suite_file, {"name": "durable-fixed-suite-" + nonce, "cases": [
            {"id": "one", "prompt": "Apply EVAL_ONE and return only its prescribed text.", "expected": expected[0]},
            {"id": "two", "prompt": "Apply EVAL_TWO and return only its prescribed text.", "expected": expected[1]}],
            "max_tokens": 100000, "max_duration_seconds": int(self.args.query_timeout)})
        candidate, _ = self.command(["learning", "stage", "--name", self.skill_name, "--file", str(candidate_file)], "stage-managed-candidate")
        self.candidate = candidate["id"]
        require(candidate.get("parent") == self.baseline and digest(active) == self.baseline, "Staging replaced the active baseline")
        self.configure_controller("skill", self.skill_name, suite_file, "configure-managed-candidate-evaluation")
        evaluation, tick = self.controller_evaluate("skill", self.skill_name, self.candidate, "promoted", "evaluate-and-promote-real-managed-skill")
        self.assert_evaluation(evaluation, self.skill_name, self.candidate, self.baseline)
        self.command(["learning", "quality-configure", "--kind", "skill", "--name", self.skill_name,
            "--verifier", VERIFIER, "--window", "2", "--minimum-samples", "1", "--max-failure-rate", "0", "--action", "rollback"], "configure-host-derived-quality-policy")
        self.record("managed candidate evaluated by real queries and activated before business tasks", baseline=self.baseline,
            candidate=self.candidate, evaluation=evaluation, automatic_promotion=tick)

    def configure_native_billing(self) -> None:
        gateway = self.args.gateway_usage_db
        if gateway is None:
            known = Path.home() / "Library/Application Support/com.cpa.gui/usage-records/usage.db"
            gateway = known if known.is_file() else None
        parts = urlsplit(self.settings["base_url"])
        try:
            loopback = parts.scheme == "http" and ipaddress.ip_address(parts.hostname or "").is_loopback
        except ValueError:
            loopback = False
        if gateway is None or not loopback:
            self.report["native_billing"] = {"verified": False, "reason": "existing_loopback_gateway_database_unavailable", "actual_usd_known": False}
            return
        gateway = gateway.expanduser().resolve()
        require(gateway.is_file() and not gateway.is_symlink(), "Gateway ledger must be an existing regular file")
        self.gateway_db = gateway
        requests = self.read_run().get("usage_requests", [])
        endpoints = {row.get("endpoint_sha256") for row in requests}
        require(len(endpoints) == 1 and None not in endpoints, "Bootstrap did not retain one exact inference endpoint identity")
        endpoint = inference_endpoint(self.settings["base_url"], next(iter(endpoints)))
        self.billing_path = self.root / "native-billing.json"
        atomic_json(self.billing_path, {"provider": "cliproxy_gui", "ledger_provider": self.provider,
            "inference_endpoint": endpoint, "usage_db": str(gateway), "sqlite_executable": "/usr/bin/sqlite3"})
        self.billing_path.chmod(0o600)
        self.report["native_billing"] = {"verified": False, "configured": True, "database_read_only": True,
            "endpoint_sha256": next(iter(endpoints)), "actual_usd_known": False, "supplier_invoice_verified": False}

    def management(self, action: str | None = None) -> dict[str, Any]:
        command = ["operations", "snapshot", "--cwd", str(self.workspace), "--json"]
        if action:
            request = self.root / (action + "-management.json")
            atomic_json(request, {"action": action, "id": self.dispatch_job["id"]})
            request.chmod(0o600)
            command = ["operations", "action", "--cwd", str(self.workspace), "--request-file", str(request), "--json"]
        value, _ = self.command(command, "desktop-management-" + (action or "snapshot"))
        if action:
            require(value == {"ok": True}, "Desktop management action did not acknowledge host completion")
        return value

    def quality_state(self, *, failed: bool) -> dict[str, Any]:
        path = self.home / "skills" / ".learning" / self.skill_name / "state.json"
        deadline = time.monotonic() + 30
        while True:
            state = json.loads(path.read_text())
            try:
                observation = quality_observation(state, self.candidate, self.baseline, failed=failed)
                break
            except AssertionError:
                if time.monotonic() >= deadline:
                    raise
                time.sleep(.1)
        public, _ = self.command(["learning", "quality", "--name", self.skill_name], "quality-" + ("automatic-rollback" if failed else "successful-observation"))
        require(public["active_version"] == observation["active_version"] and public["quality"] == observation["quality"], "Quality CLI differs from durable host feedback")
        return observation

    def verify_business(self, case: dict[str, Any], record: dict[str, Any], before: dict[str, int], *, failed: bool = False) -> dict[str, Any]:
        receipt = business_receipt(record, "rejected" if failed else "completed")
        assignment = receipt["assignment"]
        run = self.read_run()
        items = {item["id"]: item for item in run["items"]}
        implementation, verifier = items[assignment["work_item_id"]], items[assignment["verification_item_id"]]
        require(assignment["workspace"] == str(self.workspace) and assignment["run_id"] == self.run_id and
            verifier.get("depends_on") == [implementation["id"]] and verifier.get("phase") == "verification", "Business receipt lost its independent verifier binding")
        binding = assert_frozen_binding(implementation, self.dispatch_job)
        require(implementation.get("attempts") == verifier.get("attempts") == 1 and record.get("state") == "acknowledged" and not record.get("admission"), "Business work duplicated attempts, lacks delivery ACK, or retained waiting status")
        actual = json.loads((self.workspace / case["output"]).read_text())
        acceptance = verifier.get("acceptance", {})
        require(acceptance.get("status") == ("failed" if failed else "passed") and acceptance.get("checks"), "Host did not persist concrete independent acceptance")
        require((actual != case["expected"]) if failed else (actual == case["expected"]), "Independent artifact arithmetic disagrees with expected outcome")
        if not failed:
            require(receipt["acceptance"]["evidence_sha256"] == go_json_sha(acceptance), "Receipt does not hash the actual host acceptance")
            require({a["path"]: a["sha256"] for a in receipt["artifacts"]} == {case["output"]: digest(self.workspace / case["output"])}, "Receipt artifact hash differs from actual output")
        else:
            require(any(check.get("exit_code") != 0 for check in acceptance["checks"]), "Negative artifact was not rejected by a real host process")
        proof, rows = self.real_proof(before, case["name"] + "-real-provider", required_tools=("Skill", "Read", "Write", "Bash"))
        skill = assert_skill_invoked(rows, self.skill_name, self.skill_marker)
        require(digest(self.calculator) == self.calculator_sha and digest(self.checker) == self.checker_sha and
            digest(self.workspace / case["input"]) == case["source_sha256"], "Model changed immutable host evidence")
        return {"message_id": record["id"], "business": receipt, "frozen_binding": binding, "actual_skill": skill,
            "provider_proof": proof, "physical_usage": physical_usage(run, {implementation["id"], verifier["id"]}, self.provider, self.model)}

    def paused_sender_and_resume(self) -> None:
        self.await_idle()
        self.stop_services()
        self.configure_native_billing()
        self.management("pause_job")
        service = self.start_service("paused-operations")
        case = self.new_case("paused-quality-business")
        case["prompt"] = "BUSINESS: Invoke Skill " + self.skill_name + " first. " + case["prompt"]
        session = str(uuid.uuid4())
        task = {"id": case["name"], "objective": case["prompt"], "artifacts": [{"path": case["input"], "sha256": case["source_sha256"]}],
            "deliverables": [case["output"]], "acceptance": ["Output must equal immutable host integer arithmetic for all orders."]}
        arguments = {"to": "job:" + self.dispatch_job["id"], "body": case["prompt"], "idempotency_key": case["name"], "task": task}
        before_sender = self.trace_snapshot()
        prompt = "Call MessageTeammate exactly once using these exact arguments, then return SENT. Use no other tool: " + json.dumps(arguments)
        self.command(["run", "--bare", "--tools", "MessageTeammate", "--session-id", session, prompt], "real-sender-queues-paused-task", json_output=False)
        sender_proof, sender_rows = self.real_proof(before_sender, "real-paused-sender", required_tools=("MessageTeammate",))
        calls = tool_calls(sender_rows, "MessageTeammate")
        require(len(calls) == 1 and all(calls[0]["arguments"].get(key) == value for key, value in arguments.items()), "Sender did not make the exact durable send")
        sent = json.loads(calls[0]["result"])
        deadline = time.monotonic() + 15
        while True:
            record = self.message(sent["id"])
            if record.get("admission", {}).get("waiting_reason") == "job_paused":
                break
            require(time.monotonic() < deadline, "Paused message has no durable waiting reason")
            time.sleep(.1)
        expected_wait = waiting_view(record, "job_paused")
        self.no_dispatch(case)
        snapshot = self.management()
        row = next(row for row in snapshot["handoffs"] if row["id"] == sent["id"])
        require(set(row.get("admission", {})) == set(expected_wait) and row["admission"]["waiting_reason"] == "job_paused" and
            row["admission"]["recovery_hint"] == expected_wait["recovery_hint"], "Desktop snapshot did not show the safe durable wait")
        transcript = self.home / "sessions" / (session + ".jsonl")
        prefix = transcript.read_bytes()
        before_status = self.trace_snapshot()
        status_arguments = {"operation": "status", "message_id": sent["id"]}
        self.command(["run", "--bare", "--tools", "MessageTeammate", "--resume", session,
            "Read your queued message status exactly once with MessageTeammate and these arguments; do not send or use other tools: " + json.dumps(status_arguments)],
            "same-sender-reads-paused-reason", json_output=False)
        status_proof, rows = self.real_proof(before_status, "same-sender-wait-status", required_tools=("MessageTeammate",))
        statuses = tool_calls(rows, "MessageTeammate")
        require(len(statuses) == 1 and statuses[0]["arguments"] == status_arguments and status_proof["session_ids"] == [session] and transcript.read_bytes().startswith(prefix), "Queue status did not use the original sender session")
        status = json.loads(statuses[0]["result"])
        require(status.get("admission", {}).get("waiting_reason") == "job_paused" and
            status["admission"].get("recovery_hint") == expected_wait["recovery_hint"] and status["admission"].get("last_checked_at"), "Sender status omitted its wait reason")
        require(set(status["admission"]) == set(expected_wait) and not any(name in status for name in ("token", "consumer", "workspace")), "Model status leaked host authority")
        self.stop_services()
        self.start_service("paused-after-restart")
        self.no_dispatch(case)
        restarted_wait = waiting_view(self.message(sent["id"]), "job_paused")
        require(restarted_wait["waiting_reason"] == expected_wait["waiting_reason"], "Restart lost the durable hold")
        before_business = self.trace_snapshot()
        self.management("resume_job")
        completed = self.await_business(sent["id"], {"completed"})
        result = self.verify_business(case, completed, before_business)
        self.await_idle()
        quality = self.quality_state(failed=False)
        after = self.management()
        updated = next(row for row in after["handoffs"] if row["id"] == sent["id"])
        require(not updated.get("admission") and updated["business"]["acceptance_passed"] is True, "Desktop snapshot kept obsolete waiting state")
        self.record("same sender sees paused reason; Desktop host action resumes automatic work with successful quality feedback", waiting=expected_wait,
            restarted_wait=restarted_wait, sender_query=sender_proof, status_query=status_proof, actual_sender_call=calls[0], actual_status_call=statuses[0],
            business=result, quality=quality, desktop_management_contract_verified=True, screenshot_verified=False,
            service_id=service["service_id"], manual_dispatch_used=False)

    def failed_host_check_rolls_back(self) -> None:
        self.await_idle()
        case = self.new_case("negative-quality-business", wrong=True)
        case["prompt"] = "BUSINESS: Invoke Skill " + self.skill_name + " first. " + case["prompt"]
        before = self.trace_snapshot()
        sent = self.send_case(case, label="queue-negative-quality-task")
        rejected = self.await_business(sent["id"], {"rejected"})
        result = self.verify_business(case, rejected, before, failed=True)
        self.await_idle()
        quality = self.quality_state(failed=True)
        snapshot = self.management()
        managed = next(row for row in snapshot["learning"] if row["name"] == self.skill_name and row["kind"] == "skill")
        require(managed["active_version"] == self.baseline and managed["quality"]["paused"] is True, "Desktop view missed automatic rollback")
        self.record("actual managed Skill plus independently rejected artifact automatically rolls back its activation", business=result,
            quality=quality, model_claim_is_not_acceptance=True, manually_submitted_feedback=False)

    def verify_native_billing(self) -> None:
        if not hasattr(self, "gateway_db"):
            raise AssertionError("Native gateway billing remains unverified: no existing compatible read-only source")
        deadline = time.monotonic() + self.args.billing_wait
        matches = []
        pending = []
        while True:
            run = self.read_run()
            requests = run.get("usage_requests", [])
            require(isinstance(requests, list) and requests, "No physical graph requests to reconcile")
            identities = {"client_request_id": [request.get("client_request_id") for request in requests],
                "provider_request_id": [request.get("provider_request_id") for request in requests if request.get("provider_request_id") not in (None, "")],
                "response_id": [request.get("response_id") for request in requests if request.get("response_id") not in (None, "")],
                "receipt_id": [receipt.get("receipt_id") for request in requests for receipt in request.get("receipts", [])]}
            for name, values in identities.items():
                require(all(isinstance(value, str) and value for value in values) and len(set(values)) == len(values), "Invalid/duplicate graph " + name)
            require(all(re.fullmatch(r"[a-f0-9]{32}", value) for value in identities["client_request_id"]), "Invalid reserved physical client ID")
            matches, pending = [], []
            uri = "file:" + quote(str(self.gateway_db), safe="/") + "?mode=ro"
            with sqlite3.connect(uri, uri=True, timeout=2) as connection:
                connection.row_factory = sqlite3.Row
                for request in requests:
                    row = exact_gateway_row(connection, request)
                    receipts = [value for value in request.get("receipts", []) if value.get("authority") == AUTHORITY]
                    if row is None or not receipts:
                        require(not receipts if not request.get("provider_request_id") else True, "Gateway receipt invented without a gateway identifier")
                        pending.append({"client_request_id": request["client_request_id"], "provider_request_id": request.get("provider_request_id"),
                            "observed_gateway_request_id": row["request_id"] if row else None,
                            "reason": "exact_marker_not_observed" if row is None else "native_receipt_not_yet_imported"})
                        continue
                    receipt = receipts[-1]
                    require(request.get("status") == "reconciled" and request.get("usage_reported") is True and
                        type(request.get("response_status_code")) is int and request["response_status_code"] == 200,
                        "Native receipt did not settle the physical request")
                    require(request.get("actual_usd") is None and receipt.get("actual_usd") is None, "Token-only collector invented a monetary charge")
                    require(request.get("usage") == row["exclusive_usage"] == receipt.get("usage") and
                        request.get("provider_request_id") == receipt.get("provider_request_id") == row["request_id"] and
                        receipt.get("client_request_id") == request["client_request_id"] and
                        all(receipt.get(field) == request.get(field) for field in ("provider", "endpoint_sha256", "model", "response_id")),
                        "Native receipt does not match exact request ID, route and tokens")
                    evidence = self.home / "coordinator-usage-receipts" / self.run_id / "provider-evidence" / (receipt["evidence_sha256"] + ".json")
                    require(evidence.is_file() and digest(evidence) == receipt["evidence_sha256"], "Native token receipt has no exact durable raw evidence")
                    raw = json.loads(evidence.read_text())
                    require(isinstance(raw, list) and len(raw) == 1 and isinstance(raw[0], dict) and set(raw[0]) == set(NATIVE_GATEWAY_FIELDS) and
                        all(type(raw[0][field]) is type(row[field]) and raw[0][field] == row[field] for field in NATIVE_GATEWAY_FIELDS),
                        "Saved gateway evidence differs from the exact persisted GUI row")
                    matches.append({"client_request_id": request["client_request_id"], "gateway_request_id": row["request_id"], "gateway_event_key": row["event_key"],
                        "exact_user_agent_marker": "metis-usage/" + request["client_request_id"],
                        "usage": row["exclusive_usage"], "receipt_id": receipt["receipt_id"], "evidence_sha256": receipt["evidence_sha256"], "actual_usd_known": False})
            for field in ("gateway_request_id", "gateway_event_key", "receipt_id", "evidence_sha256"):
                require(len({row[field] for row in matches}) == len(matches), "Multiple graph requests reused native " + field)
            if matches and (not pending or time.monotonic() >= deadline):
                break
            if time.monotonic() >= deadline:
                break
            time.sleep(.5)
        self.report["native_billing"].update({"verified": bool(matches) and not pending, "exact_matches": matches, "pending": pending,
            "matched_requests": len(matches), "graph_requests": len(requests), "unconfirmed_requests": len(pending),
            "compared_raw_fields": list(NATIVE_GATEWAY_FIELDS), "graph_identities_unique": True,
            "actual_usd_known": False, "supplier_invoice_verified": False})
        require(matches and not pending, "Native billing is unconfirmed for " + str(len(pending)) + " requests; missing IDs/receipts remain unknown")
        self.record("native_gateway_token_reconciliation", **self.report["native_billing"])

    def copy_evidence(self) -> None:
        super().copy_evidence()
        manifest_path = self.output / "evidence-manifest.json"
        manifest = json.loads(manifest_path.read_text())
        for directory in ("learning-business", "coordinator-usage-receipts"):
            for source in (self.home / directory).rglob("*.json"):
                if source.is_symlink() or not source.is_file():
                    continue
                target = self.output / "evidence" / directory / source.relative_to(self.home / directory)
                target.parent.mkdir(parents=True, exist_ok=True, mode=0o700)
                target.write_text(self.redact(source.read_text()))
                target.chmod(0o600)
                manifest.append({"path": str(target.relative_to(self.output)), "sha256": digest(target)})
        atomic_json(manifest_path, manifest)

    def run(self) -> int:
        signal.setitimer(signal.ITIMER_REAL, self.args.case_timeout)
        try:
            self.prepare()
            from learning_fixture_acceptance import check_workflow_fixture
            check_workflow_fixture(self)
            self.configure_managed_skill()
            self.create_dispatch_job()
            self.bootstrap()
            self.paused_sender_and_resume()
            self.failed_host_check_rolls_back()
            self.verify_native_billing()
            from desktop_operations_acceptance import check_desktop_operations
            check_desktop_operations(self)
            assert_no_manual_dispatch(self.report["commands"])
            require(not any(tuple(row["arguments"][:2]) in (("learning", "feedback"), ("learning", "feedback-bind"), ("learning", "rollback")) for row in self.report["commands"]), "Harness manually supplied quality outcome or rollback")
            self.report["passed"] = True
        except BaseException as error:
            self.report.update({"failure_type": type(error).__name__, "failure": self.redact(str(error))})
        finally:
            signal.setitimer(signal.ITIMER_REAL, 0)
            for label, action in (("cleanup", self.cleanup), ("evidence_copy", self.copy_evidence), ("source_freeze", self.verify_freeze)):
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
            self.progress("finished", passed=self.report["passed"], failure=self.report.get("failure"))
        print(json.dumps({"passed": self.report["passed"], "checks": len(self.report["checks"]), "report": str(self.output / "verification-report.json"), "failure": self.report.get("failure")}), flush=True)
        return 0 if self.report["passed"] else 1


def self_test() -> dict[str, Any]:
    checked = []
    def rejects(action, label):
        try:
            action()
        except AssertionError:
            checked.append(label)
            return
        raise AssertionError("Offline oracle accepted " + label)
    record = {"state": "pending", "attempts": 0, "admission": {"state": "waiting", "waiting_reason": "job_paused", "last_checked_at": "2026-10-16T00:00:00Z", "recovery_hint": "Resume the saved job."}}
    require(waiting_view(record, "job_paused")["waiting_reason"] == "job_paused", "Valid wait rejected")
    rejects(lambda: waiting_view({**record, "business": {"state": "completed"}}, "job_paused"), "delivery/business confusion")
    request = {"client_request_id": "c" * 32, "provider_request_id": "gateway-real-id", "model": "selected-model"}
    with tempfile.TemporaryDirectory(prefix="metis-operations-oracle-") as temporary:
        database = Path(temporary) / "offline-fixture.db"
        with sqlite3.connect(database) as fixture:
            fixture.execute("CREATE TABLE usage_events(event_key TEXT,request_id TEXT,provider TEXT,model TEXT,alias TEXT,user_agent TEXT,failed INTEGER,canceled INTEGER,input_tokens INTEGER,output_tokens INTEGER,cache_read_tokens INTEGER,cache_creation_tokens INTEGER)")
            fixture.execute("INSERT INTO usage_events VALUES(?,?,?,?,?,?,?,?,?,?,?,?)", ("event-1", "gateway-real-id", "codex", "selected-model", "selected-model", "Metis/fixture metis-usage/" + "c" * 32, 0, 0, 100, 7, 25, 5))
        with sqlite3.connect("file:" + str(database) + "?mode=ro", uri=True) as readonly:
            readonly.row_factory = sqlite3.Row
            value = exact_gateway_row(readonly, request)
            require(value["exclusive_usage"] == {"input": 70, "output": 7, "cache_read": 25, "cache_write": 5}, "Inclusive tokens counted cache twice")
            require(exact_gateway_row(readonly, {"model": "selected-model"}) is None and exact_gateway_row(readonly, {**request, "client_request_id": "d" * 32}) is None, "Missing physical identity guessed by time/model")
            require(exact_gateway_row(readonly, {**request, "provider_request_id": ""})["request_id"] == "gateway-real-id", "Exact reserved marker could not recover real gateway identifier")
            rejects(lambda: exact_gateway_row(readonly, {**request, "provider_request_id": "other-gateway-id"}), "gateway header identifier mismatch")
            rejects(lambda: exact_gateway_row(readonly, {**request, "model": "different-model"}), "gateway model mismatch")
        # Exercise the actual billing verifier, not a parallel reimplementation
        # of its assertions. These fixtures make no CLI/provider calls.
        second_client = "d" * 32
        with sqlite3.connect(database) as fixture:
            fixture.execute("INSERT INTO usage_events VALUES(?,?,?,?,?,?,?,?,?,?,?,?)", ("event-2", "gateway-second-id", "codex", "selected-model", "selected-model", "Metis/fixture metis-usage/" + second_client, 0, 0, 120, 9, 20, 0))
        def exercise_billing(*, raw_change=None, mutate_run=None):
            home = Path(temporary) / ("billing-evidence-" + uuid.uuid4().hex)
            evidence_dir = home / "coordinator-usage-receipts" / "oracle-run" / "provider-evidence"
            evidence_dir.mkdir(parents=True, mode=0o700)
            requests = []
            with sqlite3.connect("file:" + str(database) + "?mode=ro", uri=True) as readonly:
                readonly.row_factory = sqlite3.Row
                for index, identity in enumerate((request, {"client_request_id": second_client, "provider_request_id": "gateway-second-id", "model": "selected-model"})):
                    row = exact_gateway_row(readonly, identity)
                    usage = row.pop("exclusive_usage")
                    if index == 0 and raw_change:
                        row.update(raw_change)
                    raw = json.dumps([row]).encode()
                    evidence_sha = hashlib.sha256(raw).hexdigest()
                    (evidence_dir / (evidence_sha + ".json")).write_bytes(raw)
                    receipt = {"authority": AUTHORITY, "receipt_id": "receipt-" + str(index), "client_request_id": identity["client_request_id"],
                        "provider_request_id": identity["provider_request_id"], "response_id": "response-" + str(index), "usage": usage,
                        "provider": "selected-provider", "model": "selected-model", "endpoint_sha256": "a" * 64, "evidence_sha256": evidence_sha}
                    requests.append({**identity, "provider": "selected-provider", "endpoint_sha256": "a" * 64, "response_id": receipt["response_id"],
                        "status": "reconciled", "usage_reported": True, "response_status_code": 200, "usage": usage, "receipts": [receipt]})
            run = {"usage_requests": requests}
            if mutate_run:
                mutate_run(run)
            checker = object.__new__(DurableOperationsLiveCheck)
            checker.gateway_db, checker.home, checker.run_id = database, home, "oracle-run"
            checker.args = argparse.Namespace(billing_wait=0)
            checker.report = {"native_billing": {"endpoint_sha256": "a" * 64},
                "original_route": {"provider": "selected-provider", "model": "selected-model"}}
            checker.read_run = lambda: run
            checker.record = lambda *_args, **_kwargs: None
            checker.verify_native_billing()
            require(checker.report["native_billing"]["verified"] is True, "Valid synthetic native evidence was not confirmed")
        exercise_billing()
        for field, wrong in (("input_tokens", 101), ("output_tokens", 8), ("cache_read_tokens", 24), ("cache_creation_tokens", 6),
                             ("user_agent", "Changed/1 metis-usage/" + "c" * 32), ("provider", "openai"), ("model", "wrong-model"),
                             ("alias", "wrong-alias"), ("event_key", "wrong-event"), ("failed", 1), ("canceled", 1)):
            rejects(lambda field=field, wrong=wrong: exercise_billing(raw_change={field: wrong}), "rehash raw evidence with wrong " + field)
        rejects(lambda: exercise_billing(mutate_run=lambda run: run["usage_requests"].append(json.loads(json.dumps(run["usage_requests"][0])))), "duplicate graph physical request")
        for field in ("provider_request_id", "response_id"):
            rejects(lambda field=field: exercise_billing(mutate_run=lambda run: run["usage_requests"][1].update({field: run["usage_requests"][0][field]})), "duplicate graph " + field)
        rejects(lambda: exercise_billing(mutate_run=lambda run: run["usage_requests"][1]["receipts"][0].update(receipt_id=run["usage_requests"][0]["receipts"][0]["receipt_id"])), "duplicate graph receipt ID")
        rejects(lambda: exercise_billing(mutate_run=lambda run: run["usage_requests"][0].update(actual_usd=0)), "unknown ledger dollars changed to zero")
        rejects(lambda: exercise_billing(mutate_run=lambda run: run["usage_requests"][0]["receipts"][0].update(actual_usd=0)), "unknown receipt dollars changed to zero")
    endpoint = "http://127.0.0.1:8317/v1/responses"
    require(inference_endpoint("http://127.0.0.1:8317/v1", hashlib.sha256(endpoint.encode()).hexdigest()) == endpoint, "Exact endpoint was not matched")
    rejects(lambda: inference_endpoint("http://127.0.0.1:8317/v1", "a" * 64), "endpoint substitution")
    outcome = {"passed": True, "verifier": VERIFIER, "invocation_sha256": "a" * 64, "evidence_sha256": "b" * 64}
    state = {"active_version": "candidate", "activation_epoch": 2, "quality": {"paused": False, "records": [{"binding": {"version_sha256": "candidate"}, "outcome": outcome, "decision": {"action": "observed"}}]}}
    quality_observation(state, "candidate", "baseline", failed=False)
    rejects(lambda: quality_observation(state, "candidate", "baseline", failed=True), "model success counted as host rollback")
    failed = {"active_version": "baseline", "activation_epoch": 3, "quality": {"paused": True, "records": [{"binding": {"version_sha256": "candidate"}, "outcome": {**outcome, "passed": False}, "decision": {"action": "rolled_back", "from_version": "candidate", "to_version": "baseline", "failures": 1}}]}}
    quality_observation(failed, "candidate", "baseline", failed=True)
    checked.extend(["waiting fields", "read-only exact request matching", "exclusive cache accounting", "missing physical identity remains unknown", "successful quality has no rollback", "host failure exact-version rollback"])
    return {"passed": True, "offline_only": True, "real_provider_verified": False, "checks": checked}


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("--repo", type=Path, default=Path(__file__).resolve().parents[2])
    parser.add_argument("--metis", "--binary", dest="binary", type=Path)
    parser.add_argument("--expected-sha256")
    parser.add_argument("--source-freeze", type=Path)
    parser.add_argument("--output", type=Path)
    parser.add_argument("--destroy-input", action="store_true")
    parser.add_argument("--gateway-usage-db", type=Path)
    parser.add_argument("--billing-wait", type=float, default=120)
    parser.add_argument("--query-timeout", type=float, default=600)
    parser.add_argument("--case-timeout", type=float, default=3600)
    parser.add_argument("--self-test", action="store_true")
    args = parser.parse_args()
    os.umask(0o077)
    if args.self_test:
        print(json.dumps(self_test(), ensure_ascii=False, indent=2))
        return 0
    require(args.binary and args.expected_sha256 and args.source_freeze and args.output and args.destroy_input, "Live mode requires a fresh CLI, exact SHA, source freeze, new output and --destroy-input")
    require(re.fullmatch(r"[a-f0-9]{64}", args.expected_sha256), "Expected binary SHA must be exact SHA-256")
    require(0 < args.query_timeout <= 600 and 0 < args.case_timeout <= 3600 and 0 <= args.billing_wait <= 300, "Live deadlines must remain bounded")
    def interrupted(signum, _frame):
        raise KeyboardInterrupt("Harness interrupted by " + signal.Signals(signum).name)
    def timed_out(_signum, _frame):
        raise TimeoutError("Bounded durable operations acceptance exceeded its deadline")
    signal.signal(signal.SIGINT, interrupted)
    signal.signal(signal.SIGTERM, interrupted)
    signal.signal(signal.SIGALRM, timed_out)
    try:
        return DurableOperationsLiveCheck(args).run()
    finally:
        # Constructor failures happen before run's finally block. An exact
        # parent-supplied disposable input must still be removed on that path.
        binary = args.binary.expanduser().resolve()
        if args.destroy_input and binary.exists():
            require(digest(binary) == args.expected_sha256, "Refusing to delete a replaced supplied executable")
            binary.unlink()


if __name__ == "__main__":
    raise SystemExit(main())
