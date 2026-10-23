"""Offline membership oracles; every value and credential is synthetic."""
import copy
from pathlib import Path
import json
import sys
import tempfile
import unittest

sys.path.insert(0, str(Path(__file__).resolve().parent))
from provider_membership_live_check import (BUILTIN_IDS, added_connection_oracle,
    membership_oracle, membership_target, no_placeholder_oracle,
    picker_membership_oracle, private_credential_present)
from provider_settings_live_check import delete_oracle


def connection(provider="original", credential=True):
    return {"id": provider, "custom": provider not in BUILTIN_IDS, "editable": True,
            "readOnly": False, "removable": True, "transport": "openai_chat",
            "model": "model-a", "models": [{"id": "model-a", "name": "Membership model",
                "contextWindow": 0, "maxOutputTokens": 0, "outputBudget": 0}],
            "credentialConfigured": credential}


def snapshot(*providers):
    return {"revision": "synthetic-r1", "savedDefault": {"provider": "original", "model": "model-a"},
            "current": {"provider": "original", "model": "model-a"},
            "providers": list(providers) if providers else [connection()]}


def picker(*pairs):
    return {"models": [{"provider": p, "model": m, "credentialConfigured": p == "original"}
                       for p, m in pairs]}


class ProviderMembershipOracles(unittest.TestCase):
    def test_original_connection_without_placeholders(self):
        value = snapshot()
        membership_oracle(value, {"original"})
        no_placeholder_oracle(value, set())

    def test_reject_every_implicit_builtin_and_unknown_extra_connection(self):
        for provider in [*sorted(BUILTIN_IDS), "unexpected-route"]:
            with self.subTest(provider=provider):
                value = snapshot(connection(), connection(provider, False))
                with self.assertRaises(AssertionError):
                    membership_oracle(value, {"original"})
                if provider in BUILTIN_IDS:
                    with self.assertRaises(AssertionError): no_placeholder_oracle(value, set())

    def test_explicit_no_key_connection_survives_membership_and_allows_removal(self):
        value = snapshot(connection(), connection("openai", False))
        models = value["providers"][1]["models"]
        added_connection_oracle(value, "original", value["savedDefault"], models, False)
        no_placeholder_oracle(value, {"openai"})
        picker_membership_oracle(picker(("original", "model-a"), ("openai", "model-a")),
                                 {"original", "openai"}, {("openai", "model-a")}, "openai")

    def test_reject_lost_no_key_connection_and_incorrect_removal_or_auth_flags(self):
        saved = snapshot(connection(), connection("openai", False))
        models = copy.deepcopy(saved["providers"][1]["models"])
        variants = [snapshot()]
        for patch in ({"removable": False}, {"removable": "true"}, {"editable": False},
                      {"readOnly": True}, {"credentialConfigured": True}, {"custom": True}):
            changed = copy.deepcopy(saved)
            changed["providers"][1].update(patch)
            variants.append(changed)
        for value in variants:
            with self.subTest(value=value):
                with self.assertRaises(AssertionError):
                    added_connection_oracle(value, "original", saved["savedDefault"], models, False)
        with self.assertRaises(AssertionError): no_placeholder_oracle(snapshot(), {"openai"})

    def test_saved_no_key_model_cannot_disappear_or_claim_credentials_in_picker(self):
        good = picker(("original", "model-a"), ("openai", "model-a"))
        for mutate in (lambda x: x["models"].pop(),
                       lambda x: x["models"][1].update(credentialConfigured=True),
                       lambda x: x["models"][1].pop("credentialConfigured"),
                       lambda x: x["models"].append({"provider": "gemini", "model": "ghost"})):
            value = copy.deepcopy(good)
            mutate(value)
            with self.assertRaises(AssertionError):
                picker_membership_oracle(value, {"original", "openai"}, {("openai", "model-a")}, "openai")

    def test_removed_connection_must_leave_settings_picker_and_credential_namespaces(self):
        value = snapshot()
        value.update(deleted=True, id="openai")
        delete_oracle(value, "openai", value["savedDefault"], False)
        membership_oracle(value, {"original"})
        picker_membership_oracle(picker(("original", "model-a")), {"original"}, {("original", "model-a")})
        with self.assertRaises(AssertionError): delete_oracle(value, "openai", value["savedDefault"], True)
        with self.assertRaises(AssertionError):
            membership_oracle(snapshot(connection(), connection("openai", False)), {"original"})
        with self.assertRaises(AssertionError):
            picker_membership_oracle(picker(("original", "model-a"), ("openai", "model-a")),
                                     {"original"}, {("original", "model-a")})

    def test_credential_namespace_check_returns_boolean_and_counts_api_oauth_and_aliases(self):
        with tempfile.TemporaryDirectory() as directory:
            home = Path(directory)
            cred = home / ".credentials"
            cred.mkdir()
            self.assertFalse(private_credential_present(home, "openai"))
            api = cred / "auth.json"
            api.write_text(json.dumps({"original": {"key": "synthetic-original"},
                                       " OpenAI ": {"key": "synthetic-openai"}}))
            self.assertIs(private_credential_present(home, "openai"), True)
            api.write_text('{"original":{"key":"synthetic-original"}}')
            self.assertFalse(private_credential_present(home, "openai"))
            oauth = cred / "llm-oauth.json"
            oauth.write_text('{"format_version":1,"credentials":{"openai":{"access_token":"synthetic-oauth"}}}')
            self.assertTrue(private_credential_present(home, "openai"))
            oauth.write_text('{"format_version":1,"credentials":{}}')
            self.assertFalse(private_credential_present(home, "openai"))

    def test_credential_namespace_check_rejects_symlink_and_invalid_shape(self):
        with tempfile.TemporaryDirectory() as directory:
            home = Path(directory)
            cred = home / ".credentials"
            cred.mkdir()
            data = home / "synthetic-data"
            data.write_text("{}")
            path = cred / "auth.json"
            path.symlink_to(data)
            with self.assertRaises(AssertionError): private_credential_present(home, "openai")
            path.unlink()
            path.write_text("[]")
            with self.assertRaises(AssertionError): private_credential_present(home, "openai")

    def test_target_requires_selected_custom_openai_route_without_fallback(self):
        self.assertEqual(membership_target("original", "openai_chat"), "openai_chat")
        self.assertEqual(membership_target("original", "openai_responses"), "openai_responses")
        self.assertEqual(membership_target("original", "openai"), "openai_chat")
        for provider, transport in (("openai", "openai_chat"), ("original", "anthropic_messages"),
                                    ("original", "gemini_native"), ("original", "")):
            with self.assertRaises(AssertionError): membership_target(provider, transport)


if __name__ == "__main__":
    unittest.main()
