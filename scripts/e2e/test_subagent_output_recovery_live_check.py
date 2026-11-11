from __future__ import annotations
import argparse
import contextlib
import copy
import io
import json
from pathlib import Path
import tempfile
import unittest

import subagent_output_recovery_live_check as check


def text(value: str) -> dict:
    return {"role": "assistant", "content": [{"type": "text", "text": value}]}


def boundary(attempt: int, reason: str = "max_tokens") -> dict:
    return {"role": "user", "content": [{"type": "text", "text": "host boundary", "synthetic": True,
        "provider_hint": {"metis.output_continuation": reason, "metis.output_continuation_attempt": str(attempt)}}]}


def fixture(outcome: str = "recovered") -> tuple[list, list, list]:
    attempts = {"default": 0, "recovered": 1, "exhausted": 3}[outcome]
    rows = [{"type": "header", "header": {"id": "agt-fixture", "sub_agent_of": "parent", "teammate_name": "中文子代理", "model": check.MODEL}},
            {"type": "execution", "execution": {"model": check.MODEL, "effort": "high", "maxOutputTokens": 0 if not attempts else 2048}}]
    history = [text("part0")]
    for index in range(1, attempts + 1):
        history += [boundary(index), text("part" + str(index))]
    body = "".join("part" + str(index) for index in range(attempts + 1))
    rows.append({"type": "terminal", "terminal": {"status": "failed" if outcome == "exhausted" else "completed",
        "output": body, "stopHint": "max_tokens" if outcome == "exhausted" else "end_turn", "continuationAttempts": attempts}})
    usage = [{"agent_id": "agt-fixture", "parent_session_id": "parent", "receipt_id": "receipt-" + str(index), "reported": True,
              "input_tokens": 15, "output_tokens": 20} for index in range(attempts + 1)]
    return rows, history, usage


class ChildOracleTests(unittest.TestCase):
    def oracle(self, outcome: str = "recovered", data: tuple | None = None) -> dict:
        rows, history, usage = data or fixture(outcome)
        return check.child_oracle(rows, history, usage, "parent", "中文子代理", cap=0 if outcome == "default" else 2048, outcome=outcome)

    def test_default_recovered_and_three_attempt_exhaustion(self) -> None:
        for outcome in ("default", "recovered", "exhausted"):
            with self.subTest(outcome=outcome):
                result = self.oracle(outcome)
                self.assertTrue(result["partial_segments_preserved_in_order"])
                self.assertEqual(result["host_output_length_trigger_count"], {"default": 0, "recovered": 1, "exhausted": 3}[outcome])

    def test_claimed_continuation_without_host_length_boundary_is_rejected(self) -> None:
        rows, history, usage = fixture()
        history.pop(1)
        with self.assertRaises(AssertionError): self.oracle(data=(rows, history, usage))

    def test_recovery_of_non_length_failure_is_rejected(self) -> None:
        rows, history, usage = fixture()
        history[1] = boundary(1, "timeout")
        with self.assertRaises(AssertionError): self.oracle(data=(rows, history, usage))

    def test_duplicate_terminal_and_unbounded_attempts_are_rejected(self) -> None:
        for mutation in ("terminal", "attempt"):
            rows, history, usage = fixture()
            if mutation == "terminal": rows.append(copy.deepcopy(rows[-1]))
            else: rows[-1]["terminal"]["continuationAttempts"] = 4
            with self.assertRaises(AssertionError): self.oracle(data=(rows, history, usage))

    def test_provider_length_reason_is_preserved_without_fake_normalization(self) -> None:
        rows, history, usage = fixture("exhausted")
        rows[-1]["terminal"]["stopHint"] = "length"
        for message in history:
            for block in message["content"]:
                if "provider_hint" in block:
                    block["provider_hint"]["metis.output_continuation"] = "length"
        self.assertEqual(self.oracle("exhausted", (rows, history, usage))["stop_reason"], "length")

    def test_parent_model_effort_or_cap_drift_is_rejected(self) -> None:
        for field, value in (("model", "other"), ("effort", "low"), ("maxOutputTokens", 4096)):
            rows, history, usage = fixture()
            rows[1]["execution"][field] = value
            with self.assertRaises(AssertionError): self.oracle(data=(rows, history, usage))

    def test_partial_body_cannot_be_replaced_by_only_final_segment(self) -> None:
        rows, history, usage = fixture()
        rows[-1]["terminal"]["output"] = "part1"
        with self.assertRaises(AssertionError): self.oracle(data=(rows, history, usage))

    def test_reasoning_only_stop_is_not_claimed_as_lost_visible_partial(self) -> None:
        rows, history, usage = fixture()
        rows[-1]["terminal"]["output"] = ""
        with self.assertRaisesRegex(AssertionError, "reasoning-only length stop does not qualify"):
            self.oracle(data=(rows, history, usage))

    def test_canonical_history_is_not_deduplicated_a_second_time(self) -> None:
        rows, history, usage = fixture()
        history[-1] = text("part0part1")
        rows[-1]["terminal"]["output"] = "part0part0part1"
        self.assertEqual(self.oracle(data=(rows, history, usage))["cumulative_output_characters"], len("part0part0part1"))
        rows[-1]["terminal"]["output"] = "part0part1"
        with self.assertRaises(AssertionError): self.oracle(data=(rows, history, usage))

    def test_tool_side_effect_after_continuation_boundary_is_rejected(self) -> None:
        rows, history, usage = fixture()
        history[-1]["content"].append({"type": "tool_use", "tool_use_id": "effect", "name": "Bash", "input": {"command": "inert"}})
        with self.assertRaises(AssertionError): self.oracle(data=(rows, history, usage))

    def test_receipts_use_last_absolute_value_without_double_counting(self) -> None:
        usage = [{"agent_id": "a", "parent_session_id": "p", "receipt_id": "r", "reported": True, "output_tokens": value} for value in (3, 5, 5)]
        result = check.ledger_oracle(usage, "a", "p")
        self.assertEqual(result["unique_physical_stream_receipts"], 1)
        self.assertEqual(result["usage_last_absolute_per_receipt"]["output_tokens"], 5)

    def test_foreign_or_missing_real_usage_is_rejected(self) -> None:
        for usage in ([], [{"agent_id": "foreign", "parent_session_id": "p"}]):
            with self.assertRaises(AssertionError): check.ledger_oracle(usage, "a", "p")


class ParentReceiptTests(unittest.TestCase):
    def block(self, started: bool = True) -> dict:
        return {"type": "tool_result", "is_error": True, "presentation": {"metis.agent_started": started,
            "subagent": {"status": "failed", "continuationAttempts": 3, "stopReason": "max_tokens",
                         "diagnostic": "output budget exhausted", "partialOutput": True}}}

    def test_started_exhaustion_is_not_startup_failure(self) -> None:
        result = check.presentation_oracle(self.block(), started=True, status="failed", attempts=3, reason="max_tokens")
        self.assertTrue(result["started"])
        with self.assertRaises(AssertionError):
            check.presentation_oracle(self.block(False), started=True, status="failed", attempts=3, reason="max_tokens")

    def test_failed_receipt_requires_reason_partial_and_real_error_flag(self) -> None:
        for mutation in ("diagnostic", "partial", "flag"):
            block = self.block()
            if mutation == "diagnostic": block["presentation"]["subagent"].pop("diagnostic")
            elif mutation == "partial": block["presentation"]["subagent"]["partialOutput"] = False
            else: block["is_error"] = False
            with self.assertRaises(AssertionError):
                check.presentation_oracle(block, started=True, status="failed", attempts=3, reason="max_tokens")

    def test_agent_call_pairs_metis_occurrence_ids_and_canonical_tool_ids(self) -> None:
        history = [{"role": "assistant", "content": [{"type": "tool_use", "trace_call_id": "occ", "tool_use_id": "provider", "name": "Agent", "input": {"name": "中文"}}]},
                   {"role": "user", "content": [{"type": "tool_result", "trace_call_id": "occ", "tool_use_id": "provider", "content": "receipt"}]}]
        self.assertEqual(check.agent_result_blocks(history)[0][0]["name"], "中文")


class IndependentVerificationOracleTests(unittest.TestCase):
    def setUp(self) -> None:
        self.effect, self.source = Path("/private/fixture/effect"), Path("/private/fixture/copy")
        self.script = Path("/private/fixture/run-once.sh")
        self.command = str(self.script)
        def tool(name: str, arguments: dict, result: str, identity: str) -> list:
            return [{"role": "assistant", "content": [{"type": "tool_use", "tool_use_id": identity, "name": name, "input": arguments}]},
                    {"role": "user", "content": [{"type": "tool_result", "tool_use_id": identity, "content": result}]}]
        self.history = tool("Bash", {"command": self.command}, "ok\tfixture.local/effectcheck\t0.001s", "bash")
        self.history += tool("Read", {"path": str(self.source)}, "source", "read")
        self.history += tool("Agent", {"subagent_type": "verify"}, "Read both fixtures\nVERDICT: PASS", "verify")
        self.history += [boundary(1), text("recovered text")]
        self.verifier_history = tool("Read", {"path": str(self.effect)}, "nonce", "ve")
        self.verifier_history += tool("Read", {"path": str(self.source)}, "source", "vs")
        self.terminal = {"status": "completed", "stopHint": "end_turn", "output": "Actually read two fixtures\nVERDICT: PASS"}

    def oracle(self) -> dict:
        return check.side_effect_verification_oracle(self.history, self.verifier_history, self.terminal,
            script=self.script, effect=self.effect, source=self.source)

    def test_actual_validation_and_read_only_verifier_pass_before_continuation(self) -> None:
        proof = self.oracle()
        self.assertTrue(proof["actual_go_validation_pass"])
        self.assertTrue(proof["independent_verify_pass_before_recovery"])

    def test_missing_verify_cannot_claim_mutation_completed(self) -> None:
        self.history = self.history[:4] + self.history[6:]
        with self.assertRaises(AssertionError): self.oracle()

    def test_failed_or_nonpass_verifier_is_rejected(self) -> None:
        for mutation in ("error", "verdict", "journal"):
            with self.subTest(mutation=mutation):
                self.setUp()
                if mutation == "error": self.history[5]["content"][0]["is_error"] = True
                elif mutation == "verdict": self.history[5]["content"][0]["content"] = "VERDICT: FAIL"
                else: self.terminal["output"] = "VERDICT: FAIL"
                with self.assertRaises(AssertionError): self.oracle()

    def test_model_claim_without_go_test_receipt_is_rejected(self) -> None:
        self.history[1]["content"][0]["content"] = "I believe tests pass"
        with self.assertRaises(AssertionError): self.oracle()

    def test_only_one_authorized_script_equivalent_shell_forms_are_allowed(self) -> None:
        for command in (str(self.script), "'" + str(self.script) + "'", "sh " + str(self.script),
                        "/bin/sh " + str(self.script), "bash " + str(self.script), "/bin/bash " + str(self.script)):
            self.history[0]["content"][0]["input"]["command"] = command
            self.assertTrue(self.oracle()["actual_go_validation_pass"])
        for command in (str(self.script) + "; echo bad", str(self.script) + " && true", str(self.script) + " extra",
                        "sh -c '" + str(self.script) + "'", "env sh " + str(self.script), "/private/other.sh",
                        str(self.script) + " | cat", str(self.script) + "\n" + str(self.script)):
            self.history[0]["content"][0]["input"]["command"] = command
            with self.assertRaises(AssertionError): self.oracle()

    def test_failure_command_diagnostics_redact_unapproved_tokens_and_preserve_hash(self) -> None:
        self.history[0]["content"][0]["input"]["command"] = "/bin/sh /private/fixture/run-once.sh secret-value"
        diagnostic = check.safe_tool_diagnostics(self.history, self.script, Path("/private/fixture"))[0]
        self.assertEqual(diagnostic["command_tokens"], ["/bin/sh", "<private-root>/run-once.sh", "<redacted-token>"])
        self.assertNotIn("secret-value", json.dumps(diagnostic))
        self.assertEqual(len(diagnostic["command_sha256"]), 64)

    def test_read_diagnostics_and_bounded_input_correction_do_not_hide_valid_read_failure(self) -> None:
        with tempfile.TemporaryDirectory() as folder:
            workspace = Path(folder)
            source = workspace / "long-copy.txt"; source.write_text("fixture")
            def call(arguments, result="Read: path is required", tool="Read"):
                return {"tool": tool, "input": arguments, "is_error": True, "result": result, "call_index": 0, "result_index": 1}
            for arguments in ({}, {"path": "long-copy.txt"}, {"path": str(workspace / "long-cop.txt")}, {"path": str(source), "byte_limit": 1}):
                self.assertEqual(len(check.recoverable_read_errors([call(arguments)], workspace)), 1)
            self.assertEqual(check.recoverable_read_errors([call({"path": "long-copy.txt"}, "path must be absolute")], workspace)[0]["category"],
                "nonabsolute_native_path_parameter")
            for value in (call({"path": str(source)}, "open " + str(source) + ": no such file"),
                          call({"path": str(workspace.parent / "other.txt")}), call({}, tool="Bash")):
                with self.assertRaises(AssertionError): check.recoverable_read_errors([value], workspace)
            self.assertTrue(check.fixture_read_path(str(source), source))
            self.assertFalse(check.fixture_read_path("long-copy.txt", source))
            self.assertFalse(check.fixture_read_path("../long-copy.txt", source))
            history = [{"role": "assistant", "content": [{"type": "tool_use", "tool_use_id": "read", "name": "Read",
                "input": {"path": str(source), "byte_offset": 8192, "byte_limit": 8192}}]},
                {"role": "user", "content": [{"type": "tool_result", "tool_use_id": "read", "is_error": True,
                    "content": "open " + str(source) + ": no such file or directory"}]}]
            diagnostic = check.safe_tool_diagnostics(history, None, workspace)[0]
            self.assertEqual(diagnostic["read_parameters"]["byte_offset"], 8192)
            self.assertEqual(diagnostic["read_parameters"]["path"], "<private-root>/long-copy.txt")
            self.assertIn("no such file", diagnostic["read_error_context"])
            self.assertNotIn(folder, json.dumps(diagnostic))

    def test_verifier_after_recovery_or_with_write_is_rejected(self) -> None:
        self.history = self.history[:4] + [boundary(1)] + self.history[4:6] + [text("late")]
        with self.assertRaises(AssertionError): self.oracle()
        self.setUp()
        self.verifier_history[0]["content"][0]["name"] = "Write"
        with self.assertRaises(AssertionError): self.oracle()


class CleanupTests(unittest.TestCase):
    def test_preparation_failure_destroys_cli_without_any_model_or_ui(self) -> None:
        with tempfile.TemporaryDirectory() as folder:
            root = Path(folder); binary = root / "disposable"; binary.write_bytes(b"not executed")
            source = root / "source"; source.write_text("freeze")
            freeze = root / "freeze.json"; freeze.write_text(json.dumps({"files": {"source": check.digest(source)}}))
            args = argparse.Namespace(binary=binary, destroy_input=True, expected_sha256="0" * 64, source_freeze=freeze,
                repo=root, source_home=root / "never-read", output=root / "evidence.noindex", query_timeout=60,
                success_cap=2048, exhaustion_cap=1024, hold_seconds=0, stop_file=None)
            with contextlib.redirect_stdout(io.StringIO()):
                harness = check.SubagentOutputRecoveryLiveCheck(args)
                result = harness.run()
            self.assertEqual(result, 1)
            self.assertFalse(binary.exists()); self.assertFalse(harness.root.exists())
            self.assertEqual(harness.report["model_turn_submissions"], 0)
            self.assertEqual(harness.report["real_cli_invocations"], 0)
            self.assertTrue(harness.report["cleanup"]["credential_environment_cleared"])


class ExecutionDiagnosticsTests(unittest.TestCase):
    def test_safe_execution_and_agent_input_do_not_retain_prompt_or_path(self) -> None:
        execution = {"model": check.MODEL, "effort": "high", "maxOutputTokens": 2048,
            "profile": "output-copy", "permissionMode": "fullAccess", "tools": ["Read"],
            "system": "private prompt", "workDir": "/private/fixture"}
        safe = check.safe_execution(execution)
        self.assertEqual(safe["effectiveEffort"], "high")
        self.assertEqual(safe["effectiveMaxOutputTokens"], 2048)
        self.assertEqual(len(safe["systemSha256"]), 64)
        self.assertNotIn("private prompt", json.dumps(safe))
        self.assertNotIn("/private/fixture", json.dumps(safe))
        arguments = {"name": "中文", "model": check.MODEL, "max_output_tokens": 2048,
            "subagent_type": "output-copy", "prompt": "private prompt", "cwd": "/private/fixture"}
        self.assertEqual(set(check.safe_agent_input(arguments)), {"name", "model", "max_output_tokens", "subagent_type"})

    def test_model_effort_and_cap_have_separate_actual_value_errors(self) -> None:
        for field, value, message in (("model", "other", "model mismatch: actual='other'"),
                ("effort", "low", "effort mismatch: actual='low'"),
                ("maxOutputTokens", 4096, "cap mismatch: actual=4096")):
            execution = {"model": check.MODEL, "effort": "high", "maxOutputTokens": 2048}
            execution[field] = value
            with self.assertRaisesRegex(AssertionError, message): check.assert_execution(execution, 2048)
        with self.assertRaisesRegex(AssertionError, "effort mismatch: actual='default'"):
            check.assert_execution({"model": check.MODEL, "maxOutputTokens": 2048}, 2048)

    def harness(self, *, after: str = "high", persisted: str = "high"):
        harness = object.__new__(check.SubagentOutputRecoveryLiveCheck)
        harness.report = {}
        calls = []
        replies = [{}, {"supported": True, "model": check.MODEL, "effort": "default"},
            {"effort": "high"}, {"model": check.MODEL, "effort": after}, {"session": {"effort": persisted}}]
        def http(method, path, data=None):
            calls.append((method, path, data))
            return 200, replies.pop(0), {}
        harness.http = http
        return harness, calls

    def test_web_turn_explicitly_selects_and_persists_actual_high(self) -> None:
        harness, calls = self.harness()
        harness.ensure_web_high("parent")
        self.assertEqual(calls[2], ("POST", "/api/effort", {"effort": "high"}))
        self.assertEqual(harness.report["web_effort_checks"][0]["before"], "default")
        self.assertEqual(harness.report["web_effort_checks"][0]["actual_after"], "high")

    def test_web_high_requires_actual_and_canonical_values_not_only_post_receipt(self) -> None:
        for values in ({"after": "default"}, {"persisted": "default"}):
            harness, _ = self.harness(**values)
            with self.assertRaises(AssertionError): harness.ensure_web_high("parent")


class ExhaustionCopyFixtureTests(unittest.TestCase):
    def test_random_fixture_has_bounded_source_size_and_exact_record_count(self) -> None:
        source = check.exhaustion_fixture("nonce")
        self.assertEqual(len(source.splitlines()), 514)
        self.assertGreaterEqual(len(source.encode()), 49152)
        self.assertLessEqual(len(source.encode()), 73728)
        self.assertEqual(len({line.split("：")[1].split("。")[0] for line in source.splitlines()[1:-1]}), 512)

    def test_strict_contiguous_prefix_allows_bounded_durable_preface(self) -> None:
        source = check.exhaustion_fixture("nonce")
        for prefix in (source[:800], "# 原文\n" + source[:800], "I will read and copy the private source.\n" + source[:800]):
            self.assertTrue(check.incomplete_prefix_oracle(prefix, source)["exact_contiguous_source_prefix"])
        for invalid in (source, source[:300] + source[350:800], source[:300] + source[:300],
                        "x" * 257 + source[:800], source[:800] + "...", "no begin marker"):
            with self.assertRaises(AssertionError): check.incomplete_prefix_oracle(invalid, source)

    def test_copy_difference_retains_only_position_and_sizes(self) -> None:
        source = "BEGIN\nprivate random body\nEND"
        diagnostic = check.copy_difference("prefix\nBEGIN\nprivate Xandom body\nEND", source)
        self.assertEqual(diagnostic["begin_marker_offset"], 7)
        self.assertEqual(diagnostic["first_different_character_offset"], 14)
        self.assertNotIn("private", json.dumps(diagnostic))


class EmittedProviderTextOracleTests(unittest.TestCase):
    def events(self, first="part0", second="part1"):
        return [{"kind": "text", "text": first, "stream_id": "stream1"},
                {"kind": "loop_done", "text": "max_tokens"},
                {"kind": "context", "source": "recovery", "text": '<subagent_output_continuation reason="max_tokens" attempt="1" max_attempts="3"/>'},
                {"kind": "text", "text": second, "stream_id": "stream2"},
                {"kind": "loop_done", "text": "end_turn"}]

    def oracle(self, events=None, history=None, terminal=None):
        rows, canonical, _ = fixture()
        return check.provider_text_oracle(events or self.events(), history or canonical, terminal or rows[-1]["terminal"])

    def test_independent_raw_provider_text_matches_both_canonical_and_terminal(self):
        self.assertTrue(self.oracle()["emitted_provider_text_verified"])
        self.assertTrue(self.oracle(self.events(second="part0part1"))["emitted_provider_text_verified"])

    def test_original_provider_copy_difference_is_quality_not_a_host_mutation(self):
        source = "BEGIN\n步骤001：aaaa\n步骤002：bbbb\nEND"
        changed = source.replace("aaaa", "aXXaa")
        proof = check.provider_text_oracle([{"kind": "text", "text": changed, "stream_id": "s"},
            {"kind": "loop_done", "text": "end_turn"}], [text(changed)], {"output": changed, "stopHint": "end_turn"})
        self.assertTrue(proof["emitted_provider_text_verified"])
        quality = check.source_copy_quality(changed, source, complete=True)
        self.assertFalse(quality["source_copy_exact"])
        self.assertEqual(quality["ordered_numbered_records"], 2)

    def test_host_dropped_changed_or_incorrectly_removed_text_fails(self):
        for second in ("partX1", "part", "NEWpart1"):
            with self.assertRaises(AssertionError): self.oracle(self.events(second=second))
        rows, history, _ = fixture()
        rows[-1]["terminal"]["output"] = "part0NEWpart1"
        with self.assertRaises(AssertionError): self.oracle(terminal=rows[-1]["terminal"])

    def test_full_previous_assistant_prefix_is_removed_once_but_short_matches_preserved(self):
        events = self.events(first="commentarybody", second="bodytail")
        events.insert(1, {"kind": "text", "text": "body", "stream_id": "last-initial-response"})
        # Initial cumulative text is commentarybodybody; only the last assistant
        # body is repeated in the second response.
        expected = "commentarybodybodytail"
        self.oracle(events, [text(expected)], {"output": expected, "stopHint": "end_turn", "continuationAttempts": 1})
        short = self.events(first="long prefix", second="long ")
        expected = "long prefixlong "
        self.oracle(short, [text(expected)], {"output": expected, "stopHint": "end_turn", "continuationAttempts": 1})

    def test_missing_truncated_redacted_or_mismatched_boundary_evidence_fails(self):
        for mutation in ("missing", "truncated", "redacted", "boundary", "stop"):
            events = self.events()
            if mutation == "missing": events = []
            elif mutation == "truncated": events[0]["truncated"] = True
            elif mutation == "redacted": events[0]["text"] = "[REDACTED]"
            elif mutation == "boundary": events[2]["text"] = events[2]["text"].replace('attempt="1"', 'attempt="2"')
            else: events[1]["text"] = "timeout"
            rows, history, _ = fixture()
            with self.assertRaises(AssertionError): check.provider_text_oracle(events, history, rows[-1]["terminal"])

    def test_business_record_loss_or_out_of_order_still_fails(self):
        source = "BEGIN\n步骤001：aaaa\n步骤002：bbbb\nEND"
        for output in ("BEGIN\n步骤002：bbbb\nEND", "BEGIN\n步骤002：bbbb\n步骤001：aaaa\nEND", source.replace("END", "")):
            with self.assertRaises(AssertionError): check.source_copy_quality(output, source, complete=True)

    def test_actual_source_pages_must_cover_every_byte_and_retain_page_bodies(self) -> None:
        source = "BEGIN\n" + "abcdefghij" * 1000 + "\nEND"
        raw = source.encode()
        def page(start, end):
            return {"result": f"[Read source bytes {start}-{end} of {len(raw)}; end exclusive]\n" + raw[start:end].decode()}
        calls = [page(0, 8192), page(8192, len(raw))]
        self.assertEqual(check.complete_read_coverage_oracle(calls, source)["complete_native_read_source_bytes"], len(raw))
        for invalid in ([calls[0]], [page(0, 8191), calls[1]], [{"result": calls[0]["result"][:100]}, calls[1]]):
            with self.assertRaises(AssertionError): check.complete_read_coverage_oracle(invalid, source)


if __name__ == "__main__": unittest.main()
