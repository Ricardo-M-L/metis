#!/usr/bin/env python3
"""Check a packaged CLI through a private byte-identical copy and real queries.

Required: --binary /path/to/unpacked/metis --output /new/evidence/directory.
The supplied binary is never edited, executed in place or removed. No rebuild,
install, provider/model switch, ZIP modification or staging cleanup is performed.
Python 3.11+ and the adjacent live-check helper scripts are required.

Checks package fidelity and verbose CLI version, then authenticated HTTP event
ingestion -> automatic model Read/Bash/Write -> independent host acceptance.
A second real-query event has host acceptance forced to exit 7; the child must
exit 11 while the service remains available and processes a new valid event.
The full --json coordinator branch then reloads that blocked state, emits no
new outcomes, preserves failed verification evidence and also exits 11 without
making another model request.
This focused package check does not repeat the two-hour soak or crash suite.

All owned service/worker processes, the private binary copy, copied credentials
and bearer token are cleaned up on success or failure. The original is hashed
again afterward to verify preservation. Expected runtime is approximately 3-8
minutes, depending on the currently configured real model/provider.
"""

from __future__ import annotations

import argparse
import hashlib
import json
import os
from pathlib import Path
import shutil
import signal
import time
from typing import Any

from project_coordinator_live_check import atomic_json, require, utc_now
from project_coordinator_service_live_check import ServiceLiveCheck


def file_sha256(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as stream:
        for chunk in iter(lambda: stream.read(1024 * 1024), b""):
            digest.update(chunk)
    return digest.hexdigest()


class PackageLiveCheck(ServiceLiveCheck):
    def __init__(self, args: argparse.Namespace) -> None:
        super().__init__(args)
        self.supplied_path = args.binary.expanduser().absolute()
        self.source_binary = self.supplied_path.resolve()
        self.source_sha256: str | None = None
        self.source_stat: dict[str, Any] | None = None
        self.report.update({
            "mode": "package-live", "binary_source": "provided packaged CLI; no source compilation",
            "supplied_binary": str(self.supplied_path), "resolved_supplied_binary": str(self.source_binary),
            "boundary": "Package fidelity plus real HTTP event dispatch, file delivery, host acceptance and INCOMPLETE exit 11. The provided binary is preserved; only an isolated copy is executed and destroyed. Two-hour soak and crash recovery are separate evidence.",
        })

    def stage_binary(self) -> None:
        require(self.source_binary.is_file(), "--binary must identify an existing unpacked CLI file")
        require(self.source_binary != self.binary and self.root not in self.source_binary.parents,
                "Input binary must be outside this invocation's temporary cleanup root")
        before = self.source_binary.stat()
        self.source_stat = {"size": before.st_size, "mode": before.st_mode & 0o777,
                            "mtime_ns": before.st_mtime_ns, "device": before.st_dev, "inode": before.st_ino}
        self.source_sha256 = file_sha256(self.source_binary)
        shutil.copyfile(self.source_binary, self.binary)
        self.binary.chmod(0o700)
        copied_sha256 = file_sha256(self.binary)
        require(copied_sha256 == self.source_sha256, "Isolated CLI copy does not match the supplied package bytes")
        require(file_sha256(self.source_binary) == self.source_sha256, "Input binary changed during staging")
        version_text, command = self.command(["version", "--verbose"], "packaged-cli-version", timeout=30)
        require("(Metis)" in version_text, "Supplied CLI did not return a Metis verbose version fingerprint")
        manifest = {"source": str(self.source_binary), "copy": str(self.binary),
                    "source_sha256": self.source_sha256, "copy_sha256": copied_sha256,
                    "byte_identical": True, "source_stat": self.source_stat,
                    "version_output": version_text.strip(), "version_command": command,
                    "rebuilt": False, "recorded_utc": utc_now()}
        self.report["package_fidelity"] = manifest
        self.report["build"] = {"origin": "provided packaged CLI", "sha256": copied_sha256,
                                "binary": str(self.binary), "rebuilt": False}
        atomic_json(self.output / "binary-manifest.json", manifest)
        self.record("isolated package CLI copy is byte-identical and reports its version",
                    source_sha256=self.source_sha256, copy_sha256=copied_sha256,
                    verbose_version=version_text.strip(), original_executed_in_place=False)

    def assert_original_preserved(self) -> None:
        require(self.source_sha256 is not None and self.source_stat is not None,
                "No original CLI fingerprint was recorded")
        after = self.source_binary.stat()
        after_hash = file_sha256(self.source_binary)
        after_stat = {"size": after.st_size, "mode": after.st_mode & 0o777,
                      "mtime_ns": after.st_mtime_ns, "device": after.st_dev, "inode": after.st_ino}
        unchanged = after_hash == self.source_sha256 and after_stat == self.source_stat
        self.report["original_preservation"] = {"path": str(self.source_binary), "exists": True,
                                                 "sha256_after": after_hash, "sha256_before": self.source_sha256,
                                                 "content_and_file_metadata_unchanged": unchanged}
        require(unchanged, "Supplied CLI content or file metadata changed during the package test")

    def assert_full_json_blocked_status(self) -> None:
        before = self.state_on_disk(self.run_id)
        require(before.get("status") == "blocked", "Full JSON negative check requires the actual failed-query blocked graph")
        require(not any(item["status"] in ("ready", "running") for item in before.get("items", [])),
                "Full JSON negative check must have no claimable or running work")
        evidence_fields = ("status", "attempts", "failure", "acceptance", "output", "artifacts")
        failed = {item["id"]: {key: item.get(key) for key in evidence_fields}
                  for item in before["items"] if item["phase"] == "verification" and item["status"] == "failed" and
                  any(check.get("exit_code") == 7 for check in (item.get("acceptance") or {}).get("checks", []))}
        require(failed, "No persisted host exit 7 verification evidence exists for the JSON negative check")
        snapshot = self.trace_snapshot()
        result, command = self.coord("run", self.run_id, "--worker", "package-json-status",
                                     label="package-full-json-blocked-status", expect=11, timeout=30)
        require(result.get("outcomes") == [], "Full JSON blocked reload unexpectedly executed new work")
        require(result.get("run", {}).get("status") == "blocked", "Full JSON output masked the persisted blocked run")
        reloaded_items = {item["id"]: item for item in result["run"]["items"]}
        require(not any(item["status"] in ("ready", "running") for item in reloaded_items.values()),
                "Full JSON reload unexpectedly changed the idle blocked graph")
        for identifier, prior in failed.items():
            current = reloaded_items.get(identifier, {})
            require({key: current.get(key) for key in evidence_fields} == prior,
                    "Full JSON reload lost or changed old failed verification evidence: " + identifier)
        model_evidence = self.trace_evidence(self.trace_events(snapshot))
        require(model_evidence["model_responses"] == 0 and not model_evidence["tool_results"],
                "Blocked full JSON status check unexpectedly made a model request or used a tool")
        self.record("packaged CLI full JSON branch preserves blocked failures and exits 11 without new model work",
                    exit_code=command["exit_code"], outcomes=[], run_status=result["run"]["status"],
                    preserved_failed_verification_ids=sorted(failed), persisted_host_failure_exit=7,
                    model_responses=0, command=command)

    def run(self) -> int:
        try:
            self.setup_provider()
            self.stage_binary()
            self.write_checker()
            self.setup_service()
            service = self.start_service("packaged-cli-resident-service")
            event = self.make_event("package-http-delivery", 1)
            snapshot = self.trace_snapshot()
            acknowledgement = self.post_event(service, event)
            state, proof = self.await_events(service, [event], snapshot)
            require(state["status"] == "completed", "Packaged CLI did not complete the first HTTP event")
            self.record("packaged CLI automatically processes HTTP event with real tools and host acceptance",
                        event_id=event["item_id"], http_acknowledgement=acknowledgement, proof=proof,
                        manual_coordinator_run_used=False)
            # Reuse the real-service failure scenario: a fresh model event is
            # executed with exit 7 host acceptance, its child exit 11 is logged,
            # and another HTTP event then succeeds without erasing that failure.
            service = self.failed_acceptance(service)
            self.await_idle(service)
            self.assert_full_json_blocked_status()
            self.stop_service(service)
            self.report["passed"] = True
        except BaseException as error:
            self.report["failure"] = self.redact(str(error))
            self.report["failure_type"] = type(error).__name__
        finally:
            # Stop and destroy owned runtime resources before producing a
            # report that could be followed by a source repair/rebuild.
            try:
                self.cleanup()
            except BaseException as error:
                self.report["passed"] = False
                self.report["cleanup_failure"] = self.redact(str(error))
            try:
                self.assert_original_preserved()
            except BaseException as error:
                self.report["passed"] = False
                self.report["original_preservation_failure"] = self.redact(str(error))
            try:
                self.copy_evidence()
            except BaseException as error:
                self.report["passed"] = False
                self.report["evidence_copy_failure"] = self.redact(str(error))
            self.report["elapsed_seconds"] = round(time.monotonic() - self.started, 3)
            self.report["finished_utc"] = utc_now()
            atomic_json(self.output / "verification-report.json", json.loads(self.redact(json.dumps(self.report, ensure_ascii=False))))
            self.progress("finished", passed=self.report["passed"], failure=self.report.get("failure"),
                          cleanup=self.report.get("cleanup"), original_preservation=self.report.get("original_preservation"))
        print(self.redact(json.dumps({"passed": self.report["passed"], "checks": len(self.report["checks"]),
                                      "report": str(self.output / "verification-report.json"), "failure": self.report.get("failure"),
                                      "cleanup": self.report.get("cleanup"), "original_preservation": self.report.get("original_preservation")},
                                     ensure_ascii=False)), flush=True)
        return 0 if self.report["passed"] else 1


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("--binary", type=Path, required=True, help="existing unpacked CLI; preserved and never executed in place")
    parser.add_argument("--output", type=Path, required=True, help="new or empty private evidence directory")
    parser.add_argument("--stage-timeout", type=float, default=600, help="maximum seconds per real event stage")
    parser.set_defaults(repo=Path(__file__).resolve().parents[2], quick=True, soak_seconds=0)
    args = parser.parse_args()
    require(args.stage_timeout > 0, "--stage-timeout must be positive")
    os.umask(0o077)

    def interrupted(signum: int, _frame: Any) -> None:
        raise KeyboardInterrupt("Package harness interrupted by " + signal.Signals(signum).name)

    signal.signal(signal.SIGINT, interrupted)
    signal.signal(signal.SIGTERM, interrupted)
    return PackageLiveCheck(args).run()


if __name__ == "__main__":
    raise SystemExit(main())
