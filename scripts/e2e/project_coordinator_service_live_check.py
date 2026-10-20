#!/usr/bin/env python3
"""Exercise event HTTP ingestion and resident workers with a fresh real CLI.

Uses the currently selected provider/model and isolated private state. Requires
Python 3.11+ and Go. No mock queries, installed service or global configuration
changes are involved. Expected duration is roughly 8-15 minutes, depending on
the real provider. A failure destroys this test CLI before any source repair.

Tests authenticated HTTP events, automatic dispatch, parallel workers, graceful
service shutdown/restart, combined service+worker crash/restart, offline durable
queueing and failed host acceptance. SIGKILL recovery explicitly kills both the
service and its children; killing only the leader can leave children working.
"""

from __future__ import annotations

import argparse
from datetime import datetime
import hashlib
import http.client
import json
import os
from pathlib import Path
import secrets
import queue
import shlex
import signal
import socket
import subprocess
import sys
import threading
import time
from typing import Any
from urllib.error import HTTPError
from urllib.request import Request, urlopen
from urllib.parse import urlsplit

from project_coordinator_live_check import LiveCheck, atomic_json, require, utc_now


EVENT_BODY_LIMIT = 1 << 20


def oversized_event_response(url: str, headers: dict[str, str], body: bytes) -> tuple[int, bytes, dict[str, Any]]:
    """Read the actual rejection while uploading only enough to hit the limit.

    urllib writes the entire body before reading a response. A server that
    rejects during upload can therefore produce BrokenPipe before urllib sees
    its valid 413. Read concurrently here; a write error alone never passes.
    Content-Length remains the real oversized length, and limit+1 actual bytes
    suffice to trigger Go's MaxBytesReader without uploading the remaining body.
    """
    target = urlsplit(url)
    require(target.scheme == "http" and target.hostname and not target.username,
            "Oversized probe requires the resident service's HTTP endpoint")
    require(len(body) > EVENT_BODY_LIMIT, "Oversized probe body must exceed the actual service limit")
    connection = http.client.HTTPConnection(target.hostname, target.port, timeout=15)
    replies: queue.Queue = queue.Queue(maxsize=1)
    reader = None
    wire_socket = None
    upload_error_type = None
    prefix = body[:EVENT_BODY_LIMIT + 1]

    def read_response() -> None:
        try:
            with connection.getresponse() as response:
                replies.put((True, response.status, response.read(16384)))
        except BaseException as error:
            replies.put((False, type(error).__name__, b""))

    try:
        path = target.path or "/"
        if target.query:
            path += "?" + target.query
        connection.putrequest("POST", path, skip_accept_encoding=True)
        for name, value in headers.items():
            if name.lower() not in ("content-length", "connection"):
                connection.putheader(name, value)
        connection.putheader("Content-Length", str(len(body)))
        connection.putheader("Connection", "close")
        connection.endheaders()
        wire_socket = connection.sock
        require(wire_socket is not None, "Oversized probe did not establish a socket")
        reader = threading.Thread(target=read_response, daemon=True)
        reader.start()
        try:
            # Use the captured socket, not HTTPConnection.send(): getresponse()
            # may close the connection upon an early reply, and send() would
            # otherwise reconnect and upload body bytes as a new request.
            wire_socket.sendall(prefix)
        except OSError as error:
            upload_error_type = type(error).__name__
        try:
            received, code, text = replies.get(timeout=15)
        except queue.Empty:
            raise AssertionError("Oversized probe never read an actual HTTP response") from None
        require(received, "Oversized probe could not read an HTTP response: " + str(code))
        return code, text, {"client": "http.client concurrent response reader", "declared_body_bytes": len(body),
                            "upload_prefix_bytes_attempted": len(prefix), "full_body_uploaded": False,
                            "upload_error_type": upload_error_type, "actual_response_status": code}
    finally:
        if wire_socket is not None:
            try:
                wire_socket.shutdown(socket.SHUT_RDWR)
            except OSError:
                pass
        connection.close()
        if reader is not None:
            reader.join(timeout=3)
            require(not reader.is_alive(), "Oversized HTTP response reader did not stop")


class ServiceLiveCheck(LiveCheck):
    def __init__(self, args: argparse.Namespace) -> None:
        super().__init__(args)
        self.services: list[dict[str, Any]] = []
        self.child_pids: set[int] = set()
        self.token = ""
        self.token_path = self.root / "service-auth" / "bearer-token"
        self.run_id = ""
        self.report.update({
            "mode": "service-live", "http_requests": [],
            "boundary": "Temporary loopback resident service with actual model workers. No OS service installation. Crash test kills both leader and children; HTTP submission while stopped is not supported, local durable CLI queueing is tested.",
        })
        for key in ("requested_soak_seconds", "effective_soak_seconds", "long_task_acceptance"):
            self.report.pop(key, None)

    def service_arguments(self, listen: str = "127.0.0.1:0") -> list[str]:
        return ["coordinator", "serve", self.run_id, "--cwd", str(self.workspace),
                "--agent", "live-scoped", "--verify-agent", "live-verifier",
                "--token-file", str(self.token_path), "--listen", listen,
                "--workers", "2", "--poll", "100ms", "--lease", "2s"]

    def setup_service(self) -> None:
        agents = self.home / "agents"
        (agents / "live-verifier.md").write_text(
            "---\nname: live-verifier\ntools: [Read, Bash]\n---\n"
            "Independently inspect the current event's input and output using actual Read tools. "
            "Use Bash to run python3 host-check.py to independently check the produced JSON files. "
            "Work only in the stated workspace. Preserve all files; never write, edit, delegate, read credentials or run background work. "
            "Report concrete validation evidence for the current event.\n", encoding="utf-8")
        (agents / "live-verifier.md").chmod(0o600)
        self.token_path.parent.mkdir(mode=0o700)
        self.token = secrets.token_hex(32)
        self.secrets.append(self.token)
        self.token_path.write_text(self.token + "\n", encoding="utf-8")
        self.token_path.chmod(0o600)
        source, delivery = self.generate_input("base", 0)
        data = json.loads((self.workspace / source).read_text())
        groups: dict[str, dict[str, int]] = {}
        for row in data["records"]:
            group = groups.setdefault(row["group"], {"count": 0, "sum": 0})
            group["count"] += 1
            group["sum"] += row["value"]
        # This one seed delivery is host-created and explicitly excluded from
        # real-query assertions; every event output must be model-written.
        atomic_json(self.workspace / delivery, {
            "marker": data["marker"], "batch": data["batch"], "total_count": len(data["records"]),
            "total_sum": sum(row["value"] for row in data["records"]), "groups": groups,
        })
        run, _ = self.coord("create", "--goal", "Process each scoped event independently, validate its randomized JSON delivery and preserve previous deliveries.",
                            "--check", self.global_check(), label="create-service-project")
        self.run_id = run["id"]
        for _ in range(4):
            self.host_complete(self.run_id, evidence="Host prepared and independently checked the base seed file; no model query was made.")
        state, _ = self.coord("status", self.run_id, label="seed-completed-state")
        require(state["status"] == "completed" and all(item["attempts"] == 1 for item in state["items"]), "Host seed scaffold did not complete")
        self.report["run_id"] = self.run_id
        self.report["seed_delivery"] = {"path": delivery, "created_by": "host", "model_evidence": False}

    def global_check(self) -> str:
        return shlex.quote(sys.executable) + " host-check.py"

    def service_events(self, service: dict[str, Any]) -> list[dict[str, Any]]:
        path = self.process_logs[service["process"].pid][1]
        events = []
        if path.exists():
            for line in path.read_text(encoding="utf-8", errors="replace").splitlines():
                try:
                    event = json.loads(line)
                except ValueError:
                    continue
                if isinstance(event, dict) and event.get("kind"):
                    events.append(event)
                    if event.get("kind") == "worker_started" and isinstance(event.get("pid"), int):
                        self.child_pids.add(event["pid"])
        return events

    def active_child_pids(self, service: dict[str, Any]) -> set[int]:
        active: set[int] = set()
        for event in self.service_events(service):
            if event.get("kind") == "worker_started":
                active.add(event["pid"])
            elif event.get("kind") in ("worker_finished", "worker_idle"):
                active.discard(event.get("pid", -1))
        return active

    def start_service(self, label: str) -> dict[str, Any]:
        process = self.spawn(self.service_arguments(), label)
        service: dict[str, Any] = {"process": process, "label": label, "finished": False}
        self.services.append(service)
        started = time.monotonic()
        while time.monotonic() - started < 30:
            require(process.poll() is None, "Service ended before readiness: " + label)
            ready = next((event for event in self.service_events(service) if event.get("kind") == "service_ready"), None)
            if ready:
                require(ready.get("run_id") == self.run_id and ready.get("workers") == 2, "Wrong service readiness identity/capacity")
                service.update({"url": "http://" + ready["listen"], "service_id": ready["service_id"], "ready": ready})
                code, status = self.http(service, "GET", "/v1/health")
                require(code == 200 and status.get("run_id") == self.run_id, "Authenticated service health unavailable")
                self.progress("service-ready", service_id=ready["service_id"], pid=process.pid, address=ready["listen"])
                return service
            time.sleep(0.1)
        raise AssertionError("Service readiness not received within 30 seconds")

    def http(self, service: dict[str, Any], method: str, path: str, value: Any = None, *, authorization: str | None = "correct", raw: bytes | None = None) -> tuple[int, Any]:
        headers = {"Content-Type": "application/json"}
        if authorization is not None:
            headers["Authorization"] = "Bearer " + (self.token if authorization == "correct" else authorization)
        body = raw if raw is not None else (json.dumps(value).encode("utf-8") if value is not None else None)
        oversized_probe = None
        if method == "POST" and path == "/v1/events" and body is not None and len(body) > EVENT_BODY_LIMIT:
            code, text, oversized_probe = oversized_event_response(service["url"] + path, headers, body)
        else:
            request = Request(service["url"] + path, data=body, headers=headers, method=method)
            try:
                with urlopen(request, timeout=15) as response:
                    code, text = response.status, response.read(16384)
            except HTTPError as error:
                code, text = error.code, error.read(16384)
        parsed = json.loads(text) if text else None
        self.report["http_requests"].append({"utc": utc_now(), "method": method, "path": path, "status": code,
                                              "authorization": "correct" if authorization == "correct" else "absent" if authorization is None else "wrong",
                                              "service_id": service.get("service_id"), "response": parsed})
        if oversized_probe is not None:
            self.report["http_requests"][-1]["oversized_probe"] = oversized_probe
        return code, parsed

    def make_event(self, identifier: str, number: int) -> dict[str, Any]:
        source, delivery = self.generate_input(identifier, number)
        return {"payload": {"source": "service-live-check", "id": identifier, "subject": "Aggregate " + identifier,
                            "prompt": self.prompt(source, delivery), "deliverables": [delivery]},
                "source": source, "delivery": delivery, "http_role_binding": True}

    def post_event(self, service: dict[str, Any], event: dict[str, Any]) -> dict[str, Any]:
        code, response = self.http(service, "POST", "/v1/events", event["payload"])
        require(code == 202 and response.get("item_id"), "HTTP event not accepted")
        event["item_id"] = response["item_id"]
        return response

    def assert_model_evidence(self, snapshot: dict[str, int], *, creation_events: list[dict[str, Any]] | None = None) -> dict[str, Any]:
        evidence = self.checked_tool_evidence(self.trace_events(snapshot), self.state_on_disk(self.run_id), creation_events or [])
        require(evidence["model_responses"] > 0 and sum(evidence["tokens"].values()) > 0, "No real provider tokens/response evidence")
        require(all(evidence["tool_results"].get(tool, 0) > 0 for tool in ("Read", "Write", "Bash")), "Missing native Read / Write / Bash results")
        require(not evidence["unexpected_tool_errors"], "Unexpected model tool errors: " + json.dumps(evidence["unexpected_tool_errors"]))
        return evidence

    def validate_event(self, state: dict[str, Any], event: dict[str, Any], *, attempts: int = 1) -> None:
        items = {item["id"]: item for item in state["items"]}
        implementation, verification = items[event["item_id"]], items[event["item_id"] + "-verify"]
        require(implementation["status"] == verification["status"] == "completed", "Target event nodes are incomplete")
        require(implementation["attempts"] == attempts and verification["attempts"] == 1, "Unexpected target event attempts")
        require(implementation.get("agent_profile") == "live-scoped", "Implementation role was not persisted")
        if event.get("http_role_binding"):
            require(verification.get("agent_profile") == "live-verifier", "HTTP verification role was not persisted")
        acceptance = verification.get("acceptance", {})
        require(acceptance.get("status") == "passed" and acceptance.get("checks") and all(check["exit_code"] == 0 for check in acceptance["checks"]), "Host event acceptance failed")
        self.validate_delivery(event["source"], event["delivery"])
        artifact = next((item for item in implementation.get("artifacts", []) if item["path"] == event["delivery"]), None)
        require(artifact and artifact["sha256"] == hashlib.sha256((self.workspace / event["delivery"]).read_bytes()).hexdigest(), "Event delivery manifest/hash mismatch")

    def await_events(self, service: dict[str, Any], events: list[dict[str, Any]], snapshot: dict[str, int], *, attempts: int = 1, parallel: bool = False) -> tuple[dict[str, Any], dict[str, Any]]:
        started = time.monotonic()
        peak_running, peak_children = 0, 0
        expiries: dict[str, set[str]] = {}
        requeue_wait: dict[str, float] = {}
        last_progress = 0.0
        while time.monotonic() - started < self.args.stage_timeout:
            require(service["process"].poll() is None, "Resident service exited while processing events")
            self.service_events(service)
            state = self.state_on_disk(self.run_id)
            items = {item["id"]: item for item in state.get("items", [])}
            target_ids = {event["item_id"] for event in events}
            running = [item for item in items.values() if item.get("status") == "running" and item["id"] in target_ids]
            peak_running = max(peak_running, len(running))
            for item in items.values():
                if item.get("status") == "running" and item.get("lease"):
                    expiries.setdefault(item["id"], set()).add(item["lease"]["expires_at"])
            code, health = self.http(service, "GET", "/v1/status")
            require(code == 200, "Resident HTTP status stopped responding during model work")
            peak_children = max(peak_children, health.get("active_children", 0))
            monitored_ids = {identifier for event in events for identifier in (event["item_id"], event["item_id"] + "-verify")}
            self.guard_work_states(items, monitored_ids, "automatic service events", requeue_wait)
            if all(items.get(event["item_id"], {}).get("status") == "completed" and
                   items.get(event["item_id"] + "-verify", {}).get("status") == "completed" for event in events):
                for event in events:
                    self.validate_event(state, event, attempts=attempts)
                evidence = self.assert_model_evidence(snapshot, creation_events=events)
                if parallel:
                    require(peak_running >= 2 and peak_children >= 2, "Two simultaneous real implementation workers were not observed")
                return state, {"elapsed_seconds": round(time.monotonic() - started, 3), "peak_target_implementations_running": peak_running,
                               "peak_active_children": peak_children, "unique_lease_expiries": {key: len(value) for key, value in expiries.items()}, "evidence": evidence}
            if time.monotonic() - last_progress >= 5:
                self.progress("waiting-for-automatic-event-processing", service_id=service["service_id"], event_ids=sorted(target_ids),
                              peak_target_implementations_running=peak_running, peak_active_children=peak_children)
                last_progress = time.monotonic()
            time.sleep(0.2)
        raise AssertionError("Automatic service events did not finish before stage timeout")

    def await_idle(self, service: dict[str, Any]) -> None:
        started = time.monotonic()
        while time.monotonic() - started < 15:
            self.service_events(service)
            code, status = self.http(service, "GET", "/v1/status")
            if code == 200 and status.get("active_children") == 0:
                return
            time.sleep(0.1)
        raise AssertionError("Resident service did not return to an idle worker pool")

    @staticmethod
    def pid_alive(pid: int) -> bool:
        result = subprocess.run(["ps", "-p", str(pid), "-o", "stat=,command="], capture_output=True, text=True, timeout=5)
        text = result.stdout.strip()
        return result.returncode == 0 and bool(text) and not text.split()[0].startswith("Z")

    def stop_service(self, service: dict[str, Any], *, graceful: bool = True) -> dict[str, Any]:
        if service.get("finished"):
            return service["stop_proof"]
        process = service["process"]
        self.service_events(service)
        if process.poll() is None:
            process.send_signal(signal.SIGTERM if graceful else signal.SIGKILL)
        elapsed_before_stop = time.monotonic() - self.process_logs[process.pid][2]
        _, proof = self.finish(process, expect=None, timeout=elapsed_before_stop + 25)
        service["finished"], service["stop_proof"] = True, proof
        events = self.service_events(service)
        if graceful:
            require(proof["exit_code"] in (0, 130), "Graceful service stop returned an unexpected exit")
            require(any(event.get("kind") == "service_stopped" for event in events), "Service did not record graceful shutdown")
        return proof

    def idle_and_http_contract(self, service: dict[str, Any]) -> None:
        snapshot = self.trace_snapshot()
        started = time.monotonic()
        while time.monotonic() - started < 3:
            code, status = self.http(service, "GET", "/v1/status")
            require(code == 200 and status.get("active_children") == 0, "Idle service launched workers without ready work")
            require(not self.active_child_pids(service), "Idle completed project spawned a worker")
            time.sleep(0.1)
        require(self.trace_evidence(self.trace_events(snapshot))["model_responses"] == 0, "Idle service made a model request")
        for authorization in (None, "0" * 64):
            code, _ = self.http(service, "GET", "/v1/status", authorization=authorization)
            require(code == 401, "Unauthenticated/wrong bearer request was not rejected")
        body = {"source": "rejected", "id": "invalid", "subject": "Invalid payload", "prompt": "Do not execute this rejected request", "deliverables": []}
        for field, value in (("unknown", 1), ("agent_profile", "injected-role"), ("verify_agent", "injected-verifier")):
            code, _ = self.http(service, "POST", "/v1/events", {**body, field: value})
            require(code == 400, "Unknown field/role injection accepted: " + field)
        oversized = json.dumps({**body, "prompt": "x" * (2 * 1024 * 1024)}).encode()
        code, _ = self.http(service, "POST", "/v1/events", raw=oversized)
        require(code == 413, "Oversized event body was not rejected")
        state = self.state_on_disk(self.run_id)
        require(len(state["items"]) == 4, "Rejected HTTP payloads polluted the durable graph")
        self.record("idle service makes no model calls; HTTP authentication, strict fields and body limit enforced", idle_seconds=3,
                    provider_model_requests=0, authentication_status=401, unknown_field_status=400, body_limit_status=413)

    def wait_real_event_read(self, service: dict[str, Any], event: dict[str, Any], snapshot: dict[str, int]) -> dict[str, Any]:
        started = time.monotonic()
        while time.monotonic() - started < 180:
            require(service["process"].poll() is None, "Service ended before fault injection")
            self.service_events(service)
            state = self.state_on_disk(self.run_id)
            item = next((item for item in state.get("items", []) if item["id"] == event["item_id"]), {})
            evidence = self.trace_evidence(self.trace_events(snapshot))
            if item.get("status") == "running" and item.get("lease") and evidence["model_responses"] > 0 and evidence["tool_results"].get("Read", 0) > 0:
                return item
            time.sleep(0.1)
        raise AssertionError("No actual token and Read evidence before service fault injection")

    def restart_test(self, service: dict[str, Any], *, crash: bool, number: int) -> dict[str, Any]:
        self.await_idle(service)
        label = "service-crash" if crash else "service-sigterm"
        event = self.make_event(label, number)
        snapshot = self.trace_snapshot()
        self.post_event(service, event)
        old = self.wait_real_event_read(service, event, snapshot)
        before_spend = self.state_on_disk(self.run_id).get("spent_usd", 0)
        interrupted = self.trace_evidence(self.trace_events(snapshot))
        stopped_at = time.monotonic()
        killed_children: list[int] = []
        if crash:
            killed_children = sorted(self.active_child_pids(service))
            require(killed_children, "Crash test found no owned child process to kill")
            for pid in killed_children:
                try:
                    os.killpg(pid, signal.SIGKILL)
                except ProcessLookupError:
                    pass
            stop_proof = self.stop_service(service, graceful=False)
        else:
            stop_proof = self.stop_service(service)
        expiry = datetime.fromisoformat(old["lease"]["expires_at"].replace("Z", "+00:00")).timestamp()
        while time.time() <= expiry + 0.2:
            time.sleep(0.05)
        state, _ = self.coord("status", self.run_id, label=label + "-persisted-state")
        pending = next(item for item in state["items"] if item["id"] == event["item_id"])
        require(pending["status"] in ("ready", "pending"), "Service stop left an unrecoverable node: " + pending["status"])
        require(state.get("spent_usd", 0) >= before_spend, "Service stop lost already persisted usage")
        new_snapshot = self.trace_snapshot()
        restarted = self.start_service(label + "-restart")
        replacement = self.wait_real_event_read(restarted, event, new_snapshot)
        require(replacement["attempts"] == 2, "Service restart did not reclaim interrupted work exactly once")
        self.assert_stale_fencing(self.run_id, old, replacement, live=True)
        state, proof = self.await_events(restarted, [event], new_snapshot, attempts=2)
        require(state.get("spent_usd", 0) >= before_spend, "Restart reset cumulative estimated usage")
        self.record(label + " automatically resumes interrupted real work and rejects old claim writes", event_id=event["item_id"],
                    interrupted_evidence=interrupted, resumed=proof, old_service_id=service["service_id"], new_service_id=restarted["service_id"],
                    interruption_to_completion_seconds=round(time.monotonic() - stopped_at, 3), stop=stop_proof,
                    killed_child_pids=killed_children, estimated_spend_before_stop=before_spend, estimated_spend_after=state.get("spent_usd", 0),
                    crash_scope="leader and owned workers" if crash else "service SIGTERM drains worker cancellation")
        return restarted

    def offline_queue(self, service: dict[str, Any]) -> dict[str, Any]:
        self.await_idle(service)
        self.stop_service(service)
        event = self.make_event("offline-durable", 6)
        event["http_role_binding"] = False
        payload = event["payload"]
        item, _ = self.coord("event", self.run_id, "--source", payload["source"], "--id", payload["id"],
                             "--agent", "live-scoped", "--subject", payload["subject"], "--prompt", payload["prompt"],
                             "--deliverable", event["delivery"], label="offline-local-adapter-enqueue")
        event["item_id"] = item["id"]
        snapshot = self.trace_snapshot()
        restarted = self.start_service("offline-queue-restart")
        _, proof = self.await_events(restarted, [event], snapshot)
        self.record("durable event queued while stopped is consumed automatically after restart", event_id=item["id"], resumed=proof,
                    ingestion="local CLI adapter while HTTP service is down", verifier_profile="CLI event default; HTTP role binding checked separately")
        return restarted

    def failed_acceptance(self, service: dict[str, Any]) -> dict[str, Any]:
        self.await_idle(service)
        self.stop_service(service)
        event = self.make_event("failed-host-check", 7)
        event["http_role_binding"] = False
        payload = event["payload"]
        item, _ = self.coord("event", self.run_id, "--source", payload["source"], "--id", payload["id"], "--agent", "live-scoped",
                             "--subject", payload["subject"], "--prompt", payload["prompt"], "--deliverable", event["delivery"], label="enqueue-before-failed-check")
        event["item_id"] = item["id"]
        self.coord("configure", self.run_id, "--check", "exit 7", label="configure-failed-host-check")
        snapshot = self.trace_snapshot()
        service = self.start_service("failed-acceptance-service")
        started = time.monotonic()
        failed = None
        while time.monotonic() - started < self.args.stage_timeout:
            require(service["process"].poll() is None, "Service exited after a child acceptance failure")
            state = self.state_on_disk(self.run_id)
            failed = next((item for item in state.get("items", []) if item["id"] == event["item_id"] + "-verify" and item["status"] == "failed"), None)
            logs = self.service_events(service)
            if failed and any(log.get("kind") == "worker_finished" and log.get("exit_code") == 11 for log in logs):
                break
            time.sleep(0.2)
        require(failed is not None and state["status"] == "blocked", "Failed host acceptance did not block the graph")
        acceptance = failed.get("acceptance", {})
        require(acceptance.get("status") == "failed" and acceptance.get("checks") and acceptance["checks"][0]["exit_code"] == 7, "Host exit 7 evidence not recorded")
        require(any(log.get("kind") == "worker_finished" and log.get("exit_code") == 11 for log in self.service_events(service)), "Child did not preserve INCOMPLETE exit 11")
        code, status = self.http(service, "GET", "/v1/health")
        require(code == 200 and status["status"] == "blocked", "Service HTTP endpoint became unavailable after failed work")
        evidence = self.assert_model_evidence(snapshot, creation_events=[event])
        self.record("failed actual acceptance records host exit 7 and child exit 11 while service stays available", event_id=event["item_id"], evidence=evidence, health_status=code)
        following = self.make_event("after-failed-check", 8)
        snapshot = self.trace_snapshot()
        self.post_event(service, following)
        self.coord("configure", self.run_id, "--check", self.global_check(), label="restore-check-for-independent-event")
        state, proof = self.await_events(service, [following], snapshot)
        old_failed = next(item for item in state["items"] if item["id"] == failed["id"])
        require(old_failed["status"] == "failed", "New independent event silently erased prior failed acceptance")
        self.record("resident service accepts and completes new independent event after earlier failure", event_id=following["item_id"], proof=proof,
                    prior_failed_verification_preserved=True, aggregate_run_status=state["status"])
        return service

    def cleanup(self) -> None:
        errors = []
        remaining: list[int] = []
        try:
            for service in self.services:
                try:
                    if service["process"].poll() is None:
                        self.stop_service(service)
                    self.service_events(service)
                except BaseException as error:
                    errors.append(self.redact(str(error)))
            # Service children run in separate process groups. The generic CLI
            # harness cannot reap them; use the service's explicit PID audit log.
            for pid in self.child_pids:
                if self.pid_alive(pid):
                    result = subprocess.run(["ps", "-p", str(pid), "-o", "command="], capture_output=True, text=True, timeout=5)
                    if str(self.binary) in result.stdout:
                        try:
                            os.killpg(pid, signal.SIGKILL)
                        except ProcessLookupError:
                            pass
            deadline = time.monotonic() + 10
            while any(self.pid_alive(pid) for pid in self.child_pids) and time.monotonic() < deadline:
                time.sleep(0.1)
        except BaseException as error:
            errors.append(self.redact(str(error)))
        finally:
            # Even failed process inspection cannot preserve credentials or a
            # runnable binary after a failed acceptance run.
            try:
                self.token_path.unlink(missing_ok=True)
            finally:
                super().cleanup()
        for pid in sorted(self.child_pids):
            try:
                if self.pid_alive(pid):
                    remaining.append(pid)
            except BaseException as error:
                remaining.append(pid)
                errors.append(self.redact(str(error)))
        self.report["cleanup"].update({"service_token_removed": not self.token_path.exists(), "service_child_pids": sorted(self.child_pids),
                                        "remaining_service_child_pids": remaining, "all_service_children_stopped": not remaining})
        if errors:
            self.report["cleanup"]["graceful_stop_errors"] = errors
        require(not remaining and not self.token_path.exists(), "Service child/token cleanup incomplete")

    def run(self) -> int:
        try:
            self.setup_provider()
            self.build()
            self.write_checker()
            self.setup_service()
            _, proof = self.command(self.service_arguments("0.0.0.0:0"), "reject-public-listen", expect=None)
            require(proof["exit_code"] != 0, "Service accepted a public listen address")
            self.record("public listen address rejected", exit_code=proof["exit_code"])
            service = self.start_service("first-resident-service")
            self.idle_and_http_contract(service)
            first = self.make_event("http-first", 1)
            snapshot = self.trace_snapshot()
            first_response = self.post_event(service, first)
            replay = self.post_event(service, first)
            require(first_response["item_id"] == replay["item_id"], "HTTP replay created new work")
            code, _ = self.http(service, "POST", "/v1/events", {**first["payload"], "prompt": first["payload"]["prompt"] + " conflicting instruction"})
            require(code == 409, "Conflicting event ID payload was not rejected")
            state, proof = self.await_events(service, [first], snapshot)
            require(len(state["items"]) == 6, "HTTP dedup did not preserve exactly two event nodes")
            self.record("HTTP event automatically dispatched to real model and host verification; replay deduplicated", event_id=first["item_id"], proof=proof)
            self.await_idle(service)
            self.stop_service(service)
            service = self.start_service("dedup-restart-service")
            self.post_event(service, first)
            require(len(self.state_on_disk(self.run_id)["items"]) == 6, "Restart lost event deduplication")
            self.record("event replay after resident-service restart preserves two nodes", event_id=first["item_id"])
            parallel = [self.make_event("parallel-a", 2), self.make_event("parallel-b", 3)]
            snapshot = self.trace_snapshot()
            for event in parallel:
                self.post_event(service, event)
            _, proof = self.await_events(service, parallel, snapshot, parallel=True)
            self.record("two resident workers process separate real events concurrently without duplicate attempts", event_ids=[event["item_id"] for event in parallel], proof=proof)
            service = self.restart_test(service, crash=False, number=4)
            service = self.restart_test(service, crash=True, number=5)
            service = self.offline_queue(service)
            service = self.failed_acceptance(service)
            self.await_idle(service)
            self.stop_service(service)
            self.report["passed"] = True
        except BaseException as error:
            self.report["failure"] = self.redact(str(error))
            self.report["failure_type"] = type(error).__name__
        finally:
            try:
                self.cleanup()
            except BaseException as error:
                self.report["passed"] = False
                self.report["cleanup_failure"] = self.redact(str(error))
            try:
                self.copy_evidence()
            except BaseException as error:
                self.report["passed"] = False
                self.report["evidence_copy_failure"] = self.redact(str(error))
            self.report["elapsed_seconds"] = round(time.monotonic() - self.started, 3)
            self.report["finished_utc"] = utc_now()
            atomic_json(self.output / "verification-report.json", json.loads(self.redact(json.dumps(self.report, ensure_ascii=False))))
            self.progress("finished", passed=self.report["passed"], failure=self.report.get("failure"), cleanup=self.report.get("cleanup"))
        print(self.redact(json.dumps({"passed": self.report["passed"], "checks": len(self.report["checks"]), "failure": self.report.get("failure"),
                                      "report": str(self.output / "verification-report.json"), "cleanup": self.report.get("cleanup")}, ensure_ascii=False)), flush=True)
        return 0 if self.report["passed"] else 1


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("--repo", type=Path, default=Path(__file__).resolve().parents[2])
    parser.add_argument("--output", type=Path, required=True, help="new or empty private evidence directory")
    parser.add_argument("--stage-timeout", type=float, default=600, help="maximum seconds for each real event stage")
    parser.set_defaults(quick=True, soak_seconds=0)
    args = parser.parse_args()
    require(args.stage_timeout > 0, "--stage-timeout must be positive")
    os.umask(0o077)

    def interrupted(signum: int, _frame: Any) -> None:
        raise KeyboardInterrupt("Service harness interrupted by " + signal.Signals(signum).name)

    signal.signal(signal.SIGINT, interrupted)
    signal.signal(signal.SIGTERM, interrupted)
    return ServiceLiveCheck(args).run()


if __name__ == "__main__":
    raise SystemExit(main())
