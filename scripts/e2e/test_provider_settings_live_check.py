"""Offline contract oracles only; no authentication, network or GUI."""
import copy
import argparse
from pathlib import Path
import shutil
import sys
import tempfile
import unittest
from unittest.mock import patch

sys.path.insert(0, str(Path(__file__).resolve().parent))
from provider_settings_live_check import (ProviderSettingsLiveCheck, cas_oracle, delete_oracle,
    import_model_settings, model_settings_oracle, restart_oracle, snapshot_oracle, wire_oracle)
from context_usage_live_check import merged_route_header


def snapshot():
    return {"revision": "r1", "savedDefault": {"provider": "original", "model": "model-a"},
        "current": {"provider": "original", "model": "model-a"}, "providers": [
        {"id": "original", "transport": "openai_chat", "baseUrl": "http://127.0.0.1:8317/v1",
         "model": "model-a", "models": [{"id": "model-a", "name": "A", "contextWindow": 1000000,
             "maxOutputTokens": 8192, "outputBudget": 1024}], "credentialConfigured": True},
        {"id": "private-test", "transport": "openai_chat", "baseUrl": "http://127.0.0.1:8317/v1",
         "model": "model-a", "models": [{"id": "model-a", "name": "A", "contextWindow": 1000000,
             "maxOutputTokens": 8192, "outputBudget": 1024}, {"id": "model-b", "name": "B",
             "contextWindow": 200000, "maxOutputTokens": 4096, "outputBudget": 1024}],
         "credentialConfigured": True}]}


def wire():
    return [{"type": "request", "_session_id": "owner", "reqid": "request-1", "method": "POST",
        "url": "http://127.0.0.1:8317/v1/chat/completions", "request_sha256": "a" * 64,
        "request_bytes": 500, "body": {"model": "model-a", "max_tokens": 1024,
            "messages": [{"role": "user", "content": "private test prompt"}]}},
        {"type": "response", "_session_id": "owner", "reqid": "request-1", "status": 200}]


class ProviderSettingsOracles(unittest.TestCase):
    def test_discovery_import_keeps_dynamic_caps_and_only_sets_explicit_budget(self):
        result = import_model_settings("model-a", {"name": "A", "contextWindow": 272000,
            "maxContextWindow": 1000000, "maxOutputTokens": 32768, "windowKind": "client_profile"})
        self.assertEqual(result, {"id": "model-a", "name": "A", "contextWindow": 0,
            "maxOutputTokens": 0, "outputBudget": 1024})
        with self.assertRaises(AssertionError):
            import_model_settings("model-a", {"maxOutputTokens": 1024})

    def test_snapshot_and_multimodel_parameters(self):
        value = snapshot()
        snapshot_oracle(value)
        model_settings_oracle(value, "private-test", value["providers"][1]["models"], value["savedDefault"])

    def test_reject_missing_revision_duplicate_provider_and_bad_budget(self):
        for mutate in (lambda x: x.pop("revision"),
                       lambda x: x["providers"].append(x["providers"][0]),
                       lambda x: x["providers"][1]["models"][0].update(outputBudget=-1)):
            value = snapshot()
            mutate(value)
            with self.assertRaises(AssertionError): snapshot_oracle(value)

    def test_create_edit_must_not_implicitly_change_default(self):
        value = snapshot()
        value["savedDefault"] = {"provider": "private-test", "model": "model-a"}
        with self.assertRaises(AssertionError):
            model_settings_oracle(value, "private-test", value["providers"][1]["models"], snapshot()["savedDefault"])

    def test_cas_requires_conflict_no_mutation_and_auth_unchanged(self):
        before = snapshot()
        cas_oracle(409, before, before, True, True)
        changed = copy.deepcopy(before)
        changed["providers"][1]["models"][0]["outputBudget"] = 1536
        for status, after, cfg, auth in ((200, before, True, True), (409, changed, True, True),
                                       (409, before, False, True), (409, before, True, False)):
            with self.assertRaises(AssertionError): cas_oracle(status, after, before, cfg, auth)

    def test_restart_preserves_saved_settings_and_applies_default(self):
        before = snapshot()
        before["savedDefault"] = {"provider": "private-test", "model": "model-a"}
        after = copy.deepcopy(before)
        after["current"] = before["savedDefault"]
        restart_oracle(before, after)
        for field in ("revision", "savedDefault", "providers"):
            changed = copy.deepcopy(after)
            changed[field] = "wrong"
            with self.assertRaises(AssertionError): restart_oracle(before, changed)
        after["current"] = {"provider": "original", "model": "model-a"}
        with self.assertRaises(AssertionError): restart_oracle(before, after)

    def test_delete_requires_removed_profile_credential_and_preserved_default(self):
        value = snapshot()
        value["providers"] = value["providers"][:1]
        value.update(deleted=True, id="private-test")
        delete_oracle(value, "private-test", snapshot()["savedDefault"], False)
        with self.assertRaises(AssertionError): delete_oracle(value, "private-test", snapshot()["savedDefault"], True)
        with self.assertRaises(AssertionError): delete_oracle({**value, "deleted": False}, "private-test", snapshot()["savedDefault"], False)
        with self.assertRaises(AssertionError): delete_oracle({**value, "providers": snapshot()["providers"]}, "private-test", snapshot()["savedDefault"], False)

    def test_wire_actual_budget_model_endpoint_receipt(self):
        proof = wire_oracle(wire(), "owner", "model-a", "http://127.0.0.1:8317/v1", "openai_chat", 1024, set())
        self.assertEqual(proof[0]["output_budget"], 1024)
        self.assertNotIn("private test prompt", str(proof))

    def test_wire_rejects_parameter_route_owner_duplicate_and_missing_receipt(self):
        for mutate in (lambda x: x[0]["body"].update(max_tokens=2048),
                       lambda x: x[0]["body"].update(model="other"),
                       lambda x: x[0].update(url="http://127.0.0.1:9999/v1/chat/completions"),
                       lambda x: x[0].update(_session_id="other"),
                       lambda x: x.append(copy.deepcopy(x[0])),
                       lambda x: x.pop(),
                       lambda x: x[1].update(status=500)):
            entries = wire()
            mutate(entries)
            with self.assertRaises(AssertionError):
                wire_oracle(entries, "owner", "model-a", "http://127.0.0.1:8317/v1", "openai_chat", 1024, set())

    def test_responses_wire_uses_correct_output_field(self):
        entries = wire()
        entries[0]["url"] = "http://127.0.0.1:8317/v1/responses"
        entries[0]["body"] = {"model": "model-a", "max_output_tokens": 1536}
        wire_oracle(entries, "owner", "model-a", "http://127.0.0.1:8317/v1", "openai_responses", 1536, set())

    def test_explicit_reasoning_map_reaches_chat_and_responses_wire(self):
        entries = wire()
        entries[0]["body"]["reasoning_effort"] = "high"
        wire_oracle(entries, "owner", "model-a", "http://127.0.0.1:8317/v1", "openai_chat", 1024, set(), "high")
        with self.assertRaises(AssertionError):
            wire_oracle(entries, "owner", "model-a", "http://127.0.0.1:8317/v1", "openai_chat", 1024, set())
        entries[0]["body"]["reasoning_effort"] = "xhigh"
        with self.assertRaises(AssertionError):
            wire_oracle(entries, "owner", "model-a", "http://127.0.0.1:8317/v1", "openai_chat", 1024, set(), "high")
        entries[0]["url"] = "http://127.0.0.1:8317/v1/responses"
        entries[0]["body"] = {"model": "model-a", "max_output_tokens": 1536, "reasoning": {"effort": "high"}}
        wire_oracle(entries, "owner", "model-a", "http://127.0.0.1:8317/v1", "openai_responses", 1536, set(), "high")

    def test_partial_lifecycle_headers_merge_but_explicit_switch_rejected(self):
        rows = [{"type": "header", "header": {"id": "owner", "provider": "private-test", "model": "model-a"}},
                {"type": "header", "header": {"id": "owner", "status": "completed", "model": ""}}]
        merged_route_header(rows, "owner", "private-test", "model-a")
        for patch in ({"id": "owner", "model": "other"}, {"id": "wrong", "status": "idle"}):
            with self.assertRaises(AssertionError):
                merged_route_header([*rows, {"type": "header", "header": patch}], "owner", "private-test", "model-a")

    def test_process_inspection_failure_still_destroys_input_and_private_credentials(self):
        with tempfile.TemporaryDirectory() as root:
            root = Path(root)
            source = root / "inert-disposable-input"
            source.write_text("offline data fixture; not an executable")
            args = argparse.Namespace(repo=root, output=root / "report", binary=source, destroy_input=True,
                expected_sha256=__import__("hashlib").sha256(source.read_bytes()).hexdigest(),
                source_freeze=root / "unused-freeze", query_timeout=1, case_timeout=1, visual_wait_seconds=0)
            checker = ProviderSettingsLiveCheck(args)
            try:
                checker.binary.write_text("inert private fixture")
                (checker.home / ".credentials").mkdir()
                (checker.home / ".credentials" / "auth.json").write_text('{"offline":"synthetic"}')
                checker.key = "synthetic"
                with patch("persistent_agent_live_check.subprocess.run", side_effect=RuntimeError("offline inspection failure")):
                    with self.assertRaises(RuntimeError): checker.cleanup()
                self.assertFalse(source.exists())
                self.assertFalse(checker.binary.exists())
                self.assertFalse((checker.home / ".credentials").exists())
                self.assertEqual(checker.key, "")
            finally:
                shutil.rmtree(checker.root)


if __name__ == "__main__": unittest.main()
