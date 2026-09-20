#!/usr/bin/env python3
"""Exercise actual CLI-backed Desktop APIs only inside a desktop_fixture root."""

from __future__ import annotations

import argparse
import json
from pathlib import Path
import subprocess
import time
from urllib.error import HTTPError
from urllib.parse import urlparse
from urllib.request import Request, urlopen


def request(base: str, suffix: str, body=None, method=None, expected=(200,)):
    req = Request(base + suffix, data=json.dumps(body).encode() if body is not None else None,
                  method=method or ("POST" if body is not None else "GET"), headers={"Content-Type": "application/json"})
    try:
        response = urlopen(req, timeout=40)
    except HTTPError as error:
        response = error
    with response:
        status = response.status
        raw = response.read()
    payload = json.loads(raw) if raw else None
    if status not in expected:
        raise AssertionError(f"{req.method} {suffix}: expected {expected}, received {status}: {payload}")
    return payload


def wait_for(probe, predicate, description: str, timeout=25):
    deadline = time.monotonic() + timeout
    value = None
    while time.monotonic() < deadline:
        value = probe()
        if predicate(value):
            return value
        time.sleep(0.15)
    raise AssertionError(f"Timed out waiting for {description}: {value}")


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("manifest", type=Path)
    args = parser.parse_args()
    manifest = json.loads(args.manifest.read_text())
    root = Path(manifest["root"]).resolve()
    if not root.name.startswith("metis-desktop-e2e-") or args.manifest.resolve().parent != root:
        raise RuntimeError("Refusing anything except an isolated desktop_fixture manifest")
    if Path(manifest["metis_home"]).resolve() != root / "home":
        raise RuntimeError("Fixture home must be isolated below the fixture root")
    base, provider = manifest["web_url"], manifest["fixture_url"]
    for address in (base, provider):
        parsed = urlparse(address)
        if parsed.hostname != "127.0.0.1" or parsed.scheme != "http":
            raise RuntimeError("Fixture services must use loopback HTTP")
    environment = json.loads((root / "environment.json").read_text())
    if environment.get("METIS_HOME") != manifest["metis_home"]:
        raise RuntimeError("Fixture environment does not match manifest")
    report = {"started": time.time(), "checks": []}
    checks = report["checks"]
    disposable_sessions = []
    disposable_jobs = []
    token = f"HTTP_E2E_{int(time.time())}"
    try:
        request(base, "/api/health")
        session = request(base, "/api/sessions", {}, expected=(200, 201))
        sid = session.get("id", session.get("sessionId"))
        disposable_sessions.append(sid)
        request(base, "/api/turns", {"sessionId": sid, "input": token + " session history"})
        transcript = request(base, f"/api/sessions/{sid}")
        assert token in json.dumps(transcript), "Actual transcript lacks submitted content"
        assert "FIXTURE_REPLY" in json.dumps(transcript), "Actual transcript lacks model output"
        request(base, "/api/sessions/rename", {"id": sid, "title": token + " renamed"})
        request(base, "/api/sessions/archive", {"id": sid, "archived": True})
        listed = request(base, "/api/sessions")
        assert sid not in {item["id"] for item in listed["sessions"]}
        request(base, "/api/sessions/archive", {"id": sid, "archived": False})
        if manifest.get("sessions"):
            other = manifest["sessions"][0]["id"]
            request(base, "/api/sessions/activate", {"id": other})
            request(base, "/api/sessions/activate", {"id": sid})
        listed = request(base, "/api/sessions")
        assert next(item for item in listed["sessions"] if item["id"] == sid)["title"] == token + " renamed"
        checks.append({"name": "session CRUD persists across activation", "sessionId": sid})

        # Six-field seconds are the actual supported cron parser, not a mocked clock.
        job = request(base, "/api/automations", {
            "name": token, "prompt": token + " BEFORE_EDIT", "paused": True,
            "schedule": {"kind": "cron", "cron": "*/3 * * * * *"},
            "sessionMode": "isolated", "allowTools": [], "repeat": 2,
        }, expected=(201,))
        jid = job["id"]
        disposable_jobs.append(jid)
        job = request(base, f"/api/automations/{jid}", {"name": token + " edited", "prompt": token + " AFTER_EDIT"}, method="PATCH")
        assert job["paused"] and job["prompt"].endswith("AFTER_EDIT")
        request(base, "/api/automations/scheduler", {"enabled": True}, method="PATCH")
        time.sleep(3.3)
        assert request(base, f"/api/automations/{jid}/runs")["runs"] == [], "Paused job fired"
        request(base, f"/api/automations/{jid}", {"paused": False}, method="PATCH")
        records = wait_for(lambda: request(base, f"/api/automations/{jid}/runs")["runs"],
                           lambda rows: any(row["status"] == "succeeded" for row in rows), "an actual scheduled run")
        request(base, f"/api/automations/{jid}", {"paused": True}, method="PATCH")
        scheduled = next(row for row in records if row["status"] == "succeeded")
        assert scheduled["trigger"] == "scheduled", scheduled
        assert scheduled.get("sessionId"), "Scheduled run must link its persisted transcript"
        saved = request(base, "/api/sessions/" + scheduled["sessionId"])
        assert token + " AFTER_EDIT" in json.dumps(saved), "Scheduled transcript does not reflect latest prompt"
        assert "FIXTURE_REPLY" in json.dumps(saved), "Scheduled transcript lacks actual model reply"
        model_calls = request(provider, "/fixture/state")["calls"]
        assert any(token + " AFTER_EDIT" in call["input"] and call["state"] == "completed" for call in model_calls)
        assert not any(token + " BEFORE_EDIT" in call["input"] for call in model_calls)
        result = subprocess.run([manifest["metis_bin"], "cron", "list", "--json"],
                                cwd=manifest["workspace"], env=environment, capture_output=True, text=True, check=True)
        assert any(item["id"] == jid and item["prompt"] == token + " AFTER_EDIT" for item in json.loads(result.stdout))
        checks.append({"name": "due-time scheduler, edited prompt, CLI parity and durable history", "jobId": jid, "run": scheduled})

        # A paused definition may be explicitly run, and in-flight duplicates/deletion must conflict.
        lock_job = request(base, "/api/automations", {
            "name": token + " run lock", "prompt": token + " [gate:manual-run-lock]",
            "paused": True, "schedule": {"kind": "interval", "intervalSeconds": 30}, "allowTools": [],
        }, expected=(201,))
        lock_id = lock_job["id"]
        disposable_jobs.append(lock_id)
        accepted = request(base, f"/api/automations/{lock_id}/run", {}, expected=(202,))
        wait_for(lambda: request(provider, "/fixture/state")["calls"],
                 lambda calls: any("[gate:manual-run-lock]" in call["input"] for call in calls), "manual run reaching model")
        request(base, f"/api/automations/{lock_id}/run", {}, expected=(409,))
        request(base, f"/api/automations/{lock_id}", method="DELETE", expected=(409,))
        request(provider, "/fixture/release", {"gate": "manual-run-lock"})
        manual = wait_for(lambda: request(base, f"/api/automations/{lock_id}/runs/{accepted['runId']}"),
                          lambda run: run["status"] == "succeeded", "manual run completion")
        assert manual["trigger"] == "manual"
        checks.append({"name": "run-now admission, duplicate protection and history", "run": manual})

        for job_id in disposable_jobs[:]:
            wait_for(lambda: request(base, f"/api/automations/{job_id}"), lambda row: not row["running"], "job idle before deletion")
            request(base, f"/api/automations/{job_id}", method="DELETE", expected=(204,))
            request(base, f"/api/automations/{job_id}", expected=(404,))
            disposable_jobs.remove(job_id)
        before = len(request(provider, "/fixture/state")["calls"])
        time.sleep(3.3)
        assert len(request(provider, "/fixture/state")["calls"]) == before, "Deleted automation ran again"
        request(base, "/api/automations/scheduler", {"enabled": False}, method="PATCH")
        request(base, f"/api/sessions/{sid}", method="DELETE")
        disposable_sessions.remove(sid)
        request(base, f"/api/sessions/{sid}", expected=(404,))
        checks.append({"name": "delete persists and prevents subsequent scheduling"})
        report["passed"] = True
    finally:
        # Keep the seeded Alpha/Beta sessions for browser verification. Only own IDs are touched.
        request(provider, "/fixture/release", {"gate": "manual-run-lock"})
        for job_id in disposable_jobs:
            try:
                request(base, f"/api/automations/{job_id}", {"paused": True}, method="PATCH")
            except (HTTPError, AssertionError):
                pass
        request(base, "/api/automations/scheduler", {"enabled": False}, method="PATCH")
        report["finished"] = time.time()
        destination = root / "http-e2e-report.json"
        destination.write_text(json.dumps(report, ensure_ascii=False, indent=2))
        print(json.dumps({"passed": report.get("passed", False), "checks": len(checks), "report": str(destination)}), flush=True)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
