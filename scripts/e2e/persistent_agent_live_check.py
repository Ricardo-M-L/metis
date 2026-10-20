#!/usr/bin/env python3
"""Exercise persistent jobs, durable handoffs and measured skill promotion.

The caller supplies a freshly compiled, source-frozen CLI. This script never
builds, installs, changes the original configuration, mocks a model completion,
or substitutes the original selected provider/model/endpoint. Each live query
uses a private home, credential copy and workspace. Expected outputs are host
assertions over files, transcripts, receipts and tool traces, not model claims.

On every outcome all owned process groups are stopped, copied credentials and
both disposable executables (--destroy-input) are destroyed, and only then is
redacted evidence retained. Run --self-test to check the host oracles offline;
offline fixtures are explicitly not evidence of a working provider or CLI.
"""

from __future__ import annotations

import argparse
from collections import Counter
import hashlib
import json
import os
from pathlib import Path
import re
import secrets
import shutil
import signal
import subprocess
import sys
import tempfile
import time
import tomllib
from typing import Any
from urllib.parse import urlsplit
import uuid

from model_context_live_check import digest
from project_coordinator_live_check import atomic_json, require, root_trace_identity, safe_endpoint, toml_table, utc_now


def json_sha(value: Any) -> str:
    return hashlib.sha256(json.dumps(value, ensure_ascii=False, sort_keys=True, separators=(",", ":")).encode()).hexdigest()


def go_history_sha(messages: list[dict[str, Any]]) -> str:
    """Go json.Marshal([]Message) escaping, retaining persisted struct order."""
    encoded = json.dumps(messages, ensure_ascii=False, separators=(",", ":"))
    for value, escaped in (("<", "\\u003c"), (">", "\\u003e"), ("&", "\\u0026"), ("\u2028", "\\u2028"), ("\u2029", "\\u2029")):
        encoded = encoded.replace(value, escaped)
    return hashlib.sha256(encoded.encode()).hexdigest()


def history_snapshots(path: Path) -> list[list[dict[str, Any]]]:
    histories: list[list[dict[str, Any]]] = []
    history: list[dict[str, Any]] = []
    for line in path.read_text().splitlines():
        row = json.loads(line)
        if row.get("type") == "message":
            history = [*history, row["message"]]
        elif row.get("type") == "history_replace":
            history = row.get("messages", [])
        elif row.get("type") == "checkpoint":
            checkpoint = row["checkpoint"]
            if checkpoint.get("reset"):
                require(checkpoint.get("offset") == 0, "Subagent reset checkpoint has a nonzero offset")
                history = []
            require(checkpoint.get("offset") == len(history), "Subagent checkpoint offset differs from durable history")
            history = [*history, *checkpoint.get("messages", [])]
        else:
            continue
        histories.append(history)
    return histories


def message_text(message: dict[str, Any]) -> str:
    return "".join(block.get("text", "") for block in message.get("content", []) if block.get("type") == "text")


def final_assistant(history: list[dict[str, Any]]) -> str:
    for message in reversed(history):
        if message.get("role") == "assistant" and message_text(message).strip():
            return message_text(message).strip()
    return ""


def envelope_count(history: list[dict[str, Any]], identifier: str) -> int:
    # Counting only the injected durable envelope avoids treating an ordinary
    # tool's queue receipt or a model's acknowledgement as another delivery.
    return sum(message_text(message).count('<peer_message durable="true">')
               for message in history if message.get("role") == "user" and identifier in message_text(message))


def trace_origins(events: list[dict[str, Any]]) -> tuple[set[tuple[str, str]], dict[int, dict[str, Any]]]:
    """Separate child invocations from root-owned orchestration boundaries.

    An Agent/Fork container start and result use the child invocation ID, even
    though their tool_args belong to the root invocation. Child Read/Write
    results may omit subagent_of. Correlate the exact root args occurrence,
    container start and result; never treat an arbitrary nested result as root.
    """
    nested = {(row.get("session_id", ""), row.get("trace_invocation_id", "")) for row in events
              if row.get("subagent_of") or row.get("trace_parent_invocation_id")}
    nested.discard(("", ""))
    anchors: dict[tuple[str, str, str, str, str], dict[str, Any]] = {}
    starts: dict[tuple[str, str, str, str, str], tuple[str, int]] = {}
    boundaries: dict[int, dict[str, Any]] = {}
    for index, row in enumerate(events):
        name, kind = row.get("tool_name"), row.get("kind")
        if name not in ("Agent", "Fork"):
            continue
        sid, invocation = row.get("session_id", ""), row.get("trace_invocation_id", "")
        call, use = row.get("trace_call_id", ""), row.get("tool_use_id", "")
        if not all((sid, invocation, call, use)):
            continue
        if kind == "tool_args" and root_trace_identity(row) and (sid, invocation) not in nested:
            try:
                arguments = json.loads(row.get("text", ""))
            except ValueError:
                continue
            if not isinstance(arguments, dict):
                continue
            key = sid, invocation, call, use, name
            require(key not in anchors or anchors[key]["arguments"] == arguments, "One orchestration args occurrence changed its arguments")
            anchors[key] = {"session_id": sid, "root_invocation_id": invocation, "call_id": call, "tool_use_id": use,
                            "tool_name": name, "arguments": arguments, "anchor_event_id": row.get("id"), "anchor_index": index}
            continue
        parent = row.get("trace_parent_invocation_id", "")
        key = sid, parent, call, use, name
        anchor = anchors.get(key)
        if not anchor or row.get("subagent_of") or not parent or invocation == parent:
            continue
        if kind == "tool_start" and row.get("parent_id", "") in ("", use):
            try:
                arguments = json.loads(row.get("text", ""))
            except ValueError:
                continue
            require(arguments == anchor["arguments"], "Container start differs from its root-owned args occurrence")
            starts[key] = invocation, index
            boundaries[index] = {**anchor, "child_invocation_id": invocation, "start_event_id": row.get("id")}
        elif kind == "tool_result" and row.get("parent_id") == use and key in starts and starts[key][0] == invocation:
            start_index = starts[key][1]
            require(anchor["anchor_index"] < start_index < index, "Orchestration result precedes its owned start/args")
            boundaries[index] = {**anchor, "child_invocation_id": invocation, "start_event_id": events[start_index].get("id"),
                                 "result_event_id": row.get("id")}
    return nested, boundaries


def trace_proof(events: list[dict[str, Any]], provider: str, model: str) -> dict[str, Any]:
    usage, tools, invocations, sessions = [], Counter(), set(), set()
    errors = []
    nested, boundaries = trace_origins(events)
    orchestration = []
    for index, event in enumerate(events):
        boundary = boundaries.get(index)
        if boundary:
            identity = boundary["session_id"], boundary["root_invocation_id"]
        elif (event.get("session_id", ""), event.get("trace_invocation_id", "")) in nested:
            continue
        else:
            identity = root_trace_identity(event)
        if not identity:
            continue
        sessions.add(identity[0])
        invocations.add(identity[1])
        if event.get("kind") == "tokens":
            values = dict((name, int(value)) for name, value in re.findall(r"(input|output|cache_write|cache_read)=(\d+)", event.get("text", "")))
            require(set(values) == {"input", "output", "cache_write", "cache_read"}, "Incomplete absolute usage trace")
            usage.append({"session_id": identity[0], "invocation_id": identity[1], **values})
        if event.get("kind") == "tool_result":
            tools[event.get("tool_name", "")] += 1
            if boundary:
                orchestration.append(boundary)
        if event.get("kind") == "error" or event.get("is_error"):
            errors.append({"kind": event.get("kind"), "tool": event.get("tool_name"), "text": event.get("text", "")[:500]})
    return {"provider": provider, "model": model, "usage": usage, "model_responses": len(usage),
            "tool_results": dict(tools), "root_owned_orchestration_results": orchestration,
            "session_ids": sorted(sessions), "invocation_ids": sorted(invocations), "errors": errors}


def assert_skill_invoked(events: list[dict[str, Any]], name: str, marker: str) -> dict[str, Any]:
    calls: dict[tuple[str, str, str], dict[str, Any]] = {}
    matches = []
    for event in events:
        key = tuple(event.get(field, "") for field in ("session_id", "trace_invocation_id", "trace_call_id"))
        if not all(key) or event.get("tool_name") != "Skill":
            continue
        if event.get("kind") in ("tool_start", "tool_args"):
            try:
                args = json.loads(event.get("text", ""))
            except ValueError:
                continue
            if isinstance(args, dict):
                calls[key] = args
        if event.get("kind") == "tool_result" and not event.get("is_error"):
            args = calls.get(key, {})
            if args.get("action") == "invoke" and args.get("name") == name and marker in event.get("text", ""):
                matches.append({"session_id": key[0], "invocation_id": key[1], "call_id": key[2],
                                "arguments": args, "tool_result_sha256": hashlib.sha256(event["text"].encode()).hexdigest()})
    require(matches, "No successful, occurrence-matched Skill invoke with the selected active body")
    return {"matching_invokes": matches}


def tool_calls(events: list[dict[str, Any]], name: str) -> list[dict[str, Any]]:
    calls: dict[tuple[str, str, str], dict[str, Any]] = {}
    results = []
    _, boundaries = trace_origins(events)
    for index, event in enumerate(events):
        if name in ("Agent", "Fork"):
            boundary = boundaries.get(index)
            if event.get("tool_name") == name and event.get("kind") == "tool_result" and not event.get("is_error") and boundary:
                results.append({"session_id": boundary["session_id"], "invocation_id": boundary["root_invocation_id"],
                    "child_invocation_id": boundary["child_invocation_id"], "call_id": boundary["call_id"], "tool_use_id": boundary["tool_use_id"],
                    "arguments": boundary["arguments"], "result": event.get("text", ""), "anchor_event_id": boundary["anchor_event_id"],
                    "start_event_id": boundary["start_event_id"], "result_event_id": boundary["result_event_id"],
                    "result_sha256": hashlib.sha256(event.get("text", "").encode()).hexdigest()})
            continue
        key = tuple(event.get(field, "") for field in ("session_id", "trace_invocation_id", "trace_call_id"))
        if not all(key) or event.get("tool_name") != name:
            continue
        if event.get("kind") in ("tool_start", "tool_args"):
            try:
                arguments = json.loads(event.get("text", ""))
            except ValueError:
                continue
            if isinstance(arguments, dict):
                calls[key] = arguments
        elif event.get("kind") == "tool_result" and not event.get("is_error"):
            if key in calls:
                results.append({"session_id": key[0], "invocation_id": key[1], "call_id": key[2], "arguments": calls[key], "result": event.get("text", ""),
                                "result_sha256": hashlib.sha256(event.get("text", "").encode()).hexdigest()})
    return results


def nested_trace_proof(events: list[dict[str, Any]]) -> dict[str, Any]:
    usage, tools, errors = [], Counter(), []
    nested, boundaries = trace_origins(events)
    for index, row in enumerate(events):
        if index in boundaries or (row.get("session_id", ""), row.get("trace_invocation_id", "")) not in nested:
            continue
        if row.get("kind") == "tokens":
            values = dict((field, int(value)) for field, value in re.findall(r"(input|output|cache_write|cache_read)=(\d+)", row.get("text", "")))
            require(set(values) == {"input", "output", "cache_write", "cache_read"}, "Named child lacks complete actual usage")
            usage.append({"session_id": row.get("session_id"), "invocation_id": row.get("trace_invocation_id"),
                          "parent_invocation_id": row.get("trace_parent_invocation_id"), **values})
        if row.get("kind") == "tool_result":
            tools[row.get("tool_name", "")] += 1
        if row.get("kind") == "error" or row.get("is_error"):
            errors.append({"kind": row.get("kind"), "tool": row.get("tool_name"), "text": row.get("text", "")[:500]})
    return {"actual_child_usage": usage, "child_model_responses": len(usage), "child_tool_results": dict(tools), "child_errors": errors}


def workflow_result(value: Any, name: str, version: str, expected: Any) -> dict[str, Any]:
    if isinstance(value, str):
        value = json.loads(value)
    require(isinstance(value, dict) and value.get("name") == name and value.get("recipe_sha256") == version,
            "Workflow result does not name the exact active recipe version")
    steps = value.get("steps", [])
    require(steps and all(step.get("status") == "ok" and step.get("exit_code") == 0 for step in steps), "Workflow's actual subprocess steps did not all succeed")
    output = "".join(step.get("output", "") for step in steps).strip()
    require(json.loads(output) == expected, "Actual Workflow subprocess output failed the host assertion")
    return {"recipe_sha256": version, "actual_step_output_matched": True, "steps": steps}


def select_child_transcript(directory: Path, parent: str, name: str, model: str, previous: set[Path], expected_id: str = "") -> tuple[Path, dict[str, Any]]:
    """Identify the produced child from persisted host metadata, never text."""
    matches = []
    for path in directory.glob("*.jsonl"):
        require(path.is_file() and not path.is_symlink(), "Named-child transcript is not a regular owned file")
        rows = [json.loads(line) for line in path.read_text().splitlines()]
        if not rows or rows[0].get("type") != "header":
            continue
        header = rows[0].get("header", {})
        bindings = [row["handoff"] for row in rows if row.get("type") == "handoff"]
        if header.get("sub_agent_of") != parent or header.get("teammate_name") != name or header.get("model") != model:
            continue
        require(header.get("id") == path.stem and bindings == [{"scope": "session:" + parent, "recipient": name}],
                "Child identity or mailbox binding does not match its own transcript path")
        matches.append((path, header))
    require(len(matches) == 1, "Parent query did not produce one uniquely bound named-child transcript")
    path, header = matches[0]
    if expected_id:
        require(header["id"] == expected_id and path in previous, "Resume created a different child identity/transcript")
    else:
        require(path not in previous, "Initial child identity was taken from a pre-existing transcript")
    return path, header


class PersistentAgentLiveCheck:
    def __init__(self, args: argparse.Namespace) -> None:
        self.args = args
        self.repo, self.output = args.repo.resolve(), args.output.resolve()
        require(not self.output.exists() or not any(self.output.iterdir()), "Use a new/empty evidence directory")
        self.output.mkdir(parents=True, exist_ok=True, mode=0o700)
        self.root = Path(tempfile.mkdtemp(prefix="metis-persistent-agent-live-")).resolve()
        self.root.chmod(0o700)
        self.binary, self.home, self.workspace = self.root / "metis", self.root / "metis-home", self.root / "workspace"
        self.user_home = self.root / "user-home"
        for path in (self.home, self.workspace, self.user_home):
            path.mkdir(mode=0o700)
        self.source_binary = args.binary.expanduser().resolve()
        self.processes: list[subprocess.Popen] = []
        self.handles: dict[int, tuple[Any, Any]] = {}
        self.secrets: list[str] = []
        self.key = ""
        self.index, self.started = 0, time.monotonic()
        self.jobs: dict[str, dict[str, Any]] = {}
        self.report: dict[str, Any] = {"passed": False, "started_utc": utc_now(), "checks": [], "commands": [],
            "mock_model_completions": False, "temporary_root": str(self.root),
            "boundary": "Real CLI queries on the original selected route, finite job tasks and a fixed host-checked skill suite. This does not establish OS boot autostart, external-platform delivery, supplier invoice reconciliation, indefinite soak or general autonomous learning quality."}

    def redact(self, text: str) -> str:
        for value in self.secrets:
            if value:
                text = text.replace(value, "[REDACTED]")
        return text

    def progress(self, phase: str, **fields: Any) -> None:
        atomic_json(self.output / "progress.json", {"phase": phase, "utc": utc_now(),
            "elapsed_seconds": round(time.monotonic() - self.started, 3), **fields})

    def record(self, name: str, **fields: Any) -> None:
        value = json.loads(self.redact(json.dumps({"name": name, "passed": True, "utc": utc_now(), **fields})))
        self.report["checks"].append(value)
        with (self.output / "progress.jsonl").open("a") as target:
            target.write(json.dumps(value, ensure_ascii=False) + "\n")
        print(json.dumps({"check": name, "passed": True}), flush=True)
        self.progress(name)

    def stage(self) -> None:
        require(self.source_binary.is_file() and self.source_binary != self.binary, "Supply a fresh disposable parent-built CLI")
        self.input_sha = digest(self.source_binary)
        require(self.input_sha == self.args.expected_sha256, "Fresh supplied CLI digest changed")
        snapshot = json.loads(self.args.source_freeze.read_text())
        require(snapshot.get("files"), "Source freeze has no digests")
        changed = [name for name, sha in snapshot["files"].items() if not (self.repo / name).is_file() or digest(self.repo / name) != sha]
        require(not changed, "Source changed before the live run: " + ", ".join(changed[:8]))
        self.freeze_sha = digest(self.args.source_freeze)
        self.report["source_freeze"] = {"path": str(self.args.source_freeze.resolve()), "sha256": self.freeze_sha, "files_checked": len(snapshot["files"])}
        shutil.copyfile(self.source_binary, self.binary)
        self.binary.chmod(0o700)
        require(digest(self.binary) == self.input_sha, "Private CLI differs from the supplied fresh build")
        self.report["binary"] = {"input": str(self.source_binary), "sha256": self.input_sha, "recompiled_by_harness": False, "destroy_input_requested": self.args.destroy_input}

    def load_route(self) -> None:
        # This function runs only in explicitly invoked live mode. --self-test
        # and --help neither select nor read the real authentication store.
        original_home = Path(os.environ.get("METIS_HOME", str(Path.home() / ".metis"))).expanduser().resolve()
        self.original_config = original_home / "config.toml"
        self.original_sha = digest(self.original_config)
        config = tomllib.loads(self.original_config.read_text())
        self.provider = config.get("provider", {}).get("default", "")
        require(self.provider and self.provider in config["provider"].get("custom", {}), "Live check requires the original selected custom API-key provider; no fallback is allowed")
        self.settings = dict(config["provider"]["custom"][self.provider])
        self.model = self.settings.get("model", "")
        require(self.model, "Original selected model is missing")
        endpoint = urlsplit(self.settings.get("base_url", ""))
        require(endpoint.scheme in ("http", "https") and endpoint.hostname and not endpoint.username and not endpoint.query and not endpoint.fragment,
                "Selected endpoint must be a fixed plain HTTP(S) route")
        key_env = self.settings.get("api_key_env", "")
        inline_key = self.settings.pop("api_key", "")
        self.key = (os.environ.get(key_env, "") if key_env else "") or inline_key
        if not self.key:
            auth = json.loads((original_home / ".credentials" / "auth.json").read_text())
            selected = auth.get(self.provider, {})
            require(selected.get("type") == "api", "Selected authentication is not a reusable API key")
            self.key = selected.get("key", "")
            binding = selected.get("endpoint", {})
            if binding:
                require(binding.get("provider") == self.provider and binding.get("base_url", "").rstrip("/") == self.settings["base_url"].rstrip("/") and
                        binding.get("transport") == self.settings.get("transport", "anthropic_messages"), "Stored credential belongs to another route")
        require(self.key and "\n" not in self.key and "\r" not in self.key, "Original selected route has no usable existing API key")
        self.secrets.append(self.key)
        (self.home / ".credentials").mkdir(mode=0o700)
        atomic_json(self.home / ".credentials" / "auth.json", {self.provider: {"type": "api", "key": self.key,
            "endpoint": {"provider": self.provider, "transport": self.settings.get("transport", "anthropic_messages"), "base_url": self.settings["base_url"]}}})
        self.settings.pop("api_key_env", None)
        self.settings["max_tokens"] = 2048
        text = toml_table(["provider"], {"default": self.provider}) + "\n" + toml_table(["provider", "custom", self.provider], self.settings)
        text += "\n" + toml_table(["session"], {"dir": str(self.home / "sessions"), "skill_dir": str(self.home / "skills"), "max_iterations": 12, "auto_compact_threshold": 0.85})
        text += '\n[tools]\nallowed = ["Read", "Write", "MessageTeammate", "Skill", "Workflow", "Agent"]\n\n[permission]\nmode = "dontAsk"\n'
        for tool in ("Read", "Write", "MessageTeammate", "Skill", "Workflow", "Agent", "Bash"):
            text += '\n[[permission.allow]]\ntool = "' + tool + '"\n'
        (self.home / "config.toml").write_text(text)
        (self.home / "config.toml").chmod(0o600)
        (self.home / "agents").mkdir(mode=0o700)
        (self.home / "skills").mkdir(mode=0o700)
        # A public model catalog can be seeded without copying unrelated user
        # state or a provider metadata cache. Discovery still uses this route.
        catalog = original_home / "cache" / "models.json"
        if catalog.is_file() and not catalog.is_symlink():
            (self.home / "cache").mkdir(mode=0o700)
            shutil.copyfile(catalog, self.home / "cache" / "models.json")
        self.env = {name: os.environ[name] for name in ("PATH", "TMPDIR", "SHELL", "LANG", "LC_ALL") if name in os.environ}
        self.env.update({"HOME": str(self.user_home), "METIS_HOME": str(self.home), "METIS_NO_UPDATE_CHECK": "1", "METIS_AUTO_MEMORY": "0",
            "METIS_RUN_CACHE": "0", "METIS_RUN_MAX_SECONDS": str(int(self.args.query_timeout)), "NO_PROXY": "127.0.0.1,localhost"})
        self.report["original_route"] = {"provider": self.provider, "model": self.model, "transport": self.settings.get("transport", "anthropic_messages"),
            "endpoint": safe_endpoint(self.settings["base_url"]), "real_config_sha256": self.original_sha, "provider_model_or_endpoint_changed": False,
            "private_parameter_overrides": {"max_tokens": 2048, "max_iterations": 12, "auto_compact_threshold": 0.85}}
        self.record("original selected route isolated with a private credential and fixed model", route=self.report["original_route"])

    def spawn(self, args: list[str], label: str) -> tuple[subprocess.Popen, Path, Path]:
        self.index += 1
        stem = self.output / (f"{self.index:03d}-" + label)
        out, err = stem.with_suffix(".stdout.log"), stem.with_suffix(".stderr.log")
        handles = out.open("w"), err.open("w")
        try:
            process = subprocess.Popen([str(self.binary), *args], cwd=self.workspace, env=self.env, stdin=subprocess.DEVNULL,
                                       stdout=handles[0], stderr=handles[1], start_new_session=True)
        except BaseException:
            for handle in handles:
                handle.close()
            raise
        self.processes.append(process)
        self.handles[process.pid] = handles
        self.report["commands"].append({"label": label, "pid": process.pid, "arguments": args, "stdout": out.name, "stderr": err.name})
        self.progress(label, owned_pid=process.pid)
        return process, out, err

    def finish(self, child: tuple[subprocess.Popen, Path, Path], label: str, *, expect_success: bool = True,
               json_output: bool = True, rejection_contains: str | None = None, operation_timeout: float | None = None) -> tuple[Any, dict[str, Any]]:
        process, out, err = child
        try:
            process.wait(timeout=operation_timeout or self.args.query_timeout)
        except subprocess.TimeoutExpired:
            self.stop(process)
            raise AssertionError(label + " exceeded its bounded deadline")
        for handle in self.handles.pop(process.pid, ()):
            handle.close()
        stdout, stderr = self.redact(out.read_text(errors="replace")), self.redact(err.read_text(errors="replace"))
        out.write_text(stdout)
        err.write_text(stderr)
        proof = {"pid": process.pid, "exit_code": process.returncode, "stdout_sha256": digest(out), "stderr_sha256": digest(err)}
        self.report["commands"][-1].update(proof)
        require((process.returncode == 0) == expect_success, label + " unexpected exit status " + str(process.returncode) + ": " + stderr[-1200:])
        if rejection_contains:
            require(not expect_success and rejection_contains.lower() in stderr.lower(), label + " failed for a different reason: " + stderr[-1200:])
        value = json.loads(stdout) if json_output else stdout
        return value, proof

    def command(self, args: list[str], label: str, **options: Any) -> tuple[Any, dict[str, Any]]:
        return self.finish(self.spawn(args, label), label, **options)

    def trace_snapshot(self) -> dict[str, int]:
        return {str(path): path.stat().st_size for path in self.home.rglob("*.jsonl") if path.parent.name == "traces"}

    def trace_events(self, before: dict[str, int]) -> list[dict[str, Any]]:
        rows = []
        for path in self.home.rglob("*.jsonl"):
            if path.parent.name != "traces":
                continue
            with path.open("rb") as source:
                source.seek(before.get(str(path), 0))
                for line in source.read().splitlines():
                    if line:
                        rows.append(json.loads(line))
        return rows

    def real_proof(self, before: dict[str, int], label: str, *, required_tools: tuple[str, ...] = ()) -> tuple[dict[str, Any], list[dict[str, Any]]]:
        rows = self.trace_events(before)
        proof = trace_proof(rows, self.provider, self.model)
        require(proof["model_responses"] > 0 and any(sum(row.get(field, 0) for field in ("input", "output", "cache_write", "cache_read")) > 0 for row in proof["usage"]), label + " has no real provider usage")
        require(not proof["errors"], label + " has unexpected provider/tool errors: " + json.dumps(proof["errors"]))
        for tool in required_tools:
            require(proof["tool_results"].get(tool, 0) > 0, label + " did not actually execute " + tool)
        headers = []
        for sid in proof["session_ids"]:
            paths = [path for path in self.home.rglob(sid + ".jsonl") if path.parent.name == "sessions"]
            require(len(paths) == 1, label + " session identity is absent or ambiguous on disk")
            records = [json.loads(line) for line in paths[0].read_text().splitlines()]
            latest = [row["header"] for row in records if row.get("type") == "header"][-1]
            require(latest.get("provider") == self.provider and latest.get("model") == self.model,
                    label + " session used a different provider/model route")
            headers.append({"session_id": sid, "provider": latest["provider"], "model": latest["model"], "transcript_sha256": digest(paths[0])})
        proof["session_headers"] = headers
        atomic_json(self.output / (label + "-trace-proof.json"), proof)
        return proof, rows

    def job(self, verb: str, *args: str, label: str | None = None, **options: Any) -> tuple[Any, dict[str, Any]]:
        return self.command(["job", verb, *args, "--json"], label or "job-" + verb, **options)

    def status(self, identifier: str, label: str) -> dict[str, Any]:
        value, _ = self.job("status", identifier, label=label)
        require(value.get("id") == identifier, "Reloaded job lost its stable identity")
        return value

    def create_job(self, name: str, profile: str, *, cap: int = 0) -> dict[str, Any]:
        args = ["--name", name, "--cwd", str(self.workspace), "--agent", profile, "--provider", self.provider, "--model", self.model]
        if cap:
            args += ["--max-input-tokens", str(cap)]
        value, _ = self.job("create", *args, label="create-" + name)
        require(value.get("id") and value.get("status") == "active" and re.fullmatch(r"[a-f0-9]{64}", value.get("config_sha256", "")), "Job definition lacks stable identity/frozen config proof")
        self.jobs[name] = value
        return value

    def run_job(self, identifier: str, prompt: str, label: str, *, required_tools: tuple[str, ...] = (), verb: str = "run") -> tuple[dict[str, Any], dict[str, Any], list[dict[str, Any]]]:
        before = self.trace_snapshot()
        result, command = self.job(verb, identifier, "--prompt", prompt, "--subject", label, label=label)
        run = result.get("run", {})
        require(run.get("id") and run.get("status") == "completed", label + " coordinator task did not complete")
        proof, rows = self.real_proof(before, label, required_tools=required_tools)
        return result, {**command, **proof}, rows

    def session_path(self, state: dict[str, Any]) -> Path:
        identifier = state.get("session_id", "")
        require(identifier, "Persistent job lacks a stable session ID")
        path = Path(state.get("session_dir") or self.home / "sessions") / (identifier + ".jsonl")
        require(path.is_file() and self.root in path.resolve().parents, "Job transcript must exist inside the private root")
        return path

    def assert_artifact(self, name: str, expected: Any) -> dict[str, Any]:
        path = self.workspace / name
        require(path.is_file() and not path.is_symlink() and json.loads(path.read_text()) == expected, "Host artifact contract failed for " + name)
        return {"path": name, "sha256": digest(path), "host_value_matched": True}

    def persistent_jobs(self) -> None:
        self.contract = "JOB_FROZEN_" + secrets.token_hex(8)
        profile = self.home / "agents" / "live-contract.md"
        profile.write_text('---\nname: live-contract\ntools: [Read, Write, MessageTeammate]\npermission_mode: dontAsk\nmax_turns: 10\n---\n'
            'Follow the current task only. Never inspect unrelated directories or delegate. For JOB_CONTRACT_WRITE tasks, fully Read the requested existing JSON file, overwrite it with {"contract":"' + self.contract + '"}, then Read it back with limit=500, then return DONE. For other tasks follow their explicit instruction.\n')
        (self.home / "agents" / "live-budget.md").write_text('---\nname: live-budget\ntools: [Read]\npermission_mode: dontAsk\nmax_turns: 3\n---\nReturn only the requested marker. Do not use tools or delegate.\n')
        first = self.create_job("producer", "live-contract")
        second = self.create_job("recipient", "live-contract")
        require(first["id"] != second["id"] and first.get("session_id") != second.get("session_id"), "Different jobs share their principal/session identity")
        for name in ("frozen-first.json", "frozen-second.json", "recipient-result.json"):
            atomic_json(self.workspace / name, {})
        result1, proof1, _ = self.run_job(first["id"], "JOB_CONTRACT_WRITE: fully Read the existing " + str(self.workspace / "frozen-first.json") + ", write the frozen profile contract there, read it back and stop.", "job-frozen-first", required_tools=("Write", "Read"))
        artifact1 = self.assert_artifact("frozen-first.json", {"contract": self.contract})
        after_first = self.status(first["id"], "producer-status-after-first")
        untouched = self.status(second["id"], "recipient-unused-status")
        require(after_first.get("input_tokens", 0) > 0 and untouched.get("input_tokens", 0) == 0, "Accumulated input budget leaked across job identities")
        frozen_sha = after_first["config_sha256"]
        old_run = result1["run"]
        require(old_run.get("operations"), "Real Write had no durable operation journal")
        journal_sha = json_sha(old_run["operations"])
        profile.write_text('---\nname: live-contract\ntools: [Read]\npermission_mode: plan\n---\nReturn MUTATED_PROFILE and never write any file.\n')
        result2, proof2, _ = self.run_job(first["id"], "JOB_CONTRACT_WRITE: fully Read the existing " + str(self.workspace / "frozen-second.json") + ", write the frozen profile contract there, read it back and stop.", "job-frozen-after-source-mutation", required_tools=("Write", "Read"), verb="rotate")
        artifact2 = self.assert_artifact("frozen-second.json", {"contract": self.contract})
        after_second = self.status(first["id"], "producer-status-after-second")
        require(after_second["config_sha256"] == frozen_sha and after_second["session_id"] == after_first["session_id"], "Restart/source mutation changed frozen job configuration or session identity")
        require(result2["run"]["id"] != old_run["id"] and len(after_second.get("runs", [])) >= 2, "Successive job tasks did not rotate independent runs")
        loaded_old, _ = self.command(["coordinator", "status", old_run["id"], "--cwd", str(self.workspace), "--json"], "old-run-journal-after-rotation")
        require(json_sha(loaded_old.get("operations", [])) == journal_sha, "Run rotation lost or changed the old completed operation journal")
        require(after_second.get("input_tokens", 0) > after_first.get("input_tokens", 0), "Job cumulative input usage did not persist across CLI processes")
        self.record("stable job principal uses frozen profile across independent CLI tasks and retains previous run journal", job_id=first["id"],
            session_id=after_second["session_id"], config_sha256=frozen_sha, run_ids=[old_run["id"], result2["run"]["id"]],
            old_operations_sha256=journal_sha, artifacts=[artifact1, artifact2], real_queries=[proof1, proof2],
            restart_between_completed_assignments=True, active_provider_request_SIGKILL_tested=False)
        self.job("pause", first["id"], label="pause-producer")
        before = self.trace_snapshot()
        self.job("run", first["id"], "--prompt", "Return PAUSE_SHOULD_NOT_RUN", label="paused-job-dispatch-rejected", expect_success=False, json_output=False,
                 rejection_contains="paused or retired")
        require(not trace_proof(self.trace_events(before), self.provider, self.model)["model_responses"], "Paused job made a provider request")
        self.job("resume", first["id"], label="resume-producer")
        require(self.status(first["id"], "producer-resumed-status")["status"] == "active", "Paused job did not resume")
        capped = self.create_job("budget-isolation", "live-budget", cap=1)
        marker = "BUDGET_REAL_" + secrets.token_hex(8)
        result, proof, _ = self.run_job(capped["id"], "Return exactly " + marker + ". Do not use tools.", "capped-job-first-query")
        capped_state = self.status(capped["id"], "capped-job-status-after-spend")
        require(final_assistant(history_snapshots(self.session_path(capped_state))[-1]) == marker, "Budget probe did not return the actual requested marker")
        require(capped_state.get("input_tokens", 0) >= 1, "Job did not persist its actual token spend")
        before = self.trace_snapshot()
        self.job("run", capped["id"], "--prompt", "Return BUDGET_SHOULD_NOT_RUN", label="cumulative-budget-next-dispatch-rejected", expect_success=False, json_output=False,
                 rejection_contains="budget")
        require(not trace_proof(self.trace_events(before), self.provider, self.model)["model_responses"], "Exhausted persistent job dispatched another model request")
        require(self.status(second["id"], "recipient-after-other-job-budget-status").get("input_tokens", 0) == 0, "Exhausted job changed another job's budget")
        self.record("paused and budget-exhausted jobs reject dispatch while independent job budgets remain isolated", paused_job=first["id"],
            capped_job=capped["id"], capped_usage=capped_state.get("input_tokens"), independent_job=second["id"], first_real_capped_query=proof,
            rejected_provider_responses=0)
        self.active_worker_crash_recovery()

    def active_worker_crash_recovery(self) -> None:
        # Read intentionally refuses FIFOs and other nonregular files. A large
        # real regular file gives a bounded pure-read interruption window; the
        # model sees only line one. No provider result or usage receipt is faked.
        require(shutil.which("lsof"), "Active Read fault proof requires the host lsof descriptor inspector")
        marker = "REAL_READ_GATE_" + secrets.token_hex(12)
        gate = self.workspace / "active-read-gate.txt"
        chunk = (b"benign local regular-file read padding\n" * 28000)
        with gate.open("wb") as target:
            target.write((marker + "\n").encode())
            while target.tell() < 128 * (1 << 20):
                target.write(chunk[:min(len(chunk), 128 * (1 << 20) - target.tell())])
        original_gate_size = gate.stat().st_size
        artifact_path = self.workspace / "crash-recovery-result.json"
        atomic_json(artifact_path, {})
        (self.home / "agents" / "live-crash.md").write_text('---\nname: live-crash\ntools: [Read, Write]\npermission_mode: dontAsk\nmax_turns: 10\n---\n'
            'First execute Read on the gate path requested by the user with limit=1, before any other tool. Then fully Read the existing output JSON, overwrite it with {"gate":the exact first line from the gate}, and Read the result back. Return DONE. Never read the rest of the gate or use any other tool.\n')
        job = self.create_job("active-crash-recovery", "live-crash")
        prompt = "Read the real gate " + str(gate) + " with limit=1 first. Then overwrite the existing " + str(artifact_path) + " with the prescribed gate JSON, read it back and return DONE."
        before = self.trace_snapshot()
        child = self.spawn(["job", "run", job["id"], "--prompt", prompt, "--subject", "active-read-crash", "--lease", "2s", "--json"], "real-active-job-before-SIGKILL")
        process, stdout, stderr = child
        run_path: Path | None = None
        frozen: dict[str, Any] | None = None
        descriptor_proof: dict[str, Any] | None = None
        deadline = time.monotonic() + self.args.query_timeout
        while time.monotonic() < deadline:
            require(process.poll() is None, "Crash fixture worker exited before its pure-read interruption boundary")
            if run_path is None:
                match = re.search(r"executing run ([a-zA-Z0-9_-]+) ", stderr.read_text(errors="replace"))
                if match:
                    paths = list((self.home / "project-coordinator").rglob(match.group(1) + ".json"))
                    if len(paths) == 1:
                        run_path = paths[0]
            if run_path is not None:
                try:
                    candidate = json.loads(run_path.read_text())
                except (OSError, ValueError):
                    candidate = {}
                requests = candidate.get("usage_requests", [])
                terminal = requests and all(row.get("status") == "completed" and row.get("usage_reported") for row in requests)
                if terminal:
                    os.killpg(process.pid, signal.SIGSTOP)
                    try:
                        # While the worker is stopped, its pinned regular Read
                        # descriptor cannot close and no second POST can begin.
                        listing = subprocess.run([shutil.which("lsof"), "-a", "-p", str(process.pid), "-Fn"], capture_output=True, text=True, timeout=5)
                        names = [line[1:] for line in listing.stdout.splitlines() if line.startswith("n")]
                        if str(gate) in names:
                            stopped = json.loads(run_path.read_text())
                            current = stopped.get("usage_requests", [])
                            require(current and all(row.get("status") == "completed" and row.get("usage_reported") for row in current),
                                    "SIGSTOP boundary still has unknown/in-flight physical provider usage")
                            require(not stopped.get("operations"), "Pure-read crash boundary already has an unresolved writing operation")
                            require(json.loads(artifact_path.read_text()) == {}, "Crash boundary occurred after the business write")
                            frozen = stopped
                            descriptor_proof = {"pid": process.pid, "gate_path": str(gate), "gate_bytes": original_gate_size,
                                "regular_file_descriptor_open": True, "lsof_stdout_sha256": hashlib.sha256(listing.stdout.encode()).hexdigest(),
                                "all_provider_requests_completed_before_kill": True, "physical_request_count": len(current)}
                            break
                    finally:
                        if frozen is None and process.poll() is None:
                            os.killpg(process.pid, signal.SIGCONT)
            time.sleep(0.002)
        require(frozen is not None and descriptor_proof is not None, "No independently observed active regular Read boundary was captured")
        os.killpg(process.pid, signal.SIGKILL)
        process.wait(timeout=10)
        for handle in self.handles.pop(process.pid, ()):
            handle.close()
        require(process.returncode == -signal.SIGKILL, "Active job was not actually killed with SIGKILL")
        require(json.loads(artifact_path.read_text()) == {}, "Strong-killed worker still completed the business write")
        early_events = self.trace_events(before)
        early_usage = trace_proof(early_events, self.provider, self.model)
        require(early_usage["model_responses"] > 0, "Killed worker has no durable actual model token event")
        old_physical = {row["client_request_id"]: row for row in frozen["usage_requests"]}
        for record in old_physical.values():
            require(record.get("provider") == self.provider and record.get("model") == self.model and re.fullmatch(r"[a-f0-9]{64}", record.get("request_sha256", "")) and
                    sum(record.get("usage", {}).values()) > 0, "Pre-kill physical receipt lacks actual selected-route usage")
        atomic_json(self.output / "active-worker-before-kill-physical-ledger.json", frozen)
        atomic_json(self.output / "active-worker-Read-descriptor-proof.json", descriptor_proof)
        gate.write_text(marker + "\n")
        # The original, now-dead worker renewed both short leases. Wait past
        # their final saved deadlines; recovery keeps the original run ID.
        expires = []
        for item in frozen.get("items", []):
            if item.get("lease", {}).get("expires_at"):
                expires.append(item["lease"]["expires_at"])
        from datetime import datetime
        for expiry in expires:
            stamp = datetime.fromisoformat(expiry.replace("Z", "+00:00")).timestamp()
            while time.time() <= stamp + 0.1:
                time.sleep(0.02)
        time.sleep(2.2)
        before_resume = self.trace_snapshot()
        result, command = self.job("run", job["id"], "--run-id", frozen["id"], "--lease", "2s", label="real-active-job-SIGKILL-same-run-recovery")
        recovered = result.get("run", {})
        require(recovered.get("id") == frozen["id"] and recovered.get("status") == "completed", "Strong-killed job did not recover its original assignment")
        proof, _ = self.real_proof(before_resume, "real-active-job-SIGKILL-same-run-recovery", required_tools=("Read", "Write"))
        actual = self.assert_artifact(artifact_path.name, {"gate": marker})
        state = self.status(job["id"], "active-crash-recovered-job-status")
        require(state["session_id"] == job["session_id"] and state["config_sha256"] == job["config_sha256"] and len(state.get("runs", [])) == 1 and not state.get("unresolved_requests"),
                "Crash recovery changed job identity/session or left unknown usage")
        final_records = {row["client_request_id"]: row for row in recovered.get("usage_requests", [])}
        require(all(final_records.get(identifier) == record for identifier, record in old_physical.items()), "Recovery lost or modified settled pre-kill physical usage")
        require(recovered.get("items", [{}])[0].get("attempts") == 2 and recovered.get("operations"), "Recovery did not replace the worker claim and retain its final operation journal")
        self.record("actual active worker SIGKILL resumes same persistent job and run after settled provider usage", job_id=job["id"], run_id=frozen["id"],
            session_id=job["session_id"], killed_pid=process.pid, kill_exit_code=process.returncode, resumed_pid=command["pid"],
            descriptor_proof=descriptor_proof, before_kill_actual_usage=early_usage, before_kill_physical_usage=list(old_physical.values()),
            after_recovery_usage=proof, artificial_usage_receipts=False, artifact=actual)

    def durable_handoffs(self) -> None:
        sender, recipient = self.jobs["producer"], self.jobs["recipient"]
        body = "HANDOFF_BODY_" + secrets.token_hex(16)
        key, task_id = "handoff-key-" + secrets.token_hex(8), "task-" + secrets.token_hex(8)
        task = {"id": task_id, "objective": "Persist the received body and task ID into recipient-result.json and read it back.",
                "acceptance": ["recipient-result.json has exactly message_body and task_id from this envelope"]}
        prompt = "Use MessageTeammate exactly once to enqueue the following durable handoff to the offline recipient. Do not write files or delegate. Arguments: " + json.dumps({"to": "job:" + recipient["id"], "body": body, "idempotency_key": key, "task": task}) + ". After the tool succeeds, return QUEUED."
        _, sender_proof, rows = self.run_job(sender["id"], prompt, "real-sender-offline-job-handoff", required_tools=("MessageTeammate",))
        listing, _ = self.command(["handoff", "list", "--scope", "jobs", "--to", "job:" + recipient["id"]], "offline-recipient-mailbox-after-sender-exit")
        records = listing if isinstance(listing, list) else listing.get("messages", listing.get("records", []))
        matches = [row for row in records if row.get("envelope", row).get("body") == body]
        require(len(matches) == 1, "Sender did not queue exactly one durable message for the offline job")
        queued = matches[0]
        envelope = queued.get("envelope", queued)
        identifier = envelope["id"]
        require(queued.get("state") == "pending" and envelope.get("task") == task, "Offline handoff lost its pending state or structured task contract")
        # Repeat through the same real persistent sender. The stable sender
        # scope forms part of key ownership; a CLI operator impersonating a
        # textual --from value would not establish this idempotency property.
        _, duplicate_proof, duplicate_rows = self.run_job(sender["id"], prompt, "real-sender-same-key-idempotent-replay", required_tools=("MessageTeammate",))
        duplicate_results = []
        for row in duplicate_rows:
            if row.get("kind") == "tool_result" and row.get("tool_name") == "MessageTeammate" and not row.get("is_error"):
                value = json.loads(row.get("text", ""))
                if value.get("id") == identifier:
                    duplicate_results.append(value)
        require(len(duplicate_results) == 1 and duplicate_results[0].get("duplicate") is True,
                "Same sender handoff idempotency key did not replay one existing delivery")
        replay_listing, _ = self.command(["handoff", "list", "--scope", "jobs", "--to", "job:" + recipient["id"]], "offline-recipient-mailbox-after-duplicate")
        replay_records = replay_listing if isinstance(replay_listing, list) else replay_listing.get("messages", replay_listing.get("records", []))
        require(sum(row.get("envelope", row).get("body") == body for row in replay_records) == 1,
                "Duplicate sender request added another mailbox record")
        before_recipient = self.status(recipient["id"], "recipient-before-handoff-consumption")
        _, recipient_proof, _ = self.run_job(recipient["id"], "Process the available durable handoff. Fully Read the existing " + str(self.workspace / "recipient-result.json") + ". Use the received body and task ID to overwrite that file with exactly {\"message_body\":received_body,\"task_id\":received_task_id}. Read it back with limit=500 and return DONE. These values are available only in the handoff, not in this request.",
            "recipient-auto-inject-and-checkpoint", required_tools=("Write", "Read"))
        artifact = self.assert_artifact("recipient-result.json", {"message_body": body, "task_id": task_id})
        receipt, _ = self.command(["handoff", "status", identifier], "acknowledged-handoff-receipt")
        require(receipt.get("state") == "acknowledged" and receipt.get("acknowledged_at") and re.fullmatch(r"[a-f0-9]{64}", receipt.get("checkpoint_sha256", "")), "Handoff was not acknowledged after a durable transcript checkpoint")
        recipient_state = self.status(recipient["id"], "recipient-after-handoff-status")
        path = self.session_path(recipient_state)
        snapshots = history_snapshots(path)
        require(snapshots and envelope_count(snapshots[-1], identifier) == 1, "Receiver transcript lacks exactly one durable envelope")
        matched = [index for index, history in enumerate(snapshots) if go_history_sha(history) == receipt["checkpoint_sha256"] and envelope_count(history, identifier) == 1]
        require(matched, "Receipt checkpoint SHA does not match a persisted receiver transcript snapshot")
        before_second_usage = recipient_state.get("input_tokens", 0)
        _, restart_proof, _ = self.run_job(recipient["id"], "Return exactly HANDOFF_RESTART_OK. Do not write files or use tools; the previous handed-off task is already complete.", "receiver-process-restart-no-redelivery")
        final_state = self.status(recipient["id"], "recipient-restarted-status")
        require(final_state["session_id"] == recipient_state["session_id"] and final_state.get("input_tokens", 0) > before_second_usage, "Receiver did not resume the same persistent session/budget")
        require(envelope_count(history_snapshots(self.session_path(final_state))[-1], identifier) == 1, "Acknowledged handoff was injected again after receiver restart")
        final_receipt, _ = self.command(["handoff", "status", identifier], "ack-receipt-after-recipient-restart")
        require(final_receipt.get("attempts") == receipt.get("attempts") and final_receipt["checkpoint_sha256"] == receipt["checkpoint_sha256"], "Restart re-claimed or re-acknowledged the finished handoff")
        self.record("real sender queues offline job and restarted recipient checkpoints once before acknowledgement", message_id=identifier,
            structured_task_sha256=json_sha(task), idempotency_key_sha256=hashlib.sha256(key.encode()).hexdigest(), receiver_session=recipient_state["session_id"],
            checkpoint_sha256=receipt["checkpoint_sha256"], matching_persisted_snapshot_indexes=matched, attempts=receipt.get("attempts"),
            artifact=artifact, real_queries=[sender_proof, duplicate_proof, recipient_proof, restart_proof])
        self.handoff_lease_fencing()
        self.named_child_handoffs()

    def named_child_handoffs(self) -> None:
        name = "live-reliable-child"
        sid = str(uuid.uuid4())
        profile_name = "live-named-receiver"
        (self.home / "agents" / (profile_name + ".md")).write_text('---\nname: ' + profile_name + '\ntools: [Read, Write]\npermission_mode: dontAsk\nmax_turns: 8\n---\n'
            'Process the current durable handoff only. Obtain message body and task id from the delivered peer envelope. Fully Read the existing requested JSON output, Write exactly {"message_body":received_body,"task_id":received_task_id}, then Read back with limit=500. Return DONE. Never inspect other directories, delegate or ask the parent to repeat the message body.\n')
        receipts: list[dict[str, Any]] = []
        real_queries, artifacts = [], []
        agent_id = ""
        child_path: Path | None = None
        initial_transcript_prefix = b""
        for attempt in (1, 2):
            body, task_id, key = "NAMED_CHILD_BODY_" + secrets.token_hex(16), "named-task-" + secrets.token_hex(8), "named-key-" + secrets.token_hex(8)
            artifact_name = "named-child-" + str(attempt) + ".json"
            atomic_json(self.workspace / artifact_name, {})
            task = {"id": task_id, "objective": "Persist the delivered body and task ID into " + artifact_name,
                    "acceptance": [artifact_name + " exactly preserves the body and ID from this envelope"]}
            agent_args: dict[str, Any] = {"name": name, "subagent_type": profile_name, "run_in_background": False,
                "allowed_tools": ["Read", "Write"], "isolation": "none", "cwd": str(self.workspace),
                "prompt": "Process the available durable handoff; fully Read and overwrite the existing " + str(self.workspace / artifact_name) + " with the received body/task JSON, Read it back with limit=500 and stop. The message body and task ID must be obtained from the peer inbox. Do not ask for them."}
            if agent_id:
                agent_args["resume_from"] = agent_id
            prompt = "First call MessageTeammate exactly once and wait for its queue receipt, using these arguments: " + json.dumps({"to": name, "body": body, "idempotency_key": key, "task": task}) + \
                ". Then call Agent exactly once with these arguments: " + json.dumps(agent_args) + ". Do not add or repeat the message body/task ID in Agent.prompt. Do not execute Read or Write yourself. The Agent must finish in the foreground. After it returns, answer DONE."
            before = self.trace_snapshot()
            child_directory = self.home / "sessions" / "subagents"
            prior_child_paths = set(child_directory.glob("*.jsonl"))
            args = ["run", "--bare", "--tools", "Agent,MessageTeammate,Read,Write"]
            args += ["--session-id", sid] if attempt == 1 else ["--resume", sid]
            _, command = self.command([*args, prompt], "named-child-real-parent-" + str(attempt), json_output=False)
            parent_proof, rows = self.real_proof(before, "named-child-real-parent-" + str(attempt), required_tools=("Agent", "MessageTeammate"))
            require(not parent_proof["tool_results"].get("Read") and not parent_proof["tool_results"].get("Write"), "Parent performed the named child's file delivery")
            child_proof = nested_trace_proof(rows)
            require(child_proof["child_model_responses"] > 0 and not child_proof["child_errors"] and child_proof["child_tool_results"].get("Write") and child_proof["child_tool_results"].get("Read"),
                    "Named child did not perform its own actual model/tool work")
            agent_calls = tool_calls(rows, "Agent")
            require(len(agent_calls) == 1 and body not in agent_calls[0]["arguments"].get("prompt", "") and task_id not in agent_calls[0]["arguments"].get("prompt", ""),
                    "Parent smuggled the handed-off values into Agent.prompt or spawned extra children")
            if attempt == 2:
                require(agent_calls[0]["arguments"].get("resume_from") == agent_id, "Second named-child query did not actually use resume_from")
            child_path, header = select_child_transcript(child_directory, sid, name, self.model, prior_child_paths, agent_id)
            agent_id = header["id"]
            child_entries = [json.loads(line) for line in child_path.read_text().splitlines()]
            header = child_entries[0].get("header", {})
            require(header.get("id") == agent_id and header.get("model") == self.model and header.get("sub_agent_of") == sid and header.get("teammate_name") == name,
                    "Named child transcript is not bound to this parent/model/name")
            bindings = [row["handoff"] for row in child_entries if row.get("type") == "handoff"]
            require(bindings == [{"scope": "session:" + sid, "recipient": name}], "Named child lost or changed its original durable mailbox binding")
            terminal = [row.get("terminal", {}) for row in child_entries if row.get("type") == "terminal"][-1]
            require(terminal.get("status") == "completed" and not terminal.get("exitError"), "Named child did not finish cleanly")
            if initial_transcript_prefix:
                require(child_path.read_bytes().startswith(initial_transcript_prefix), "Resume overwrote the completed child transcript instead of appending")
            listing, _ = self.command(["handoff", "list", "--scope", "session:" + sid, "--to", name], "named-child-mailbox-" + str(attempt))
            matches = [row for row in listing if row.get("body") == body]
            require(len(matches) == 1 and matches[0].get("state") == "acknowledged", "Named child did not checkpoint and acknowledge its own durable message")
            receipt = matches[0]
            snapshots = history_snapshots(child_path)
            require(envelope_count(snapshots[-1], receipt["id"]) == 1 and any(go_history_sha(history) == receipt["checkpoint_sha256"] for history in snapshots),
                    "Named-child receipt does not match a producer-owned persisted history checkpoint")
            for old in receipts:
                require(envelope_count(snapshots[-1], old["id"]) == 1, "Resume injected an earlier acknowledged child message again")
                reloaded, _ = self.command(["handoff", "status", old["id"]], "named-old-receipt-after-resume")
                require(reloaded.get("attempts") == old.get("attempts") and reloaded.get("checkpoint_sha256") == old.get("checkpoint_sha256"), "Resume re-claimed an already acknowledged child message")
            receipts.append(receipt)
            initial_transcript_prefix = child_path.read_bytes()
            artifacts.append(self.assert_artifact(artifact_name, {"message_body": body, "task_id": task_id}))
            real_queries.append({"parent": parent_proof, "named_child": child_proof, "actual_Agent_call": agent_calls[0], **command})
        require(child_path is not None, "Named-child delivery test never produced a transcript")
        self.record("real named Agent receives offline handoff and resume_from continues same producer-owned mailbox after parent restart", parent_session_id=sid,
            agent_id=agent_id, mailbox_scope="session:" + sid, transcript_sha256=digest(child_path), receipt_ids=[row["id"] for row in receipts],
            checkpoint_sha256s=[row["checkpoint_sha256"] for row in receipts], artifacts=artifacts, real_queries=real_queries)

    def handoff_lease_fencing(self) -> None:
        recipient = "job:" + self.jobs["budget-isolation"]["id"]
        sent, _ = self.command(["handoff", "send", "--to", recipient, "--body", "Local lease fencing fixture, no business side effect.",
            "--key", "lease-" + secrets.token_hex(8)], "handoff-lease-fixture-send")
        claimed, _ = self.command(["handoff", "claim", "--scope", "jobs", "--to", recipient, "--consumer", "first-process", "--lease", "100ms"], "handoff-first-process-claim")
        record = claimed.get("record", claimed)
        identifier, old_token = record.get("envelope", record)["id"], record["token"]
        expires = record.get("lease_expires", "")
        require(expires, "Handoff claim has no persisted lease expiry")
        time.sleep(0.25)
        reclaimed, _ = self.command(["handoff", "claim", "--scope", "jobs", "--to", recipient, "--consumer", "replacement-process", "--lease", "30s"], "handoff-second-process-reclaim-expired")
        replacement = reclaimed.get("record", reclaimed)
        require(replacement.get("envelope", replacement).get("id") == identifier and replacement["token"] != old_token and replacement.get("attempts") == 2,
            "Expired message lease was not replaced with a fenced token")
        self.command(["handoff", "renew", identifier, "--token", old_token, "--lease", "30s"], "old-message-token-renew-rejected", expect_success=False, json_output=False,
                     rejection_contains="lease")
        self.command(["handoff", "release", identifier, "--token", old_token], "old-message-token-release-rejected", expect_success=False, json_output=False,
                     rejection_contains="lease")
        self.command(["handoff", "release", identifier, "--token", replacement["token"]], "replacement-message-token-release")
        self.record("independent CLI processes reclaim expired handoff and reject old-token mutations", message_id=identifier,
            attempts=replacement["attempts"], consumer=replacement["consumer"], lease_expiration_recovery=True, old_renew_and_release_rejected=True,
            fixture_only=True, additional_provider_requests=0)

    def ordinary_skill_query(self, name: str, marker: str, expected: str, label: str) -> dict[str, Any]:
        before = self.trace_snapshot()
        session = str(uuid.uuid4())
        prompt = "Invoke the " + name + " skill via Skill action=invoke once, then apply its ACTIVE instruction. Return only the skill-prescribed final text. Do not use other tools."
        _, command = self.command(["run", "--bare", "--tools", "Skill", "--session-id", session, prompt], label, json_output=False)
        proof, rows = self.real_proof(before, label, required_tools=("Skill",))
        path = self.home / "sessions" / (session + ".jsonl")
        require(path.is_file(), "Ordinary skill query lacks a durable session transcript")
        history = history_snapshots(path)[-1]
        require(final_assistant(history) == expected, "Ordinary query did not apply the selected active skill version")
        proof.update(assert_skill_invoked(rows, name, marker))
        proof.update({"session_id": session, "history_sha256": digest(path), "expected_final_matched": True, **command})
        return proof

    def learning_loop(self) -> None:
        name = "live-learning-contract"
        nonce = secrets.token_hex(8)
        quality1, quality2 = "QUALITY_ONE_" + nonce, "QUALITY_TWO_" + nonce
        baseline_marker, good_marker = "ACTIVE_BASELINE_" + nonce, "ACTIVE_GOOD_" + nonce
        def skill(first: str, second: str, active: str) -> str:
            return "---\nname: " + name + "\ndescription: Deterministic host-checked learning contract. Invoke when explicitly requested.\n---\n" + \
                "After invocation, follow exactly one mode from the current user task. QUAL_ONE: return exactly " + first + ". QUAL_TWO: return exactly " + second + ". ACTIVE: return exactly " + active + ". Return only that text, no punctuation or explanation. Never invoke another tool.\n"
        active_path = self.home / "skills" / (name + ".md")
        active_path.write_text(skill(quality1, quality2, baseline_marker))
        baseline_sha = digest(active_path)
        bad_file, good_file = self.root / "candidate-bad.md", self.root / "candidate-good.md"
        bad_file.write_text(skill("WRONG_ONE_" + nonce, "WRONG_TWO_" + nonce, "ACTIVE_BAD_" + nonce))
        good_file.write_text(skill(quality1, quality2, good_marker))
        suite_file = self.root / "learning-suite.json"
        atomic_json(suite_file, {"name": "fixed-host-contract-" + nonce, "cases": [
            {"id": "quality-one", "prompt": "Invoke the requested skill and apply its QUAL_ONE instruction. Return only its prescribed text.", "expected": quality1},
            {"id": "quality-two", "prompt": "Invoke the requested skill and apply its QUAL_TWO instruction. Return only its prescribed text.", "expected": quality2}],
            "max_tokens": 100000, "max_duration_seconds": int(self.args.query_timeout)})
        bad, _ = self.command(["learning", "stage", "--name", name, "--file", str(bad_file)], "stage-bad-hidden-candidate")
        require(bad.get("id") and digest(active_path) == baseline_sha, "Staging candidate overwrote the active baseline skill")
        policy = self.configure_controller("skill", name, suite_file, "configure-automatic-skill-learning")
        failed, failed_tick = self.controller_evaluate("skill", name, bad["id"], "rejected", "automatic-skill-bad-candidate-rejected")
        self.assert_evaluation(failed, name, bad["id"], bad.get("parent", ""))
        require(failed.get("passed") is False and failed.get("baseline", {}).get("score") == 1 and failed.get("candidate", {}).get("score") == 0,
            "Bad candidate rejection did not come from the host quality assertion")
        require(digest(active_path) == baseline_sha, "Failed candidate was promoted into the active skill")
        self.controller_idle("skill", name, "automatic-skill-does-not-repeat-rejected-candidate")
        before_good = self.ordinary_skill_query(name, baseline_marker, baseline_marker, "ordinary-query-after-bad-evaluation-still-baseline")
        good, _ = self.command(["learning", "stage", "--name", name, "--file", str(good_file)], "stage-good-hidden-candidate")
        require(good.get("id") and digest(active_path) == baseline_sha, "Good staging changed the active skill before promotion")
        evaluation, good_tick = self.controller_evaluate("skill", name, good["id"], "promoted", "automatic-skill-good-candidate-evaluated-and-promoted")
        self.assert_evaluation(evaluation, name, good["id"], good.get("parent", ""))
        require(evaluation.get("passed") is True and evaluation.get("baseline", {}).get("score") == 1 and evaluation.get("candidate", {}).get("score") == 1,
            "Good candidate did not pass the fixed host oracle against its baseline")
        promoted, _ = self.command(["learning", "list", "--name", name], "skill-authority-after-automatic-promotion")
        require(promoted.get("active_version") == good["id"], "Automatic promotion did not activate the exact tested skill version")
        self.controller_idle("skill", name, "automatic-skill-idle-after-promotion")
        promoted_query = self.ordinary_skill_query(name, good_marker, good_marker, "ordinary-query-uses-promoted-good-version")
        # Promotion changed the active baseline, so an evaluation tied to the
        # old baseline must never be accepted a second time (CAS protection).
        self.command(["learning", "promote", "--name", name, "--version", good["id"], "--evaluation", evaluation["id"], "--suite", str(suite_file)],
            "stale-evaluation-promotion-rejected", expect_success=False, json_output=False, rejection_contains="active version changed")
        baseline_version = good.get("parent") or bad.get("parent")
        require(baseline_version, "Candidate lacks a frozen baseline version identity")
        self.command(["learning", "rollback", "--name", name, "--version", baseline_version], "rollback-tested-skill-to-baseline")
        rollback, _ = self.command(["learning", "list", "--name", name], "skill-authority-after-rollback")
        require(rollback.get("active_version") == baseline_sha, "Rollback did not restore the exact previous active skill version")
        rolled_back_query = self.ordinary_skill_query(name, baseline_marker, baseline_marker, "ordinary-query-uses-rolled-back-baseline")
        state, _ = self.command(["learning", "list", "--name", name], "learning-version-history-after-rollback")
        require(state.get("active_version") == baseline_version and len(state.get("evaluations", [])) >= 2 and len(state.get("transitions", [])) >= 2,
            "Learning history lost evaluation, promotion or rollback records")
        self.report["learning_evaluations"] = [failed, evaluation]
        self.record("real fixed-suite candidate evaluation gates activation and exact rollback with stale-baseline CAS", skill=name,
            suite_sha256=digest(suite_file), baseline_version=baseline_version, bad_version=bad["id"], good_version=good["id"],
            baseline_file_sha256=baseline_sha, good_file_sha256=digest(good_file), bad_evaluation=failed, good_evaluation=evaluation,
            controller_policy=policy, rejected_tick=failed_tick, automatically_promoted_tick=good_tick,
            real_queries=[before_good, promoted_query, rolled_back_query], final_state_sha256=json_sha(state))

    def configure_controller(self, kind: str, name: str, suite: Path, label: str) -> dict[str, Any]:
        value, _ = self.command(["learning", "configure", "--kind", kind, "--name", name, "--suite", str(suite),
            "--max-evaluations", "4", "--max-total-tokens", "1000000", "--auto-promote"], label)
        identity = value.get("identity", {})
        require(value.get("kind") == kind and value.get("name") == name and value.get("auto_promote") is True and value.get("max_evaluations") == 4 and
                identity.get("binary_sha256") == self.input_sha and identity.get("provider") == self.provider and identity.get("model") == self.model and
                re.fullmatch(r"[a-f0-9]{64}", identity.get("provider_config_sha256", "")), "Automatic learning policy lost its frozen checker/CLI/route identity")
        require(value.get("suite") == json.loads(suite.read_text()) and value.get("revision", 0) > 0 and value.get("suite_sha256"), "Controller did not freeze exact host suite contents")
        return value

    def controller_evaluate(self, kind: str, name: str, version: str, status: str, label: str) -> tuple[dict[str, Any], dict[str, Any]]:
        tick, _ = self.command(["learning", "watch", "--once", "--kind", kind, "--name", name], label,
                               operation_timeout=min(self.args.case_timeout, self.args.query_timeout * 4 + 30))
        receipt = tick.get("receipt", {})
        require(tick.get("idle") is False and receipt.get("candidate_sha256") == version and receipt.get("status") == status and receipt.get("evaluation_id"),
                "Automatic watcher did not evaluate/promote the new exact candidate: " + json.dumps(tick))
        state, _ = self.command(["learning", "list", "--kind", kind, "--name", name], label + "-persisted-evaluation")
        evaluations = [value for value in state.get("evaluations", []) if value.get("id") == receipt["evaluation_id"]]
        require(len(evaluations) == 1, "Automatic watcher receipt has no unique persisted evaluation")
        evaluation = evaluations[0]
        reported = evaluation.get("baseline", {}).get("tokens", 0) + evaluation.get("candidate", {}).get("tokens", 0)
        require(reported > 0 and receipt.get("reported_tokens") == reported and receipt.get("charged_tokens") == reported and receipt.get("reserved_tokens") == 200000,
                "Controller did not settle its admission reservation using complete actual evaluation usage")
        if status == "promoted":
            require(state.get("active_version") == version and evaluation.get("passed") is True, "Watcher activated an unpassed or different artifact version")
        else:
            require(state.get("active_version") == receipt.get("baseline_version") and evaluation.get("passed") is False, "Watcher changed active state after a failed candidate")
        return evaluation, tick

    def controller_idle(self, kind: str, name: str, label: str) -> None:
        before = self.trace_snapshot()
        state_before, _ = self.command(["learning", "policy"], label + "-controller-before")
        tick, _ = self.command(["learning", "watch", "--once", "--kind", kind, "--name", name], label)
        state_after, _ = self.command(["learning", "policy"], label + "-controller-after")
        require(tick.get("idle") is True and not tick.get("receipt") and state_before.get("receipts", []) == state_after.get("receipts", []),
                "Idle watcher retried a consumed candidate or reserved another paid evaluation")
        require(not trace_proof(self.trace_events(before), self.provider, self.model)["model_responses"] and not nested_trace_proof(self.trace_events(before))["child_model_responses"],
                "Idle watcher emitted another actual model usage event")
        self.record(label, controller_receipts_unchanged=True, new_evaluations=0, new_model_usage_events=0,
            boundary="No new candidate evaluation or token-bearing model invocation; anonymous HTTP preconnects are not measured here")

    def assert_evaluation(self, value: dict[str, Any], name: str, candidate: str, baseline: str, *, kind: str = "skill") -> None:
        require(value.get("binary_sha256") == self.input_sha and value.get("provider") == self.provider and value.get("model") == self.model and
                value.get("candidate_version") == candidate and value.get("baseline_version") == baseline and value.get("inputs_stable") is True and value.get("kind") == kind,
                "Evaluation was not bound to this frozen CLI, original model, artifact kind and baseline")
        for label, expected_version in (("baseline", baseline), ("candidate", candidate)):
            group = value.get(label, {})
            require(group.get("version") == expected_version and len(group.get("cases", [])) == 2, "Evaluation lost its exact version or fixed suite cases")
            for case in group["cases"]:
                evidence = case.get("evidence", {})
                require(case.get("exit_code") == 0 and case.get(kind + "_invoked") is True and not case.get("error") and case.get("usage_reported") is True and
                        case.get("model_requests", 0) > 0 and len(evidence.get("token_event_ids", [])) == case["model_requests"],
                        "Quality evaluation lacks successful actual model/Skill execution")
                usage = case.get("usage", {})
                require(set(usage) == {"input", "output", "cache_read", "cache_create"} and all(type(number) is int and number >= 0 for number in usage.values()) and sum(usage.values()) > 0,
                        "Quality case lacks complete nonnegative actual usage")
                require(evidence.get("provider") == self.provider and evidence.get("model") == self.model and evidence.get("session_id") and
                        evidence.get("manifest_sha256") == expected_version and re.fullmatch(r"[a-f0-9]{64}", evidence.get("trace_sha256", "")),
                        "Quality case evidence belongs to another provider, model or skill version")
                calls = evidence.get(kind + "_invocations", [])
                require(calls, "Quality case omitted actual artifact invocation evidence")
                for call in calls:
                    args = call.get("arguments", {})
                    if isinstance(args, str):
                        args = json.loads(args)
                    operation_ok = args.get("operation") == "run_named" if kind == "workflow" else args.get("action") == "invoke"
                    require(operation_ok and args.get("name") == name and call.get("call_id") and call.get("invocation_id") and
                            call.get("result_sha256") == hashlib.sha256(call.get("result", "").encode()).hexdigest(),
                            "Quality case result is not tied to its concrete artifact invocation")
                    if kind == "workflow":
                        require(call.get("recipe_sha256") == expected_version and json.loads(call["result"]).get("recipe_sha256") == expected_version,
                                "Actual Workflow invocation did not use the frozen evaluated recipe")

    def ordinary_workflow_query(self, name: str, version: str, expected: Any, label: str) -> dict[str, Any]:
        before = self.trace_snapshot()
        sid = str(uuid.uuid4())
        prompt = "Invoke Workflow exactly once with operation=run_named and name=" + name + ". Do not submit inline steps, stage candidates or call another tool. After it completes, return DONE."
        (self.workspace / "answer.json").unlink(missing_ok=True)
        _, command = self.command(["run", "--bare", "--tools", "Workflow", "--session-id", sid, prompt], label, json_output=False)
        proof, rows = self.real_proof(before, label, required_tools=("Workflow",))
        calls = [call for call in tool_calls(rows, "Workflow") if call["arguments"].get("operation") == "run_named" and call["arguments"].get("name") == name]
        require(len(calls) == 1, "Ordinary query did not invoke exactly one active named Workflow")
        actual = workflow_result(calls[0]["result"], name, version, expected)
        return {**proof, **command, "actual_workflow_call": calls[0], "host_step_assertion": actual, "session_id": sid}

    def workflow_learning_loop(self) -> None:
        name = "live-workflow-contract"
        recipes = self.home / "workflows"
        recipes.mkdir(mode=0o700, exist_ok=True)
        active_path = recipes / (name + ".json")
        baseline = {"name": name, "steps": [{"name": "baseline-answer", "command": "printf '{\"answer\":322}\\n'"}]}
        bad_recipe = {"name": name, "steps": [{"name": "wrong-answer", "command": "printf '{\"answer\":999}\\n'"}]}
        good_recipe = {"name": name, "steps": [
            {"name": "compute-answer", "command": "printf '{\"answer\":%s}\\n' \"$((17 * 19))\" > answer.json"},
            {"name": "read-answer", "command": "cat answer.json"}]}
        atomic_json(active_path, baseline)
        baseline_version = digest(active_path)
        bad_file, good_file, suite_file = self.root / "workflow-bad.json", self.root / "workflow-good.json", self.root / "workflow-suite.json"
        atomic_json(bad_file, bad_recipe)
        atomic_json(good_file, good_recipe)
        atomic_json(suite_file, {"name": "actual-posix-workflow-output", "cases": [
            {"id": "first-independent-run", "prompt": "Execute the requested named workflow once.", "expected_json": {"answer": 323}},
            {"id": "second-independent-run", "prompt": "Execute the requested named workflow once again in this independent case.", "expected_json": {"answer": 323}}],
            "max_tokens": 100000, "max_duration_seconds": int(self.args.query_timeout)})
        bad, _ = self.command(["learning", "stage", "--kind", "workflow", "--name", name, "--file", str(bad_file)], "workflow-bad-candidate-staged-hidden")
        require(bad.get("parent") == baseline_version and digest(active_path) == baseline_version, "Workflow staging changed its runnable host baseline")
        policy = self.configure_controller("workflow", name, suite_file, "configure-automatic-workflow-learning")
        failed, failed_tick = self.controller_evaluate("workflow", name, bad["id"], "rejected", "automatic-workflow-bad-real-output-rejected")
        self.assert_evaluation(failed, name, bad["id"], baseline_version, kind="workflow")
        require(failed.get("passed") is False and failed.get("baseline", {}).get("score") == 0 and failed.get("candidate", {}).get("score") == 0,
                "Wrong Workflow subprocess output did not fail the host quality gate")
        self.controller_idle("workflow", name, "automatic-workflow-does-not-repeat-rejected-candidate")
        baseline_query = self.ordinary_workflow_query(name, baseline_version, {"answer": 322}, "ordinary-workflow-after-failed-candidate-baseline")
        # The production model-facing save path must propose, never directly
        # activate. This real query connects model proposal to the automatic
        # watcher instead of relying entirely on host-authored stage commands.
        before_proposal = self.trace_snapshot()
        proposal_sid = str(uuid.uuid4())
        prompt = "Call Workflow exactly once with operation=save and the following name/steps. Do not run it, alter the recipe or call another tool. This must stage a candidate and leave the active recipe unchanged. Arguments: " + json.dumps({"operation": "save", **good_recipe}) + ". After the staging receipt, return STAGED."
        self.command(["run", "--bare", "--tools", "Workflow", "--session-id", proposal_sid, prompt], "real-model-proposes-workflow-candidate", json_output=False)
        proposal_proof, proposal_rows = self.real_proof(before_proposal, "real-model-proposes-workflow-candidate", required_tools=("Workflow",))
        calls = tool_calls(proposal_rows, "Workflow")
        require(len(calls) == 1 and calls[0]["arguments"].get("operation") == "save" and calls[0]["arguments"].get("name") == name and
                calls[0]["arguments"].get("steps") == good_recipe["steps"], "Model did not actually propose the specified Workflow recipe")
        proposal_state, _ = self.command(["learning", "list", "--kind", "workflow", "--name", name], "workflow-state-after-model-proposal")
        proposed = [version for version in proposal_state.get("versions", {}).values() if version.get("source") == "model-workflow-proposal" and json.loads(version.get("content", "null")) == good_recipe]
        require(len(proposed) == 1 and proposal_state.get("active_version") == baseline_version, "Model proposal bypassed activation or lost its exact recipe identity")
        good = proposed[0]
        require(good.get("id") == hashlib.sha256(good["content"].encode()).hexdigest() and good.get("parent") == baseline_version,
                "Proposed Workflow version is not bound to immutable bytes/current baseline")
        evaluation, good_tick = self.controller_evaluate("workflow", name, good["id"], "promoted", "automatic-workflow-real-output-improvement-and-promotion")
        self.assert_evaluation(evaluation, name, good["id"], baseline_version, kind="workflow")
        require(evaluation.get("passed") is True and evaluation.get("baseline", {}).get("score") == 0 and evaluation.get("candidate", {}).get("score") == 1,
                "Good Workflow recipe did not actually improve subprocess output against the fixed host oracle")
        self.controller_idle("workflow", name, "automatic-workflow-idle-after-promotion")
        promoted_query = self.ordinary_workflow_query(name, good["id"], {"answer": 323}, "ordinary-workflow-uses-promoted-exact-recipe")
        artifact = self.assert_artifact("answer.json", {"answer": 323})
        saved_artifact = self.output / "artifacts" / "workflow-promoted-answer.json"
        saved_artifact.parent.mkdir(mode=0o700, exist_ok=True)
        shutil.copyfile(self.workspace / "answer.json", saved_artifact)
        require(digest(saved_artifact) == artifact["sha256"], "Retained promoted Workflow artifact differs from the actual subprocess output file")
        self.command(["learning", "promote", "--kind", "workflow", "--name", name, "--version", good["id"], "--evaluation", evaluation["id"], "--suite", str(suite_file)],
            "workflow-stale-evaluation-CAS-rejected", expect_success=False, json_output=False, rejection_contains="active version changed")
        self.command(["learning", "rollback", "--kind", "workflow", "--name", name, "--version", baseline_version], "workflow-rollback-to-exact-baseline")
        rollback_query = self.ordinary_workflow_query(name, baseline_version, {"answer": 322}, "ordinary-workflow-uses-rolled-back-recipe")
        final, _ = self.command(["learning", "list", "--kind", "workflow", "--name", name], "workflow-version-authority-after-rollback")
        require(final.get("active_version") == baseline_version and len(final.get("evaluations", [])) == 2 and len(final.get("transitions", [])) >= 2,
                "Workflow learning authority lost activation/evaluation/rollback history")
        self.report["workflow_evaluations"] = [failed, evaluation]
        self.record("actual Workflow subprocess outputs gate version promotion and exact active-recipe rollback", workflow=name,
            fixed_suite_sha256=digest(suite_file), baseline_version=baseline_version, bad_version=bad["id"], good_version=good["id"],
            bad_evaluation=failed, good_evaluation=evaluation, promoted_artifact=artifact,
            controller_policy=policy, rejected_tick=failed_tick, automatically_promoted_tick=good_tick,
            actual_model_proposal={**proposal_proof, "actual_Workflow_save": calls[0], "candidate_source": good["source"]},
            real_ordinary_queries=[baseline_query, promoted_query, rollback_query], authority_sha256=json_sha(final),
            recipe_evaluation_surface="Restricted POSIX fixture inside an owned OS-sandboxed workspace; not arbitrary production scripts")

    @staticmethod
    def stop(process: subprocess.Popen) -> None:
        if process.poll() is not None:
            return
        try:
            os.killpg(process.pid, signal.SIGTERM)
        except ProcessLookupError:
            pass
        try:
            process.wait(timeout=10)
        except subprocess.TimeoutExpired:
            try:
                os.killpg(process.pid, signal.SIGKILL)
            except ProcessLookupError:
                pass
            process.wait(timeout=10)

    def cleanup(self) -> None:
        errors = []
        for process in self.processes:
            try:
                self.stop(process)
            except BaseException as error:
                errors.append(self.redact(str(error)))
        # A killed learning parent may have created a child process group.
        # Only commands containing this exact private executable are owned.
        rows = subprocess.run(["ps", "-axo", "pid=,pgid=,command="], capture_output=True, text=True, timeout=10).stdout.splitlines()
        owned_children = []
        for row in rows:
            parts = row.strip().split(None, 2)
            if len(parts) == 3 and parts[0].isdigit() and parts[1].isdigit() and str(self.binary) in parts[2]:
                pid, pgid = int(parts[0]), int(parts[1])
                if pid != os.getpid():
                    owned_children.append(pid)
                    try:
                        require(pgid != os.getpgrp(), "Owned child unexpectedly shares the harness process group")
                        os.killpg(pgid, signal.SIGKILL)
                    except ProcessLookupError:
                        pass
        deadline = time.monotonic() + 10
        def alive(pid: int) -> bool:
            row = subprocess.run(["ps", "-p", str(pid), "-o", "stat="], capture_output=True, text=True, timeout=5).stdout.strip()
            return bool(row) and not row.startswith("Z")
        while any(alive(pid) for pid in owned_children) and time.monotonic() < deadline:
            time.sleep(0.05)
        for handles in self.handles.values():
            for handle in handles:
                handle.close()
        self.handles.clear()
        self.binary.unlink(missing_ok=True)
        shutil.rmtree(self.home / ".credentials", ignore_errors=True)
        self.key = ""
        if self.args.destroy_input and self.source_binary.exists():
            require(digest(self.source_binary) == getattr(self, "input_sha", self.args.expected_sha256), "Refusing to destroy a replaced supplied CLI")
            self.source_binary.unlink()
        for path in self.output.glob("*.log"):
            path.write_text(self.redact(path.read_text(errors="replace")))
        proof = {"private_CLI_destroyed": not self.binary.exists(), "private_credentials_removed": not (self.home / ".credentials").exists(),
            "input_binary_destroyed": not self.source_binary.exists() if self.args.destroy_input else False,
            "all_owned_parent_processes_stopped": all(process.poll() is not None for process in self.processes),
            "owned_child_pids": owned_children, "all_owned_child_processes_stopped": not any(alive(pid) for pid in owned_children),
            "selected_key_handle_cleared": not self.key, "cleanup_errors": errors}
        self.report["cleanup"] = proof
        require(not errors and all(proof[field] for field in ("private_CLI_destroyed", "private_credentials_removed", "all_owned_parent_processes_stopped", "all_owned_child_processes_stopped", "selected_key_handle_cleared")), "Disposable CLI/auth/process cleanup incomplete")
        if self.args.destroy_input:
            require(proof["input_binary_destroyed"], "Parent-supplied disposable CLI was not destroyed")

    def copy_evidence(self) -> None:
        paths = []
        for directory in ("sessions", "traces", "jobs", "handoff", "skills/.learning", "workflows", "learning-controller", "project-coordinator"):
            source_dir = self.home / directory
            if not source_dir.exists():
                continue
            for source in source_dir.rglob("*"):
                if not source.is_file() or source.is_symlink() or source.suffix not in (".json", ".jsonl", ".md"):
                    continue
                destination = self.output / "evidence" / directory / source.relative_to(source_dir)
                destination.parent.mkdir(parents=True, exist_ok=True, mode=0o700)
                destination.write_text(self.redact(source.read_text(errors="replace")))
                destination.chmod(0o600)
                paths.append({"path": str(destination.relative_to(self.output)), "sha256": digest(destination)})
        for source in self.workspace.iterdir():
            if source.is_file() and source.suffix == ".json" and not source.is_symlink():
                destination = self.output / "artifacts" / source.name
                destination.parent.mkdir(exist_ok=True, mode=0o700)
                destination.write_text(self.redact(source.read_text()))
                destination.chmod(0o600)
                paths.append({"path": str(destination.relative_to(self.output)), "sha256": digest(destination)})
        atomic_json(self.output / "evidence-manifest.json", paths)

    def verify_freeze(self) -> None:
        if hasattr(self, "original_sha"):
            require(digest(self.original_config) == self.original_sha, "Original real configuration changed during the live test")
            self.report["real_config_sha256_unchanged"] = True
        snapshot = json.loads(self.args.source_freeze.read_text())
        require(digest(self.args.source_freeze) == getattr(self, "freeze_sha", digest(self.args.source_freeze)), "Freeze manifest changed")
        changed = [name for name, sha in snapshot["files"].items() if not (self.repo / name).is_file() or digest(self.repo / name) != sha]
        require(not changed, "Source changed while the compiled CLI was under test: " + ", ".join(changed[:8]))
        self.report["source_freeze_unchanged_after_cleanup"] = True

    def run(self) -> int:
        signal.setitimer(signal.ITIMER_REAL, self.args.case_timeout)
        try:
            self.stage()
            self.load_route()
            self.persistent_jobs()
            self.durable_handoffs()
            self.learning_loop()
            self.workflow_learning_loop()
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
        print(json.dumps({"passed": self.report["passed"], "checks": len(self.report["checks"]), "report": str(self.output / "verification-report.json"), "failure": self.report.get("failure")}), flush=True)
        return 0 if self.report["passed"] else 1


def round_02_agent_trace_fixture() -> list[dict[str, Any]]:
    """Reduced actual round-02 trace shape; generated paths/body are replaced.

    Source: live-cli-round-02/evidence/traces/ef2b4fc8-56af-448f-9313-4fb09ae215bb.jsonl.
    Preserve the real occurrence/parent IDs and usage while making this fixture
    portable. The container result is DONE and supplies no textual agent ID.
    """
    sid = "ef2b4fc8-56af-448f-9313-4fb09ae215bb"
    prefix = "trace-inv-b42e2cc7-9407-4fa3-9b04-d8407d1fccc9-"
    parent, child, outer_call = prefix + "1", prefix + "4", prefix + "3"
    use = "call_GxZ6yL58WiradJsCl7SpkgDI"
    arguments = json.dumps({"name": "live-reliable-child", "prompt": "portable fixture task"})
    rows = [
        {"id": "be6305b2-c2cc-4288-aad2-6eca516ebfdf", "kind": "tool_args", "trace_invocation_id": parent,
         "trace_call_id": outer_call, "tool_use_id": use, "tool_name": "Agent", "text": arguments},
        {"kind": "tokens", "trace_invocation_id": parent, "text": "input=4888 output=118 cache_write=0 cache_read=0"},
        {"id": "c10c2cf2-d11c-4325-9cb5-2a96bff03ff6", "kind": "tool_start", "trace_invocation_id": child,
         "trace_parent_invocation_id": parent, "trace_call_id": outer_call, "tool_use_id": use, "tool_name": "Agent", "text": arguments},
        {"kind": "tool_args", "trace_invocation_id": child, "trace_call_id": prefix + "5", "tool_use_id": "call_zNqGof1p2qK0QhWuiYBWYwKO",
         "subagent_of": use, "tool_name": "Read", "text": '{"path":"/owned/fixture.json"}'},
        {"kind": "tokens", "trace_invocation_id": child, "subagent_of": use, "text": "input=2514 output=66 cache_write=0 cache_read=0"},
        {"kind": "tool_start", "trace_invocation_id": child, "trace_call_id": prefix + "5", "tool_use_id": "call_zNqGof1p2qK0QhWuiYBWYwKO",
         "parent_id": use, "tool_name": "Read", "text": '{"path":"/owned/fixture.json"}'},
        {"kind": "tool_result", "trace_invocation_id": child, "trace_call_id": prefix + "5", "tool_use_id": "call_zNqGof1p2qK0QhWuiYBWYwKO",
         "parent_id": "call_zNqGof1p2qK0QhWuiYBWYwKO", "tool_name": "Read", "text": "{}"},
        {"kind": "tool_args", "trace_invocation_id": child, "trace_call_id": prefix + "6", "tool_use_id": "call_p9E1E3D0K30xVRdBWlC2h2Y1",
         "subagent_of": use, "tool_name": "Write", "text": '{"path":"/owned/fixture.json","content":"fixture"}'},
        {"kind": "tool_result", "trace_invocation_id": child, "trace_call_id": prefix + "6", "tool_use_id": "call_p9E1E3D0K30xVRdBWlC2h2Y1",
         "parent_id": "call_p9E1E3D0K30xVRdBWlC2h2Y1", "tool_name": "Write", "text": "wrote /owned/fixture.json"},
        {"id": "8457b694-3dda-431a-8f7f-ba41837e415a", "kind": "tool_result", "trace_invocation_id": child,
         "trace_parent_invocation_id": parent, "trace_call_id": outer_call, "tool_use_id": use, "parent_id": use, "tool_name": "Agent", "text": "DONE"},
        {"kind": "tokens", "trace_invocation_id": parent, "text": "input=367 output=5 cache_write=0 cache_read=4992"}]
    return [{"session_id": sid, **row} for row in rows]


def self_test() -> dict[str, Any]:
    message = {"role": "user", "content": [{"type": "text", "text": '<peer_message durable="true"> {"id":"m-1"} </peer_message>', "synthetic": True}]}
    require(envelope_count([message], "m-1") == 1 and envelope_count([message], "m-2") == 0, "Durable envelope oracle is not ID-scoped")
    require(final_assistant([message, {"role": "assistant", "content": [{"type": "text", "text": "  FINAL  "}]}]) == "FINAL", "Final transcript oracle failed")
    events = [{"session_id": "session", "trace_invocation_id": "inv", "kind": "tokens", "text": "input=179 output=74 cache_write=512 cache_read=0"},
              {"session_id": "session", "trace_invocation_id": "inv", "trace_call_id": "call", "kind": "tool_args", "tool_name": "Skill", "text": '{"action":"invoke","name":"contract"}'},
              {"session_id": "session", "trace_invocation_id": "inv", "trace_call_id": "call", "kind": "tool_result", "tool_name": "Skill", "text": "## Skill: contract\nACTIVE_BASELINE"}]
    proof = trace_proof(events, "offline-fixture", "offline-fixture")
    require(proof["usage"][0]["input"] == 179 and proof["tool_results"]["Skill"] == 1, "Absolute invocation accounting fixture failed")
    assert_skill_invoked(events, "contract", "ACTIVE_BASELINE")
    rejected = False
    try:
        assert_skill_invoked(events, "contract", "ACTIVE_GOOD")
    except AssertionError:
        rejected = True
    require(rejected, "Host skill body oracle accepted the wrong active version")
    nested = nested_trace_proof([*events,
        {"session_id": "session", "trace_invocation_id": "child", "trace_parent_invocation_id": "parent-call", "kind": "tokens", "text": "input=12 output=3 cache_write=0 cache_read=0"},
        {"session_id": "session", "trace_invocation_id": "child", "subagent_of": "parent-tool", "kind": "tool_result", "tool_name": "Write", "text": "fixture"}])
    require(nested["child_model_responses"] == 1 and nested["child_tool_results"]["Write"] == 1 and nested["actual_child_usage"][0]["input"] == 12,
            "Named child oracle included parent usage or missed child tools")
    recipe = {"name": "fixture-flow", "recipe_sha256": "a" * 64, "steps": [{"name": "output", "command": "fixture-not-executed", "status": "ok", "exit_code": 0, "output": '{"answer":323}\n'}]}
    workflow_result(recipe, "fixture-flow", "a" * 64, {"answer": 323})
    wrong_workflow_rejected = False
    try:
        workflow_result(recipe, "fixture-flow", "a" * 64, {"answer": 322})
    except AssertionError:
        wrong_workflow_rejected = True
    require(wrong_workflow_rejected, "Workflow host oracle accepted the wrong actual subprocess output")
    actual_shape = round_02_agent_trace_fixture()
    actual_parent = trace_proof(actual_shape, "offline-fixture", "offline-fixture")
    actual_child = nested_trace_proof(actual_shape)
    actual_calls = tool_calls(actual_shape, "Agent")
    require(actual_parent["tool_results"] == {"Agent": 1} and actual_parent["model_responses"] == 2 and actual_parent["invocation_ids"] == [actual_shape[0]["trace_invocation_id"]],
            "Actual Agent boundary was dropped or child Read/Write/tokens were charged to its parent")
    require(actual_child["child_tool_results"] == {"Read": 1, "Write": 1} and actual_child["child_model_responses"] == 1 and
            len(actual_calls) == 1 and actual_calls[0]["invocation_id"] == actual_shape[0]["trace_invocation_id"] and actual_calls[0]["result"] == "DONE",
            "Actual child results were lost or Agent result was not correlated to root-owned args")
    for field, value in (("trace_call_id", "unrelated-call"), ("tool_use_id", "unrelated-use"), ("trace_parent_invocation_id", "unrelated-root"), ("subagent_of", "grandchild-container")):
        invalid = [dict(row) for row in actual_shape]
        invalid[-2][field] = value
        require(not trace_proof(invalid, "offline-fixture", "offline-fixture")["tool_results"].get("Agent") and not tool_calls(invalid, "Agent"),
                "Unanchored nested orchestration result was accepted for " + field)
    # Exercise executable-copy identity and failure cleanup using a inert file,
    # never a installed/local CLI and never real authentication. The fixture
    # is not executed. The original provider config is not read at all.
    repo = Path(__file__).resolve().parents[2]
    with tempfile.TemporaryDirectory(prefix="metis-persistent-host-oracles-") as directory:
        base = Path(directory)
        inert = base / "never-executed-inert-file"
        inert.write_text("OFFLINE_FIXTURE_NOT_A_CLI\n")
        freeze = base / "source-freeze.json"
        atomic_json(freeze, {"files": {"scripts/e2e/persistent_agent_live_check.py": digest(Path(__file__))}})
        child_transcript = base / "child.jsonl"
        child_transcript.write_text("\n".join(json.dumps(row) for row in [
            {"type": "header", "header": {"id": "fixture-only"}},
            {"type": "checkpoint", "checkpoint": {"offset": 0, "messages": [message]}},
            {"type": "terminal", "terminal": {"status": "completed"}},
            {"type": "checkpoint", "checkpoint": {"offset": 1, "messages": [{"role": "assistant", "content": [{"type": "text", "text": "FINAL"}]}]}}]) + "\n")
        rebuilt = history_snapshots(child_transcript)
        require(len(rebuilt) == 2 and len(rebuilt[-1]) == 2 and envelope_count(rebuilt[-1], "m-1") == 1 and final_assistant(rebuilt[-1]) == "FINAL",
                "Append-only named-child checkpoint replay lost its acknowledged envelope")
        named_directory = base / "subagents"
        named_directory.mkdir()
        named_path = named_directory / "agt-host-fixture.jsonl"
        named_path.write_text(json.dumps({"type": "header", "header": {"id": "agt-host-fixture", "model": "offline-fixture", "sub_agent_of": "parent", "teammate_name": "child"}}) + "\n" +
            json.dumps({"type": "handoff", "handoff": {"scope": "session:parent", "recipient": "child"}}) + "\n")
        selected, header = select_child_transcript(named_directory, "parent", "child", "offline-fixture", set())
        require(selected == named_path and header["id"] == "agt-host-fixture", "Child identity did not come from unique produced host metadata")
        selected, _ = select_child_transcript(named_directory, "parent", "child", "offline-fixture", {named_path}, "agt-host-fixture")
        require(selected == named_path, "Resume host identity did not retain its original transcript")
        args = argparse.Namespace(repo=repo, output=base / "evidence", binary=inert, expected_sha256=digest(inert),
                                  source_freeze=freeze, destroy_input=True, query_timeout=5, case_timeout=10)
        harness = PersistentAgentLiveCheck(args)
        try:
            harness.stage()
            require(harness.binary.read_bytes() == inert.read_bytes(), "Private staged bytes differ")
            fixture_credentials = harness.home / ".credentials"
            fixture_credentials.mkdir(mode=0o700)
            atomic_json(fixture_credentials / "auth.json", {"fixture": "NEVER_A_REAL_KEY"})
            harness.key = "NEVER_A_REAL_KEY"
            harness.secrets.append(harness.key)
            harness.cleanup()
            require(not inert.exists() and not harness.binary.exists() and not fixture_credentials.exists(), "Offline failure cleanup left a copied executable/credential file")
        finally:
            shutil.rmtree(harness.root, ignore_errors=True)
    return {"passed": True, "offline_fixtures_only": True, "real_CLI_invocations": 0, "real_provider_requests": 0,
            "real_credential_files_read": False, "checks": ["ID-scoped envelope count", "final transcript output", "absolute usage", "occurrence-matched Skill invoke", "wrong-version rejection",
                "child usage separated from parent", "actual round-02 root Agent occurrence correlation", "unanchored nested results rejected", "child identity from unique host transcript",
                "actual Workflow result and recipe hash", "wrong Workflow output rejection", "append-only child checkpoint replay", "inert staged-file and private credential cleanup"],
            "go_history_fixture_sha256": go_history_sha([message])}


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("--repo", type=Path, default=Path(__file__).resolve().parents[2])
    parser.add_argument("--binary", type=Path)
    parser.add_argument("--expected-sha256")
    parser.add_argument("--source-freeze", type=Path)
    parser.add_argument("--output", type=Path)
    parser.add_argument("--destroy-input", action="store_true")
    parser.add_argument("--query-timeout", type=float, default=300)
    parser.add_argument("--case-timeout", type=float, default=3600)
    parser.add_argument("--self-test", action="store_true")
    args = parser.parse_args()
    os.umask(0o077)
    if args.self_test:
        print(json.dumps(self_test(), ensure_ascii=False, indent=2))
        return 0
    require(args.binary and args.expected_sha256 and args.source_freeze and args.output, "Live mode requires --binary, --expected-sha256, --source-freeze and --output")
    require(args.destroy_input, "This acceptance contract requires --destroy-input for both failed and passing CLIs")
    require(re.fullmatch(r"[a-f0-9]{64}", args.expected_sha256), "Expected binary SHA must be exact SHA-256")
    require(0 < args.query_timeout <= 600 and 0 < args.case_timeout <= 3600, "Live timeouts must be bounded")
    def interrupted(signum: int, _frame: Any) -> None:
        raise KeyboardInterrupt("Harness interrupted by " + signal.Signals(signum).name)
    def timed_out(_signum: int, _frame: Any) -> None:
        raise TimeoutError("Bounded persistent-agent case exceeded its deadline")
    signal.signal(signal.SIGINT, interrupted)
    signal.signal(signal.SIGTERM, interrupted)
    signal.signal(signal.SIGALRM, timed_out)
    return PersistentAgentLiveCheck(args).run()


if __name__ == "__main__":
    raise SystemExit(main())
