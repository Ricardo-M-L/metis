#!/usr/bin/env python3
"""Fresh-CLI Desktop HTTP acceptance, called at the end of the live checker.

The caller owns the disposable executable, isolated HOME/METIS_HOME/workspace,
process registry, credentials, evidence redaction and final destruction. This
helper never builds a binary, starts a model query or selects another route.
--self-test exercises only local Python oracles and a loopback HTTP fixture.
"""
from __future__ import annotations

import argparse
import hashlib
import http.client
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import json
from pathlib import Path
import socket
import threading
import time
from typing import Any
import uuid

from persistent_agent_live_check import digest, json_sha, require


RESPONSE_LIMIT = 4 << 20
SNAPSHOT_FIELDS = {"schema_version", "generated_at", "workspace", "jobs", "handoffs", "runs", "learning", "warnings"}
SAFE_TOKEN_FIELDS = {"input_tokens", "output_tokens", "max_input_tokens", "max_output_tokens", "cache_read_tokens", "cache_write_tokens"}


def _safe_projection(value: Any) -> None:
    """Reject private execution data instead of saving a raw HTTP response."""
    if isinstance(value, dict):
        for key, item in value.items():
            lower = key.lower()
            forbidden = (any(part in lower for part in ("secret", "credential", "password", "prompt")) or
                ("token" in lower and lower not in SAFE_TOKEN_FIELDS) or lower in {
                    "result_json", "output", "command", "check_commands", "payload", "raw", "body", "definition",
                    "task", "lease", "binding", "authorization", "api_key", "headers", "env"})
            require(not forbidden, "Desktop HTTP projection contains private execution fields")
            _safe_projection(item)
    elif isinstance(value, list):
        for item in value:
            _safe_projection(item)


def _snapshot(value: Any, workspace: str) -> dict[str, Any]:
    require(isinstance(value, dict) and set(value) == SNAPSHOT_FIELDS and
        type(value.get("schema_version")) is int and value["schema_version"] == 1 and
        value.get("workspace") == workspace and value.get("generated_at"),
        "Desktop HTTP snapshot lost its trusted workspace/schema binding")
    require(all(isinstance(value[field], list) for field in ("jobs", "handoffs", "runs", "learning", "warnings")),
        "Desktop HTTP snapshot collections are invalid")
    _safe_projection(value)
    return {key: item for key, item in value.items() if key != "generated_at"}


def _job(snapshot: dict[str, Any], identifier: str) -> dict[str, Any]:
    rows = [row for row in snapshot["jobs"] if row.get("id") == identifier]
    require(len(rows) == 1, "Desktop HTTP snapshot omitted/duplicated the acceptance job")
    return rows[0]


def _learning(snapshot: dict[str, Any], name: str) -> dict[str, Any]:
    rows = [row for row in snapshot["learning"] if row.get("kind") == "skill" and row.get("name") == name]
    require(len(rows) == 1 and len(rows[0].get("active_version", "")) == 64 and
        type(rows[0].get("activation_epoch")) is int and rows[0]["activation_epoch"] > 0,
        "Desktop HTTP acceptance requires an existing managed skill activation")
    return rows[0]


def _rejected_unchanged(status: int, before: Any, after: Any) -> None:
    require(400 <= status < 600, "Desktop HTTP accepted a stale or cross-origin action")
    require(before == after, "Rejected Desktop HTTP action changed durable state")


def _request(checker: Any, port: int, method: str, path: str, value: Any = None,
             headers: dict[str, str] | None = None) -> tuple[int, dict[str, str], bytes]:
    require(type(port) is int and 0 < port < 65536 and path.startswith("/"), "Invalid owned loopback request")
    connection = http.client.HTTPConnection("127.0.0.1", port, timeout=25)
    checker.active_http.add(connection)
    try:
        body = None if value is None else json.dumps(value, ensure_ascii=False).encode()
        request_headers = {"Connection": "close", "Content-Type": "application/json", **(headers or {})}
        connection.request(method, path, body, request_headers)
        response = connection.getresponse()
        raw = response.read(RESPONSE_LIMIT + 1)
        require(len(raw) <= RESPONSE_LIMIT, "Desktop HTTP response exceeded its bounded capacity")
        return response.status, {key.lower(): val for key, val in response.getheaders()}, raw
    finally:
        connection.close()
        checker.active_http.discard(connection)


def _json_request(checker: Any, port: int, method: str, path: str, value: Any = None,
                  headers: dict[str, str] | None = None) -> tuple[int, dict[str, str], Any]:
    status, response_headers, raw = _request(checker, port, method, path, value, headers)
    try:
        decoded = json.loads(raw)
    except (ValueError, UnicodeError):
        raise AssertionError("Desktop HTTP returned invalid JSON") from None
    return status, response_headers, decoded


def check_desktop_operations(checker: Any) -> dict[str, Any]:
    """Run after quality/business/billing checks; existing job and skill required.

    Stops idle resident services before reversible job actions. All processes
    are spawned through the caller, and every temporary path is below its root.
    Learning actions intentionally use stale CAS values and never roll back a
    current activation. The caller must still run cleanup on every outcome.
    """
    require(checker.binary.is_file() and digest(checker.binary) == checker.input_sha,
        "Desktop HTTP acceptance requires the caller's unchanged fresh CLI")
    workspace = str(checker.workspace.resolve())
    require(checker.workspace.resolve().is_relative_to(checker.root.resolve()) and
        checker.home.resolve().is_relative_to(checker.root.resolve()), "Desktop HTTP acceptance must use isolated state")
    if any(service["process"].poll() is None for service in checker.services):
        checker.stop_services()
    before_trace = checker.trace_snapshot()
    identifier, name = checker.dispatch_job["id"], checker.skill_name

    def cli_snapshot(label: str) -> dict[str, Any]:
        value, _ = checker.command(["operations", "snapshot", "--cwd", workspace, "--json"], label)
        return _snapshot(value, workspace)

    initial = cli_snapshot("desktop-http-initial-persistent-snapshot")
    initial_job = _job(initial, identifier)["state"]
    require(initial_job in ("active", "paused"), "Desktop HTTP acceptance job is not reversible")
    initial_learning = _learning(initial, name)
    state_path = checker.home / "skills" / ".learning" / name / "state.json"
    require(state_path.is_file() and not state_path.is_symlink(), "Managed skill state is unavailable")
    state_sha = digest(state_path)
    temporary = checker.root / ("desktop-http-tmp-" + uuid.uuid4().hex[:12])
    temporary.mkdir(mode=0o700)
    with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as reservation:
        reservation.bind(("127.0.0.1", 0))
        port = reservation.getsockname()[1]
    saved_environment = checker.env
    checker.env = {**saved_environment, "TMPDIR": str(temporary)}
    try:
        process, stdout, stderr = checker.spawn(["desktop", "--web", "--port", str(port)], "fresh-cli-desktop-http")
    finally:
        checker.env = saved_environment
    statuses: dict[str, int] = {}
    job_changed = False
    result: dict[str, Any] = {}

    def http_snapshot() -> dict[str, Any]:
        status, headers, value = _json_request(checker, port, "GET", "/api/operations")
        require(status == 200 and headers.get("cache-control") == "no-store", "Desktop HTTP snapshot failed or became cacheable")
        statuses["snapshot"] = status
        return _snapshot(value, workspace)

    def post(action: dict[str, Any], label: str, *, headers: dict[str, str] | None = None) -> tuple[int, Any]:
        status, _, value = _json_request(checker, port, "POST", "/api/operations/action",
            {"expected_workspace": workspace, **action}, headers)
        statuses[label] = status
        return status, value

    try:
        deadline = time.monotonic() + 60
        while True:
            require(process.poll() is None, "Fresh CLI Desktop backend exited before HTTP readiness")
            try:
                current = http_snapshot()
                break
            except (OSError, http.client.HTTPException):
                require(time.monotonic() < deadline, "Fresh CLI Desktop HTTP readiness timed out")
                time.sleep(.1)
        require(current == initial, "Desktop HTTP snapshot differs from the fresh CLI persistent snapshot")
        require(("Open http://127.0.0.1:" + str(port)) in stderr.read_text(errors="replace"),
            "Desktop startup did not identify its owned loopback address")
        status, _, root = _request(checker, port, "GET", "/")
        statuses["root"] = status
        require(status == 200 and b'durable_ops.js' in root and b'operationsNav' in root,
            "Fresh CLI does not serve the Desktop operations entry")

        for action, state in (("pause_job", "paused"), ("resume_job", "active")):
            status, acknowledgement = post({"action": action, "id": identifier}, action)
            # Even a malformed acknowledgement may have committed the action.
            job_changed = True
            require(status == 200 and acknowledgement == {"ok": True}, "Desktop HTTP did not acknowledge its host job action")
            persisted = cli_snapshot("desktop-http-cli-reloads-" + state)
            require(_job(persisted, identifier)["state"] == state and http_snapshot() == persisted,
                "Desktop HTTP job action did not persist through a separate fresh CLI invocation")

        stable = cli_snapshot("desktop-http-before-rejected-actions")
        status, _ = post({"action": "pause_job", "id": identifier,
            "expected_workspace": str(checker.root / "previous-selected-workspace")}, "stale_workspace")
        require(status == 409, "Old Desktop workspace selection was not rejected with 409")
        _rejected_unchanged(status, stable, cli_snapshot("desktop-http-stale-workspace-unchanged"))
        status, _ = post({"action": "pause_job", "id": identifier}, "cross_origin",
            headers={"Origin": "https://unrelated.invalid", "Sec-Fetch-Site": "cross-site"})
        require(status == 403, "Cross-origin Desktop write was not rejected with 403")
        _rejected_unchanged(status, stable, cli_snapshot("desktop-http-cross-origin-unchanged"))

        version = initial_learning["active_version"]
        wrong_version = ("0" if version != "0" * 64 else "1") * 64
        for action, expected_version, epoch, label in (
            ("rollback_learning", wrong_version, initial_learning["activation_epoch"], "stale_learning_version"),
            ("resume_learning", version, initial_learning["activation_epoch"] + 1, "stale_learning_epoch")):
            status, _ = post({"action": action, "kind": "skill", "name": name,
                "expected_version": expected_version, "expected_epoch": epoch,
                "note": "Host acceptance checks stale Desktop review rejection."}, label)
            _rejected_unchanged(status, stable, cli_snapshot("desktop-http-" + label + "-unchanged"))
            require(digest(state_path) == state_sha, "Rejected learning action modified its persistent state")

        require(not list(temporary.glob(".metis-desktop-operations-*.json")), "Desktop bridge left private action request files")
        require(not any(row.get("kind") == "tokens" for row in checker.trace_events(before_trace)),
            "Desktop management unexpectedly generated model usage")
        result = {"fresh_cli_sha256": checker.input_sha, "workspace": workspace, "http_statuses": statuses,
            "job_id": identifier, "job_persistence_verified": ["paused", "active"],
            "learning_name": name, "active_version_unchanged": version,
            "activation_epoch_unchanged": initial_learning["activation_epoch"], "learning_state_sha256": state_sha,
            "snapshot_sha256": json_sha(stable), "root_html_sha256": hashlib.sha256(root).hexdigest(),
            "private_action_files_removed": True, "model_query_endpoints_called": False,
            "observed_model_token_events": 0, "browser_rendering_verified": False}
    finally:
        try:
            if job_changed and initial_job == "paused" and process.poll() is None:
                status, acknowledgement = post({"action": "pause_job", "id": identifier}, "restore_initial_job")
                require(status == 200 and acknowledgement == {"ok": True} and
                    _job(cli_snapshot("desktop-http-restored-initial-job"), identifier)["state"] == initial_job,
                    "Desktop HTTP did not restore the caller's initial paused job")
        finally:
            checker.stop(process)
            for handle in checker.handles.pop(process.pid, ()):
                handle.close()
            for path in (stdout, stderr):
                path.write_text(checker.redact(path.read_text(errors="replace")))
            for row in checker.report["commands"]:
                if row.get("pid") == process.pid:
                    row.update({"exit_code": process.returncode, "stdout_sha256": digest(stdout), "stderr_sha256": digest(stderr)})
            require(process.poll() is not None and not list(temporary.glob(".metis-desktop-operations-*.json")),
                "Desktop HTTP process/private action cleanup did not complete")
    result["desktop_process_stopped"] = True
    checker.record("fresh CLI Desktop HTTP actions persist and stale reviews cannot write", **result)
    return result


def self_test() -> dict[str, Any]:
    """Pure local oracle checks; this never claims a real CLI/provider passed."""
    checks = []

    def rejects(action, label: str) -> None:
        try:
            action()
        except AssertionError:
            checks.append(label)
            return
        raise AssertionError("Desktop offline oracle accepted " + label)

    fixture = {"schema_version": 1, "generated_at": "2026-10-16T00:00:00Z", "workspace": "/isolated",
        "jobs": [{"id": "job-1", "state": "active", "usage": {"input_tokens": 7}}], "handoffs": [], "runs": [],
        "learning": [{"kind": "skill", "name": "fixture", "active_version": "a" * 64, "activation_epoch": 2}], "warnings": []}
    stable = _snapshot(fixture, "/isolated")
    require(_job(stable, "job-1")["state"] == "active" and _learning(stable, "fixture")["activation_epoch"] == 2,
        "Desktop offline oracle rejected its safe fixture")
    require(stable == _snapshot({**fixture, "generated_at": "later"}, "/isolated"), "Snapshot comparison retained volatile generation time")
    rejects(lambda: _snapshot(fixture, "/other"), "wrong workspace")
    rejects(lambda: _snapshot({**fixture, "schema_version": True}, "/isolated"), "boolean schema")
    rejects(lambda: _safe_projection({"jobs": [{"credentials": "private"}]}), "nested credentials")
    rejects(lambda: _safe_projection({"runs": [{"claim_token": "private"}]}), "worker claim token")
    rejects(lambda: _safe_projection({"learning": [{"system_prompt": "private"}]}), "execution prompt")
    rejects(lambda: _rejected_unchanged(200, stable, stable), "silently accepted stale action")
    rejects(lambda: _rejected_unchanged(409, "before", "after"), "rejection after durable mutation")
    _rejected_unchanged(502, stable, stable)

    class FixtureHandler(BaseHTTPRequestHandler):
        def do_POST(self) -> None:
            body = json.loads(self.rfile.read(int(self.headers["Content-Length"])))
            require(body == {"note": "中文"} and not self.headers.get("Authorization"), "HTTP helper changed its body or attached credentials")
            payload = b'{"ok":true}'
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.send_header("Cache-Control", "no-store")
            self.send_header("Content-Length", str(len(payload)))
            self.end_headers()
            self.wfile.write(payload)

        def log_message(self, *_args: Any) -> None:
            pass

    class FixtureChecker:
        active_http: set[Any] = set()

    with ThreadingHTTPServer(("127.0.0.1", 0), FixtureHandler) as server:
        thread = threading.Thread(target=server.serve_forever, daemon=True)
        thread.start()
        try:
            status, headers, value = _json_request(FixtureChecker, server.server_port, "POST", "/fixture", {"note": "中文"})
            require(status == 200 and headers["cache-control"] == "no-store" and value == {"ok": True} and
                not FixtureChecker.active_http, "Loopback HTTP response/connection registration is incorrect")
        finally:
            server.shutdown()
            thread.join(timeout=5)
        require(not thread.is_alive(), "Offline HTTP fixture thread survived cleanup")
    checks.extend(["safe token metadata preserved", "volatile snapshot timestamp excluded", "loopback JSON transport and connection cleanup"])
    return {"passed": True, "offline_only": True, "fresh_cli_verified": False, "real_provider_verified": False, "checks": checks}


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--self-test", action="store_true", required=True)
    parser.parse_args()
    print(json.dumps(self_test(), ensure_ascii=False, indent=2))
