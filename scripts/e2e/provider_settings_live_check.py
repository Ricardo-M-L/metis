#!/usr/bin/env python3
"""Opt-in real Desktop provider-settings acceptance, with no browser or OS input.

Supply a fresh source-frozen disposable CLI. All saves affect a private HOME;
only the selected API-key route is copied. A deliberately named private alias
uses that exact endpoint, protocol and model. Two short real queries prove
1024/1536 output budgets from dispatched wire records and provider receipts.
All executables, credentials, private prompts, listeners and owned processes
are destroyed on every outcome. Retained evidence contains safe scalar
projections and hashes, never full assembled prompts or request headers.
--self-test reads no authentication and uses no network or GUI.
"""
from __future__ import annotations

import argparse
import copy
import hashlib
import json
import os
from pathlib import Path
import re
import shutil
import signal
import socket
import sys
import time
from typing import Any
from urllib.parse import quote
import uuid

from context_usage_live_check import ContextUsageLiveCheck, merged_route_header, usage_oracle
from persistent_agent_live_check import atomic_json, digest, history_snapshots, message_text, require, trace_proof, utc_now


def canonical(value: Any) -> str:
    return json.dumps(value, ensure_ascii=False, sort_keys=True, separators=(",", ":"))


def snapshot_oracle(value: dict[str, Any]) -> dict[str, Any]:
    require(isinstance(value, dict) and isinstance(value.get("revision"), str) and value["revision"],
            "Provider settings have no CAS revision")
    default, current, providers = value.get("savedDefault"), value.get("current"), value.get("providers")
    require(isinstance(default, dict) and isinstance(current, dict) and isinstance(providers, list),
            "Provider settings lack saved/current route or provider list")
    require(all(isinstance(route.get(field), str) and route[field] for route in (default, current)
                for field in ("provider", "model")), "Provider settings route is incomplete")
    ids = []
    for provider in providers:
        require(isinstance(provider, dict) and isinstance(provider.get("id"), str) and provider["id"], "Provider has no stable id")
        ids.append(provider["id"])
        models = provider.get("models")
        require(isinstance(models, list), "Provider has no persistent model list")
        model_ids = []
        for model in models:
            require(isinstance(model, dict) and isinstance(model.get("id"), str) and model["id"], "Saved model has no id")
            model_ids.append(model["id"])
            require(all(type(model.get(field)) is int and model[field] >= 0 for field in
                        ("contextWindow", "maxOutputTokens", "outputBudget")), "Model parameters are missing or negative")
            require("supportsVision" not in model or type(model["supportsVision"]) is bool,
                    "Model vision capability is not a boolean")
            require("supportsReasoning" not in model or type(model["supportsReasoning"]) is bool,
                    "Model reasoning capability is not a boolean")
            mapping = model.get("reasoningEfforts", {})
            require(isinstance(mapping, dict) and all(key in ("low", "medium", "high") and
                isinstance(token, str) and token for key, token in mapping.items()), "Model reasoning map has invalid canonical levels")
        require(len(model_ids) == len(set(model_ids)), "Saved model list contains duplicate ids")
    require(len(ids) == len(set(ids)), "Provider settings contain duplicate ids")
    return value


def provider_view(value: dict[str, Any], provider: str) -> dict[str, Any]:
    snapshot_oracle(value)
    matches = [row for row in value["providers"] if row["id"] == provider]
    require(len(matches) == 1, "Expected provider is absent or ambiguous")
    return matches[0]


def model_settings_oracle(value: dict[str, Any], provider: str, models: list[dict[str, Any]],
                          expected_default: dict[str, str]) -> dict[str, Any]:
    row = provider_view(value, provider)
    require(value["savedDefault"] == expected_default, "Saving provider/model settings implicitly changed the saved default")
    actual = {model["id"]: model for model in row["models"]}
    require(set(actual) == {model["id"] for model in models}, "Persistent multi-model list differs from saved choices")
    for expected in models:
        require(all(actual[expected["id"]].get(key) == number for key, number in expected.items()),
                "Saved model parameters differ from submitted values")
    return row


def import_model_settings(model_id: str, source: dict[str, Any]) -> dict[str, Any]:
    maximum = source.get("maxOutputTokens", 0)
    require(type(maximum) is int and maximum >= 0 and (not maximum or maximum >= 1536),
            "Real model metadata cannot support the bounded acceptance output budgets")
    # Match UI import: discovery supplies identity, while zero keeps dynamic
    # capability inheritance. A recommended client window is not a manual cap.
    return {"id": model_id, "name": source.get("name") or model_id,
            "contextWindow": 0, "maxOutputTokens": 0, "outputBudget": 1024}


def cas_oracle(status: int, after: dict[str, Any], before: dict[str, Any], config_unchanged: bool,
               auth_unchanged: bool) -> None:
    require(status == 409 and canonical(after) == canonical(before) and config_unchanged and auth_unchanged,
            "Stale CAS write was accepted or mutated private settings/credentials")


def restart_oracle(before: dict[str, Any], after: dict[str, Any]) -> None:
    snapshot_oracle(before)
    snapshot_oracle(after)
    require(all(canonical(before[key]) == canonical(after[key]) for key in ("revision", "savedDefault", "providers")),
            "Private restart did not preserve saved providers/models/parameters/default/revision")
    require(after["current"] == after["savedDefault"], "Restarted runtime did not use the persisted default")


def delete_oracle(value: dict[str, Any], provider: str, expected_default: dict[str, str],
                  credential_present: bool) -> None:
    snapshot_oracle(value)
    require(value.get("deleted") is True and value.get("id") == provider and
            value["savedDefault"] == expected_default and
            all(row["id"] != provider for row in value["providers"]) and not credential_present,
            "Deleting a nondefault provider lost the default or retained its profile/credential")


def wire_oracle(entries: list[dict[str, Any]], sid: str, model: str, base_url: str, transport: str,
                budget: int, previous_ids: set[str], expected_effort: str | None = None) -> list[dict[str, Any]]:
    base = base_url.rstrip("/")
    if transport in ("openai", "openai_chat"):
        endpoint, field = base + "/chat/completions", "max_tokens"
    elif transport == "openai_responses":
        endpoint, field = base + "/responses", "max_output_tokens"
    elif transport in ("anthropic", "anthropic_messages"):
        endpoint, field = base + "/v1/messages", "max_tokens"
    else:
        raise AssertionError("This wire oracle does not support the original selected transport")
    requests = [entry for entry in entries if entry.get("type") == "request" and
                entry.get("_session_id") == sid and entry.get("reqid") not in previous_ids]
    require(requests, "Real query has no dispatched session-owned wire request")
    proofs = []
    for request in requests:
        rid, body = request.get("reqid"), request.get("body")
        require(isinstance(rid, str) and rid and isinstance(body, dict) and request.get("method") == "POST" and
                request.get("url") == endpoint and body.get("model") == model,
                "Real dispatched request changed model/endpoint/protocol/owner")
        require(type(body.get(field)) is int and body[field] == budget,
                "Dispatched output budget differs from the saved model setting")
        effort = None
        if transport in ("openai", "openai_chat"):
            effort = body.get("reasoning_effort")
            require(effort == expected_effort, "Chat reasoning effort differs from the explicitly configured map/default")
        elif transport == "openai_responses":
            reasoning = body.get("reasoning")
            require(reasoning is None or isinstance(reasoning, dict), "Responses reasoning configuration is malformed")
            effort = reasoning.get("effort") if isinstance(reasoning, dict) else None
            require(effort == expected_effort, "Responses reasoning effort differs from the explicitly configured map/default")
        relevant = [entry for entry in entries if entry.get("reqid") == rid and entry.get("_session_id") == sid]
        responses = [entry for entry in relevant if entry.get("type") == "response"]
        require(sum(entry.get("type") == "request" for entry in relevant) == 1 and len(responses) == 1 and
                responses[0].get("status") == 200 and not any(entry.get("type") == "error" for entry in relevant),
                "Physical request/response is missing, duplicate or not successful")
        require(isinstance(request.get("request_sha256"), str) and re.fullmatch(r"[a-f0-9]{64}", request["request_sha256"]) and
                type(request.get("request_bytes")) is int and request["request_bytes"] > 0,
                "Dispatched payload lacks an exact transport hash/byte count")
        proofs.append({"session_id": sid, "request_id": rid, "model": model, "transport": transport,
            "url": endpoint, "output_budget_field": field, "output_budget": budget, "status": 200,
            "mapped_reasoning_effort": effort,
            "request_sha256": request["request_sha256"], "request_bytes": request["request_bytes"],
            "full_prompt_or_headers_retained": False})
    return proofs


class ProviderSettingsLiveCheck(ContextUsageLiveCheck):
    def __init__(self, args: argparse.Namespace) -> None:
        super().__init__(args)
        self.alias = "private-model-settings-" + uuid.uuid4().hex[:12]
        self.sessions: dict[str, tuple[str, str]] = {}
        self.safe_turn_evidence: list[dict[str, Any]] = []
        self.listener_ports: list[int] = []
        self.reasoning_live = False
        self.report["boundary"] = ("Private headless Desktop provider settings, real original-route metadata discovery, an intentional "
            "private alias with unchanged endpoint/protocol/model, CAS, multi-model persistence, immediate new-session default, "
            "existing-session isolation, two real short queries with 1024/1536 wire output budgets and restart recovery. "
            "No browser/native input, OS permissions, supplier billing, model-quality or other-provider live claims.")

    def api(self, label: str, method: str, path: str, body: Any = None, *, expected: int = 200,
            timeout: float = 10, headers: dict[str, str] | None = None) -> Any:
        status, value = self.http(method, path, body, timeout=timeout, headers=headers)
        encoded = json.dumps(value)
        leaked = bool(self.key and self.key in encoded)
        # Never persist a credential even if a production regression echoes it.
        if path.startswith("/api/turns"):
            safe = {key: value.get(key) for key in ("sessionId", "stopped")}
            safe["response_sha256"] = hashlib.sha256(encoded.encode()).hexdigest()
        elif path.startswith("/api/sessions/") and not path.endswith("/activate"):
            safe = {"session": {key: value.get("session", {}).get(key) for key in ("id", "model")},
                    "response_sha256": hashlib.sha256(encoded.encode()).hexdigest()}
        elif path.startswith("/api/trace"):
            safe = {key: value.get(key) for key in ("enabled", "sessionId", "source", "stats")}
            safe["response_sha256"] = hashlib.sha256(encoded.encode()).hexdigest()
        else:
            safe = value
        self.api_index += 1
        atomic_json(self.output / (f"api-{self.api_index:02d}-" + label + ".json"),
            json.loads(self.redact(json.dumps({"method": method, "path": path, "status": status, "body": safe}))))
        require(not leaked, "API response exposed the selected API credential")
        require(status == expected, label + " returned HTTP " + str(status))
        return value

    def snapshot(self, label: str) -> dict[str, Any]:
        return snapshot_oracle(self.api(label, "GET", "/api/providers"))

    def session_route(self, sid: str, provider: str, model: str) -> dict[str, Any]:
        path = self.home / "sessions" / (sid + ".jsonl")
        records = [json.loads(line) for line in path.read_text().splitlines() if line]
        proof = merged_route_header(records, sid, provider, model)
        self.sessions[sid] = provider, model
        return {**proof, "transcript_sha256": digest(path)}

    def create_session(self, label: str, provider: str) -> str:
        sid = self.new_session(label)
        self.session_route(sid, provider, self.model)
        return sid

    def start_headless_server(self) -> None:
        super().start_headless_server()
        self.listener_ports.append(self.port)

    def stop_for_restart(self) -> None:
        require(self.server_child and self.server_child[0].poll() is None, "No owned live service to restart")
        process, port = self.server_child[0], self.port
        self.api("owned-service-restart-shutdown", "POST", "/api/shutdown", {}, expected=202,
                 headers={"X-Metis-Desktop-Token": self.shutdown_token})
        self.finish(self.server_child, "headless-desktop-service", json_output=False, operation_timeout=20)
        require(process.poll() == 0, "Owned service did not stop cleanly before restart")
        with socket.socket() as probe:
            probe.settimeout(1)
            require(probe.connect_ex(("127.0.0.1", port)) != 0, "Previous listener survived private restart shutdown")
        self.server_child = None
        self.record("private backend stopped before restart without deleting its settings", pid=process.pid, port=port)

    def dumps(self) -> list[dict[str, Any]]:
        rows = []
        for path in (self.home / "dump-prompts").glob("*.jsonl"):
            require(path.is_file() and not path.is_symlink() and path.stat().st_size <= 16 * 1024 * 1024,
                    "Private transport evidence is unsafe or exceeds its bound")
            for line in path.read_text().splitlines():
                if line:
                    rows.append({**json.loads(line), "_session_id": path.stem})
        return rows

    def real_turn(self, sid: str, budget: int, number: int) -> None:
        before = self.trace_snapshot()
        previous_ids = {row.get("reqid") for row in self.dumps() if row.get("type") == "request"}
        prompt = "Do not use any tools. Reply only SETTINGS-" + uuid.uuid4().hex[:12] + "."
        result = self.api("real-settings-turn-" + str(number), "POST", "/api/turns",
            {"sessionId": sid, "input": prompt}, timeout=self.args.query_timeout)
        require(result.get("sessionId") == sid and not result.get("stopped"), "Real settings query was interrupted or changed owner")
        events = self.trace_events(before)
        usage = trace_proof(events, self.alias, self.model)
        require(usage["session_ids"] == [sid] and usage["model_responses"] > 0 and not usage["errors"] and
                not usage["tool_results"] and any(sum(row[key] for key in ("input", "output", "cache_write", "cache_read")) > 0
                for row in usage["usage"]), "Real settings query lacks successful original-model token receipts or executed an unexpected tool")
        route = self.session_route(sid, self.alias, self.model)
        canonical_session = self.api("real-turn-session-route-" + str(number), "GET", "/api/sessions/" + quote(sid))
        require(canonical_session.get("session", {}).get("id") == sid and canonical_session["session"].get("model") == self.model,
                "Formal session reader disagrees with the merged original-model route")
        history = history_snapshots(self.home / "sessions" / (sid + ".jsonl"))[-1]
        require(sum(message.get("role") == "user" and message_text(message) == prompt for message in history) == 1,
                "Real query prompt is absent or duplicated in durable session history")
        deadline = time.monotonic() + 5
        while True:
            entries = self.dumps()
            try:
                wire = wire_oracle(entries, sid, self.model, self.settings["base_url"],
                    self.settings.get("transport", "anthropic_messages"), budget, previous_ids,
                    "high" if self.reasoning_live else None)
                break
            except AssertionError:
                if time.monotonic() >= deadline:
                    raise
                time.sleep(0.1)  # Await asynchronous dump flushing; never retry a query/action.
        trace = self.api("real-turn-usage-" + str(number), "GET", "/api/trace?sessionId=" + quote(sid) + "&limit=2000")
        cost = json.loads((self.home / "sessions" / (sid + ".cost.json")).read_text())
        cumulative = usage_oracle(trace, sid, events, cost, 1)
        proof = {"turn": number, "route": route, "wire": wire, "usage": usage, "cumulative": cumulative,
                 "prompt_sha256": hashlib.sha256(prompt.encode()).hexdigest()}
        self.safe_turn_evidence.append(proof)
        if self.reasoning_live and number == 2:
            self.report["reasoning_mapping_boundary"]["actual_mapping_live_tested"] = True
        self.record("real private-alias query " + str(number) + " dispatched the exact saved output budget", **proof)

    def save_payload(self, revision: str, models: list[dict[str, Any]], *, create: bool = False) -> dict[str, Any]:
        body = {"expectedRevision": revision, "id": self.alias, "displayName": "Private Settings Acceptance",
            "catalogProvider": self.settings.get("catalog_provider", self.original_provider),
            "transport": self.settings.get("transport", "anthropic_messages"), "baseUrl": self.settings["base_url"],
            "model": self.model, "models": models, "credentialAction": "replace" if create else "keep"}
        if create:
            body["apiKey"] = self.key
        return body

    def select_effort(self, sid: str) -> None:
        self.api("activate-effort-owner-" + sid[:8], "POST", "/api/sessions/activate", {"id": sid})
        if self.reasoning_live:
            applied = self.api("explicit-high-effort-" + sid[:8], "POST", "/api/effort", {"effort": "high"})
            require(applied.get("effort") == "high" and applied.get("supported") is True,
                    "Explicit private reasoning effort did not apply to the next request")

    def check_settings(self) -> str:
        initial = self.snapshot("initial-provider-snapshot")
        original_default = copy.deepcopy(initial["savedDefault"])
        require(original_default == {"provider": self.original_provider, "model": self.model}, "Private staging changed the original selected route")
        old_sid = self.create_session("original-route-existing-session", self.original_provider)
        old_before = self.session_route(old_sid, self.original_provider, self.model)
        original_effort = self.api("original-model-reasoning-capability", "GET", "/api/effort")
        self.reasoning_live = (self.settings.get("transport") in ("openai", "openai_chat", "openai_responses") and
                               original_effort.get("supported") is True)
        discovered = self.api("real-provider-model-discovery", "POST", "/api/providers/models", {"id": self.original_provider}, timeout=20)
        require(discovered.get("source") == "provider" and discovered.get("provider") == self.original_provider and
                isinstance(discovered.get("models"), list), "Model listing is not a real selected-provider metadata discovery")
        by_id = {row.get("id"): row for row in discovered["models"] if isinstance(row, dict)}
        require(self.model in by_id and len(by_id) >= 2, "Real provider listing does not contain the selected model and a second persistent choice")
        secondary_id = sorted(key for key in by_id if isinstance(key, str) and key and key != self.model)[0]
        models = []
        for model_id in (self.model, secondary_id):
            models.append(import_model_settings(model_id, by_id[model_id]))
        if self.reasoning_live:
            models[0].update(supportsReasoning=True, reasoningEfforts={"high": "high"})
        self.report["reasoning_mapping_boundary"] = {"original_model_advertised_support": original_effort.get("supported") is True,
            "mapping_configured_for_live_test": self.reasoning_live, "actual_mapping_live_tested": False,
            "canonical_effort": "high" if self.reasoning_live else None,
            "wire_token": "high" if self.reasoning_live else None,
            "other_wire_tokens_not_tested_live": True, "unsupported_route_not_substituted": True}
        created = self.api("create-private-provider", "POST", "/api/providers", self.save_payload(initial["revision"], models, create=True), expected=201)
        saved = model_settings_oracle(created, self.alias, models, original_default)
        require(created["revision"] != initial["revision"] and saved.get("credentialConfigured") is True and
                saved.get("transport") == self.settings.get("transport", "anthropic_messages") and
                saved.get("baseUrl", "").rstrip("/") == self.settings["base_url"].rstrip("/"),
                "Private provider save lost credential/endpoint/protocol or CAS revision")
        config_path, auth_path = self.home / "config.toml", self.home / ".credentials" / "auth.json"
        require(self.key not in config_path.read_text(), "Managed provider API key leaked into private config.toml")
        cfg_hash, auth_hash = digest(config_path), digest(auth_path)
        stale = self.save_payload(initial["revision"], [{**model, "outputBudget": 1536} for model in models])
        stale_response = self.api("reject-stale-provider-edit", "POST", "/api/providers", stale, expected=409)
        cas_oracle(409, self.snapshot("after-stale-provider-edit"), created,
                   digest(config_path) == cfg_hash, digest(auth_path) == auth_hash)
        require(isinstance(stale_response, dict), "Stale provider write lacks a structured error")
        selected = self.api("save-private-default", "POST", "/api/providers/default",
            {"id": self.alias, "model": self.model, "expectedRevision": created["revision"]})
        require(selected.get("savedDefault") == {"provider": self.alias, "model": self.model} and
                selected.get("restartRequired") is False, "Saved default did not apply to new tasks immediately")
        # Remove response-only decoration before comparing canonical snapshots.
        selected = self.snapshot("after-private-default")
        self.provider = self.alias
        composer = self.api("persistent-multi-model-composer", "GET", "/api/models")
        choices = {(row.get("provider"), row.get("model")) for row in composer.get("models", [])}
        require(all((self.alias, model["id"]) in choices for model in models), "Composer omitted a persistent configured model")
        first_sid = self.create_session("new-session-immediate-saved-default", self.alias)
        require(self.session_route(old_sid, self.original_provider, self.model) == old_before,
                "Default selection silently rewrote an existing session")
        self.api("activate-first-private-session", "POST", "/api/sessions/activate", {"id": first_sid})
        applied = self.api("explicitly-apply-private-model", "POST", "/api/models", {"provider": self.alias, "model": self.model})
        require(applied.get("provider") == self.alias and applied.get("model") == self.model, "Explicit runtime model application changed the intended route")
        self.select_effort(first_sid)
        self.real_turn(first_sid, 1024, 1)
        updated_models = [{**model, "outputBudget": 1536 if model["id"] == self.model else model["outputBudget"]} for model in models]
        edited = self.api("edit-private-output-budget", "POST", "/api/providers", self.save_payload(selected["revision"], updated_models))
        model_settings_oracle(edited, self.alias, updated_models, selected["savedDefault"])
        require(edited["revision"] != selected["revision"], "Parameter edit did not advance its CAS revision")
        before_restart = self.snapshot("saved-settings-before-restart")
        config_before, auth_before = digest(config_path), digest(auth_path)
        self.stop_for_restart()
        self.start_headless_server()
        after_restart = self.snapshot("restored-settings-after-restart")
        restart_oracle(before_restart, after_restart)
        require(digest(config_path) == config_before and digest(auth_path) == auth_before,
                "Restart unexpectedly rewrote saved provider config/credentials")
        composer = self.api("restored-multi-model-composer", "GET", "/api/models")
        require(all((self.alias, model["id"]) in {(row.get("provider"), row.get("model")) for row in composer.get("models", [])}
                    for model in models), "Restart lost a composer model")
        second_sid = self.create_session("new-session-after-private-restart", self.alias)
        self.select_effort(second_sid)
        self.real_turn(second_sid, 1536, 2)
        self.record("CAS settings/default, persistent models and existing-session boundaries survived private restart",
            intentional_private_alias=self.alias, original_provider=self.original_provider, original_model=self.model,
            original_existing_session=old_before, restored_revision=after_restart["revision"],
            model_ids=[model["id"] for model in models], second_model_queried=False,
            default_applied_to_new_tasks_without_restart=True, private_config_sha256=config_before,
            private_auth_sha256=auth_before, original_endpoint_protocol_model_preserved=True)
        before_delete = self.snapshot("before-default-delete-rejection")
        cfg_hash, auth_hash = digest(config_path), digest(auth_path)
        self.api("reject-deletion-of-saved-default", "DELETE", "/api/providers",
            {"id": self.alias, "expectedRevision": before_delete["revision"]}, expected=409)
        cas_oracle(409, self.snapshot("after-default-delete-rejection"), before_delete,
                   digest(config_path) == cfg_hash, digest(auth_path) == auth_hash)
        self.visual_window(second_sid)
        restored_default = self.api("restore-original-private-default", "POST", "/api/providers/default",
            {"id": self.original_provider, "model": self.model, "expectedRevision": after_restart["revision"]})
        deleted = self.api("delete-nondefault-private-provider", "DELETE", "/api/providers",
            {"id": self.alias, "expectedRevision": restored_default["revision"]})
        auth = json.loads(auth_path.read_text())
        delete_oracle(deleted, self.alias, {"provider": self.original_provider, "model": self.model}, self.alias in auth)
        self.record("private nondefault provider and its credential were deleted through the API", provider=self.alias)
        return second_sid

    def copy_evidence(self) -> None:
        # Full sessions include assembled system prompts. Persist only formal
        # route projections, token receipts and hashes before deleting them.
        evidence = []
        for sid, (provider, model) in self.sessions.items():
            path = self.home / "sessions" / (sid + ".jsonl")
            if path.is_file() and not path.is_symlink():
                records = [json.loads(line) for line in path.read_text().splitlines() if line]
                row = {"session_id": sid, "transcript_sha256": digest(path), "expected_provider": provider, "expected_model": model,
                    "route_header_patches": [{key: record.get("header", {}).get(key) for key in ("id", "provider", "model")}
                        for record in records if record.get("type") == "header"]}
                try:
                    row["route"] = merged_route_header(records, sid, provider, model)
                except AssertionError:
                    row["route_oracle_failed"] = True
                    self.report["passed"] = False
                target = self.output / ("session-" + sid + "-safe-proof.json")
                atomic_json(target, row)
                evidence.append({"path": target.name, "sha256": digest(target)})
        for path in (self.home / "dump-prompts").glob("*.jsonl"):
            evidence.append({"private_dump_id": path.stem, "sha256": digest(path), "raw_file_retained": False})
        safe_wire = []
        for entry in self.dumps():
            row = {key: entry.get(key) for key in ("type", "_session_id", "reqid", "method", "url", "status", "request_sha256", "request_bytes")}
            body = entry.get("body")
            if entry.get("type") == "request" and isinstance(body, dict):
                row["request_parameters"] = {key: body.get(key) for key in
                    ("model", "max_tokens", "max_output_tokens", "temperature", "reasoning_effort")}
                if isinstance(body.get("reasoning"), dict):
                    row["request_parameters"]["reasoning_effort_responses"] = body["reasoning"].get("effort")
            if entry.get("type") == "error":
                row["error_record_present"] = True  # Never retain a provider's free-form error body.
            safe_wire.append(row)
        target = self.output / "safe-physical-request-evidence.json"
        atomic_json(target, json.loads(self.redact(json.dumps(safe_wire))))
        evidence.append({"path": target.name, "sha256": digest(target)})
        atomic_json(self.output / "evidence-manifest.json", evidence)
        self.report["safe_evidence_only"] = {"full_prompts_retained": False, "raw_headers_retained": False,
            "raw_transport_dumps_destroyed_with_private_root": True, "safe_real_turn_proofs": self.safe_turn_evidence}

    def run_settings(self) -> int:
        def interrupted(_signum: int, _frame: Any) -> None:
            raise TimeoutError("Bounded provider settings acceptance interrupted or timed out")
        previous = {number: signal.signal(number, interrupted) for number in (signal.SIGINT, signal.SIGTERM, signal.SIGALRM)}
        signal.setitimer(signal.ITIMER_REAL, self.args.case_timeout)
        try:
            self.installed_and_original_guard()
            self.stage()
            self.load_route()
            self.original_provider = self.provider
            require(self.settings.get("transport", "anthropic_messages") in
                    ("openai", "openai_chat", "openai_responses", "anthropic", "anthropic_messages"),
                    "Selected transport has no implemented acceptance wire oracle; no substitute is allowed")
            self.env["METIS_DUMP_PROMPTS"] = "1"
            self.report["intentional_private_alias"] = {"id": self.alias, "same_endpoint_protocol_model": True,
                "original_provider": self.original_provider, "model": self.model, "not_a_fallback": True}
            self.start_headless_server()
            self.check_settings()
            self.report["passed"] = True
        except BaseException as error:
            self.report.update({"passed": False, "failure_type": type(error).__name__, "failure": self.redact(str(error))})
        finally:
            signal.setitimer(signal.ITIMER_REAL, 0)
            for label, action in (("cleanup", self.cleanup), ("evidence_copy", self.copy_evidence),
                                  ("source_freeze", self.verify_freeze), ("original_files", self.verify_originals)):
                try:
                    action()
                except BaseException as error:
                    self.report["passed"] = False
                    self.report[label + "_failure"] = self.redact(str(error))
            shutil.rmtree(self.root, ignore_errors=True)
            self.report.setdefault("cleanup", {})["temporary_workspace_destroyed"] = not self.root.exists()
            self.report["passed"] = self.report["passed"] and not self.root.exists()
            self.report.update({"finished_utc": utc_now(), "elapsed_seconds": round(time.monotonic() - self.started, 3)})
            self.report = json.loads(self.redact(json.dumps(self.report)))
            self.secrets.clear()
            self.report["cleanup"]["redaction_secret_values_cleared_after_evidence"] = not self.secrets
            atomic_json(self.output / "verification-report.json", self.report)
            for number, handler in previous.items(): signal.signal(number, handler)
        print(json.dumps({"passed": self.report["passed"], "failure": self.report.get("failure"),
                          "report": str(self.output / "verification-report.json")}), flush=True)
        return 0 if self.report["passed"] else 1


def self_test() -> dict[str, Any]:
    import unittest
    import test_provider_settings_live_check
    suite = unittest.defaultTestLoader.loadTestsFromModule(test_provider_settings_live_check)
    result = unittest.TextTestRunner(stream=sys.stderr, verbosity=1).run(suite)
    require(result.wasSuccessful(), "Offline provider settings oracle checks failed")
    return {"offline_oracle_tests_passed": result.testsRun, "authentication_files_read": False,
            "real_provider_requests": 0, "network_used": False, "GUI_used": False}


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    mode = parser.add_mutually_exclusive_group(required=True)
    mode.add_argument("--run-live", action="store_true")
    mode.add_argument("--self-test", action="store_true")
    parser.add_argument("--repo", type=Path, default=Path(__file__).resolve().parents[2])
    parser.add_argument("--binary", type=Path)
    parser.add_argument("--expected-sha256")
    parser.add_argument("--source-freeze", type=Path)
    parser.add_argument("--output", type=Path)
    parser.add_argument("--destroy-input", action="store_true")
    parser.add_argument("--query-timeout", type=float, default=180)
    parser.add_argument("--case-timeout", type=float, default=600)
    parser.add_argument("--visual-wait-seconds", type=int, default=0)
    args = parser.parse_args(argv)
    if args.self_test:
        print(json.dumps(self_test(), indent=2))
        return 0
    require(all((args.binary, args.expected_sha256, args.source_freeze, args.output, args.destroy_input)),
            "Live mode requires --binary --expected-sha256 --source-freeze --output --destroy-input")
    require(re.fullmatch(r"[a-f0-9]{64}", args.expected_sha256), "Supply the exact fresh CLI SHA-256")
    require(0 < args.query_timeout <= 600 and 0 < args.case_timeout <= 1800 and 0 <= args.visual_wait_seconds <= 60,
            "Query/case/visual timeouts must be bounded")
    installed = shutil.which("metis")
    require(not installed or Path(installed).resolve() != args.binary.expanduser().resolve(), "Never destroy the installed CLI")
    os.umask(0o077)
    return ProviderSettingsLiveCheck(args).run_settings()


if __name__ == "__main__": raise SystemExit(main())
