"""Offline host-oracle tests; no helper, credentials, compiler, network or UI."""
import copy
import json
from pathlib import Path
import struct
import tempfile
import time
import tomllib
import unittest
import zlib

from official_computer_use_live_check import (INITIAL, expected_steps, official_geometry, parse_png, read_state,
    OfficialToolError, configured_permission_mode, fixture_query_prompt, helper_reply_diagnostic, proxy_mcp_config, result_ok,
    screen_size_arguments, screenshot_oracle, state_identity, stopped_reply, validate_plan_call)


def fixture():
    return {"pid": 1234, "bundle_id": "dev.metis.owned", "name": "Owned Fixture", "nonce": "a51020203040",
            "app_path": "/owned/fixture.app", "state_path": "/owned/state.json"}


def state():
    f = fixture()
    return {**{k: f[k] for k in ("pid", "bundle_id", "name", "nonce")}, "schema_version": 1, "ticks": 2,
            "active": True, "foreground_pid": f["pid"], "foreground_bundle_id": f["bundle_id"],
            "opaque_full_display": True, "screen": {"x": 0, "y": 0, "width": 1000, "height": 700},
            "input_point": {"x": 450, "y": 187}, "button_point": {"x": 265, "y": 286},
            "input": INITIAL, "counter": 0, "selection_length": 0, "focus_serial": 2}


def png(width=800, height=560, bits=0xa5, bad=False):
    def chunk(kind, data):
        return struct.pack(">I", len(data)) + kind + data + struct.pack(">I", zlib.crc32(kind + data) & 0xffffffff)
    row = bytearray([24, 32, 48] * width)
    for i in range(8):
        x = int(width * (0.1375 + i * 0.1))
        rgb = [25, 200, 85] if bits & (1 << i) else [220, 35, 50]
        if bad: rgb = [0, 0, 0]
        row[x * 3:x * 3 + 3] = bytes(rgb)
    raw = (b"\0" + row) * height
    return b"\x89PNG\r\n\x1a\n" + chunk(b"IHDR", struct.pack(">IIBBBBB", width, height, 8, 2, 0, 0, 0)) + chunk(b"IDAT", zlib.compress(raw)) + chunk(b"IEND", b"")


class OfficialOracles(unittest.TestCase):
    def test_private_mode_matches_existing_desktop_full_access(self):
        original = {"permission": {"mode": "fullAccess"}}
        self.assertEqual(configured_permission_mode(original), "fullAccess")
        self.assertEqual(original, {"permission": {"mode": "fullAccess"}})
    def test_private_mode_never_elevates_missing_or_restricted_config(self):
        for original in [{}, {"permission": {}}, {"permission": {"mode": "bypassPermissions"}},
                         {"permission": {"mode": "dontAsk"}}, {"permission": {"mode": "default"}}]:
            with self.assertRaises(AssertionError): configured_permission_mode(original)
    def test_private_mode_cannot_select_a_different_posture(self):
        with self.assertRaises(AssertionError): configured_permission_mode({"permission": {"mode": "fullAccess"}}, "bypassPermissions")
    def test_exact_owned_state(self): state_identity(state(), fixture())
    def test_wrong_pid(self):
        s = state(); s["pid"] += 1
        with self.assertRaises(AssertionError): state_identity(s, fixture())
    def test_wrong_bundle(self):
        s = state(); s["bundle_id"] = "foreign"
        with self.assertRaises(AssertionError): state_identity(s, fixture())
    def test_focus_loss(self):
        s = state(); s["active"] = False
        with self.assertRaises(AssertionError): state_identity(s, fixture())
    def test_active_bit_without_foreground_identity_rejected(self):
        s = state(); s["foreground_pid"] = 9999
        with self.assertRaises(AssertionError): state_identity(s, fixture())
    def test_secondary_display_rejected(self):
        s = state(); s["screen"]["x"] = 1000
        with self.assertRaises(AssertionError): state_identity(s, fixture())
    def test_exact_official_primary_geometry(self):
        self.assertEqual(official_geometry({"active": 0, "displays": [{"index": 0, **state()["screen"]}]}, state())["width"], 1000)
    def test_wrong_helper_geometry_rejected_before_mouse(self):
        for value in [{"active": 1, "displays": [{"index": 0, **state()["screen"]}]},
                      {"active": 0, "displays": [{"index": 0, **state()["screen"], "width": 999}]}]:
            with self.assertRaises(AssertionError): official_geometry(value, state())
    def test_invalid_coordinate_rejected(self):
        for point in [{"x": -10, "y": 20}, {"x": True, "y": 20}, {"x": 50, "y": 9999}]:
            s = state(); s["input_point"] = point
            with self.assertRaises(AssertionError): state_identity(s, fixture())
    def test_opaque_screen_required(self):
        s = state(); s["opaque_full_display"] = False
        with self.assertRaises(AssertionError): state_identity(s, fixture())
    def test_fresh_file(self):
        with tempfile.TemporaryDirectory() as d:
            f = fixture(); f["state_path"] = str(Path(d) / "state.json")
            Path(f["state_path"]).write_text(json.dumps(state()))
            self.assertEqual(read_state(f)["pid"], f["pid"])
    def test_stale_file_rejected(self):
        import os
        with tempfile.TemporaryDirectory() as d:
            f = fixture(); f["state_path"] = str(Path(d) / "state.json")
            Path(f["state_path"]).write_text(json.dumps(state()))
            os.utime(f["state_path"], (time.time() - 3, time.time() - 3))
            with self.assertRaises(AssertionError): read_state(f)
    def test_exact_six_step_plan(self):
        plan = expected_steps(fixture(), state(), "metis-marker")
        self.assertEqual(len(plan), 6)
        for i, step in enumerate(plan): validate_plan_call(i, step["name"], step["arguments"], plan)
        self.assertFalse(any(step["name"] == "screenshot" for step in plan))
    def test_published_screen_size_requires_nonempty_dummy_argument(self):
        plan = expected_steps(fixture(), state(), "metis-marker")
        self.assertEqual(plan[1]["arguments"], {"_": "noop"})
        arguments = screen_size_arguments(); arguments["_"] = "changed"
        self.assertEqual(screen_size_arguments(), {"_": "noop"})
    def test_prompt_supports_runtime_without_lazy_tool_search(self):
        prompt = fixture_query_prompt(expected_steps(fixture(), state(), "metis-marker"))
        self.assertIn("If ToolSearch is available and a schema is deferred", prompt)
        self.assertIn("otherwise invoke the already exposed computer-use tools directly", prompt)
        self.assertIn("never retry an input", prompt)
    def test_original_helper_error_is_preserved_and_forwarded_exactly(self):
        reply = {"jsonrpc": "2.0", "id": 7, "result": {"isError": True, "content": [
            {"type": "text", "text": "open_application: actual LaunchServices error 123"}]}}
        with self.assertRaisesRegex(OfficialToolError, "actual LaunchServices error 123") as error: result_ok(reply)
        self.assertIs(stopped_reply(7, error.exception, reply), reply)
        self.assertIn("actual LaunchServices error 123", helper_reply_diagnostic(reply)["text"])
    def test_original_jsonrpc_error_is_preserved(self):
        reply = {"jsonrpc": "2.0", "id": 7, "error": {"code": -32000, "message": "actual transport error"}}
        with self.assertRaisesRegex(OfficialToolError, "actual transport error") as error: result_ok(reply)
        self.assertIs(stopped_reply(7, error.exception, reply), reply)
        self.assertEqual(helper_reply_diagnostic(reply)["jsonrpc_error_code"], -32000)
    def test_image_error_is_not_forwarded_or_included_in_diagnostics(self):
        reply = {"jsonrpc": "2.0", "id": 7, "result": {"isError": True, "content": [
            {"type": "image", "data": "PRIVATE_IMAGE_BASE64"}, {"type": "text", "text": "actual error"}]}}
        evidence = helper_reply_diagnostic(reply)
        self.assertNotIn("PRIVATE_IMAGE_BASE64", json.dumps(evidence))
        self.assertEqual(evidence["nontext_block_types"], ["image"])
        with self.assertRaises(OfficialToolError) as error: result_ok(reply)
        self.assertNotIn("PRIVATE_IMAGE_BASE64", json.dumps(stopped_reply(7, error.exception, reply)))
    def test_fence_failure_never_forwards_prior_success(self):
        reply = {"jsonrpc": "2.0", "id": 7, "result": {"content": [{"type": "text", "text": "opened"}]}}
        self.assertTrue(stopped_reply(7, AssertionError("focus changed"), reply)["result"]["isError"])
    def test_helper_error_diagnostics_are_bounded(self):
        reply = {"result": {"isError": True, "content": [{"type": "text", "text": "x" * 40000}]}}
        evidence = helper_reply_diagnostic(reply)
        self.assertEqual(len(evidence["text"]), 8192)
        self.assertTrue(evidence["text_truncated"])
    def test_private_proxy_evidence_is_in_explicit_generic_sandbox_cwd(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory).resolve()
            policy = root / "policy-A.json"
            entry = tomllib.loads(proxy_mcp_config(policy, root))["servers"][0]
            self.assertEqual(entry["working_dir"], str(root))
            self.assertEqual(Path(entry["args"][-1]).parent, root)
            self.assertNotEqual(entry["command"], "@metis/computer-use")
            self.assertNotIn("env", entry)
            self.assertFalse(entry["disabled"])
    def test_proxy_cwd_cannot_be_root_or_unrelated_to_policy(self):
        for policy, cwd in [(Path("/policy.json"), Path("/")),
                            (Path("/owned/policy.json"), Path("/unrelated"))]:
            with self.assertRaises(AssertionError): proxy_mcp_config(policy, cwd)
    def test_duplicate_input_rejected(self):
        plan = expected_steps(fixture(), state(), "metis-marker")
        with self.assertRaises(AssertionError): validate_plan_call(3, plan[2]["name"], plan[2]["arguments"], plan)
    def test_seventh_call_rejected(self):
        plan = expected_steps(fixture(), state(), "metis-marker")
        with self.assertRaises(AssertionError): validate_plan_call(6, "left_click", plan[5]["arguments"], plan)
    def test_wrong_app_rejected(self):
        plan = expected_steps(fixture(), state(), "metis-marker")
        with self.assertRaises(AssertionError): validate_plan_call(0, "open_application", {"name": "/Applications/Mail.app"}, plan)
    def test_unexpected_screenshot_rejected(self):
        plan = expected_steps(fixture(), state(), "metis-marker")
        with self.assertRaises(AssertionError): validate_plan_call(1, "screenshot", {}, plan)
    def test_screenshot_flag_cannot_be_changed(self):
        plan = expected_steps(fixture(), state(), "metis-marker")
        args = {**plan[2]["arguments"], "return_screenshot": True}
        with self.assertRaises(AssertionError): validate_plan_call(2, "left_click", args, plan)
    def test_no_clipboard_typing(self):
        for marker in ["x" * 80, "中文", ""]:
            with self.assertRaises(AssertionError): expected_steps(fixture(), state(), marker)
    def test_real_png_nonce_and_geometry(self):
        proof = screenshot_oracle(png(), fixture(), state()["screen"])
        self.assertTrue(proof["visual_nonce_verified"])
        self.assertFalse(proof["image_sent_to_model"])
        self.assertEqual((proof["width"], proof["height"]), (800, 560))
    def test_foreign_visual_nonce_rejected(self):
        with self.assertRaises(AssertionError): screenshot_oracle(png(bits=0x5a), fixture(), state()["screen"])
    def test_blank_screen_rejected(self):
        with self.assertRaises(AssertionError): screenshot_oracle(png(bad=True), fixture(), state()["screen"])
    def test_corrupt_png_rejected(self):
        data = bytearray(png()); data[45] ^= 1
        with self.assertRaises(AssertionError): parse_png(bytes(data))
    def test_wrong_aspect_rejected(self):
        with self.assertRaises(AssertionError): screenshot_oracle(png(), fixture(), {"width": 1920, "height": 1080})
    def test_missing_png_end_rejected(self):
        with self.assertRaises(AssertionError): parse_png(png()[:-12])


if __name__ == "__main__": unittest.main()
