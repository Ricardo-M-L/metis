#!/usr/bin/env python3
"""Three real, isolated image-tool turns on the original configured route.

The caller supplies a source-frozen native CLI and authorizes its destruction
with --destroy-input. This harness never builds or installs a CLI, modifies the
real Metis home, resumes a real session, or reads user images. Two independent
synthetic PNGs contain giant A/B glyphs above random pixels: their byte pressure
exceeds the historical erroneous base64-as-text compaction trigger while their
native visual input is small. Each turn must freshly ViewImage both PNGs.

Only secret-free aggregate proof is retained in evidence.noindex. CLI output,
trace/session data, PNG/base64 bytes, private config, environment credentials,
and both test binaries are private and removed in finally, even after failure.
No request-recording proxy is used; adapter wire tests cover native image_url
encoding separately. This live test proves CLI tool/history/compaction behavior.
"""

from __future__ import annotations

import argparse
import base64
from collections import Counter
import hashlib
import ipaddress
import json
import os
from pathlib import Path
import re
import secrets
import shutil
import signal
import struct
import subprocess
import sys
import tempfile
import time
import tomllib
from typing import Any
from urllib.parse import urlsplit, urlunsplit
import zlib

from project_coordinator_live_check import atomic_json, require, safe_endpoint, toml_table, utc_now


TARGET_MODEL = "gpt-6.1-sol"
TARGET_PROVIDER = "easycliproxyapi"
PRIVATE_KEY_ENV = "METIS_IMAGE_PRESSURE_ROUTE_KEY"
MIN_LEGACY_PRESSURE = 783_700
ROUNDS = 3
MAX_TRACE_BYTES = 64 << 20
GLYPHS = {
    "A": ("01110", "11011", "11011", "11111", "11011", "11011", "11011"),
    "B": ("11110", "11011", "11011", "11110", "11011", "11011", "11110"),
}


def digest(path: Path) -> str:
    return hashlib.sha256(path.read_bytes()).hexdigest()


def normalized_binding(provider: str, transport: str, endpoint: str) -> tuple[str, str, str]:
    """Mirror Metis endpoint identity normalization without accepting redirects."""
    aliases = {"": "anthropic_messages", "anthropic": "anthropic_messages",
               "anthropic_messages": "anthropic_messages", "openai": "openai_chat",
               "chat": "openai_chat", "openai_chat": "openai_chat",
               "responses": "openai_responses", "openai_responses": "openai_responses"}
    require(isinstance(transport, str) and transport.strip().lower() in aliases,
            "Selected route transport is unsupported by this image acceptance harness")
    require(isinstance(provider, str) and bool(re.fullmatch(r"[A-Za-z0-9_.-]+", provider)), "Invalid selected provider")
    parsed = urlsplit(endpoint.strip())
    require(parsed.scheme.lower() in ("http", "https") and parsed.hostname and
            not parsed.username and not parsed.password and not parsed.query and not parsed.fragment,
            "Selected endpoint must be a fixed HTTP(S) route without credentials/query/fragment")
    host = parsed.hostname.lower()
    if parsed.scheme.lower() == "http":
        try:
            loopback = ipaddress.ip_address(host.rstrip(".")).is_loopback
        except ValueError:
            loopback = host.rstrip(".") == "localhost"
        require(loopback, "Plain HTTP endpoint is allowed only on loopback")
    port = parsed.port
    if (parsed.scheme.lower(), port) in (("http", 80), ("https", 443)):
        port = None
    authority = ("[" + host + "]") if ":" in host else host
    if port is not None:
        authority += ":" + str(port)
    path = parsed.path
    if path.endswith("/") and not path.endswith("//"):
        path = path[:-1]
    return provider.strip(), aliases[transport.strip().lower()], urlunsplit((parsed.scheme.lower(), authority, path, "", ""))


def load_selected_route(source_home: Path, environment: dict[str, str]) -> dict[str, Any]:
    """Read only the selected route; callers must not log the returned key."""
    raw_config = (source_home / "config.toml").read_bytes()
    document = tomllib.loads(raw_config.decode("utf-8"))
    providers = document.get("provider", {})
    provider = providers.get("default")
    require(provider == TARGET_PROVIDER, "Original selected provider changed; no fallback is authorized")
    settings = dict(providers.get("custom", {}).get(provider, {}))
    require(settings, "Original selected custom provider has no configuration")
    binding = normalized_binding(provider, settings.get("transport", ""), settings.get("base_url", ""))
    key = environment.get(settings.get("api_key_env", ""), "")
    source = "selected_api_key_env"
    if not key:
        key, source = settings.get("api_key", ""), "selected_inline_api_key"
    if not key:
        store_path = source_home / ".credentials" / "auth.json"
        require(store_path.is_file(), "Original route has no existing credential source")
        entry = json.loads(store_path.read_text()).get(provider, {})
        require(isinstance(entry, dict) and entry.get("type", "api") == "api", "Selected credential is not an API key")
        bound = entry.get("endpoint")
        require(isinstance(bound, dict) and bool(bound), "Custom stored key lacks endpoint binding")
        require(normalized_binding(bound.get("provider", ""), bound.get("transport", ""), bound.get("base_url", "")) == binding,
                "Stored key is bound to another provider, transport, host or path")
        key, source = entry.get("key", ""), "selected_endpoint_bound_store_entry"
    require(isinstance(key, str) and bool(key.strip()) and not any(char in key for char in "\r\n\x00"),
            "Original route has no usable existing credential")
    threshold = document.get("session", {}).get("auto_compact_threshold", 0.85)
    require(isinstance(threshold, (int, float)) and 0 < threshold <= 1, "Original compaction threshold is invalid")
    # An explicit hand-set window would hide whether real model metadata is used.
    require(not settings.get("context_window"), "Explicit original context_window needs a separately reviewed acceptance scope")
    return {"provider": provider, "settings": settings, "key": key, "credential_source": source,
            "binding": binding, "threshold": threshold, "config_sha256": hashlib.sha256(raw_config).hexdigest()}


def catalog_proof(source: Path) -> tuple[bytes, dict[str, Any]]:
    raw = source.read_bytes()
    require(len(raw) <= 32 << 20, "Real model catalog exceeds bounded snapshot size")
    model = json.loads(raw).get("openai", {}).get("models", {}).get(TARGET_MODEL, {})
    require(model.get("id") == TARGET_MODEL, "Real catalog lacks exact canonical target model")
    limits = model.get("limit", {})
    require(all(type(limits.get(name)) is int and limits[name] > 0 for name in ("context", "input", "output")),
            "Real catalog lacks target context/input/output limits")
    return raw, {"source_sha256": hashlib.sha256(raw).hexdigest(), "model": TARGET_MODEL,
                 "limits": {name: limits[name] for name in ("context", "input", "output")},
                 "source": "existing_real_models_json", "fabricated_capacity": False}


def png_bytes(letter: str, *, random_bytes: Any = secrets.token_bytes) -> bytes:
    """600 square pixels; 300 white glyph rows and 300 independent noise rows."""
    require(letter in GLYPHS, "Synthetic glyph must be A or B")
    width, height, scale = 600, 600, 32
    rows = []
    left, top = (width - 5 * scale) // 2, (300 - 7 * scale) // 2
    for y in range(height):
        if y >= 300:
            row = random_bytes(width * 3)
        else:
            pixels = bytearray(b"\xff" * (width * 3))
            gy = (y - top) // scale
            if 0 <= gy < 7:
                for gx, bit in enumerate(GLYPHS[letter][gy]):
                    if bit == "1":
                        start = (left + gx * scale) * 3
                        pixels[start:start + scale * 3] = b"\x00" * (scale * 3)
            row = bytes(pixels)
        require(len(row) == width * 3, "Synthetic random row has invalid size")
        rows.append(b"\x00" + row)

    def chunk(kind: bytes, payload: bytes) -> bytes:
        return struct.pack(">I", len(payload)) + kind + payload + struct.pack(">I", zlib.crc32(kind + payload) & 0xffffffff)

    return b"\x89PNG\r\n\x1a\n" + chunk(b"IHDR", struct.pack(">IIBBBBB", width, height, 8, 2, 0, 0, 0)) + chunk(b"IDAT", zlib.compress(b"".join(rows), 6)) + chunk(b"IEND", b"")


def read_jsonl(path: Path) -> list[dict[str, Any]]:
    require(path.stat().st_size <= MAX_TRACE_BYTES, "Private trace/session exceeds bounded inspection size")
    rows = []
    with path.open(encoding="utf-8") as stream:
        for line in stream:
            if line.strip():
                value = json.loads(line)
                require(isinstance(value, dict), "Private JSONL has a non-object record")
                rows.append(value)
    return rows


def private_output_diagnostics(path: Path) -> dict[str, Any]:
    """Retain classified failure evidence, never raw subprocess output."""
    raw = path.read_bytes()
    require(len(raw) <= MAX_TRACE_BYTES, "Private subprocess output exceeded bounded diagnostics size")
    lower = raw.lower()
    vocabulary = ("unauthorized", "forbidden", "timeout", "timed out", "max_iterations", "max_tokens",
                  "budget", "context", "compaction", "sandbox", "permission", "authentication", "error")
    return {"bytes": len(raw), "sha256": hashlib.sha256(raw).hexdigest(),
            "diagnostic_keyword_counts": {word: lower.count(word.encode()) for word in vocabulary if word.encode() in lower},
            "raw_retained": False}


def active_history(rows: list[dict[str, Any]]) -> list[dict[str, Any]]:
    messages = []
    for row in rows:
        if row.get("type") == "message" and isinstance(row.get("message"), dict):
            messages.append(row["message"])
        elif row.get("type") == "history_replace":
            require(isinstance(row.get("messages", []), list), "Private history replacement has invalid messages")
            messages = list(row.get("messages") or [])
    return messages


def collect_image_hashes(value: Any, attachments: Path | None = None) -> Counter[str]:
    """Verify externalized/inline native llm image blocks without returning data."""
    hashes: Counter[str] = Counter()
    if isinstance(value, dict):
        if value.get("type") == "image":
            sha = value.get("attachment_hash", "")
            if sha:
                require(isinstance(sha, str) and bool(re.fullmatch(r"[a-f0-9]{64}", sha)), "Private image attachment hash is invalid")
                require(attachments is not None and (attachments / sha).is_file(), "Private image attachment blob is missing")
                require(digest(attachments / sha) == sha, "Private image attachment blob hash is corrupt")
                hashes[sha] += 1
            elif isinstance(value.get("data"), str) and value["data"]:
                decoded = base64.b64decode(value["data"], validate=True)
                hashes[hashlib.sha256(decoded).hexdigest()] += 1
        for child in value.values():
            hashes.update(collect_image_hashes(child, attachments))
    elif isinstance(value, list):
        for child in value:
            hashes.update(collect_image_hashes(child, attachments))
    return hashes


def trace_summary(events: list[dict[str, Any]], paths: list[Path], session_id: str) -> dict[str, Any]:
    require(events and all(event.get("session_id") == session_id for event in events), "Round trace has no exact private-session identity")
    counts = Counter(event.get("kind", "") for event in events)
    automatic_compactions = sum(event.get("kind") == "auto_compaction_start" or
                                (event.get("kind") == "compaction_start" and event.get("text") == "auto") for event in events)
    require(automatic_compactions == 0, "Image tool payload caused automatic compaction")
    require(not any(event.get("kind") == "error" for event in events), "Round trace reports an agent error")
    starts = [event for event in events if event.get("kind") == "tool_start"]
    require(starts and all(event.get("tool_name") == "ViewImage" for event in starts), "Round used an unapproved tool or did not freshly use ViewImage")
    results = [event for event in events if event.get("kind") == "tool_result"]
    matched: Counter[str] = Counter()
    result_ids = {event.get("trace_call_id") or event.get("tool_use_id") for event in results
                  if event.get("tool_name") == "ViewImage" and not event.get("is_error")}
    for event in starts:
        arguments = json.loads(event.get("text", "{}"))
        path = arguments.get("path", arguments.get("file_path"))
        require(isinstance(path, str), "ViewImage trace has no path argument")
        resolved = Path(path).resolve()
        require(resolved in paths, "ViewImage attempted to inspect an image outside synthetic fixture paths")
        identity = event.get("trace_call_id") or event.get("tool_use_id")
        require(identity and identity in result_ids, "Fresh ViewImage call lacks matching successful result")
        matched[resolved.name] += 1
    require(all(matched[path.name] >= 1 for path in paths), "Round did not freshly inspect both independent PNGs")
    require(any(event.get("kind") == "loop_done" and event.get("text") == "end_turn" for event in events), "Round did not terminate naturally with end_turn")
    usage: Counter[str] = Counter()
    for event in events:
        if event.get("kind") == "tokens":
            for name, number in re.findall(r"(input|output|cache_write|cache_read)=(\d+)", event.get("text", "")):
                usage[name] += int(number)
    return {"event_type_counts": dict(counts), "fresh_ViewImage_by_fixture": dict(matched), "usage": dict(usage),
            "auto_compaction_start_count": 0, "natural_end_turn": True, "successful_fresh_image_results": len(result_ids)}


class ImageContextPressureLiveCheck:
    def __init__(self, args: argparse.Namespace) -> None:
        require(args.destroy_input, "--destroy-input is required for every live run")
        self.args = args
        self.source_binary = args.binary.expanduser().resolve()
        require(self.source_binary.is_file() and not args.binary.expanduser().is_symlink(), "Supply a regular disposable test --binary")
        self.input_sha = digest(self.source_binary)
        self.output = args.output.expanduser().resolve()
        require(any(part.endswith(".noindex") for part in self.output.parts), "Retained reports must be inside an evidence .noindex directory")
        require(not self.output.exists() or not any(self.output.iterdir()), "Use a new/empty evidence output directory")
        require(self.source_binary != self.output and self.output not in self.source_binary.parents,
                "Disposable input binary must be outside retained evidence directory")
        self.output.mkdir(parents=True, exist_ok=True, mode=0o700)
        self.output.chmod(0o700)
        self.root = Path(tempfile.mkdtemp(prefix="metis-image-pressure-")).resolve()
        self.root.chmod(0o700)
        self.binary = self.root / "metis"
        self.home = self.root / "metis-home"
        self.user_home = self.root / "user-home"
        self.workspace = self.root / "workspace"
        self.processes: list[subprocess.Popen] = []
        self.key = ""
        self.environment: dict[str, str] = {}
        self.report: dict[str, Any] = {"passed": False, "mode": "image-context-pressure-live", "started_utc": utc_now(),
            "model": TARGET_MODEL, "effort": "high", "round_count": ROUNDS, "rounds": [],
            "mock_model_completions": False, "retained_raw_output": False, "recording_proxy": False,
            "boundary": "Real CLI ViewImage and durable same-session image history, with real model catalog metadata. Native HTTP image_url wire shape is covered by separate adapter unit tests; native desktop pixels are outside this CLI check."}

    def progress(self, phase: str) -> None:
        atomic_json(self.output / "progress.json", {"phase": phase, "utc": utc_now(), "completed_rounds": len(self.report["rounds"])})
        print(json.dumps({"phase": phase, "completed_rounds": len(self.report["rounds"])}, ensure_ascii=False), flush=True)

    def prepare(self) -> None:
        if self.args.expected_sha256:
            require(self.input_sha == self.args.expected_sha256, "Disposable CLI differs from frozen build digest")
        if self.args.source_freeze:
            freeze = json.loads(self.args.source_freeze.read_text())
            require(isinstance(freeze.get("files"), dict) and freeze["files"], "Source freeze must contain file digests")
            repo = self.args.repo.resolve()
            require(all((repo / name).is_file() and digest(repo / name) == sha for name, sha in freeze["files"].items()),
                    "Source changed after supplied source freeze")
            self.report["source_freeze"] = {"sha256": digest(self.args.source_freeze), "file_count": len(freeze["files"])}
        shutil.copyfile(self.source_binary, self.binary)
        self.binary.chmod(0o700)
        require(digest(self.binary) == self.input_sha, "Private CLI copy differs from supplied binary")
        self.report["binary"] = {"sha256": self.input_sha, "compiled_by_harness": False, "destroy_input_requested": True}
        source_home = self.args.source_home.expanduser().resolve()
        route = load_selected_route(source_home, dict(os.environ))
        self.source_config = source_home / "config.toml"
        self.source_config_sha = route["config_sha256"]
        self.key = route["key"]
        settings = dict(route["settings"])
        original_model = settings.get("model", "")
        settings.pop("api_key", None)
        settings["api_key_env"] = PRIVATE_KEY_ENV
        settings.update({"model": TARGET_MODEL, "max_tokens": self.args.max_output_tokens})
        for path in (self.home, self.user_home, self.workspace, self.home / "agents", self.home / "cache"):
            path.mkdir(mode=0o700)
        raw_catalog, proof = catalog_proof(source_home / "cache" / "models.json")
        require(self.args.max_output_tokens <= proof["limits"]["output"], "Private output bound exceeds real catalog limit")
        (self.home / "cache" / "models.json").write_bytes(raw_catalog)
        (self.home / "cache" / "models.json").chmod(0o600)
        config = toml_table(["provider"], {"default": TARGET_PROVIDER})
        config += "\n" + toml_table(["provider", "custom", TARGET_PROVIDER], settings)
        config += "\n" + toml_table(["session"], {"dir": str(self.home / "sessions"), "skill_dir": str(self.home / "skills"),
            "max_iterations": self.args.max_iterations, "auto_compact_threshold": route["threshold"]})
        config += '\n[tools]\nallowed = ["ViewImage"]\n\n[permission]\nmode = "dontAsk"\n\n[[permission.allow]]\ntool = "ViewImage"\n'
        (self.home / "config.toml").write_text(config)
        (self.home / "config.toml").chmod(0o600)
        (self.home / "agents" / "image-pressure-probe.md").write_text("---\nname: image-pressure-probe\ntools: [ViewImage]\nmodel: " + TARGET_MODEL + "\neffort: high\nomit_claude_md: true\nmax_turns: " + str(self.args.max_iterations) + "\n---\n"
            "You verify two synthetic local PNG glyphs. On every user turn, freshly call ViewImage once for each exact supplied absolute path, even when an identical image is already in the session history. "
            "Use no other tool. After both calls finish, answer only the two requested letters and the current completion marker. Do not describe the random lower half or perform any other task.\n")
        self.environment = {name: os.environ[name] for name in ("PATH", "SHELL", "LANG", "LC_ALL") if name in os.environ}
        self.environment.update({"HOME": str(self.user_home), "METIS_HOME": str(self.home), "TMPDIR": str(self.root),
            "XDG_CACHE_HOME": str(self.root / "cache"), "METIS_AUTO_MEMORY": "0", "METIS_NO_UPDATE_CHECK": "1",
            "METIS_RUN_CACHE": "0", "METIS_RUN_MAX_SECONDS": str(self.args.round_timeout),
            "METIS_TURN_MAX_SECONDS": str(self.args.round_timeout), PRIVATE_KEY_ENV: self.key})
        self.report["original_route"] = {"provider": TARGET_PROVIDER, "transport": route["binding"][1],
            "endpoint": safe_endpoint(settings["base_url"]), "original_model": original_model,
            "credential_source": route["credential_source"], "provider_transport_endpoint_changed": False}
        self.report["private_parameter_overrides"] = {"model": TARGET_MODEL, "max_tokens": self.args.max_output_tokens,
            "effort": "high", "auto_compact_threshold": route["threshold"], "explicit_context_window_override": False}
        self.report["catalog_reference"] = proof
        self.session_id = "image-pressure-" + secrets.token_hex(12)
        self.paths = [self.workspace / "synthetic-first.png", self.workspace / "synthetic-second.png"]
        images = []
        for path, letter in zip(self.paths, ("A", "B")):
            path.write_bytes(png_bytes(letter))
            path.chmod(0o600)
            images.append({"fixture": path.name, "sha256": digest(path), "bytes": path.stat().st_size,
                           "base64_length": 4 * ((path.stat().st_size + 2) // 3), "width": 600, "height": 600})
        require(images[0]["sha256"] != images[1]["sha256"], "Synthetic PNG fixtures must be independent")
        weighted = sum(row["base64_length"] for row in images) * 0.75
        require(weighted > MIN_LEGACY_PRESSURE, "Synthetic base64 pressure is insufficient to reproduce old false trigger")
        self.report["fixtures"] = {"images": images, "combined_png_bytes": sum(row["bytes"] for row in images),
            "legacy_base64_times_point75": weighted, "minimum_legacy_pressure": MIN_LEGACY_PRESSURE,
            "source": "new_independent_synthetic_pngs_only", "contains_user_data": False}
        self.progress("private CLI, route and synthetic fixtures prepared")

    def traces(self) -> list[dict[str, Any]]:
        return [row for path in sorted((self.home / "traces").glob("*.jsonl")) for row in read_jsonl(path)
                if row.get("session_id") == self.session_id]

    def run_round(self, number: int) -> None:
        before = self.traces()
        marker = "METIS_IMAGE_ROUND_" + str(number) + "_" + secrets.token_hex(8)
        prompt = ("Round " + str(number) + ": freshly invoke ViewImage now for BOTH independent synthetic files in this exact order: "
                  + str(self.paths[0]) + " then " + str(self.paths[1]) + ". Re-read BOTH even if their content or an earlier tool result exists in history. "
                  "Read only the giant letter in each image's upper white band. After the two fresh calls, answer exactly: <first-letter> <second-letter> " + marker)
        args = ["run", "--bare", "--mode", "fullAccess", "--agent", "image-pressure-probe", "--tools", "ViewImage", "--model", TARGET_MODEL, "--effort", "high", "--no-auth-wizard", "--no-markdown",
                "--max-iter", str(self.args.max_iterations), "--session-id" if number == 1 else "--resume", self.session_id, prompt]
        started = time.monotonic()
        out_path, err_path = self.root / f"round-{number}.stdout", self.root / f"round-{number}.stderr"
        with out_path.open("wb") as out, err_path.open("wb") as err:
            process = subprocess.Popen([str(self.binary), *args], cwd=self.workspace, env=self.environment,
                                       stdout=out, stderr=err, start_new_session=True)
            self.processes.append(process)
            try:
                code = process.wait(timeout=self.args.round_timeout)
            except subprocess.TimeoutExpired:
                self.stop_process(process)
                raise AssertionError("Private CLI round exceeded finite time budget") from None
        row: dict[str, Any] = {"round": number, "mode": "new_session" if number == 1 else "resume", "effort": "high",
            "exit_code": code, "duration_seconds": round(time.monotonic() - started, 3), "marker_present": False}
        self.report["rounds"].append(row)
        row["output_diagnostics"] = {"stdout": private_output_diagnostics(out_path), "stderr": private_output_diagnostics(err_path)}
        atomic_json(self.output / f"round-{number}-diagnostics.json", row["output_diagnostics"])
        require(code == 0, "Private CLI returned nonzero exit status; raw output remains private and will be destroyed")
        events = self.traces()
        require(events[:len(before)] == before, "Resume rewrote earlier private trace history")
        row.update(trace_summary(events[len(before):], self.paths, self.session_id))
        output = out_path.read_bytes()
        require(len(output) <= 4 << 20, "Private CLI output exceeded bounded response size")
        row["marker_present"] = ("A B " + marker).encode() in output
        require(row["marker_present"], "Private CLI did not return the short A/B completion marker")
        sessions = list((self.home / "sessions").rglob(self.session_id + ".jsonl"))
        require(len(sessions) == 1, "Exact private session persistence file is missing or ambiguous")
        history = active_history(read_jsonl(sessions[0]))
        image_hashes = collect_image_hashes(history, sessions[0].with_suffix(".jsonl.attachments"))
        expected = {image["sha256"] for image in self.report["fixtures"]["images"]}
        require(set(image_hashes) == expected, "Durable history lost a synthetic image or contains an unexpected image")
        require(all(image_hashes[sha] >= number for sha in expected), "Resume discarded an earlier image-tool payload")
        row["durable_image_sha256_counts"] = dict(image_hashes)
        row["image_hashes_preserved_on_resume"] = True
        self.progress("private image round " + str(number) + " verified")

    @staticmethod
    def stop_process(process: subprocess.Popen) -> None:
        # The process group is ours because Popen used start_new_session=True.
        try:
            os.killpg(process.pid, signal.SIGTERM)
        except ProcessLookupError:
            pass
        try:
            process.wait(timeout=5)
        except subprocess.TimeoutExpired:
            try:
                os.killpg(process.pid, signal.SIGKILL)
            except ProcessLookupError:
                pass
            process.wait(timeout=5)
        # A completed leader can leave children in its private group.
        try:
            os.killpg(process.pid, signal.SIGKILL)
        except ProcessLookupError:
            pass

    def cleanup(self) -> None:
        errors = []
        for process in self.processes:
            try:
                self.stop_process(process)
            except BaseException as error:
                errors.append(type(error).__name__)
        if hasattr(self, "source_config"):
            try:
                self.report["original_config_unchanged"] = digest(self.source_config) == self.source_config_sha
            except BaseException as error:
                self.report["original_config_unchanged"] = False
                errors.append(type(error).__name__)
        self.key = ""
        self.environment.clear()
        try:
            shutil.rmtree(self.root)
        except FileNotFoundError:
            pass
        except BaseException as error:
            errors.append(type(error).__name__)
        if self.source_binary.exists():
            if digest(self.source_binary) == self.input_sha:
                try:
                    self.source_binary.unlink()
                except BaseException as error:
                    errors.append(type(error).__name__)
            else:
                errors.append("SuppliedBinaryReplacedRefusedDeletion")
        cleanup = {"private_root_removed": not self.root.exists(), "private_binary_removed": not self.binary.exists(),
            "input_binary_removed": not self.source_binary.exists(), "private_credentials_cleared": not self.key and not self.environment,
            "owned_process_leaders_stopped": all(process.poll() is not None for process in self.processes), "error_types": errors}
        self.report["cleanup"] = cleanup
        require(not errors and all(value for name, value in cleanup.items() if name != "error_types"), "Private CLI cleanup incomplete")

    def run(self) -> int:
        try:
            self.prepare()
            for number in range(1, ROUNDS + 1):
                self.run_round(number)
            self.report["passed"] = True
        except BaseException as error:
            # Exception messages or subprocess output may contain credentials;
            # retain only bounded harness assertion text (never parser errors).
            self.report["failure_type"] = type(error).__name__
            if type(error) is AssertionError:
                self.report["failure"] = str(error)[:300]
            self.report["passed"] = False
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
    parser.add_argument("--binary", type=Path, required=True, help="Fresh disposable source-frozen native CLI")
    parser.add_argument("--destroy-input", action="store_true", required=True, help="Required: destroy supplied CLI in finally")
    parser.add_argument("--expected-sha256", default="")
    parser.add_argument("--source-freeze", type=Path)
    parser.add_argument("--repo", type=Path, default=Path(__file__).resolve().parents[2])
    parser.add_argument("--source-home", type=Path, default=Path(os.environ.get("METIS_HOME", str(Path.home() / ".metis"))))
    parser.add_argument("--output", type=Path, required=True, help="New directory under evidence.noindex")
    parser.add_argument("--round-timeout", type=int, default=300)
    parser.add_argument("--max-iterations", type=int, default=5)
    parser.add_argument("--max-output-tokens", type=int, default=2048)
    args = parser.parse_args(argv)
    parser.error("--round-timeout must be 30..900") if not 30 <= args.round_timeout <= 900 else None
    parser.error("--max-iterations must be 3..8") if not 3 <= args.max_iterations <= 8 else None
    parser.error("--max-output-tokens must be 256..4096") if not 256 <= args.max_output_tokens <= 4096 else None
    parser.error("--expected-sha256 must be a SHA256 hex digest") if args.expected_sha256 and not re.fullmatch(r"[a-f0-9]{64}", args.expected_sha256) else None
    return args


if __name__ == "__main__":
    try:
        raise SystemExit(ImageContextPressureLiveCheck(arguments()).run())
    except (AssertionError, OSError, ValueError) as error:
        # Constructor failures must not echo config/auth parser material.
        print(json.dumps({"passed": False, "startup_failure_type": type(error).__name__}), file=sys.stderr)
        raise SystemExit(1)
