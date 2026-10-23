"""Offline host-oracle regressions. No model, credentials, GUI or TCC access."""
from __future__ import annotations

from copy import deepcopy
import json
from pathlib import Path
import subprocess
import sys
import tempfile
from types import SimpleNamespace
import unittest
from unittest.mock import patch

from computer_use_cross_app_acceptance import CrossAppCheck, PREFIX, PreflightBlocked, display_geometry, effective_private_session_parameters, fixture_command_a_diagnostics, fixture_focus_diagnostics, fixture_input_grants, hydrate_native_results, identity, permission_preflight, validate_query
from persistent_agent_live_check import PersistentAgentLiveCheck, digest


GEOMETRY = {"active": 0, "displays": [{"index": 0, "x": 0, "y": 0, "width": 1440, "height": 900}],
            "display_width_px": 1440, "display_height_px": 900, "display_origin_x": 0, "display_origin_y": 0,
            "coord_space_note": "Mouse coordinates are GLOBAL desktop logical pixels."}


def oracle_fixture(keyboard_mouse=False):
    fixture = {"name": "Owned Fixture A", "pid": 12345, "bundle_id": "dev.metis.owned.a", "nonce": "owned-nonce"}
    app = {**{key: fixture[key] for key in ("name", "pid", "bundle_id")}, "instance_id": "kernel-start-42"}
    marker = "metis-cross-a-test"
    state = {"schema_version": 1, **fixture, "input": marker, "counter": 1, "ticks": 15, "active": True}
    rows = []
    index = 0
    def call(name, args, result):
        nonlocal index
        index += 1
        common = {"session_id": "owned-session", "trace_invocation_id": "root-invocation", "trace_call_id": "call-" + str(index),
                  "tool_use_id": "provider-use-" + str(index), "tool_name": PREFIX + name}
        rows.append({**common, "kind": "tool_args", "text": json.dumps(args)})
        rows.append({**common, "kind": "tool_result", "parent_id": common["tool_use_id"], "text": json.dumps(result)})
    snapshot_index = 0
    def snapshot(value="fixture initial value", count=0):
        nonlocal snapshot_index
        snapshot_index += 1
        sid = "snapshot-" + str(snapshot_index)
        field, button = "field-" + str(snapshot_index), "button-" + str(snapshot_index)
        call("native_ax_snapshot", {"max_nodes": 100, "max_depth": 8}, {"app": app, "snapshot_id": sid, "window_ref": "owned-window",
            "expires_at": "2099-01-01T00:00:00Z", "truncated": False, "nodes": [
                {"name": "Fixture plain input", "role": "AXTextField", "element_ref": field, "value": value,
                 "enabled": True, "editable": True, "bounds": {"Min": {"X": 100, "Y": 100}, "Max": {"X": 300, "Y": 150}}},
                {"name": "Increment fixture counter", "element_ref": button, "enabled": True, "actions": ["AXPress"]},
                {"name": "Fixture count " + str(count), "enabled": True, "element_ref": "count"}]})
        return sid, field, button
    call("activate_application", {"pid": fixture["pid"], "bundle_id": fixture["bundle_id"], "timeout_ms": 1500},
         {"app": app, "dispatch": "sent", "verification": "passed", "reason": "frontmost_identity_matched"})
    call("screen_size", {}, GEOMETRY)
    sid, field, _ = snapshot()
    if keyboard_mouse:
        call("left_click", {"x": 200, "y": 120, "return_screenshot": False}, {"dispatched": True})
        snapshot()
        call("key", {"combo": "cmd+a", "return_screenshot": False}, {"dispatched": True})
        snapshot()
        call("type", {"text": marker, "return_screenshot": False}, {"dispatched": True})
    else:
        call("native_ax_set_value", {"snapshot_id": sid, "element_ref": field, "value": marker},
             {"dispatched": True, "verified": True, "reason": "value_readback_matched"})
    sid, _, button = snapshot(marker)
    call("native_ax_press", {"snapshot_id": sid, "element_ref": button},
         {"dispatched": True, "verified": False, "reason": "native_press_dispatched"})
    snapshot(marker, 1)
    return fixture, marker, rows, state


def persisted_fixture(rows):
    records = [{"type": "header", "header": {"id": "owned-session"}}]
    for row in rows:
        block = {"tool_use_id": row["tool_use_id"], "trace_call_id": row["trace_call_id"]}
        if row["kind"] == "tool_args":
            block.update({"type": "tool_use", "name": row["tool_name"], "input": json.loads(row["text"])})
            role = "assistant"
        else:
            block.update({"type": "tool_result", "content": row["text"]})
            role = "user"
        records.append({"type": "message", "message": {"role": role, "content": [block]}})
    return records


class CrossAppOracleTests(unittest.TestCase):
    def test_command_a_diagnostics_are_bounded_metadata_without_arbitrary_keys_or_text(self):
        state = {"command_a_events": [{"sequence": index, "keycode": 0, "modifier_flags": 1 << 20,
            "selection_length_before": 0, "selection_length_after": 21, "uptime": 100 + index,
            "characters": "do-not-copy", "text": "private arbitrary keyboard text"} for index in range(12)]}
        report = fixture_command_a_diagnostics(state)
        self.assertEqual(len(report["events"]), 8)
        self.assertEqual(report["events"][0]["sequence"], 4)
        self.assertEqual(report["events"][0]["selection_length_after"], 21)
        self.assertTrue(report["local_to_owned_fixture_only"])
        self.assertFalse(report["event_modified_or_consumed"])
        self.assertNotIn("do-not-copy", json.dumps(report))
        self.assertNotIn("private arbitrary", json.dumps(report))
        state["command_a_events"] = [{"keycode": 12, "modifier_flags": 1 << 20}, {"keycode": 0, "modifier_flags": 0}]
        self.assertEqual(fixture_command_a_diagnostics(state)["events"], [])

    def test_focus_diagnostics_classify_owned_and_other_processes_without_content(self):
        fixture, _, _, state = oracle_fixture()
        state["focus_events"] = [
            {"event": "fixture_activated", "uptime": 42.0, "fixture_active": True,
             "foreground_pid": fixture["pid"], "foreground_bundle_id": fixture["bundle_id"]},
            {"event": "workspace_activation", "uptime": 43.0, "fixture_active": False,
             "foreground_pid": 56789, "foreground_bundle_id": "dev.other.application",
             "activated_pid": 56789, "activated_bundle_id": "dev.other.application",
             "name": "do not record user app name", "window_title": "private window content"}]
        report = fixture_focus_diagnostics(state, [fixture])
        self.assertEqual(report["events"][0]["foreground_classification"], "owned_fixture")
        self.assertEqual(report["events"][1]["foreground_classification"], "nonowned_process")
        self.assertEqual(report["events"][1]["activated_classification"], "nonowned_process")
        self.assertFalse(report["reactivation_performed"])
        self.assertNotIn("private window", json.dumps(report))
        self.assertNotIn("user app name", json.dumps(report))

    def test_focus_diagnostics_are_bounded_and_ignore_invalid_event_payloads(self):
        fixture, _, _, state = oracle_fixture()
        state["focus_events"] = [{"event": "workspace_activation", "uptime": index,
            "foreground_pid": 56789, "foreground_bundle_id": "dev.other.application"} for index in range(100)]
        state["focus_events_dropped"] = 5
        report = fixture_focus_diagnostics(state, [fixture])
        self.assertEqual(len(report["events"]), 64)
        self.assertEqual(report["events"][0]["uptime"], 36)
        self.assertEqual(report["earlier_events_dropped"], 5)
        state["focus_events"] = [{"event": "private arbitrary payload", "uptime": 0},
            {"event": "workspace_activation", "uptime": float("nan")}]
        report = fixture_focus_diagnostics(state, [fixture])
        self.assertEqual(report["events"], [])
        self.assertEqual(report["invalid_events_omitted"], 2)

    def test_effective_session_metadata_matches_actual_explicit_configuration(self):
        effective = effective_private_session_parameters('[session]\nmax_iterations = 24\ndir = "/owned/sessions"\n')
        self.assertEqual(effective, {"max_iterations": 24, "auto_compact_threshold_overridden": False})
        self.assertNotIn("auto_compact_threshold", effective)
        explicit = effective_private_session_parameters('[session]\nmax_iterations = 24\nauto_compact_threshold = 0.75\n')
        self.assertEqual(explicit, {"max_iterations": 24, "auto_compact_threshold": 0.75, "auto_compact_threshold_overridden": True})

    def test_full_persisted_snapshot_recovers_bounded_trace_without_relaxing_oracle(self):
        fixture, marker, rows, state = oracle_fixture()
        snapshot = next(row for row in rows if row["kind"] == "tool_result" and row["tool_name"] == PREFIX + "native_ax_snapshot")
        full = {**json.loads(snapshot["text"]), "fixture_padding": "x" * 2500}
        snapshot["text"] = json.dumps(full)
        records = persisted_fixture(rows)
        shortened = deepcopy(rows)
        next(row for row in shortened if row["kind"] == "tool_result" and row["tool_name"] == PREFIX + "native_ax_snapshot")["text"] = snapshot["text"][:2000] + "...(truncated)"
        with self.assertRaises(json.JSONDecodeError):
            validate_query(shortened, fixture, marker, state)
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "owned-session.jsonl"
            path.write_text("\n".join(json.dumps(record) for record in records) + "\n")
            hydrated, evidence = hydrate_native_results(shortened, {"owned-session": path})
            validate_query(hydrated, fixture, marker, state)
            self.assertEqual(evidence[0]["full_transcript_sha256"], digest(path))
            self.assertEqual(evidence[0]["hydrated_native_results"], 7)
            self.assertEqual(shortened[5]["text"], snapshot["text"][:2000] + "...(truncated)")

    def test_full_transcript_pairing_rejects_wrong_ids_tools_args_and_missing_results(self):
        _, _, rows, _ = oracle_fixture()
        original = persisted_fixture(rows)
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "owned-session.jsonl"
            for kind in ("wrong_id", "wrong_tool", "wrong_args", "duplicate_call", "duplicate_result", "missing_result", "orphan", "wrong_occurrence", "wrong_session"):
                records = deepcopy(original)
                if kind == "wrong_id":
                    records[1]["message"]["content"][0]["tool_use_id"] = "another-use"
                elif kind == "wrong_tool":
                    records[1]["message"]["content"][0]["name"] = PREFIX + "another_tool"
                elif kind == "wrong_args":
                    records[1]["message"]["content"][0]["input"]["pid"] = 99
                elif kind == "duplicate_call":
                    records.insert(2, deepcopy(records[1]))
                elif kind == "duplicate_result":
                    records.append(deepcopy(records[-1]))
                elif kind == "missing_result":
                    records.pop()
                elif kind == "orphan":
                    records[1], records[2] = records[2], records[1]
                elif kind == "wrong_occurrence":
                    records[2]["message"]["content"][0]["trace_call_id"] = "another-occurrence"
                else:
                    records[0]["header"]["id"] = "wrong-session"
                path.write_text("\n".join(json.dumps(record) for record in records) + "\n")
                with self.subTest(kind=kind), self.assertRaises(AssertionError):
                    hydrate_native_results(rows, {"owned-session": path})

    def test_full_transcript_binding_rejects_trace_id_parent_preview_and_order_changes(self):
        _, _, rows, _ = oracle_fixture()
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "owned-session.jsonl"
            path.write_text("\n".join(json.dumps(record) for record in persisted_fixture(rows)) + "\n")
            for kind in ("id", "parent", "preview", "order", "duplicate"):
                bad = deepcopy(rows)
                if kind == "id":
                    bad[1]["tool_use_id"] = "different-provider-id"
                elif kind == "parent":
                    bad[1]["parent_id"] = "different-provider-id"
                elif kind == "preview":
                    bad[1]["text"] = "forged ...(truncated)"
                elif kind == "order":
                    bad[:4] = bad[2:4] + bad[:2]
                else:
                    bad.append(deepcopy(bad[-1]))
                with self.subTest(kind=kind), self.assertRaises(AssertionError):
                    hydrate_native_results(bad, {"owned-session": path})

    def test_real_effect_oracle_accepts_complete_ax_shape(self):
        fixture, marker, rows, state = oracle_fixture()
        proof = validate_query(rows, fixture, marker, state)
        self.assertEqual(proof["successful_native_calls"]["native_ax_set_value"], 1)
        self.assertEqual(proof["distinct_snapshots"], 3)
        self.assertFalse(proof["screenshots_taken"])

    def test_keyboard_mouse_requires_bounded_click_and_fresh_snapshots(self):
        fixture, marker, rows, state = oracle_fixture(True)
        proof = validate_query(rows, fixture, marker, state, keyboard_mouse=True)
        self.assertEqual(proof["successful_native_calls"]["type"], 1)
        bad = deepcopy(rows)
        row = next(row for row in bad if row["tool_name"] == PREFIX + "left_click" and row["kind"] == "tool_args")
        row["text"] = json.dumps({"x": 20, "y": 20, "return_screenshot": False})
        with self.assertRaisesRegex(AssertionError, "inside"):
            validate_query(bad, fixture, marker, state, keyboard_mouse=True)

    def test_prose_pass_is_not_native_execution(self):
        fixture, marker, _, state = oracle_fixture()
        with self.assertRaises(AssertionError):
            validate_query([{"kind": "assistant", "text": "PASS all apps were updated"}], fixture, marker, state)

    def test_host_state_rejects_wrong_pid_nonce_counter_and_input(self):
        fixture, marker, rows, state = oracle_fixture()
        for field, value in (("pid", 12346), ("nonce", "other"), ("input", "wrong"), ("counter", 2), ("active", False)):
            with self.subTest(field=field), self.assertRaises(AssertionError):
                validate_query(rows, fixture, marker, {**state, field: value})

    def test_identity_rejects_pid_bundle_name_and_kernel_instance_changes(self):
        fixture, _, _, _ = oracle_fixture()
        app = {**{key: fixture[key] for key in ("pid", "name", "bundle_id")}, "instance_id": "known"}
        for field, value in (("pid", 0), ("bundle_id", "other"), ("name", "other"), ("instance_id", "new")):
            with self.subTest(field=field), self.assertRaises(AssertionError):
                identity({**app, field: value}, fixture, "known")

    def test_orphan_result_and_duplicate_results_are_rejected(self):
        fixture, marker, rows, state = oracle_fixture()
        for bad in (rows[1:], [*rows, rows[-1]]):
            with self.assertRaisesRegex(AssertionError, "Orphan|[Dd]uplicate"):
                validate_query(bad, fixture, marker, state)

    def test_stale_snapshot_and_unobserved_ref_are_rejected(self):
        fixture, marker, rows, state = oracle_fixture()
        for key, value in (("snapshot_id", "expired"), ("element_ref", "unobserved")):
            bad = deepcopy(rows)
            row = next(row for row in bad if row["tool_name"] == PREFIX + "native_ax_set_value" and row["kind"] == "tool_args")
            args = json.loads(row["text"])
            row["text"] = json.dumps({**args, key: value})
            with self.assertRaises(AssertionError):
                validate_query(bad, fixture, marker, state)

    def test_unknown_focus_and_native_failures_are_rejected(self):
        fixture, marker, rows, state = oracle_fixture()
        for kind in ("unknown", "is_error"):
            bad = deepcopy(rows)
            if kind == "unknown":
                result = json.loads(bad[1]["text"])
                bad[1]["text"] = json.dumps({**result, "verification": "unknown"})
            else:
                bad[-1]["is_error"] = True
            with self.assertRaises(AssertionError):
                validate_query(bad, fixture, marker, state)

    def test_other_tools_and_screenshots_are_rejected(self):
        fixture, marker, rows, state = oracle_fixture(True)
        bad = deepcopy(rows)
        bad.append({"kind": "tool_result", "tool_name": "Bash", "text": "PASS"})
        with self.assertRaisesRegex(AssertionError, "allowlist"):
            validate_query(bad, fixture, marker, state, keyboard_mouse=True)
        bad = deepcopy(rows)
        row = next(row for row in bad if row["tool_name"] == PREFIX + "type" and row["kind"] == "tool_args")
        row["text"] = json.dumps({"text": marker, "return_screenshot": True})
        with self.assertRaises(AssertionError):
            validate_query(bad, fixture, marker, state, keyboard_mouse=True)

    def test_missing_permissions_are_blocked_without_requesting_tcc(self):
        description = {"name": "metis-cu", "protocolVersion": 1, "platform": "darwin", "permissions": {"accessibility": "granted", "screenRecording": "notGranted"}}
        permission_preflight(description)  # No screenshots, so screen access is unnecessary.
        for state in (None, "notGranted", "runtime", "unknown"):
            with self.subTest(state=state), self.assertRaises(PreflightBlocked):
                permission_preflight({**description, "permissions": {"accessibility": state}})

    def test_screen_geometry_requires_matching_active_bounds_and_logical_units(self):
        self.assertFalse(display_geometry(GEOMETRY)["multi_monitor_workflow_tested"])
        for bad in ({}, {**GEOMETRY, "display_origin_x": 1}, {**GEOMETRY, "display_width_px": 2880},
                    {**GEOMETRY, "active": 1}, {**GEOMETRY, "coord_space_note": "image pixels"}):
            with self.assertRaises(AssertionError):
                display_geometry(bad)

    def test_help_and_missing_live_optin_do_not_start_acceptance(self):
        path = Path(__file__).with_name("computer_use_cross_app_acceptance.py")
        result = subprocess.run([sys.executable, "-B", str(path), "--help"], capture_output=True, text=True, timeout=10)
        self.assertEqual(result.returncode, 0)
        self.assertIn("--run-live", result.stdout)
        result = subprocess.run([sys.executable, "-B", str(path)], capture_output=True, text=True, timeout=10)
        self.assertEqual(result.returncode, 2)

    def test_only_two_fixtures_have_input_even_for_builtin_terminal_defaults(self):
        source = 'var defaultTiers = map[string]AccessTier{\n "Terminal": TierClick,\n "Safari": TierRead,\n}\n'
        grants = fixture_input_grants(source, [{"name": "Owned A"}, {"name": "Owned B"}])
        self.assertEqual(grants, {"Terminal": "read", "Safari": "read", "Owned A": "full", "Owned B": "full"})
        with self.assertRaises(AssertionError):
            fixture_input_grants("unrecognized source", [{"name": "Owned A"}, {"name": "Owned B"}])

    def test_native_inspection_failure_still_destroys_inputs_and_runs_cli_auth_cleanup(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            checker = object.__new__(CrossAppCheck)
            checker.root, checker.processes, checker.report = root, [], {}
            checker.helper_copy, checker.helper_source = root / "copy", root / "input"
            checker.helper_copy.write_text("private helper")
            checker.helper_source.write_text("caller helper")
            checker.args = SimpleNamespace(helper_sha256=digest(checker.helper_source))
            checker.redact = lambda text: text
            with patch.object(checker, "stop_native_descendants", side_effect=OSError("native inspection unavailable")), \
                 patch.object(PersistentAgentLiveCheck, "cleanup") as parent_cleanup:
                with self.assertRaisesRegex(AssertionError, "native inspection"):
                    checker.cleanup()
                parent_cleanup.assert_called_once()
            self.assertFalse(checker.helper_copy.exists())
            self.assertFalse(checker.helper_source.exists())

    def test_failed_round_captures_only_owned_state_before_cleanup_with_safe_errors(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory).resolve()
            checker = object.__new__(CrossAppCheck)
            checker.root, checker.output, checker.report = root, root, {"passed": False}
            fixture, marker, _, state = oracle_fixture()
            path = root / "fixture-state.json"
            path.write_text(json.dumps({**state, "unused_secret_field": "do-not-copy"}))
            checker.fixtures = [{**fixture, "state_path": path},
                {**fixture, "state_path": root / "missing.json"}]
            checker.redact = lambda text: text.replace("redact-this-secret", "[REDACTED]")
            checker.capture_failed_fixture_states()
            evidence = json.loads((root / "failed-fixture-states-before-cleanup.json").read_text())
            self.assertTrue(evidence["before_fixture_cleanup"])
            self.assertFalse(evidence["screenshots_taken"])
            self.assertTrue(evidence["fixtures"][0]["identity_matches_owned_fixture"])
            self.assertEqual(evidence["fixtures"][0]["state"]["input"], marker)
            self.assertEqual(evidence["fixtures"][0]["state"]["counter"], 1)
            self.assertNotIn("do-not-copy", json.dumps(evidence))
            self.assertFalse(evidence["fixtures"][1]["available"])
            self.assertEqual(evidence["fixtures"][1]["error"], "Owned fixture state unavailable or invalid")
            # Malformed bytes must not be quoted into an error or evidence.
            path.write_text('redact-this-secret malformed fixture input')
            checker.capture_failed_fixture_states()
            evidence = json.loads((root / "failed-fixture-states-before-cleanup.json").read_text())
            self.assertFalse(evidence["fixtures"][0]["available"])
            self.assertEqual(evidence["fixtures"][0]["failure_type"], "JSONDecodeError")
            self.assertNotIn("malformed fixture input", json.dumps(evidence))
            self.assertNotIn("redact-this-secret", json.dumps(evidence))

    def test_failed_fixture_diagnostics_do_not_follow_symlinks(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory).resolve()
            checker = object.__new__(CrossAppCheck)
            checker.root, checker.output, checker.report = root, root, {"passed": False}
            fixture, _, _, state = oracle_fixture()
            target = root / "actual-state.json"
            target.write_text(json.dumps(state))
            link = root / "fixture-state.json"
            link.symlink_to(target)
            checker.fixtures, checker.redact = [{**fixture, "state_path": link}], lambda text: text
            checker.capture_failed_fixture_states()
            evidence = checker.report["failed_fixture_states_before_cleanup"]
            self.assertFalse(evidence["fixtures"][0]["available"])
            self.assertNotIn("state", evidence["fixtures"][0])


if __name__ == "__main__":
    unittest.main()
