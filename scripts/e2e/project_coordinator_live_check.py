#!/usr/bin/env python3
"""Build an isolated CLI and exercise durable coordination with the real model.

Requires Python 3.11+, Go, and an already configured API-key provider. No mock
provider is used. A normal run continuously submits useful randomized batches
for at least two hours; --quick uses 90 seconds and is explicitly NOT long-task
acceptance. Both modes check cross-process claim contention, SIGKILL recovery,
SIGTERM restart recovery, stale-token fencing, host acceptance and durable usage.

Every failure stops owned process groups and destroys the temporary CLI and
credential copy before returning. Code fixes/retries must start a new invocation
of this script, hence a newly compiled CLI. Successful runs also destroy them.
The soak uses one continuously running coordinator CLI process and keeps useful
work queued while its agent runtime handles successive items. It does not
install a resident OS service or claim one uninterrupted provider request.
"""

from __future__ import annotations

import argparse
from collections import Counter
from datetime import datetime, timezone
import hashlib
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
import tempfile
import time
import tomllib
from typing import Any, Callable
from urllib.parse import urlsplit, urlunsplit
import uuid


def utc_now() -> str:
    return datetime.now(timezone.utc).isoformat()


def trace_timestamp(value: Any) -> float | None:
    if not isinstance(value, str):
        return None
    try:
        parsed = datetime.fromisoformat(value.replace("Z", "+00:00"))
        return parsed.timestamp() if parsed.tzinfo is not None else None
    except ValueError:
        return None


def root_trace_identity(event: dict[str, Any]) -> tuple[str, str] | None:
    if any(event.get(field) for field in ("subagent_of", "trace_parent_invocation_id", "parent_session_id")):
        return None
    # Native tool_result nests under its own tool_use_id. This parent link is
    # not a sub-agent origin; explicit sub-agent fields above remain excluded.
    if event.get("parent_id") and not (event.get("kind") == "tool_result" and event.get("parent_id") == event.get("tool_use_id")):
        return None
    session, invocation = event.get("session_id"), event.get("trace_invocation_id")
    return (session, invocation) if session and invocation else None


def exact_stream_error_trace(event: dict[str, Any]) -> bool:
    if event.get("kind") != "error" or not event.get("is_error") or not root_trace_identity(event):
        return False
    try:
        body = json.loads(event.get("text", ""))
    except (ValueError, TypeError):
        return False
    if not isinstance(body, dict) or body.get("type") != "error" or not isinstance(body.get("error"), dict):
        return False
    error = body["error"]
    return (error.get("type") in ("invalid_request_error", "api_error", "stream_error") and
            not error.get("code") and not error.get("param") and
            str(error.get("message", "")).strip().lower() in (
                "stream closed before response.completed",
                "stream disconnected before completion: stream closed before response.completed",
                "stream error: stream disconnected before completion: stream closed before response.completed"))


def require(condition: Any, message: str) -> None:
    if not condition:
        raise AssertionError(message)


def read_json(path: Path) -> Any:
    return json.loads(path.read_text(encoding="utf-8"))


def atomic_json(path: Path, value: Any) -> None:
    temporary = path.with_suffix(path.suffix + ".tmp")
    temporary.write_text(json.dumps(value, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")
    temporary.chmod(0o600)
    temporary.replace(path)


def toml_value(value: Any) -> str:
    if isinstance(value, str):
        return json.dumps(value, ensure_ascii=False)
    if isinstance(value, bool):
        return "true" if value else "false"
    if isinstance(value, (int, float)):
        return repr(value)
    if isinstance(value, list):
        return "[" + ", ".join(toml_value(item) for item in value) + "]"
    raise ValueError("Unsupported selected provider setting type: " + type(value).__name__)


def toml_table(parts: list[str], values: dict[str, Any]) -> str:
    text = ["[" + ".".join(json.dumps(part) for part in parts) + "]"]
    for key, value in values.items():
        if not isinstance(value, dict):
            text.append(json.dumps(key) + " = " + toml_value(value))
    for key, value in values.items():
        if isinstance(value, dict):
            text.extend(["", toml_table(parts + [key], value)])
    return "\n".join(text) + "\n"


def safe_endpoint(value: str) -> str:
    parsed = urlsplit(value)
    host = parsed.hostname or ""
    if ":" in host:
        host = "[" + host + "]"
    if parsed.port:
        host += ":" + str(parsed.port)
    return urlunsplit((parsed.scheme, host, parsed.path, "", ""))


def classify_creation_probe_errors(events: list[dict[str, Any]], state: dict[str, Any], creations: list[dict[str, Any]],
                                   artifact_dir: Path, validate_delivery: Callable[[str, str], None]) -> tuple[list[dict[str, Any]], list[dict[str, Any]]]:
    """Recognize one resolved lstat ENOENT probe for an explicitly declared output.

    Keep all original trace errors. A handled probe requires exact path/call
    identity, same invocation's later successful Write and actual JSON Readback,
    a completed item's delivery manifest, matching artifact bytes and an
    independent oracle. Inputs, permission errors, caches that merely say
    "unchanged", repeated missing-file probes and incomplete chains stay errors.
    """
    workspace = Path(state.get("workspace", str(artifact_dir))).resolve()
    items = {item["id"]: item for item in state.get("items", [])}

    def identity(event: dict[str, Any]) -> tuple[str, str, str] | None:
        values = tuple(event.get(key, "") for key in ("session_id", "trace_invocation_id", "trace_call_id"))
        return values if all(isinstance(value, str) and value for value in values) else None

    def scoped_path(value: Any) -> Path | None:
        if not isinstance(value, str) or not value:
            return None
        path = Path(value)
        return (path if path.is_absolute() else workspace / path).resolve()

    def readback_json(text: str) -> Any:
        lines = []
        for line in text.splitlines():
            match = re.match(r"^\s*\d+\t(.*)$", line)
            lines.append(match.group(1) if match else line)
        return json.loads("\n".join(lines))

    calls: dict[tuple[str, str, str], dict[str, Any]] = {}
    for event in events:
        key = identity(event)
        if key and event.get("kind") in ("tool_start", "tool_args"):
            try:
                arguments = json.loads(event.get("text", ""))
            except ValueError:
                continue
            if isinstance(arguments, dict):
                calls[key] = {"tool": event.get("tool_name"), "path": scoped_path(arguments.get("path") or arguments.get("file_path")),
                              "content": arguments.get("content")}
    declarations: dict[Path, dict[str, Any]] = {}
    for creation in creations:
        item = items.get(creation.get("item_id"), {})
        delivery = creation.get("delivery")
        if not isinstance(delivery, str) or Path(delivery).is_absolute() or ".." in Path(delivery).parts:
            continue
        if item.get("status") != "completed" or item.get("phase") not in ("implementation", "custom") or delivery not in item.get("deliverables", []):
            continue
        artifact = next((artifact for artifact in item.get("artifacts", []) if artifact.get("path") == delivery), None)
        if artifact:
            declarations[(workspace / delivery).resolve()] = {**creation, "artifact": artifact}
    results = [(index, event, identity(event)) for index, event in enumerate(events) if event.get("kind") == "tool_result"]
    candidates: dict[int, tuple[Path, tuple[str, str, str]]] = {}
    counts: Counter = Counter()
    for index, event, key in results:
        if not event.get("is_error") or event.get("tool_name") != "Read" or key is None:
            continue
        # Match the precise filesystem error seen in service-05, never arbitrary
        # ENOENT prose or permission failures containing a filename.
        match = re.fullmatch(r"denied: lstat (.+): no such file or directory", event.get("text", ""))
        call = calls.get(key, {})
        if match and call.get("tool") == "Read" and call.get("path") in declarations and scoped_path(match.group(1)) == call["path"]:
            path = call["path"]
            candidates[index] = path, key
            counts[(path, key[:2])] += 1
    handled = []
    handled_indexes: set[int] = set()
    validated: dict[Path, Any] = {}
    for index, (path, key) in candidates.items():
        if counts[(path, key[:2])] != 1:
            continue
        related = [(other_index, event, other_key, calls.get(other_key, {})) for other_index, event, other_key in results
                   if other_key is not None and other_key[:2] == key[:2] and calls.get(other_key, {}).get("path") == path]
        if any(other_index < index and event.get("tool_name") == "Write" and not event.get("is_error") for other_index, event, _, _ in related):
            continue
        declaration = declarations[path]
        delivery = declaration["delivery"]
        if path not in validated:
            try:
                file_path = artifact_dir / delivery
                require(file_path.is_file() and not file_path.is_symlink(), "Declared probe output is not a regular artifact")
                require(hashlib.sha256(file_path.read_bytes()).hexdigest() == declaration["artifact"]["sha256"], "Declared probe output differs from its manifest")
                validate_delivery(declaration["source"], delivery)
                validated[path] = json.loads(file_path.read_text())
            except (AssertionError, OSError, ValueError, KeyError):
                validated[path] = None
        expected = validated[path]
        if expected is None:
            continue
        chain = None
        for write_index, write, write_key, arguments in related:
            if write_index <= index or write.get("tool_name") != "Write" or write.get("is_error"):
                continue
            try:
                if json.loads(arguments.get("content", "")) != expected:
                    continue
            except (ValueError, TypeError):
                continue
            for read_index, read, read_key, _ in related:
                if read_index <= write_index or read.get("tool_name") != "Read" or read.get("is_error"):
                    continue
                try:
                    if readback_json(read.get("text", "")) == expected:
                        chain = write, write_key, read, read_key
                        break
                except (ValueError, TypeError):
                    pass
            if chain:
                break
        if chain is None:
            continue
        write, write_key, read, read_key = chain
        probe = events[index]
        handled_indexes.add(index)
        handled.append({"kind": "handled_creation_probe", "output": delivery, "item_id": declaration["item_id"],
                        "session_id": key[0], "trace_invocation_id": key[1], "probe_call_id": key[2], "probe_at": probe.get("ts"),
                        "write_call_id": write_key[2], "write_at": write.get("ts"), "readback_call_id": read_key[2], "readback_at": read.get("ts"),
                        "error": probe.get("text", ""), "oracle_passed": True, "artifact_sha256": declaration["artifact"]["sha256"]})
    unexpected = [{"tool": event.get("tool_name"), "text": event.get("text", "")[:500], "trace_invocation_id": event.get("trace_invocation_id"),
                   "trace_call_id": event.get("trace_call_id")} for index, event, _ in results if event.get("is_error") and index not in handled_indexes]
    return handled, unexpected


class LiveCheck:
    def __init__(self, args: argparse.Namespace) -> None:
        self.args = args
        self.repo = args.repo.resolve()
        self.output = args.output.resolve()
        require(not self.output.exists() or not any(self.output.iterdir()),
                "Use a new or empty --output directory; previous evidence will not be overwritten")
        self.output.mkdir(parents=True, exist_ok=True)
        self.output.chmod(0o700)
        self.root = Path(tempfile.mkdtemp(prefix="metis-coordinator-live-"))
        self.root.chmod(0o700)
        self.home = self.root / "home"
        self.workspace = self.root / "workspace"
        self.binary = self.root / "metis"
        for directory in (self.home, self.workspace, self.root / "system-home"):
            directory.mkdir(mode=0o700)
        self.processes: list[subprocess.Popen] = []
        self.handles: dict[int, tuple[Any, Any]] = {}
        self.process_logs: dict[int, tuple[Path, Path, float, str]] = {}
        self.secrets: list[str] = []
        self.environment = {key: os.environ[key] for key in ("PATH", "TMPDIR", "SHELL", "LANG", "LC_ALL") if key in os.environ}
        self.environment.update({
            "HOME": str(self.root / "system-home"), "METIS_HOME": str(self.home),
            "METIS_AUTO_MEMORY": "0", "METIS_NO_UPDATE_CHECK": "1", "METIS_RUN_CACHE": "0",
            "METIS_RUN_MAX_SECONDS": "300", "NO_PROXY": "127.0.0.1,localhost",
        })
        self.index = 0
        self.query_intervals: list[tuple[float, float]] = []
        self.started = time.monotonic()
        self.report: dict[str, Any] = {
            "passed": False, "mock": False, "started_utc": utc_now(),
            "mode": "quick" if args.quick else "soak", "requested_soak_seconds": args.soak_seconds,
            "effective_soak_seconds": 90.0 if args.quick else args.soak_seconds,
            "long_task_acceptance": False, "checks": [], "commands": [],
            "temporary_root": str(self.root), "repo": str(self.repo),
            "boundary": "One coordinator CLI process handles successive real model tasks for the soak; many provider requests, not one two-hour request. Recovery restarts coordinator processes, not an installed resident service.",
        }

    def redact(self, text: str) -> str:
        for secret in self.secrets:
            text = text.replace(secret, "[REDACTED]")
        return text

    def progress(self, phase: str, **fields: Any) -> None:
        value = {"phase": phase, "utc": utc_now(), "elapsed_seconds": round(time.monotonic() - self.started, 3), **fields}
        atomic_json(self.output / "progress.json", json.loads(self.redact(json.dumps(value, ensure_ascii=False))))

    def record(self, name: str, **fields: Any) -> None:
        value = {"name": name, "passed": True, "utc": utc_now(), **fields}
        value = json.loads(self.redact(json.dumps(value, ensure_ascii=False)))
        self.report["checks"].append(value)
        with (self.output / "progress.jsonl").open("a", encoding="utf-8") as stream:
            stream.write(json.dumps(value, ensure_ascii=False) + "\n")
        print(self.redact(json.dumps({"passed": name, **fields}, ensure_ascii=False)), flush=True)
        progress_fields = dict(fields)
        if "phase" in progress_fields:
            progress_fields["work_phase"] = progress_fields.pop("phase")
        self.progress(name, **progress_fields)

    def setup_provider(self) -> None:
        source_home = Path(os.environ.get("METIS_HOME", str(Path.home() / ".metis"))).expanduser().resolve()
        source_path = source_home / "config.toml"
        configuration = tomllib.loads(source_path.read_text(encoding="utf-8"))
        providers = configuration.get("provider", {})
        selected = providers.get("default")
        require(isinstance(selected, str) and selected, "Current config must explicitly select a provider")
        custom = providers.get("custom", {})
        is_custom = selected in custom
        settings = dict(custom[selected] if is_custom else providers.get(selected, {}))
        require(settings.get("model"), "Current selected provider must explicitly select a model")
        require(selected != "openai-codex", "OAuth providers need a separately reviewed isolated credential setup")
        transport = settings.get("transport") or {"anthropic": "anthropic_messages", "openai": "openai_chat", "gemini": "gemini_native"}.get(selected)
        require(transport, "Selected provider transport is unresolved")
        key_env = settings.get("api_key_env", "")
        environment_key = os.environ.get(key_env, "") if key_env else ""
        auth_path = source_home / ".credentials" / "auth.json"
        auth = read_json(auth_path) if auth_path.exists() else {}
        selected_auth = auth.get(selected)
        inline_key = settings.pop("api_key", "")
        for secret in (environment_key, inline_key, (selected_auth or {}).get("key", "")):
            if secret and secret not in self.secrets:
                self.secrets.append(secret)
        if environment_key:
            self.environment[key_env] = environment_key
        elif selected_auth and selected_auth.get("key"):
            credentials = self.home / ".credentials"
            credentials.mkdir(mode=0o700)
            atomic_json(credentials / "auth.json", {selected: selected_auth})
        elif inline_key:
            settings["api_key_env"] = "METIS_LIVE_PROVIDER_KEY"
            self.environment["METIS_LIVE_PROVIDER_KEY"] = inline_key
        else:
            raise AssertionError("Selected provider has no existing API-key credential; no provider/model fallback is attempted")
        provider_parts = ["provider", "custom", selected] if is_custom else ["provider", selected]
        text = toml_table(["provider"], {"default": selected}) + "\n" + toml_table(provider_parts, settings)
        text += "\n" + toml_table(["session"], {"dir": str(self.home / "sessions"), "skill_dir": str(self.home / "skills"), "max_iterations": 12})
        text += "\n" + toml_table(["tools"], {"allowed": ["Read", "Write", "Bash"]})
        text += '\n[permission]\nmode = "dontAsk"\n'
        for tool in ("Read", "Write", "Bash"):
            text += '\n[[permission.allow]]\ntool = ' + json.dumps(tool) + "\n"
        (self.home / "config.toml").write_text(text, encoding="utf-8")
        (self.home / "config.toml").chmod(0o600)
        agents = self.home / "agents"
        agents.mkdir(mode=0o700)
        (agents / "live-scoped.md").write_text(
            "---\nname: live-scoped\ntools: [Read, Write, Bash]\n---\n"
            "Complete only the JSON task explicitly named in the current work item. Use actual Read, Bash and Write tools. "
            "Work exclusively in the stated workspace, never inspect credentials or unrelated files, and preserve earlier deliveries. "
            "Report concrete file and validation evidence. Do not delegate or spawn background work.\n", encoding="utf-8")
        self.report["provider"] = selected
        self.report["model"] = settings["model"]
        self.report["transport"] = transport
        self.report["endpoint"] = safe_endpoint(settings.get("base_url", ""))
        self.report["provider_settings_fields"] = sorted(settings)
        self.report["configuration_source"] = str(source_path)
        self.report["provider_model_changed"] = False
        self.progress("configured", provider=selected, model=settings["model"], transport=transport)

    def spawn(self, arguments: list[str], label: str, *, executable: str | None = None, cwd: Path | None = None, environment: dict[str, str] | None = None) -> subprocess.Popen:
        self.index += 1
        stem = self.output / f"{self.index:04d}-{label}"
        stdout_path, stderr_path = stem.with_suffix(".stdout.log"), stem.with_suffix(".stderr.log")
        out, err = stdout_path.open("w", encoding="utf-8"), stderr_path.open("w", encoding="utf-8")
        try:
            process = subprocess.Popen([executable or str(self.binary), *arguments], cwd=cwd or self.workspace,
                                       env=environment or self.environment, stdout=out, stderr=err, start_new_session=True)
        except BaseException:
            out.close()
            err.close()
            raise
        self.processes.append(process)
        self.handles[process.pid] = (out, err)
        self.process_logs[process.pid] = (stdout_path, stderr_path, time.monotonic(), label)
        return process

    def finish(self, process: subprocess.Popen, *, expect: int | None = 0, timeout: float = 30, monitor_run: str | None = None) -> tuple[str, dict[str, Any]]:
        stdout_path, stderr_path, started, label = self.process_logs[process.pid]
        expiries: dict[str, set[str]] = {}
        last_progress = 0.0
        while process.poll() is None:
            if time.monotonic() - started > timeout:
                self.signal_group(process, signal.SIGKILL)
                process.wait(timeout=10)
                raise AssertionError(f"{label} exceeded {timeout}s; inspect {stdout_path.name} / {stderr_path.name}")
            if monitor_run:
                state = self.state_on_disk(monitor_run)
                for item in state.get("items", []):
                    if item.get("status") == "running" and item.get("lease"):
                        expiries.setdefault(item["id"], set()).add(item["lease"]["expires_at"])
            if time.monotonic() - last_progress >= 5:
                self.progress(label, pid=process.pid, invocation_elapsed_seconds=round(time.monotonic() - started, 2))
                last_progress = time.monotonic()
            time.sleep(0.15)  # Observe active work; never used to fill the soak duration.
        for handle in self.handles.pop(process.pid, ()):
            handle.close()
        for path in (stdout_path, stderr_path):
            path.write_text(self.redact(path.read_text(encoding="utf-8", errors="replace")), encoding="utf-8")
        ended = time.monotonic()
        proof = {"label": label, "pid": process.pid, "exit_code": process.returncode,
                 "elapsed_seconds": round(ended - started, 3), "stdout": stdout_path.name, "stderr": stderr_path.name}
        if expiries:
            proof["unique_lease_expiries"] = {key: len(value) for key, value in expiries.items()}
        self.report["commands"].append(proof)
        if expect is not None:
            require(process.returncode == expect, f"{label}: exit {process.returncode}, expected {expect}; inspect {stdout_path.name} / {stderr_path.name}")
        return stdout_path.read_text(encoding="utf-8"), proof

    def command(self, arguments: list[str], label: str, *, expect: int | None = 0, timeout: float = 30, monitor_run: str | None = None) -> tuple[str, dict[str, Any]]:
        return self.finish(self.spawn(arguments, label), expect=expect, timeout=timeout, monitor_run=monitor_run)

    def coord(self, *arguments: str, label: str, expect: int | None = 0, timeout: float = 30, monitor: bool = False) -> tuple[Any, dict[str, Any]]:
        text, proof = self.command(["coordinator", *arguments, "--cwd", str(self.workspace), "--json"], label,
                                   expect=expect, timeout=timeout, monitor_run=arguments[1] if monitor else None)
        return json.loads(text) if text.strip() else None, proof

    def state_on_disk(self, run_id: str) -> dict[str, Any]:
        for path in (self.home / "project-coordinator").glob("*/" + run_id + ".json"):
            try:
                return read_json(path)
            except (OSError, ValueError):
                continue
        return {}

    def build(self) -> None:
        self.progress("compiling")
        go = shutil.which("go")
        require(go, "Go is not on PATH")
        commit = subprocess.check_output(["git", "rev-parse", "HEAD"], cwd=self.repo, text=True).strip()
        version = (self.repo / "VERSION").read_text().strip()
        built = datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")
        flags = "-s -w " + " ".join(f"-X github.com/Ricardo-M-L/metis/internal/version.{key}={value}" for key, value in (("Version", version), ("Commit", commit[:12]), ("Date", built)))
        # Use the caller's compiler environment, isolated runtime credentials are
        # not given to the compiler. No install target or version bump is used.
        process = self.spawn(["build", "-trimpath", "-ldflags", flags, "-o", str(self.binary), "./cmd/metis"],
                             "go-build", executable=go, cwd=self.repo, environment=dict(os.environ))
        self.finish(process, timeout=300)
        self.binary.chmod(0o700)
        self.report["build"] = {"version": version, "commit": commit, "built_utc": built, "go": go,
                                "sha256": hashlib.sha256(self.binary.read_bytes()).hexdigest(), "binary": str(self.binary)}
        atomic_json(self.output / "binary-manifest.json", self.report["build"])
        self.record("isolated fresh CLI compiled", version=version, commit=commit)

    def host_complete(self, run_id: str, *, worker: str = "host-preparation", evidence: str = "Host inspected the isolated randomized input and documented the output schema.") -> dict[str, Any]:
        result, _ = self.coord("claim", run_id, "--worker", worker, "--lease", "15m", label="host-claim")
        item = result["item"]
        self.coord("complete", run_id, item["id"], "--worker", worker, "--lease-token", item["lease"]["token"], "--output", evidence, label="host-complete")
        return item

    @staticmethod
    def protected_item(item: dict[str, Any]) -> dict[str, Any]:
        return {key: item.get(key) for key in ("id", "status", "owner", "attempts", "lease", "output", "artifacts", "acceptance", "failure", "spent_usd", "usage")}

    def assert_stale_fencing(self, run_id: str, old: dict[str, Any], replacement: dict[str, Any], *, live: bool = False) -> None:
        require(old["lease"]["token"] != replacement["lease"]["token"], "Replacement reused an old lease token")
        before = self.protected_item(replacement)
        common = [run_id, old["id"], "--worker", old["lease"]["worker"], "--lease-token", old["lease"]["token"]]
        for operation, extra in (("complete", ["--output", "STALE_RESULT_MUST_NOT_BE_RECORDED"]),
                                 ("renew", ["--lease", "15m"]),
                                 ("fail", ["--summary", "STALE_FAILURE_MUST_NOT_BE_RECORDED"])):
            _, proof = self.command(["coordinator", operation, *common, *extra, "--cwd", str(self.workspace), "--json"], "reject-stale-" + operation, expect=None)
            require(proof["exit_code"] != 0, "Old token unexpectedly accepted for " + operation)
            state, _ = self.coord("status", run_id, label="fencing-state")
            current = next(item for item in state["items"] if item["id"] == old["id"])
            if live:
                # The replacement's legitimate model work and lease renewals
                # continue while these independent CLI writes are rejected.
                require(current["attempts"] == replacement["attempts"], operation + " changed replacement attempts")
                if current["status"] == "running":
                    require(current.get("owner") == replacement.get("owner") and current.get("lease", {}).get("token") == replacement["lease"]["token"], operation + " changed replacement ownership")
                require("STALE_RESULT_MUST_NOT_BE_RECORDED" not in current.get("output", "") and
                        "STALE_FAILURE_MUST_NOT_BE_RECORDED" not in json.dumps(current.get("failure")), operation + " polluted replacement evidence")
            else:
                require(self.protected_item(current) == before, operation + " polluted the replacement claim")

    def contention(self) -> None:
        run, _ = self.coord("create", "--goal", "Cross-process claim contention; no model call needed.", label="create-contention")
        gate = self.root / "claim-gate"
        # Each Python wrapper is an independent process that execs the real CLI
        # immediately after the shared gate opens. The barrier avoids serial
        # claim calls being mistaken for simultaneous contention.
        wrapper = "import os,sys,time; from pathlib import Path; p=Path(sys.argv[1]); deadline=time.monotonic()+20\nwhile not p.exists():\n if time.monotonic()>deadline: raise SystemExit(70)\n time.sleep(0.005)\nos.execve(sys.argv[2],sys.argv[2:],dict(os.environ))"
        children = []
        for number in range(8):
            arguments = ["-c", wrapper, str(gate), str(self.binary), "coordinator", "claim", run["id"],
                         "--worker", "contender-" + str(number), "--lease", "10s", "--cwd", str(self.workspace), "--json"]
            children.append(self.spawn(arguments, "contender-" + str(number), executable=sys.executable))
        gate.touch()
        results = [self.finish(child, expect=None) for child in children]
        winners = [json.loads(text)["item"] for text, proof in results if proof["exit_code"] == 0]
        require(len(winners) == 1, f"Eight processes produced {len(winners)} successful claims, expected exactly one")
        old = winners[0]
        require(old["id"] == "research" and old["attempts"] == 1, "Contention advanced or duplicated the same pending item")
        # Reduce the winning lease now that contention is over, then wait only
        # for that active claim to expire. This is a fault boundary, not soak.
        renewed, _ = self.coord("renew", run["id"], old["id"], "--worker", old["owner"], "--lease-token", old["lease"]["token"], "--lease", "1s", label="shorten-contention-lease")
        deadline = datetime.fromisoformat(renewed["lease"]["expires_at"].replace("Z", "+00:00")).timestamp()
        while time.time() <= deadline + 0.1:
            time.sleep(0.05)
        result, _ = self.coord("claim", run["id"], "--worker", old["owner"], "--lease", "15m", label="same-name-replacement")
        replacement = result["item"]
        require(replacement["attempts"] == 2, "Expired claim was not replaced exactly once")
        self.assert_stale_fencing(run["id"], old, replacement)
        self.record("eight independent processes claim one node; old same-name tokens fenced", run_id=run["id"], processes=8, successful_claims=1)

    def trace_snapshot(self) -> dict[str, int]:
        return {str(path): path.stat().st_size for path in (self.home / "traces").glob("*.jsonl")}

    def trace_events(self, snapshot: dict[str, int]) -> list[dict[str, Any]]:
        events = []
        for path in (self.home / "traces").glob("*.jsonl"):
            try:
                with path.open("rb") as stream:
                    stream.seek(snapshot.get(str(path), 0))
                    lines = stream.read().splitlines()
                for line in lines:
                    try:
                        events.append(json.loads(line))
                    except ValueError:
                        pass  # A running process may not have flushed its tail.
            except OSError:
                continue
        return events

    @staticmethod
    def trace_evidence(events: list[dict[str, Any]]) -> dict[str, Any]:
        tokens = Counter()
        responses = 0
        tools = Counter()
        errors = []
        sessions = set()
        invocations = set()
        for event in events:
            sessions.add(event.get("session_id", ""))
            if event.get("trace_invocation_id"):
                invocations.add(event["trace_invocation_id"])
            if event.get("kind") == "tokens":
                responses += 1
                for name, value in re.findall(r"(input|output|cache_write|cache_read)=(\d+)", event.get("text", "")):
                    tokens[name] += int(value)
            if event.get("kind") == "tool_result":
                tools[event.get("tool_name", "")] += 1
                if event.get("is_error"):
                    errors.append({"tool": event.get("tool_name"), "text": event.get("text", "")[:500]})
        return {"model_responses": responses, "tokens": dict(tokens), "tool_results": dict(tools), "tool_errors": errors,
                "session_ids": sorted(sessions - {""}), "invocation_ids": sorted(invocations)}

    def checked_tool_evidence(self, events: list[dict[str, Any]], state: dict[str, Any], creations: list[dict[str, Any]]) -> dict[str, Any]:
        evidence = self.trace_evidence(events)
        handled, unexpected = classify_creation_probe_errors(events, state, creations, self.workspace, self.validate_delivery)
        evidence["handled_creation_probes"] = handled
        evidence["unexpected_tool_errors"] = unexpected
        return evidence

    def guard_work_states(self, items: dict[str, dict[str, Any]], identifiers: set[str], label: str,
                          requeue_wait: dict[str, float]) -> None:
        """Fail promptly on terminal work, allowing a bounded requeue transition."""
        now = time.monotonic()
        for identifier in identifiers:
            item = items.get(identifier, {})
            status = item.get("status")
            if status not in ("failed", "blocked", "cancelled"):
                requeue_wait.pop(identifier, None)
                continue
            failure = item.get("failure") or {}
            remaining = item.get("attempts", 0) < item.get("max_attempts", 2)
            recoverable = bool(failure.get("recoverable")) and remaining
            if status == "blocked":
                failed_dependencies = [items.get(dependency, {}) for dependency in item.get("depends_on", [])
                                       if items.get(dependency, {}).get("status") in ("failed", "blocked")]
                recoverable = bool(failed_dependencies) and all(
                    (dependency.get("failure") or {}).get("recoverable") and
                    dependency.get("attempts", 0) < dependency.get("max_attempts", 2)
                    for dependency in failed_dependencies)
            if status != "cancelled" and recoverable:
                started = requeue_wait.setdefault(identifier, now)
                require(now - started <= 5, f"{label}: recoverable {identifier} did not requeue within 5 seconds")
                continue
            raise AssertionError(f"{label}: terminal {status} node {identifier}, phase={item.get('phase')}, "
                                 f"code={failure.get('code')}, attempts={item.get('attempts')}/{item.get('max_attempts')}")

    def capture_stream_retry(self, state: dict[str, Any], item: dict[str, Any], observed: dict[str, dict[str, Any]],
                             snapshot: dict[str, int]) -> None:
        """Accept attempt 2 only with bounded, durable automatic SSE recovery proof."""
        identifier = item["id"]
        attempts = item.get("attempts", 0)
        if attempts <= 1:
            return
        require(attempts == 2 and item.get("max_attempts") == 2, "Automatic stream retry exceeded the two-attempt contract: " + identifier)
        require(not item.get("forced_recoveries"), "Forced retry is not automatic stream recovery: " + identifier)
        usage = item.get("usage", [])
        tokens = [entry.get("token") for entry in usage]
        require(len(tokens) == attempts and all(tokens) and len(set(tokens)) == attempts,
                "Retry has no distinct durable attempt tokens: " + identifier)
        token_hashes = [hashlib.sha256(token.encode()).hexdigest() for token in tokens]
        if identifier in observed:
            require(observed[identifier]["claim_token_sha256"] == token_hashes,
                    "Recorded retry claim tokens changed unexpectedly: " + identifier)
            return
        failure = item.get("failure") or {}
        proof_source = "persisted recoverable failure on retrying node"
        invocation_proof: dict[str, Any] = {}
        if failure:
            require(failure.get("code") == "provider_stream_interrupted" and failure.get("recoverable") and failure.get("auto_recovered"),
                    "Unexplained automatic retry: " + identifier + " / " + str(failure.get("code")))
            cause = failure.get("summary", "")
        else:
            # Completion clears Failure. Preserve evidence while the bounded
            # event journal still contains this freshly completed retry.
            journal = [event for event in state.get("events", []) if event.get("item_id") == identifier]
            claims = [event for event in journal if event.get("kind") == "work_claimed"]
            recoveries = [event for event in journal if event.get("kind") == "work_auto_recovered"]
            completions = [event for event in journal if event.get("kind") == "work_completed"]
            if (item.get("status") != "completed" or len(claims) != 2 or len(recoveries) != 1 or len(completions) != 1 or
                    any(event.get("kind") in ("work_requeued", "worker_lease_expired", "work_failed") for event in journal)):
                return
            recovery = recoveries[0]
            # Event schema currently stores Summary, not a failure code. Require
            # the fixed provider summary plus exact native error; never infer a
            # provider retry merely from generic work_auto_recovered presence.
            if recovery.get("code") not in (None, "", "provider_stream_interrupted") or recovery.get("detail") != "Provider stream closed before its completion marker was received.":
                return
            times = [trace_timestamp(event.get("at")) for event in (claims[0], recovery, claims[1], completions[0])]
            if any(value is None for value in times) or not times[0] <= times[1] <= times[2] <= times[3]:
                return
            workers = [claims[0].get("worker"), recovery.get("worker"), claims[1].get("worker"), completions[0].get("worker")]
            if not workers[0] or len(set(workers)) != 1 or any(entry.get("worker") != workers[0] for entry in usage):
                return
            # This fallback is confined to the harness's one sequential worker.
            # A competing item claim during this interval makes inference unsafe.
            if any(event.get("kind") == "work_claimed" and event.get("item_id") != identifier and
                   trace_timestamp(event.get("at")) is not None and times[0] <= trace_timestamp(event.get("at")) <= times[3]
                   for event in state.get("events", [])):
                return
            case_events = self.trace_events(snapshot)
            errors_in_window = [event for event in case_events if exact_stream_error_trace(event) and
                                trace_timestamp(event.get("ts")) is not None and times[0] <= trace_timestamp(event.get("ts")) <= times[1]]
            failed_identities = {root_trace_identity(event) for event in errors_in_window}
            if len(failed_identities) != 1 or len(errors_in_window) != 1:
                return
            failed_identity = next(iter(failed_identities))
            successes = [event for event in case_events if root_trace_identity(event) and event.get("kind") == "loop_done" and
                         event.get("text") == "end_turn" and trace_timestamp(event.get("ts")) is not None and
                         times[2] <= trace_timestamp(event.get("ts")) <= times[3]]
            retry_identities = {root_trace_identity(event) for event in successes}
            if len(retry_identities) != 1:
                return
            retry_identity = next(iter(retry_identities))
            if retry_identity == failed_identity:
                return
            retry_events = [event for event in case_events if root_trace_identity(event) == retry_identity]
            if any(event.get("kind") == "error" or trace_timestamp(event.get("ts")) is None or
                   not times[2] <= trace_timestamp(event.get("ts")) <= times[3] for event in retry_events):
                return
            cause = recovery.get("detail", "")
            proof_source = "persisted provider recovery summary and exact native SSE error bound to sequential claim invocations"
            invocation_proof = {"failed_session_id": failed_identity[0], "failed_invocation_id": failed_identity[1],
                                "retry_session_id": retry_identity[0], "retry_invocation_id": retry_identity[1],
                                "native_error_trace_id": errors_in_window[0].get("id")}
        observed[identifier] = {"item_id": identifier, "phase": item.get("phase"), "attempts": attempts,
                                "max_attempts": item["max_attempts"], "cause_code": "provider_stream_interrupted",
                                "cause": self.redact(cause), "proof_source": proof_source,
                                "claim_token_sha256": token_hashes, **invocation_proof}
        self.record("bounded automatic stream recovery observed", **observed[identifier])

    def generate_input(self, stem: str, batch: int) -> tuple[str, str]:
        source, delivery = stem + "-input.json", stem + "-output.json"
        rng = random.SystemRandom()
        data = {"marker": uuid.uuid4().hex, "batch": batch,
                "records": [{"group": rng.choice(("alpha", "beta", "gamma", "delta")), "value": rng.randint(-10000, 10000)} for _ in range(192)]}
        atomic_json(self.workspace / source, data)
        return source, delivery

    def prompt(self, source: str, delivery: str) -> str:
        return (f"Use Read to inspect {source}. Use Bash with Python to compute a summary from that file, then use Write to create {delivery}. "
                "Output JSON must contain exactly marker, batch, total_count, total_sum, groups. Copy marker and batch from the input. "
                "total_count counts records and total_sum sums every value. groups maps each present group to exactly count and sum. "
                "You may use a Python dictionary aggregation; do not calculate hundreds of values mentally. "
                f"Read back {delivery} and verify its JSON. Work only on these named files in this workspace. "
                "Preserve every earlier delivery, do not read other directories, do not spawn agents or background commands, and report concrete evidence.")

    def write_checker(self) -> None:
        # Independent implementation: sort/group rather than the model's
        # suggested dictionary accumulator. Expected values stay on the host.
        text = '''import itertools,json,sys
from pathlib import Path
def check(source,delivery):
    data=json.loads(source.read_text())
    got=json.loads(delivery.read_text())
    groups={}
    for key,rows in itertools.groupby(sorted(data["records"],key=lambda x:x["group"]),key=lambda x:x["group"]):
        values=[row["value"] for row in rows]
        groups[key]={"count":len(values),"sum":sum(values)}
    expected={"marker":data["marker"],"batch":data["batch"],"total_count":len(data["records"]),"total_sum":sum(row["value"] for row in data["records"]),"groups":groups}
    if got!=expected: raise SystemExit("Output does not match independently recalculated input: "+str(delivery))
if len(sys.argv)==3:
    check(*map(Path,sys.argv[1:3]))
else:
    deliveries=sorted(Path('.').glob('*-output.json'))
    if not deliveries: raise SystemExit("No produced outputs to validate")
    for delivery in deliveries:
        check(delivery.with_name(delivery.name.replace('-output.json','-input.json')),delivery)
print("PASS: markers, batches, counts, signed sums and every produced group match")
'''
        (self.workspace / "host-check.py").write_text(text, encoding="utf-8")

    def check_command(self, source: str, delivery: str) -> str:
        return " ".join(shlex.quote(value) for value in (sys.executable, "host-check.py", source, delivery))

    def validate_delivery(self, source: str, delivery: str) -> None:
        result = subprocess.run([sys.executable, "host-check.py", source, delivery], cwd=self.workspace, capture_output=True, text=True, timeout=20)
        require(result.returncode == 0, "Host independently rejected delivery " + delivery + ": " + result.stderr.strip())

    def signal_group(self, process: subprocess.Popen, signum: int) -> None:
        try:
            os.killpg(process.pid, signum)
        except ProcessLookupError:
            pass

    def create_recovery_run(self, label: str) -> tuple[dict[str, Any], str, str]:
        source, delivery = self.generate_input(label, 0)
        run, _ = self.coord("create", "--goal", self.prompt(source, delivery), "--check", self.check_command(source, delivery), label="create-" + label)
        self.host_complete(run["id"])
        self.host_complete(run["id"], evidence="Plan: perform actual Read, Bash aggregation, Write output, then independent verification. Host has not created the output.")
        return run, source, delivery

    def observe_model_work(self, process: subprocess.Popen, run_id: str, snapshot: dict[str, int]) -> tuple[dict[str, Any], dict[str, Any]]:
        started = time.monotonic()
        while time.monotonic() - started < 180:
            require(process.poll() is None, "Worker ended before the fault could be injected")
            state = self.state_on_disk(run_id)
            running = [item for item in state.get("items", []) if item.get("status") == "running" and item.get("lease")]
            evidence = self.trace_evidence(self.trace_events(snapshot))
            if running and evidence["model_responses"] > 0 and evidence["tool_results"].get("Read", 0) > 0:
                return running[0], evidence
            self.progress("waiting-for-real-model-work", pid=process.pid, observed_model_responses=evidence["model_responses"])
            time.sleep(0.1)
        raise AssertionError("No real model tokens and Read result observed before fault injection")

    def restart_recovery(self, signum: int, label: str) -> dict[str, Any]:
        run, source, delivery = self.create_recovery_run(label)
        worker = "same-name-" + label
        snapshot = self.trace_snapshot()
        process = self.spawn(["coordinator", "run", run["id"], "--worker", worker, "--lease", "2s", "--until-idle", "--cwd", str(self.workspace), "--json"], label + "-interrupted-worker")
        old, interrupted_evidence = self.observe_model_work(process, run["id"], snapshot)
        killed_at = time.monotonic()
        self.signal_group(process, signum)
        _, stopped = self.finish(process, expect=None, timeout=20)
        if signum == signal.SIGKILL:
            require(stopped["exit_code"] == -signal.SIGKILL, "SIGKILL did not stop the owned CLI")
        else:
            require(stopped["exit_code"] != 0, "SIGTERM masked interrupted work as shell success")
        # Poll actual persisted lease expiry, without editing state or using
        # coordinator recover. A graceful stop may requeue immediately.
        expiry = datetime.fromisoformat(old["lease"]["expires_at"].replace("Z", "+00:00")).timestamp()
        while time.time() <= expiry + 0.15:
            time.sleep(0.05)
        state, _ = self.coord("status", run["id"], label=label + "-post-stop-state")
        old_after = next(item for item in state["items"] if item["id"] == old["id"])
        require(old_after["status"] in ("ready", "pending"), f"{label} left {old['id']} {old_after['status']} ({(old_after.get('failure') or {}).get('code')}); automatic restart cannot claim it")
        query_start = time.monotonic()
        snapshot = self.trace_snapshot()
        resumed = self.spawn(["coordinator", "run", run["id"], "--worker", worker, "--lease", "2s", "--until-idle", "--cwd", str(self.workspace), "--json"], label + "-managed-restart-query")
        replacement, _ = self.observe_model_work(resumed, run["id"], snapshot)
        require(replacement["attempts"] == 2 and replacement["id"] == old["id"], "Restart did not reclaim the interrupted node exactly once")
        self.assert_stale_fencing(run["id"], old, replacement, live=True)
        text, proof = self.finish(resumed, timeout=720, monitor_run=run["id"])
        completed = json.loads(text)
        query_end = time.monotonic()
        self.query_intervals.append((query_start, query_end))
        evidence = self.trace_evidence(self.trace_events(snapshot))
        self.assert_completed(completed["run"], source, delivery, evidence, implementation_id="implementation", expected_attempts=2)
        require(all(item["attempts"] == 1 for item in completed["run"]["items"] if item["id"] != old["id"]), "Restart reran already completed preparation or verification")
        self.record(label + " interrupted real model work, same-name restart and stale-token rejection", run_id=run["id"],
                    signal=signal.Signals(signum).name, interrupted=interrupted_evidence, resumed=evidence,
                    interruption_to_completion_seconds=round(query_end - killed_at, 3), stop_exit=stopped["exit_code"], verification=proof)
        return completed["run"]

    def assert_completed(self, state: dict[str, Any], source: str, delivery: str, evidence: dict[str, Any], *, implementation_id: str,
                         expected_attempts: int = 1, expected_verification_attempts: int = 1) -> None:
        require(state["status"] == "completed", "Real worker did not complete the project: " + state["status"])
        require(evidence["model_responses"] > 0 and sum(evidence["tokens"].values()) > 0, "No real model response/token evidence")
        require(all(evidence["tool_results"].get(tool, 0) > 0 for tool in ("Read", "Write", "Bash")), "Missing actual Read / Write / Bash tool-result evidence")
        if "unexpected_tool_errors" not in evidence and evidence["tool_errors"]:
            invocation_ids = set(evidence.get("invocation_ids", []))
            scoped_events = [event for event in self.trace_events({}) if event.get("trace_invocation_id") in invocation_ids]
            checked = self.checked_tool_evidence(scoped_events, state, [{"item_id": implementation_id, "source": source, "delivery": delivery}])
            # Missing trace identities must never hide an original tool error.
            if checked["tool_errors"] == evidence["tool_errors"]:
                evidence["handled_creation_probes"] = checked["handled_creation_probes"]
                evidence["unexpected_tool_errors"] = checked["unexpected_tool_errors"]
        unexpected = evidence.get("unexpected_tool_errors", evidence["tool_errors"])
        require(not unexpected, "Unexpected model tool errors: " + json.dumps(unexpected))
        self.validate_delivery(source, delivery)
        implementation = next(item for item in state["items"] if item["id"] == implementation_id)
        require(implementation["status"] == "completed" and implementation["attempts"] == expected_attempts, "Unexpected implementation status/attempt count")
        verifications = [item for item in state["items"] if item["phase"] == "verification" and implementation_id in item.get("depends_on", [])]
        require(len(verifications) == 1, "Missing unique dependent verification")
        require(verifications[0]["attempts"] == expected_verification_attempts, "Unexpected verification attempt count")
        acceptance = verifications[0].get("acceptance", {})
        require(acceptance.get("status") == "passed" and acceptance.get("checks") and all(check["exit_code"] == 0 for check in acceptance["checks"]), "Host acceptance did not pass")
        if implementation.get("deliverables"):
            artifact = next((artifact for artifact in implementation.get("artifacts", []) if artifact["path"] == delivery), None)
            require(artifact and artifact["sha256"] == hashlib.sha256((self.workspace / delivery).read_bytes()).hexdigest(), "Deliverable manifest hash is wrong")

    def soak(self, state: dict[str, Any]) -> None:
        target = self.report["effective_soak_seconds"]
        submitted: dict[int, dict[str, Any]] = {}
        completed: set[int] = set()
        cumulative_spend = state.get("spent_usd", 0)
        initial_spend = cumulative_spend
        run_id = state["id"]

        def enqueue() -> None:
            number = len(submitted) + 1
            source, delivery = self.generate_input(f"batch-{number:05d}", number)
            # Keep the same default runtime for both implementation and
            # verification; changing role profiles would recreate the runtime.
            arguments = ["event", run_id, "--source", "live-soak", "--id", f"batch-{number:05d}",
                         "--subject", "Aggregate randomized batch " + str(number), "--prompt", self.prompt(source, delivery), "--deliverable", delivery]
            event, _ = self.coord(*arguments, label="enqueue-batch")
            replay, _ = self.coord(*arguments, label="deduplicate-batch")
            require(event["id"] == replay["id"], "Replayed batch created duplicate work")
            submitted[number] = {"source": source, "delivery": delivery, "item_id": event["id"], "enqueued_monotonic": time.monotonic()}

        for _ in range(3):
            enqueue()
        global_check = shlex.quote(sys.executable) + " host-check.py"
        self.coord("configure", run_id, "--check", global_check, label="configure-soak-acceptance")
        snapshot = self.trace_snapshot()
        process = self.spawn(["coordinator", "run", run_id, "--worker", "continuous-live-worker", "--lease", "2s", "--until-idle", "--cwd", str(self.workspace), "--json"], "single-process-real-soak")
        soak_start = self.process_logs[process.pid][2]
        last_completion = soak_start
        completion_gaps: list[float] = []
        expiries: dict[str, set[str]] = {}
        automatic_retries: dict[str, dict[str, Any]] = {}
        requeue_wait: dict[str, float] = {}
        last_progress = 0.0
        while process.poll() is None:
            now = time.monotonic()
            require(now - last_completion < 900, "No accepted batch progress for 900 seconds despite queued work")
            state = self.state_on_disk(run_id)
            items = {item["id"]: item for item in state.get("items", [])}
            require(state.get("status") != "cancelled", "Continuous worker was cancelled")
            target_ids = {identifier for batch in submitted.values() for identifier in (batch["item_id"], batch["item_id"] + "-verify")}
            self.guard_work_states(items, target_ids, "continuous soak", requeue_wait)
            for identifier in target_ids:
                if identifier in items:
                    self.capture_stream_retry(state, items[identifier], automatic_retries, snapshot)
            for item in items.values():
                if item.get("status") == "running" and item.get("lease"):
                    expiries.setdefault(item["id"], set()).add(item["lease"]["expires_at"])
            for number, batch in submitted.items():
                if number in completed:
                    continue
                implementation = items.get(batch["item_id"], {})
                verification = items.get(batch["item_id"] + "-verify", {})
                if implementation.get("status") == "completed" and verification.get("status") == "completed":
                    self.validate_delivery(batch["source"], batch["delivery"])
                    acceptance = verification.get("acceptance", {})
                    require(acceptance.get("status") == "passed" and acceptance.get("checks") and all(check["exit_code"] == 0 for check in acceptance["checks"]), "Batch host verification did not pass")
                    for item in (implementation, verification):
                        require(item.get("attempts") == 1 or item["id"] in automatic_retries,
                                "Soak retry lacks saved automatic recovery evidence: " + item["id"])
                    completed_at = time.monotonic()
                    completion_gaps.append(completed_at - last_completion)
                    last_completion = completed_at
                    batch["completed_monotonic"] = completed_at
                    completed.add(number)
                    self.record("continuous randomized batch independently verified", batch=number, worker_pid=process.pid,
                                delivery=batch["delivery"], elapsed_soak_seconds=round(completed_at - soak_start, 3),
                                estimated_spend_usd=state.get("spent_usd", 0),
                                crossed_duration_target=batch["enqueued_monotonic"] <= soak_start + target <= completed_at)
            require(state.get("spent_usd", 0) >= cumulative_spend, "Continuous cumulative estimated usage decreased")
            cumulative_spend = state.get("spent_usd", 0)
            if now - soak_start < target:
                while len(submitted) - len(completed) < 3:
                    require(process.poll() is None, "The coordinator drained before the next batch could be queued")
                    enqueue()
            if now - last_progress >= 5:
                self.progress("continuous-real-soak", worker_pid=process.pid, elapsed_soak_seconds=round(now - soak_start, 3),
                              batches_submitted=len(submitted), batches_verified=len(completed), pending_batches=len(submitted) - len(completed),
                              admitting_new_work=now - soak_start < target, estimated_spend_usd=cumulative_spend)
                last_progress = now
            time.sleep(0.15)  # Observe actual useful work and refill its queue.
        text, command_proof = self.finish(process, timeout=target + 1200)
        soak_end = time.monotonic()
        state = json.loads(text)["run"]
        evidence = self.checked_tool_evidence(self.trace_events(snapshot), state, list(submitted.values()))
        require(len(completed) == len(submitted) or state["status"] == "completed", "The process stopped with outstanding submitted batches")
        events = self.trace_events(snapshot)
        native_results = {event.get("trace_call_id") or event.get("tool_use_id"): event for event in events if event.get("kind") == "tool_result" and not event.get("is_error")}
        calls: dict[str, set[str]] = {"Read": set(), "Write": set()}
        for event in events:
            if event.get("kind") != "tool_start" or event.get("tool_name") not in calls:
                continue
            identity = event.get("trace_call_id") or event.get("tool_use_id")
            if identity not in native_results:
                continue
            try:
                arguments = json.loads(event.get("text", ""))
            except ValueError:
                continue
            path = arguments.get("path") or arguments.get("file_path")
            if path:
                calls[event["tool_name"]].add(Path(path).name)
        for number, batch in submitted.items():
            implementation = next(item for item in state["items"] if item["id"] == batch["item_id"])
            verification = next(item for item in state["items"] if item["id"] == batch["item_id"] + "-verify")
            for item in (implementation, verification):
                self.capture_stream_retry(state, item, automatic_retries, snapshot)
                require(item["attempts"] == 1 or item["id"] in automatic_retries, "Unverified final soak retry: " + item["id"])
            self.assert_completed(state, batch["source"], batch["delivery"], evidence, implementation_id=batch["item_id"],
                                  expected_attempts=implementation["attempts"], expected_verification_attempts=verification["attempts"])
            require(batch["source"] in calls["Read"] and batch["delivery"] in calls["Write"], "Batch has no successful native Read / Write evidence: " + str(number))
        # Fresh CLI status loads the durable graph after the one long-lived
        # model worker has exited; cumulative usage must be identical.
        persisted, _ = self.coord("status", run_id, label="post-soak-new-process-status")
        require(persisted.get("spent_usd", 0) == state.get("spent_usd", 0), "Usage did not survive the long worker exit and a separate CLI reload")
        elapsed = soak_end - soak_start
        crossed = any(batch["enqueued_monotonic"] <= soak_start + target <= batch.get("completed_monotonic", soak_end) for batch in submitted.values())
        self.query_intervals.append((soak_start, soak_end))
        self.report["soak"] = {"seconds": round(elapsed, 3), "batches": len(submitted), "run_id": run_id,
                               "worker_processes": 1, "worker_pid": process.pid, "runtime_role_profile": "default for every item",
                               "cumulative_cli_query_wall_seconds": round(command_proof["elapsed_seconds"], 3),
                               "longest_cli_invocation_seconds": round(command_proof["elapsed_seconds"], 3),
                               "max_batch_completion_gap_seconds": round(max(completion_gaps, default=elapsed), 3),
                               "estimated_spend_usd": state.get("spent_usd", 0), "initial_estimated_spend_usd": initial_spend,
                               "batch_crossed_duration_target": crossed, "idle_wait_used_to_fill_duration": False,
                               "automatic_stream_recoveries": list(automatic_retries.values()),
                               "unique_lease_expiries": {key: len(value) for key, value in expiries.items()}, "evidence": evidence,
                               "command": command_proof}
        require(elapsed >= target and crossed, "No useful batch crossed the requested active-work duration")
        require(max((len(value) for value in expiries.values()), default=0) >= 3, "Live lease renewals not observed during the soak")
        self.report["long_task_acceptance"] = not self.args.quick and target >= 7200 and elapsed >= 7200
        self.record("continuous real-query workflow finished", **self.report["soak"], long_task_acceptance=self.report["long_task_acceptance"])

    def cleanup(self) -> None:
        # Cleanup comes before evidence copying/report finalization. A failure
        # never leaves a runnable test binary available for a source fix.
        for process in self.processes:
            if process.poll() is None:
                self.signal_group(process, signal.SIGKILL)
        for process in self.processes:
            if process.poll() is None:
                try:
                    process.wait(timeout=10)
                except subprocess.TimeoutExpired:
                    pass
        for handles in self.handles.values():
            for handle in handles:
                handle.close()
        self.handles.clear()
        self.binary.unlink(missing_ok=True)
        shutil.rmtree(self.home / ".credentials", ignore_errors=True)
        for key in list(self.environment):
            if self.environment[key] in self.secrets:
                self.environment.pop(key)
        self.report["cleanup"] = {"binary_destroyed": not self.binary.exists(), "credential_copy_removed": not (self.home / ".credentials").exists(),
                                  "owned_children_stopped": all(process.poll() is not None for process in self.processes), "finished_utc": utc_now()}
        require(all(self.report["cleanup"][key] for key in ("binary_destroyed", "credential_copy_removed", "owned_children_stopped")), "Test CLI/credential/process cleanup incomplete")

    def copy_evidence(self) -> None:
        copied = []
        for directory, target in ((self.workspace, "workspace"), (self.home / "project-coordinator", "project-coordinator"),
                                  (self.home / "sessions", "sessions"), (self.home / "traces", "traces")):
            if not directory.exists():
                continue
            for source in directory.rglob("*"):
                if not source.is_file() or source.is_symlink():
                    continue
                require(source.stat().st_size <= 32 * 1024 * 1024, "Unexpectedly large evidence file: " + str(source))
                destination = self.output / "evidence" / target / source.relative_to(directory)
                destination.parent.mkdir(parents=True, exist_ok=True)
                destination.write_text(self.redact(source.read_text(encoding="utf-8", errors="replace")), encoding="utf-8")
                destination.chmod(0o600)
                copied.append({"path": str(destination.relative_to(self.output)), "sha256": hashlib.sha256(destination.read_bytes()).hexdigest(), "bytes": destination.stat().st_size})
        # Runtime logs stay private and are redacted before final delivery.
        for path in list(self.output.glob("*.log")):
            path.write_text(self.redact(path.read_text(encoding="utf-8", errors="replace")), encoding="utf-8")
        atomic_json(self.output / "evidence-manifest.json", copied)

    def run(self) -> int:
        try:
            self.setup_provider()
            self.build()
            self.write_checker()
            self.contention()
            self.restart_recovery(signal.SIGKILL, "sigkill")
            state = self.restart_recovery(signal.SIGTERM, "sigterm")
            self.soak(state)
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
            self.progress("finished", passed=self.report["passed"], long_task_acceptance=self.report["long_task_acceptance"], failure=self.report.get("failure"), cleanup=self.report.get("cleanup"))
        print(json.dumps({"passed": self.report["passed"], "long_task_acceptance": self.report["long_task_acceptance"],
                          "checks": len(self.report["checks"]), "report": str(self.output / "verification-report.json"), "failure": self.report.get("failure"), "cleanup": self.report.get("cleanup")}, ensure_ascii=False), flush=True)
        return 0 if self.report["passed"] else 1


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("--repo", type=Path, default=Path(__file__).resolve().parents[2])
    parser.add_argument("--output", type=Path, required=True, help="new or empty private evidence directory")
    parser.add_argument("--soak-seconds", type=float, default=7200, help="continuous useful work duration, default 7200 (two hours)")
    parser.add_argument("--quick", action="store_true", help="development pass: 90-second workflow after recovery tests; does not meet long-task acceptance")
    args = parser.parse_args()
    require(args.soak_seconds > 0, "--soak-seconds must be positive")
    os.umask(0o077)

    def interrupted(signum: int, _frame: Any) -> None:
        raise KeyboardInterrupt("Harness interrupted by " + signal.Signals(signum).name)

    signal.signal(signal.SIGINT, interrupted)
    signal.signal(signal.SIGTERM, interrupted)
    return LiveCheck(args).run()


if __name__ == "__main__":
    raise SystemExit(main())
