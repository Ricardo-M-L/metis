#!/usr/bin/env python3
"""Freeze sources, run fresh checks, build one disposable CLI and accept it.

No model route is overridden. The live checker owns all provider credentials
and processes and removes both executable copies before this runner returns.
Never repair product sources while this runner still owns a live test CLI.
"""
from __future__ import annotations

import argparse
import hashlib
import json
import os
from pathlib import Path
import shutil
import signal
import subprocess
import sys
import tempfile
import time
from datetime import datetime, timezone


def sha(path: Path) -> str:
    return hashlib.sha256(path.read_bytes()).hexdigest()


def stamp() -> str:
    return datetime.now(timezone.utc).isoformat()


def save(path: Path, value: object) -> None:
    temporary = path.with_suffix(path.suffix + ".tmp")
    temporary.write_text(json.dumps(value, ensure_ascii=False, indent=2) + "\n")
    temporary.chmod(0o600)
    temporary.replace(path)


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--repo", type=Path, default=Path(__file__).resolve().parents[2])
    parser.add_argument("--output", type=Path, required=True)
    parser.add_argument("--go", type=Path, default=Path("/Users/yujun2/.local/go-1.26.4/bin/go"))
    args = parser.parse_args()
    repo, output = args.repo.resolve(), args.output.resolve()
    output.mkdir(parents=True, exist_ok=False, mode=0o700)
    root = Path(tempfile.mkdtemp(prefix="mdo-", dir="/tmp")).resolve()
    root.chmod(0o700)
    def interrupted(signum, _frame):
        raise KeyboardInterrupt("Delivery interrupted by " + signal.Signals(signum).name)
    signal.signal(signal.SIGINT, interrupted)
    signal.signal(signal.SIGTERM, interrupted)
    source, binary = root / "s", root / "metis"
    source.mkdir(mode=0o700)
    report: dict = {"passed": False, "started_utc": stamp(), "checks": {}, "source_snapshot": str(source),
                    "input_binary": str(binary), "installed_client_modified": False}
    built_sha = None
    installed = shutil.which("metis")
    if installed and Path(installed).is_file():
        report["installed_cli_before"] = {"path": installed, "sha256": sha(Path(installed))}

    def progress(phase: str) -> None:
        save(output / "driver-state.json", {**report, "phase": phase, "updated_utc": stamp()})
        print(json.dumps({"phase": phase, "utc": stamp()}), flush=True)

    def run(command: list[str], label: str, *, cwd: Path = source, timeout: int = 1800) -> dict:
        progress(label)
        started = time.monotonic()
        log = output / (label + ".log")
        with log.open("wb") as handle:
            os.fchmod(handle.fileno(), 0o600)
            child = subprocess.Popen(command, cwd=cwd, stdout=handle, stderr=subprocess.STDOUT,
                                     umask=0o022, start_new_session=True)
            try:
                child.wait(timeout=timeout)
            except BaseException:
                # Let the live checker execute its credential/process cleanup
                # instead of killing it before its finally block can run.
                if child.poll() is None:
                    child.send_signal(signal.SIGTERM)
                    try:
                        child.wait(timeout=45)
                    except subprocess.TimeoutExpired:
                        os.killpg(child.pid, signal.SIGKILL)
                        child.wait(timeout=10)
                raise
        result = {"exit_code": child.returncode, "elapsed_seconds": round(time.monotonic()-started,3), "log": log.name}
        if label == "go-test":
            counts = {"passed": 0, "skipped": 0, "failed": 0, "packages_passed": 0}
            failures = []
            for line in log.read_text(errors="replace").splitlines():
                try:
                    row = json.loads(line)
                except ValueError:
                    continue
                action = row.get("Action")
                if row.get("Test"):
                    if action in ("pass","skip","fail"):
                        counts[{"pass":"passed","skip":"skipped","fail":"failed"}[action]] += 1
                    if action=="fail": failures.append({"package":row.get("Package"),"test":row["Test"]})
                elif action=="pass": counts["packages_passed"] += 1
            result.update({"counts":counts,"failures":failures})
        report["checks"][label]=result
        save(output / (label + "-summary.json"),result)
        if child.returncode:
            raise RuntimeError(label + " failed; inspect retained log")
        return result

    try:
        progress("source-freeze")
        names = subprocess.check_output(["git","ls-files","-co","--exclude-standard","-z"],cwd=repo).decode().split("\0")
        files = {}
        for name in sorted(set(names)):
            relative = Path(name)
            if not name or any(part in (".git","node_modules","dist","__pycache__") for part in relative.parts) or relative.suffix==".pyc":
                continue
            original = repo / relative
            if not original.is_file() or original.is_symlink():
                continue
            destination=source / relative
            destination.parent.mkdir(parents=True,exist_ok=True)
            shutil.copy2(original,destination)
            files[name]=sha(destination)
            if sha(original)!=files[name]: raise RuntimeError("source changed during freeze: "+name)
        revision=subprocess.check_output(["git","rev-parse","HEAD"],cwd=repo,text=True).strip()
        manifest={"created_at_utc":stamp(),"original_repository":str(repo),"snapshot_repository":str(source),"captured_revision":revision,"source_files":len(files),"files":files}
        freeze=output / "source-freeze.json"
        save(freeze,manifest)
        report["source_files"]=len(files)
        run([str(args.go),"test","-p","4","-json","-count=1","-timeout=300s","./..."],"go-test")
        run([str(args.go),"vet","-p","4","./..."],"go-vet")
        run(["python3","scripts/e2e/durable_operations_live_check.py","--self-test"],"harness-self-test")
        run(["python3","scripts/e2e/learning_fixture_acceptance.py","--self-test"],"code-fixture-self-test")
        run(["python3","scripts/e2e/desktop_operations_acceptance.py","--self-test"],"desktop-http-self-test")
        run(["node","--check","internal/webui/static/durable_ops.js"],"desktop-js-check")
        # Tests must not mutate a source they later claim to have validated.
        changed=[name for name,digest in files.items() if not (source/name).is_file() or sha(source/name)!=digest]
        if changed: raise RuntimeError("source changed during checks: "+", ".join(changed[:8]))
        date=datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")
        linker="-X github.com/Ricardo-M-L/metis/internal/version.Commit="+revision[:7]+"-dirty -X github.com/Ricardo-M-L/metis/internal/version.Date="+date
        run([str(args.go),"build","-trimpath","-ldflags",linker,"-o",str(binary),"./cmd/metis"],"build-cli")
        built_sha=sha(binary)
        report["build"]={"sha256":built_sha,"compiled_at_utc":date,"commit":revision[:7]+"-dirty","source_manifest_sha256":sha(freeze)}
        save(output / "build-manifest.json",report["build"])
        live=output / "live-cli"
        run([sys.executable,"scripts/e2e/durable_operations_live_check.py","--repo",str(source),"--metis",str(binary),"--expected-sha256",built_sha,"--source-freeze",str(freeze),"--output",str(live),"--destroy-input"],"live-query",timeout=3700)
        live_report=json.loads((live/"verification-report.json").read_text())
        if not live_report.get("passed") or not live_report.get("cleanup",{}).get("private_CLI_destroyed"):
            raise RuntimeError("live acceptance or cleanup was not confirmed")
        current=[name for name,digest in files.items() if not (repo/name).is_file() or sha(repo/name)!=digest]
        if current: raise RuntimeError("original source changed during acceptance: "+", ".join(current[:8]))
        report["live_report"]="live-cli/verification-report.json"
        report["passed"]=True
    except BaseException as error:
        report["failure_type"]=type(error).__name__
        report["failure"]=str(error)
    finally:
        if binary.exists():
            if built_sha is None or sha(binary)==built_sha:
                binary.unlink()
            else:
                report["passed"]=False
                report["cleanup_error"]="supplied test binary was replaced; preserved unexpected file"
        if not binary.exists(): shutil.rmtree(root)
        report["cleanup"]={"input_cli_destroyed":not binary.exists(),"source_snapshot_destroyed":not root.exists()}
        if report.get("installed_cli_before"):
            unchanged = Path(installed).is_file() and sha(Path(installed)) == report["installed_cli_before"]["sha256"]
            report["installed_client_hash_unchanged"] = unchanged
            if not unchanged:
                report["passed"] = False
                report["failure"] = "installed client changed during acceptance"
        report["finished_utc"]=stamp()
        save(output/"delivery-verification.json",report)
        progress("complete" if report["passed"] else "failed")
    return 0 if report["passed"] else 1


if __name__=="__main__":
    raise SystemExit(main())
