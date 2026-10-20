#!/usr/bin/env python3
"""Test a freshly built CLI with isolated state and the local fixture provider."""

from __future__ import annotations

import argparse
import json
import os
from pathlib import Path
import select
import shlex
import subprocess
import sys
import time
from urllib.request import Request, urlopen


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--binary", type=Path, required=True)
    parser.add_argument("--report", type=Path, required=True)
    args = parser.parse_args()
    binary = args.binary.resolve()
    fixture_script = Path(__file__).with_name("desktop_fixture.py")
    clean_env = {key: os.environ[key] for key in ("PATH", "HOME", "TMPDIR", "SHELL", "LANG", "LC_ALL") if key in os.environ}
    fixture = subprocess.Popen([sys.executable, str(fixture_script), "--binary", str(binary), "--no-web", "--duration", "600"], env=clean_env, stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
    report = {"passed": False, "binary": str(binary), "provider": "local controlled OpenAI Responses fixture", "checks": []}
    metadata = None
    try:
        if not select.select([fixture.stdout], [], [], 30)[0]:
            raise RuntimeError("Fixture did not become ready")
        metadata = json.loads(fixture.stdout.readline())
        root = Path(metadata["root"])
        environment = json.loads((root / "environment.json").read_text())
        workspace = metadata["workspace"]
        report["fixture_root"] = str(root)

        def cli(*command, success=True):
            result = subprocess.run([str(binary), "coordinator", *command, "--cwd", workspace, "--json"], env=environment, cwd=workspace, capture_output=True, text=True, timeout=50)
            if success and result.returncode:
                raise AssertionError(f"{command[0]} exited {result.returncode}: {result.stderr[-2000:]}")
            if not success:
                assert result.returncode, "Invalid transition returned success"
                return result
            return json.loads(result.stdout)

        def record(name, **evidence):
            report["checks"].append({"name": name, **evidence})
            print(json.dumps({"passed": name}), flush=True)

        check = shlex.quote(sys.executable) + " -c " + shlex.quote("from pathlib import Path; assert 'Isolated UI acceptance' in Path('fixture-notes.md').read_text()")
        run = cli("create", "--goal", "Coordinator lifecycle [slow:2]", "--check", check, "--max-budget-usd", "1")
        claimed = cli("claim", run["id"], "--worker", "external", "--lease", "1s")
        token = claimed["item"]["lease"]["token"]
        cli("complete", run["id"], "research", "--worker", "external", "--lease-token", token, "--output", "", success=False)
        cli("renew", run["id"], "research", "--worker", "external", "--lease-token", token, "--lease", "1s")
        cli("complete", run["id"], "research", "--worker", "external", "--lease-token", token, "--output", "Workspace inspected")
        record("CLI empty evidence rejection and token-bound renewal")

        outcomes = cli("run", run["id"], "--worker", "managed", "--lease", "1s", "--until-idle")
        assert outcomes["run"]["status"] == "completed", outcomes
        assert all(item["status"] == "completed" for item in outcomes["run"]["items"])
        acceptance = outcomes["run"]["items"][-1]["acceptance"]
        assert acceptance["status"] == "passed" and acceptance["checks"][0]["exit_code"] == 0
        assert all(item["usage"] for item in outcomes["run"]["items"])
        record("Fresh CLI keeps 1s lease during 2s model requests and runs host acceptance", run_id=run["id"], outcomes=len(outcomes["outcomes"]))

        agents = Path(metadata["metis_home"]) / "agents"
        agents.mkdir()
        (agents / "maintainer.md").write_text("---\nname: maintainer\ntools: [Read]\n---\nMaintain the isolated fixture workspace and record concrete evidence.\n")
        event_args = ("event", run["id"], "--source", "ci", "--id", "build-42", "--agent", "maintainer", "--subject", "Inspect CI failure", "--prompt", "Read fixture-notes.md and report findings.", "--deliverable", "fixture-notes.md")
        first, replay = cli(*event_args), cli(*event_args)
        assert first["id"] == replay["id"] and first["agent_profile"] == "maintainer"
        state = cli("status", run["id"])
        assert len(state["items"]) == 6 and state["status"] == "active"
        resumed = cli("run", run["id"], "--until-idle", "--lease", "1s")
        assert resumed["run"]["status"] == "completed", resumed
        event = next(item for item in resumed["run"]["items"] if item["id"] == first["id"])
        assert event["artifacts"][0]["path"] == "fixture-notes.md" and len(event["artifacts"][0]["sha256"]) == 64
        record("Event replay, saved role loading, project reopening and delivery hashes", event_id=first["id"])

        expired = cli("create", "--goal", "Expired", "--deadline", "2000-01-01T00:00:00Z")
        assert cli("status", expired["id"])["status"] == "blocked"
        cli("claim", expired["id"], "--worker", "late", success=False)
        record("Persisted deadline blocks new-process claims")

        failed = cli("create", "--goal", "Failed checks", "--check", "exit 7")
        failed_process = cli("run", failed["id"], "--until-idle", success=False)
        assert failed_process.returncode == 11, "Failed project must return INCOMPLETE"
        failure = json.loads(failed_process.stdout)
        assert failure["run"]["status"] == "blocked", failure
        assert failure["run"]["items"][-1]["acceptance"]["status"] == "failed"
        record("Failed host check cannot complete the graph", run_id=failed["id"])
        report["passed"] = True
    finally:
        if metadata:
            try:
                request = Request(metadata["fixture_url"] + "/fixture/stop", data=b"{}", headers={"Content-Type": "application/json"})
                urlopen(request, timeout=5).close()
            except OSError:
                pass
        if fixture.poll() is None:
            fixture.terminate()
        try:
            fixture.wait(timeout=10)
        except subprocess.TimeoutExpired:
            fixture.kill()
            fixture.wait(timeout=5)
        report["finished"] = time.time()
        args.report.parent.mkdir(parents=True, exist_ok=True)
        args.report.write_text(json.dumps(report, ensure_ascii=False, indent=2))
    print(json.dumps({"passed": report["passed"], "checks": len(report["checks"]), "report": str(args.report.resolve())}), flush=True)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
