#!/usr/bin/env python3
"""Opt-in Desktop provider membership acceptance using a disposable CLI.

Add/edit/remove an OpenAI connection without implicit builtin placeholders,
including a saved connection without credentials and two private restarts.
Then reuse the unchanged provider-settings acceptance for real selected-route
queries, wire parameters, CAS, session boundaries, restart and cleanup.
--self-test uses synthetic data only; it reads no authentication or network.
"""
from __future__ import annotations

import argparse
import copy
import json
import os
from pathlib import Path
import re
import shutil
import sys
from typing import Any

from persistent_agent_live_check import digest, require
from provider_settings_live_check import (ProviderSettingsLiveCheck, cas_oracle,
    delete_oracle, model_settings_oracle, restart_oracle, snapshot_oracle)


BUILTIN_IDS = frozenset(("anthropic", "openai", "openai-codex", "gemini"))


def membership_oracle(value: dict[str, Any], expected_ids: set[str]) -> dict[str, Any]:
    snapshot_oracle(value)
    require({row["id"] for row in value["providers"]} == expected_ids,
            "Provider membership contains an implicit placeholder or lost an explicitly added connection")
    require(all(type(row.get("removable")) is bool for row in value["providers"]),
            "Added provider membership lacks an explicit removal policy")
    return value


def no_placeholder_oracle(value: dict[str, Any], allowed_builtin_ids: set[str]) -> None:
    snapshot_oracle(value)
    actual = {row["id"] for row in value["providers"]} & BUILTIN_IDS
    require(actual == allowed_builtin_ids,
            "Unregistered builtin placeholder appeared, or an explicitly added builtin connection disappeared")


def added_connection_oracle(value: dict[str, Any], original_provider: str,
                            original_default: dict[str, str], models: list[dict[str, Any]],
                            has_credential: bool) -> dict[str, Any]:
    membership_oracle(value, {original_provider, "openai"})
    row = model_settings_oracle(value, "openai", models, original_default)
    require(row.get("custom") is False and row.get("removable") is True and
            row.get("editable") is True and row.get("readOnly") is False and
            row.get("credentialConfigured") is has_credential,
            "Explicit OpenAI connection lost edit/removal semantics or credential state")
    return row


def picker_membership_oracle(value: dict[str, Any], allowed_providers: set[str],
                             expected_pairs: set[tuple[str, str]],
                             without_credentials: str | None = None) -> None:
    rows = value.get("models")
    require(isinstance(rows, list) and all(isinstance(row, dict) for row in rows),
            "Model picker lacks its configured model list")
    require(all(row.get("provider") in allowed_providers and isinstance(row.get("model"), str)
                and row["model"] for row in rows),
            "Model picker exposed an unregistered provider or invalid model")
    require(expected_pairs <= {(row["provider"], row["model"]) for row in rows},
            "Model picker lost an explicitly saved model")
    if without_credentials:
        selected = [row for row in rows if row["provider"] == without_credentials]
        require(selected and all(row.get("credentialConfigured") is False for row in selected),
                "Uncredentialed saved models disappeared or claimed available authentication")


def membership_target(original_provider: str, transport: str) -> str:
    require(original_provider not in BUILTIN_IDS and
            transport in ("openai", "openai_chat", "openai_responses"),
            "Membership live check requires the original custom OpenAI route; no substitute is allowed")
    return "openai_chat" if transport == "openai" else transport


def private_credential_present(home: Path, provider: str) -> bool:
    """Inspect only owned private credential namespaces; return no key values."""
    for filename, member in (("auth.json", None), ("llm-oauth.json", "credentials")):
        path = home / ".credentials" / filename
        if not path.exists():
            continue
        require(path.is_file() and not path.is_symlink() and path.stat().st_size <= 4 * 1024 * 1024,
                "Private credential evidence is not a bounded regular file")
        value = json.loads(path.read_text())
        require(isinstance(value, dict), "Private credential namespace has an invalid shape")
        entries = value.get(member, {}) if member else value
        require(isinstance(entries, dict), "Private credential entries have an invalid shape")
        if any(key.strip().lower() == provider for key in entries):
            return True
    return False


class ProviderMembershipLiveCheck(ProviderSettingsLiveCheck):
    def __init__(self, args: argparse.Namespace) -> None:
        super().__init__(args)
        self.membership_openai_added = False
        self.report["membership_boundary"] = (
            "Private settings API registration/removal, saved no-key connection, model picker, "
            "same-endpoint managed credential deletion and two extra restarts. No extra model "
            "completion is sent for the temporary OpenAI connection; inherited real turns use "
            "the original selected endpoint/protocol/model through an explicit private alias.")

    def snapshot(self, label: str) -> dict[str, Any]:
        value = super().snapshot(label)
        no_placeholder_oracle(value, {"openai"} if self.membership_openai_added else set())
        return value

    def check_settings(self) -> str:
        transport = membership_target(self.original_provider, self.settings.get("transport", ""))
        requests_before = {(row.get("_session_id"), row.get("reqid")) for row in self.dumps()
                           if row.get("type") == "request"}
        initial = self.snapshot("membership-initial-no-builtin-placeholders")
        membership_oracle(initial, {self.original_provider})
        original_default = copy.deepcopy(initial["savedDefault"])
        models = [{"id": self.model, "name": "Membership model", "contextWindow": 0,
                   "maxOutputTokens": 0, "outputBudget": 0}]

        def payload(revision: str, intent: str, credential_action: str = "keep") -> dict[str, Any]:
            value = {"expectedRevision": revision, "intent": intent, "id": "openai",
                     "displayName": "OpenAI membership acceptance", "transport": transport,
                     "baseUrl": self.settings["base_url"], "model": self.model,
                     "models": copy.deepcopy(models), "credentialAction": credential_action}
            if credential_action == "replace":
                value["apiKey"] = self.key
            return value

        created = self.api("membership-add-openai-without-key", "POST", "/api/providers",
                           payload(initial["revision"], "create"), expected=201)
        self.membership_openai_added = True
        added_connection_oracle(created, self.original_provider, original_default, models, False)
        saved = self.snapshot("membership-no-key-connection-still-listed")
        added_connection_oracle(saved, self.original_provider, original_default, models, False)
        config_path, auth_path = self.home / "config.toml", self.home / ".credentials" / "auth.json"
        for index, spelling in enumerate(("openai", " OpenAI "), 1):
            before = self.snapshot("membership-before-duplicate-create-" + str(index))
            config_hash, auth_hash = digest(config_path), digest(auth_path)
            duplicate = payload(before["revision"], "create")
            duplicate["id"] = spelling
            self.api("membership-reject-duplicate-create-" + str(index), "POST", "/api/providers",
                     duplicate, expected=409)
            cas_oracle(409, self.snapshot("membership-after-duplicate-create-" + str(index)), before,
                       digest(config_path) == config_hash, digest(auth_path) == auth_hash)
        models[0].update(name="Edited membership model", outputBudget=1024)
        before = self.snapshot("membership-before-edit")
        edited = self.api("membership-edit-openai-without-key", "POST", "/api/providers",
                          payload(before["revision"], "update"))
        added_connection_oracle(edited, self.original_provider, original_default, models, False)
        before_restart = self.snapshot("membership-before-no-key-restart")
        self.stop_for_restart()
        self.start_headless_server()
        after_restart = self.snapshot("membership-after-no-key-restart")
        restart_oracle(before_restart, after_restart)
        added_connection_oracle(after_restart, self.original_provider, original_default, models, False)
        composer = self.api("membership-no-key-model-picker", "GET", "/api/models")
        picker_membership_oracle(composer, {self.original_provider, "openai"},
                                 {("openai", self.model)}, "openai")

        bound = self.api("membership-bind-same-endpoint-managed-key", "POST", "/api/providers",
                         payload(after_restart["revision"], "update", "replace"))
        added_connection_oracle(bound, self.original_provider, original_default, models, True)
        require(private_credential_present(self.home, "openai"),
                "Managed credential was not saved under the explicitly added route")
        require(self.key not in config_path.read_text(), "Managed membership API key leaked into config.toml")
        deleted = self.api("membership-remove-added-openai", "DELETE", "/api/providers",
                           {"id": "openai", "expectedRevision": bound["revision"]})
        self.membership_openai_added = False
        delete_oracle(deleted, "openai", original_default, private_credential_present(self.home, "openai"))
        membership_oracle(deleted, {self.original_provider})
        before_restart = self.snapshot("membership-before-removed-route-restart")
        self.stop_for_restart()
        self.start_headless_server()
        after_restart = self.snapshot("membership-after-removed-route-restart")
        restart_oracle(before_restart, after_restart)
        membership_oracle(after_restart, {self.original_provider})
        require(not private_credential_present(self.home, "openai"), "Removed route credentials reappeared after restart")
        picker_membership_oracle(self.api("membership-removed-route-model-picker", "GET", "/api/models"),
                                 {self.original_provider}, {(self.original_provider, self.model)})
        requests_after = {(row.get("_session_id"), row.get("reqid")) for row in self.dumps()
                          if row.get("type") == "request"}
        require(requests_after == requests_before,
                "Membership-only settings operations unexpectedly dispatched a model completion")
        self.record("explicit provider registration, no-key persistence and removal survived private restarts",
                    provider="openai", original_default=original_default, saved_without_key=True,
                    saved_model_in_picker_without_credentials=True, duplicate_create_status=409,
                    credential_bound_to_original_endpoint=True, managed_credential_removed=True,
                    builtin_placeholders_after_removal=[], additional_model_requests=0)
        return super().check_settings()


def self_test() -> dict[str, Any]:
    import unittest
    import test_provider_membership_live_check
    import test_provider_settings_live_check
    suite = unittest.TestSuite(unittest.defaultTestLoader.loadTestsFromModule(module)
                               for module in (test_provider_membership_live_check, test_provider_settings_live_check))
    result = unittest.TextTestRunner(stream=sys.stderr, verbosity=1).run(suite)
    require(result.wasSuccessful(), "Offline membership/settings oracle checks failed")
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
    return ProviderMembershipLiveCheck(args).run_settings()


if __name__ == "__main__":
    raise SystemExit(main())
