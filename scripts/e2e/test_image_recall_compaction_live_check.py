"""Offline fixture contracts; never read user credentials or call a model."""

import argparse
import contextlib
import copy
import hashlib
import io
import json
from pathlib import Path
import random
import struct
import tempfile
import unittest
from unittest.mock import patch
import zlib

import image_recall_compaction_live_check as check


class SyntheticVisualTests(unittest.TestCase):
    def test_three_letters_footer_cells_and_base64_pressure(self) -> None:
        images = [check.synthetic_png(letter, footer, random_bytes=random.Random(index).randbytes)
            for index, (letter, footer) in enumerate(zip("ABC", ("ABABAB", "BAABAB", "ABBABA")))]
        self.assertEqual(len({hashlib.sha256(body).hexdigest() for body in images}), 3)
        self.assertGreater(sum(len(body) for body in images), 783700)
        for image, letter, footer in zip(images, "ABC", ("ABABAB", "BAABAB", "ABBABA")):
            self.assertEqual(struct.unpack(">II", image[16:24]), (600, 640))
            offset, compressed = 8, bytearray()
            while offset < len(image):
                size = struct.unpack(">I", image[offset:offset + 4])[0]
                kind, payload = image[offset + 4:offset + 8], image[offset + 8:offset + 8 + size]
                if kind == b"IDAT":
                    compressed.extend(payload)
                offset += size + 12
            rows = zlib.decompress(bytes(compressed))
            self.assertEqual(len(rows), 640 * 1801)
            for gy, line in enumerate(check.ALL_GLYPHS[letter]):
                for gx, bit in enumerate(line):
                    position = (24 + gy * 24 + 12) * 1801 + 1 + (240 + gx * 24 + 12) * 3
                    self.assertEqual(rows[position:position + 3], (b"\0" if bit == "1" else b"\xff") * 3)
            for index, character in enumerate(footer):
                for gy, line in enumerate(check.ALL_GLYPHS[character]):
                    for gx, bit in enumerate(line):
                        position = (224 + gy * 10 + 5) * 1801 + 1 + (120 + index * 62 + gx * 10 + 5) * 3
                        self.assertEqual(rows[position:position + 3], (b"\0" if bit == "1" else b"\xff") * 3)


class NativeRecallTests(unittest.TestCase):
    def setUp(self) -> None:
        temporary = tempfile.TemporaryDirectory(prefix="metis-image-recall-unit-")
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name)
        self.body = b"synthetic-native-png"
        self.sha = hashlib.sha256(self.body).hexdigest()
        (self.root / self.sha).write_bytes(self.body)
        self.image = {"type": "image", "media_type": "image/png", "attachment_hash": self.sha}
        self.result = {"type": "tool_result", "trace_call_id": "actual-call", "tool_result_blocks": [self.image]}

    def test_exact_call_must_carry_correct_native_image_blob(self) -> None:
        proof = check.native_result_proof([{ "type": "message", "message": {"role": "user", "content": [self.result]}}], self.root, {"actual-call": self.sha})
        self.assertEqual(proof[0]["native_image_sha256"], self.sha)
        with self.assertRaises(AssertionError):
            check.native_result_proof([self.result], self.root, {"different-call": self.sha})
        (self.root / self.sha).write_bytes(b"corrupt")
        with self.assertRaises(AssertionError):
            check.native_result_proof([self.result], self.root, {"actual-call": self.sha})

    def checkpoint_rows(self) -> list[dict]:
        return [{"type": "message", "message": {"role": "user", "content": [self.image]}},
            {"type": "history_replace", "messages": [{"role": "assistant", "content": [{"type": "text", "synthetic": True,
                "text": "[Earlier conversation summarized: attachment_id=" + self.sha + " lower code ABABBA]"}]}]}]

    def test_withheld_image_must_leave_active_history_and_keep_id_and_visual_footer(self) -> None:
        rows = self.checkpoint_rows()
        proof = check.checkpoint_recall_proof(rows, self.root, self.sha, "ABABBA")
        self.assertEqual(proof["remaining_active_native_image_sha256_counts"], {})
        # Old physical ledger image is present, but active replacement contains none.
        self.assertTrue(proof["previously_untranscribed_visual_footer_verified"])
        for change in ("tail", "no-id", "no-vision"):
            broken = copy.deepcopy(rows)
            if change == "tail":
                broken[-1]["messages"].append({"role": "user", "content": [self.image]})
            elif change == "no-id":
                broken[-1]["messages"][0]["content"][0]["text"] = "[Earlier conversation summarized: ABABBA]"
            else:
                broken[-1]["messages"][0]["content"][0]["text"] = "[Earlier conversation summarized: attachment_id=" + self.sha + " unparsed image]"
            with self.assertRaises(AssertionError):
                check.checkpoint_recall_proof(broken, self.root, self.sha, "ABABBA")

    def test_call_segment_pairs_real_success_and_explicit_negative_categories(self) -> None:
        def events(sha: str, error: bool, text: str) -> list[dict]:
            return [{"kind": "tool_start", "toolName": "ViewImage", "traceCallID": sha, "text": json.dumps({"attachment_id": sha})},
                {"kind": "tool_result", "traceCallID": sha, "isError": error, "text": text}]
        missing = "b" * 64
        proof = check.validate_call_segment(events(self.sha, False, "native image") + events(missing, True, "no such file"), [self.sha], {missing: ("no such file",)})
        self.assertEqual(proof["success_calls"], {self.sha: self.sha})
        self.assertEqual(proof["error_calls"], {missing: missing})
        for bad in (events(self.sha, False, "native") * 2, events(self.sha, True, "error")):
            with self.assertRaises(AssertionError):
                check.validate_call_segment(bad, [self.sha])
        with self.assertRaises(AssertionError):
            check.validate_call_segment(events(missing, True, "different failure"), [], {missing: ("checksum",)})

    def test_http200_marker_is_insufficient_without_normal_root_end_turn(self) -> None:
        normal = [{"kind": "loop_done", "depth": 0, "text": "end_turn"}]
        check.validate_turn_completion({"text": "marker"}, normal)
        for result, events in (({"stopped": True}, normal), ({}, []), ({}, [{"kind": "loop_done", "text": "max_iterations"}]),
                               ({}, [{"kind": "loop_done", "depth": 1, "text": "end_turn"}])):
            with self.assertRaises(AssertionError):
                check.validate_turn_completion(result, events)


class PrivatePreparationTests(unittest.TestCase):
    def test_private_viewimage_palette_and_no_answer_textfile(self) -> None:
        with tempfile.TemporaryDirectory(prefix="metis-image-recall-prepare-") as folder:
            root = Path(folder)
            source = root / "fixture-source"
            (source / "cache").mkdir(parents=True)
            (source / "config.toml").write_text('[provider]\ndefault="easycliproxyapi"\n[provider.custom.easycliproxyapi]\ntransport="openai_chat"\nbase_url="http://127.0.0.1:23456/v1"\napi_key_env="OFFLINE_IMAGE_UNIT_KEY"\nmodel="existing"\n')
            (source / "cache" / "models.json").write_text(json.dumps({"openai": {"models": {"gpt-6.1-sol": {"id": "gpt-6.1-sol", "limit": {"context": 1000000, "input": 900000, "output": 32000}}}}}))
            binary = root / "never-executed-binary"
            binary.write_bytes(b"offline fixture only")
            args = argparse.Namespace(binary=binary, destroy_input=True, expected_sha256=hashlib.sha256(binary.read_bytes()).hexdigest(),
                output=root / "evidence.noindex" / "offline", source_home=source, query_timeout=30, max_output_tokens=8192, hold_seconds=0, stop_file=None)
            with patch.dict("os.environ", {"OFFLINE_IMAGE_UNIT_KEY": "offline-unit-key"}), contextlib.redirect_stdout(io.StringIO()):
                harness = check.ImageRecallCompactionLiveCheck(args)
                try:
                    harness.prepare()
                    private = (harness.home / "config.toml").read_text()
                    self.assertIn('allowed = ["ViewImage"]', private)
                    self.assertNotIn('allowed = ["Read"]', private)
                    self.assertFalse(harness.text_path.exists())
                    self.assertEqual(len(harness.source_paths), 3)
                    self.assertNotIn("offline-unit-key", json.dumps(harness.report))
                    for footer in harness.footers:
                        self.assertNotIn(footer, json.dumps(harness.report))
                finally:
                    harness.cleanup()
            self.assertFalse(binary.exists())
            self.assertFalse(harness.root.exists())


if __name__ == "__main__":
    unittest.main()
