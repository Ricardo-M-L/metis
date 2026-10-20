#!/usr/bin/env python3
"""Fresh real-CLI acceptance for launchd, operation holds and usage receipts.

Run only after the production source has been frozen. Every invocation compiles
its own CGO-disabled native CLI and uses the original configured provider/model.
The launchd case installs a uniquely named job with private HOME/METIS_HOME,
then uninstalls it and removes every owned executable/token even on failure.
The operation case controls a counter as the simulated external system; model
responses are real. The usage proxy forwards original bytes and keeps reading a
real upstream response after the worker is killed. Only original upstream usage
frames form a receipt. These are reported tokens, never a provider bill.
"""

from __future__ import annotations

import argparse
from copy import deepcopy
import hashlib
import http.client
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import json
import os
from pathlib import Path
import plistlib
import secrets
import shlex
import signal
import socket
import subprocess
import sys
import threading
import time
import tomllib
from typing import Any
from urllib.parse import urlsplit

from project_coordinator_live_check import atomic_json, require, root_trace_identity, toml_table, utc_now
from project_coordinator_service_live_check import ServiceLiveCheck


HOP_HEADERS = {"connection", "keep-alive", "proxy-authenticate", "proxy-authorization", "te", "trailer", "transfer-encoding", "upgrade", "host", "content-length"}
IMPLEMENTATION_PROFILE_MARKER = "METIS_DURABILITY_IMPLEMENTATION_PROFILE_V1"
VERIFICATION_PROFILE_MARKER = "METIS_DURABILITY_VERIFICATION_PROFILE_V1"


def sha256(path: Path) -> str:
    return hashlib.sha256(path.read_bytes()).hexdigest()


class ManagedProcess:
    """A launchd-owned PID supports read-only liveness checks, never Popen reap."""

    def __init__(self, pid: int) -> None:
        self.pid = pid

    def poll(self) -> int | None:
        return None if ServiceLiveCheck.pid_alive(self.pid) else -signal.SIGKILL


class ReceiptForwarder:
    """Pinned transparent gateway with one bounded real-SSE frame gate at a time."""

    def __init__(self, owner: "DurabilityLiveCheck", upstream: str, key: str, sentinel: str) -> None:
        self.owner, self.key, self.sentinel = owner, key, sentinel
        self.upstream = urlsplit(upstream)
        require(self.upstream.scheme in ("http", "https") and self.upstream.hostname and not self.upstream.username and not self.upstream.query,
                "Usage forwarder requires the original fixed HTTP(S) endpoint")
        self.lock = threading.Lock()
        self.events: list[dict[str, Any]] = []
        self.active: dict[str, dict[str, Any]] = {}
        self.connections: set[http.client.HTTPConnection] = set()
        self.responses: set[http.client.HTTPResponse] = set()
        self.clients: set[socket.socket] = set()
        self.workers: set[threading.Thread] = set()
        self.accepted_clients: set[socket.socket] = set()
        self.request_threads: set[threading.Thread] = set()
        self.closing = False
        self.next_mode: str | None = None
        self.gated = threading.Event()
        self.release = threading.Event()
        self.target: dict[str, Any] | None = None
        self.request_count = 0

        proxy = self

        class Handler(BaseHTTPRequestHandler):
            protocol_version = "HTTP/1.1"

            def do_POST(self) -> None:
                proxy.handle(self)

            def log_message(self, *_args: Any) -> None:
                pass

        class Server(ThreadingHTTPServer):
            daemon_threads = True
            block_on_close = False

            def process_request(self, request: socket.socket, address: Any) -> None:
                # Register at accept, before any requestline/header read. A
                # partial HTTP header must not escape shutdown accounting.
                with proxy.lock:
                    proxy.accepted_clients.add(request)
                try:
                    super().process_request(request, address)
                except BaseException:
                    with proxy.lock:
                        proxy.accepted_clients.discard(request)
                    raise

            def process_request_thread(self, request: socket.socket, address: Any) -> None:
                worker = threading.current_thread()
                with proxy.lock:
                    proxy.request_threads.add(worker)
                try:
                    super().process_request_thread(request, address)
                finally:
                    with proxy.lock:
                        proxy.accepted_clients.discard(request)
                        proxy.request_threads.discard(worker)

        self.server = Server(("127.0.0.1", 0), Handler)
        self.thread = threading.Thread(target=self.server.serve_forever, kwargs={"poll_interval": 0.1}, daemon=True)
        self.endpoint = "http://127.0.0.1:" + str(self.server.server_address[1])

    def arm(self, mode: str) -> None:
        require(mode in ("after_start", "before_start"), "Unknown usage gate mode")
        with self.lock:
            require(self.next_mode is None and not self.target, "A usage gate is already active")
            self.next_mode = mode
            self.gated.clear()
            self.release.clear()

    def reset_gate(self) -> None:
        with self.lock:
            self.target = None
            self.next_mode = None
            self.gated.clear()
            self.release.clear()

    def start(self) -> None:
        self.thread.start()

    def handle(self, handler: BaseHTTPRequestHandler) -> None:
        connection = None
        response = None
        event: dict[str, Any] = {"utc": utc_now(), "stage": "local_authorization"}
        thread = threading.current_thread()
        with self.lock:
            self.clients.add(handler.connection)
            self.workers.add(thread)
            self.request_count += 1
            event["request_sequence"] = self.request_count
        try:
            require(not self.closing and handler.path == "/v1/messages" and secrets.compare_digest(handler.headers.get("x-api-key", ""), self.sentinel),
                    "Unrecognized local provider RPC")
            length = int(handler.headers.get("Content-Length", "0"))
            require(0 < length <= 16 << 20, "Provider request size outside bound")
            raw = handler.rfile.read(length)
            body = json.loads(raw)
            require(body.get("model") == self.owner.report["model"] and isinstance(body.get("stream", False), bool), "Provider/model must stay pinned")
            streaming = body.get("stream", False)
            system_parts = body.get("system", "")
            actual_system = system_parts if isinstance(system_parts, str) else "\n".join(
                block.get("text", "") for block in system_parts if isinstance(block, dict) and isinstance(block.get("text"), str)) if isinstance(system_parts, list) else ""
            event["actual_request_profile_markers"] = {
                "implementation": IMPLEMENTATION_PROFILE_MARKER in actual_system,
                "verification": VERIFICATION_PROFILE_MARKER in actual_system,
                "foreground_policy": "This worker uses synchronous durable operation receipts." in actual_system and
                    "The general background-job and finite-background-check instructions do not apply here." in actual_system and
                    "Never replace an uncertain external action with rewritten arguments or another tool; the host must resolve its receipt." in actual_system,
            }
            if streaming:
                markers = event["actual_request_profile_markers"]
                require(markers["implementation"] != markers["verification"] and markers["foreground_policy"],
                        "Actual provider request lost the selected profile body or durable foreground system policy")
            client_id = handler.headers.get("X-Metis-Request-Id", "")
            mode = None
            with self.lock:
                if streaming and self.next_mode is not None:
                    mode, self.next_mode = self.next_mode, None
            event.update({"stream": streaming, "client_request_id": client_id, "gate_mode": mode, "request_sha256": hashlib.sha256(raw).hexdigest(), "stage": "upstream_request"})
            if mode:
                require(len(client_id) == 32 and all(ch in "0123456789abcdef" for ch in client_id), "No reserved durable request ID on real RPC")
                with self.lock:
                    self.target = event
            kind = http.client.HTTPSConnection if self.upstream.scheme == "https" else http.client.HTTPConnection
            connection = kind(self.upstream.hostname, self.upstream.port, timeout=300)
            with self.lock:
                self.connections.add(connection)
                self.active[client_id] = event
            headers = {name: value for name, value in handler.headers.items() if name.lower() not in HOP_HEADERS | {"authorization", "x-api-key", "accept-encoding"}}
            headers.update({"x-api-key": self.key, "Content-Length": str(len(raw)), "Connection": "close", "Accept-Encoding": "identity"})
            connection.request("POST", self.upstream.path.rstrip("/") + "/v1/messages", body=raw, headers=headers)
            response = connection.getresponse()
            with self.lock:
                self.responses.add(response)
            event.update({"stage": "upstream_response", "status": response.status, "provider_request_id": response.getheader("request-id") or response.getheader("x-request-id") or ""})
            require(not mode or response.status == 200 and "text/event-stream" in response.getheader("Content-Type", ""), "Gated real RPC must be original successful SSE")
            require(response.getheader("Content-Encoding", "").strip().lower() in ("", "identity"), "Cannot inspect compressed upstream usage")
            handler.send_response(response.status)
            for name, value in response.getheaders():
                if name.lower() not in HOP_HEADERS:
                    handler.send_header(name, value)
            handler.send_header("Connection", "close")
            handler.end_headers()
            client_open = True
            buffered = b""
            usage_frames: list[dict[str, Any]] = []
            usage = {"input": 0, "output": 0, "cache_read": 0, "cache_write": 0}
            message_stopped = False
            while not self.closing:
                chunk = response.read1(65536)
                if not chunk:
                    break
                if not streaming:
                    if client_open:
                        try:
                            handler.wfile.write(chunk)
                            handler.wfile.flush()
                        except OSError:
                            client_open = False
                    continue
                buffered += chunk
                require(len(buffered) <= 2 << 20, "SSE frame exceeds bound")
                while True:
                    boundary, separator = buffered.find(b"\n\n"), 2
                    if boundary < 0:
                        boundary, separator = buffered.find(b"\r\n\r\n"), 4
                    if boundary < 0:
                        break
                    frame, buffered = buffered[:boundary + separator], buffered[boundary + separator:]
                    data = b"\n".join(line[5:].lstrip() for line in frame.replace(b"\r\n", b"\n").splitlines() if line.startswith(b"data:"))
                    parsed = json.loads(data) if data and data != b"[DONE]" else {}
                    frame_type = parsed.get("type")
                    message = parsed.get("message") or {}
                    frame_usage = message.get("usage") if frame_type == "message_start" else parsed.get("usage")
                    if frame_type == "message_start":
                        event["response_id"] = message.get("id", "")
                    if isinstance(frame_usage, dict):
                        for field, native in (("input", "input_tokens"), ("output", "output_tokens"), ("cache_read", "cache_read_input_tokens"), ("cache_write", "cache_creation_input_tokens")):
                            if native in frame_usage:
                                value = frame_usage[native]
                                require(type(value) is int and value >= 0, "Original provider usage counter is invalid")
                                usage[field] = value
                        usage_frames.append({"frame_sha256": hashlib.sha256(frame).hexdigest(), "original_json": parsed})
                    if mode and frame_type == "message_start":
                        require(event.get("response_id") and frame_usage is not None and sum(usage.values()) > 0, "Real message_start has no response identity/reported usage")
                        event["partial_usage"] = dict(usage)
                        if mode == "after_start":
                            handler.wfile.write(frame)
                            handler.wfile.flush()
                            event["original_message_start_forwarded"] = True
                        else:
                            event["original_message_start_forwarded"] = False
                        event["stage"] = "gated_at_original_message_start"
                        self.gated.set()
                        require(self.release.wait(60), "Usage gate was not released within 60 seconds")
                        client_open = False
                        event["client_detached_after_gate"] = True
                        continue
                    if frame_type == "message_stop":
                        message_stopped = True
                    if frame_type == "error":
                        event["provider_stream_error"] = True
                    if client_open:
                        try:
                            handler.wfile.write(frame)
                            handler.wfile.flush()
                        except OSError:
                            client_open = False
            event.update({"stage": "upstream_finished", "message_stop_observed": message_stopped, "final_usage": dict(usage)})
            if mode:
                require(message_stopped and not event.get("provider_stream_error") and usage_frames, "Killed worker's original upstream stream did not report final usage")
                evidence = {"source": "original configured gateway Anthropic SSE", "client_request_id": client_id,
                            "provider_request_id": event["provider_request_id"], "response_id": event["response_id"],
                            "frames": usage_frames, "message_stop_observed": True, "model": self.owner.report["model"]}
                evidence_path = self.owner.output / ("upstream-usage-" + client_id + ".json")
                atomic_json(evidence_path, evidence)
                event["evidence_path"] = evidence_path.name
                event["evidence_sha256"] = sha256(evidence_path)
                if mode == "after_start":
                    ledger = self.owner.usage_record(client_id)
                    require(ledger and ledger["request_sha256"] == event["request_sha256"], "Receipt does not bind the exact persisted real request")
                    receipt = {"receipt_id": "real-sse-" + client_id, "evidence_sha256": event["evidence_sha256"],
                               "authority": "original-gateway-reported-Anthropic-SSE", "provider": ledger["provider"],
                               "endpoint_sha256": ledger["endpoint_sha256"], "model": ledger["model"],
                               "client_request_id": client_id, "response_id": event["response_id"], "usage": usage}
                    if event["provider_request_id"]:
                        receipt["provider_request_id"] = event["provider_request_id"]
                    inbox = self.owner.home / "coordinator-usage-receipts" / self.owner.run_id
                    inbox.mkdir(mode=0o700, parents=True, exist_ok=True)
                    atomic_json(inbox / (client_id + ".json"), receipt)
                    atomic_json(self.owner.output / ("imported-receipt-" + client_id + ".json"), receipt)
                    event["receipt_written"] = True
                else:
                    event["receipt_written"] = False
                    event["missing_feed_simulation"] = "Original response kept as test evidence; deliberately not provided to runtime receipt feed"
        except BaseException as error:
            event["error_type"] = type(error).__name__
            if isinstance(error, AssertionError):
                event["assertion"] = str(error)  # Every require message above is fixed, non-secret text.
            event["during_shutdown"] = self.closing
            handler.close_connection = True
        finally:
            if response is not None:
                response.close()
            if connection is not None:
                connection.close()
            with self.lock:
                self.connections.discard(connection)
                self.responses.discard(response)
                self.clients.discard(handler.connection)
                self.workers.discard(thread)
                self.active.pop(event.get("client_request_id", ""), None)
                self.events.append(deepcopy(event))
            handler.close_connection = True

    def observations(self) -> list[dict[str, Any]]:
        with self.lock:
            return deepcopy(self.events + list(self.active.values()))

    def stop(self) -> None:
        self.closing = True
        self.release.set()
        all_threads: set[threading.Thread] = set()
        try:
            # Stop accepting before snapshotting accepted clients. The server
            # thread never waits for provider handlers (our explicit joins do).
            if self.thread.is_alive():
                self.server.shutdown()
            self.server.server_close()
            if self.thread.ident is not None:
                self.thread.join(timeout=5)
            deadline = time.monotonic() + 10
            while time.monotonic() < deadline:
                with self.lock:
                    clients = list(self.clients | self.accepted_clients)
                    connections, responses = list(self.connections), list(self.responses)
                    workers = list(self.workers | self.request_threads)
                    all_threads.update(workers)
                for client in clients:
                    try:
                        client.shutdown(socket.SHUT_RDWR)
                        client.close()
                    except OSError:
                        pass
                for connection in connections:
                    try:
                        if connection.sock:
                            connection.sock.shutdown(socket.SHUT_RDWR)
                        connection.close()
                    except OSError:
                        pass
                for response in responses:
                    try:
                        stream_socket = getattr(getattr(getattr(response, "fp", None), "raw", None), "_sock", None)
                        if stream_socket is not None:
                            stream_socket.shutdown(socket.SHUT_RDWR)
                        response.close()
                    except OSError:
                        pass
                for worker in workers:
                    worker.join(timeout=min(0.2, max(0, deadline - time.monotonic())))
                with self.lock:
                    if not (self.workers or self.clients or self.connections or self.responses or self.accepted_clients or self.request_threads):
                        break
                time.sleep(0.02)
        finally:
            self.key = ""
        with self.lock:
            still_active = bool(self.workers or self.clients or self.connections or self.responses or self.accepted_clients or self.request_threads)
        self.owner.report["forwarder_cleanup"] = {"server_thread_stopped": not self.thread.is_alive(), "request_threads_stopped": all(not worker.is_alive() for worker in all_threads),
            "no_active_client_upstream_sockets": not still_active, "upstream_key_removed_from_memory": not self.key}
        require(all(self.owner.report["forwarder_cleanup"].values()), "Real-usage proxy thread/key cleanup incomplete")


class DurabilityLiveCheck(ServiceLiveCheck):
    def __init__(self, args: argparse.Namespace) -> None:
        args.quick, args.soak_seconds = True, 0
        super().__init__(args)
        # macOS /var is a symlink; trusted result-file checks require real paths.
        self.root = self.root.resolve()
        self.home, self.workspace, self.binary = self.root / "home", self.root / "workspace", self.root / "metis"
        self.environment.update({"HOME": str(self.root / "system-home"), "METIS_HOME": str(self.home)})
        self.token_path = self.root / "service-auth" / "bearer-token"
        self.proxy: ReceiptForwarder | None = None
        self.managed: dict[str, Any] | None = None
        self.managed_services: list[dict[str, Any]] = []
        self.owned_managed_pids: set[int] = set()
        self.external_pids: dict[int, dict[str, Any]] = {}
        self.external_markers: set[str] = set()
        self.extra_service_arguments: list[str] = []
        self.auxiliary_executables: set[Path] = set()
        self.report.update({"mode": "durability-live", "case": args.case, "temporary_root": str(self.root),
                            "case_timeout_seconds": args.case_timeout,
                            "fixture_contract": "Implementation reads only named input/output and performs the requested calculation. Verification reads declared files and executes only the supplied immutable host-check.py command. No directory discovery or additional self-authored validation programs. Native tool errors still fail acceptance. This measures the three durability mechanisms, not arbitrary business-task competence.",
                            "boundary": "Private macOS user launchd job, real model queries, controlled external counter and original gateway reported-token receipts. Login auto-start is verified in the plist; this test does not reboot/log out the user's computer. Missing provider receipts remain explicitly unresolved; estimated costs are not a provider bill."})

    def setup_provider(self) -> None:
        super().setup_provider()
        profile = self.home / "agents" / "live-scoped.md"
        profile.write_text(profile.read_text() +
            "\nProfile identity: " + IMPLEMENTATION_PROFILE_MARKER + ". "
            "Bounded fixture contract: input and output paths are already stated in the work item. "
            "Use Read only on those declared files and perform only the requested Python/Bash calculation and Write delivery. "
            "Do not discover directories with pwd, ls, find, globbing, or absolute system utility probes; no such discovery is needed. "
            "Use python3 from PATH for the necessary computation unless the work item gives an exact command, in which case follow it verbatim. "
            "Do not add self-authored verification programs. The final verifier uses the supplied independent host oracle. "
            "Every Bash command must finish in the foreground; omit all async flags or set them false.\n", encoding="utf-8")
        profile.chmod(0o600)

    def setup_service(self) -> None:
        super().setup_service()
        profile = self.home / "agents" / "live-verifier.md"
        profile.write_text(
            "---\nname: live-verifier\ntools: [Read, Bash]\n---\n"
            "Profile identity: " + VERIFICATION_PROFILE_MARKER + ". "
            "Verify only the current work item's declared input and output files with actual Read tools. "
            "This bounded fixture provides an independently implemented, immutable host oracle. "
            "Your single Bash validation command must be exactly: " + self.global_check() + ". "
            "Do not add &&, pipes, redirects, extra arguments, another Python program, directory scans, ls/find/pwd, or checks of how many earlier files exist. "
            "Do not invent an additional validator; the supplied oracle independently recalculates all required JSON fields. "
            "Run Bash in the foreground with async flags omitted or false. Preserve all files, never write/edit/delegate/read credentials or background work. "
            "Report concrete evidence from the declared file Reads and the oracle's actual successful output.\n", encoding="utf-8")
        profile.chmod(0o600)

    def make_event(self, identifier: str, number: int) -> dict[str, Any]:
        event = super().make_event(identifier, number)
        event["payload"]["prompt"] += (" The input/output filenames above are complete; do not discover directories or unrelated files. "
            "Implementation performs only the requested calculation; verification executes only the immutable host oracle command: " + self.global_check() + ". "
            "Do not append a self-authored validator or inspect all earlier files. All Bash calls finish in the foreground with async flags omitted or false.")
        return event

    def assert_profile_fidelity(self) -> None:
        profiles: dict[str, list[dict[str, Any]]] = {"implementation": [], "verification": []}
        for path in sorted((self.home / "sessions").glob("*.jsonl")):
            if ".timing." in path.name:
                continue
            lines = path.read_text(encoding="utf-8", errors="replace").splitlines()
            require(lines, "Actual worker session has no persisted header")
            record = json.loads(lines[0])
            header = record.get("header", {})
            system = header.get("system", "")
            require(record.get("type") == "header" and header.get("system_prompt_kind") == "custom" and isinstance(system, str),
                    "Actual worker session did not retain a custom system-prompt header")
            implementation = IMPLEMENTATION_PROFILE_MARKER in system
            verification = VERIFICATION_PROFILE_MARKER in system
            require(implementation != verification, "Actual session system body lost its selected implementation/verifier fixture profile")
            role = "implementation" if implementation else "verification"
            exact_body = (self.home / "agents" / ("live-scoped.md" if implementation else "live-verifier.md")).read_text().split("\n---\n", 1)[1].strip()
            require(exact_body in system, "Persisted session system contains a profile marker but not the complete actual profile body")
            profiles[role].append({"session_id": header.get("id"), "path": path.name, "system_sha256": hashlib.sha256(system.encode()).hexdigest(),
                                   "system_bytes": len(system.encode()), "complete_selected_profile_body_present": True})
        require(all(profiles.values()), "Actual implementation and independent verifier profile sessions were not both observed")
        proof: dict[str, Any] = {"sessions": profiles, "profile_headers_checked": sum(map(len, profiles.values()))}
        if self.proxy:
            requests = [event for event in self.proxy.observations() if event.get("stream")]
            require(requests and all(event.get("actual_request_profile_markers", {}).get("foreground_policy") and
                                    event["actual_request_profile_markers"]["implementation"] != event["actual_request_profile_markers"]["verification"] for event in requests),
                    "Original provider-bound request body did not carry the selected profile and durable system policy")
            require(all(any(event["actual_request_profile_markers"][role] for event in requests) for role in profiles),
                    "Original provider-bound implementation and verifier system bodies were not both observed")
            proof["actual_provider_stream_requests_checked"] = len(requests)
            proof["actual_provider_request_system_markers"] = [{"client_request_id": event.get("client_request_id"), **event["actual_request_profile_markers"]} for event in requests]
        self.record("actual worker session bodies retain complete role profiles; provider-bound bodies also retain durable rules when proxied", **proof)

    def build(self) -> None:
        if self.args.source_freeze:
            frozen = json.loads(self.args.source_freeze.read_text())
            require(frozen.get("files"), "Source freeze has no file manifest")
            changed = [name for name, digest in frozen["files"].items() if not (self.repo / name).is_file() or sha256(self.repo / name) != digest]
            require(not changed, "Production source changed after freeze: " + ", ".join(changed[:8]))
            self.report["source_freeze"] = {"path": str(self.args.source_freeze.resolve()), "sha256": sha256(self.args.source_freeze), "files_verified": len(frozen["files"])}
        previous = os.environ.get("CGO_ENABLED")
        os.environ["CGO_ENABLED"] = "0"
        try:
            super().build()
        finally:
            if previous is None:
                os.environ.pop("CGO_ENABLED", None)
            else:
                os.environ["CGO_ENABLED"] = previous
        text, _ = self.command(["version", "--verbose"], "verify-native-compiled-build")
        require("darwin/arm64" in text or "darwin" in text and "arm64" in text, "Compiled test CLI is not the native macOS ARM64 target")
        atomic_json(self.output / "build-profile.json", {"CGO_ENABLED": "0", "native_target": "darwin/arm64", "binary_sha256": sha256(self.binary), "verbose_version": text})

    def service_arguments(self, listen: str = "127.0.0.1:0") -> list[str]:
        arguments = super().service_arguments(listen)
        arguments[arguments.index("--lease") + 1] = "3s"
        arguments.extend(self.extra_service_arguments)
        return arguments

    def start_service(self, label: str) -> dict[str, Any]:
        # The inherited readiness check requires exactly two workers; use two
        # ordinary workers, while launchd remains one worker for crash identity.
        return super().start_service(label)

    def service_events(self, service: dict[str, Any]) -> list[dict[str, Any]]:
        if not service.get("launchd"):
            return super().service_events(service)
        path = Path(service["manifest"]["directory"]) / "stderr.log"
        events = []
        if path.exists():
            for line in path.read_text(encoding="utf-8", errors="replace").splitlines()[service.get("log_offset", 0):]:
                try:
                    event = json.loads(line)
                except ValueError:
                    continue
                if isinstance(event, dict) and event.get("kind"):
                    events.append(event)
                    if event.get("kind") == "worker_started" and isinstance(event.get("pid"), int):
                        self.child_pids.add(event["pid"])
        return events

    def service_control(self, action: str, *arguments: str, label: str, expect: int | None = 0) -> tuple[Any, dict[str, Any]]:
        text, proof = self.command(["coordinator", "service", action, *arguments, "--json"], label, expect=expect)
        return json.loads(text) if text.strip() else None, proof

    def launchd_ready(self, *, previous_pid: int = 0, log_offset: int = 0) -> dict[str, Any]:
        deadline = time.monotonic() + 45
        while time.monotonic() < deadline:
            status, _ = self.service_control("status", self.managed["id"], label="launchd-status")
            pid = status.get("pid", 0)
            if status.get("loaded") and status.get("binary_verified") and pid and pid != previous_pid and self.pid_alive(pid):
                service = {"process": ManagedProcess(pid), "manifest": self.managed, "label": "private-launchd", "launchd": True,
                           "log_offset": log_offset, "url": "http://" + self.managed["listen"]}
                ready = next((event for event in self.service_events(service) if event.get("kind") == "service_ready"), None)
                if ready:
                    service["service_id"] = ready["service_id"]
                    code, health = self.http(service, "GET", "/v1/health")
                    if code == 200 and health.get("run_id") == self.run_id:
                        self.owned_managed_pids.add(pid)
                        self.managed_services.append(service)
                        return service
            time.sleep(0.3)
        raise AssertionError("launchd did not automatically start a distinct ready service PID")

    def launchd_case(self) -> None:
        require(sys.platform == "darwin", "Native launchd acceptance requires macOS")
        with socket.socket() as reservation:
            reservation.bind(("127.0.0.1", 0))
            listen = "127.0.0.1:" + str(reservation.getsockname()[1])
        identifier = "live-durability-" + secrets.token_hex(6)
        # Store cleanup identity before installation: even an install error may
        # have reached launchd and created a stable binary/token.
        self.report["launchd_id"] = identifier
        arguments = self.service_arguments(listen)[3:]
        arguments[arguments.index("--workers") + 1] = "1"
        status, proof = self.service_control("install", self.run_id, "--id", identifier, *arguments, label="install-private-launchd")
        self.managed = status["manifest"]
        atomic_json(self.output / "launchd-manifest.json", self.managed)
        plist = plistlib.loads(Path(self.managed["plist"]).read_bytes())
        require(plist.get("RunAtLoad") is True and plist.get("KeepAlive") is True, "launchd does not declare login/start and crash restart")
        require(plist["EnvironmentVariables"]["HOME"] == self.environment["HOME"] and plist["EnvironmentVariables"]["METIS_HOME"] == str(self.home), "launchd did not pin private homes")
        require(plist["ProgramArguments"][0] == self.managed["executable"] and sha256(Path(self.managed["executable"])) == sha256(self.binary), "launchd stable binary differs from fresh tested CLI")
        atomic_json(self.output / "launchd-plist-projection.json", {key: plist[key] for key in ("Label", "RunAtLoad", "KeepAlive", "ProgramArguments", "EnvironmentVariables")})
        service = self.launchd_ready()
        first = self.make_event("launchd-before-crash", 11)
        snapshot = self.trace_snapshot()
        self.post_event(service, first)
        _, evidence = self.await_events(service, [first], snapshot)
        self.await_idle(service)
        self.record("launchd auto-started fresh CLI and processed actual HTTP event", pid=service["process"].pid, installation=proof, proof=evidence,
                    run_at_load=True, keep_alive=True, system_login_reboot_performed=False)
        following = self.make_event("launchd-crash-in-flight", 12)
        snapshot = self.trace_snapshot()
        self.post_event(service, following)
        deadline = time.monotonic() + 60
        old_item = None
        old_children: set[int] = set()
        while time.monotonic() < deadline:
            state = self.state_on_disk(self.run_id)
            item = next((item for item in state.get("items", []) if item["id"] == following["item_id"]), {})
            requests = [request for request in state.get("usage_requests", []) if request.get("item_id") == following["item_id"]]
            operations = [operation for operation in state.get("operations", []) if operation["item_id"] == following["item_id"]]
            old_children = self.active_child_pids(service)
            if item.get("status") == "running" and requests and old_children and not operations:
                old_item = item
                break
            time.sleep(0.03)
        require(old_item and len(old_children) == 1, "No unambiguous in-flight child before launchd leader crash")
        stderr = Path(self.managed["directory"]) / "stderr.log"
        offset = len(stderr.read_text(encoding="utf-8", errors="replace").splitlines())
        old_pid = service["process"].pid
        os.kill(old_pid, signal.SIGKILL)  # Only the leader; child termination is production parent-watch behavior.
        restarted = self.launchd_ready(previous_pid=old_pid, log_offset=offset)
        deadline = time.monotonic() + 15
        while any(self.pid_alive(pid) for pid in old_children) and time.monotonic() < deadline:
            time.sleep(0.05)
        require(not any(self.pid_alive(pid) for pid in old_children), "Leader-only crash left an old worker alive")
        _, proof = self.await_events(restarted, [following], snapshot, attempts=2)
        self.record("launchd replaces SIGKILL leader automatically and parent guard stops old worker; task resumes", old_pid=old_pid,
                    new_pid=restarted["process"].pid, old_worker_pids=sorted(old_children), manual_start_after_crash=False, proof=proof)
        final = self.make_event("launchd-after-crash", 13)
        snapshot = self.trace_snapshot()
        self.post_event(restarted, final)
        _, proof = self.await_events(restarted, [final], snapshot)
        self.record("automatically restarted launchd service accepts a new real event", proof=proof)

    def operation_case(self) -> None:
        counter = self.workspace / "external-counter.py"
        counter.write_text('''import json,os,sys,time\nfrom pathlib import Path\nwith Path("external-process.json").open("w") as f:\n json.dump({"pid":os.getpid(),"pgid":os.getpgrp(),"marker":sys.argv[1]},f); f.flush(); os.fsync(f.fileno())\np=Path("external-count.json")\nn=json.loads(p.read_text())["count"]+1 if p.exists() else 1\nr={"count":n,"marker":sys.argv[1],"applied":True}\nfor target in (p,Path("external-receipt.json"),Path("external-effect-ready.json")):\n with target.open("w") as f:\n  json.dump(r,f); f.flush(); os.fsync(f.fileno())\ntime.sleep(60)\nprint(json.dumps(r))\n''', encoding="utf-8")
        event = self.make_event("operation-killed-after-effect", 21)
        marker = json.loads((self.workspace / event["source"]).read_text())["marker"]
        self.external_markers.add(marker)
        command = " ".join(shlex.quote(value) for value in (sys.executable, str(counter), marker))
        arguments = {"command": command, "description": "Apply controlled external counter and receive receipt", "timeout_ms": 90000, "run_in_background": False}
        event["payload"]["prompt"] = ("First use Bash EXACTLY ONCE with these exact JSON arguments (do not add or omit keys): " + json.dumps(arguments) + ". "
            "This calls the controlled external system and returns its receipt. On a recovered attempt use exactly the same first Bash arguments once; a confirmed journal result will be replayed. "
            "Never call this counter a second time in one attempt, never change the script, never use another command to perform its side effect. " + event["payload"]["prompt"])
        service = self.start_service("operation-private-service")
        snapshot = self.trace_snapshot()
        self.post_event(service, event)
        deadline = time.monotonic() + self.args.stage_timeout
        operation = None
        killed = None
        while time.monotonic() < deadline:
            state = self.state_on_disk(self.run_id)
            counter_path = self.workspace / "external-count.json"
            ready_path = self.workspace / "external-effect-ready.json"
            operations = [value for value in state.get("operations", []) if value.get("item_id") == event["item_id"] and value.get("tool_name") == "Bash" and value.get("status") == "started"]
            if counter_path.exists() and ready_path.exists() and operations:
                try:
                    receipt = json.loads(counter_path.read_text())
                    ready = json.loads(ready_path.read_text())
                except (OSError, ValueError):
                    time.sleep(0.03)
                    continue
                require(receipt == {"count": 1, "marker": marker, "applied": True}, "External counter already executed more than once")
                require(ready == receipt, "External effect sentinel does not match its applied receipt")
                external = json.loads((self.workspace / "external-process.json").read_text())
                require(external["marker"] == marker and self.pid_alive(external["pid"]) and os.getpgid(external["pid"]) == external["pgid"], "Controlled external process identity was not retained")
                self.external_pids[external["pid"]] = external
                operation = operations[0]
                children = self.active_child_pids(service)
                require(len(children) == 1, "Counter case has ambiguous owned worker identity")
                killed = next(iter(children))
                os.killpg(killed, signal.SIGKILL)
                break
            time.sleep(0.05)
        require(operation is not None, "Real query did not apply the controlled external operation")
        deadline = time.monotonic() + 20
        while time.monotonic() < deadline:
            state = self.state_on_disk(self.run_id)
            item = next(item for item in state["items"] if item["id"] == event["item_id"])
            current = next(op for op in state.get("operations", []) if op["id"] == operation["id"])
            if (item.get("failure") or {}).get("code") == "operation_uncertain" and current["status"] == "uncertain":
                break
            time.sleep(0.1)
        require((item.get("failure") or {}).get("code") == "operation_uncertain" and state["status"] == "blocked" and current["status"] == "uncertain", "Unreceipted applied effect did not hold work")
        self.await_idle(service)
        time.sleep(2)
        require(json.loads((self.workspace / "external-count.json").read_text())["count"] == 1, "Automatic recovery duplicated the external effect")
        _, rejected = self.command(["coordinator", "recover", self.run_id, event["item_id"], "--cwd", str(self.workspace), "--note", "Unresolved operation must remain blocked", "--json"],
                                   "reject-unresolved-operation-recovery", expect=None)
        require(rejected["exit_code"] != 0, "Unresolved operation was recoverable without trustworthy resolution")
        self.record("real side effect applied before worker SIGKILL; unconfirmed operation blocks retry", operation_id=operation["id"], worker_pid=killed,
                    external_count=1, automatic_duplicate_count=0, failure_code="operation_uncertain", rejected_recovery_exit=rejected["exit_code"])
        receipt_path = self.workspace / "external-receipt.json"
        require(json.loads(receipt_path.read_text()) == {"count": 1, "marker": marker, "applied": True}, "Trusted host receipt does not prove this external effect")
        result_path = self.root / "confirmed-external-result.json"
        atomic_json(result_path, {"Output": receipt_path.read_text(), "IsError": False, "Display": "", "Meta": {"receipt_sha256": sha256(receipt_path)}, "Presentation": None, "Images": None})
        self.coord("operation", "resolve", self.run_id, operation["id"], "--outcome", "applied", "--result-file", str(result_path), "--note", "Host independently checked the fsynced controlled external receipt and exactly one matching counter effect", label="confirm-applied-operation")
        recovery_snapshot = self.trace_snapshot()
        self.coord("recover", self.run_id, event["item_id"], "--note", "External effect was independently confirmed applied and successful full tool result durably supplied", label="recover-confirmed-operation")
        state, proof = self.await_events(service, [event], snapshot, attempts=2)
        confirmed = next(op for op in state["operations"] if op["id"] == operation["id"])
        require(confirmed["status"] == "completed" and confirmed["execution_attempts"] == 1 and json.loads((self.workspace / "external-count.json").read_text())["count"] == 1,
                "Resolved replay sent the external operation again")
        replacement = self.trace_events(recovery_snapshot)
        replay_calls = {}
        for trace in replacement:
            identity = root_trace_identity(trace)
            if identity and trace.get("trace_call_id") and trace.get("kind") in ("tool_start", "tool_args") and trace.get("tool_name") == "Bash":
                try:
                    actual_arguments = json.loads(trace.get("text", ""))
                except (TypeError, ValueError):
                    continue
                if actual_arguments.get("command") == command:
                    replay_calls[(*identity, trace["trace_call_id"])] = trace
        replay_results = []
        for trace in replacement:
            identity = root_trace_identity(trace)
            if not identity or not trace.get("trace_call_id") or (*identity, trace["trace_call_id"]) not in replay_calls or trace.get("kind") != "tool_result" or trace.get("is_error"):
                continue
            try:
                if json.loads(trace.get("text", "")) == json.loads(receipt_path.read_text()):
                    replay_results.append(trace)
            except (TypeError, ValueError):
                pass
        require(replay_results, "Replacement root invocation did not actually receive the matching trusted Bash receipt from durable replay")
        invocations = {root_trace_identity(trace) for trace in replay_results}
        self.record("trusted applied resolution replays the original result to replacement actual query and completes delivery without another effect", external_count=1,
                    operation_id=operation["id"], execution_attempts=1, replacement_root_invocations=len(invocations),
                    replay_trace_call_ids=[trace["trace_call_id"] for trace in replay_results], proof=proof)
        self.stop_service(service)

    def setup_forwarder(self) -> None:
        configuration = tomllib.loads((self.home / "config.toml").read_text())
        providers = configuration["provider"]
        provider = providers["default"]
        custom = provider in providers.get("custom", {})
        parts = ["provider", "custom", provider] if custom else ["provider", provider]
        settings = dict(providers["custom"][provider] if custom else providers[provider])
        require(provider == "easycliproxyapi" and settings.get("model") == "gpt-6-sol" and self.report["transport"] == "anthropic_messages", "Original provider/model/protocol changed; no fallback is authorized")
        key_env = settings.get("api_key_env", "")
        upstream_key = self.environment.get(key_env, "") if key_env else ""
        credentials = self.home / ".credentials" / "auth.json"
        auth = json.loads(credentials.read_text()) if credentials.exists() else {}
        upstream_key = upstream_key or auth.get(provider, {}).get("key", "")
        require(upstream_key, "No original selected-provider authentication")
        sentinel = secrets.token_hex(32)
        self.secrets.append(sentinel)
        self.proxy = ReceiptForwarder(self, settings["base_url"], upstream_key, sentinel)
        settings["base_url"], settings["api_key_env"] = self.proxy.endpoint, "METIS_DURABILITY_SENTINEL"
        for key in list(self.environment):
            if self.environment[key] == upstream_key:
                self.environment.pop(key)
        self.environment["METIS_DURABILITY_SENTINEL"] = sentinel
        credentials.parent.mkdir(mode=0o700, exist_ok=True)
        atomic_json(credentials, {provider: {"type": "api", "key": sentinel, "endpoint": {"provider": provider, "transport": "anthropic_messages", "base_url": self.proxy.endpoint}}})
        original = (self.home / "config.toml").read_text()
        text = toml_table(["provider"], {"default": provider}) + "\n" + toml_table(parts, settings)
        text += "\n" + toml_table(["session"], configuration["session"]) + "\n" + toml_table(["tools"], configuration["tools"])
        text += original[original.index("\n[permission]\n"):]
        (self.home / "config.toml").write_text(text)
        self.proxy.start()

    def usage_record(self, identifier: str) -> dict[str, Any] | None:
        return next((record for record in self.state_on_disk(self.run_id).get("usage_requests", []) if record["client_request_id"] == identifier), None)

    def usage_gate_case(self, service: dict[str, Any], mode: str, number: int) -> tuple[dict[str, Any], dict[str, Any]]:
        self.await_idle(service)
        self.proxy.arm(mode)
        event = self.make_event("usage-" + mode, number)
        snapshot = self.trace_snapshot()
        self.post_event(service, event)
        require(self.proxy.gated.wait(180), "No real message_start usage gate observed")
        observed = deepcopy(self.proxy.target)
        identifier = observed["client_request_id"]
        deadline = time.monotonic() + 10
        while time.monotonic() < deadline:
            record = self.usage_record(identifier)
            ready = record and (record.get("usage_reported") and record.get("response_id") == observed["response_id"] if mode == "after_start" else not record.get("usage_reported"))
            if ready:
                break
            time.sleep(0.03)
        require(ready and record["request_sha256"] == observed["request_sha256"], "Real request/partial usage did not persist before killing worker")
        if mode == "after_start":
            require(record["usage"] == observed["partial_usage"] and sum(record["usage"].values()) > 0, "Partial durable ledger differs from original message_start usage")
        children = self.active_child_pids(service)
        require(len(children) == 1, "Usage gate has ambiguous owned worker")
        killed = next(iter(children))
        killed_at = time.monotonic()
        os.killpg(killed, signal.SIGKILL)
        self.proxy.release.set()
        before = deepcopy(record)
        _, proof = self.await_events(service, [event], snapshot, attempts=2)
        deadline = time.monotonic() + 90
        while time.monotonic() < deadline:
            final = next((entry for entry in self.proxy.observations() if entry.get("client_request_id") == identifier and entry.get("stage") == "upstream_finished"), None)
            if final and not final.get("error_type") and (mode == "before_start" or self.usage_record(identifier).get("status") == "reconciled"):
                break
            time.sleep(0.1)
        require(final and not final.get("error_type") and final.get("message_stop_observed"), "Original killed worker RPC did not reach authoritative final reported usage")
        after = self.usage_record(identifier)
        if mode == "after_start":
            require(after["status"] == "reconciled" and after["usage"] == final["final_usage"] and after["usage"]["output"] > before["usage"]["output"], "Service did not automatically add missing original output usage")
            require(after.get("actual_usd") is None, "Reported token receipt invented an actual bill")
        else:
            self.await_idle(service)
            # Keep the missing-feed request observable across one full default
            # 30-second resident scan period; no request may be silently zeroed.
            while time.monotonic() - killed_at < 35:
                require(service["process"].poll() is None, "Service stopped before missing-feed scan observation")
                code, _ = self.http(service, "GET", "/v1/health")
                require(code == 200, "Resident service stopped responding while receipt feed was absent")
                self.progress("observing-missing-receipt-through-resident-scan", client_request_id=identifier,
                              missing_feed_seconds=round(time.monotonic() - killed_at, 3))
                time.sleep(0.5)
            self.coord("usage", "reconcile", self.run_id, label="reconcile-absent-receipt-feed")
            after = self.usage_record(identifier)
            listing, _ = self.coord("usage", "list", self.run_id, label="usage-unresolved-list")
            require(after["status"] in ("started", "interrupted") and not after.get("usage_reported") and listing["unresolved"] >= 1 and not after.get("receipts"), "Missing provider feed was silently marked reconciled/zero billed")
        self.record("service automatically reconciles original final SSE usage after worker SIGKILL" if mode == "after_start" else "real request killed before usage arrives remains unresolved without a receipt feed",
                    client_request_id=identifier, killed_worker_pid=killed, partial_usage=before["usage"], final_original_usage=final["final_usage"],
                    ledger_after=after["usage"], status=after["status"], upstream_original_frames=final["evidence_path"], actual_usd_known=False, proof=proof)
        self.proxy.reset_gate()
        return before, after

    def authoritative_receipt(self, identifier: str, prefix: str) -> dict[str, Any]:
        observed = next((event for event in self.proxy.observations() if event.get("client_request_id") == identifier and event.get("stage") == "upstream_finished"), None)
        ledger = self.usage_record(identifier)
        require(observed and ledger and not observed.get("error_type") and observed.get("message_stop_observed") and ledger["request_sha256"] == observed["request_sha256"],
                "Private provider-feed receipt does not bind an original completed upstream request")
        evidence = self.output / observed["evidence_path"]
        require(sha256(evidence) == observed["evidence_sha256"], "Original provider usage evidence changed before adapter import")
        receipt = {"receipt_id": prefix + identifier, "evidence_sha256": observed["evidence_sha256"],
                   "authority": "original-gateway-reported-Anthropic-SSE-via-private-host-adapter",
                   "provider": ledger["provider"], "endpoint_sha256": ledger["endpoint_sha256"], "model": ledger["model"],
                   "client_request_id": identifier, "response_id": observed["response_id"], "usage": observed["final_usage"]}
        if observed.get("provider_request_id"):
            receipt["provider_request_id"] = observed["provider_request_id"]
        return receipt

    def usage_adapter_case(self, service: dict[str, Any], unknown: dict[str, Any]) -> dict[str, Any]:
        identifier = unknown["client_request_id"]
        receipt = self.authoritative_receipt(identifier, "adapter-real-sse-")
        feed = self.root / "private-original-provider-feed.json"
        # At-least-once feed delivery repeats the exact same real receipt. Token
        # counts and evidence remain byte-for-byte equivalent; none are invented.
        atomic_json(feed, [receipt, receipt])
        atomic_json(self.output / "adapter-real-provider-feed.json", [receipt, receipt])
        helper = self.root / "trusted-usage-adapter.py"
        self.auxiliary_executables.add(helper)
        audit = self.root / "trusted-usage-adapter-input.json"
        helper.write_text(f'''#!{sys.executable}\nimport json,sys\nfrom pathlib import Path\nrequest=json.load(sys.stdin)\nif request.get("schema")!=1: raise SystemExit(2)\nif any("claim_token" in entry or "worker" in entry for entry in request.get("requests",[])): raise SystemExit(3)\nPath({str(audit)!r}).write_text(json.dumps(request,sort_keys=True))\nids={{entry["client_request_id"] for entry in request.get("requests",[])}}\nreceipts=json.loads(Path({str(feed)!r}).read_text())\njson.dump([receipt for receipt in receipts if receipt["client_request_id"] in ids],sys.stdout)\n''', encoding="utf-8")
        helper.chmod(0o700)
        before = self.state_on_disk(self.run_id)
        before_request_ids = {record["client_request_id"] for record in before.get("usage_requests", [])}
        with self.proxy.lock:
            before_proxy_requests = self.proxy.request_count
        snapshot = self.trace_snapshot()
        self.stop_service(service)
        self.extra_service_arguments = ["--usage-reconcile-command", str(helper)]
        restarted = self.start_service("actual-private-usage-adapter-service")
        deadline = time.monotonic() + 60
        while time.monotonic() < deadline:
            updated = self.usage_record(identifier)
            if updated and updated["status"] == "reconciled" and audit.exists():
                break
            time.sleep(0.1)
        require(updated and updated["status"] == "reconciled" and updated["usage"] == receipt["usage"] and len(updated.get("receipts", [])) == 1,
                "Resident service did not automatically query the private real-receipt adapter and reconcile this unknown request")
        observed_input = json.loads(audit.read_text())
        require(observed_input["run_id"] == self.run_id and any(entry["client_request_id"] == identifier for entry in observed_input["requests"]), "Actual helper stdin did not carry this outstanding request identity")
        require(not any("claim_token" in entry or "worker" in entry for entry in observed_input["requests"]), "Trusted adapter was given worker credentials")
        after = self.state_on_disk(self.run_id)
        expected_delta = updated["budget_usd"] - unknown["budget_usd"]
        require(abs(after["spent_usd"] - before["spent_usd"] - expected_delta) < 1e-9 and updated.get("actual_usd") is None, "Duplicated adapter receipt overcharged the durable budget or invented actual billed cost")
        self.coord("usage", "reconcile", self.run_id, "--command", str(helper), label="repeat-trusted-provider-adapter-reconciliation")
        require(self.state_on_disk(self.run_id)["spent_usd"] == after["spent_usd"] and len(self.usage_record(identifier)["receipts"]) == 1,
                "Repeated actual CLI provider-adapter reconciliation changed budget/receipt cardinality")
        self.await_idle(restarted)
        final_request_ids = {record["client_request_id"] for record in self.state_on_disk(self.run_id).get("usage_requests", [])}
        with self.proxy.lock:
            final_proxy_requests = self.proxy.request_count
        require(before_request_ids == final_request_ids and before_proxy_requests == final_proxy_requests and self.trace_evidence(self.trace_events(snapshot))["model_responses"] == 0,
                "Receipt-adapter service restart issued a provider request, including a failed or zero-token request")
        atomic_json(self.output / "trusted-adapter-input.json", observed_input)
        atomic_json(self.output / "trusted-adapter-manifest.json", {"path": str(helper), "sha256": sha256(helper), "mode": oct(helper.stat().st_mode & 0o777),
                    "feed_sha256": sha256(feed), "original_evidence_sha256": receipt["evidence_sha256"], "returned_identical_receipt_copies": 2,
                    "original_client_request_id": identifier, "actual_model_queries_during_restart": 0})
        self.record("actual compiled CLI restarts resident service with private provider adapter; duplicate original receipts replenish unknown usage once without new model queries",
                    client_request_id=identifier, original_usage=receipt["usage"], adapter_receipt_copies=2, stored_receipt_count=1,
                    estimated_budget_delta_usd=expected_delta, actual_usd_known=False, new_model_queries=0, adapter_sha256=sha256(helper))
        return restarted

    def usage_case(self) -> None:
        self.setup_forwarder()
        service = self.start_service("usage-private-service")
        _, reconciled = self.usage_gate_case(service, "after_start", 31)
        self.await_idle(service)
        identifier = reconciled["client_request_id"]
        receipt = json.loads((self.output / ("imported-receipt-" + identifier + ".json")).read_text())
        before = self.state_on_disk(self.run_id)
        inbox = self.home / "coordinator-usage-receipts" / self.run_id
        duplicate_path = inbox / "same-authoritative-receipt-again.json"
        atomic_json(duplicate_path, receipt)
        duplicate_bytes = duplicate_path.read_bytes()
        # Explicit reconcile triggers the same production inbox path; original
        # first import above was done automatically by resident service.
        self.coord("usage", "reconcile", self.run_id, label="reconcile-identical-receipt")
        after = self.state_on_disk(self.run_id)
        processed = inbox / "processed" / (hashlib.sha256(duplicate_bytes).hexdigest() + ".json")
        require(not duplicate_path.exists() and processed.is_file() and processed.read_bytes() == duplicate_bytes,
                "Duplicate provider receipt was ignored rather than consumed and archived")
        require(after["spent_usd"] == before["spent_usd"] and self.usage_record(identifier)["usage"] == reconciled["usage"] and len(self.usage_record(identifier)["receipts"]) == 1,
                "Duplicate authoritative receipt increased usage or budget")
        mismatch = {**receipt, "receipt_id": "wrong-id-" + identifier, "response_id": "deliberately-unrelated-response"}
        bad_path = self.root / "wrong-provider-receipt.json"
        atomic_json(bad_path, mismatch)
        _, proof = self.command(["coordinator", "usage", "import", self.run_id, "--receipt-file", str(bad_path), "--cwd", str(self.workspace), "--json"], "reject-mismatching-provider-receipt", expect=None)
        error_text = (self.output / proof["stderr"]).read_text()
        require(proof["exit_code"] != 0 and "receipt_conflict" in error_text and self.state_on_disk(self.run_id)["spent_usd"] == before["spent_usd"] and self.usage_record(identifier) == reconciled,
                "Mismatched receipt ID did not fail specifically at identity binding or changed the target ledger")
        self.record("identical original provider receipt is idempotent; mismatched response identity is rejected", receipt_count=1, duplicate_budget_delta_usd=0, rejected_exit_code=proof["exit_code"])
        _, unknown = self.usage_gate_case(service, "before_start", 32)
        service = self.usage_adapter_case(service, unknown)
        self.stop_service(service)

    def uninstall_managed(self) -> None:
        identifier = self.report.get("launchd_id")
        if not identifier:
            return
        # Retrieve the persisted manifest if installation failed after bootstrap.
        if self.managed is None:
            status, _ = self.service_control("status", identifier, label="cleanup-find-private-launchd", expect=None)
            self.managed = (status or {}).get("manifest")
        if self.managed:
            for service in self.managed_services:
                self.service_events(service)
            for filename in ("stdout.log", "stderr.log"):
                source = Path(self.managed["directory"]) / filename
                if source.exists():
                    (self.output / ("launchd-" + filename)).write_text(self.redact(source.read_text(encoding="utf-8", errors="replace")))
        _, proof = self.service_control("uninstall", identifier, label="uninstall-private-launchd", expect=None)
        require(proof["exit_code"] == 0, "Could not uninstall private launchd job; do not modify source before completing cleanup")
        status, _ = self.service_control("status", identifier, label="verify-private-launchd-uninstalled")
        require(not status.get("installed") and not status.get("loaded"), "Private launchd job remains installed/loaded")
        if self.managed:
            require(not Path(self.managed["directory"]).exists() and not Path(self.managed["plist"]).exists(), "Managed CLI/token/plist was not destroyed")
        self.report["launchd_cleanup"] = {"id": identifier, "loaded": False, "installed": False, "stable_binary_removed": True, "stable_token_removed": True, "plist_removed": True}

    def stop_external_processes(self) -> None:
        # Bash subprocesses may own a separate process group from their worker.
        # Retain and verify the controlled endpoint's unique marker before kill.
        path = self.workspace / "external-process.json"
        if path.is_file() and not path.is_symlink():
            try:
                latest = json.loads(path.read_text())
            except (OSError, ValueError):
                latest = {}  # The complete owned-process inventory below remains authoritative.
            if latest.get("marker") in self.external_markers and type(latest.get("pid")) is int and type(latest.get("pgid")) is int:
                self.external_pids[latest["pid"]] = latest
        # A broken implementation might duplicate the endpoint before the first
        # PID was captured, so inspect every process with this private absolute
        # script path and this run's random marker, never arbitrary Python.
        inventory = subprocess.run(["ps", "-axo", "pid=,pgid=,command="], capture_output=True, text=True, timeout=10)
        require(inventory.returncode == 0, "Cannot inventory owned external endpoint processes for cleanup")
        script = str(self.workspace / "external-counter.py")
        for line in inventory.stdout.splitlines():
            parts = line.strip().split(None, 2)
            if len(parts) != 3 or script not in parts[2]:
                continue
            matching = next((marker for marker in self.external_markers if marker in parts[2]), None)
            if matching and parts[0].isdigit() and parts[1].isdigit():
                pid, pgid = int(parts[0]), int(parts[1])
                if pid > 1 and pgid > 1:
                    self.external_pids[pid] = {"pid": pid, "pgid": pgid, "marker": matching}
        for pid, external in self.external_pids.items():
            if not self.pid_alive(pid):
                continue
            command = subprocess.run(["ps", "-p", str(pid), "-o", "command="], capture_output=True, text=True, timeout=5).stdout
            if str(self.workspace / "external-counter.py") in command and external["marker"] in command:
                try:
                    if os.getpgid(pid) == external["pgid"] and external["pgid"] != os.getpgrp():
                        os.killpg(external["pgid"], signal.SIGKILL)
                except ProcessLookupError:
                    pass
        deadline = time.monotonic() + 10
        while any(self.pid_alive(pid) for pid in self.external_pids) and time.monotonic() < deadline:
            time.sleep(0.05)

    def stop_managed_survivors(self) -> None:
        if self.managed and self.report.get("launchd_cleanup", {}).get("loaded") is False:
            # Stable launchd executables differ from the original test binary.
            # Stop any audited survivor only after the job is confirmed unloaded.
            managed_path = self.managed["executable"]
            for pid in self.child_pids | self.owned_managed_pids:
                if not self.pid_alive(pid):
                    continue
                inspected = subprocess.run(["ps", "-p", str(pid), "-o", "command="], capture_output=True, text=True, timeout=5)
                if managed_path in inspected.stdout:
                    try:
                        os.killpg(pid, signal.SIGKILL)
                    except ProcessLookupError:
                        pass
            deadline = time.monotonic() + 10
            while any(self.pid_alive(pid) for pid in self.child_pids | self.owned_managed_pids) and time.monotonic() < deadline:
                time.sleep(0.05)

    def cleanup(self) -> None:
        errors = []
        for action in (self.stop_external_processes, self.uninstall_managed, self.stop_managed_survivors):
            try:
                action()
            except BaseException as error:
                errors.append(self.redact(str(error)))
        try:
            if self.proxy:
                self.proxy.stop()
                atomic_json(self.output / "usage-forwarder-audit.json", self.proxy.observations())
        except BaseException as error:
            errors.append(self.redact(str(error)))
        # Managed services are deliberately not added to Popen-owned services;
        # uninstall is their only authorized lifecycle controller.
        try:
            super().cleanup()
        except BaseException as error:
            errors.append(self.redact(str(error)))
        for executable in self.auxiliary_executables:
            try:
                executable.unlink(missing_ok=True)
            except BaseException as error:
                errors.append(self.redact(str(error)))
        self.report.setdefault("cleanup", {}).update({"owned_managed_pids": sorted(self.owned_managed_pids),
            "all_managed_pids_stopped": not any(self.pid_alive(pid) for pid in self.owned_managed_pids), "controlled_external_pids": sorted(self.external_pids),
            "all_controlled_external_pids_stopped": not any(self.pid_alive(pid) for pid in self.external_pids),
            "trusted_adapter_executables_destroyed": not any(path.exists() for path in self.auxiliary_executables)})
        require(not errors and self.report["cleanup"]["all_managed_pids_stopped"] and self.report["cleanup"]["all_controlled_external_pids_stopped"] and self.report["cleanup"]["trusted_adapter_executables_destroyed"], "Durability cleanup incomplete: " + "; ".join(errors))

    def run(self) -> int:
        # A stalled provider cannot extend a single acceptance case indefinitely.
        # The alarm is disabled before cleanup, so it cannot interrupt deletion
        # of a loaded job's stable executable/token or copied credentials.
        signal.setitimer(signal.ITIMER_REAL, self.args.case_timeout)
        try:
            self.setup_provider()
            require(self.report["provider"] == "easycliproxyapi" and self.report["model"] == "gpt-6-sol" and self.report["transport"] == "anthropic_messages", "Original selected provider/model/protocol changed")
            self.build()
            self.write_checker()
            self.setup_service()
            if self.args.case in ("launchd", "all"):
                self.launchd_case()
                self.uninstall_managed()
                self.report.pop("launchd_id", None)
            if self.args.case in ("operation", "all"):
                self.operation_case()
            if self.args.case in ("usage", "all"):
                self.usage_case()
            self.assert_profile_fidelity()
            self.report["passed"] = True
        except BaseException as error:
            self.report["failure"] = self.redact(str(error))
            self.report["failure_type"] = type(error).__name__
        finally:
            signal.setitimer(signal.ITIMER_REAL, 0)
            for name, action in (("cleanup", self.cleanup), ("evidence_copy", self.copy_evidence)):
                try:
                    action()
                except BaseException as error:
                    self.report["passed"] = False
                    self.report[name + "_failure"] = self.redact(str(error))
            self.report.update({"elapsed_seconds": round(time.monotonic() - self.started, 3), "finished_utc": utc_now()})
            atomic_json(self.output / "verification-report.json", json.loads(self.redact(json.dumps(self.report, ensure_ascii=False))))
            self.progress("finished", passed=self.report["passed"], failure=self.report.get("failure"), cleanup=self.report.get("cleanup"))
        print(json.dumps({"passed": self.report["passed"], "case": self.args.case, "checks": len(self.report["checks"]), "report": str(self.output / "verification-report.json"), "failure": self.report.get("failure"), "cleanup": self.report.get("cleanup")}, ensure_ascii=False), flush=True)
        return 0 if self.report["passed"] else 1


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("--repo", type=Path, default=Path(__file__).resolve().parents[2])
    parser.add_argument("--output", type=Path, required=True)
    parser.add_argument("--case", choices=("launchd", "operation", "usage", "all"), default="all")
    parser.add_argument("--source-freeze", type=Path, help="runtime source file-hash manifest to verify before compiling")
    parser.add_argument("--stage-timeout", type=float, default=600)
    parser.add_argument("--case-timeout", type=float, default=540, help="overall deadline before guaranteed finally cleanup; at most 600 seconds")
    args = parser.parse_args()
    require(args.stage_timeout > 0, "Stage timeout must be positive")
    require(0 < args.case_timeout <= 600, "Case timeout must be positive and at most 600 seconds")
    os.umask(0o077)

    def interrupted(signum: int, _frame: Any) -> None:
        raise KeyboardInterrupt("Harness interrupted by " + signal.Signals(signum).name)

    signal.signal(signal.SIGINT, interrupted)
    signal.signal(signal.SIGTERM, interrupted)
    def timed_out(_signum: int, _frame: Any) -> None:
        raise TimeoutError("Bounded real-CLI case exceeded its configured deadline")
    signal.signal(signal.SIGALRM, timed_out)
    return DurabilityLiveCheck(args).run()


if __name__ == "__main__":
    raise SystemExit(main())
