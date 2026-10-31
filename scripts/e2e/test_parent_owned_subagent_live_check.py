"""Host-oracle tests only; never contact a provider or read real credentials."""
from __future__ import annotations

import json
import os
from pathlib import Path
import shutil
import sys
import tempfile
import tomllib
from types import SimpleNamespace
import unittest
from unittest.mock import patch

sys.path.insert(0, str(Path(__file__).resolve().parent))
import parent_owned_subagent_live_check as live_check
from parent_owned_subagent_live_check import (accepted_continuation, active_followup_recorded_before_terminal,
    agent_calls, assert_delegated_execution, assert_execution, execution_record, live_child_id, owned_child,
    ParentOwnedSubagentLiveCheck, restore_selected_output_limit, validate_inputs)
from project_coordinator_live_check import toml_table


class ParentOwnedOracles(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix="parent-owned-oracle-")
        self.root = Path(self.temp.name).resolve()

    def tearDown(self):
        self.temp.cleanup()

    def test_caller_must_supply_regular_private_binary_and_profile(self):
        cli = self.root / "metis-test"
        cli.write_text("offline fixture")
        profile = self.root / "profile"
        profile.mkdir()
        (profile / "config.toml").write_text("# private offline fixture")
        validate_inputs(cli, profile)
        link = self.root / "launcher"
        link.symlink_to(cli)
        with self.assertRaises(AssertionError):
            validate_inputs(link, profile)
        (profile / "config.toml").unlink()
        (profile / "config.toml").symlink_to(cli)
        with self.assertRaises(AssertionError):
            validate_inputs(cli, profile)

    def test_load_route_accepts_actual_quoted_toml_table_format(self):
        cli = self.root / "metis-test"
        cli.write_text("offline fixture, never executed")
        profile = self.root / "profile"
        profile.mkdir()
        config = toml_table(["provider"], {"default": "offline-route"}) + toml_table(
            ["provider", "custom", "offline-route"], {"model": "offline-model", "transport": "openai_chat",
                "base_url": "https://provider.invalid/v1", "max_tokens": 8192, "api_key": "offline-fixture-key"})
        (profile / "config.toml").write_text(config)
        checker = ParentOwnedSubagentLiveCheck(SimpleNamespace(repo=Path(__file__).resolve().parents[2],
            output=self.root / "evidence", binary=cli, profile_home=profile, query_timeout=60))
        try:
            with patch.dict(os.environ, {"METIS_HOME": "do-not-read-this-profile"}):
                checker.load_route()
                self.assertEqual(os.environ["METIS_HOME"], "do-not-read-this-profile")
            actual = tomllib.loads((checker.home / "config.toml").read_text())
            self.assertEqual(actual["provider"]["custom"]["offline-route"]["max_tokens"], 8192)
            self.assertEqual(actual["provider"]["custom"]["offline-route"]["model"], "offline-model")
            self.assertEqual(checker.report["original_route"]["private_parameter_overrides"]["max_tokens"], 8192)
            self.assertEqual(checker.report["checks"][0]["route"]["private_parameter_overrides"]["max_tokens"], 8192)
            recorded = [json.loads(line) for line in (checker.output / "progress.jsonl").read_text().splitlines()]
            self.assertEqual(recorded[0]["route"]["private_parameter_overrides"]["max_tokens"], 8192)
            self.assertEqual((profile / "config.toml").read_text(), config)
        finally:
            checker.secrets.clear()
            shutil.rmtree(checker.root)

    def test_output_limit_replacement_preserves_other_fields_and_rejects_ambiguity(self):
        for key in ("max_tokens", '"max_tokens"', "'max_tokens'"):
            text = '[provider.custom.fixture]\nmodel = "unit"\n' + key + ' = 2048 # output cap\n'
            replaced = restore_selected_output_limit(text, "fixture", 8192)
            self.assertEqual(tomllib.loads(replaced), {"provider": {"custom": {"fixture": {"model": "unit", "max_tokens": 8192}}}})
        for text in ('[provider.custom.fixture]\nmax_tokens = 2049\n',
                     '[provider.custom.fixture]\nmax_tokens = 2048\n[other]\nmax_tokens = 2048\n'):
            with self.subTest(text=text), self.assertRaises(AssertionError):
                restore_selected_output_limit(text, "fixture", 8192)

    def test_child_is_selected_by_real_parent_name_and_filename(self):
        child = self.root / "agt-one.jsonl"
        header = {"id": "agt-one", "sub_agent_of": "parent-one", "teammate_name": "中文核对"}
        child.write_text(json.dumps({"type": "header", "header": header}) + "\n")
        selected, _ = owned_child(self.root, "parent-one", "中文核对", "agt-one")
        self.assertEqual(selected, child)
        for parent, name, identifier in (("foreign", "中文核对", ""), ("parent-one", "other", ""),
                                        ("parent-one", "中文核对", "agt-other")):
            with self.subTest(parent=parent, name=name, identifier=identifier), self.assertRaises(AssertionError):
                owned_child(self.root, parent, name, identifier)
        header["id"] = "different-filename"
        child.write_text(json.dumps({"type": "header", "header": header}) + "\n")
        with self.assertRaises(AssertionError):
            owned_child(self.root, "parent-one", "中文核对")

    def test_execution_identity_rejects_loss_or_change(self):
        saved = {"version": 1, "provider": "route", "model": "model", "effort": "high", "workDir": str(self.root), "tools": ["Read", "Bash"]}
        rows = [{"type": "execution", "execution": saved}]
        self.assertEqual(execution_record(rows), saved)
        assert_execution(saved, provider="route", model="model", cwd=self.root, effort="high")
        with self.assertRaises(AssertionError):
            execution_record([])
        with self.assertRaises(AssertionError):
            execution_record(rows + [{"type": "execution", "execution": {**saved, "effort": "low"}}])
        for field, value in (("provider", "different"), ("model", "different"), ("workDir", "/other"),
                             ("effort", "low"), ("tools", ["Read", "Bash", "Write"]), ("tools", None)):
            with self.subTest(field=field, value=value), self.assertRaises(AssertionError):
                assert_execution({**saved, field: value}, provider="route", model="model", cwd=self.root, effort="high")

    def test_followup_after_terminal_does_not_count_as_live_input(self):
        delivery = {"type": "pending_input", "prompt": "opaque-followup"}
        terminal = {"type": "terminal", "terminal": {"status": "completed"}}
        active_followup_recorded_before_terminal([delivery, terminal], "opaque-followup")
        for rows in ([terminal, delivery], [terminal], [delivery]):
            with self.subTest(rows=rows), self.assertRaises(AssertionError):
                active_followup_recorded_before_terminal(rows, "opaque-followup")

    def event(self, kind, invocation="root", parent="", **values):
        return {"session_id": "session", "trace_invocation_id": invocation, "trace_parent_invocation_id": parent,
            "trace_call_id": "call", "tool_use_id": "use", "tool_name": "Agent", "kind": kind, **values}

    def test_rejected_tool_requires_real_matching_root_args_and_result(self):
        args = {"resume_from": "agt-one", "prompt": "continue"}
        rejected = self.event("tool_result", text="resume refused: different parent", is_error=True, parent_id="use")
        self.assertEqual(agent_calls([rejected]), [])
        result = agent_calls([self.event("tool_args", text=json.dumps(args)), rejected])
        self.assertEqual(len(result), 1)
        self.assertEqual(result[0]["arguments"], args)
        self.assertTrue(result[0]["is_error"])
        wrong_call = {**rejected, "trace_call_id": "other"}
        self.assertEqual(agent_calls([self.event("tool_args", text=json.dumps(args)), wrong_call]), [])

    def test_started_child_container_result_is_matched_to_root(self):
        args = {"name": "中文核对", "prompt": "read fixture"}
        rows = [self.event("tool_args", text=json.dumps(args)),
                self.event("tool_start", invocation="child", parent="root", text=json.dumps(args)),
                self.event("tool_result", invocation="child", parent="root", parent_id="use", text="done")]
        result = agent_calls(rows)
        self.assertEqual(len(result), 1)
        self.assertEqual(result[0]["arguments"], args)
        self.assertFalse(result[0]["is_error"])

    def test_nested_child_agent_attempt_cannot_satisfy_parent_oracle(self):
        args = {"resume_from": "agt-one", "prompt": "continue"}
        rows = [self.event("tool_args", invocation="nested", parent="root", text=json.dumps(args)),
                self.event("tool_result", invocation="nested", parent="root", parent_id="use", text="parent refused", is_error=True)]
        self.assertEqual(agent_calls(rows), [])

    def assert_plan_boundary(self, rows, **changes):
        options = {"child_before": b"original journal\n", "child_after": b"original journal\n",
            "protected_before": b"ORIGINAL\n", "protected_after": b"ORIGINAL\n"}
        options.update(changes)
        return live_check.assert_plan_continuation_rejected(rows, "agt-one", **options)

    def test_plan_requires_actual_exact_child_permission_rejection(self):
        arguments = self.event("tool_args", text=json.dumps({"resume_from": "agt-one", "prompt": "read only"}))
        for reason in ("denied: mode:plan",
                       "plan mode cannot continue an implementation sub-agent; stop it and create a read-only planning child",
                       'resume_from=agt-one refused: saved child permission mode "dontAsk" cannot be enforced under current parent mode "plan"; restore the original parent mode or start a new child'):
            with self.subTest(reason=reason):
                rejection = self.event("tool_result", text=reason, is_error=True)
                self.assertEqual(len(self.assert_plan_boundary([arguments, rejection])), 1)
        for rows in ([self.event("text", text="I refused because plan mode is enabled")],
                     [arguments],
                     [arguments, self.event("tool_result", text="denied: mode:plan")],
                     [arguments, self.event("tool_result", text="unknown child in this plan", is_error=True)],
                     [self.event("tool_args", text=json.dumps({"resume_from": "agt-other"})),
                      self.event("tool_result", text="denied: mode:plan", is_error=True)]):
            with self.subTest(rows=rows), self.assertRaises(AssertionError):
                self.assert_plan_boundary(rows)

    def test_plan_rejection_requires_unchanged_journal_and_files_and_no_child_execution(self):
        rows = [self.event("tool_args", text=json.dumps({"resume_from": "agt-one", "prompt": "read only"})),
                self.event("tool_result", text="denied: mode:plan", is_error=True)]
        for change in ({"child_after": b"new journal entry\n"}, {"protected_after": b"CHANGED\n"}):
            with self.subTest(change=change), self.assertRaises(AssertionError):
                self.assert_plan_boundary(rows, **change)
        for event in (self.event("tokens", invocation="nested", parent="root", text="input=1 output=1 cache_read=0 cache_write=0"),
                      self.event("tool_result", invocation="nested", parent="root", tool_name="Read", text="fixture")):
            with self.subTest(event=event), self.assertRaises(AssertionError):
                self.assert_plan_boundary([*rows, event])

    def test_gate_requires_successful_receipt_for_the_exact_live_child(self):
        args = {"resume_from": "agt-one", "prompt": "continue"}
        rows = [self.event("tool_args", text=json.dumps(args)),
                self.event("tool_result", parent_id="use", text="accepted")]
        self.assertIsNotNone(accepted_continuation(rows, "agt-one"))
        self.assertIsNone(accepted_continuation(rows, "agt-other"))
        self.assertIsNone(accepted_continuation([rows[0], {**rows[1], "is_error": True}], "agt-one"))

    def test_parent_fixture_access_cannot_pass_as_child_delegation(self):
        child = {"child_tool_results": {"Read": 1, "Bash": 1}}
        assert_delegated_execution({"tool_results": {"Agent": 2, "SubAgentOutput": 1}}, child, require_bash=True)
        for tool in ("Read", "Write", "Bash"):
            with self.subTest(tool=tool), self.assertRaises(AssertionError):
                assert_delegated_execution({"tool_results": {"Agent": 2, tool: 1}}, child, require_bash=True)

    def test_child_text_or_tokens_without_real_read_cannot_pass(self):
        parent = {"tool_results": {"Agent": 1}}
        for child in ({"child_tool_results": {}}, {"child_tool_results": {"Bash": 1}}):
            with self.subTest(child=child), self.assertRaises(AssertionError):
                assert_delegated_execution(parent, child, require_bash=False)
        child = {"child_tool_results": {"Read": 1}}
        assert_delegated_execution(parent, child, require_bash=False)
        with self.assertRaises(AssertionError):
            assert_delegated_execution(parent, child, require_bash=True)

    def test_live_identity_poll_ignores_partial_header_but_rejects_duplicate(self):
        header = {"type": "header", "header": {"id": "agt-one", "sub_agent_of": "parent", "teammate_name": "中文核对"}}
        child = self.root / "agt-one.jsonl"
        child.write_text(json.dumps(header))
        self.assertEqual(live_child_id(self.root, "parent", "中文核对"), "")
        child.write_text(json.dumps(header) + "\n" + "partial-tail")
        self.assertEqual(live_child_id(self.root, "parent", "中文核对"), "agt-one")
        other = self.root / "agt-two.jsonl"
        other.write_text(json.dumps({"type": "header", "header": {**header["header"], "id": "agt-two"}}) + "\n")
        with self.assertRaises(AssertionError):
            live_child_id(self.root, "parent", "中文核对")


if __name__ == "__main__":
    unittest.main()
