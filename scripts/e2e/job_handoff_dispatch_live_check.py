#!/usr/bin/env python3
"""Real CLI acceptance for automatic job dispatch and business receipts.

The orchestrator supplies a freshly built, source-frozen disposable executable.
This script never builds a CLI, changes the user's configuration, chooses a
different provider/model/endpoint, or calls job run/coordinator add to complete
an assignment. Offline fixtures prove only the host oracles, never a provider.
Every live outcome first destroys owned processes, executables and credential
copies, then retains redacted evidence. Business completion requires persisted
host acceptance; a durable mailbox delivery ACK is deliberately insufficient.
"""
from __future__ import annotations

import argparse
from collections import Counter
import copy
import hashlib
import http.client
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
import tempfile
import threading
import time
from typing import Any
from urllib.parse import urlsplit
import uuid

from persistent_agent_live_check import (PersistentAgentLiveCheck, atomic_json,
    digest, json_sha, require, tool_calls, trace_proof, utc_now)
from persistent_agent_soak_live_check import (private_header_timeout,
    sanitized_transport_log)


def aggregate(source: dict[str, Any]) -> dict[str, Any]:
    require(isinstance(source.get("orders"), list) and source["orders"], "Orders must be a nonempty list")
    require(all(type(row.get("amount")) is int and isinstance(row.get("group"), str) for row in source["orders"]),
        "Orders require integer amounts and string groups")
    groups = sorted({row["group"] for row in source["orders"]})
    return {"task": source["task"], "nonce": source["nonce"], "count": len(source["orders"]),
        "amount": sum(row["amount"] for row in source["orders"]),
        "groups": {name: {"count": sum(row["group"] == name for row in source["orders"]),
            "amount": sum(row["amount"] for row in source["orders"] if row["group"] == name)} for name in groups}}


def go_json_sha(value: Any) -> str:
    """Persisted struct field order plus Go's default HTML string escaping."""
    encoded = json.dumps(value, ensure_ascii=False, separators=(",", ":"))
    for source, escaped in (("<", "\\u003c"), (">", "\\u003e"), ("&", "\\u0026"), ("\u2028", "\\u2028"), ("\u2029", "\\u2029")):
        encoded = encoded.replace(source, escaped)
    return hashlib.sha256(encoded.encode()).hexdigest()


def business_receipt(record: dict[str, Any], expected_state: str) -> dict[str, Any]:
    """Never infer business completion from mailbox ACK or assistant prose."""
    receipt = record.get("business")
    require(isinstance(receipt, dict), "Message has no separate durable business receipt")
    require(receipt.get("state") == expected_state, "Unexpected business receipt state: " + str(receipt.get("state")))
    assignment = receipt.get("assignment", {})
    require(all(assignment.get(field) for field in ("job_id", "workspace", "run_id", "work_item_id", "verification_item_id", "config_sha256")),
        "Business receipt lacks its frozen principal/run/item binding")
    require(type(receipt.get("source_revision")) is int and receipt["source_revision"] > 0, "Business receipt lacks a positive graph revision")
    if expected_state == "completed":
        require(record.get("state") == "acknowledged", "Business completion precedes durable delivery ACK")
        acceptance = receipt.get("acceptance", {})
        require(acceptance.get("passed") is True and acceptance.get("source") == "host" and
            re.fullmatch(r"[a-f0-9]{64}", acceptance.get("evidence_sha256", "")), "Business completion has no host acceptance proof")
        require(acceptance.get("checks") and all(check.get("exit_code") == 0 and
            re.fullmatch(r"[a-f0-9]{64}", check.get("output_sha256", "")) for check in acceptance["checks"]),
            "Business completion has no successful concrete host checks")
        require(receipt.get("output") and receipt.get("output_sha256") == hashlib.sha256(receipt["output"].encode()).hexdigest(),
            "Business completion output lacks its exact hash")
        require(receipt.get("completed_at") not in (None, "", "0001-01-01T00:00:00Z"), "Business completion lacks a durable completion timestamp")
    elif expected_state in ("failed", "rejected"):
        require(receipt.get("acceptance", {}).get("passed") is not True, "Failed/rejected receipt incorrectly reports acceptance")
        require(receipt.get("failure", {}).get("code"), "Failed/rejected receipt lacks durable failure evidence")
        require(receipt.get("completed_at") in (None, "", "0001-01-01T00:00:00Z"), "Failed/rejected receipt has a completion timestamp")
    return receipt


def assert_frozen_binding(item: dict[str, Any], job: dict[str, Any]) -> dict[str, Any]:
    binding = item.get("principal_binding")
    require(isinstance(binding, dict), "Automatically dispatched item has no principal binding")
    require(binding.get("job_id") == job["id"] and binding.get("config_sha256") == job["config_sha256"],
        "Automatically dispatched item changed its principal identity/configuration")
    require(binding.get("definition") == job["definition"], "Automatically dispatched item did not freeze the job definition")
    return {"job_id": job["id"], "config_sha256": job["config_sha256"], "definition_sha256": json_sha(binding["definition"])}


def assert_physical_usage(run: dict[str, Any], item_ids: set[str], provider: str, model: str) -> dict[str, Any]:
    rows = [row for row in run.get("usage_requests", []) if row.get("item_id") in item_ids]
    require(rows, "Automatically dispatched work has no physical provider request records")
    ids = [row.get("client_request_id") for row in rows]
    require(all(ids) and len(ids) == len(set(ids)), "Physical request IDs are missing or duplicated")
    totals = Counter()
    for row in rows:
        require(row.get("provider") == provider and row.get("model") == model, "Physical request used a different provider/model")
        require(row.get("status") == "completed" and row.get("usage_reported") is True and row.get("response_status_code") == 200,
            "Physical provider request is incomplete, failed or has unknown usage")
        usage = row.get("usage", {})
        require(set(usage) == {"input", "output", "cache_read", "cache_write"} and
            all(type(value) is int and value >= 0 for value in usage.values()), "Invalid physical absolute usage")
        totals.update(usage)
    require(sum(totals.values()) > 0, "No nonzero reported provider usage")
    return {"physical_requests": len(rows), "tokens": dict(totals), "client_request_ids": ids,
        "all_usage_reported": True, "unknown_usage_requests": 0,
        "estimated_usd": sum(row.get("estimated_usd", 0) for row in rows), "supplier_invoice_verified": False}


def assert_no_manual_dispatch(commands: list[dict[str, Any]]) -> None:
    forbidden = (("job", "run"), ("job", "rotate"), ("coordinator", "add"), ("coordinator", "run"))
    require(not any(tuple(row.get("arguments", [])[:2]) in forbidden for row in commands),
        "Harness manually dispatched work instead of proving automatic job pickup")


def assert_sender_tool_receipt(result: dict[str, Any], actual: dict[str, Any], sender_scope: str) -> dict[str, Any]:
    require(result.get("id") == actual["id"] and result.get("sender_scope") == sender_scope == actual.get("sender_scope") and
        result.get("from") == actual.get("from") and result.get("to") == actual.get("to"), "Model status result lost its original sender ownership")
    require(result.get("state") == "acknowledged" and result.get("checkpoint_sha256") == actual.get("checkpoint_sha256"),
        "Model status result did not observe the durable delivery checkpoint")
    summary, business = result.get("business", {}), actual["business"]
    require(summary.get("state") == "completed", "Sender model cannot observe host-completed business state")
    for key in ("source_revision", "output", "output_sha256", "artifacts"):
        require(summary.get(key) == business.get(key), "Sender model business summary differs from persisted " + key)
    require(summary.get("assignment") == {key: value for key, value in business["assignment"].items() if key != "workspace"},
        "Sender model summary lost its frozen assignment or leaked host workspace")
    acceptance = summary.get("acceptance", {})
    for key in ("source", "passed", "evidence_sha256"):
        require(acceptance.get(key) == business["acceptance"].get(key), "Sender model acceptance summary differs from persisted " + key)
    expected_checks = [{"exit_code": value["exit_code"], "output_sha256": value["output_sha256"]} for value in business["acceptance"]["checks"]]
    actual_checks = [{"exit_code": value.get("exit_code"), "output_sha256": value.get("output_sha256")} for value in acceptance.get("checks", [])]
    require(actual_checks == expected_checks, "Sender model did not receive the exact independent host check result hashes")
    require("token" not in result and "consumer" not in result and all("command" not in value for value in acceptance.get("checks", [])),
        "Model-facing status leaked worker authority or host shell commands")
    return {"sender_scope": sender_scope, "message_id": result["id"], "business_state": summary["state"],
        "output_sha256": summary["output_sha256"], "acceptance_evidence_sha256": acceptance["evidence_sha256"],
        "host_check_output_sha256": [value["output_sha256"] for value in expected_checks]}


WORKSPACE_RULES = ("This is a private synthetic order-data workspace, not a Git repository. "
    "Do not inspect .git or search unrelated directories. Do not delegate or change host scripts. "
    "Each Bash call must be one simple foreground readonly Python helper invocation without &&, "
    "pipes, redirects, heredocs, command substitution, or compound shell flow. ")

BOOTSTRAP_GOAL = (WORKSPACE_RULES +
    "The initial research, synthesis, implementation, and verification scaffold has one finite objective: "
    "make bootstrap-output.json contain exactly the JSON object in bootstrap-input.json, then verify equality. "
    "Research: Read those two files and report their current contents. "
    "Synthesis: plan this exact input-to-output copy and its equality check. "
    "Implementation: Read both files, Write the exact input object into the output, and Read it back. "
    "Verification: independently Read both files and verify their JSON objects are equal; the configured host checker must pass. "
    "If a preceding stage already made the copy, preserve or reapply that correct copy and finish the current stage's own check. "
    "No additional assignment, project code, rewrite scope, or external input is required to complete these four stages. "
    "The bootstrap objective is complete when these two JSON objects match and host acceptance passes.")

CALCULATOR = '''#!/usr/bin/env python3
import hashlib,json,sys
from pathlib import Path
p=Path(sys.argv[1]); data=json.loads(p.read_text()); orders=data["orders"]
groups=sorted({r["group"] for r in orders})
print(json.dumps({"task":data["task"],"nonce":data["nonce"],"count":len(orders),"amount":sum(r["amount"] for r in orders),"groups":{g:{"count":sum(r["group"]==g for r in orders),"amount":sum(r["amount"] for r in orders if r["group"]==g)} for g in groups}},sort_keys=True))
'''

CHECKER = '''#!/usr/bin/env python3
import hashlib,json,sys
from pathlib import Path
plan=json.loads(Path(sys.argv[1]).read_text()); task=sys.argv[2]
root=Path(plan["workspace"])
tasks=[task]
if task=="*":
    graph=json.loads(Path(plan["run_path"]).read_text()); items={i["id"]:i for i in graph["items"]}
    outputs={v["output"]:k for k,v in plan["cases"].items()}
    tasks=["bootstrap"]
    for item in items.values():
        if item.get("phase")!="verification" or item.get("status") not in ("running","completed"): continue
        for dependency in item.get("depends_on",[]):
            for output in items[dependency].get("deliverables",[]):
                if output not in outputs: raise ValueError("no immutable expected output for declared deliverable")
                tasks.append(outputs[output])
    tasks=sorted(set(tasks))
try:
    results=[]
    for task in tasks:
        case=plan["cases"][task]; output=root/case["output"]
        if output.is_symlink() or not output.is_file(): raise ValueError("missing regular output")
        actual=json.loads(output.read_text())
        if actual!=case["expected"]: raise ValueError("output differs from immutable host expectation: "+task)
        results.append({"task":task,"output_sha256":hashlib.sha256(output.read_bytes()).hexdigest()})
except Exception as e:
    print(json.dumps({"task":task,"passed":False,"reason":str(e)})); raise SystemExit(1)
print(json.dumps({"task":task,"passed":True,"results":results}))
'''


class JobHandoffDispatchLiveCheck(PersistentAgentLiveCheck):
    def __init__(self, args: argparse.Namespace) -> None:
        super().__init__(args)
        self.report["boundary"] = ("Real selected-route CLI requests with synthetic order data and immutable host checks. "
            "Delivery ACK and business completion are checked separately. This does not establish arbitrary external API exactly-once delivery, "
            "supplier invoice reconciliation, OS boot recovery, or a multi-hour soak.")
        self.services: list[dict[str, Any]] = []
        self.owned: dict[int, dict[str, Any]] = {}
        self.registry_lock = threading.RLock()
        self.monitor_stop = threading.Event()
        self.monitor_error: str | None = None
        self.active_http: set[http.client.HTTPConnection] = set()

    def capture_owned(self) -> dict[int, dict[str, Any]]:
        raw = subprocess.run(["ps", "-axo", "pid=,ppid=,pgid=,stat=,lstart=,command="], capture_output=True, text=True, timeout=5)
        require(raw.returncode == 0, "Process inventory failed")
        table = {}
        for line in raw.stdout.splitlines():
            fields = line.split(None, 9)
            if len(fields) == 10 and all(value.isdigit() for value in fields[:3]):
                pid, ppid, pgid = map(int, fields[:3])
                table[pid] = {"pid": pid, "ppid": ppid, "pgid": pgid, "stat": fields[3],
                    "birth": " ".join(fields[4:9]), "command": fields[9]}
        owned = {process.pid for process in self.processes if process.poll() is None}
        owned |= {pid for pid, row in table.items() if pid != os.getpid() and str(self.root) in row["command"]}
        with self.registry_lock:
            owned |= {pid for pid, row in self.owned.items() if pid in table and row["birth"] == table[pid]["birth"]}
            while True:
                children = {pid for pid, row in table.items() if row["ppid"] in owned and pid != os.getpid()}
                if children <= owned:
                    break
                owned |= children
            for pid in owned & table.keys():
                require(table[pid]["pgid"] != os.getpgrp(), "Owned child unexpectedly shares the harness process group")
                self.owned[pid] = table[pid]
        return table

    def monitor_loop(self) -> None:
        try:
            while not self.monitor_stop.wait(0.5):
                self.capture_owned()
        except BaseException as error:
            self.monitor_error = self.redact(str(error))

    def command(self, args: list[str], label: str, **options: Any) -> tuple[Any, dict[str, Any]]:
        assert_no_manual_dispatch([{"arguments": args}])
        require(not self.monitor_error, "Process ownership monitor failed: " + str(self.monitor_error))
        return super().command(args, label, **options)

    def profile(self, name: str, tools: list[str], body: str) -> None:
        (self.home / "agents" / (name + ".md")).write_text("---\nname: " + name + "\ntools: [" + ", ".join(tools) +
            "]\npermission_mode: dontAsk\nmax_turns: 12\n---\n" + WORKSPACE_RULES + body + "\n")

    def prepare(self) -> None:
        self.stage()
        self.load_route()
        config_path = self.home / "config.toml"
        text = config_path.read_text().replace('"Workflow", "Agent"]', '"Workflow", "Agent", "Bash"]')
        text, original_timeout = private_header_timeout(text, self.provider, 300)
        config_path.write_text(text)
        config_path.chmod(0o600)
        self.transport_log = self.home / "transport-debug.log"
        self.env.update({"METIS_DEBUG": "1", "METIS_DEBUG_LOG": str(self.transport_log)})
        self.report["original_route"]["private_parameter_overrides"]["timeout_seconds"] = 300
        self.report["transport_timeout"] = {"original_seconds": original_timeout, "private_response_header_timeout_seconds": 300,
            "network_or_provider_repair_proven": False}
        self.calculator, self.checker, self.plan_path = self.root / "readonly-calculator.py", self.root / "immutable-checker.py", self.root / "host-plan.json"
        self.calculator.write_text(CALCULATOR)
        self.checker.write_text(CHECKER)
        self.calculator.chmod(0o400)
        self.checker.chmod(0o400)
        self.calculator_sha, self.checker_sha = digest(self.calculator), digest(self.checker)
        self.plan = {"workspace": str(self.workspace), "cases": {}}
        atomic_json(self.plan_path, self.plan)
        (self.home / "system.md").write_text(WORKSPACE_RULES)
        self.profile("dispatch-orders", ["Read", "Write", "Bash"], "Follow only the current durable assignment. "
            "Fully Read its input JSON and existing output JSON. Run the supplied readonly calculator exactly once, "
            "Write the exact returned JSON to the requested output, then Read it back. End with DONE. "
            "For the explicitly named negative fixture, write the exact supplied intentionally wrong JSON; do not fix it.")
        self.profile("dispatch-bootstrap", ["Read", "Write"], "Follow only the bootstrap goal. Read bootstrap-input.json; "
            "fully Read bootstrap-output.json, Write the exact input object to the output, then Read it back. End with DONE.")
        self.profile("dispatch-verifier", ["Read"], "Inspect the current work item's direct prerequisite deliverables with Read. "
            "For the initial bootstrap inspect bootstrap-input.json and bootstrap-output.json. For a handoff or HTTP event inspect the "
            "exact declared output and input files from its prerequisite evidence. Do not write or fix artifacts. This acceptance fixture "
            "uses independent host checks as its decision authority, including an intentionally incorrect negative output. Return only "
            "INSPECTED after reading the files; the host decides whether their values pass or fail.")
        self.service_token = secrets.token_hex(32)
        self.secrets.append(self.service_token)
        self.token_path = self.root / "service-token"
        self.token_path.write_text(self.service_token + "\n")
        self.token_path.chmod(0o600)
        self.monitor = threading.Thread(target=self.monitor_loop, name="owned-process-registry", daemon=True)
        self.monitor.start()

    def new_case(self, name: str, *, wrong: bool = False) -> dict[str, Any]:
        source = {"task": name, "nonce": secrets.token_hex(12), "orders": [
            {"group": "abc"[index % 3], "amount": secrets.randbelow(8000) + 1} for index in range(64)]}
        input_name, output_name = name + "-input.json", name + "-output.json"
        atomic_json(self.workspace / input_name, source)
        atomic_json(self.workspace / output_name, {})
        expected = aggregate(source)
        self.plan["cases"][name] = {"output": output_name, "expected": expected}
        atomic_json(self.plan_path, self.plan)
        calculator = shlex.join([sys.executable, str(self.calculator), str(self.workspace / input_name)])
        check = shlex.join([sys.executable, str(self.checker), str(self.plan_path), name])
        prompt = ("Task " + name + ": Read " + input_name + " and " + output_name + ". Run Bash with this one simple readonly calculator command: " +
            calculator + ". Write its exact JSON output into " + output_name + ", then Read it back. Do not process any other task. Return DONE.")
        if wrong:
            bad = {**expected, "amount": expected["amount"] + 1}
            prompt = ("Negative fixture " + name + ": Read " + input_name + " and " + output_name + ". Run Bash " + calculator +
                ". Deliberately Write exactly this intentionally wrong JSON to " + output_name + ": " + json.dumps(bad, separators=(",", ":")) +
                ". Read it back and return DONE. This fixture tests host rejection; do not repair the deliberately wrong amount.")
        return {"name": name, "prompt": prompt, "check": check, "input": input_name, "output": output_name,
            "expected": expected, "source_sha256": digest(self.workspace / input_name), "wrong": wrong, "calculator_command": calculator}

    def service_logs(self, service: dict[str, Any]) -> list[dict[str, Any]]:
        rows = []
        for line in service["stderr"].read_text(errors="replace").splitlines():
            try:
                value = json.loads(line)
            except ValueError:
                continue
            if isinstance(value, dict) and value.get("kind"):
                rows.append(value)
        return rows

    def coord(self, verb: str, *args: str, label: str) -> dict[str, Any]:
        value, _ = self.command(["coordinator", verb, *args, "--cwd", str(self.workspace), "--json"], label)
        return value

    def read_run(self) -> dict[str, Any]:
        require(self.run_path.is_file() and not self.run_path.is_symlink(), "Owned durable graph disappeared")
        return json.loads(self.run_path.read_text())

    def start_service(self, name: str) -> dict[str, Any]:
        child = self.spawn(["coordinator", "serve", self.run_id, "--cwd", str(self.workspace), "--agent", "dispatch-bootstrap",
            "--verify-agent", "dispatch-verifier", "--job", self.dispatch_job["id"], "--token-file", str(self.token_path),
            "--workers", "1", "--poll", "100ms", "--lease", "10s"],
            "resident-" + name)
        service = {"name": name, "process": child[0], "stdout": child[1], "stderr": child[2]}
        self.services.append(service)
        deadline = time.monotonic() + 30
        while time.monotonic() < deadline:
            require(child[0].poll() is None, "Resident coordinator exited before readiness")
            require(not self.monitor_error, "Process registry failed: " + str(self.monitor_error))
            ready = next((row for row in self.service_logs(service) if row["kind"] == "service_ready"), None)
            if ready:
                require(ready.get("run_id") == self.run_id, "Resident coordinator attached to a different project")
                service.update({"url": "http://" + ready["listen"], "service_id": ready["service_id"]})
                code, health = self.http(service, "GET", "/v1/health")
                require(code == 200 and health.get("run_id") == self.run_id, "Authenticated resident health failed")
                return service
            time.sleep(.1)
        raise AssertionError("Resident coordinator readiness exceeded deadline")

    def await_items(self, identifiers: set[str], *, allow_failed: bool = False) -> dict[str, Any]:
        deadline = time.monotonic() + self.args.query_timeout * 2
        while time.monotonic() < deadline:
            require(not self.monitor_error, "Process registry failed: " + str(self.monitor_error))
            require(any(service["process"].poll() is None for service in self.services), "No resident coordinator remains alive")
            run = self.read_run()
            items = {item["id"]: item for item in run["items"]}
            require(identifiers <= items.keys(), "Dispatched assignment disappeared from durable graph")
            terminal = [items[key]["status"] for key in identifiers]
            bad = [items[key] for key in identifiers if items[key]["status"] in ("blocked", "failed", "cancelled")]
            if bad and not allow_failed:
                raise AssertionError("Automatically dispatched work failed: " + json.dumps(bad))
            if all(state == "completed" for state in terminal) or (allow_failed and bad):
                return run
            time.sleep(.15)
        raise AssertionError("Automatically dispatched work exceeded bounded deadline")

    def bootstrap(self) -> None:
        expected = {"fixture": "automatic-job-dispatch", "nonce": secrets.token_hex(12)}
        atomic_json(self.workspace / "bootstrap-input.json", expected)
        atomic_json(self.workspace / "bootstrap-output.json", {})
        self.plan["cases"]["bootstrap"] = {"output": "bootstrap-output.json", "expected": expected}
        checker = shlex.join([sys.executable, str(self.checker), str(self.plan_path), "*"])
        run = self.coord("create", "--goal", BOOTSTRAP_GOAL, "--check", checker, "--max-budget-usd", "20",
            label="create-owned-resident-project")
        self.run_id = run["id"]
        self.run_path = self.home / "project-coordinator" / hashlib.sha256(str(self.workspace).encode()).hexdigest()[:32] / (self.run_id + ".json")
        self.plan["run_path"] = str(self.run_path)
        atomic_json(self.plan_path, self.plan)
        before = self.trace_snapshot()
        self.primary = self.start_service("primary")
        self.contender = self.start_service("contender")
        self.await_items({"research", "synthesis", "implementation", "verification"})
        proof, _ = self.real_proof(before, "bootstrap-real-provider", required_tools=("Read", "Write"))
        require(json.loads((self.workspace / "bootstrap-output.json").read_text()) == expected, "Bootstrap output failed independent host check")
        self.record("real bootstrap and two competing resident coordinators ready", provider_proof=proof,
            services=[{"name": value["name"], "pid": value["process"].pid, "service_id": value["service_id"]} for value in self.services])

    def create_dispatch_job(self) -> None:
        value, _ = self.job("create", "--name", "automatic-orders", "--cwd", str(self.workspace), "--agent", "dispatch-orders",
            "--provider", self.provider, "--model", self.model, "--max-input-tokens", "4000000", "--max-output-tokens", "100000",
            label="create-frozen-automatic-job")
        require(value.get("id") and value.get("session_id") and re.fullmatch(r"[a-f0-9]{64}", value.get("config_sha256", "")),
            "Automatic job lacks its stable principal/session/config identity")
        self.dispatch_job = value
        self.jobs["automatic-orders"] = value
        # The live worker must use its saved definition even if the ordinary
        # named profile changes after job creation and before the first send.
        self.profile("dispatch-orders", ["Read"], "Return PROFILE_CHANGED_ONLY. Never Write or use Bash.")

    def send_case(self, case: dict[str, Any], *, label: str) -> dict[str, Any]:
        task_path = self.root / (case["name"] + "-handoff-task.json")
        if not task_path.exists():
            atomic_json(task_path, {"id": case["name"], "objective": case["prompt"],
                "artifacts": [{"path": case["input"], "sha256": case["source_sha256"]}],
                "deliverables": [case["output"]], "acceptance": ["The output JSON must exactly match independent host arithmetic for all 64 orders."]})
        value, _ = self.command(["handoff", "send", "--from", "fixture-sender", "--to", "job:" + self.dispatch_job["id"],
            "--body", case["prompt"], "--task-file", str(task_path), "--key", case["name"]], label)
        require(value.get("id") and value.get("to") == "job:" + self.dispatch_job["id"], "Durable send returned another mailbox")
        return value

    def message(self, identifier: str) -> dict[str, Any]:
        paths = [self.home / "handoff" / name / (identifier + ".json") for name in ("receipts", "messages")]
        for path in paths:
            try:
                value = json.loads(path.read_text())
            except FileNotFoundError:
                continue
            require(value.get("id") == identifier, "Durable message identity changed on disk")
            return value
        raise AssertionError("Durable message is absent from both inbox and receipts")

    def await_business(self, identifier: str, states: set[str]) -> dict[str, Any]:
        deadline = time.monotonic() + self.args.query_timeout * 2
        while time.monotonic() < deadline:
            require(not self.monitor_error, "Process registry failed: " + str(self.monitor_error))
            require(any(service["process"].poll() is None for service in self.services), "Resident dispatch service exited")
            record = self.message(identifier)
            state = record.get("business", {}).get("state")
            if state in states:
                return record
            require(state not in ("failed", "rejected", "blocked", "cancelled"), "Automatic business failed unexpectedly: " + json.dumps(record.get("business")))
            time.sleep(.15)
        raise AssertionError("Business receipt did not reach its bounded expected state")

    def await_idle(self) -> None:
        deadline = time.monotonic() + 30
        while time.monotonic() < deadline:
            active = []
            for service in self.services:
                if service["process"].poll() is not None:
                    continue
                code, status = self.http(service, "GET", "/v1/status")
                require(code == 200, "Resident status is unavailable")
                active.append(status.get("active_children"))
            if active and all(value == 0 for value in active):
                return
            time.sleep(.1)
        raise AssertionError("Resident task children did not become idle")

    def stop_services(self) -> None:
        self.await_idle()
        self.capture_owned()
        for service in self.services:
            if service["process"].poll() is None:
                self.stop(service["process"])
        require(not any(service["process"].poll() is None for service in self.services), "Resident services did not stop")

    def no_dispatch(self, case: dict[str, Any] | None, *, seconds: float = 2) -> None:
        before = self.read_run()
        deadline = time.monotonic() + seconds
        while time.monotonic() < deadline:
            time.sleep(.1)
            run = self.read_run()
            require(len(run.get("usage_requests", [])) == len(before.get("usage_requests", [])), "Paused or notification-only work issued a model request")
            if case:
                require(json.loads((self.workspace / case["output"]).read_text()) == {}, "Paused assignment wrote its output")
        require(not self.monitor_error, "Process registry failed: " + str(self.monitor_error))

    def verify_result(self, case: dict[str, Any], record: dict[str, Any], before: dict[str, int], *, expected_state: str = "completed") -> dict[str, Any]:
        receipt = business_receipt(record, expected_state)
        assignment = receipt["assignment"]
        require(assignment["job_id"] == self.dispatch_job["id"] and assignment["config_sha256"] == self.dispatch_job["config_sha256"] and
            assignment["workspace"] == str(self.workspace) and assignment["run_id"] == self.run_id, "Business receipt changed its frozen job/project binding")
        run = self.read_run()
        require(0 < receipt["source_revision"] <= run["revision"], "Business receipt revision is outside durable graph history")
        items = {item["id"]: item for item in run["items"]}
        implementation, verification = items[assignment["work_item_id"]], items[assignment["verification_item_id"]]
        require(verification.get("depends_on") == [implementation["id"]] and verification.get("phase") == "verification",
            "Business verification does not depend on its exact implementation")
        binding = assert_frozen_binding(implementation, self.dispatch_job)
        require(implementation.get("deliverables") == [case["output"]] and implementation.get("attempts") == 1 and verification.get("attempts") == 1,
            "Automatic task lost its deliverable or ran a duplicate attempt")
        require(record.get("state") == "acknowledged" and record.get("checkpoint_sha256"), "Completed model work did not durably ACK its assigned envelope")
        if expected_state == "completed":
            require(implementation["status"] == verification["status"] == "completed" and verification.get("acceptance", {}).get("status") == "passed",
                "Business completed before its implementation and host verification")
            require(json.loads((self.workspace / case["output"]).read_text()) == case["expected"], "Host independently rejected successful business output")
            artifacts = {value["path"]: value["sha256"] for value in receipt.get("artifacts", [])}
            require(artifacts == {case["output"]: digest(self.workspace / case["output"])}, "Business receipt artifact hashes differ from actual deliverables")
            require(receipt["output"] == implementation.get("output"), "Sender receipt does not contain its implementation's persisted result")
            require(receipt["acceptance"]["evidence_sha256"] == go_json_sha(verification["acceptance"]),
                "Business receipt does not hash the actual persisted host acceptance")
            checks = [{key: check[key] for key in ("command", "exit_code", "output_sha256")} for check in verification["acceptance"]["checks"]]
            require(receipt["acceptance"]["checks"] == checks, "Business receipt host checks differ from its verifier's concrete checks")
        else:
            require(verification["status"] in ("failed", "blocked") and verification.get("acceptance", {}).get("status") == "failed",
                "Wrong output was not rejected by the actual host verifier")
            require(json.loads((self.workspace / case["output"]).read_text()) != case["expected"], "Negative fixture accidentally produced a correct output")
        physical = assert_physical_usage(run, {implementation["id"], verification["id"]}, self.provider, self.model)
        proof, rows = self.real_proof(before, case["name"] + "-real-provider", required_tools=("Read", "Bash", "Write"))
        calculator_calls = [call for call in tool_calls(rows, "Bash") if shlex.split(call["arguments"].get("command", "")) == shlex.split(case["calculator_command"])]
        require(len(calculator_calls) == 1, "Job did not execute its exact readonly calculator once")
        require(digest(self.workspace / case["input"]) == case["source_sha256"], "Job changed its immutable source data")
        persisted, _ = self.command(["handoff", "status", record["id"]], case["name"] + "-sender-reads-durable-result")
        require(persisted.get("business", {}).get("assignment") == assignment, "Sender cannot reload the durable business assignment")
        business_receipt(persisted, expected_state)
        require(digest(self.calculator) == self.calculator_sha and digest(self.checker) == self.checker_sha,
            "Model changed an immutable host calculator or checker")
        return {"message_id": record["id"], "business": receipt, "implementation_id": implementation["id"], "verification_id": verification["id"],
            "frozen_binding": binding, "physical_usage": physical, "provider_proof": proof,
            "exact_readonly_calculator_call": calculator_calls[0],
            "completed_owners": {item["id"]: [event.get("worker") for event in run.get("events", []) if event.get("kind") == "work_completed" and
                event.get("item_id") == item["id"]] for item in (implementation, verification)}, "model_trace_events": len(rows)}

    def offline_dispatch_and_legacy(self) -> None:
        self.await_idle()
        before_run = self.read_run()
        legacy, _ = self.command(["handoff", "send", "--from", "fixture-sender", "--to", "job:" + self.dispatch_job["id"],
            "--body", "Ordinary notification only: no task or action is requested. Continue only the separately assigned objective.", "--key", "legacy-notification"],
            "send-legacy-notification-without-task")
        self.no_dispatch(None)
        require(len(self.read_run()["items"]) == len(before_run["items"]) and not self.message(legacy["id"]).get("business"),
            "Ordinary notification incorrectly became a scheduled business task")
        self.stop_services()
        case = self.new_case("offline-single-send")
        first = self.send_case(case, label="one-offline-business-send")
        require(first.get("duplicate") is False and first.get("state") == "pending" and not first.get("business"),
            "Offline send incorrectly claimed immediate delivery or business completion")
        duplicate = self.send_case(case, label="offline-business-send-idempotent-replay")
        require(duplicate.get("duplicate") is True and duplicate["id"] == first["id"], "Repeated logical send created another envelope")
        require(not self.message(first["id"]).get("business"), "Offline mailbox ACK was mistaken for business completion")
        before = self.trace_snapshot()
        self.primary = self.start_service("offline-restarted-primary")
        self.contender = self.start_service("offline-restarted-contender")
        completed = self.await_business(first["id"], {"completed"})
        result = self.verify_result(case, completed, before)
        legacy_after = self.message(legacy["id"])
        require(not legacy_after.get("business"), "A plain notification acquired a business outcome")
        self.record("offline single handoff automatically dispatched by competing resident services with host-completed sender receipt",
            **result, duplicate_message_id=duplicate["id"], legacy_notification={"id": legacy["id"], "transport_state": legacy_after["state"],
                "business_receipt_absent": True}, manual_dispatch_used=False)

    def real_model_sender_and_result(self) -> None:
        self.await_idle()
        case = self.new_case("model-sender-business-result")
        sender_session = str(uuid.uuid4())
        task = {"id": case["name"], "objective": case["prompt"], "artifacts": [{"path": case["input"], "sha256": case["source_sha256"]}],
            "deliverables": [case["output"]], "acceptance": ["The output JSON must exactly match immutable host grouped integer arithmetic."]}
        arguments = {"to": "job:" + self.dispatch_job["id"], "body": case["prompt"], "idempotency_key": case["name"], "task": task}
        prompt = ("Call MessageTeammate exactly once to send this structured task to the persistent job. "
            "Do not do the task yourself, write files, delegate, or call another tool. Use these exact arguments: " + json.dumps(arguments) +
            ". After the send succeeds return SENT.")
        before = self.trace_snapshot()
        self.command(["run", "--bare", "--tools", "MessageTeammate", "--session-id", sender_session, prompt],
            "real-model-sender-structured-task", json_output=False)
        send_rows = self.trace_events(before)
        sender_rows = [row for row in send_rows if row.get("session_id") == sender_session]
        send_proof = trace_proof(sender_rows, self.provider, self.model)
        require(send_proof["model_responses"] > 0 and not send_proof["errors"] and
            any(sum(row.get(key, 0) for key in ("input", "output", "cache_read", "cache_write")) > 0 for row in send_proof["usage"]),
            "Sender model query has no successful real provider evidence")
        calls = tool_calls(sender_rows, "MessageTeammate")
        require(len(calls) == 1 and all(calls[0]["arguments"].get(key) == value for key, value in arguments.items()) and
            calls[0]["arguments"].get("operation", "send") in ("", "send"), "Sender model did not actually send the exact structured task once")
        sent = json.loads(calls[0]["result"])
        require(sent.get("id"), "Actual sender tool returned no durable message ID")
        completed = self.await_business(sent["id"], {"completed"})
        scope = "session:" + sender_session
        require(completed.get("sender_scope") == scope and sent.get("sender_scope") == scope, "Sender mailbox scope is not owned by its original session")
        result = self.verify_result(case, completed, before)
        self.await_idle()
        transcript = self.home / "sessions" / (sender_session + ".jsonl")
        require(transcript.is_file(), "Original sender transcript is absent")
        original_bytes = transcript.read_bytes()
        before_status = self.trace_snapshot()
        status_arguments = {"operation": "status", "message_id": completed["id"]}
        prompt = ("Read the completed business receipt from your previous send. Call MessageTeammate exactly once with these arguments: " +
            json.dumps(status_arguments) + ". Do not send another message or use another tool. Return RECEIPT_READ after the tool returns.")
        self.command(["run", "--bare", "--tools", "MessageTeammate", "--resume", sender_session, prompt],
            "same-sender-session-resumes-and-reads-business-result", json_output=False)
        status_proof, status_rows = self.real_proof(before_status, "same-sender-status-real-provider", required_tools=("MessageTeammate",))
        require(status_proof["session_ids"] == [sender_session] and transcript.read_bytes().startswith(original_bytes),
            "Business status query changed sender sessions or overwrote its durable transcript")
        status_calls = tool_calls(status_rows, "MessageTeammate")
        require(len(status_calls) == 1 and all(status_calls[0]["arguments"].get(key) == value for key, value in status_arguments.items()),
            "Resumed sender model did not actually invoke its read-only status operation")
        tool_result = json.loads(status_calls[0]["result"])
        receipt_proof = assert_sender_tool_receipt(tool_result, self.message(completed["id"]), scope)
        self.report["sender_queries_outside_graph"] = {"send": send_proof, "status": status_proof,
            "not_in_graph_catalogue_estimate": True, "supplier_invoice_verified": False}
        self.record("real sender model sends a structured task and same resumed session reads its host-completed business receipt", **result,
            sender_session_id=sender_session, actual_sender_call=calls[0], actual_status_call=status_calls[0], sender_status=receipt_proof,
            sender_provider_queries={"send": send_proof, "status": status_proof}, manual_dispatch_used=False)

    def http_job_dispatch(self) -> None:
        self.await_idle()
        case = self.new_case("http-job-route")
        event = {"source": "live-owned-http", "id": case["name"], "subject": case["name"], "prompt": case["prompt"],
            "deliverables": [case["output"]], "job_id": self.dispatch_job["id"]}
        before = self.trace_snapshot()
        code, first = self.http(self.primary, "POST", "/v1/events", event)
        require(code == 202 and first.get("item_id"), "HTTP explicit job route was not accepted")
        duplicate_code, duplicate = self.http(self.contender, "POST", "/v1/events", event)
        require(duplicate_code == 202 and duplicate.get("item_id") == first["item_id"], "HTTP event replay created another job task")
        conflict_code, conflict = self.http(self.primary, "POST", "/v1/events", {**event, "prompt": event["prompt"] + " Changed payload."})
        require(conflict_code == 409, "HTTP event ID accepted a changed payload")
        denied_code, denied = self.http(self.primary, "POST", "/v1/events", {**event, "id": "unlisted-job", "job_id": "job-not-in-allowlist"})
        require(denied_code in (400, 403, 404), "HTTP job route bypassed the explicit service allowlist")
        run = self.await_items({first["item_id"], first["item_id"] + "-verify"})
        items = {item["id"]: item for item in run["items"]}
        implementation, verification = items[first["item_id"]], items[first["item_id"] + "-verify"]
        binding = assert_frozen_binding(implementation, self.dispatch_job)
        require(implementation["attempts"] == verification["attempts"] == 1 and verification.get("acceptance", {}).get("status") == "passed",
            "HTTP duplicate was rerun or lacked host acceptance")
        require(json.loads((self.workspace / case["output"]).read_text()) == case["expected"], "HTTP job output differs from independent host arithmetic")
        physical = assert_physical_usage(run, {implementation["id"], verification["id"]}, self.provider, self.model)
        proof, _ = self.real_proof(before, case["name"] + "-real-provider", required_tools=("Read", "Bash", "Write"))
        self.record("HTTP explicit job route freezes identity and deduplicates across resident services", item_id=implementation["id"],
            frozen_binding=binding, provider_proof=proof, physical_usage=physical, duplicate_http_status=duplicate_code,
            payload_conflict={"status": conflict_code, "body": conflict}, allowlist_rejection={"status": denied_code, "body": denied},
            output_sha256=digest(self.workspace / case["output"]))

    def paused_restart_resume(self) -> None:
        self.await_idle()
        self.job("pause", self.dispatch_job["id"], label="pause-automatic-job")
        case = self.new_case("paused-restart-resume")
        first = self.send_case(case, label="send-while-job-paused")
        self.no_dispatch(case)
        self.stop_services()
        self.contender = self.start_service("paused-service-restarted-contender")
        self.no_dispatch(case)
        require(self.message(first["id"]).get("business", {}).get("state") != "completed", "Paused/restarted job reported business completion")
        before = self.trace_snapshot()
        self.job("resume", self.dispatch_job["id"], label="resume-job-only-without-manual-task-run")
        completed = self.await_business(first["id"], {"completed"})
        result = self.verify_result(case, completed, before)
        self.primary = self.start_service("after-resume-primary")
        self.record("paused assignment survived full service restart and resume automatically completed on the contender", **result,
            contender_pid=self.contender["process"].pid, task_resubmitted=False, manual_dispatch_used=False)

    def rejected_business(self) -> None:
        self.await_idle()
        case = self.new_case("wrong-output-rejected", wrong=True)
        before = self.trace_snapshot()
        first = self.send_case(case, label="send-negative-business-acceptance-fixture")
        terminal = self.await_business(first["id"], {"rejected", "failed"})
        state = terminal["business"]["state"]
        result = self.verify_result(case, terminal, before, expected_state=state)
        self.await_idle()
        requests = len(self.read_run().get("usage_requests", []))
        time.sleep(2)
        require(len(self.read_run().get("usage_requests", [])) == requests and self.message(first["id"])["business"]["state"] == state,
            "Host rejection automatically retried or changed into business completion")
        self.record("normal model completion with deliberately wrong artifact produces a durable rejection instead of completed", **result)

    def final_acceptance(self) -> None:
        assert_no_manual_dispatch(self.report["commands"])
        require(digest(self.calculator) == self.calculator_sha and digest(self.checker) == self.checker_sha, "Host fixture source changed")
        run = self.read_run()
        require(not any(record.get("status") == "uncertain" for record in run.get("operations", [])), "A write operation has uncertain outcome")
        usage = assert_physical_usage(run, {item["id"] for item in run["items"]}, self.provider, self.model)
        require(usage["estimated_usd"] <= 20, "Known graph catalogue estimate exceeded its test allowance")
        reloaded = self.status(self.dispatch_job["id"], "reload-final-automatic-principal")
        require(reloaded["id"] == self.dispatch_job["id"] and reloaded["session_id"] == self.dispatch_job["session_id"] and
            reloaded["config_sha256"] == self.dispatch_job["config_sha256"] and reloaded["definition"] == self.dispatch_job["definition"],
            "Automatic dispatch or service restart changed the principal's frozen identity")
        self.report["physical_usage"] = usage
        self.report["final_job"] = {"id": reloaded["id"], "session_id": reloaded["session_id"], "config_sha256": reloaded["config_sha256"],
            "input_tokens": reloaded.get("input_tokens"), "output_tokens": reloaded.get("output_tokens"),
            "unresolved_requests": reloaded.get("unresolved_requests", 0)}
        self.record("final automatic dispatch evidence has settled selected-route usage and stable principal identity", physical_usage=usage,
            no_manual_job_run_or_coordinator_add=True, host_checker_sha256=self.checker_sha, readonly_calculator_sha256=self.calculator_sha)

    def http(self, service: dict[str, Any], method: str, path: str, value: Any = None) -> tuple[int, Any]:
        address = urlsplit(service["url"])
        require(address.hostname == "127.0.0.1" and not address.username, "Service must stay on owned loopback")
        connection = http.client.HTTPConnection(address.hostname, address.port, timeout=15)
        self.active_http.add(connection)
        try:
            connection.request(method, path, None if value is None else json.dumps(value).encode(),
                {"Authorization": "Bearer " + self.service_token, "Content-Type": "application/json", "Connection": "close"})
            response = connection.getresponse()
            raw = response.read(1 << 20)
            return response.status, json.loads(raw) if raw else None
        finally:
            connection.close()
            self.active_http.discard(connection)

    def cleanup(self) -> None:
        errors = []
        self.monitor_stop.set()
        if hasattr(self, "monitor"):
            self.monitor.join(timeout=10)
            if self.monitor.is_alive():
                errors.append("Process registry thread did not stop")
        if self.monitor_error:
            errors.append("Process registry failed: " + self.monitor_error)
        try:
            self.capture_owned()
        except BaseException as error:
            errors.append(self.redact(str(error)))
        for process in self.processes:
            try:
                self.stop(process)
            except BaseException as error:
                errors.append(self.redact(str(error)))
        try:
            table = self.capture_owned()
            groups = {row["pgid"] for pid, row in self.owned.items() if pid in table and table[pid]["birth"] == row["birth"]}
            for group in groups:
                require(group != os.getpgrp(), "Cleanup would target the harness process group")
                try:
                    os.killpg(group, signal.SIGKILL)
                except ProcessLookupError:
                    pass
            deadline = time.monotonic() + 10
            while True:
                table = self.capture_owned()
                remaining = [pid for pid, row in self.owned.items() if pid in table and table[pid]["birth"] == row["birth"] and
                    not table[pid]["stat"].startswith("Z")]
                if not remaining or time.monotonic() >= deadline:
                    break
                time.sleep(.05)
            if remaining:
                errors.append("Owned descendants survived cleanup: " + str(remaining))
        except BaseException as error:
            errors.append(self.redact(str(error)))
            remaining = ["process inventory unavailable"]
        for connection in list(self.active_http):
            connection.close()
        self.active_http.clear()
        super().cleanup()
        if hasattr(self, "token_path"):
            self.token_path.unlink(missing_ok=True)
        self.service_token = ""
        self.report["cleanup"].update({"private_service_token_destroyed": not getattr(self, "token_path", self.root / "absent").exists(),
            "owned_descendant_groups_destroyed": not remaining, "registry_thread_stopped": not hasattr(self, "monitor") or not self.monitor.is_alive(),
            "dispatch_cleanup_errors": errors})
        atomic_json(self.output / "owned-process-tree-cleanup-proof.json", {"captured": list(self.owned.values()), "remaining_live_owned_pids": remaining, "errors": errors})
        require(not errors, "Automatic-dispatch cleanup incomplete: " + str(errors))

    def copy_evidence(self) -> None:
        super().copy_evidence()
        manifest_path = self.output / "evidence-manifest.json"
        manifest = json.loads(manifest_path.read_text())
        fixture = self.output / "host-fixture"
        fixture.mkdir(mode=0o700, exist_ok=True)
        for name in ("calculator", "checker", "plan_path"):
            path = getattr(self, name, None)
            if path and path.is_file() and not path.is_symlink():
                destination = fixture / path.name
                destination.write_text(self.redact(path.read_text()))
                destination.chmod(0o600)
                manifest.append({"path": str(destination.relative_to(self.output)), "sha256": digest(destination), "bytes": destination.stat().st_size})
        if not hasattr(self, "transport_log") or not self.transport_log.exists():
            atomic_json(manifest_path, manifest)
            return
        sanitized, summary = sanitized_transport_log(self.transport_log.read_text(errors="replace"), self.redact)
        log = self.output / "transport-debug.redacted.log"
        log.write_text(self.redact(sanitized))
        atomic_json(self.output / "transport-debug-summary.json", json.loads(self.redact(json.dumps(summary))))
        for path in (log, self.output / "transport-debug-summary.json"):
            manifest.append({"path": path.name, "sha256": digest(path), "bytes": path.stat().st_size})
        atomic_json(manifest_path, manifest)

    def run(self) -> int:
        signal.setitimer(signal.ITIMER_REAL, self.args.case_timeout)
        try:
            self.prepare()
            self.create_dispatch_job()
            self.bootstrap()
            self.real_model_sender_and_result()
            self.offline_dispatch_and_legacy()
            self.http_job_dispatch()
            self.paused_restart_resume()
            self.rejected_business()
            self.final_acceptance()
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
            if self.root.exists():
                self.report["passed"] = False
            self.report.update({"finished_utc": utc_now(), "elapsed_seconds": round(time.monotonic() - self.started, 3)})
            self.report = json.loads(self.redact(json.dumps(self.report)))
            self.secrets.clear()
            self.report["cleanup"]["redaction_secret_values_cleared_after_evidence"] = not self.secrets
            atomic_json(self.output / "verification-report.json", self.report)
            self.progress("finished", passed=self.report["passed"], failure=self.report.get("failure"))
        print(json.dumps({"passed": self.report["passed"], "checks": len(self.report["checks"]), "report": str(self.output / "verification-report.json"),
            "failure": self.report.get("failure")}), flush=True)
        return 0 if self.report["passed"] else 1


def expect_rejected(action: Any, label: str) -> None:
    try:
        action()
    except AssertionError:
        return
    raise AssertionError("Oracle incorrectly accepted " + label)


def self_test() -> dict[str, Any]:
    checks = []
    require(all(stage + ":" in BOOTSTRAP_GOAL for stage in ("Research", "Synthesis", "Implementation", "Verification")) and
        "No additional assignment" in BOOTSTRAP_GOAL and "bootstrap-input.json" in BOOTSTRAP_GOAL and "bootstrap-output.json" in BOOTSTRAP_GOAL and
        "accept durable job assignments" not in BOOTSTRAP_GOAL and "when it arrives" not in BOOTSTRAP_GOAL,
        "Bootstrap scaffold goal must be a finite copy/check objective independent of another assignment")
    checks.append("all bootstrap scaffold stages have a finite copy/check objective without awaiting another assignment")
    source = {"task": "fixture", "nonce": "offline-only", "orders": [
        {"group": "b", "amount": 3}, {"group": "a", "amount": 5}, {"group": "b", "amount": 8}]}
    expected = {"task": "fixture", "nonce": "offline-only", "count": 3, "amount": 16,
        "groups": {"a": {"count": 1, "amount": 5}, "b": {"count": 2, "amount": 11}}}
    require(aggregate(source) == expected, "Independent integer arithmetic oracle failed")
    checks.append("independent exact grouped integer arithmetic")
    completed = {"id": "msg-fixture", "state": "acknowledged", "business": {"state": "completed", "source_revision": 9,
        "assignment": {"job_id": "job-fixture", "workspace": "/tmp/fixture", "run_id": "run-fixture", "work_item_id": "assignment",
            "verification_item_id": "assignment-verify", "config_sha256": "a" * 64},
        "acceptance": {"passed": True, "source": "host", "evidence_sha256": "b" * 64,
            "checks": [{"command": "fixture", "exit_code": 0, "output_sha256": "c" * 64}]},
        "output": "DONE", "output_sha256": hashlib.sha256(b"DONE").hexdigest(), "completed_at": "2026-10-15T00:00:00Z"}}
    business_receipt(completed, "completed")
    checks.append("separate host-accepted business completion")
    expect_rejected(lambda: business_receipt({"state": "acknowledged"}, "completed"), "delivery ACK as business completion")
    checks.append("ACK alone cannot count as completion")
    missing_check = copy.deepcopy(completed)
    missing_check["business"]["acceptance"]["passed"] = False
    expect_rejected(lambda: business_receipt(missing_check, "completed"), "assistant claim without host acceptance")
    checks.append("assistant completion claim without acceptance rejected")
    missing_checks = copy.deepcopy(completed)
    missing_checks["business"]["acceptance"]["checks"] = []
    expect_rejected(lambda: business_receipt(missing_checks, "completed"), "no concrete successful host check")
    premature_ack = copy.deepcopy(completed)
    premature_ack["state"] = "claimed"
    expect_rejected(lambda: business_receipt(premature_ack, "completed"), "business completion before durable ACK")
    checks.append("concrete zero-exit host checks and prior durable ACK required")
    owned = copy.deepcopy(completed)
    owned.update({"sender_scope": "session:fixture", "from": "parent", "to": "job:fixture", "checkpoint_sha256": "d" * 64})
    safe = {key: value for key, value in owned.items() if key != "business"}
    safe["business"] = copy.deepcopy(owned["business"])
    safe["business"]["assignment"].pop("workspace")
    safe["business"]["acceptance"]["checks"] = [{"index": 1, "exit_code": 0, "output_sha256": "c" * 64}]
    assert_sender_tool_receipt(safe, owned, "session:fixture")
    expect_rejected(lambda: assert_sender_tool_receipt(safe, owned, "session:other"), "different sender session scope")
    checks.append("model-facing business receipt binds original sender session and exact result hashes")
    leaked = copy.deepcopy(safe)
    leaked["business"]["acceptance"]["checks"][0]["command"] = "private-host-command"
    expect_rejected(lambda: assert_sender_tool_receipt(leaked, owned, "session:fixture"), "model-facing host command leakage")
    checks.append("model-facing result excludes host commands and workspace authority")
    rejected = copy.deepcopy(completed)
    rejected["business"].update({"state": "rejected", "failure": {"code": "acceptance_failed"}})
    rejected["business"]["acceptance"]["passed"] = False
    rejected["business"].pop("completed_at")
    business_receipt(rejected, "rejected")
    checks.append("wrong artifact rejected without completion timestamp")
    rejected["business"]["completed_at"] = "2026-10-15T00:00:00Z"
    expect_rejected(lambda: business_receipt(rejected, "rejected"), "rejected task with completion timestamp")
    checks.append("rejected business cannot carry completed timestamp")
    job = {"id": "job-fixture", "config_sha256": "a" * 64, "definition": {"provider": "fixture", "profile": {"model": "model"}}}
    item = {"principal_binding": {"job_id": job["id"], "config_sha256": job["config_sha256"], "definition": job["definition"]}}
    assert_frozen_binding(item, job)
    wrong_binding = copy.deepcopy(item)
    wrong_binding["principal_binding"]["definition"]["profile"]["model"] = "other"
    expect_rejected(lambda: assert_frozen_binding(wrong_binding, job), "changed principal model")
    checks.append("frozen principal route and definition enforced")
    usage = {"item_id": "assignment", "client_request_id": "offline-request", "provider": "fixture", "model": "model",
        "status": "completed", "usage_reported": True, "response_status_code": 200,
        "usage": {"input": 1, "output": 2, "cache_read": 3, "cache_write": 0}}
    proof = assert_physical_usage({"usage_requests": [usage]}, {"assignment"}, "fixture", "model")
    require(proof["physical_requests"] == 1 and proof["tokens"]["cache_read"] == 3, "Absolute token proof failed")
    unknown = {**usage, "usage_reported": False}
    expect_rejected(lambda: assert_physical_usage({"usage_requests": [unknown]}, {"assignment"}, "fixture", "model"), "unknown usage")
    expect_rejected(lambda: assert_physical_usage({"usage_requests": [usage, usage]}, {"assignment"}, "fixture", "model"), "duplicate physical request")
    checks.append("physical usage must be unique reported successful selected route")
    for command in (["job", "run", "x"], ["job", "rotate", "x"], ["coordinator", "add", "x"], ["coordinator", "run", "x"]):
        expect_rejected(lambda: assert_no_manual_dispatch([{"arguments": command}]), "manual dispatch")
    assert_no_manual_dispatch([{"arguments": ["handoff", "send"]}, {"arguments": ["job", "resume"]}, {"arguments": ["coordinator", "serve"]}])
    checks.append("manual job execution or coordinator task addition forbidden")
    for script in (CALCULATOR, CHECKER):
        compile(script, "offline-helper", "exec")
    checks.append("readonly calculator and independent checker syntax")
    with tempfile.TemporaryDirectory(prefix="metis-dispatch-oracle-offline-") as temporary:
        root = Path(temporary)
        calculator, checker = root / "calculator.py", root / "checker.py"
        calculator.write_text(CALCULATOR)
        checker.write_text(CHECKER)
        atomic_json(root / "input.json", source)
        calculated = subprocess.run([sys.executable, "-B", str(calculator), str(root / "input.json")], capture_output=True, text=True, timeout=10)
        require(calculated.returncode == 0 and json.loads(calculated.stdout) == expected, "Readonly calculator differs from independent host arithmetic")
        atomic_json(root / "output.json", expected)
        plan = root / "plan.json"
        atomic_json(plan, {"workspace": str(root), "cases": {"fixture": {"output": "output.json", "expected": expected}}})
        good = subprocess.run([sys.executable, "-B", str(checker), str(plan), "fixture"], capture_output=True, text=True, timeout=10)
        require(good.returncode == 0 and json.loads(good.stdout).get("passed") is True, "Host checker rejected an exact artifact")
        atomic_json(root / "output.json", {**expected, "amount": expected["amount"] + 1})
        bad = subprocess.run([sys.executable, "-B", str(checker), str(plan), "fixture"], capture_output=True, text=True, timeout=10)
        require(bad.returncode == 1 and json.loads(bad.stdout).get("passed") is False, "Host checker accepted wrong arithmetic")
        checks.append("actual offline calculator and checker accept exact output and reject wrong arithmetic")
        (root / "output.json").unlink()
        (root / "output.json").symlink_to(root / "input.json")
        linked = subprocess.run([sys.executable, "-B", str(checker), str(plan), "fixture"], capture_output=True, text=True, timeout=10)
        require(linked.returncode == 1 and json.loads(linked.stdout).get("passed") is False, "Host checker accepted a linked artifact")
        checks.append("host checker rejects symlink artifact")
        (root / "output.json").unlink()
        atomic_json(root / "output.json", expected)
        atomic_json(root / "future.json", {})
        graph = root / "graph.json"
        items = [{"id": "future", "deliverables": ["future.json"], "status": "pending"},
            {"id": "future-verify", "phase": "verification", "depends_on": ["future"], "status": "pending"}]
        atomic_json(graph, {"items": items})
        atomic_json(plan, {"workspace": str(root), "run_path": str(graph), "cases": {
            "bootstrap": {"output": "output.json", "expected": expected}, "future": {"output": "future.json", "expected": expected}}})
        future = subprocess.run([sys.executable, "-B", str(checker), str(plan), "*"], capture_output=True, text=True, timeout=10)
        require(future.returncode == 0, "A future queued task incorrectly failed another assignment's host check")
        items[0]["status"], items[1]["status"] = "completed", "running"
        atomic_json(graph, {"items": items})
        empty = subprocess.run([sys.executable, "-B", str(checker), str(plan), "*"], capture_output=True, text=True, timeout=10)
        require(empty.returncode == 1 and json.loads(empty.stdout).get("passed") is False, "Current verification skipped an empty declared output")
        checks.append("shared graph checker ignores future work but rejects empty current deliverables")
        atomic_json(root / "future.json", expected)
        complete = subprocess.run([sys.executable, "-B", str(checker), str(plan), "*"], capture_output=True, text=True, timeout=10)
        require(complete.returncode == 0 and len(json.loads(complete.stdout)["results"]) == 2, "Shared graph checker omitted current completed work")
        checks.append("shared graph checker verifies bootstrap and exact current prerequisite artifact")
    return {"passed": True, "offline_only": True, "provider_requests": 0, "checks": checks, "count": len(checks)}


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("--repo", type=Path, default=Path(__file__).resolve().parents[2])
    parser.add_argument("--binary", type=Path)
    parser.add_argument("--expected-sha256")
    parser.add_argument("--source-freeze", type=Path)
    parser.add_argument("--output", type=Path)
    parser.add_argument("--destroy-input", action="store_true")
    parser.add_argument("--query-timeout", type=float, default=600)
    parser.add_argument("--case-timeout", type=float, default=3600)
    parser.add_argument("--self-test", action="store_true")
    args = parser.parse_args()
    os.umask(0o077)
    if args.self_test:
        print(json.dumps(self_test(), ensure_ascii=False, indent=2))
        return 0
    require(args.binary and args.expected_sha256 and args.source_freeze and args.output, "Live mode requires a binary, exact digest, source freeze and new output")
    require(args.destroy_input, "Both failed and passing CLIs must be destroyed")
    require(re.fullmatch(r"[a-f0-9]{64}", args.expected_sha256), "Expected binary SHA must be exact SHA-256")
    require(0 < args.query_timeout <= 600 and 0 < args.case_timeout <= 3600, "Live timeouts must remain bounded")
    def interrupted(signum: int, _frame: Any) -> None:
        raise KeyboardInterrupt("Harness interrupted by " + signal.Signals(signum).name)
    def timed_out(_signum: int, _frame: Any) -> None:
        raise TimeoutError("Bounded automatic-dispatch case exceeded its deadline")
    signal.signal(signal.SIGINT, interrupted)
    signal.signal(signal.SIGTERM, interrupted)
    signal.signal(signal.SIGALRM, timed_out)
    return JobHandoffDispatchLiveCheck(args).run()


if __name__ == "__main__":
    raise SystemExit(main())
