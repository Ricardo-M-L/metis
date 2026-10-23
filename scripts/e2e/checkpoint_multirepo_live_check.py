#!/usr/bin/env python3
"""Opt-in checkpoint acceptance in a real multi-project cwd.

The caller builds and freezes a disposable CLI; this runner never builds or
installs one. A non-TTY `metis chat` executes real selected-route queries and
the product's `/rewind` command. Only nonce-owned fixture files may be changed.
An actual PreToolUse policy fails closed for every other tool/path/command.
Checkpoints remain local, including any unrelated files the implementation
scans; evidence retains only fixture blobs, scalar proofs and safe transcripts.
Both supplied/private CLIs, credentials, checkpoints and fixtures are removed
on success or failure, before any source repair. Offline tests use no auth.
"""
from __future__ import annotations

import argparse
from collections import Counter
import hashlib
import json
import os
from pathlib import Path
import re
import shlex
import shutil
import signal
import subprocess
import sys
import time
from typing import Any
import uuid

from persistent_agent_live_check import (PersistentAgentLiveCheck, atomic_json,
    digest, history_snapshots, require, tool_calls, trace_proof, utc_now)


AUTH_COMMAND = "metis auth keys help; metis auth keys list"
ANSI = re.compile(r"\x1b\[[0-?]*[ -/]*[@-~]")


def sha_text(text: str) -> str:
    return hashlib.sha256(text.encode()).hexdigest()


def owned_file(path: str, fixture: Path, allowed: set[str]) -> Path:
    candidate = Path(path)
    require(candidate.is_absolute() and str(candidate) in allowed,
            "Tool path is outside the explicit fixture allowlist")
    require(fixture.is_dir() and not fixture.is_symlink(), "Fixture root changed identity")
    require(candidate.resolve().is_relative_to(fixture.resolve()), "Tool path escapes the owned fixture")
    current = candidate
    while current != fixture.parent:
        require(not current.is_symlink(), "Symlink in owned fixture path")
        current = current.parent
    return candidate


def guard_decision(event: dict[str, Any], plan: dict[str, Any]) -> None:
    require(event.get("hook_event_name") == "PreToolUse", "Unexpected policy event")
    require(event.get("session_id") == plan["session_id"], "Unexpected policy session")
    name, arguments = event.get("tool_name"), event.get("tool_input")
    require(isinstance(arguments, dict), "Tool arguments must be an object")
    fixture = Path(plan["fixture"])
    if name == "Bash":
        require(arguments.get("command") in plan["commands"], "Bash command is outside the exact acceptance plan")
        require(not any(arguments.get(name) for name in ("run_in_background", "await_completion", "required_for_completion")),
                "Background operations are forbidden")
        require(arguments.get("cwd", plan["cwd"]) == plan["cwd"] and
                arguments.get("working_dir", plan["cwd"]) == plan["cwd"], "Bash cwd changed")
        # Every shell argument other than harmless presentation/deadline
        # controls is rejected; no alternate interpreter or env injection.
        require(set(arguments) <= {"command", "description", "timeout_ms", "timeout",
                "run_in_background", "await_completion", "required_for_completion", "cwd", "working_dir"},
                "Unexpected Bash control argument")
        for value in plan["paths"]:
            owned_file(value, fixture, set(plan["paths"]))
        return
    require(name in ("Read", "Write", "Edit"), "Unexpected tool")
    raw = arguments.get("file_path", arguments.get("path"))
    require(isinstance(raw, str), "Missing file path")
    if "path" in arguments and "file_path" in arguments:
        require(arguments["path"] == arguments["file_path"], "Conflicting path aliases")
    owned_file(raw, fixture, set(plan["paths"]))
    if name == "Write":
        require(raw == plan["write_path"] and arguments.get("content") == plan["write_content"],
                "Write differs from the exact fixture mutation")
    if name == "Edit":
        require(raw == plan["edit_path"] and arguments.get("old", arguments.get("old_string")) == plan["edit_old"] and
                arguments.get("new", arguments.get("new_string")) == plan["edit_new"] and
                not arguments.get("all", arguments.get("replace_all")),
                "Edit differs from the exact fixture mutation")


def run_guard(plan_path: Path) -> int:
    # Invoked by the real CLI policy boundary. It sees tool arguments, never
    # provider credentials. Invalid/missing plans and malformed input halt.
    decision, reason = "allow", ""
    event: dict[str, Any] = {}
    try:
        plan = json.loads(plan_path.read_text())
        event = json.loads(sys.stdin.read(1024 * 1024 + 1))
        guard_decision(event, plan)
    except BaseException as error:
        decision, reason = "halt", str(error)
    try:
        row = {"utc": utc_now(), "decision": decision, "tool": event.get("tool_name"),
               "input_sha256": sha_text(json.dumps(event.get("tool_input"), sort_keys=True)), "reason": reason}
        with plan_path.with_suffix(".audit.jsonl").open("a") as output:
            output.write(json.dumps(row) + "\n")
    except BaseException:
        decision, reason = "halt", "Acceptance policy audit could not be persisted"
    print(json.dumps({"decision": decision, "reason": reason}))
    return 0


def managed_oracle(value: Any, fixture_relative: str) -> dict[str, str]:
    require(isinstance(value, dict), "Checkpoint managed-path journal is malformed")
    for path, fingerprint in value.items():
        parts = Path(path).parts
        require(not Path(path).is_absolute() and ".." not in parts and
                len(parts) >= 3 and parts[0] == fixture_relative and
                parts[1] in ("project-a", "project-b") and isinstance(fingerprint, str),
                "Refusing restore: checkpoint owns an unrelated path")
    return value


def readonly_managed_oracle(value: Any) -> None:
    require(isinstance(value, dict) and all(isinstance(path, str) and isinstance(state, str)
            for path, state in value.items()), "Read-only checkpoint journal is malformed")
    require(not value, "Read-only Bash incorrectly attributed filesystem mutations; refusing further mutation or restore")


def readonly_diagnostic(managed: dict[str, str], head_paths: set[str], ignored_paths: set[str]) -> dict[str, Any]:
    # A novel extension can itself contain a person's name or a token. Keep
    # common format names only; neither path fragments nor file bodies leave
    # the private shadow repository through this diagnostic.
    known = {".go", ".py", ".js", ".ts", ".tsx", ".jsx", ".json", ".jsonl", ".md", ".txt", ".yaml", ".yml",
             ".toml", ".xml", ".html", ".css", ".sh", ".csv", ".sql", ".lock", ".log", ".sum", ".mod",
             ".png", ".jpg", ".jpeg", ".svg", ".pdf", ".proto", ".conf", ".ini", ".rst"}
    counts: Counter[str] = Counter()
    for path in managed:
        extension = Path(path).suffix.lower()
        counts[extension if extension in known else "<none>" if not extension else "<other>"] += 1
    return {"path_count": len(managed), "missing_from_HEAD_count": len(set(managed) - head_paths),
            "gitignored_count": len(set(managed) & ignored_paths), "extension_counts": dict(sorted(counts.items())),
            "user_paths_exported": False, "file_contents_exported": False}


def assert_fixture(fixture: Path, expected: dict[str, str]) -> dict[str, str]:
    actual: dict[str, str] = {}
    for candidate in fixture.rglob("*"):
        require(not candidate.is_symlink(), "Fixture contains an unexpected symlink")
        if candidate.is_file():
            actual[str(candidate.relative_to(fixture))] = candidate.read_text()
    require(actual == expected, "Actual fixture files differ from the expected host-owned state")
    return {name: sha_text(text) for name, text in actual.items()}


class CheckpointMultiRepoLiveCheck(PersistentAgentLiveCheck):
    def __init__(self, args: argparse.Namespace) -> None:
        super().__init__(args)
        self.workspace = args.cwd.resolve()
        # Bash strips METIS_*; making both homes identical keeps its nested
        # `metis auth keys ...` safely on the same private defaults.
        self.home = self.user_home / ".metis"
        self.home.mkdir(mode=0o700)
        self.nonce = uuid.uuid4().hex[:16]
        self.sid = str(uuid.uuid4())
        self.fixture = self.workspace / ("metis-checkpoint-live-" + self.nonce)
        self.fixture_created = False
        self.destroy_source_allowed = False
        self.total_tool_calls = 0
        self.child: tuple[subprocess.Popen, Path, Path] | None = None
        self.originals: dict[str, str | None] = {}
        self.report["boundary"] = ("Three real CLI chat turns in the unchanged multi-project cwd; exact policy-bound "
            "Read/Write/Edit/Bash mutations in two owned fixture subdirectories; local shadow baseline inspection "
            "and actual /rewind file/history verification. No user-code changes, GUI, model stubs, provider substitution, "
            "broad restore, restart durability or crash/external exactly-once claim.")

    def prepare(self) -> None:
        require(self.workspace.is_dir() and self.workspace != self.repo and self.workspace not in self.repo.parents,
                "Supply the actual multi-project cwd, separate from the implementation checkout")
        require(not self.fixture.exists(), "Fixture path already exists")
        # Project executable configuration must not override the isolated
        # policy or launch unrelated components during acceptance.
        for name in (".metis/config.toml", ".metis/config.local.toml", ".metis/mcp.toml", ".mcp.json"):
            require(not (self.workspace / name).exists(), "Workspace executable configuration needs separate review: " + name)
        original_home = Path(os.environ.get("METIS_HOME", str(Path.home() / ".metis"))).expanduser().resolve()
        guarded = [original_home / "config.toml", original_home / ".credentials/auth.json",
                   Path("/Applications/Metis.app/Contents/Resources/bin/metis")]
        installed = shutil.which("metis")
        if installed:
            guarded.append(Path(installed).resolve())
        require(self.source_binary not in guarded, "Never supply the installed CLI as disposable")
        self.destroy_source_allowed = True
        self.originals = {str(path): digest(path) if path.is_file() else None for path in guarded}
        self.report["original_files_before"] = self.originals
        self.stage()
        self.load_route()
        require(self.provider == self.args.provider and self.model == self.args.model,
                "Selected provider/model differ from the explicitly expected live route")
        self.fixture.mkdir(mode=0o700)
        self.fixture_created = True
        for name in ("project-a", "project-b"):
            (self.fixture / name).mkdir(mode=0o700)
        self.initial = {"project-a/existing.txt": "ALPHA_BEFORE_" + self.nonce + "\n",
                        "project-b/existing.txt": "BETA_BEFORE_" + self.nonce + "\n",
                        "project-a/deleted.txt": "DELETE_BEFORE_" + self.nonce + "\n",
                        "project-b/rename-source.txt": "RENAME_BEFORE_" + self.nonce + "\n",
                        "project-a/untouched.txt": "UNTOUCHED_" + self.nonce + "\n"}
        for name, text in self.initial.items():
            (self.fixture / name).write_text(text)
        self.write_text = "ALPHA_AFTER_" + self.nonce + "\n"
        self.edit_text = "BETA_AFTER_" + self.nonce + "\n"
        self.bash_text = "BASH_AFTER_" + self.nonce + "\n"
        self.created_text = "CREATED_" + self.nonce + "\n"
        target = lambda name: shlex.quote(str(self.fixture / name))
        self.mutation_command = " && ".join([
            "printf '%s\\n' " + shlex.quote(self.bash_text.rstrip("\n")) + " > " + target("project-a/existing.txt"),
            "printf '%s\\n' " + shlex.quote(self.created_text.rstrip("\n")) + " > " + target("project-b/created.txt"),
            "rm -- " + target("project-a/deleted.txt"),
            "mv -- " + target("project-b/rename-source.txt") + " " + target("project-a/renamed.txt")])
        all_paths = [str(self.fixture / name) for name in (*self.initial, "project-b/created.txt", "project-a/renamed.txt")]
        self.plan_path = self.root / "policy.json"
        self.plan = {"session_id": self.sid, "cwd": str(self.workspace), "fixture": str(self.fixture),
            "paths": all_paths, "commands": [AUTH_COMMAND, self.mutation_command],
            "write_path": str(self.fixture / "project-a/existing.txt"), "write_content": self.write_text,
            "edit_path": str(self.fixture / "project-b/existing.txt"),
            "edit_old": self.initial["project-b/existing.txt"], "edit_new": self.edit_text}
        atomic_json(self.plan_path, self.plan)
        hook = " ".join(shlex.quote(value) for value in (sys.executable, "-B", str(Path(__file__).resolve()), "--guard", str(self.plan_path)))
        config_path = self.home / "config.toml"
        config_text = config_path.read_text().replace(
            'allowed = ["Read", "Write", "MessageTeammate", "Skill", "Workflow", "Agent"]',
            'allowed = ["Read", "Write", "Edit", "Bash"]')
        config_text += '\n[[permission.allow]]\ntool = "Edit"\n'
        config_text += '\n[[hooks.pre_tool_use]]\ntype = "command"\ntimeout_seconds = 10\ncommand = ' + json.dumps(hook) + '\n'
        config_path.write_text(config_text)
        self.env.update({"PATH": str(self.binary.parent) + os.pathsep + os.environ.get("PATH", "/usr/bin:/bin"),
                         "NO_COLOR": "1", "TERM": "dumb", "METIS_LAZY_MCP": "off"})
        require(shutil.which("metis", path=self.env["PATH"]) == str(self.binary), "PATH did not select the private CLI")
        self.report["isolation"] = {"cwd": str(self.workspace), "fixture": str(self.fixture),
            "HOME": str(self.user_home), "METIS_HOME": str(self.home), "checkpoint_root": str(self.home / "checkpoints"),
            "private_cli_on_path": str(self.binary), "model_tools": ["Read", "Write", "Edit", "Bash"],
            "policy_sha256": digest(self.plan_path), "project_config_absent": True}
        self.record("private CLI and policy prepared without narrowing the real multi-project cwd", **self.report["isolation"])

    def start(self) -> None:
        out, err = self.output / "chat.stdout.log", self.output / "chat.stderr.log"
        handles = out.open("w"), err.open("w")
        system = ("Execute only the explicit acceptance instructions. Do not inspect unrelated files, delegate, "
                  "run other commands, or use background jobs. Preserve the exact tool names, commands, contents and paths. "
                  "All requested fixture mutations are authorized. Return DONE after the requested tools succeed.")
        args = ["chat", "--no-markdown", "--no-auth-wizard", "--session-id", self.sid,
                "--provider", self.provider, "--model", self.model, "--mode", "dontAsk",
                "--tools", "Read,Write,Edit,Bash", "--max-iter", "12", "--system", system]
        process = subprocess.Popen([str(self.binary), *args], cwd=self.workspace, env=self.env,
            stdin=subprocess.PIPE, stdout=handles[0], stderr=handles[1], text=True, start_new_session=True)
        self.processes.append(process)
        self.handles[process.pid] = handles
        self.child = process, out, err
        self.report["commands"].append({"label": "real-plain-repl", "pid": process.pid, "arguments": args,
                                        "cwd": str(self.workspace), "stdout": out.name, "stderr": err.name})
        self.wait_prompt(0, 40)

    def wait_prompt(self, offset: int, timeout: float) -> str:
        require(self.child is not None, "REPL not started")
        process, out, err = self.child
        deadline = time.monotonic() + timeout
        while time.monotonic() < deadline:
            require(process.poll() is None, "CLI exited before the next prompt: " + self.redact(err.read_text()[-1500:]))
            body = out.read_text(errors="replace")[offset:]
            if ANSI.sub("", body).endswith("> "):
                return body
            time.sleep(0.05)
        raise AssertionError("CLI did not return to its plain REPL prompt before the deadline")

    def send(self, text: str, label: str) -> tuple[str, list[dict[str, Any]]]:
        require(self.child is not None and "\n" not in text, "REPL instructions must be a single line")
        process, out, _ = self.child
        offset, before, started = len(out.read_text(errors="replace")), self.trace_snapshot(), time.monotonic()
        require(process.stdin is not None, "REPL stdin closed")
        process.stdin.write(text + "\n")
        process.stdin.flush()
        output = self.wait_prompt(offset, self.args.query_timeout)
        rows = self.trace_events(before)
        require(not re.search(r"checkpoint .*skipped|checkpoint.*(?:timed out|deadline exceeded|unavailable)|context deadline exceeded|rewind protection.*incomplete", output, re.I),
                label + " reported an incomplete code checkpoint")
        self.report["commands"].append({"label": label, "elapsed_seconds": round(time.monotonic() - started, 3),
            "output_sha256": sha_text(output), "query_sha256": sha_text(text), "cwd": str(self.workspace)})
        return output, rows

    def proof(self, rows: list[dict[str, Any]], expected: dict[str, int], label: str) -> dict[str, Any]:
        proof = trace_proof(rows, self.provider, self.model)
        require(proof["session_ids"] == [self.sid] and proof["model_responses"] > 0 and
                any(sum(row[name] for name in ("input", "output", "cache_read", "cache_write")) > 0 for row in proof["usage"]),
                "Real query lacks owned nonzero provider usage")
        require(not proof["errors"] and proof["tool_results"] == expected, label + " has unexpected tool errors/counts")
        transcript = self.home / "sessions" / (self.sid + ".jsonl")
        records = [json.loads(line) for line in transcript.read_text().splitlines() if line]
        header = next(row["header"] for row in records if row.get("type") == "header")
        require(header.get("provider") == self.provider and header.get("model") == self.model and
                Path(header.get("cwd", header.get("work_dir", ""))).resolve() == self.workspace,
                "Persisted session route/cwd differs from the selected live query")
        audit = self.plan_path.with_suffix(".audit.jsonl")
        entries = [json.loads(line) for line in audit.read_text().splitlines() if line]
        self.total_tool_calls += sum(expected.values())
        require(len(entries) == self.total_tool_calls and all(row["decision"] == "allow" for row in entries),
                "Acceptance policy rejected an operation or failed to observe every tool")
        atomic_json(self.output / (label + "-trace-proof.json"), proof)
        return proof

    def shadow(self) -> Path:
        path = self.home / "checkpoints" / self.sid
        require((path / ".git").is_dir() and not path.is_symlink(), "Successful mutations have no real private shadow repository")
        return path

    def git(self, *args: str) -> bytes:
        result = subprocess.run(["git", "-C", str(self.shadow()), *args], capture_output=True, timeout=30,
                                env={**self.env, "GIT_CONFIG_NOSYSTEM": "1"})
        require(result.returncode == 0, "Local shadow inspection failed: " + result.stderr.decode(errors="replace")[:300])
        return result.stdout

    def checkpoint_proof(self, names: list[str], label: str) -> dict[str, Any]:
        managed_file = self.shadow() / ".git" / "metis-managed-paths.json"
        managed = managed_oracle(json.loads(managed_file.read_text()), self.fixture.name)
        required = {self.fixture.name + "/" + name for name in names}
        require(required <= managed.keys(), "Checkpoint did not record every actual owned mutation")
        hashes = self.git("rev-list", "HEAD").decode().splitlines()
        require(hashes, "Checkpoint has no committed baseline")
        matched: dict[str, dict[str, str]] = {}
        for name in names:
            if name not in self.initial:
                continue
            relative = self.fixture.name + "/" + name
            for commit in hashes:
                value = subprocess.run(["git", "-C", str(self.shadow()), "show", commit + ":" + relative],
                                       capture_output=True, timeout=30, env=self.env)
                if value.returncode == 0 and value.stdout == self.initial[name].encode():
                    matched[name] = {"commit": commit, "baseline_sha256": sha_text(self.initial[name])}
                    break
            require(name in matched, "Checkpoint lacks the actual pre-edit blob for " + name)
        proof = {"managed_paths": sorted(managed), "baseline_blobs": matched,
                 "shadow_commit_count": len(hashes), "unrelated_restore_paths": 0,
                 "unrelated_shadow_contents_exported": False}
        atomic_json(self.output / (label + "-checkpoint-proof.json"), proof)
        return proof

    def readonly_checkpoint_proof(self) -> dict[str, Any]:
        shadow = self.shadow()
        journal = shadow / ".git" / "metis-managed-paths.json"
        managed = json.loads(journal.read_text()) if journal.exists() else {}
        require(isinstance(managed, dict) and all(isinstance(path, str) and isinstance(state, str)
                and "\0" not in path for path, state in managed.items()), "Read-only checkpoint journal is malformed")
        head = {os.fsdecode(path) for path in self.git("ls-tree", "-r", "--name-only", "-z", "HEAD").split(b"\0") if path}
        ignored: set[str] = set()
        if managed:
            checked = subprocess.run(["git", "-C", str(shadow), "check-ignore", "--no-index", "-z", "--stdin"],
                input=b"".join(os.fsencode(path) + b"\0" for path in managed), capture_output=True,
                timeout=30, env={**self.env, "GIT_CONFIG_NOSYSTEM": "1"})
            require(checked.returncode in (0, 1), "Private shadow ignore diagnostic failed")
            ignored = {os.fsdecode(path) for path in checked.stdout.split(b"\0") if path}
        proof = {**readonly_diagnostic(managed, head, ignored), "managed_journal_present": journal.exists(),
                 "HEAD_path_count": len(head)}
        # Persist this bounded projection before fail-closed assertion, so a
        # rejected run still explains false attributions without exporting
        # the user's unrelated shadow files or full path list.
        atomic_json(self.output / "readonly-checkpoint-diagnostic.json", proof)
        self.report["readonly_checkpoint_diagnostic"] = proof
        readonly_managed_oracle(managed)
        return proof

    def rewind(self, label: str) -> None:
        # The product restore uses its managed-path union. Gate the actual
        # call on an independent disk journal check, never on model claims.
        managed_oracle(json.loads((self.shadow() / ".git/metis-managed-paths.json").read_text()), self.fixture.name)
        transcript = self.home / "sessions" / (self.sid + ".jsonl")
        before = history_snapshots(transcript)[-1]
        output, rows = self.send("/rewind", label)
        require("rewound: restored files + undid 1 turn(s)" in ANSI.sub("", output),
                "Product /rewind did not report an actual one-turn file/conversation restore")
        require(not trace_proof(rows, self.provider, self.model)["model_responses"], "/rewind unexpectedly queried the provider")
        after = history_snapshots(transcript)[-1]
        prompts = [index for index, row in enumerate(before) if row.get("role") == "user" and
                   any(block.get("type") == "text" for block in row.get("content", []))]
        require(prompts and after == before[:prompts[-1]],
                "Product rewind did not persist the exact conversation prefix before the reverted turn")
        state = assert_fixture(self.fixture, self.initial)
        self.record(label + " restores exact fixture files and persists conversation rewind", restored_files=state,
                    history_before=len(before), history_after=len(after), actual_product_rewind=True)

    def exercise(self) -> None:
        self.start()
        _, rows = self.send("Use Bash exactly once with command " + json.dumps(AUTH_COMMAND) +
            ". Do not use any other tool, change directory or modify files. Return DONE.", "readonly-real-query")
        proof = self.proof(rows, {"Bash": 1}, "readonly-real-query")
        require(tool_calls(rows, "Bash")[0]["arguments"].get("command") == AUTH_COMMAND, "Model changed the original read-only reproduction")
        assert_fixture(self.fixture, self.initial)
        checkpoint = self.readonly_checkpoint_proof()
        self.record("original read-only Bash completes without checkpoint timeout or falsely attributed changes in the real multi-project cwd",
                    real_query=proof, checkpoint=checkpoint)
        prompt = ("Execute sequentially: Read " + json.dumps(self.plan["write_path"]) + "; then Write that file with exact content " +
            json.dumps(self.write_text) + "; Read " + json.dumps(self.plan["edit_path"]) + "; then Edit that file replacing exact old " +
            json.dumps(self.plan["edit_old"]) + " with exact new " + json.dumps(self.edit_text) +
            ". Use only those four tools, once each in that order; do not use Bash. Return DONE.")
        _, rows = self.send(prompt, "write-edit-real-query")
        proof = self.proof(rows, {"Read": 2, "Write": 1, "Edit": 1}, "write-edit-real-query")
        expected = {**self.initial, "project-a/existing.txt": self.write_text, "project-b/existing.txt": self.edit_text}
        actual = assert_fixture(self.fixture, expected)
        checkpoint = self.checkpoint_proof(["project-a/existing.txt", "project-b/existing.txt"], "write-edit")
        self.record("real Write and Edit across two owned subprojects have actual baseline blobs", real_query=proof,
                    fixture_files=actual, checkpoint=checkpoint)
        self.rewind("write-edit-rewind")
        _, rows = self.send("Use Bash exactly once with command " + json.dumps(self.mutation_command) +
            ". Do not change directory or use any other tool. All listed paths are disposable test fixtures. Return DONE.", "bash-mutation-real-query")
        proof = self.proof(rows, {"Bash": 1}, "bash-mutation-real-query")
        require(tool_calls(rows, "Bash")[0]["arguments"].get("command") == self.mutation_command, "Model changed the exact Bash mutation")
        expected = {**self.initial, "project-a/existing.txt": self.bash_text, "project-b/created.txt": self.created_text,
                    "project-a/renamed.txt": self.initial["project-b/rename-source.txt"]}
        del expected["project-a/deleted.txt"], expected["project-b/rename-source.txt"]
        actual = assert_fixture(self.fixture, expected)
        changed = ["project-a/existing.txt", "project-b/created.txt", "project-a/deleted.txt", "project-b/rename-source.txt", "project-a/renamed.txt"]
        checkpoint = self.checkpoint_proof(changed, "bash-mutation")
        self.record("real Bash edit/create/delete/rename is fully attributed to owned files", real_query=proof,
                    fixture_files=actual, checkpoint=checkpoint)
        self.rewind("bash-mutation-rewind")
        process, out, _ = self.child
        require(process.stdin is not None, "REPL stdin lost")
        process.stdin.write("/quit\n")
        process.stdin.flush()
        process.wait(timeout=30)
        require(process.returncode == 0, "Clean chat exit was not successful")
        self.report["commands"][0]["exit_code"] = process.returncode
        self.record("same local CLI exits successfully after both actual rewind operations", exit_code=process.returncode)

    def copy_evidence(self) -> None:
        # Never call the parent's workspace collector: cwd is the user's real
        # workspace, not its normal private test workspace.
        evidence = []
        for directory in ("sessions", "traces"):
            for source in (self.home / directory).glob("*.jsonl"):
                if source.stem != self.sid or source.is_symlink():
                    continue
                destination = self.output / (directory + "-" + source.name)
                destination.write_text(self.redact(source.read_text(errors="replace")))
                evidence.append({"path": destination.name, "sha256": digest(destination)})
        audit = self.root / "policy.audit.jsonl"
        if audit.exists():
            destination = self.output / "policy-audit.jsonl"
            destination.write_text(audit.read_text())
            evidence.append({"path": destination.name, "sha256": digest(destination)})
        atomic_json(self.output / "evidence-manifest.json", evidence)

    def cleanup(self) -> None:
        requested_destroy = self.args.destroy_input
        self.args.destroy_input = requested_destroy and self.destroy_source_allowed
        try:
            super().cleanup()
        finally:
            self.args.destroy_input = requested_destroy
            if self.fixture_created and self.fixture.exists():
                require(not self.fixture.is_symlink() and self.fixture.parent == self.workspace and
                        self.fixture.name == "metis-checkpoint-live-" + self.nonce, "Refusing to clean an unowned fixture")
                shutil.rmtree(self.fixture)
            self.report.setdefault("cleanup", {})["owned_fixture_destroyed"] = not self.fixture.exists()

    def verify_originals(self) -> None:
        current = {name: digest(Path(name)) if Path(name).is_file() else None for name in self.originals}
        require(current == self.originals, "Original configuration/auth or installed CLI changed")
        self.report["original_files_unchanged"] = True

    def run(self) -> int:
        signal.setitimer(signal.ITIMER_REAL, self.args.case_timeout)
        try:
            self.prepare()
            self.exercise()
            self.report["passed"] = True
        except BaseException as error:
            self.report.update(failure_type=type(error).__name__, failure=self.redact(str(error)))
        finally:
            signal.setitimer(signal.ITIMER_REAL, 0)
            for label, action in (("cleanup", self.cleanup), ("evidence", self.copy_evidence),
                                  ("freeze", self.verify_freeze), ("originals", self.verify_originals)):
                try:
                    action()
                except BaseException as error:
                    self.report["passed"] = False
                    self.report[label + "_failure"] = self.redact(str(error))
            shutil.rmtree(self.root, ignore_errors=True)
            self.report.setdefault("cleanup", {})["temporary_root_destroyed"] = not self.root.exists()
            if self.root.exists():
                self.report["passed"] = False
            self.report.update(finished_utc=utc_now(), elapsed_seconds=round(time.monotonic() - self.started, 3))
            atomic_json(self.output / "verification-report.json", json.loads(self.redact(json.dumps(self.report))))
            self.progress("finished", passed=self.report["passed"], failure=self.report.get("failure"))
            self.secrets.clear()
        print(json.dumps({"passed": self.report["passed"], "report": str(self.output / "verification-report.json"),
                          "failure": self.report.get("failure")}), flush=True)
        return 0 if self.report["passed"] else 1


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("--repo", type=Path, default=Path(__file__).resolve().parents[2])
    parser.add_argument("--cwd", type=Path, default=Path("/Users/yujun2/Documents/sensetime"))
    parser.add_argument("--binary", type=Path)
    parser.add_argument("--expected-sha256")
    parser.add_argument("--source-freeze", type=Path)
    parser.add_argument("--output", type=Path)
    parser.add_argument("--provider", default="easycliproxyapi")
    parser.add_argument("--model", default="gpt-6.1-sol")
    parser.add_argument("--destroy-input", action="store_true")
    parser.add_argument("--query-timeout", type=float, default=300)
    parser.add_argument("--case-timeout", type=float, default=900)
    parser.add_argument("--guard", type=Path)
    args = parser.parse_args()
    os.umask(0o077)
    if args.guard:
        return run_guard(args.guard)
    require(args.binary and args.expected_sha256 and args.source_freeze and args.output and args.destroy_input,
            "Live mode requires a disposable --binary, --expected-sha256, --source-freeze, --output and --destroy-input")
    require(re.fullmatch(r"[a-f0-9]{64}", args.expected_sha256), "Binary digest must be SHA256")
    require(0 < args.query_timeout <= 600 and 0 < args.case_timeout <= 900, "Query limit must be bounded and total limit must not exceed 15 minutes")
    def interrupted(signum: int, _frame: Any) -> None:
        raise KeyboardInterrupt("Acceptance interrupted by " + signal.Signals(signum).name)
    def timeout(_signum: int, _frame: Any) -> None:
        raise TimeoutError("Acceptance exceeded its total deadline")
    signal.signal(signal.SIGINT, interrupted)
    signal.signal(signal.SIGTERM, interrupted)
    signal.signal(signal.SIGALRM, timeout)
    return CheckpointMultiRepoLiveCheck(args).run()


if __name__ == "__main__":
    raise SystemExit(main())
