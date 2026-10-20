#!/usr/bin/env python3
"""Two hours of resident coordination, time-distributed real work and learning.

The orchestrator supplies a freshly compiled, source-frozen CLI. This script
never builds, installs, mocks completions, changes the real configuration or
substitutes its selected provider/model/endpoint. Bootstrap is real model work
and excluded from the clock. A coordinator PID and learning watcher PID remain
resident throughout at least 24 order-processing cycles spanning two hours.
Worker CLI children rotate per task as coordinator serve actually implements.
Every outcome destroys owned processes, CLI/credential copies and workspace.
"""
from __future__ import annotations

import argparse
from datetime import datetime
import hashlib
import http.client
import json
import os
from pathlib import Path
import random
import re
import shlex
import shutil
import signal
import subprocess
import sys
import time
import threading
import tempfile
import tomllib
from typing import Any
from urllib.parse import urlsplit

from persistent_agent_live_check import (PersistentAgentLiveCheck, atomic_json, digest, envelope_count,
    go_history_sha, history_snapshots, json_sha, require, tool_calls, toml_table, trace_proof, utc_now, workflow_result)


def private_header_timeout(text: str, provider: str, seconds: int) -> tuple[str, Any]:
    """Change exactly the selected cloned profile, never the source config."""
    require(seconds == 300, "The soak transport override must remain explicitly bounded at 300 seconds")
    before = tomllib.loads(text)
    profile = before["provider"]["custom"][provider]
    original = profile.get("timeout_seconds")
    replacement = {**profile, "timeout_seconds": seconds}
    table_header = toml_table(["provider", "custom", provider], replacement).splitlines()[0]
    lines = text.splitlines(keepends=True)
    starts = [i for i, line in enumerate(lines) if line.strip() == table_header]
    require(len(starts) == 1, "Private selected provider table is not uniquely identifiable")
    start = starts[0]
    end = next((i for i in range(start + 1, len(lines)) if lines[i].lstrip().startswith("[")), len(lines))
    modified = "".join(lines[:start]) + toml_table(["provider", "custom", provider], replacement) + "\n\n" + "".join(lines[end:])
    expected = json.loads(json.dumps(before))
    expected["provider"]["custom"][provider] = replacement
    require(tomllib.loads(modified) == expected, "Private timeout override changed another configuration field")
    return modified, original


TRANSPORT_LINE = re.compile(r"^(?P<utc>\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}\.\d{3}Z) \[http\] "
    r"(?P<provider>[A-Za-z0-9_.-]+) (?P<method>[A-Z]+) (?P<path>/\S*) "
    r"(?P<status>ERR:.+|[1-5]\d{2}|—) elapsed=(?P<elapsed>(?:\d+(?:\.\d+)?(?:h|ms|µs|us|ns|m|s))+) "
    r"reqid=(?P<reqid>[a-f0-9]{8,64})$")


def transport_error_phase(error: str) -> tuple[str, str]:
    """Only known diagnostic text crosses the boundary; arbitrary errors may echo secrets."""
    value = error.lower()
    if "timeout awaiting response headers" in value:
        return "response_headers", "timeout awaiting response headers"
    if "tls" in value or "certificate" in value or "x509" in value:
        return "tls", "TLS transport failure"
    if "no such host" in value or "lookup " in value:
        return "dns", "DNS lookup failure"
    if "dial " in value or "connection refused" in value or "connect:" in value:
        return "connect", "connection establishment failure"
    if "context canceled" in value or "context deadline exceeded" in value:
        return "context", "request context canceled or expired"
    if "eof" in value or "connection reset" in value or "broken pipe" in value:
        return "transport", "connection ended before response headers"
    if "i/o timeout" in value:
        return "transport", "transport I/O timeout; exact phase unproven"
    return "unclassified", "unclassified transport error; unsafe arbitrary text omitted"


def sanitized_transport_log(raw: str, redact: Any) -> tuple[str, dict[str, Any]]:
    """Preserve reqid/status/timing proof, discard any unrecognized header/body line."""
    rows, retained, rejected = [], [], 0
    units = {"h": 3600, "m": 60, "s": 1, "ms": .001, "µs": .000001, "us": .000001, "ns": .000000001}
    for line in raw.splitlines():
        match = TRANSPORT_LINE.fullmatch(line)
        if not match:
            rejected += 1
            continue
        row = match.groupdict()
        datetime.fromisoformat(row["utc"].replace("Z", "+00:00"))
        elapsed = sum(float(value) * units[unit] for value, unit in re.findall(r"(\d+(?:\.\d+)?)(h|ms|µs|us|ns|m|s)", row["elapsed"]))
        path = redact(row.pop("path")).split("?", 1)[0]
        path = re.sub(r"(?<=/)[A-Za-z0-9_-]{24,}(?=/|$)", "[REDACTED-PATH]", path)
        path = path if re.fullmatch(r"/[A-Za-z0-9_.%/\[\]-]{0,256}", path) else "[REDACTED-PATH]"
        status = row["status"]
        if status.startswith("ERR:"):
            phase, description = transport_error_phase(status[4:])
            status = "ERR:" + description
            row.update({"status": "error", "http_status": None, "error_phase": phase, "error_description": description,
                "raw_error_sha256": hashlib.sha256(row["status"][4:].encode()).hexdigest()})
        else:
            row.update({"status": "response_headers_received" if status != "—" else "unclassified",
                "http_status": int(status) if status != "—" else None, "error_phase": None})
        row.update({"path": path, "elapsed_seconds": elapsed})
        retained.append(f"{row['utc']} [http] {row['provider']} {row['method']} {path} {status} elapsed={row['elapsed']} reqid={row['reqid']}\n")
        rows.append(row)
    errors = sorted((row for row in rows if row["error_phase"]), key=lambda row: row["utc"])
    by_reqid: dict[str, list[dict[str, Any]]] = {}
    for row in rows: by_reqid.setdefault(row["reqid"], []).append(row)
    return "".join(retained), {"observations": rows, "by_reqid": by_reqid, "first_transport_error": errors[0] if errors else None,
        "retained_lines": len(rows), "rejected_unrecognized_lines": rejected,
        "boundary": "HTTP RoundTrip headers/network observations only; receipt of HTTP headers does not prove completed model usage. Unknown errors are classified conservatively, not inferred. Headers and bodies are never retained."}


def aggregate(data: dict[str, Any]) -> dict[str, Any]:
    groups = sorted({row["group"] for row in data["orders"]})
    return {"cycle": data["cycle"], "nonce": data["nonce"], "total_count": len(data["orders"]),
        "total_amount": sum(row["amount"] for row in data["orders"]),
        "groups": {name: {"count": sum(row["group"] == name for row in data["orders"]),
                          "amount": sum(row["amount"] for row in data["orders"] if row["group"] == name)} for name in groups}}


def cycle_offsets(duration: float, count: int) -> list[float]:
    require(duration >= 7200 and count >= 24, "Long acceptance requires at least 7200 seconds and 24 cycles")
    require(duration / (count - 1) >= 300, "Real cycles require at least five minutes between scheduled starts")
    return [duration * index / (count - 1) for index in range(count)]


def heartbeat_gap_valid(monotonic_gap: float, wall_gap: float) -> bool:
    return 0 < monotonic_gap <= 30 and 0 < wall_gap <= 30 and abs(wall_gap - monotonic_gap) <= 5


def completion_workers(run: dict[str, Any], item_ids: list[str]) -> dict[str, str]:
    """Completed items clear Owner/Lease; the durable completion event owns it."""
    result = {}
    for item_id in item_ids:
        events = [value for value in run.get("events", []) if value.get("kind") == "work_completed" and value.get("item_id") == item_id]
        require(len(events) == 1 and events[0].get("worker"), "Current cycle lacks one durable completion worker event")
        worker = events[0]["worker"]
        rows = [value for value in run.get("usage_requests", []) if value.get("item_id") == item_id]
        require(rows and any(value.get("worker") == worker and value.get("status") == "completed" for value in rows),
            "Completion worker is not backed by its real physical provider ledger")
        result[item_id] = worker
    return result


CALCULATOR = """import hashlib,json,sys
from pathlib import Path
op=sys.argv[1]
if op=="fingerprint":
 p=Path(sys.argv[2]); print(json.dumps({"sha256":hashlib.sha256(p.read_bytes()).hexdigest(),"value":json.loads(p.read_text())})); raise SystemExit(0)
data=json.loads(Path(sys.argv[2]).read_text()); result={"cycle":data["cycle"],"nonce":data["nonce"],"total_count":0,"total_amount":0,"groups":{}}
for row in data["orders"]:
 result["total_count"]+=1; result["total_amount"]+=row["amount"]
 group=result["groups"].setdefault(row["group"],{"count":0,"amount":0}); group["count"]+=1; group["amount"]+=row["amount"]
if op=="aggregate": print(json.dumps(result,sort_keys=True)); raise SystemExit(0)
if op=="verify":
 p=Path(sys.argv[3]); value=json.loads(p.read_text()); valid=all(value.get(k)==v for k,v in result.items())
 print(json.dumps({"valid":valid,"cycle":data["cycle"],"nonce":data["nonce"],"sha256":hashlib.sha256(p.read_bytes()).hexdigest()})); raise SystemExit(0 if valid else 7)
raise SystemExit(8)
"""


CHECKER = """import json,sys,time
from pathlib import Path
plan=json.loads(Path(sys.argv[1]).read_text()); workspace=Path(plan["workspace"])
run=json.loads(Path(plan["run_path"]).read_text()); items={item["id"]:item for item in run["items"]}
if items.get("implementation",{}).get("status")=="completed":
 assert json.loads((workspace/"bootstrap-ready.json").read_text())==plan["bootstrap"], "bootstrap artifact mismatch"
checked=0
for cycle in plan["cycles"]:
 if items.get(cycle["event_item_id"],{}).get("status")!="completed": continue
 deadline=time.monotonic()+10
 while not all(name in items for name in (cycle["producer_id"],cycle["receiver_id"])):
  assert time.monotonic()<deadline, "bound admission did not finish within host check deadline"
  time.sleep(0.02); run=json.loads(Path(plan["run_path"]).read_text()); items={item["id"]:item for item in run["items"]}
 assert items[cycle["producer_id"]].get("principal_binding",{}).get("job_id")==plan["producer_job"], "producer principal binding mismatch"
 assert items[cycle["receiver_id"]].get("principal_binding",{}).get("job_id")==plan["recipient_job"], "recipient principal binding mismatch"
 expected={"cycle":cycle["cycle"],"nonce":cycle["nonce"],"input_sha256":cycle["input_sha256"]}
 assert json.loads((workspace/cycle["arrival"]).read_text())==expected, "completed generic event artifact mismatch"
 checked+=1
print(json.dumps({"host_checked_generic_completed_items":checked,"passed":True}))
"""


WORKSPACE_RULES = ("This is a private order-data fixture workspace, not a Git repository or an application codebase. "
    "The host has already established the environment: do not inspect .git, run Git commands, or discover another project. "
    "Use Read/Write for requested JSON data and the preinstalled readonly calculator/checker for host validation. "
    "For each Bash tool call, execute exactly one simple foreground command with its arguments, such as the supplied Python helper invocation. "
    "Do not combine commands with &&, ||, semicolons or pipes; do not use if/then/else, loops, heredocs, command substitution, shell redirection or background execution. "
    "Call independent helpers in separate Bash tool calls. These command-shape rules apply to Bash tool calls. ")

BOOTSTRAP_INSTRUCTIONS = ("Work only on the current durable task, never delegate or modify helpers/checkers/credentials. "
    "For the initial four scaffold items, inspect only bootstrap-input.json and bootstrap-ready.json; there is no source code to implement or repository metadata to verify. "
    "Bootstrap research/synthesis: use Read on bootstrap-input.json and bootstrap-ready.json, report concrete data findings and the minimal copy/validation plan. "
    "Bootstrap implementation: fully Read bootstrap-ready.json then Write exactly the JSON from bootstrap-input.json there; Read back with limit=500. "
    "Bootstrap verification: Read both bootstrap files, compare their JSON values, and report concrete evidence; any supplied readonly checker must be invoked as one simple Bash command. "
    "These bootstrap instructions apply only to the initial four scaffold tasks. Later event/job tasks follow their own prompts. ")


class PersistentAgentSoak(PersistentAgentLiveCheck):
    def __init__(self, args: argparse.Namespace) -> None:
        super().__init__(args)
        self.services: list[dict[str, Any]] = []
        self.active_http: set[http.client.HTTPConnection] = set()
        self.known_children: set[int] = set()
        self.cycles: list[dict[str, Any]] = []
        self.last_sample = 0.0
        self.max_sample_gap = 0.0
        self.sample_lock = threading.RLock()
        self.heartbeat_stop = threading.Event()
        self.heartbeat_failure: str | None = None
        self.owned_identities: dict[int, dict[str, Any]] = {}
        self.intentional_stops: set[int] = set()
        self.report.update({"mode": "persistent-agent-soak", "minimum_resident_seconds": args.duration,
            "scheduled_cycles": args.cycles, "business_cycles": [],
            "boundary": "Two resident coordinator supervisors share one graph; CLI workers rotate per task. HTTP events create generic implementation/verification; coordinator add --job binds two fixed principals. Worker SIGKILL occurs after terminal provider usage inside actual regular Read, never with invented upstream receipts. Catalog spend is an estimate, not an invoice."})

    def coord(self, verb: str, *args: str, label: str) -> tuple[Any, dict[str, Any]]:
        return self.command(["coordinator", verb, *args, "--cwd", str(self.workspace), "--json"], label)

    def read_run(self) -> dict[str, Any]:
        return json.loads(self.run_path.read_text())

    def prepare(self) -> None:
        self.stage()
        self.load_route()
        self.configure_transport_diagnostics()
        config_path = self.home / "config.toml"
        text = config_path.read_text().replace('"Workflow", "Agent"]', '"Workflow", "Agent", "Bash"]')
        require('"Bash"]' in text, "Private fixture failed to enable actual Bash calculator")
        config_path.write_text(text)
        self.producer_contract = "SOAK_PRODUCER_" + os.urandom(8).hex()
        self.receiver_contract = "SOAK_RECEIVER_" + os.urandom(8).hex()
        self.calculator, self.checker, self.plan_path = self.root / "readonly-calculator.py", self.root / "immutable-checker.py", self.root / "host-plan.json"
        self.calculator.write_text(CALCULATOR)
        self.checker.write_text(CHECKER)
        self.calculator.chmod(0o400)
        self.checker.chmod(0o400)
        self.checker_sha, self.calculator_sha = digest(self.checker), digest(self.calculator)
        bootstrap = {"fixture": "persistent-order-workspace", "nonce": os.urandom(8).hex()}
        atomic_json(self.workspace / "bootstrap-input.json", bootstrap)
        atomic_json(self.workspace / "bootstrap-ready.json", {})
        (self.home / "system.md").write_text(WORKSPACE_RULES + BOOTSTRAP_INSTRUCTIONS)
        self.profile("soak-arrival", ["Read", "Write", "Bash"], "Read the current input and produce the requested arrival receipt from cycle, nonce and actual SHA. Fully Read the existing arrival file before Write and verify after Write. Do not calculate an order report or process another cycle. End with DONE.")
        checker_command = shlex.quote(sys.executable) + " " + shlex.quote(str(self.checker)) + " " + shlex.quote(str(self.plan_path))
        self.profile("soak-verifier", ["Read", "Bash"], "Inspect only the current generic event arrival receipt and input. Derive N from current subject cycle-N; input is cycle-NN-input.json and arrival is cycle-NN-arrival.json (two-digit NN). Use Bash " + checker_command + " and report actual validation. Never write or assert completion of separate persistent producer/recipient tasks.")
        self.profile("soak-producer", ["Read", "Write", "Bash", "MessageTeammate"], "Use the host readonly standard-library calculator for arithmetic. Fully Read existing output before Write. Include contract=" + self.producer_contract + " in report JSON. Obtain actual report SHA after Write, send exactly one durable MessageTeammate with requested logical key and body-only handoff nonce. Final response exactly DONE; never echo message body or handoff nonce in prose. If supplied a large gate, Read its first line with limit=1 before any other tool.")
        self.profile("soak-recipient", ["Read", "Write", "Bash"], "Use the delivered durable envelope body/artifact references. Independently validate the report using the host readonly calculator. Fully Read existing receipt before Write, include envelope-only handoff_nonce and contract=" + self.receiver_contract + ". Verify receipt with Read limit=500. Final response exactly DONE. Never ask sender to repeat inbox values.")
        self.producer = self.create_budgeted_job("soak-producer")
        self.recipient = self.create_budgeted_job("soak-recipient")
        run, _ = self.coord("create", "--goal", "Bootstrap this private order-data fixture workspace by copying bootstrap-input.json into bootstrap-ready.json with Read/Write and verifying the JSON values. "
            "It is not a Git repository or application codebase; do not inspect .git or run Git commands. Bash helpers must be single simple foreground invocations, without compound shell control flow or redirection. "
            "Then process incoming batches independently and retain checked results.",
            "--check", shlex.quote(sys.executable) + " " + shlex.quote(str(self.checker)) + " " + shlex.quote(str(self.plan_path)), label="create-real-soak-graph")
        self.run_id = run["id"]
        self.run_path = self.home / "project-coordinator" / hashlib.sha256(str(self.workspace).encode()).hexdigest()[:32] / (self.run_id + ".json")
        require(self.run_path.is_file(), "Graph path differs from canonical workspace binding")
        self.plan = {"workspace": str(self.workspace), "run_path": str(self.run_path), "bootstrap": bootstrap, "cycles": [],
            "producer_job": self.producer["id"], "recipient_job": self.recipient["id"]}
        atomic_json(self.plan_path, self.plan)
        self.service_token = os.urandom(32).hex()
        self.secrets.append(self.service_token)
        self.token_path = self.root / "service-token"
        self.token_path.write_text(self.service_token + "\n")
        self.token_path.chmod(0o600)
        self.setup_learning()
        before = self.trace_snapshot()
        self.service_a = self.start_service("primary-A")
        self.service_b = self.start_service("contender-B")
        self.start_watcher()
        self.await_items({"research", "synthesis", "implementation", "verification"}, self.args.query_timeout * 4)
        bootstrap_proof, _ = self.real_proof(before, "real-bootstrap-before-soak-clock", required_tools=("Read", "Write"))
        require(json.loads((self.workspace / "bootstrap-ready.json").read_text()) == bootstrap, "Real bootstrap artifact mismatch")
        self.await_idle()
        self.soak_started = time.monotonic()
        self.soak_started_utc = utc_now()
        self.report["bootstrap"] = {"real_model_proof": bootstrap_proof, "completed_before_soak_clock": True, "elapsed_seconds": round(self.soak_started - self.started, 3)}
        self.report["resident_start"] = {"utc": self.soak_started_utc, "primary_PID": self.service_a["process"].pid, "watcher_PID": self.watcher[0].pid}
        self.sample(force=True)
        self.heartbeat = threading.Thread(target=self.heartbeat_loop, name="independent-soak-heartbeat", daemon=True)
        self.heartbeat.start()

    def configure_transport_diagnostics(self) -> None:
        config_path = self.home / "config.toml"
        modified, original_timeout = private_header_timeout(config_path.read_text(), self.provider, 300)
        config_path.write_text(modified)
        config_path.chmod(0o600)
        self.settings["timeout_seconds"] = 300
        self.transport_log = self.home / "transport-debug.log"
        require(self.transport_log.is_absolute() and self.transport_log.parent == self.home,
            "Transport diagnostic path must stay inside the private METIS_HOME")
        self.env.update({"METIS_DEBUG": "1", "METIS_DEBUG_LOG": str(self.transport_log)})
        require(not any(name.startswith("METIS_DUMP") or name in ("METIS_DEBUG_OPENAI", "METIS_DEBUG_GEMINI") for name in self.env),
            "Body/header provider dumps must not be enabled")
        self.report["original_route"]["private_parameter_overrides"]["timeout_seconds"] = 300
        self.report["transport_diagnostics"] = {"METIS_DEBUG": "1", "private_absolute_log_path": str(self.transport_log),
            "selected_profile_original_timeout_seconds": original_timeout, "default_if_missing_or_nonpositive_seconds": 120,
            "private_response_header_timeout_seconds": 300, "harness_query_deadline_seconds": self.args.query_timeout,
            "whole_HTTP_client_timeout_seconds_unchanged": 1200, "harness_total_deadline_seconds": self.args.case_timeout,
            "model_endpoint_and_transport_unchanged": True, "timeout_change_proves_upstream_repair": False,
            "coverage": "Coordinator service/worker and ordinary harness query transports inherit both debug variables. Learning evaluation children filter METIS_* and do not retain extra transport debug; their existing model usage/trace receipts remain required.",
            "headers_or_bodies_retained": False, "original_configuration_mutated": False}

    def profile(self, name: str, tools: list[str], body: str) -> None:
        (self.home / "agents" / (name + ".md")).write_text("---\nname: " + name + "\ntools: [" + ", ".join(tools) + "]\npermission_mode: dontAsk\nmax_turns: 12\n---\n" + WORKSPACE_RULES + body + "\n")

    def create_budgeted_job(self, name: str) -> dict[str, Any]:
        value, _ = self.job("create", "--name", name, "--cwd", str(self.workspace), "--agent", name, "--provider", self.provider,
            "--model", self.model, "--max-input-tokens", "8000000", "--max-output-tokens", "200000", label="create-" + name)
        require(value.get("id") and value.get("session_id"), "Principal lacks stable identity/session")
        self.jobs[name] = value
        return value

    def logs(self, service: dict[str, Any]) -> list[dict[str, Any]]:
        values = []
        for line in service["stderr"].read_text(errors="replace").splitlines():
            try: value = json.loads(line)
            except ValueError: continue
            if value.get("kind"):
                values.append(value)
                if value.get("kind") == "worker_started": self.known_children.add(value["pid"])
        return values

    def start_service(self, name: str) -> dict[str, Any]:
        child = self.spawn(["coordinator", "serve", self.run_id, "--cwd", str(self.workspace), "--agent", "soak-arrival", "--verify-agent", "soak-verifier",
            "--token-file", str(self.token_path), "--workers", "1", "--poll", "100ms", "--lease", "10s"], "resident-service-" + name)
        value = {"name": name, "process": child[0], "stdout": child[1], "stderr": child[2]}
        self.services.append(value)
        deadline = time.monotonic() + 30
        while time.monotonic() < deadline:
            require(child[0].poll() is None, "Coordinator service exited before readiness")
            ready = next((event for event in self.logs(value) if event.get("kind") == "service_ready"), None)
            if ready:
                require(ready.get("run_id") == self.run_id, "Service attached to another graph")
                value.update({"url": "http://" + ready["listen"], "service_id": ready["service_id"], "ready_monotonic": time.monotonic()})
                code, status = self.http(value, "GET", "/v1/health")
                require(code == 200 and status.get("run_id") == self.run_id, "Authenticated service health unavailable")
                return value
            time.sleep(0.1)
        raise AssertionError("Service readiness timeout")

    def http(self, service: dict[str, Any], method: str, path: str, value: Any = None) -> tuple[int, Any]:
        address = urlsplit(service["url"])
        require(address.hostname == "127.0.0.1", "Service must remain owned loopback")
        connection = http.client.HTTPConnection(address.hostname, address.port, timeout=15)
        self.active_http.add(connection)
        try:
            connection.request(method, path, None if value is None else json.dumps(value).encode(), {"Authorization": "Bearer " + self.service_token, "Content-Type": "application/json", "Connection": "close"})
            response = connection.getresponse()
            raw = response.read(1 << 20)
            return response.status, json.loads(raw) if raw else None
        finally:
            connection.close()
            self.active_http.discard(connection)

    def service_active_pids(self, service: dict[str, Any]) -> set[int]:
        values: set[int] = set()
        for event in self.logs(service):
            if event["kind"] == "worker_started": values.add(event["pid"])
            elif event["kind"] in ("worker_finished", "worker_idle"): values.discard(event.get("pid"))
        return values

    def await_idle(self) -> None:
        deadline = time.monotonic() + 30
        while time.monotonic() < deadline:
            if all(not self.service_active_pids(service) for service in self.services if service["process"].poll() is None): return
            self.sample(); time.sleep(0.1)
        raise AssertionError("Finished task-scoped workers did not exit")

    def await_items(self, identifiers: set[str], timeout: float) -> dict[str, Any]:
        deadline = time.monotonic() + timeout
        while time.monotonic() < deadline:
            self.sample()
            run = self.read_run()
            items = {item["id"]: item for item in run["items"]}
            require(identifiers <= items.keys(), "Scheduled items disappeared")
            bad = [items[name] for name in identifiers if items[name]["status"] in ("failed", "blocked", "cancelled")]
            require(not bad, "Business item failed/held: " + json.dumps([{"id": item["id"], "status": item["status"], "failure": item.get("failure")} for item in bad]))
            if all(items[name]["status"] == "completed" for name in identifiers): return run
            time.sleep(0.2)
        raise AssertionError("Real business work exceeded bounded completion deadline")

    def check_heartbeat(self) -> None:
        require(not self.heartbeat_failure, "Independent residency heartbeat failed: " + str(self.heartbeat_failure))

    def heartbeat_loop(self) -> None:
        previous_mono, previous_wall = time.monotonic(), time.time()
        try:
            while not self.heartbeat_stop.wait(5):
                now_mono, now_wall = time.monotonic(), time.time()
                gap, wall_gap = now_mono - previous_mono, now_wall - previous_wall
                require(heartbeat_gap_valid(gap, wall_gap), "Host suspend/clock discontinuity or heartbeat gap exceeds acceptance bound")
                previous_mono, previous_wall = now_mono, now_wall
                self.capture_owned()
                self.sample(force=True)
        except BaseException as error:
            self.heartbeat_failure = self.redact(str(error))

    def finish(self, child: Any, label: str, **options: Any) -> tuple[Any, dict[str, Any]]:
        timeout = options.get("operation_timeout") or self.args.query_timeout
        deadline = time.monotonic() + timeout
        while child[0].poll() is None and time.monotonic() < deadline:
            self.check_heartbeat(); time.sleep(0.2)
        self.check_heartbeat()
        if child[0].poll() is None:
            self.capture_owned()
            self.stop(child[0])
            raise AssertionError(label + " exceeded bounded CLI deadline")
        return super().finish(child, label, **options)

    def capture_owned(self) -> dict[int, dict[str, Any]]:
        raw = subprocess.run(["ps", "-axo", "pid=,ppid=,pgid=,stat=,lstart=,command="], capture_output=True, text=True, timeout=5).stdout
        table = {}
        for line in raw.splitlines():
            fields = line.split(None, 9)
            if len(fields) == 10 and all(value.isdigit() for value in fields[:3]):
                pid, ppid, pgid = map(int, fields[:3])
                table[pid] = {"pid": pid, "ppid": ppid, "pgid": pgid, "stat": fields[3], "birth": " ".join(fields[4:9]), "command": fields[9]}
        roots = {value.pid for value in self.processes if value.poll() is None}
        roots |= {pid for pid in self.known_children if pid in table and
            (str(self.binary) in table[pid]["command"] or self.owned_identities.get(pid, {}).get("birth") == table[pid]["birth"])}
        owned = roots | {pid for pid, row in table.items() if str(self.root) in row["command"] and pid != os.getpid()}
        changed = True
        while changed:
            children = {pid for pid, row in table.items() if row["ppid"] in owned and pid != os.getpid()}
            changed = not children <= owned
            owned |= children
        with self.sample_lock:
            for pid in owned & table.keys():
                row = table[pid]
                require(row["pgid"] != os.getpgrp(), "Owned descendant unexpectedly shares harness group")
                self.owned_identities[pid] = row
        return table

    def sample(self, *, force: bool = False) -> None:
        with self.sample_lock:
            self._sample_locked(force=force)

    def _sample_locked(self, *, force: bool = False) -> None:
        now = time.monotonic()
        if not force and now - self.last_sample < 15: return
        if self.last_sample:
            self.max_sample_gap = max(self.max_sample_gap, now - self.last_sample)
            if hasattr(self, "heartbeat"): require(now - self.last_sample <= 30, "Resource heartbeat paused beyond acceptance bound")
        self.last_sample = now
        if hasattr(self, "service_a"): require(self.service_a["process"].poll() is None, "Primary coordinator exited")
        if hasattr(self, "watcher"):
            require(self.watcher[0].poll() is None, "Resident watcher exited")
            require(not any(tick.get("receipt", {}).get("status") in ("unknown", "promotion_failed") for tick in self.watcher_ticks()), "Uncertain learning evaluation or failed promotion")
        pids = {service["process"].pid for service in self.services if service["process"].poll() is None}
        for service in self.services: pids.update(self.service_active_pids(service))
        if hasattr(self, "watcher"): pids.add(self.watcher[0].pid)
        processes = []
        for pid in sorted(pids):
            row = subprocess.run(["ps", "-p", str(pid), "-o", "stat=,rss=,vsz=,%cpu=,etime="], capture_output=True, text=True, timeout=5).stdout.strip()
            if row:
                fields = row.split()
                require(len(fields) >= 5 and fields[1].isdigit(), "Process resource sample is malformed")
                require(not fields[0].startswith("T") or pid in self.intentional_stops, "Resident/worker process was externally stopped")
                require(int(fields[1]) < 1572864, "Owned CLI exceeded the per-process 1.5 GiB RSS allowance")
                if hasattr(self, "service_a") and pid == self.service_a["process"].pid:
                    require(int(fields[1]) < 524288, "Primary supervisor exceeded its 512 MiB RSS allowance")
                processes.append({"pid": pid, "rss_kb": int(fields[1]), "ps_stat_rssKB_vszKB_cpu_etime": row})
        run = self.read_run()
        require(len(run.get("operations", [])) < 900, "Operation journal approaching bounded capacity")
        require(not any(record.get("status") == "uncertain" for record in run.get("operations", [])), "Uncertain writing operation")
        require(not any(record.get("status") == "interrupted" or (record.get("status") == "completed" and not record.get("usage_reported")) for record in run.get("usage_requests", [])), "Upstream usage is unknown; automatic resend is not accepted")
        priced_rows = [record for record in run.get("usage_requests", []) if record.get("prices", {}).get("input_per_million_usd", 0) > 0 or record.get("prices", {}).get("output_per_million_usd", 0) > 0]
        require(sum(record.get("estimated_usd", 0) for record in priced_rows) <= 20, "Known catalog-estimated consumption exceeded the bounded estimate")
        require(digest(self.checker) == self.checker_sha and digest(self.calculator) == self.calculator_sha, "Host helper/checker bytes changed")
        size = 0
        for path in self.home.rglob("*"):
            try:
                if path.is_file(): size += path.stat().st_size
            except FileNotFoundError: pass
        require(size < 512 * (1 << 20), "Private state exceeded bounded disk allowance")
        value = {"utc": utc_now(), "monotonic": now, "elapsed_soak_seconds": round(now - self.soak_started, 3) if hasattr(self, "soak_started") else None,
            "processes": processes, "run_revision": run.get("revision"), "items": len(run["items"]), "operations": len(run.get("operations", [])),
            "usage_requests": len(run.get("usage_requests", [])), "metis_home_bytes": size, "run_file_bytes": self.run_path.stat().st_size}
        with (self.output / "resident-resource-samples.jsonl").open("a") as target: target.write(json.dumps(value) + "\n")
        # A dedicated append-only heartbeat file avoids racing the main
        # thread's atomic progress.json writer.
        with (self.output / "independent-heartbeat.jsonl").open("a") as target: target.write(json.dumps(value) + "\n")

    def setup_learning(self) -> None:
        self.workflow_name = "soak-arithmetic"
        root = self.home / "workflows"
        root.mkdir(mode=0o700, exist_ok=True)
        baseline = {"name": self.workflow_name, "steps": [{"name": "answer", "command": "printf '{\"answer\":322}\\n'"}]}
        self.baseline_file = root / (self.workflow_name + ".json")
        atomic_json(self.baseline_file, baseline)
        self.baseline_version = digest(self.baseline_file)
        self.workflow_suite = self.root / "fixed-workflow-suite.json"
        atomic_json(self.workflow_suite, {"name": "fixed-real-step-output", "cases": [
            {"id": "one", "prompt": "Run the named workflow once.", "expected_json": {"answer": 323}},
            {"id": "two", "prompt": "Run the named workflow in this independent case.", "expected_json": {"answer": 323}}],
            "max_tokens": 100000, "max_duration_seconds": int(self.args.query_timeout)})
        self.learning_policy, _ = self.command(["learning", "configure", "--kind", "workflow", "--name", self.workflow_name, "--suite", str(self.workflow_suite),
            "--max-evaluations", "2", "--max-total-tokens", "1000000", "--auto-promote"], "configure-bounded-resident-learning")
        require(self.learning_policy["max_evaluations"] == 2 and self.learning_policy["identity"]["binary_sha256"] == self.input_sha, "Unexpected resident evaluator allowance/identity")

    def watcher_ticks(self) -> list[dict[str, Any]]:
        values = []
        for line in self.watcher[1].read_text(errors="replace").splitlines():
            try: value = json.loads(line)
            except ValueError: continue
            if isinstance(value, dict): values.append(value)
        return values

    def start_watcher(self) -> None:
        self.watcher = self.spawn(["learning", "watch", "--kind", "workflow", "--name", self.workflow_name, "--interval", "5s"], "resident-learning-watcher")
        deadline = time.monotonic() + 30
        while time.monotonic() < deadline:
            require(self.watcher[0].poll() is None, "Watcher exited before readiness")
            ticks = self.watcher_ticks()
            if ticks:
                require(ticks[0].get("idle") is True, "Watcher unexpectedly paid for a startup candidate")
                self.watcher_ready = time.monotonic()
                return
            time.sleep(0.1)
        raise AssertionError("Learning watcher readiness timeout")

    def await_learning(self, version: str, expected_status: str) -> dict[str, Any]:
        deadline = time.monotonic() + self.args.query_timeout * 4 + 30
        while time.monotonic() < deadline:
            self.sample()
            ticks = self.watcher_ticks()
            matches = [tick["receipt"] for tick in ticks if tick.get("receipt", {}).get("candidate_sha256") == version]
            if matches:
                require(len(matches) == 1 and matches[0]["status"] == expected_status, "Resident watcher rejected/activated the wrong candidate outcome")
                state, _ = self.command(["learning", "list", "--kind", "workflow", "--name", self.workflow_name], "resident-learning-persisted-evaluation")
                evaluations = [value for value in state.get("evaluations", []) if value["id"] == matches[0]["evaluation_id"]]
                require(len(evaluations) == 1, "Resident learning receipt lost its exact evaluation")
                evaluation = evaluations[0]
                self.assert_evaluation(evaluation, self.workflow_name, version, self.baseline_version, kind="workflow")
                require(matches[0]["charged_tokens"] == evaluation["baseline"]["tokens"] + evaluation["candidate"]["tokens"], "Watcher did not settle complete reported evaluation usage")
                return {"receipt": matches[0], "evaluation": evaluation}
            time.sleep(0.2)
        raise AssertionError("Resident watcher did not process the newly proposed version")

    def learning_checkpoint(self, cycle: int) -> None:
        if cycle == 2:
            bad = self.root / "bad-workflow.json"
            atomic_json(bad, {"name": self.workflow_name, "steps": [{"name": "wrong", "command": "printf '{\"answer\":999}\\n'"}]})
            version, _ = self.command(["learning", "stage", "--kind", "workflow", "--name", self.workflow_name, "--file", str(bad)], "soak-bad-workflow-proposal")
            result = self.await_learning(version["id"], "rejected")
            require(result["evaluation"]["passed"] is False, "Bad workflow passed its immutable host suite")
            self.report["learning_bad"] = result
        elif cycle == 6:
            recipe = {"name": self.workflow_name, "steps": [{"name": "compute", "command": "printf '{\"answer\":%s}\\n' \"$((17 * 19))\""}]}
            before = self.trace_snapshot()
            self.command(["run", "--bare", "--tools", "Workflow", "Call Workflow exactly once with operation=save using " + json.dumps({"operation": "save", **recipe}) + ". Do not run it or call another tool; return STAGED."],
                "soak-real-model-workflow-proposal", json_output=False)
            proposal, rows = self.real_proof(before, "soak-real-model-workflow-proposal", required_tools=("Workflow",))
            calls = tool_calls(rows, "Workflow")
            require(len(calls) == 1 and calls[0]["arguments"].get("operation") == "save" and calls[0]["arguments"].get("steps") == recipe["steps"], "Model did not stage the expected workflow recipe")
            state, _ = self.command(["learning", "list", "--kind", "workflow", "--name", self.workflow_name], "soak-workflow-proposal-state")
            versions = [value for value in state["versions"].values() if value.get("source") == "model-workflow-proposal" and json.loads(value["content"]) == recipe]
            require(len(versions) == 1, "Model workflow proposal lost its unique immutable identity")
            self.good_version = versions[0]["id"]
            self.report["learning_good"] = self.await_learning(self.good_version, "promoted")
            self.report["learning_good"]["proposal"] = proposal
            self.report["learning_good"]["ordinary_query"] = self.ordinary_workflow_query(self.workflow_name, self.good_version, {"answer": 323}, "soak-query-uses-promoted-workflow")
        elif cycle == 12:
            self.command(["learning", "rollback", "--kind", "workflow", "--name", self.workflow_name, "--version", self.baseline_version], "soak-workflow-rollback")
            self.report["learning_rollback_query"] = self.ordinary_workflow_query(self.workflow_name, self.baseline_version, {"answer": 322}, "soak-query-uses-rolled-back-workflow")

    def add_job_item(self, identifier: str, job: dict[str, Any], prompt: str, deliverable: str, dependencies: list[str]) -> dict[str, Any]:
        value, _ = self.coord("add", self.run_id, "--id", identifier, "--job", job["id"], "--phase", "custom", "--subject", identifier,
            "--prompt", prompt, "--deliverable", deliverable, "--depends-on", ",".join(dependencies), "--max-attempts", "3", label="queue-" + identifier)
        require(value.get("principal_binding", {}).get("job_id") == job["id"], "Resident work item was not explicitly bound to its persistent principal")
        return value

    def business_cycle(self, number: int) -> None:
        started_monotonic = time.monotonic()
        rng = random.Random(os.urandom(16))
        data = {"cycle": number, "nonce": os.urandom(8).hex(), "orders": [{"group": rng.choice(["A", "B", "C"]), "amount": rng.randrange(1, 1000)} for _ in range(64)]}
        input_name, report_name, receipt_name, arrival_name = (f"cycle-{number:02d}-{suffix}.json" for suffix in ("input", "report", "receipt", "arrival"))
        atomic_json(self.workspace / input_name, data)
        for path in (report_name, receipt_name, arrival_name): atomic_json(self.workspace / path, {})
        identifier, handoff_nonce = "cycle-" + str(number), os.urandom(16).hex()
        event_id = "event-" + hashlib.sha256(("soak\0" + identifier).encode()).hexdigest()[:32]
        plan = {"cycle": number, "nonce": data["nonce"], "input_sha256": digest(self.workspace / input_name), "arrival": arrival_name, "event_item_id": event_id,
            "producer_id": "produce-" + str(number), "receiver_id": "receive-" + str(number)}
        self.plan["cycles"].append(plan)
        atomic_json(self.plan_path, self.plan)
        source = self.workspace / input_name
        fingerprint = shlex.quote(sys.executable) + " " + shlex.quote(str(self.calculator)) + " fingerprint " + shlex.quote(str(source))
        event = {"source": "soak", "id": identifier, "subject": "Register order arrival " + identifier, "prompt":
            "Read " + str(source) + ". Use readonly Bash command " + fingerprint + " to get actual input SHA. Fully Read then Write " + str(self.workspace / arrival_name) +
            " as exactly {cycle:input cycle,nonce:input nonce,input_sha256:actual SHA}. Read back and end DONE.", "deliverables": [arrival_name]}
        before = self.trace_snapshot()
        ingress, replay_service = (self.service_a, self.service_b) if number % 2 == 0 else (self.service_b, self.service_a)
        code, accepted = self.http(ingress, "POST", "/v1/events", event)
        require(code == 202 and accepted["item_id"] == event_id, "Real external event was not durably accepted")
        before_add = self.read_run()
        require(next(item for item in before_add["items"] if item["id"] == event_id)["status"] in ("ready", "running", "claimed", "completed"), "Accepted event disappeared before bound work admission")
        producer_id, receiver_id = "produce-" + str(number), "receive-" + str(number)
        calc = shlex.quote(sys.executable) + " " + shlex.quote(str(self.calculator))
        producer_prompt = ("Use readonly Bash " + calc + " aggregate " + shlex.quote(str(source)) + " to calculate report fields. Fully Read " + str(self.workspace / report_name) +
            ", Write those report fields plus your frozen profile contract. Then use readonly Bash " + calc + " fingerprint " + shlex.quote(str(self.workspace / report_name)) +
            " to obtain actual report SHA. Send MessageTeammate to job:" + self.recipient["id"] + " with idempotency_key=" + identifier +
            " and body JSON {cycle:" + str(number) + ",report_path:" + json.dumps(report_name) + ",report_sha256:actual SHA,handoff_nonce:" + json.dumps(handoff_nonce) +
            "}. Include task.id=" + identifier + ", objective=verify report, artifacts=[{path:report_path,sha256:actual SHA}]. Return only DONE.")
        if number == 10:
            self.gate = self.workspace / "soak-read-gate.txt"
            with self.gate.open("wb") as target:
                chunk = b"benign regular read padding\n" * 35000
                while target.tell() < 128 * (1 << 20): target.write(chunk[:min(len(chunk), 128 * (1 << 20) - target.tell())])
            producer_prompt = "First Read " + str(self.gate) + " limit=1 before any other tool. " + producer_prompt
        self.add_job_item(producer_id, self.producer, producer_prompt, report_name, [event_id])
        recipient_prompt = ("Use the new durable inbox envelope for cycle " + str(number) + ". Readonly Bash " + calc + " verify " + shlex.quote(str(source)) +
            " " + shlex.quote(str(self.workspace / report_name)) + " must return valid=true and actual SHA equal to the delivered artifact/body reference. Fully Read " +
            str(self.workspace / receipt_name) + ", then Write {cycle:received cycle,nonce:verified input nonce,report_sha256:actual SHA,handoff_nonce:envelope-only nonce,verified:true,contract:your frozen profile contract}. Read back limit=500 and return only DONE.")
        self.add_job_item(receiver_id, self.recipient, recipient_prompt, receipt_name, [producer_id])
        if number == 10: self.kill_active_read_worker(producer_id)
        run = self.await_items({event_id, event_id + "-verify", producer_id, receiver_id}, self.args.query_timeout * 3)
        expected_report = {**aggregate(data), "contract": self.producer_contract}
        report_proof = self.assert_artifact(report_name, expected_report)
        expected_receipt = {"cycle": number, "nonce": data["nonce"], "report_sha256": report_proof["sha256"], "handoff_nonce": handoff_nonce, "verified": True, "contract": self.receiver_contract}
        receipt_proof = self.assert_artifact(receipt_name, expected_receipt)
        self.assert_artifact(arrival_name, {"cycle": number, "nonce": data["nonce"], "input_sha256": plan["input_sha256"]})
        items = {item["id"]: item for item in run["items"]}
        require(handoff_nonce not in items[producer_id].get("output", ""), "Producer leaked the inbox-only nonce into prerequisite prose")
        listing, _ = self.command(["handoff", "list", "--scope", "jobs", "--to", "job:" + self.recipient["id"]], "cycle-handoff-receipt-" + str(number))
        messages = [value for value in listing if handoff_nonce in value.get("body", "")]
        require(len(messages) == 1 and messages[0]["state"] == "acknowledged" and messages[0]["attempts"] == 1, "Business handoff was lost or delivered more than once")
        message = messages[0]
        state = self.status(self.recipient["id"], "recipient-persisted-cycle-" + str(number))
        snapshots = history_snapshots(self.session_path(state))
        require(envelope_count(snapshots[-1], message["id"]) == 1 and any(go_history_sha(history) == message["checkpoint_sha256"] for history in snapshots),
                "Business handoff ACK has no matching durable recipient checkpoint")
        proof, rows = self.real_proof(before, "business-cycle-" + str(number), required_tools=("Bash", "Write", "MessageTeammate"))
        job_tools = {}
        for job in (self.producer, self.recipient):
            scoped_rows = [row for row in rows if row.get("session_id") == job["session_id"]]
            scoped = trace_proof(scoped_rows, self.provider, self.model)
            require(scoped["model_responses"] > 0 and not scoped["errors"] and all(scoped["tool_results"].get(tool, 0) > 0 for tool in ("Read", "Bash", "Write")),
                "A persistent role did not perform its own actual cycle tools/model calls")
            job_tools[job["name"]] = scoped
        sender_calls = tool_calls([row for row in rows if row.get("session_id") == self.producer["session_id"]], "MessageTeammate")
        require(len(sender_calls) == 1, "Producer performed multiple or no durable handoff calls")
        sender_args = sender_calls[0]["arguments"]
        require(sender_args.get("to") == "job:" + self.recipient["id"] and sender_args.get("idempotency_key") == identifier and
            json.loads(sender_args.get("body", "null")) == {"cycle": number, "report_path": report_name, "report_sha256": report_proof["sha256"], "handoff_nonce": handoff_nonce},
            "Actual producer message destination/key/body was not this business cycle")
        require(sender_args.get("task", {}).get("id") == identifier and sender_args["task"].get("artifacts") == [{"path": report_name, "sha256": report_proof["sha256"]}],
            "Actual handoff structured artifact reference differs from produced bytes")
        for item_id in (event_id, event_id + "-verify", producer_id, receiver_id):
            require(items[item_id]["attempts"] == (2 if number == 10 and item_id == producer_id else 1), "Normal work was duplicated or the controlled recovery attempt count changed")
        require(all(value["execution_attempts"] == 1 for value in run.get("operations", []) if value["item_id"] in (event_id, event_id + "-verify", producer_id, receiver_id)),
            "Guarded operation executed more than once")
        self.assert_cycle_ledgers(run, [producer_id, receiver_id])
        for job in (self.producer, self.recipient):
            current = self.status(job["id"], "stable-job-state-" + str(number) + "-" + job["name"])
            require(current["session_id"] == job["session_id"] and current["config_sha256"] == job["config_sha256"] and not current.get("unresolved_requests"), "Persistent identity changed or upstream usage is unknown")
            require(current["input_tokens"] <= 8000000 and current["output_tokens"] <= 200000, "Persistent lifetime token allowance exhausted")
        replay_before = len(run["items"])
        code, replay = self.http(replay_service, "POST", "/v1/events", event)
        require(code == 202 and replay["item_id"] == event_id and len(self.read_run()["items"]) == replay_before, "Cross-service external event replay duplicated work")
        current = {"cycle": number, "started_monotonic": started_monotonic, "completed_monotonic": time.monotonic(), "completed_utc": utc_now(),
            "event_item_id": event_id, "producer_id": producer_id, "receiver_id": receiver_id, "handoff_id": message["id"],
            "checkpoint_sha256": message["checkpoint_sha256"], "report": report_proof, "receipt": receipt_proof, "actual_model_proof": proof,
            "per_principal_actual_tools": job_tools, "actual_MessageTeammate": sender_calls[0], "ingress_service_id": ingress["service_id"],
            "completed_item_owners": completion_workers(run, [event_id, event_id + "-verify", producer_id, receiver_id])}
        self.cycles.append(current)
        self.report["business_cycles"].append(current)
        self.record("real distributed business cycle completed", **current)
        if number == 0:
            self.activity_anchor = time.monotonic()
            self.first_activity_utc = current["completed_utc"]
        if number == 5:
            self.profile("soak-producer", ["Read"], "Return MUTATED_PROFILE; do not calculate or write.")
            self.profile("soak-recipient", ["Read"], "Return MUTATED_PROFILE; do not receive or write.")
        if number == 8:
            self.await_idle()
            old = self.service_b
            self.capture_owned()
            os.killpg(old["process"].pid, signal.SIGKILL)
            old["process"].wait(timeout=10)
            self.service_b = self.start_service("contender-B-restart")
            require(self.service_b["process"].pid != old["process"].pid, "Controlled contender service did not restart in a new process")
            self.record("contender coordinator SIGKILL restart while primary remains resident", old_pid=old["process"].pid, new_pid=self.service_b["process"].pid, primary_pid=self.service_a["process"].pid)
        self.learning_checkpoint(number)
        self.sample(force=True)

    def assert_cycle_ledgers(self, run: dict[str, Any], identifiers: list[str]) -> None:
        for identifier in identifiers:
            rows = [row for row in run.get("usage_requests", []) if row["item_id"] == identifier]
            require(rows and all(row["status"] == "completed" and row.get("usage_reported") for row in rows), "Completed job has unknown or unsettled physical usage")
            require(all(row["provider"] == self.provider and row["model"] == self.model and sum(row["usage"].values()) > 0 for row in rows), "Job physical ledger lacks actual selected-route usage")

    def owner_pid(self, owner: str) -> int | None:
        for service in self.services:
            for pid in self.service_active_pids(service):
                raw = subprocess.run(["ps", "-p", str(pid), "-o", "command="], capture_output=True, text=True, timeout=5).stdout
                arguments = shlex.split(raw)
                if str(self.binary) in raw and "--worker" in arguments and arguments[arguments.index("--worker") + 1] == owner: return pid
        return None

    def kill_active_read_worker(self, item_id: str) -> None:
        require(shutil.which("lsof"), "Actual Read fault requires lsof")
        deadline, frozen, killed_pid = time.monotonic() + self.args.query_timeout, None, None
        while time.monotonic() < deadline:
            self.sample()
            run = self.read_run()
            item = next(value for value in run["items"] if value["id"] == item_id)
            rows = [value for value in run.get("usage_requests", []) if value["item_id"] == item_id]
            if rows and all(value["status"] == "completed" and value.get("usage_reported") for value in rows):
                pid = self.owner_pid(item.get("owner", ""))
                if pid:
                    with self.sample_lock:
                        self.capture_owned()
                        self.intentional_stops.add(pid)
                        os.killpg(pid, signal.SIGSTOP)
                        try:
                            names = subprocess.run([shutil.which("lsof"), "-a", "-p", str(pid), "-Fn"], capture_output=True, text=True, timeout=5).stdout
                            if "n" + str(self.gate) in names.splitlines():
                                latest = self.read_run()
                                physical = [value for value in latest.get("usage_requests", []) if value["item_id"] == item_id]
                                require(all(value["status"] == "completed" and value.get("usage_reported") for value in physical), "Worker fault caught an in-flight provider request")
                                require(not [value for value in latest.get("operations", []) if value["item_id"] == item_id], "Worker already started a guarded writing/Bash operation")
                                frozen, killed_pid = {"item": item, "physical": physical, "descriptor_sha256": hashlib.sha256(names.encode()).hexdigest()}, pid
                                os.killpg(pid, signal.SIGKILL)
                                self.intentional_stops.discard(pid)
                                break
                        finally:
                            if frozen is None:
                                try: os.killpg(pid, signal.SIGCONT)
                                except ProcessLookupError: pass
                                self.intentional_stops.discard(pid)
            time.sleep(0.002)
        require(frozen is not None and killed_pid is not None, "Actual active regular Read interruption window was not captured")
        self.gate.write_text("benign regular read padding\n")
        deadline = time.monotonic() + self.args.query_timeout
        while time.monotonic() < deadline:
            self.sample()
            run = self.read_run()
            replacement = next(value for value in run["items"] if value["id"] == item_id)
            if replacement["attempts"] >= 2: break
            time.sleep(0.05)
        require(replacement["attempts"] == 2, "Resident services did not reclaim killed worker exactly once")
        old = frozen["item"]
        for verb, extra in (("renew", ["--lease", "10s"]), ("complete", ["--output", "stale worker must not write"])):
            self.command(["coordinator", verb, self.run_id, item_id, "--cwd", str(self.workspace), "--worker", old["owner"], "--lease-token", old["lease"]["token"], *extra, "--json"],
                "soak-old-token-" + verb + "-rejected", expect_success=False, json_output=False)
        actual = {value["client_request_id"]: value for value in run.get("usage_requests", [])}
        require(all(actual.get(value["client_request_id"]) == value for value in frozen["physical"]), "Automatic recovery modified/lost settled pre-kill usage")
        self.record("resident pool automatically replaces actual SIGKILL worker after terminal provider receipt", item_id=item_id, killed_pid=killed_pid,
            old_owner=old["owner"], replacement_owner=replacement["owner"], attempts=replacement["attempts"],
            Read_descriptor_sha256=frozen["descriptor_sha256"], retained_physical_usage=frozen["physical"], fabricated_usage_receipts=False)

    def wait_until(self, target: float) -> None:
        while time.monotonic() < target:
            self.check_heartbeat()
            self.sample()
            time.sleep(min(1, max(0.01, target - time.monotonic())))
        self.check_heartbeat()

    def final_acceptance(self) -> None:
        self.await_idle()
        now = time.monotonic()
        require(len(self.cycles) >= self.args.cycles and now - self.activity_anchor >= self.args.duration, "Real-work duration/cycle count below acceptance threshold")
        require(self.service_a["process"].poll() is None and self.watcher[0].poll() is None, "Required resident PIDs were not continuously alive")
        require(now - self.service_a["ready_monotonic"] >= self.args.duration and now - self.watcher_ready >= self.args.duration, "Coordinator/watcher residency below two hours")
        require(self.cycles[-1]["started_monotonic"] - self.activity_anchor >= self.args.duration, "Final actual business request was submitted too early")
        run = self.read_run()
        require(run["status"] == "completed" and all(item["status"] == "completed" for item in run["items"]), "Final resident graph did not drain completely")
        require(all(value["status"] == "completed" and value.get("usage_reported") for value in run.get("usage_requests", [])), "Final physical request accounting is unresolved")
        generic = [item for item in run["items"] if item.get("event_id")]
        bound = [item for item in run["items"] if item.get("principal_binding")]
        require(len(generic) == self.args.cycles and len(bound) == 2 * self.args.cycles, "HTTP/generic and fixed-principal work accounting mismatched")
        business_ids = {cycle[key] for cycle in self.cycles for key in ("event_item_id", "producer_id", "receiver_id")} | {cycle["event_item_id"] + "-verify" for cycle in self.cycles}
        completed_owners = {item_id: worker for cycle in self.cycles for item_id, worker in cycle["completed_item_owners"].items()}
        for service in (self.service_a, self.services[1], self.service_b):
            require(any(worker.startswith(service["service_id"] + "-") for worker in completed_owners.values()),
                "A/B or restarted B never actually completed a paid business item")
        model_rows = [row for row in run["usage_requests"] if row["item_id"] in business_ids]
        earliest = min(datetime.fromisoformat(row["updated_at"].replace("Z", "+00:00")).timestamp() for row in model_rows)
        latest = max(datetime.fromisoformat(row["updated_at"].replace("Z", "+00:00")).timestamp() for row in model_rows)
        require(latest - earliest >= self.args.duration, "Actual completed physical model receipt UTC span is shorter than two hours")
        require(self.max_sample_gap <= 30 and all(b["started_monotonic"] - a["started_monotonic"] >= 300 for a, b in zip(self.cycles, self.cycles[1:])),
            "Heartbeat/cadence gap or dense catch-up invalidates long acceptance")
        self.check_heartbeat()
        known, unknown, estimate = 0, 0, 0.0
        for value in run.get("usage_requests", []):
            prices = value.get("prices", {})
            if prices.get("input_per_million_usd", 0) > 0 or prices.get("output_per_million_usd", 0) > 0:
                known += 1; estimate += value.get("estimated_usd", 0)
            else: unknown += 1
        if unknown == 0: require(estimate <= 20, "Catalog-estimated spend exceeded the declared bounded estimate")
        self.report["long_acceptance"] = {"passed": True, "actual_cycles": len(self.cycles),
            "primary_PID": self.service_a["process"].pid, "primary_resident_seconds": now - self.service_a["ready_monotonic"],
            "watcher_PID": self.watcher[0].pid, "watcher_resident_seconds": now - self.watcher_ready,
            "real_business_activity_monotonic_seconds": self.cycles[-1]["completed_monotonic"] - self.activity_anchor,
            "actual_physical_model_receipt_UTC_span_seconds": latest - earliest,
            "business_owner_service_ids": [service["service_id"] for service in (self.service_a, self.services[1], self.service_b)],
            "first_cycle_completed_utc": self.first_activity_utc, "last_cycle_completed_utc": self.cycles[-1]["completed_utc"],
            "HTTP_generic_implementation_items": len(generic), "bound_principal_items": len(bound),
            "physical_model_requests": len(run.get("usage_requests", [])), "known_pricing_requests": known, "unknown_pricing_requests": unknown,
            "known_catalog_estimated_usd": estimate, "usd20_estimate_constraint_confirmed": unknown == 0,
            "price_unknown_is_not_free": unknown > 0, "maximum_resource_sample_gap_seconds": self.max_sample_gap}
        for original in (self.producer, self.recipient):
            state = self.status(original["id"], "final-lifetime-budget-" + original["name"])
            require(state["session_id"] == original["session_id"] and state["config_sha256"] == original["config_sha256"], "Final principal identity/snapshot changed")
            rows = [value for value in run.get("usage_requests", []) if value["item_id"] in {item["id"] for item in bound if item["principal_binding"]["job_id"] == original["id"]}]
            total_input = sum(value["usage"]["input"] + value["usage"]["cache_read"] + value["usage"]["cache_write"] for value in rows)
            total_output = sum(value["usage"]["output"] for value in rows)
            require(state["input_tokens"] == total_input and state["output_tokens"] == total_output, "Principal totals do not match final absolute physical ledger including cached input")
            self.report.setdefault("final_principals", []).append({"id": original["id"], "session_id": original["session_id"], "input_including_cache": total_input, "output": total_output,
                "max_input": 8000000, "max_output": 200000, "config_sha256": original["config_sha256"]})
        self.sample(force=True)

    def cleanup(self) -> None:
        cleanup_errors = []
        self.heartbeat_stop.set()
        if hasattr(self, "heartbeat"):
            self.heartbeat.join(timeout=15)
            if self.heartbeat.is_alive(): cleanup_errors.append("Independent heartbeat thread did not terminate")
        if self.heartbeat_failure: cleanup_errors.append("Independent heartbeat failed: " + self.heartbeat_failure)
        atomic_json(self.output / "owned-process-tree-before-cleanup.json", list(self.owned_identities.values()))
        try: self.capture_owned()
        except BaseException as error: cleanup_errors.append(self.redact(str(error)))
        for process in self.processes:
            try: self.stop(process)
            except BaseException as error: cleanup_errors.append(self.redact(str(error)))
        try: table = self.capture_owned()
        except BaseException as error:
            cleanup_errors.append(self.redact(str(error))); table = {}
        groups = {row["pgid"] for pid, row in self.owned_identities.items() if pid in table and table[pid]["birth"] == row["birth"]}
        for pgid in groups:
            require(pgid != os.getpgrp(), "Cleanup would target the harness group")
            try: os.killpg(pgid, signal.SIGKILL)
            except ProcessLookupError: pass
        deadline = time.monotonic() + 10
        remaining = []
        while time.monotonic() < deadline:
            try: table = self.capture_owned()
            except BaseException as error:
                cleanup_errors.append(self.redact(str(error))); break
            remaining = [pid for pid, row in self.owned_identities.items() if pid in table and table[pid]["birth"] == row["birth"] and not table[pid]["stat"].startswith("Z")]
            if not remaining: break
            time.sleep(0.05)
        if remaining: cleanup_errors.append("Owned descendants survived cleanup: " + str(remaining))
        for connection in list(self.active_http):
            connection.close()
        self.active_http.clear()
        super().cleanup()
        for pid in self.known_children:
            row = subprocess.run(["ps", "-p", str(pid), "-o", "stat=,command="], capture_output=True, text=True, timeout=5).stdout.strip()
            require(not row or row.startswith("Z") or str(self.binary) not in row, "Owned coordinator worker survived final cleanup")
        self.token_path.unlink(missing_ok=True) if hasattr(self, "token_path") else None
        self.service_token = ""
        self.report.setdefault("cleanup", {})["private_service_token_destroyed"] = not getattr(self, "token_path", self.root / "missing").exists()
        self.report["cleanup"]["owned_descendant_groups_destroyed"] = not remaining
        self.report["cleanup"]["captured_owned_processes"] = len(self.owned_identities)
        atomic_json(self.output / "owned-process-tree-cleanup-proof.json", {"captured": list(self.owned_identities.values()), "remaining_live_owned_pids": remaining,
            "heartbeat_thread_stopped": not hasattr(self, "heartbeat") or not self.heartbeat.is_alive(), "errors": cleanup_errors})
        self.report["cleanup"]["independent_heartbeat_thread_stopped"] = not hasattr(self, "heartbeat") or not self.heartbeat.is_alive()
        self.report["cleanup"]["soak_cleanup_errors"] = cleanup_errors
        require(not cleanup_errors, "Soak descendant/thread cleanup incomplete: " + str(cleanup_errors))

    def copy_evidence(self) -> None:
        super().copy_evidence()
        if not hasattr(self, "transport_log"):
            return  # Failure before route isolation created no diagnostic log.
        require(self.transport_log.is_file() and not self.transport_log.is_symlink(), "Real transport debug log missing")
        sanitized, summary = sanitized_transport_log(self.transport_log.read_text(errors="replace"), self.redact)
        sanitized = self.redact(sanitized)
        summary = json.loads(self.redact(json.dumps(summary)))
        require(summary["retained_lines"] > 0, "No real transport observations were retained")
        directory = self.output / "evidence" / "transport"
        directory.mkdir(parents=True, exist_ok=True, mode=0o700)
        log_copy, summary_path = directory / "transport-debug.redacted.log", directory / "transport-debug-summary.json"
        log_copy.write_text(sanitized)
        log_copy.chmod(0o600)
        atomic_json(summary_path, summary)
        manifest_path = self.output / "evidence-manifest.json"
        manifest = json.loads(manifest_path.read_text())
        for path in (log_copy, summary_path):
            manifest.append({"path": str(path.relative_to(self.output)), "sha256": digest(path), "bytes": path.stat().st_size})
        atomic_json(manifest_path, manifest)
        self.report["transport_diagnostics"].update({"redacted_log": str(log_copy.relative_to(self.output)),
            "redacted_log_sha256": digest(log_copy), "summary": str(summary_path.relative_to(self.output)), "summary_sha256": digest(summary_path),
            "retained_lines": summary["retained_lines"], "rejected_unrecognized_lines": summary["rejected_unrecognized_lines"],
            "first_transport_error": summary["first_transport_error"], "included_in_evidence_manifest": True})

    def run(self) -> int:
        signal.setitimer(signal.ITIMER_REAL, self.args.case_timeout)
        try:
            self.prepare()
            self.business_cycle(0)
            offsets = cycle_offsets(self.args.duration, self.args.cycles)
            for number in range(1, self.args.cycles):
                self.wait_until(max(self.activity_anchor + offsets[number], self.cycles[-1]["started_monotonic"] + 300))
                self.business_cycle(number)
            self.final_acceptance()
            self.report["passed"] = True
        except BaseException as error:
            self.report.update({"failure_type": type(error).__name__, "failure": self.redact(str(error))})
        finally:
            signal.setitimer(signal.ITIMER_REAL, 0)
            for label, action in (("cleanup", self.cleanup), ("evidence_copy", self.copy_evidence), ("source_freeze", self.verify_freeze)):
                try: action()
                except BaseException as error:
                    self.report["passed"] = False
                    self.report[label + "_failure"] = self.redact(str(error))
            shutil.rmtree(self.root, ignore_errors=True)
            self.report.setdefault("cleanup", {})["temporary_workspace_destroyed"] = not self.root.exists()
            if self.root.exists(): self.report["passed"] = False
            self.report.update({"finished_utc": utc_now(), "elapsed_seconds": time.monotonic() - self.started})
            self.report = json.loads(self.redact(json.dumps(self.report)))
            self.secrets.clear()
            self.report["cleanup"]["redaction_secret_values_cleared_after_evidence"] = True
            atomic_json(self.output / "verification-report.json", self.report)
            self.progress("finished", passed=self.report["passed"], failure=self.report.get("failure"))
        print(json.dumps({"passed": self.report["passed"], "report": str(self.output / "verification-report.json"), "failure": self.report.get("failure")}), flush=True)
        return 0 if self.report["passed"] else 1


def self_test() -> dict[str, Any]:
    schedule = cycle_offsets(7200, 24)
    require(schedule[0] == 0 and schedule[-1] == 7200 and all(b - a >= 300 for a, b in zip(schedule, schedule[1:])), "Cadence compresses real business work into short startup")
    data = {"cycle": 7, "nonce": "offline", "orders": [{"group": "B", "amount": 4}, {"group": "A", "amount": 9}, {"group": "B", "amount": 3}]}
    expected = aggregate(data)
    require(expected["total_amount"] == 16 and expected["groups"] == {"A": {"count": 1, "amount": 9}, "B": {"count": 2, "amount": 7}}, "Independent host order oracle failed")
    require(heartbeat_gap_valid(5, 5.1) and not heartbeat_gap_valid(60, 60) and not heartbeat_gap_valid(5, 7205) and not heartbeat_gap_valid(5, -1), "Suspend/clock/heartbeat discontinuities were accepted")
    completed_fixture = {"items": [{"id": "x", "status": "completed"}], "events": [{"kind": "work_completed", "item_id": "x", "worker": "service-replacement-1"}],
        "usage_requests": [{"item_id": "x", "worker": "service-old-1", "status": "completed"}, {"item_id": "x", "worker": "service-replacement-1", "status": "completed"}]}
    require(completion_workers(completed_fixture, ["x"]) == {"x": "service-replacement-1"}, "Completed worker identity incorrectly relied on cleared item.Owner")
    profile_fixture = {"base_url": "https://example.invalid/v1", "transport": "anthropic_messages", "model": "fixture-model", "max_tokens": 2048}
    private_config = toml_table(["provider"], {"default": "fixture"}) + "\n" + toml_table(["provider", "custom", "fixture"], profile_fixture)
    private_config += "\n" + toml_table(["provider", "custom", "unselected"], {**profile_fixture, "model": "another-model", "timeout_seconds": 11})
    for original in (None, 0, 120, 400):
        current = private_config
        if original is not None: current = current.replace('"max_tokens" = 2048', '"timeout_seconds" = ' + str(original) + '\n"max_tokens" = 2048', 1)
        modified, previous = private_header_timeout(current, "fixture", 300)
        before, after = tomllib.loads(current), tomllib.loads(modified)
        expected_after = json.loads(json.dumps(before)); expected_after["provider"]["custom"]["fixture"]["timeout_seconds"] = 300
        require(previous == original and after == expected_after, "Private header timeout changed model/endpoint/transport/unselected provider")
    fixture_secret = "offline-auth-do-not-retain-87654321"
    debug_fixture = ("2026-10-13T10:00:00.000Z [http] anthropic GET /v1/models 200 elapsed=125ms reqid=0123456789abcdef\n"
        "2026-10-13T10:01:00.000Z [http] anthropic POST /v1/messages ERR:net/http: timeout awaiting response headers elapsed=2m0.054s reqid=abcdef0123456789\n"
        "2026-10-13T10:02:00.000Z [http] anthropic POST /v1/" + fixture_secret + " ERR:arbitrary server echo Authorization: Bearer " + fixture_secret + " elapsed=300s reqid=abcdef0123456700\n"
        "Authorization: Bearer " + fixture_secret + "\n{\"messages\":[\"PRIVATE BODY\"]}\n")
    safe_log, debug_summary = sanitized_transport_log(debug_fixture, lambda value: value.replace(fixture_secret, "[REDACTED]"))
    require(debug_summary["retained_lines"] == 3 and debug_summary["rejected_unrecognized_lines"] == 2, "Transport logging accepted arbitrary header/body lines")
    first = debug_summary["first_transport_error"]
    require(first["reqid"] == "abcdef0123456789" and first["error_phase"] == "response_headers" and first["http_status"] is None
        and abs(first["elapsed_seconds"] - 120.054) < .000001, "Transport diagnostic misidentified first error or fabricated an HTTP status")
    require(debug_summary["by_reqid"]["0123456789abcdef"][0]["http_status"] == 200 and debug_summary["observations"][2]["error_phase"] == "unclassified", "Transport headers or unknown-error classification changed")
    require(fixture_secret not in safe_log + json.dumps(debug_summary) and "Authorization" not in safe_log and "PRIVATE BODY" not in safe_log,
        "Transport diagnostics retained secrets or unsafe errors")
    require(all(value in WORKSPACE_RULES for value in ("not a Git repository", "do not inspect .git", "exactly one simple foreground command", "&&", "if/then/else", "heredocs", "shell redirection"))
        and all(value in BOOTSTRAP_INSTRUCTIONS for value in ("inspect only bootstrap-input.json and bootstrap-ready.json", "Read", "Write", "compare their JSON values")),
        "Fixture prompts lost their data-only bootstrap and simple Bash constraints")
    compile(CALCULATOR, "<readonly-calculator>", "exec")
    compile(CHECKER, "<immutable-checker>", "exec")
    with tempfile.TemporaryDirectory(prefix="metis-soak-offline-") as directory:
        root = Path(directory); workspace = root / "workspace"; workspace.mkdir()
        calculator, checker = root / "calc.py", root / "check.py"
        calculator.write_text(CALCULATOR); checker.write_text(CHECKER)
        input_path, output_path = workspace / "input.json", workspace / "output.json"
        atomic_json(input_path, data); atomic_json(output_path, expected)
        def helper(path: Path, *args: Any) -> subprocess.CompletedProcess:
            return subprocess.run([sys.executable, "-B", str(path), *map(str, args)], capture_output=True, text=True, timeout=5)
        require(json.loads(helper(calculator, "aggregate", input_path).stdout) == expected, "Actual stdlib calculator differs from independent host")
        require(helper(calculator, "verify", input_path, output_path).returncode == 0, "Calculator rejected correct report")
        atomic_json(output_path, {**expected, "total_amount": 0})
        require(helper(calculator, "verify", input_path, output_path).returncode == 7, "Calculator accepted incorrect arithmetic")
        bootstrap = {"fixture": "offline"}
        atomic_json(workspace / "bootstrap-ready.json", bootstrap)
        atomic_json(workspace / "arrival.json", {"cycle": 7, "nonce": "offline", "input_sha256": "fixture-hash"})
        graph, plan = root / "graph.json", root / "plan.json"
        seed = [{"id": "implementation", "status": "completed"}, {"id": "event-7", "status": "completed"}]
        atomic_json(graph, {"items": seed})
        atomic_json(plan, {"workspace": str(workspace), "run_path": str(graph), "bootstrap": bootstrap, "producer_job": "job-p", "recipient_job": "job-r",
            "cycles": [{"cycle": 7, "nonce": "offline", "input_sha256": "fixture-hash", "arrival": "arrival.json", "event_item_id": "event-7", "producer_id": "produce-7", "receiver_id": "receive-7"}]})
        blocked = subprocess.Popen([sys.executable, "-B", str(checker), str(plan)], stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
        try:
            time.sleep(0.15)
            require(blocked.poll() is None, "Instant generic completion bypassed pending bound admission")
            atomic_json(graph, {"items": [*seed, {"id": "produce-7", "status": "ready", "principal_binding": {"job_id": "job-p"}},
                {"id": "receive-7", "status": "pending", "principal_binding": {"job_id": "job-r"}}]})
            stdout, stderr = blocked.communicate(timeout=5)
            require(blocked.returncode == 0 and json.loads(stdout)["passed"] is True, "Generic checker falsely required unfinished bound artifacts: " + stderr)
        finally:
            if blocked.poll() is None: blocked.kill(); blocked.wait()
        atomic_json(workspace / "arrival.json", {})
        require(helper(checker, plan).returncode != 0, "Completed invalid generic artifact passed")
        # No CLI is executed. Exercise a Python parent whose Python descendant
        # owns a separate PGID and has no private-root text in its argv.
        inert, freeze = root / "inert-not-cli", root / "freeze.json"
        inert.write_text("OFFLINE INERT FILE\n")
        repo = Path(__file__).resolve().parents[2]
        atomic_json(freeze, {"files": {"scripts/e2e/persistent_agent_soak_live_check.py": digest(Path(__file__))}})
        args = argparse.Namespace(repo=repo, output=root / "evidence", binary=inert, expected_sha256=digest(inert), source_freeze=freeze,
            destroy_input=True, duration=7200, cycles=24, query_timeout=600, case_timeout=10800)
        harness = PersistentAgentSoak(args)
        try:
            harness.stage()
            (harness.home / "agents").mkdir(mode=0o700)
            harness.profile("offline-shape", ["Read", "Write", "Bash", "MessageTeammate"], "Perform the actual fixture task.")
            profile_text = (harness.home / "agents" / "offline-shape.md").read_text()
            require("tools: [Read, Write, Bash, MessageTeammate]" in profile_text and "permission_mode: dontAsk" in profile_text
                and "max_turns: 12" in profile_text and WORKSPACE_RULES in profile_text, "Prompt repair changed role tools/permissions or omitted simple helper guidance")
            harness.provider, harness.settings = "fixture", dict(profile_fixture)
            harness.report["original_route"] = {"provider": "fixture", "model": profile_fixture["model"], "endpoint": profile_fixture["base_url"], "private_parameter_overrides": {}}
            (harness.home / "config.toml").write_text(private_config)
            harness.env = {}
            harness.configure_transport_diagnostics()
            require(harness.env["METIS_DEBUG"] == "1" and Path(harness.env["METIS_DEBUG_LOG"]).is_absolute()
                and Path(harness.env["METIS_DEBUG_LOG"]).parent == harness.home, "Debug log escaped private workspace or did not enable logging")
            harness.secrets.append(fixture_secret)
            harness.transport_log.write_text(debug_fixture)
            code = "import pathlib,subprocess,sys,time; c=subprocess.Popen([sys.executable,'-c','import time; time.sleep(60)'],start_new_session=True); pathlib.Path(sys.argv[1]).write_text(str(c.pid)); time.sleep(60)"
            marker = harness.root / "offline-child-pid"
            parent = subprocess.Popen([sys.executable, "-c", code, str(marker)], start_new_session=True, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
            harness.processes.append(parent)
            deadline = time.monotonic() + 5
            while not marker.exists() and time.monotonic() < deadline: time.sleep(0.01)
            require(marker.exists(), "Offline descendant fixture did not start")
            child_pid = int(marker.read_text())
            harness.capture_owned()
            require(child_pid in harness.owned_identities, "Independent PGID descendant was not captured by parent tree")
            harness.cleanup()
            require(harness.report["cleanup"]["owned_descendant_groups_destroyed"] and not inert.exists(), "Offline descendant/inert executable cleanup failed")
            harness.copy_evidence()
            manifest = json.loads((harness.output / "evidence-manifest.json").read_text())
            transports = [value for value in manifest if value["path"].startswith("evidence/transport/")]
            require(len(transports) == 2 and all(digest(harness.output / value["path"]) == value["sha256"] for value in transports),
                "Transport redacted log/summary are missing from digest manifest")
            require(all(fixture_secret not in (harness.output / value["path"]).read_text() for value in transports), "Manifest-retained transport evidence contains a secret")
        finally:
            for row in harness.owned_identities.values():
                if row["pgid"] != os.getpgrp():
                    try: os.killpg(row["pgid"], signal.SIGKILL)
                    except ProcessLookupError: pass
            shutil.rmtree(harness.root, ignore_errors=True)
            require(not harness.transport_log.exists(), "Raw private diagnostic log survived temporary workspace destruction")
    return {"passed": True, "offline_fixtures_only": True, "real_CLI_invocations": 0, "real_provider_requests": 0, "real_credential_files_read": False,
        "checks": ["24 time-distributed cycles", "last real cycle after two-hour activity anchor", "independent integer aggregation", "readonly calculator/checker AST",
            "actual calculator rejects wrong arithmetic", "generic admission waits for real bound IDs", "generic check permits unfinished bound artifacts",
            "completed bad generic artifact rejects", "separate-PGID descendant and inert executable cleanup",
            "private timeout only changes selected provider timeout_seconds", "transport first-error reqid/header phase and elapsed evidence",
            "transport unknown errors do not fabricate HTTP status", "transport header/body/secret rejection", "transport manifest digests and raw log destruction",
            "bootstrap prompts scope private data-only fixture and simple Bash", "generated role preserves Read/Write/Bash/Message tools and dontAsk"],
        "heartbeat_gap_and_clock_negative_fixtures_passed": True, "completed_owner_omitted_fixture_passed": True,
        "cadence_seconds": schedule[1] - schedule[0]}


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--repo", type=Path, default=Path(__file__).resolve().parents[2])
    parser.add_argument("--binary", type=Path)
    parser.add_argument("--expected-sha256")
    parser.add_argument("--source-freeze", type=Path)
    parser.add_argument("--output", type=Path)
    parser.add_argument("--destroy-input", action="store_true")
    parser.add_argument("--duration", type=float, default=7200)
    parser.add_argument("--cycles", type=int, default=24)
    parser.add_argument("--query-timeout", type=float, default=600)
    parser.add_argument("--case-timeout", type=float, default=10800)
    parser.add_argument("--self-test", action="store_true")
    args = parser.parse_args()
    os.umask(0o077)
    if args.self_test:
        print(json.dumps(self_test(), indent=2)); return 0
    require(args.binary and args.expected_sha256 and args.source_freeze and args.output and args.destroy_input, "Live mode requires fresh binary/digest/freeze/output/--destroy-input")
    require(7200 <= args.duration <= 43200 and 24 <= args.cycles <= 96 and args.case_timeout >= args.duration + 900, "Long acceptance bounds invalid")
    cycle_offsets(args.duration, args.cycles)
    require(300 < args.query_timeout <= 600, "Harness query deadline must exceed the private 300s header timeout and remain bounded")
    def interrupted(signum: int, _frame: Any) -> None: raise KeyboardInterrupt("Soak interrupted by " + signal.Signals(signum).name)
    def timed_out(_signum: int, _frame: Any) -> None: raise TimeoutError("Bounded long soak exceeded its deadline")
    signal.signal(signal.SIGINT, interrupted); signal.signal(signal.SIGTERM, interrupted); signal.signal(signal.SIGALRM, timed_out)
    return PersistentAgentSoak(args).run()


if __name__ == "__main__":
    raise SystemExit(main())
