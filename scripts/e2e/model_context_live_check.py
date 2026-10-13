#!/usr/bin/env python3
"""Verify endpoint context discovery with an isolated byte-identical real CLI.

Requires a freshly built --binary supplied by the caller after source freeze.
Never builds, installs, changes the real config, or stops a real Metis process.
Cold discovery starts without a provider metadata cache and sends a short real
query through the original easycliproxyapi/gpt-6-sol/anthropic_messages route.
Expected capacities are independently read from the real authenticated pi
listing; no historic 272k/872k/128k numbers are hardcoded as upstream evidence.

Hot-cache restart uses a clearly labeled trusted local metadata fixture, copied
from those observed values and bound to an owned loopback counting trap. It is
not evidence of another gateway's capacity. Metadata GETs must remain zero;
anonymous startup HEAD preconnects are counted separately. Local model rebind
and status sampling must emit no HTTP of any kind to that fixture endpoint.

Every outcome stops owned web/worker groups, closes HTTP reader threads and
destroys the private runnable CLI copy and credential copies before reporting.
The supplied parent-owned binary is preserved unless --destroy-input is given.
"""

from __future__ import annotations

import argparse
from collections import Counter
from datetime import datetime, timezone
import hashlib
import http.client
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import json
import os
from pathlib import Path
import re
import secrets
import shutil
import signal
import socket
import subprocess
import sys
import tempfile
import threading
import time
import tomllib
from typing import Any
from urllib.error import HTTPError
from urllib.parse import urlencode, urlsplit, urlunsplit
from urllib.request import HTTPRedirectHandler, ProxyHandler, Request, build_opener

from project_coordinator_live_check import atomic_json, require, safe_endpoint, toml_table, utc_now


STATUS_FIELDS = ("contextWindow", "contextDefaultWindow", "contextMaxWindow", "contextMaxOutputTokens",
                 "contextSource", "compactThreshold", "compactAtTokens", "contextUsed", "contextSessionId")


def digest(path: Path) -> str:
    return hashlib.sha256(path.read_bytes()).hexdigest()


def positive_int(value: Any, name: str) -> int:
    require(type(value) is int and 0 < value <= 100_000_000, "Real listing lacks a valid " + name)
    return value


class NoRedirect(HTTPRedirectHandler):
    def redirect_request(self, *_args: Any, **_kwargs: Any) -> None:
        return None


class MetadataTrap:
    """No fake model completion or discovery response; every HTTP is recorded."""

    def __init__(self) -> None:
        self.lock = threading.Lock()
        self.requests: list[dict[str, Any]] = []
        self.clients: set[socket.socket] = set()
        self.workers: set[threading.Thread] = set()
        trap = self

        class Handler(BaseHTTPRequestHandler):
            def do_HEAD(self) -> None:
                self.reply()

            def do_GET(self) -> None:
                self.reply()

            def do_POST(self) -> None:
                self.reply()

            def reply(self) -> None:
                # Headers and body are deliberately neither retained nor logged.
                with trap.lock:
                    trap.requests.append({"utc": utc_now(), "method": self.command,
                                          "path": urlsplit(self.path).path, "query": urlsplit(self.path).query})
                self.send_response(503)
                self.send_header("Content-Length", "0")
                self.send_header("Connection", "close")
                self.end_headers()
                self.close_connection = True

            def log_message(self, *_args: Any) -> None:
                pass

        class Server(ThreadingHTTPServer):
            daemon_threads = True
            block_on_close = False

            def process_request(self, request: socket.socket, address: Any) -> None:
                with trap.lock:
                    trap.clients.add(request)
                try:
                    super().process_request(request, address)
                except BaseException:
                    with trap.lock:
                        trap.clients.discard(request)
                    raise

            def process_request_thread(self, request: socket.socket, address: Any) -> None:
                thread = threading.current_thread()
                with trap.lock:
                    trap.workers.add(thread)
                try:
                    super().process_request_thread(request, address)
                finally:
                    with trap.lock:
                        trap.clients.discard(request)
                        trap.workers.discard(thread)

        self.server = Server(("127.0.0.1", 0), Handler)
        self.thread = threading.Thread(target=self.server.serve_forever, kwargs={"poll_interval": 0.1}, daemon=True)
        self.endpoint = "http://127.0.0.1:" + str(self.server.server_address[1])
        self.thread.start()

    def snapshot(self) -> list[dict[str, Any]]:
        with self.lock:
            return list(self.requests)

    def close(self) -> dict[str, Any]:
        self.server.shutdown()
        self.server.server_close()
        self.thread.join(timeout=5)
        seen: set[threading.Thread] = set()
        deadline = time.monotonic() + 10
        while time.monotonic() < deadline:
            with self.lock:
                clients, workers = list(self.clients), list(self.workers)
                seen.update(workers)
            for client in clients:
                try:
                    client.shutdown(socket.SHUT_RDWR)
                    client.close()
                except OSError:
                    pass
            for worker in workers:
                worker.join(timeout=0.1)
            with self.lock:
                if not self.clients and not self.workers:
                    break
            time.sleep(0.02)
        with self.lock:
            active = bool(self.clients or self.workers)
        proof = {"listener_stopped": not self.thread.is_alive(), "accepted_clients_and_threads_stopped": not active and all(not worker.is_alive() for worker in seen)}
        require(all(proof.values()), "Fixture trap socket/thread cleanup incomplete")
        return proof


class ModelContextLiveCheck:
    def __init__(self, args: argparse.Namespace) -> None:
        self.args = args
        self.repo, self.output = args.repo.resolve(), args.output.resolve()
        require(not self.output.exists() or not any(self.output.iterdir()), "Use a new/empty evidence directory")
        self.output.mkdir(parents=True, exist_ok=True)
        self.output.chmod(0o700)
        self.root = Path(tempfile.mkdtemp(prefix="metis-model-context-live-")).resolve()
        self.root.chmod(0o700)
        self.binary = self.root / "metis"
        self.source_binary = args.binary.expanduser().resolve()
        self.workspace = self.root / "workspace"
        self.workspace.mkdir(mode=0o700)
        self.states: dict[str, dict[str, Any]] = {}
        self.processes: list[subprocess.Popen] = []
        self.handles: dict[int, tuple[Any, Any]] = {}
        self.owned_worker_pids: set[int] = set()
        self.query_threads: list[threading.Thread] = []
        self.active_http: set[Any] = set()
        self.http_lock = threading.Lock()
        self.trap: MetadataTrap | None = None
        self.secrets: list[str] = []
        self.key = ""
        self.settings: dict[str, Any] = {}
        self.index = 0
        self.started = time.monotonic()
        self.report: dict[str, Any] = {"passed": False, "mock_model_completions": False, "started_utc": utc_now(),
            "checks": [], "commands": [], "temporary_root": str(self.root), "fixture_cases": [],
            "boundary": "Original-route real cold/short queries plus local trusted cache fixtures. HEAD preconnects are reported separately; fixture tests do not establish a different gateway capacity or an upper-limit prompt-size guarantee. Desktop worker status is exercised through the CLI web backend, not native GUI pixels."}

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
        with (self.output / "progress.jsonl").open("a", encoding="utf-8") as target:
            target.write(json.dumps(value, ensure_ascii=False) + "\n")
        print(json.dumps({"check": name, "passed": True}, ensure_ascii=False), flush=True)
        self.progress(name)

    def stage_binary(self) -> None:
        require(self.source_binary.is_file() and self.source_binary != self.binary, "Provide a fresh parent-built test --binary outside this private root")
        self.input_sha = digest(self.source_binary)
        self.input_stat = self.source_binary.stat()
        if self.args.expected_sha256:
            require(self.input_sha == self.args.expected_sha256, "Supplied CLI differs from the caller's fresh build digest")
        if self.args.source_freeze:
            snapshot = json.loads(self.args.source_freeze.read_text())
            require(snapshot.get("files"), "Source freeze must contain file digests")
            changed = [name for name, sha in snapshot["files"].items() if not (self.repo / name).is_file() or digest(self.repo / name) != sha]
            require(not changed, "Source changed since freeze: " + ", ".join(changed[:8]))
            self.report["source_freeze"] = {"path": str(self.args.source_freeze.resolve()), "sha256": digest(self.args.source_freeze), "files_checked": len(snapshot["files"])}
        shutil.copyfile(self.source_binary, self.binary)
        self.binary.chmod(0o700)
        require(digest(self.binary) == self.input_sha, "Private test CLI is not byte-identical")
        self.report["binary"] = {"input": str(self.source_binary), "private_copy": str(self.binary), "sha256": self.input_sha,
                                 "recompiled_by_harness": False, "destroy_input_requested": self.args.destroy_input}
        self.record("fresh supplied CLI privately staged byte-identical", sha256=self.input_sha)

    def load_original_route(self) -> None:
        home = Path(os.environ.get("METIS_HOME", str(Path.home() / ".metis"))).expanduser().resolve()
        original = tomllib.loads((home / "config.toml").read_text())
        providers = original["provider"]
        require(providers.get("default") == "easycliproxyapi", "Original selected provider changed; no fallback is authorized")
        settings = dict(providers["custom"]["easycliproxyapi"])
        require(settings.get("model") == "gpt-6-sol" and settings.get("transport", "anthropic_messages") == "anthropic_messages", "Original model/transport changed")
        endpoint = urlsplit(settings.get("base_url", ""))
        require(endpoint.scheme in ("http", "https") and endpoint.hostname and not endpoint.username and not endpoint.query and not endpoint.fragment,
                "Original endpoint must have a plain fixed HTTP(S) identity without embedded credentials/query")
        require(not settings.get("context_window"), "Original route now has an explicit context override; cold endpoint-default scope must be reviewed")
        self.key = os.environ.get(settings.get("api_key_env", ""), "") or settings.pop("api_key", "")
        if not self.key:
            # Parse only to select the authorized route; neither the complete
            # auth document nor other provider entries are copied or logged.
            auth = json.loads((home / ".credentials" / "auth.json").read_text())
            selected = auth.get("easycliproxyapi", {})
            self.key = selected.get("key", "")
            endpoint = selected.get("endpoint", {})
            if endpoint:
                require(endpoint.get("provider") == "easycliproxyapi" and endpoint.get("transport") == "anthropic_messages" and
                        endpoint.get("base_url", "").rstrip("/") == settings["base_url"].rstrip("/"), "Stored key is bound to another route")
        require(self.key and "\n" not in self.key and "\r" not in self.key, "Original route has no usable existing API credential")
        self.secrets.append(self.key)
        self.settings = settings
        self.report["original_route"] = {"provider": "easycliproxyapi", "model": "gpt-6-sol", "transport": "anthropic_messages", "endpoint": safe_endpoint(settings["base_url"]),
                                         "provider_or_model_changed": False, "real_config_modified": False}
        self.report["private_parameter_overrides"] = {"max_tokens": 512, "auto_compact_threshold": 0.85,
            "reason": "Bounded short responses and an independently checkable compaction denominator; advertised output capacity remains separate"}

    def prepare_state(self, name: str, endpoint: str, *, real_auth: bool) -> dict[str, Any]:
        root = self.root / name
        home, user_home = root / "metis-home", root / "user-home"
        for directory in (root, home, user_home):
            directory.mkdir(mode=0o700)
        # Register before creating credentials so any partial setup failure is
        # still owned by finally cleanup.
        state = {"name": name, "root": root, "home": home, "real_auth": real_auth}
        self.states[name] = state
        key = self.key if real_auth else "local-metadata-fixture-key"
        (home / ".credentials").mkdir(mode=0o700)
        atomic_json(home / ".credentials" / "auth.json", {"easycliproxyapi": {"type": "api", "key": key,
            "endpoint": {"provider": "easycliproxyapi", "transport": "anthropic_messages", "base_url": endpoint}}})
        settings = dict(self.settings)
        settings.pop("api_key", None)
        settings.pop("api_key_env", None)
        settings.update({"base_url": endpoint, "max_tokens": 512})
        text = toml_table(["provider"], {"default": "easycliproxyapi"}) + "\n" + toml_table(["provider", "custom", "easycliproxyapi"], settings)
        text += "\n" + toml_table(["session"], {"dir": str(home / "sessions"), "skill_dir": str(home / "skills"), "max_iterations": 3, "auto_compact_threshold": 0.85})
        text += '\n[tools]\nallowed = ["Read"]\n\n[permission]\nmode = "dontAsk"\n\n[[permission.allow]]\ntool = "Read"\n'
        (home / "config.toml").write_text(text)
        (home / "config.toml").chmod(0o600)
        (home / "agents").mkdir(mode=0o700)
        (home / "agents" / "context-probe.md").write_text("---\nname: context-probe\ntools: [Read]\n---\nYou are a context-capability smoke-test responder. Return exactly the marker requested by the user. Do not call any tool, inspect files, discover directories, delegate, or perform additional work.\n")
        atomic_json(home / "desktop-preferences.json", {"defaultPreset": "context-probe", "rootTurnParallelism": 1, "totalAgentParallelism": 1, "subagentParallelism": 1})
        environment = {key: os.environ[key] for key in ("PATH", "TMPDIR", "SHELL", "LANG", "LC_ALL") if key in os.environ}
        environment.update({"HOME": str(user_home), "METIS_HOME": str(home), "METIS_AUTO_MEMORY": "0", "METIS_NO_UPDATE_CHECK": "1",
                            "METIS_RUN_CACHE": "0", "METIS_RUN_MAX_SECONDS": "120", "METIS_DESKTOP_MAX_PARALLEL_TURNS": "1", "NO_PROXY": "127.0.0.1,localhost"})
        state["environment"] = environment
        return state

    def spawn(self, arguments: list[str], state: dict[str, Any], label: str) -> subprocess.Popen:
        self.index += 1
        stem = self.output / (f"{self.index:03d}-" + label)
        out, err = stem.with_suffix(".stdout.log"), stem.with_suffix(".stderr.log")
        handles = out.open("w"), err.open("w")
        try:
            process = subprocess.Popen([str(self.binary), *arguments], cwd=self.workspace, env=state["environment"], stdout=handles[0], stderr=handles[1], start_new_session=True)
        except BaseException:
            for handle in handles:
                handle.close()
            raise
        self.processes.append(process)
        self.handles[process.pid] = handles
        state["process"], state["stdout"], state["stderr"] = process, out, err
        self.report["commands"].append({"label": label, "pid": process.pid, "arguments": arguments, "state": state["name"], "stdout": out.name, "stderr": err.name})
        return process

    def http(self, state: dict[str, Any], method: str, path: str, value: Any = None, timeout: float = 20) -> tuple[int, Any]:
        target = urlsplit(state["url"])
        require(target.scheme == "http" and target.hostname == "127.0.0.1", "Owned web API must remain loopback HTTP")
        connection = http.client.HTTPConnection(target.hostname, target.port, timeout=timeout)
        with self.http_lock:
            self.active_http.add(connection)
        response = None
        try:
            connection.request(method, path, body=None if value is None else json.dumps(value).encode(), headers={"Content-Type": "application/json", "Connection": "close"})
            response = connection.getresponse()
            with self.http_lock:
                self.active_http.add(response)
            body = response.read(4 << 20)
            return response.status, json.loads(body) if body else None
        finally:
            if response is not None:
                response.close()
                with self.http_lock:
                    self.active_http.discard(response)
            connection.close()
            with self.http_lock:
                self.active_http.discard(connection)

    def start_web(self, state: dict[str, Any], label: str) -> dict[str, Any]:
        with socket.socket() as reservation:
            reservation.bind(("127.0.0.1", 0))
            port = reservation.getsockname()[1]
        state["url"] = "http://127.0.0.1:" + str(port)
        process = self.spawn(["desktop", "--web", "--port", str(port)], state, label)
        deadline = time.monotonic() + 30
        while time.monotonic() < deadline:
            require(process.poll() is None, "Owned web CLI exited before readiness: " + label)
            try:
                code, status = self.http(state, "GET", "/api/status", timeout=2)
            except (OSError, ValueError):
                time.sleep(0.1)
                continue
            if code == 200:
                require(status.get("isolatedTurnsEnabled"), "CLI desktop web backend did not enable real isolated workers")
                return status
            time.sleep(0.1)
        raise AssertionError("Owned web CLI did not become ready")

    @staticmethod
    def alive(pid: int) -> bool:
        text = subprocess.run(["ps", "-p", str(pid), "-o", "stat=,command="], capture_output=True, text=True, timeout=5).stdout.strip()
        return bool(text) and not text.split()[0].startswith("Z")

    def find_workers(self) -> None:
        rows = subprocess.run(["ps", "-axo", "pid=,pgid=,command="], capture_output=True, text=True, timeout=10).stdout.splitlines()
        for row in rows:
            parts = row.strip().split(None, 2)
            if len(parts) == 3 and parts[0].isdigit() and parts[1].isdigit() and str(self.binary) in parts[2] and "--desktop-worker" in parts[2]:
                self.owned_worker_pids.add(int(parts[0]))

    def stop_web(self, state: dict[str, Any]) -> None:
        self.find_workers()
        process = state.get("process")
        if process and process.poll() is None:
            os.killpg(process.pid, signal.SIGTERM)
            try:
                process.wait(timeout=15)
            except subprocess.TimeoutExpired:
                os.killpg(process.pid, signal.SIGKILL)
                process.wait(timeout=10)
        if process:
            for handle in self.handles.pop(process.pid, ()):
                handle.close()
            for filename in (state.get("stdout"), state.get("stderr")):
                if filename:
                    filename.write_text(self.redact(filename.read_text(errors="replace")))
        self.find_workers()

    def read_real_pi(self) -> dict[str, Any]:
        target = self.settings["base_url"].rstrip("/") + "/v1/models?client_version=pi"
        request = Request(target, headers={"Authorization": "Bearer " + self.key, "Accept": "application/json"})
        handlers = [NoRedirect()]
        if urlsplit(target).hostname in ("127.0.0.1", "localhost", "::1"):
            handlers.append(ProxyHandler({}))
        opener = build_opener(*handlers)
        with opener.open(request, timeout=15) as response:
            require(response.status == 200, "Original endpoint pi catalog did not return HTTP 200")
            raw = response.read((2 << 20) + 1)
        require(len(raw) <= 2 << 20, "Original pi catalog exceeds the bounded metadata size")
        data = json.loads(raw)
        records = data.get("models", data.get("data")) if isinstance(data, dict) else data
        if isinstance(records, dict):
            records = [{**row, "slug": name} for name, row in records.items() if isinstance(row, dict)]
        require(isinstance(records, list), "Original pi listing did not expose model records")
        model = next((row for row in records if isinstance(row, dict) and (row.get("slug") or row.get("id")) == "gpt-6-sol"), None)
        require(model, "Original pi listing no longer advertises the authorized model")
        result = {"id": "gpt-6-sol", "context_window": positive_int(model.get("context_window"), "default context_window"),
                  "max_context_window": positive_int(model.get("max_context_window"), "max_context_window"),
                  "max_output_tokens": positive_int(model.get("max_tokens", model.get("max_output_tokens")), "output ceiling"), "source": "endpoint_pi"}
        require(result["max_context_window"] >= result["context_window"] and result["max_output_tokens"] >= 512, "Original metadata is incompatible with bounded test output budget")
        self.expected = result
        atomic_json(self.output / "real-endpoint-pi-selected-model.json", {"queried_utc": utc_now(), "url": safe_endpoint(target),
            "query": "client_version=pi", "response_sha256": hashlib.sha256(raw).hexdigest(), "selected_model": result, "saved_raw_body": False})
        self.record("real original endpoint pi metadata independently read without populating Metis cache", selected_model=result)
        return result

    def assert_context(self, status: dict[str, Any], label: str) -> dict[str, Any]:
        expected = {"contextWindow": self.expected["context_window"], "contextDefaultWindow": self.expected["context_window"],
                    "contextMaxWindow": self.expected["max_context_window"], "contextMaxOutputTokens": self.expected["max_output_tokens"], "contextSource": "endpoint_pi"}
        require(all(status.get(key) == value for key, value in expected.items()), label + " did not bind the original endpoint capability tuple: " + json.dumps({key: status.get(key) for key in expected}))
        require(status.get("compactThreshold") == 0.85, label + " lost the private configured compaction threshold")
        trigger = int((expected["contextWindow"] - min(512, 20000)) * 0.85)
        require(status.get("compactAtTokens") == trigger, label + " compactor is not coherent with active window and configured output reserve")
        require(type(status.get("contextUsed")) is int and status["contextUsed"] >= 0, label + " has invalid context pressure")
        return {key: status.get(key) for key in STATUS_FIELDS if key in status}

    def traces(self, state: dict[str, Any], session_id: str) -> list[dict[str, Any]]:
        events = []
        for path in (state["home"] / "traces").glob("*.jsonl"):
            for line in path.read_text(errors="replace").splitlines():
                try:
                    event = json.loads(line)
                except ValueError:
                    continue
                if event.get("session_id") == session_id:
                    events.append(event)
        return events

    def short_query(self, state: dict[str, Any], label: str) -> tuple[dict[str, Any], dict[str, Any]]:
        marker = "METIS_CONTEXT_PROBE_" + secrets.token_hex(8)
        outcome: dict[str, Any] = {}

        def query() -> None:
            try:
                outcome["code"], outcome["body"] = self.http(state, "POST", "/api/turns", {"input": "Return exactly " + marker + ". Do not use tools or perform any other task."}, timeout=self.args.query_timeout)
            except BaseException as error:
                outcome["error_type"] = type(error).__name__

        thread = threading.Thread(target=query, daemon=True)
        self.query_threads.append(thread)
        thread.start()
        sampled = []
        deadline = time.monotonic() + self.args.query_timeout
        while thread.is_alive() and time.monotonic() < deadline:
            self.find_workers()
            code, status = self.http(state, "GET", "/api/status", timeout=5)
            if code == 200 and status.get("turnRunning") and status.get("runningSessionId"):
                sid = status["runningSessionId"]
                code, viewed = self.http(state, "GET", "/api/status?" + urlencode({"sessionId": sid}), timeout=5)
                if code == 200 and viewed.get("contextSessionId") == sid and viewed.get("contextSource") == "endpoint_pi":
                    sampled.append(self.assert_context(viewed, label + " running worker"))
            self.progress(label, active_worker_pids=sorted(self.owned_worker_pids), runtime_capability_samples=len(sampled))
            time.sleep(0.1)
        thread.join(timeout=1)
        require(not thread.is_alive(), "Real short query did not finish within its bounded deadline")
        require(not outcome.get("error_type") and outcome.get("code") == 200, "Real short query failed: " + json.dumps({k: v for k, v in outcome.items() if k != "body"}))
        body = outcome["body"]
        require(body.get("sessionId") and body.get("text", "").strip() == marker and not body.get("stopped"), "Original model did not return the requested short probe marker")
        sid = body["sessionId"]
        code, status = self.http(state, "GET", "/api/status?" + urlencode({"sessionId": sid}))
        require(code == 200 and status.get("contextSessionId") == sid, "Completed desktop worker context is not bound to the correct session")
        proof = self.assert_context(status, label + " completed worker")
        events = self.traces(state, sid)
        tokens = [event for event in events if event.get("kind") == "tokens"]
        require(tokens and any(sum(int(number) for number in re.findall(r"=(\d+)", event.get("text", ""))) > 0 for event in tokens), "No actual provider usage evidence for the short query")
        require(not any(event.get("kind") in ("tool_start", "tool_result", "error") for event in events), "Short metadata probe unexpectedly used tools or emitted native errors")
        self.record(label + " actual model query and desktop worker status coherent", session_id=sid, marker_matched=True,
                    real_provider_usage_events=len(tokens), running_status_samples=len(sampled), final_status=proof)
        return body, proof

    def cold_and_real_restart(self) -> None:
        self.read_real_pi()
        state = self.prepare_state("real-route", self.settings["base_url"], real_auth=True)
        before = self.start_web(state, "cold-original-route-web")
        cache = state["home"] / "cache" / "provider-models"
        require(not list(cache.glob("*.json")), "First real query must start without any provider metadata cache")
        self.report["cold_prequery_status"] = {key: before.get(key) for key in STATUS_FIELDS if key in before}
        self.report["cold_metadata_cache_empty_before_first_query"] = True
        self.short_query(state, "cold-original-route")
        records = [json.loads(path.read_text()) for path in cache.glob("*.json")]
        relevant = next((record for record in records if record.get("binding", {}).get("provider_id") == "easycliproxyapi" and record["binding"].get("base_url", "").rstrip("/") == self.settings["base_url"].rstrip("/")), None)
        require(relevant and any(model.get("id") == "gpt-6-sol" and model.get("source") == "endpoint_pi" for model in relevant.get("models", [])), "Cold Run did not create endpoint-bound pi metadata cache")
        atomic_json(self.output / "cold-runtime-selected-cache-proof.json", {"binding": relevant["binding"], "updated_at": relevant["updated_at"],
                    "selected_model": next(model for model in relevant["models"] if model["id"] == "gpt-6-sol")})
        code, listing = self.http(state, "POST", "/api/providers/models", {"id": "easycliproxyapi"})
        chosen = next((row for row in (listing or {}).get("models", []) if row.get("id") == "gpt-6-sol"), None)
        require(code == 200 and chosen and chosen.get("contextWindow") == self.expected["context_window"] and chosen.get("maxContextWindow") == self.expected["max_context_window"] and
                chosen.get("maxOutputTokens") == self.expected["max_output_tokens"] and chosen.get("source") == "endpoint_pi", "Web discovery dropped real model capacities or provenance")
        self.record("web discovery independently retains default maximum and output ceiling", discovered_model=chosen)
        self.stop_web(state)
        restarted = self.start_web(state, "cached-original-route-web-restart")
        proof = self.assert_context(restarted, "real-route warm restart")
        self.record("actual original route restarts from its own validated persisted metadata", status=proof)
        self.short_query(state, "real-route-query-after-shared-cache-refresh")
        self.stop_web(state)

    def hot_fixture(self) -> None:
        self.trap = MetadataTrap()
        state = self.prepare_state("trusted-local-cache-fixture", self.trap.endpoint, real_auth=False)
        binding = {"provider_id": "easycliproxyapi", "transport": "anthropic_messages", "base_url": self.trap.endpoint}
        key = hashlib.sha256((binding["provider_id"] + "\x00" + binding["transport"] + "\x00" + binding["base_url"]).encode()).hexdigest()
        directory = state["home"] / "cache" / "provider-models"
        directory.mkdir(parents=True, mode=0o700)
        directory.chmod(0o700)
        atomic_json(directory / (key + ".json"), {"schema": 1, "binding": binding, "updated_at": utc_now(), "models": [self.expected]})
        self.report["fixture_cases"].append({"kind": "trusted-local-metadata-cache", "endpoint": self.trap.endpoint, "source_values": "copied from independently read actual endpoint metadata", "real_upstream_capacity_claim": False, "real_model_queries": 0})
        for attempt in range(2):
            status = self.start_web(state, "trusted-fixture-restart-" + str(attempt + 1))
            proof = self.assert_context(status, "trusted fixture cached restart")
            # Allow the explicitly documented asynchronous warmup HEAD to finish
            # before measuring the no-preconnect model rebind/status interval.
            time.sleep(3.2)
            baseline = self.trap.snapshot()
            require(not any(event["method"] != "HEAD" for event in baseline), "Hot metadata cache restart issued a discovery/generation HTTP request")
            code, _ = self.http(state, "POST", "/api/models", {"provider": "easycliproxyapi", "model": "gpt-6-sol"})
            require(code == 200, "Local cached provider rebind failed")
            for _ in range(5):
                code, current = self.http(state, "GET", "/api/status")
                require(code == 200, "Local status unavailable after cached rebind")
                self.assert_context(current, "trusted fixture cached rebind/status")
            time.sleep(0.3)
            require(len(self.trap.snapshot()) == len(baseline), "Cached local model rebind or status initiated HTTP")
            self.record("trusted fixture cached restart and local rebind perform no metadata HTTP", restart=attempt + 1, status=proof,
                        startup_anonymous_HEAD_count=sum(event["method"] == "HEAD" for event in baseline), metadata_GET_count=0,
                        local_rebind_and_status_HTTP_count=0, fixture_only=True, real_model_queries=0)
            self.stop_web(state)
        atomic_json(self.output / "trusted-fixture-http-audit.json", self.trap.snapshot())

    def cleanup(self) -> None:
        errors = []
        for state in self.states.values():
            try:
                self.stop_web(state)
            except BaseException as error:
                errors.append(self.redact(str(error)))
        try:
            self.find_workers()
            for pid in self.owned_worker_pids:
                if self.alive(pid):
                    command = subprocess.run(["ps", "-p", str(pid), "-o", "command="], capture_output=True, text=True, timeout=5).stdout
                    if str(self.binary) in command:
                        try:
                            os.killpg(pid, signal.SIGKILL)
                        except ProcessLookupError:
                            pass
        except BaseException as error:
            errors.append(self.redact(str(error)))
        with self.http_lock:
            responses = list(self.active_http)
        for response in responses:
            try:
                wire = response.sock if isinstance(response, http.client.HTTPConnection) else getattr(getattr(getattr(response, "fp", None), "raw", None), "_sock", None)
                if wire is not None:
                    wire.shutdown(socket.SHUT_RDWR)
                response.close()
            except OSError:
                pass
        for thread in self.query_threads:
            thread.join(timeout=5)
        deadline = time.monotonic() + 10
        while any(self.alive(pid) for pid in self.owned_worker_pids) and time.monotonic() < deadline:
            time.sleep(0.1)
        if self.trap:
            try:
                self.report["fixture_trap_cleanup"] = self.trap.close()
            except BaseException as error:
                errors.append(self.redact(str(error)))
        for handles in self.handles.values():
            for handle in handles:
                handle.close()
        self.handles.clear()
        self.binary.unlink(missing_ok=True)
        for state in self.states.values():
            shutil.rmtree(state["home"] / ".credentials", ignore_errors=True)
        self.key = ""
        if self.args.destroy_input and self.source_binary.exists():
            require(digest(self.source_binary) == getattr(self, "input_sha", ""), "Refusing to delete a replaced parent-owned input binary")
            self.source_binary.unlink()
        elif self.source_binary.exists() and hasattr(self, "input_sha"):
            require(digest(self.source_binary) == self.input_sha, "Parent-owned supplied binary changed")
        cleanup = {"private_CLI_destroyed": not self.binary.exists(), "private_credentials_removed": all(not (state["home"] / ".credentials").exists() for state in self.states.values()),
                   "owned_web_processes_stopped": all(process.poll() is not None for process in self.processes), "owned_desktop_workers_stopped": not any(self.alive(pid) for pid in self.owned_worker_pids),
                   "query_threads_stopped": all(not thread.is_alive() for thread in self.query_threads), "selected_execution_key_handle_cleared": not self.key,
                   "redaction_secret_values_retained_until_evidence_written": bool(self.secrets),
                   "input_binary_destroyed": not self.source_binary.exists() if self.args.destroy_input else False, "input_binary_preserved_by_contract": not self.args.destroy_input,
                   "cleanup_errors": errors}
        self.report["cleanup"] = cleanup
        require(not errors and all(cleanup[name] for name in ("private_CLI_destroyed", "private_credentials_removed", "owned_web_processes_stopped", "owned_desktop_workers_stopped", "query_threads_stopped", "selected_execution_key_handle_cleared")), "Private CLI/auth/process/socket cleanup incomplete")

    def copy_evidence(self) -> None:
        # Copy traces and worker status files only after executable/auth cleanup.
        files = []
        for name, state in self.states.items():
            for directory in ("sessions", "traces"):
                root = state["home"] / directory
                if not root.exists():
                    continue
                for source in root.rglob("*.jsonl"):
                    if not source.is_file() or source.is_symlink():
                        continue
                    destination = self.output / "evidence" / name / directory / source.relative_to(root)
                    destination.parent.mkdir(parents=True, exist_ok=True)
                    destination.write_text(self.redact(source.read_text(errors="replace")))
                    destination.chmod(0o600)
                    files.append({"path": str(destination.relative_to(self.output)), "sha256": digest(destination)})
        atomic_json(self.output / "evidence-manifest.json", files)

    def run(self) -> int:
        signal.setitimer(signal.ITIMER_REAL, self.args.case_timeout)
        try:
            self.stage_binary()
            self.load_original_route()
            self.cold_and_real_restart()
            self.hot_fixture()
            self.report["passed"] = True
        except BaseException as error:
            self.report["failure_type"] = type(error).__name__
            self.report["failure"] = self.redact(str(error))
        finally:
            signal.setitimer(signal.ITIMER_REAL, 0)
            for label, action in (("cleanup", self.cleanup), ("evidence_copy", self.copy_evidence)):
                try:
                    action()
                except BaseException as error:
                    self.report["passed"] = False
                    self.report[label + "_failure"] = self.redact(str(error))
            self.report.update({"finished_utc": utc_now(), "elapsed_seconds": round(time.monotonic() - self.started, 3)})
            self.report = json.loads(self.redact(json.dumps(self.report)))
            self.secrets.clear()
            self.report.setdefault("cleanup", {})["redaction_secret_values_cleared_after_evidence"] = not self.secrets
            atomic_json(self.output / "verification-report.json", self.report)
            self.progress("finished", passed=self.report["passed"], failure=self.report.get("failure"))
        print(json.dumps({"passed": self.report["passed"], "checks": len(self.report["checks"]), "report": str(self.output / "verification-report.json"), "failure": self.report.get("failure")}), flush=True)
        return 0 if self.report["passed"] else 1


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("--repo", type=Path, default=Path(__file__).resolve().parents[2])
    parser.add_argument("--binary", type=Path, required=True, help="fresh parent-built round CLI; never an installed application")
    parser.add_argument("--expected-sha256")
    parser.add_argument("--source-freeze", type=Path)
    parser.add_argument("--destroy-input", "--destroy-cli", dest="destroy_input", action="store_true", help="delete caller-owned round binary too, after verifying its identity")
    parser.add_argument("--output", "--report-directory", dest="output", type=Path, required=True)
    parser.add_argument("--query-timeout", type=float, default=180)
    parser.add_argument("--case-timeout", type=float, default=600)
    args = parser.parse_args()
    require(0 < args.query_timeout <= 300 and 0 < args.case_timeout <= 900, "Timeouts must be bounded")
    os.umask(0o077)

    def interrupted(signum: int, _frame: Any) -> None:
        raise KeyboardInterrupt("Harness interrupted by " + signal.Signals(signum).name)

    def timed_out(_signum: int, _frame: Any) -> None:
        raise TimeoutError("Bounded model-context case exceeded its deadline")

    signal.signal(signal.SIGINT, interrupted)
    signal.signal(signal.SIGTERM, interrupted)
    signal.signal(signal.SIGALRM, timed_out)
    return ModelContextLiveCheck(args).run()


if __name__ == "__main__":
    raise SystemExit(main())
