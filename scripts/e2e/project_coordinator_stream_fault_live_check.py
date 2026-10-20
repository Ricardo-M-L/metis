#!/usr/bin/env python3
"""Real-query verification of bounded recovery from an Anthropic SSE error.

Builds a fresh isolated CLI using the currently selected provider and model.
Only the temporary client endpoint changes: a loopback forwarder streams all
ordinary RPCs to the original fixed upstream. The CLI uses a temporary sentinel
key; the real key stays in forwarder memory and is sent only to that upstream.
Request bodies and authentication headers are never logged.

After the chosen work item has real provider tokens and a successful Read, its
next RPC receives HTTP 200 / text/event-stream / event:error containing the
exact error JSON observed in soak-02. This is transport fault injection, not a
model completion. It deliberately does NOT inject HTTP 400: typed HTTP errors
and a JSON error carried inside an SSE frame have different recovery contracts.
--fault-after-real-start also sends that RPC to the real upstream and forwards
its original message_start/usage frame before injecting the error at the frame
boundary. Nonzero usage must appear in the failed attempt's real trace and
durable budget; no token values or completion frames are invented.

Default --fault-phase verification reproduces the observed long-task failure.
Use --fault-phase implementation or both to cover another work phase. Successful
acceptance requires two distinct claim tokens, recoverable cause evidence,
renewals, rejected old-token writes, actual model-written output, host checks
and a completed graph. No manual recover command is used. On any failure the
test CLI, copied credentials, sentinel key and forwarder are destroyed first.
Requires Python 3.11+ and Go. Run only after reviewing the production fix.
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
import secrets
import signal
import socket
import threading
import time
import tomllib
from typing import Any, Callable
from urllib.parse import urlsplit

from project_coordinator_live_check import LiveCheck, atomic_json, require, root_trace_identity, safe_endpoint, toml_table, trace_timestamp, utc_now


OBSERVED_ERROR = {
    "type": "error", "error": {
        "type": "invalid_request_error",
        "message": "stream error: stream disconnected before completion: stream closed before response.completed",
    },
}
ERROR_JSON = json.dumps(OBSERVED_ERROR, separators=(",", ":"))
ERROR_FRAME = ("event: error\ndata: " + ERROR_JSON + "\n\n").encode("utf-8")
HOP_HEADERS = {"connection", "keep-alive", "proxy-authenticate", "proxy-authorization", "te", "trailer", "transfer-encoding", "upgrade", "host", "content-length"}


class FaultForwarder:
    def __init__(self, upstream: str, upstream_key: str, model: str, sentinel: str,
                 fault_condition: Callable[[], dict[str, Any] | None], *, fault_after_real_start: bool = False) -> None:
        parsed = urlsplit(upstream)
        require(parsed.scheme in ("http", "https") and parsed.hostname and not parsed.username and not parsed.query and not parsed.fragment,
                "Upstream must be the fixed configured HTTP(S) endpoint without embedded credentials/query")
        self.upstream = parsed
        self.upstream_key, self.model, self.sentinel = upstream_key, model, sentinel
        self.fault_condition = fault_condition
        self.fault_after_real_start = fault_after_real_start
        self.lock = threading.Lock()
        self.events: list[dict[str, Any]] = []
        self.connections: set[http.client.HTTPConnection] = set()
        self.responses: set[http.client.HTTPResponse] = set()
        self.clients: set[socket.socket] = set()
        self.closing = False

        class Handler(BaseHTTPRequestHandler):
            protocol_version = "HTTP/1.1"

            def do_POST(handler: BaseHTTPRequestHandler) -> None:
                self.handle(handler)

            def log_message(handler: BaseHTTPRequestHandler, *_args: Any) -> None:
                pass  # Never print raw paths, headers, request bodies or keys.

        self.server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        self.server.daemon_threads = True
        self.server.block_on_close = False
        self.thread = threading.Thread(target=self.server.serve_forever, kwargs={"poll_interval": 0.1}, daemon=True)
        self.client_endpoint = "http://127.0.0.1:" + str(self.server.server_address[1])

    def start(self) -> None:
        self.thread.start()

    @staticmethod
    def send_bytes(handler: BaseHTTPRequestHandler, code: int, body: bytes, content_type: str = "application/json") -> None:
        handler.send_response(code)
        handler.send_header("Content-Type", content_type)
        handler.send_header("Content-Length", str(len(body)))
        handler.send_header("Connection", "close")
        handler.end_headers()
        handler.wfile.write(body)
        handler.wfile.flush()
        handler.close_connection = True

    def handle(self, handler: BaseHTTPRequestHandler) -> None:
        with self.lock:
            self.clients.add(handler.connection)
        connection = None
        response = None
        event: dict[str, Any] = {"utc": utc_now(), "method": "POST", "path": "/v1/messages", "stage": "local_authorization"}
        try:
            require(not self.closing, "Forwarder is stopping")
            if handler.path != "/v1/messages" or not secrets.compare_digest(handler.headers.get("x-api-key", ""), self.sentinel):
                event["kind"] = "rejected_local_request"
                self.send_bytes(handler, 401, b'{"error":"local_forwarder_unauthorized"}')
                return
            length = int(handler.headers.get("Content-Length", "0"))
            event["stage"] = "request_validation"
            require(0 < length <= 16 * 1024 * 1024, "Unexpected model RPC size")
            raw = handler.rfile.read(length)
            body = json.loads(raw)
            require(isinstance(body, dict) and body.get("model") == self.model,
                    "Forwarder only accepts the original fixed model's Messages RPCs")
            streaming = body.get("stream", False)
            require(isinstance(streaming, bool), "Messages stream flag must be boolean when present")
            event.update({"model": self.model, "request_bytes": len(raw), "stream": streaming, "stage": "fault_selection"})
            # Optional same-model memory enrichment uses non-stream Messages.
            # It is transparently forwarded and never arms or receives a fault.
            fault = self.fault_condition() if streaming else None
            if fault is not None and not self.fault_after_real_start:
                event.update({"kind": "injected_sse_error", "status": 200, "sse_event": "error",
                              "fault_json": OBSERVED_ERROR, "frame_sha256": hashlib.sha256(ERROR_FRAME).hexdigest(),
                              "upstream_request_sent": False, **fault})
                self.send_bytes(handler, 200, ERROR_FRAME, "text/event-stream")
                return
            event["kind"] = "injected_sse_error" if fault is not None else "real_upstream_forward"
            connection_type = http.client.HTTPSConnection if self.upstream.scheme == "https" else http.client.HTTPConnection
            connection = connection_type(self.upstream.hostname, self.upstream.port, timeout=300)
            with self.lock:
                self.connections.add(connection)
            headers = {name: value for name, value in handler.headers.items()
                       if name.lower() not in HOP_HEADERS | {"authorization", "x-api-key"} and
                       not (fault is not None and name.lower() == "accept-encoding")}
            headers["x-api-key"] = self.upstream_key
            headers["Content-Length"] = str(len(raw))
            headers["Connection"] = "close"
            if fault is not None:
                headers["Accept-Encoding"] = "identity"  # Only transport negotiation; body/model unchanged.
            target = self.upstream.path.rstrip("/") + "/v1/messages"
            event["stage"] = "upstream_request"
            connection.request("POST", target, body=raw, headers=headers)
            response = connection.getresponse()
            with self.lock:
                self.responses.add(response)
            event.update({"status": response.status, "upstream_request_sent": True})
            event["stage"] = "upstream_response"
            if fault is not None:
                require(response.status == 200 and "text/event-stream" in response.getheader("Content-Type", ""),
                        "Real fault RPC did not return an upstream SSE stream")
                require(response.getheader("Content-Encoding", "").strip().lower() in ("", "identity"),
                        "Upstream ignored identity negotiation; compressed SSE cannot be parsed at a message_start boundary")
            handler.send_response(response.status)
            for name, value in response.getheaders():
                if name.lower() not in HOP_HEADERS:
                    handler.send_header(name, value)
            handler.send_header("Connection", "close")
            handler.end_headers()
            forwarded = 0
            buffered = b""
            event["stage"] = "response_forwarding"
            while True:
                chunk = response.read1(65536)  # Preserve incremental SSE delivery.
                if not chunk:
                    break
                if fault is not None:
                    buffered += chunk
                    require(len(buffered) <= 1024 * 1024, "No upstream message_start frame within the first MiB")
                    boundary = buffered.find(b"\n\n")
                    separator_size = 2
                    if boundary < 0:
                        boundary = buffered.find(b"\r\n\r\n")
                        separator_size = 4
                    while boundary >= 0:
                        frame, buffered = buffered[:boundary + separator_size], buffered[boundary + separator_size:]
                        # Forward original bytes, including usage; no replacement
                        # message_start, token frame or completion is invented.
                        handler.wfile.write(frame)
                        handler.wfile.flush()
                        forwarded += len(frame)
                        data = b"\n".join(line[5:].lstrip() for line in frame.replace(b"\r\n", b"\n").splitlines() if line.startswith(b"data:"))
                        try:
                            parsed_frame = json.loads(data) if data else {}
                        except ValueError:
                            parsed_frame = {}
                        if parsed_frame.get("type") == "message_start":
                            original_usage = (parsed_frame.get("message") or {}).get("usage") or {}
                            usage = {name: original_usage.get(name, 0) for name in
                                     ("input_tokens", "output_tokens", "cache_creation_input_tokens", "cache_read_input_tokens")}
                            require(all(isinstance(value, int) and value >= 0 for value in usage.values()), "Upstream message_start usage is invalid")
                            handler.wfile.write(ERROR_FRAME)
                            handler.wfile.flush()
                            event.update({"sse_event": "error", "fault_json": OBSERVED_ERROR,
                                          "frame_sha256": hashlib.sha256(ERROR_FRAME).hexdigest(),
                                          "real_message_start_forwarded": True, "reported_partial_usage": usage,
                                          "message_start_frame_sha256": hashlib.sha256(frame).hexdigest(),
                                          "response_bytes_forwarded_before_fault": forwarded,
                                          "fault_after_real_start": True, **fault})
                            handler.close_connection = True
                            response.close()
                            return
                        require(parsed_frame.get("type") not in ("message_stop", "content_block_start", "error"),
                                "Upstream stream reached content/end/error before a message_start frame")
                        boundary = buffered.find(b"\n\n")
                        separator_size = 2
                        if boundary < 0:
                            boundary = buffered.find(b"\r\n\r\n")
                            separator_size = 4
                    continue
                handler.wfile.write(chunk)
                handler.wfile.flush()
                forwarded += len(chunk)
            event["response_bytes_forwarded"] = forwarded
            require(fault is None, "Upstream ended before the real message_start injection boundary")
            response.close()
            handler.close_connection = True
        except BaseException as error:
            # Never include exception text, URL headers or raw upstream content
            # in diagnostics. A real socket failure is separate from injection.
            event.update({"forwarder_error_type": type(error).__name__, "during_shutdown": self.closing})
            handler.close_connection = True
        finally:
            if connection is not None:
                connection.close()
            with self.lock:
                if connection is not None:
                    self.connections.discard(connection)
                if response is not None:
                    self.responses.discard(response)
                self.clients.discard(handler.connection)
                self.events.append(event)

    def audit(self) -> list[dict[str, Any]]:
        with self.lock:
            return deepcopy(self.events)

    def stop(self) -> None:
        self.closing = True
        with self.lock:
            clients, connections, responses = list(self.clients), list(self.connections), list(self.responses)
        for response in responses:
            try:
                stream_socket = getattr(getattr(getattr(response, "fp", None), "raw", None), "_sock", None)
                if stream_socket is not None:
                    stream_socket.shutdown(socket.SHUT_RDWR)
                response.close()
            except OSError:
                pass
        for connection in connections:
            try:
                if connection.sock:
                    connection.sock.shutdown(socket.SHUT_RDWR)
                connection.close()
            except OSError:
                pass
        for client in clients:
            try:
                client.shutdown(socket.SHUT_RDWR)
                client.close()
            except OSError:
                pass
        if self.thread.is_alive():
            self.server.shutdown()
        self.server.server_close()
        if self.thread.ident is not None:
            self.thread.join(timeout=5)
        self.upstream_key = ""
        deadline = time.monotonic() + 5
        while time.monotonic() < deadline:
            with self.lock:
                active = bool(self.clients or self.connections or self.responses)
            if not active:
                break
            time.sleep(0.05)


class StreamFaultLiveCheck(LiveCheck):
    def __init__(self, args: argparse.Namespace) -> None:
        super().__init__(args)
        self.proxy: FaultForwarder | None = None
        self.case: dict[str, Any] | None = None
        self.report.update({"mode": "stream-fault-live", "transport_fault_injection": True, "model_completions_mocked": False,
                            "boundary": "One HTTP200 Anthropic SSE error frame is injected after real work. All other model RPCs are transparently forwarded to the original configured upstream/model; no manual task recovery or model/provider substitution."})
        for key in ("requested_soak_seconds", "effective_soak_seconds", "long_task_acceptance"):
            self.report.pop(key, None)

    def selected_settings(self, configuration: dict[str, Any]) -> tuple[list[str], dict[str, Any]]:
        provider = configuration["provider"]
        selected = provider["default"]
        custom = selected in provider.get("custom", {})
        parts = ["provider", "custom", selected] if custom else ["provider", selected]
        return parts, dict(provider["custom"][selected] if custom else provider[selected])

    def setup_forwarder(self) -> None:
        configuration = tomllib.loads((self.home / "config.toml").read_text())
        parts, settings = self.selected_settings(configuration)
        require(self.report["transport"] == "anthropic_messages", "This exact fault protocol requires the currently selected Anthropic Messages transport; no fallback is attempted")
        upstream = settings.get("base_url")
        require(upstream, "Current provider must explicitly configure the original upstream URL")
        key_env = settings.get("api_key_env", "")
        upstream_key = self.environment.get(key_env, "") if key_env else ""
        credential_path = self.home / ".credentials" / "auth.json"
        auth = json.loads(credential_path.read_text()) if credential_path.exists() else {}
        if not upstream_key:
            upstream_key = auth.get(self.report["provider"], {}).get("key", "")
        require(upstream_key, "No isolated selected-provider key was resolved")
        sentinel = secrets.token_hex(32)
        self.secrets.append(sentinel)
        self.proxy = FaultForwarder(upstream, upstream_key, settings["model"], sentinel, self.fault_condition,
                                    fault_after_real_start=self.args.fault_after_real_start)
        for key in list(self.environment):
            if self.environment[key] == upstream_key:
                self.environment.pop(key)
        settings["base_url"] = self.proxy.client_endpoint
        settings["api_key_env"] = "METIS_STREAM_FAULT_CLIENT_KEY"
        self.environment["METIS_STREAM_FAULT_CLIENT_KEY"] = sentinel
        credentials = self.home / ".credentials"
        credentials.mkdir(mode=0o700, exist_ok=True)
        atomic_json(credential_path, {self.report["provider"]: {"type": "api", "key": sentinel,
                    "endpoint": {"provider": self.report["provider"], "transport": "anthropic_messages", "base_url": self.proxy.client_endpoint}}})
        original_text = (self.home / "config.toml").read_text()
        permission_position = original_text.index("\n[permission]\n")
        text = toml_table(["provider"], {"default": self.report["provider"]}) + "\n" + toml_table(parts, settings)
        text += "\n" + toml_table(["session"], configuration["session"])
        text += "\n" + toml_table(["tools"], configuration["tools"])
        text += original_text[permission_position:]
        (self.home / "config.toml").write_text(text)
        (self.home / "config.toml").chmod(0o600)
        self.report.update({"upstream_endpoint": safe_endpoint(upstream), "client_fault_injection_endpoint": self.proxy.client_endpoint,
                            "endpoint_changed_only_in_temporary_home": True, "upstream_provider_model_unchanged": True,
                            "fault_after_real_start": self.args.fault_after_real_start,
                            "partial_usage_boundary": "Only original usage reported before the injected error is checked; catalog estimates are not final upstream billing.",
                            "upstream_key_sent_only_to_fixed_upstream": True, "fault_protocol": {"http_status": 200, "content_type": "text/event-stream", "sse_event": "error", "json": OBSERVED_ERROR}})
        self.proxy.start()

    def item_trace_events(self, item_id: str, snapshot: dict[str, int]) -> list[dict[str, Any]]:
        case = self.case
        if not case or case["phase"] != item_id or "target_snapshot" not in case:
            return []
        claimed_at = trace_timestamp(case.get("target_claimed_at"))
        events = [event for event in self.trace_events(case["target_snapshot"])
                  if root_trace_identity(event) and trace_timestamp(event.get("ts")) is not None and
                  claimed_at is not None and trace_timestamp(event.get("ts")) >= claimed_at]
        # Context/info/prose can be emitted at a session boundary under the
        # previous owner. Pin only from actual model accounting or tool results.
        identities = {root_trace_identity(event) for event in events if event.get("kind") == "tokens" or
                      (event.get("kind") == "tool_result" and not event.get("is_error"))}
        case["target_candidate_trace_identities"] = [list(identity) for identity in sorted(identities)]
        identity = case.get("target_trace_identity")
        if identity is None:
            if len(identities) != 1:
                return []  # An ambiguous trace cannot arm an intentional fault.
            identity = next(iter(identities))
            case["target_trace_identity"] = identity
        return [event for event in events if root_trace_identity(event) == identity]

    def save_case_observation(self) -> None:
        case = self.case
        if not case or not hasattr(self, "report"):
            return
        # Do not serialize old_item/lease or raw trace bodies into diagnostics.
        self.report.setdefault("case_observations", {})[case["phase"]] = {
            key: deepcopy(case.get(key)) for key in ("phase", "run_id", "target_snapshot", "target_claimed_at",
                "target_claim_token_sha256", "target_trace_identity", "target_candidate_trace_identities",
                "target_trace_counts", "injected")
        }

    def fault_condition(self) -> dict[str, Any] | None:
        case = self.case
        if not case or case.get("injected"):
            return None
        state = self.state_on_disk(case["run_id"])
        item = next((item for item in state.get("items", []) if item["id"] == case["phase"]), None)
        if not item or item.get("status") != "running" or not item.get("lease") or item.get("attempts") != 1:
            return None
        token_hash = hashlib.sha256(item["lease"]["token"].encode()).hexdigest()
        if "target_snapshot" not in case:
            # The first target STREAM RPC is intercepted before its upstream
            # request. One sequential worker means all subsequent native trace
            # entries belong to this claim, without a synthetic user event.
            case.update({"target_snapshot": self.trace_snapshot(),
                         "target_claimed_at": item["lease"].get("claimed_at"),
                         "target_claim_token_sha256": token_hash})
            self.save_case_observation()
            return None
        if case.get("target_claim_token_sha256") != token_hash:
            return None
        evidence = self.trace_evidence(self.item_trace_events(case["phase"], case["snapshot"]))
        case["target_trace_counts"] = {"model_responses": evidence["model_responses"],
                                      "successful_Read_results": evidence["tool_results"].get("Read", 0)}
        self.save_case_observation()
        if evidence["model_responses"] == 0 or evidence["tool_results"].get("Read", 0) == 0:
            return None
        case["old_item"] = deepcopy(item)
        case["injected"] = True
        case["evidence_before_fault"] = evidence
        case["trace_ids_before_fault"] = {event.get("id") for event in self.item_trace_events(case["phase"], case["snapshot"])}
        self.save_case_observation()
        return {"target_phase": case["phase"], "run_id": case["run_id"], "item_id": item["id"],
                "attempt_before_fault": item["attempts"], "claim_token_sha256": hashlib.sha256(item["lease"]["token"].encode()).hexdigest(),
                "real_model_evidence_before_injection": evidence}

    def run_case(self, phase: str) -> None:
        run, source, delivery = self.create_recovery_run("stream-" + phase)
        snapshot = self.trace_snapshot()
        case: dict[str, Any] = {"phase": phase, "run_id": run["id"], "snapshot": snapshot, "injected": False}
        self.case = case
        worker = "same-name-stream-fault-" + phase
        process = self.spawn(["coordinator", "run", run["id"], "--worker", worker, "--lease", "2s", "--until-idle", "--cwd", str(self.workspace), "--json"], "actual-sse-fault-" + phase)
        started = time.monotonic()
        expiries: dict[str, set[str]] = {}
        observed_retries: dict[str, dict[str, Any]] = {}
        requeue_wait: dict[str, float] = {}
        fenced = False
        last_progress = 0.0
        while process.poll() is None:
            require(time.monotonic() - started < self.args.stage_timeout, "Real SSE recovery case exceeded its stage timeout")
            state = self.state_on_disk(run["id"])
            items = {item["id"]: item for item in state.get("items", [])}
            self.guard_work_states(items, {"implementation", "verification"}, "SSE recovery", requeue_wait)
            target = items.get(phase, {})
            for item in items.values():
                if item.get("status") == "running" and item.get("lease"):
                    key = item["id"] + ":" + hashlib.sha256(item["lease"]["token"].encode()).hexdigest()
                    expiries.setdefault(key, set()).add(item["lease"]["expires_at"])
            if target.get("attempts", 0) > 1:
                self.capture_stream_retry(state, target, observed_retries, snapshot)
                if not fenced and target.get("status") == "running" and target.get("lease") and case.get("old_item"):
                    self.assert_stale_fencing(run["id"], case["old_item"], target, live=True)
                    fenced = True
            if time.monotonic() - last_progress >= 5:
                self.progress("actual-sse-fault-recovery", run_id=run["id"], work_phase=phase, injected=case["injected"], attempts=target.get("attempts"), old_token_fenced=fenced)
                last_progress = time.monotonic()
            time.sleep(0.1)
        text, command = self.finish(process, timeout=self.args.stage_timeout)
        state = json.loads(text)["run"]
        target = next(item for item in state["items"] if item["id"] == phase)
        self.capture_stream_retry(state, target, observed_retries, snapshot)
        require(case["injected"] and fenced and phase in observed_retries, "SSE fault/retry/fencing was not actually observed")
        evidence = self.trace_evidence(self.trace_events(snapshot))
        error_events = [event for event in self.trace_events(snapshot) if event.get("kind") == "error" and "stream disconnected before completion" in event.get("text", "")]
        require(error_events, "Injected SSE error did not cross the actual agent trace boundary")
        self.assert_completed(state, source, delivery, evidence, implementation_id="implementation",
                              expected_attempts=2 if phase == "implementation" else 1,
                              expected_verification_attempts=2 if phase == "verification" else 1)
        require(all(item["attempts"] == (2 if item["id"] == phase else 1) for item in state["items"]), "Unrelated nodes were retried")
        require(max((len(values) for values in expiries.values()), default=0) >= 3, "Automatic lease renewals were not observed")
        faults = [event for event in self.proxy.audit() if event.get("kind") == "injected_sse_error" and event.get("run_id") == run["id"]]
        require(len(faults) == 1, "Fault injection was not exactly once for this case")
        partial_proof = None
        if self.args.fault_after_real_start:
            fault = faults[0]
            usage = fault.get("reported_partial_usage") or {}
            require(fault.get("upstream_request_sent") and fault.get("real_message_start_forwarded") and sum(usage.values()) > 0,
                    "The real message_start contained no nonzero partial usage; partial-usage coverage is unproven")
            expected_tokens = {"input": usage["input_tokens"], "output": usage["output_tokens"],
                               "cache_write": usage["cache_creation_input_tokens"], "cache_read": usage["cache_read_input_tokens"]}
            partial_token_events = []
            for event in self.trace_events(snapshot):
                if event.get("kind") != "tokens" or event.get("id") in case["trace_ids_before_fault"] or event.get("trace_invocation_id") not in case["evidence_before_fault"]["invocation_ids"]:
                    continue
                counters = self.trace_evidence([event])["tokens"]
                if {name: counters.get(name, 0) for name in expected_tokens} == expected_tokens:
                    partial_token_events.append(event)
            require(partial_token_events, "Actual failed attempt trace did not preserve the upstream message_start usage")
            usage_records = target.get("usage", [])
            old_usage = next(record for record in usage_records if record["token"] == case["old_item"]["lease"]["token"])
            before_spend = case["old_item"].get("spent_usd", 0)
            require(old_usage.get("spent_usd", 0) >= before_spend, "Failed attempt's earlier cumulative spend was reset")
            if before_spend > 0:
                require(old_usage.get("spent_usd", 0) > before_spend, "Priced partial failed-stream usage was not added to the old attempt")
            require(abs(target.get("spent_usd", 0) - sum(record.get("spent_usd", 0) for record in usage_records)) < 1e-8,
                    "Old/new attempt usage does not match durable cumulative item spend")
            partial_proof = {"original_upstream_usage": usage, "matching_failed_attempt_token_events": len(partial_token_events),
                             "old_attempt_estimated_spend_before_fault": before_spend,
                             "old_attempt_estimated_spend_after_fault": old_usage.get("spent_usd", 0),
                             "item_cumulative_estimated_spend": target.get("spent_usd", 0),
                             "billing_boundary": "Reported partial usage and model-catalog estimates, not final upstream billing"}
        self.record("real SSE error automatically recovers with bounded new claim and successful host acceptance", phase=phase,
                    run_id=run["id"], attempts=target["attempts"], max_attempts=target["max_attempts"], fault=faults[0],
                    recovery=observed_retries[phase], rejected_old_token_operations=["complete", "renew", "fail"],
                    unique_lease_expiries={key: len(values) for key, values in expiries.items()}, model_evidence=evidence,
                    native_stream_error_events=len(error_events), command=command, delivery=delivery,
                    actual_partial_usage_evidence=partial_proof,
                    estimated_spend_usd=state.get("spent_usd", 0))
        self.case = None

    def cleanup(self) -> None:
        try:
            super().cleanup()
        finally:
            if self.proxy is not None:
                self.proxy.stop()
                atomic_json(self.output / "forwarder-audit.json", self.proxy.audit())
                self.report.setdefault("cleanup", {}).update({"forwarder_stopped": not self.proxy.thread.is_alive(),
                                                               "forwarder_active_sockets_after_cleanup": {"clients": len(self.proxy.clients), "connections": len(self.proxy.connections), "responses": len(self.proxy.responses)},
                                                               "real_upstream_key_cleared_from_forwarder": self.proxy.upstream_key == ""})
                require(not self.proxy.thread.is_alive(), "Fault forwarder did not stop")
                require(not (self.proxy.clients or self.proxy.connections or self.proxy.responses), "Fault forwarder retained active sockets")

    def run(self) -> int:
        try:
            self.setup_provider()
            self.build()
            self.write_checker()
            self.setup_forwarder()
            phases = ["verification", "implementation"] if self.args.fault_phase == "both" else [self.args.fault_phase]
            for phase in phases:
                self.run_case(phase)
            forwarded = [event for event in self.proxy.audit() if event.get("kind") == "real_upstream_forward"]
            require(forwarded and all(event.get("upstream_request_sent") for event in forwarded), "No genuine upstream model RPCs were forwarded")
            require(not any(event.get("forwarder_error_type") for event in self.proxy.audit()), "Unexpected forwarder errors occurred outside the injected SSE fault")
            real_fault_rpcs = sum(event.get("kind") == "injected_sse_error" and event.get("upstream_request_sent", False) for event in self.proxy.audit())
            self.report["forwarder_summary"] = {"ordinary_real_upstream_rpcs": len(forwarded), "fault_rpcs_sent_to_real_upstream": real_fault_rpcs,
                                                 "total_real_upstream_rpcs": len(forwarded) + real_fault_rpcs, "injected_sse_errors": len(phases),
                                                 "real_partial_usage_mode": self.args.fault_after_real_start,
                                                 "request_body_logged": False, "authentication_header_logged": False}
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
            self.progress("finished", passed=self.report["passed"], failure=self.report.get("failure"), cleanup=self.report.get("cleanup"))
        print(self.redact(json.dumps({"passed": self.report["passed"], "checks": len(self.report["checks"]), "failure": self.report.get("failure"),
                                      "report": str(self.output / "verification-report.json"), "cleanup": self.report.get("cleanup")}, ensure_ascii=False)), flush=True)
        return 0 if self.report["passed"] else 1


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("--repo", type=Path, default=Path(__file__).resolve().parents[2])
    parser.add_argument("--output", type=Path, required=True, help="new or empty private evidence directory")
    parser.add_argument("--fault-phase", choices=("verification", "implementation", "both"), default="verification")
    parser.add_argument("--fault-after-real-start", action="store_true", help="forward the original upstream message_start/usage before injecting the SSE error; proves reported partial-usage preservation")
    parser.add_argument("--stage-timeout", type=float, default=600)
    parser.set_defaults(quick=True, soak_seconds=0)
    args = parser.parse_args()
    require(args.stage_timeout > 0, "--stage-timeout must be positive")
    os.umask(0o077)

    def interrupted(signum: int, _frame: Any) -> None:
        raise KeyboardInterrupt("Fault harness interrupted by " + signal.Signals(signum).name)

    signal.signal(signal.SIGINT, interrupted)
    signal.signal(signal.SIGTERM, interrupted)
    return StreamFaultLiveCheck(args).run()


if __name__ == "__main__":
    raise SystemExit(main())
