from __future__ import annotations

import argparse
import contextlib
import copy
import hashlib
import io
import json
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

import context_integrity_live_check as check


def messages(tool: str, arguments: dict, result: str, *, error: bool = False, identity: str = "call") -> list[dict]:
    return [{"role": "assistant", "content": [{"type": "tool_use", "id": identity, "name": tool, "input": arguments}]},
            {"role": "user", "content": [{"type": "tool_result", "tool_use_id": identity, "content": result, "is_error": error}]}]


class ReadVisibilityOracleTests(unittest.TestCase):
    def setUp(self) -> None:
        self.temporary = tempfile.TemporaryDirectory()
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name)
        self.target, self.control = self.root / "long", self.root / "small"
        self.target.write_bytes(b"Q" * 60000)
        self.control.write_text("SMALL_WRITTEN\n")
        self.sha = check.digest(self.target)
        self.canary = "HIDDEN_TAIL_test_only"
        self.history = []
        self.history += messages("Read", {"path": str(self.target), "offset": 1, "limit": 1}, "bounded page QQQ", identity="read-first")
        self.history += messages("Write", {"path": str(self.target), "content": "GUARD_MUST_REJECT"}, check.PARTIAL_ERROR, error=True, identity="write-guard")
        self.history += messages("Read", {"path": str(self.target), "byte_offset": 59872, "byte_limit": 128}, self.canary, identity="read-tail")
        self.history += messages("Read", {"path": str(self.control)}, "FIRST\nSECOND\nLAST", identity="read-control")
        self.history += messages("Write", {"path": str(self.control), "content": "SMALL_WRITTEN\n"}, "wrote fixture", identity="write-control")

    def oracle(self, history: list[dict] | None = None) -> dict:
        return check.read_write_oracle(history or self.history, self.target, self.control, self.canary, self.sha)

    def test_expected_guard_error_is_required_and_small_write_allows_newline(self) -> None:
        proof = self.oracle()
        self.assertTrue(proof["whole_write_rejected"])
        self.assertTrue(proof["long_file_unchanged"])
        self.assertEqual(proof["expected_tool_errors"], 1)

    def test_successful_overwrite_is_not_mistaken_for_a_passing_negative_test(self) -> None:
        history = copy.deepcopy(self.history)
        history[3]["content"][0].update({"is_error": False, "content": "wrote target"})
        with self.assertRaises(AssertionError):
            self.oracle(history)

    def test_rejected_tool_cannot_silently_change_file(self) -> None:
        self.target.write_text("GUARD_MUST_REJECT")
        with self.assertRaises(AssertionError):
            self.oracle()

    def test_spill_or_invisible_tail_is_rejected(self) -> None:
        for index, value in ((1, "Full output saved to: /fixture"), (5, "tail not found")):
            history = copy.deepcopy(self.history)
            history[index]["content"][0]["content"] = value
            with self.assertRaises(AssertionError):
                self.oracle(history)

    def test_guard_requires_a_later_model_message_after_read_result(self) -> None:
        history = copy.deepcopy(self.history)
        history[0]["content"].append(history[2]["content"][0])
        history.pop(2)
        with self.assertRaises(AssertionError):
            self.oracle(history)

    def test_non_fixture_access_and_unexpected_tool_error_are_rejected(self) -> None:
        for modification in ("path", "error"):
            history = copy.deepcopy(self.history)
            if modification == "path":
                history[0]["content"][0]["input"]["path"] = "/private/unrelated"
            else:
                history[5]["content"][0]["is_error"] = True
            with self.assertRaises(AssertionError):
                self.oracle(history)


class TaskProjectionOracleTests(unittest.TestCase):
    def setUp(self) -> None:
        self.steps = ["first", "second", "third"]
        self.extra = "separate execution"
        items = [{"content": name, "status": "completed"} for name in self.steps + [self.extra]]
        self.status = {"planSteps": items[:3], "executionItems": items[3:], "planItems": items}

    def test_canonical_four_records_and_display_three_plus_one(self) -> None:
        proof = check.plan_oracle(self.status, self.steps, self.extra)
        self.assertEqual((proof["main_steps"], proof["execution_items"], proof["canonical_items"]), (3, 1, 4))

    def test_dropping_business_record_or_flattening_display_is_rejected(self) -> None:
        for field in ("planItems", "planSteps", "executionItems"):
            status = copy.deepcopy(self.status)
            status[field] = status[field][:-1]
            with self.assertRaises(AssertionError):
                check.plan_oracle(status, self.steps, self.extra)

    def test_old_execution_task_cannot_replace_current_task(self) -> None:
        status = copy.deepcopy(self.status)
        status["executionItems"][0]["content"] = "old archived execution"
        with self.assertRaises(AssertionError):
            check.plan_oracle(status, self.steps, self.extra)

    def test_single_todo_event_is_independent_of_later_beta_and_replay(self) -> None:
        alpha = messages("TodoWrite", {"content": "ALPHA", "status": "completed"}, "completed ALPHA", identity="alpha")
        beta = messages("TodoWrite", {"content": "BETA", "status": "completed"}, "completed BETA", identity="beta")
        original = check.stable_digest(check.single_todo_event(alpha, "ALPHA"))
        rows = [{"type": "message", "message": row} for row in alpha]
        rows += [{"type": "history_replace", "messages": alpha + beta}]
        history = check.active_history(rows)
        self.assertEqual(check.stable_digest(check.single_todo_event(history, "ALPHA")), original)
        self.assertEqual(check.single_todo_event(history, "BETA")["input"]["content"], "BETA")
        with self.assertRaises(AssertionError):
            check.single_todo_event(messages("TodoWrite", {"todos": [{"content": "ALPHA", "status": "completed"}]}, "completed ALPHA"), "ALPHA")


class LifecycleTests(unittest.TestCase):
    def args(self, root: Path) -> argparse.Namespace:
        binary = root / "inert-disposable"
        binary.write_bytes(b"never executed")
        source = root / "source.txt"
        source.write_text("freeze")
        freeze = root / "freeze.json"
        freeze.write_text(json.dumps({"files": {"source.txt": check.digest(source)}}))
        return argparse.Namespace(binary=binary, destroy_input=True, expected_sha256="0" * 64, source_freeze=freeze,
            repo=root, source_home=root / "never-read-user-config", output=root / "evidence.noindex", query_timeout=30,
            max_output_tokens=4096, hold_seconds=0, stop_file=None)

    def test_preparation_failure_destroys_test_cli_before_returning(self) -> None:
        with tempfile.TemporaryDirectory() as folder:
            args = self.args(Path(folder))
            with contextlib.redirect_stdout(io.StringIO()):
                harness = check.ContextIntegrityLiveCheck(args)
                code = harness.run()
            self.assertEqual(code, 1)
            self.assertFalse(args.binary.exists())
            self.assertFalse(harness.root.exists())
            self.assertEqual(harness.report["model_turn_submissions"], 0)
            self.assertTrue(harness.report["cleanup"]["credential_environment_cleared"])

    def test_installed_cli_is_refused_before_cleanup_can_delete_it(self) -> None:
        with tempfile.TemporaryDirectory() as folder:
            binary = Path(folder) / "pretend-installed"
            binary.write_bytes(b"keep installed")
            with patch.object(check.shutil, "which", return_value=str(binary)):
                with self.assertRaises(AssertionError):
                    check.disposable_binary(binary)
            self.assertTrue(binary.exists())

    def test_symlink_input_is_refused(self) -> None:
        with tempfile.TemporaryDirectory() as folder:
            target, link = Path(folder) / "original", Path(folder) / "link"
            target.write_bytes(b"keep")
            link.symlink_to(target)
            with self.assertRaises(AssertionError):
                check.disposable_binary(link)
            self.assertTrue(target.exists())

    def test_source_freeze_detects_new_source_edits_before_any_model(self) -> None:
        with tempfile.TemporaryDirectory() as folder:
            args = self.args(Path(folder))
            args.expected_sha256 = check.digest(args.binary)
            (Path(folder) / "source.txt").write_text("changed")
            with contextlib.redirect_stdout(io.StringIO()):
                harness = check.ContextIntegrityLiveCheck(args)
                code = harness.run()
            self.assertEqual(code, 1)
            self.assertEqual(harness.report["model_turn_submissions"], 0)
            self.assertFalse(args.binary.exists())

    def test_auth_verification_error_cannot_skip_private_cleanup(self) -> None:
        with tempfile.TemporaryDirectory() as folder:
            root = Path(folder)
            args = self.args(root)
            auth = root / "synthetic-auth-check"
            auth.write_bytes(b"offline marker, no credential")
            harness = check.ContextIntegrityLiveCheck(args)
            original_digest = check.digest
            def fail_prepare() -> None:
                harness.source_auth, harness.source_auth_sha = auth, "old-hash"
                raise AssertionError("offline preparation intentionally failed")
            def failing_digest(path: Path) -> str:
                if path == auth:
                    raise OSError("offline auth verification failure")
                return original_digest(path)
            with patch.object(harness, "prepare", side_effect=fail_prepare), patch.object(check, "digest", side_effect=failing_digest):
                with contextlib.redirect_stdout(io.StringIO()):
                    code = harness.run()
            self.assertEqual(code, 1)
            self.assertFalse(harness.root.exists())
            self.assertFalse(args.binary.exists())
            self.assertFalse(harness.report["original_auth_unchanged"])


class SelectedPlanStatusTests(unittest.TestCase):
    def test_plan_status_explicitly_selects_the_owned_conversation(self) -> None:
        harness = object.__new__(check.ContextIntegrityLiveCheck)
        calls = []
        def http(method, path, value=None):
            calls.append((method, path, value))
            return 200, {"activeSessionId": "fixture-session"}, {}
        harness.http = http
        self.assertEqual(harness.status("fixture-session")["activeSessionId"], "fixture-session")
        self.assertEqual(calls[0], ("POST", "/api/sessions/activate", {"id": "fixture-session"}))
        self.assertEqual(calls[1][0], "GET")

    def test_status_for_another_selected_conversation_is_rejected(self) -> None:
        harness = object.__new__(check.ContextIntegrityLiveCheck)
        harness.http = lambda *args: (200, {"activeSessionId": "other-session"}, {})
        with self.assertRaises(AssertionError):
            harness.status("fixture-session")

    def test_failed_selection_cannot_be_accepted_as_plan_evidence(self) -> None:
        harness = object.__new__(check.ContextIntegrityLiveCheck)
        harness.http = lambda *args: (409, {}, {})
        with self.assertRaises(AssertionError):
            harness.status("fixture-session")


if __name__ == "__main__":
    unittest.main()
