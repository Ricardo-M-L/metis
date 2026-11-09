#!/usr/bin/env python3
"""One original-route real image turn, trace HTTP proof and restart recovery.

Accept a caller-built disposable CLI; never compile, install, touch real
sessions, or retry model turns. Private HOME/METIS_HOME/workspace and a Read-only
agent profile contain the only test data. POST /api/turns carries two synthetic
PNG user attachments; GET /api/events observes actual streaming separately.
Only aggregate secret-free evidence survives finally. --hold-seconds keeps the
verified restarted server available for browser inspection until its bounded
deadline or a --stop-file appears. It still cleans up on every exit.
"""

from __future__ import annotations

import argparse
import base64
from collections import Counter, defaultdict
from datetime import datetime
import hashlib
import http.client
import json
import os
from pathlib import Path
import re
import secrets
import shutil
import signal
import socket
import struct
import subprocess
import sys
import tempfile
import threading
import time
from typing import Any
from urllib.parse import quote, urlsplit

from image_context_pressure_live_check import (TARGET_MODEL, TARGET_PROVIDER, ImageContextPressureLiveCheck,
    catalog_proof, digest, load_selected_route, png_bytes, private_output_diagnostics)
from project_coordinator_live_check import atomic_json, require, safe_endpoint, toml_table, utc_now


KEY_ENV = "METIS_TRACE_PRESENTATION_ROUTE_KEY"
PROFILE = "trace-presentation-probe"
MAX_HTTP_BYTES = 12 << 20


def stable_digest(value: Any) -> str:
    return hashlib.sha256(json.dumps(value, sort_keys=True, ensure_ascii=False, separators=(",", ":")).encode()).hexdigest()


def validate_trace(document: dict[str, Any], system_canary: str, expected_text: str,
                   expected_image_hashes: set[str], expected_read_path: Path | None = None) -> tuple[dict[str, Any], list[dict[str, Any]]]:
    require(document.get("enabled") is True, "Real trace is unavailable")
    require(not document.get("hasMore"), "Bounded single-turn trace unexpectedly requires pagination")
    initial = document.get("initialContext", {})
    require(initial.get("available") is True and initial.get("source") == "logical-request",
            "Trace lacks actual logical-request initial context")
    system = initial.get("system", "")
    require(isinstance(system, str) and system_canary in system and not initial.get("truncated"),
            "Actual system snapshot is missing, truncated or not this private profile")
    tools = initial.get("tools", [])
    require(isinstance(tools, list) and {tool.get("name") for tool in tools} == {"Read"},
            "Actual request tools differ from the private Read-only palette")
    require(all(isinstance(tool.get("description"), str) and tool["description"] and isinstance(tool.get("inputSchema"), dict) for tool in tools),
            "Actual tool snapshot lacks description or input schema")
    require(isinstance(initial.get("capturedAt"), str) and initial["capturedAt"], "Initial snapshot lacks capture time")
    datetime.fromisoformat(initial["capturedAt"].replace("Z", "+00:00"))
    if initial.get("model"):
        require(initial["model"] == TARGET_MODEL, "Actual snapshot selected an unauthorized model")
    events = document.get("events", [])
    require(isinstance(events, list) and events, "Trace contains no real events")
    require(not any(event.get("kind") == "error" or event.get("isError") for event in events), "Real turn contains a trace error")
    starts = [event for event in events if event.get("kind") == "tool_start"]
    require(starts and all(event.get("toolName") == "Read" for event in starts), "Real turn did not use only Read")
    if expected_read_path is not None:
        for event in starts:
            arguments = json.loads(event.get("text", "{}"))
            require(arguments.get("path", arguments.get("file_path")) == str(expected_read_path), "Real Read inspected a path outside its synthetic fixture")
    result_ids = {event.get("traceCallID") for event in events if event.get("kind") == "tool_result" and not event.get("isError")}
    require(all(event.get("traceCallID") and event["traceCallID"] in result_ids for event in starts),
            "Real Read start lacks an exact successful traceCallID result")
    streams: dict[str, list[str]] = defaultdict(list)
    for event in events:
        if event.get("kind") == "text":
            require(isinstance(event.get("streamID"), str) and event["streamID"], "Text event has no stable streamID")
            require(not event.get("truncated"), "Real response text was truncated")
            streams[event["streamID"]].append(event.get("text", ""))
    require(any("".join(parts).strip() == expected_text.strip() for parts in streams.values()),
            "Trace text does not contain the complete greater-than-2000-character real response")
    timings = [event.get("requestTiming") for event in events if event.get("kind") == "request_timing"]
    require(timings and all(isinstance(row, dict) for row in timings), "Request timing has no structured records")
    for row in timings:
        require(isinstance(row.get("request_id"), str) and row["request_id"] and isinstance(row.get("phase"), str) and row["phase"],
                "Structured request timing lacks request identity or phase")
        require(all(type(row.get(field)) is int and row[field] >= 0 for field in ("provider_call", "attempt", "elapsed_ms")),
                "Structured request timing lacks numeric attempt, provider call or elapsed time")
        require(isinstance(row.get("at"), str) and row["at"], "Structured request timing lacks occurrence time")
    images = [image for event in events if event.get("kind") == "user" for image in event.get("images", [])]
    require({row.get("attachmentHash") for row in images} == expected_image_hashes, "Trace user image metadata lost or replaced synthetic input")
    require(len(images) == len(expected_image_hashes), "Trace duplicated synthetic image metadata")
    for image in images:
        require(image.get("mediaType") == "image/png" and image.get("width") == 600 and image.get("height") == 600,
                "Trace image MIME/dimensions differ from submitted PNG")
        require(type(image.get("bytes")) is int and image["bytes"] > 0 and image.get("name"), "Trace image byte size/name is absent")
        parsed = urlsplit(image.get("url", ""))
        require(not parsed.scheme and not parsed.netloc and parsed.path == "/api/trace/image" and parsed.query,
                "Trace image URL is not an owned relative attachment endpoint")
    return {"initial_context_sha256": stable_digest(initial), "system_bytes": len(system.encode()), "tool_names": ["Read"],
        "event_type_counts": dict(Counter(event.get("kind") for event in events)), "successful_tool_calls": len(starts),
        "complete_text_characters": len(expected_text), "text_stream_count": len(streams), "text_sha256": hashlib.sha256(expected_text.encode()).hexdigest(),
        "structured_timing_records": len(timings), "timing_phases": dict(Counter(row["phase"] for row in timings)),
        "stream_text_identity_sha256": stable_digest(dict(streams)), "image_metadata_sha256": stable_digest(images)}, images


class EventReader:
    """Owned HTTP SSE reader. Payloads exist only in private process memory."""
    def __init__(self, port: int, timeout: int) -> None:
        self.connection = http.client.HTTPConnection("127.0.0.1", port, timeout=timeout + 30)
        self.response: http.client.HTTPResponse | None = None
        self.ready, self.stopped = threading.Event(), threading.Event()
        self.events: list[tuple[str, dict[str, Any]]] = []
        self.error_type = ""
        self.thread = threading.Thread(target=self.read, name="owned-trace-sse", daemon=True)
        self.thread.start()

    def read(self) -> None:
        try:
            self.connection.request("GET", "/api/events")
            self.response = self.connection.getresponse()
            require(self.response.status == 200 and self.response.getheader("Content-Type", "").startswith("text/event-stream"), "Owned SSE did not return event stream")
            name, lines = "message", []
            while not self.stopped.is_set():
                raw = self.response.readline(MAX_HTTP_BYTES + 1)
                if not raw:
                    break
                require(len(raw) <= MAX_HTTP_BYTES, "Owned SSE line exceeded bounded size")
                line = raw.decode("utf-8").rstrip("\r\n")
                if not line:
                    if lines:
                        payload = json.loads("\n".join(lines))
                        self.events.append((name, payload))
                        if name == "ready":
                            self.ready.set()
                    name, lines = "message", []
                elif line.startswith("event: "):
                    name = line[7:]
                elif line.startswith("data: "):
                    lines.append(line[6:])
        except BaseException as error:
            if not self.stopped.is_set():
                self.error_type = type(error).__name__
        finally:
            self.ready.set()

    def close(self) -> None:
        self.stopped.set()
        wire = self.connection.sock
        if wire is None and self.response is not None:
            wire = getattr(getattr(getattr(self.response, "fp", None), "raw", None), "_sock", None)
        if wire is not None:
            try:
                wire.shutdown(socket.SHUT_RDWR)
            except OSError:
                pass
        self.connection.close()
        self.thread.join(timeout=5)
        if self.response is not None:
            self.response.close()
        require(not self.thread.is_alive(), "Owned SSE reader did not stop")


class TracePresentationLiveCheck:
    def __init__(self, args: argparse.Namespace) -> None:
        require(args.destroy_input, "--destroy-input is mandatory")
        self.args = args
        self.source_binary = args.binary.expanduser().resolve()
        require(self.source_binary.is_file() and not args.binary.expanduser().is_symlink(), "Supply a regular disposable --binary")
        self.input_sha = digest(self.source_binary)
        self.output = args.output.expanduser().resolve()
        require(any(part.endswith(".noindex") for part in self.output.parts), "Evidence must be inside a .noindex directory")
        require(not self.output.exists() or not any(self.output.iterdir()), "Use an empty/new evidence directory")
        require(self.output not in self.source_binary.parents, "Disposable binary must be outside retained evidence")
        self.output.mkdir(parents=True, exist_ok=True, mode=0o700)
        self.output.chmod(0o700)
        self.root = Path(tempfile.mkdtemp(prefix="metis-trace-presentation-")).resolve()
        self.root.chmod(0o700)
        self.binary, self.home, self.workspace = self.root / "metis", self.root / "metis-home", self.root / "workspace"
        self.processes: list[subprocess.Popen] = []
        self.stopped_process_groups: set[int] = set()
        self.handles: list[Any] = []
        self.reader: EventReader | None = None
        self.environment: dict[str, str] = {}
        self.key = ""
        self.report: dict[str, Any] = {"passed": False, "mode": "trace-presentation-live", "started_utc": utc_now(),
            "model": TARGET_MODEL, "effort": "high", "model_turn_submissions": 0, "mock_model_completions": False,
            "retained_raw_trace_or_output": False, "native_desktop_launched": False, "checks": []}

    def progress(self, phase: str) -> None:
        atomic_json(self.output / "progress.json", {"phase": phase, "utc": utc_now(), "model_turn_submissions": self.report["model_turn_submissions"]})
        print(json.dumps({"phase": phase}), flush=True)

    def prepare(self) -> None:
        require(self.input_sha == self.args.expected_sha256, "Supplied CLI differs from frozen build digest")
        shutil.copyfile(self.source_binary, self.binary)
        self.binary.chmod(0o700)
        route = load_selected_route(self.args.source_home.expanduser().resolve(), dict(os.environ))
        self.source_config = self.args.source_home.expanduser().resolve() / "config.toml"
        self.source_config_sha = route["config_sha256"]
        self.key = route["key"]
        user_home = self.root / "user-home"
        for path in (self.home, self.workspace, user_home, self.home / "cache", self.home / "agents"):
            path.mkdir(mode=0o700)
        raw_catalog, proof = catalog_proof(self.args.source_home.expanduser().resolve() / "cache" / "models.json")
        (self.home / "cache" / "models.json").write_bytes(raw_catalog)
        (self.home / "cache" / "models.json").chmod(0o600)
        settings = dict(route["settings"])
        settings.pop("api_key", None)
        settings.update({"api_key_env": KEY_ENV, "model": TARGET_MODEL, "max_tokens": self.args.max_output_tokens})
        config = toml_table(["provider"], {"default": TARGET_PROVIDER}) + "\n" + toml_table(["provider", "custom", TARGET_PROVIDER], settings)
        config += "\n" + toml_table(["session"], {"dir": str(self.home / "sessions"), "skill_dir": str(self.home / "skills"),
            "max_iterations": 4, "auto_compact_threshold": route["threshold"]})
        config += '\n[tools]\nallowed = ["Read"]\n\n[permission]\nmode = "dontAsk"\n\n[[permission.allow]]\ntool = "Read"\n'
        (self.home / "config.toml").write_text(config)
        (self.home / "config.toml").chmod(0o600)
        self.system_canary = "TRACE_ACTUAL_SYSTEM_" + secrets.token_hex(12)
        (self.home / "agents" / (PROFILE + ".md")).write_text("---\nname: " + PROFILE + "\nmodel: " + TARGET_MODEL +
            "\neffort: high\ntools: [Read]\npermission_mode: dontAsk\nmax_turns: 4\nomit_claude_md: true\n---\n" + self.system_canary + "\n"
            "You verify two synthetic user image attachments and read one explicitly supplied local text fixture. Call only Read, once, for that exact path. "
            "Return exactly the complete text file bytes as plain text, preserving all lines and characters, without a code fence or commentary. Do not summarize, shorten, inspect another file, delegate, or perform another task.\n")
        atomic_json(self.home / "desktop-preferences.json", {"defaultPreset": PROFILE, "rootTurnParallelism": 1, "totalAgentParallelism": 1, "subagentParallelism": 1})
        nonce = secrets.token_hex(6)
        self.expected_text = "TRACE_REAL_RESPONSE_" + nonce + "\n" + "\n".join(f"trace_line_{index:03d}_{nonce}_complete" for index in range(90))
        require(len(self.expected_text) > 2000, "Synthetic full-response fixture is too short")
        self.text_path = self.workspace / "synthetic-trace-response.txt"
        self.text_path.write_text(self.expected_text)
        self.images = [png_bytes(letter, random_bytes=lambda size: b"\xff" * size) for letter in ("A", "B")]
        self.image_by_hash = {hashlib.sha256(body).hexdigest(): body for body in self.images}
        self.environment = {name: os.environ[name] for name in ("PATH", "SHELL", "LANG", "LC_ALL") if name in os.environ}
        self.environment.update({"HOME": str(user_home), "METIS_HOME": str(self.home), "TMPDIR": str(self.root), "XDG_CACHE_HOME": str(self.root / "cache"),
            KEY_ENV: self.key, "METIS_AUTO_MEMORY": "0", "METIS_NO_UPDATE_CHECK": "1", "METIS_RUN_CACHE": "0", "METIS_RUN_MAX_SECONDS": str(self.args.query_timeout),
            "METIS_TURN_MAX_SECONDS": str(self.args.query_timeout), "METIS_DESKTOP_MAX_PARALLEL_TURNS": "1", "NO_PROXY": "127.0.0.1,localhost"})
        self.report.update({"binary_sha256": self.input_sha, "route": {"provider": TARGET_PROVIDER, "transport": route["binding"][1],
            "endpoint": safe_endpoint(settings["base_url"]), "credential_source": route["credential_source"], "route_changed": False}, "catalog_reference": proof,
            "startup": {"command": "desktop --web --port <owned-loopback-port>", "bare_flag_supported_by_desktop": False, "private_home_and_profile_only": True,
                        "tools": ["Read"], "max_output_tokens": self.args.max_output_tokens}})
        self.progress("private web runtime and synthetic data prepared")

    def http(self, method: str, path: str, value: Any = None, *, timeout: int = 15, raw: bool = False) -> tuple[int, Any, dict[str, str]]:
        require(path.startswith("/") and not path.startswith("//") and not urlsplit(path).netloc, "HTTP access must remain on owned loopback server")
        connection = http.client.HTTPConnection("127.0.0.1", self.port, timeout=timeout)
        try:
            connection.request(method, path, body=None if value is None else json.dumps(value).encode(), headers={"Content-Type": "application/json", "Connection": "close"})
            response = connection.getresponse()
            body = response.read(MAX_HTTP_BYTES + 1)
            require(len(body) <= MAX_HTTP_BYTES, "Owned HTTP response exceeded bounded size")
            return response.status, body if raw else json.loads(body) if body else None, {name.lower(): value for name, value in response.getheaders()}
        finally:
            connection.close()

    def start_server(self, label: str) -> None:
        with socket.socket() as reservation:
            reservation.bind(("127.0.0.1", 0))
            self.port = reservation.getsockname()[1]
        handles = [(self.root / (label + suffix)).open("wb") for suffix in (".stdout", ".stderr")]
        self.handles.extend(handles)
        self.server = subprocess.Popen([str(self.binary), "desktop", "--web", "--port", str(self.port)], cwd=self.workspace,
            env=self.environment, stdout=handles[0], stderr=handles[1], start_new_session=True)
        self.processes.append(self.server)
        deadline = time.monotonic() + 30
        while time.monotonic() < deadline:
            require(self.server.poll() is None, "Owned web server exited before readiness")
            try:
                code, status, _ = self.http("GET", "/api/status", timeout=2)
                if code == 200:
                    require(not status.get("busy"), "Fresh private server is unexpectedly busy")
                    self.progress(label + " web server ready")
                    return
            except (OSError, ValueError, http.client.HTTPException):
                pass
            time.sleep(.1)
        raise AssertionError("Owned web server did not become ready")

    def stop_server(self) -> None:
        if self.reader is not None:
            self.reader.close()
            self.reader = None
        ImageContextPressureLiveCheck.stop_process(self.server)
        self.stopped_process_groups.add(self.server.pid)
        for handle in self.handles:
            handle.flush()
            handle.close()
        self.handles.clear()

    def query(self) -> None:
        self.reader = EventReader(self.port, self.args.query_timeout)
        require(self.reader.ready.wait(5) and not self.reader.error_type and any(name == "ready" for name, _ in self.reader.events), "Owned SSE stream did not become ready")
        prompt = "Inspect the two attached synthetic letter images as user inputs. Use Read exactly once on " + str(self.text_path) + ". Then reply with exactly that entire text file and nothing else; preserve every line, do not abbreviate or add wrappers."
        self.report["model_turn_submissions"] += 1
        code, result, _ = self.http("POST", "/api/turns", {"input": prompt, "images": [{"mediaType": "image/png", "data": base64.b64encode(body).decode()} for body in self.images]}, timeout=self.args.query_timeout)
        require(code == 200 and isinstance(result, dict), "Single real image turn did not return HTTP 200")
        self.session_id = result.get("sessionId", "")
        require(isinstance(self.session_id, str) and bool(re.fullmatch(r"[A-Za-z0-9_.-]+", self.session_id)), "Real turn did not identify a private session")
        require(result.get("text", "").strip() == self.expected_text.strip(), "Real model did not return complete synthetic Read output")
        deadline = time.monotonic() + 3
        while time.monotonic() < deadline and not any(name == "loop_done" and payload.get("session") == self.session_id for name, payload in self.reader.events):
            time.sleep(.05)
        scoped = [(name, payload) for name, payload in self.reader.events if payload.get("session") == self.session_id]
        require(not self.reader.error_type and scoped, "Real SSE stream contains no private turn events")
        streamed = "".join(payload.get("delta", "") for name, payload in scoped if name == "text_delta")
        require(streamed.strip() == self.expected_text.strip(), "Actual SSE text differs from complete real response")
        self.report["sse"] = {"event_type_counts": dict(Counter(name for name, _ in scoped)), "complete_text_characters": len(streamed),
            "text_sha256": hashlib.sha256(streamed.strip().encode()).hexdigest(), "ready_observed": True}
        self.progress("single real image turn and complete SSE response received")

    def verify(self, label: str) -> dict[str, Any]:
        code, document, _ = self.http("GET", "/api/trace?sessionId=" + quote(self.session_id) + "&limit=2000")
        require(code == 200 and isinstance(document, dict), "Private trace endpoint failed")
        proof, images = validate_trace(document, self.system_canary, self.expected_text, set(self.image_by_hash), self.text_path)
        blobs = []
        for image in images:
            code, raw, headers = self.http("GET", image["url"], raw=True)
            expected = self.image_by_hash[image["attachmentHash"]]
            require(code == 200 and headers.get("content-type", "").split(";", 1)[0] == "image/png", "Trace attachment URL failed or returned wrong MIME")
            require(raw == expected and len(raw) == image["bytes"], "Trace attachment bytes differ from submitted synthetic PNG")
            require(raw.startswith(b"\x89PNG\r\n\x1a\n") and struct.unpack(">II", raw[16:24]) == (600, 600), "Trace attachment PNG dimensions are invalid")
            blobs.append({"sha256": image["attachmentHash"], "bytes": len(raw), "width": 600, "height": 600, "mime": "image/png"})
        proof["retrieved_images"] = blobs
        self.report["checks"].append({"label": label, **proof})
        self.progress(label + " HTTP trace, actual context and image URLs verified")
        return proof

    def hold(self) -> None:
        if not self.args.hold_seconds:
            return
        stop = self.args.stop_file.expanduser().resolve() if self.args.stop_file else self.output / "stop"
        require(not stop.exists(), "Hold stop-file already exists")
        atomic_json(self.output / "live-server.json", {"url": "http://127.0.0.1:" + str(self.port), "pid": self.server.pid,
            "sessionId": self.session_id, "stop_file": str(stop), "hold_seconds": self.args.hold_seconds,
            "instruction": "Inspect this isolated server read-only; create stop_file to finish cleanup."})
        self.progress("verified private server held for browser inspection")
        deadline = time.monotonic() + self.args.hold_seconds
        while time.monotonic() < deadline and not stop.exists():
            require(self.server.poll() is None, "Held private server exited unexpectedly")
            time.sleep(.25)
        self.report["hold"] = {"requested_seconds": self.args.hold_seconds, "stop_file_observed": stop.exists()}

    def cleanup(self) -> None:
        errors = []
        if self.reader is not None:
            try:
                self.reader.close()
            except BaseException as error:
                errors.append(type(error).__name__)
            self.reader = None
        for process in self.processes:
            if process.pid in self.stopped_process_groups:
                continue
            try:
                ImageContextPressureLiveCheck.stop_process(process)
                self.stopped_process_groups.add(process.pid)
            except BaseException as error:
                errors.append(type(error).__name__)
        for handle in self.handles:
            handle.close()
        self.handles.clear()
        self.report["server_output_diagnostics"] = {}
        for path in list(self.root.glob("*.stdout")) + list(self.root.glob("*.stderr")):
            try:
                self.report["server_output_diagnostics"][path.name] = private_output_diagnostics(path)
            except BaseException as error:
                errors.append(type(error).__name__)
        if hasattr(self, "source_config"):
            try:
                self.report["original_config_unchanged"] = digest(self.source_config) == self.source_config_sha
            except OSError:
                self.report["original_config_unchanged"] = False
        self.key = ""
        self.environment.clear()
        try:
            shutil.rmtree(self.root)
        except OSError as error:
            errors.append(type(error).__name__)
        if self.source_binary.exists():
            if digest(self.source_binary) == self.input_sha:
                try:
                    self.source_binary.unlink()
                except OSError as error:
                    errors.append(type(error).__name__)
            else:
                errors.append("ReplacedInputBinaryRefusedDeletion")
        cleanup = {"private_root_removed": not self.root.exists(), "private_binary_removed": not self.binary.exists(),
            "input_binary_removed": not self.source_binary.exists(), "credential_environment_cleared": not self.key and not self.environment,
            "owned_processes_stopped": all(process.poll() is not None for process in self.processes), "error_types": errors}
        self.report["cleanup"] = cleanup
        live = self.output / "live-server.json"
        if live.exists():
            live.unlink()
        require(not errors and all(value for name, value in cleanup.items() if name != "error_types"), "Private web runtime cleanup incomplete")

    def run(self) -> int:
        try:
            self.prepare()
            self.start_server("first")
            self.query()
            before = self.verify("before-restart")
            self.stop_server()
            self.start_server("restarted")
            after = self.verify("after-restart")
            require(before == after, "Restart changed durable actual context, streams, timing or image proof")
            code, _, _ = self.http("POST", "/api/sessions/activate", {"id": self.session_id})
            require(code == 200, "Restored private session could not be selected for browser inspection")
            self.report["restart_restored_identical_trace_proof"] = True
            self.hold()
            self.report["passed"] = True
        except BaseException as error:
            self.report["failure_type"] = type(error).__name__
            if type(error) is AssertionError:
                self.report["failure"] = str(error)[:300]
        finally:
            try:
                self.cleanup()
            except BaseException as error:
                self.report["cleanup_failure_type"] = type(error).__name__
                self.report["passed"] = False
            if self.report.get("original_config_unchanged") is False:
                self.report["passed"] = False
            self.report["finished_utc"] = utc_now()
            atomic_json(self.output / "summary.json", self.report)
        print(json.dumps({"passed": self.report["passed"], "summary": str(self.output / "summary.json")}), flush=True)
        return 0 if self.report["passed"] else 1


def arguments(argv: list[str] | None = None) -> argparse.Namespace:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--binary", type=Path, required=True)
    parser.add_argument("--destroy-input", action="store_true", required=True)
    parser.add_argument("--expected-sha256", required=True)
    parser.add_argument("--source-home", type=Path, default=Path(os.environ.get("METIS_HOME", str(Path.home() / ".metis"))))
    parser.add_argument("--output", type=Path, required=True)
    parser.add_argument("--query-timeout", type=int, default=300)
    parser.add_argument("--max-output-tokens", type=int, default=8192)
    parser.add_argument("--hold-seconds", type=int, default=0)
    parser.add_argument("--stop-file", type=Path)
    args = parser.parse_args(argv)
    if not re.fullmatch(r"[a-f0-9]{64}", args.expected_sha256):
        parser.error("--expected-sha256 must be a SHA256 hex digest")
    if not 30 <= args.query_timeout <= 900 or not 2048 <= args.max_output_tokens <= 16384 or not 0 <= args.hold_seconds <= 3600:
        parser.error("Timeout must be 30..900, output budget 2048..16384, hold 0..3600")
    return args


if __name__ == "__main__":
    def terminate(_signal: int, _frame: Any) -> None:
        raise InterruptedError("Private live check interrupted")
    signal.signal(signal.SIGTERM, terminate)
    try:
        raise SystemExit(TracePresentationLiveCheck(arguments()).run())
    except (AssertionError, OSError, ValueError) as error:
        print(json.dumps({"passed": False, "startup_failure_type": type(error).__name__}), file=sys.stderr)
        raise SystemExit(1)
