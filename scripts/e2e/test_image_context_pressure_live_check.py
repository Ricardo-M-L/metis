"""Offline contracts only: synthetic fixtures and fake local CLI, no real auth."""

from __future__ import annotations

import argparse
import base64
import contextlib
import hashlib
import io
import json
import os
from pathlib import Path
import random
import struct
import tempfile
import unittest
from unittest.mock import patch
import zlib

import image_context_pressure_live_check as check


class RouteTests(unittest.TestCase):
    def setUp(self) -> None:
        self.temporary = tempfile.TemporaryDirectory(prefix="metis-image-route-unit-")
        self.root = Path(self.temporary.name)
        self.addCleanup(self.temporary.cleanup)

    def fixture(self, extra: str = "", *, transport: str = "openai_chat") -> None:
        (self.root / "config.toml").write_text('[provider]\ndefault = "easycliproxyapi"\n'
            '[provider.custom.easycliproxyapi]\ntransport = "' + transport + '"\nbase_url = "https://route.invalid/v1/"\n'
            'model = "existing-model"\n' + extra)

    def store(self, endpoint: str = "https://route.invalid/v1", transport: str = "openai_chat") -> None:
        (self.root / ".credentials").mkdir(exist_ok=True)
        (self.root / ".credentials" / "auth.json").write_text(json.dumps({"unrelated": {"key": "must-not-select"},
            "easycliproxyapi": {"type": "api", "key": "unit-store-key", "endpoint": {"provider": "easycliproxyapi",
            "transport": transport, "base_url": endpoint}}}))

    def test_env_has_priority_and_no_credential_store_read(self) -> None:
        self.fixture('api_key_env = "UNIT_ONLY_ROUTE_KEY"\napi_key = "unit-inline-key"\n')
        result = check.load_selected_route(self.root, {"UNIT_ONLY_ROUTE_KEY": "unit-env-key"})
        self.assertEqual(result["key"], "unit-env-key")
        self.assertEqual(result["binding"][1], "openai_chat")
        self.assertFalse((self.root / ".credentials").exists())

    def test_selected_inline_key_before_store(self) -> None:
        self.fixture('api_key = "unit-inline-key"\n')
        self.store(endpoint="https://wrong.invalid")
        self.assertEqual(check.load_selected_route(self.root, {})["key"], "unit-inline-key")

    def test_bound_current_chat_store_accepts_normalized_alias(self) -> None:
        self.fixture()
        self.store(endpoint="https://ROUTE.invalid:443/v1", transport="chat")
        self.assertEqual(check.load_selected_route(self.root, {})["key"], "unit-store-key")

    def test_rejects_different_endpoint_transport_and_unbound_store(self) -> None:
        self.fixture()
        for endpoint, transport in (("https://wrong.invalid/v1", "openai_chat"), ("https://route.invalid/v1", "anthropic_messages")):
            self.store(endpoint, transport)
            with self.assertRaisesRegex(AssertionError, "another"):
                check.load_selected_route(self.root, {})
        path = self.root / ".credentials" / "auth.json"
        path.write_text(json.dumps({"easycliproxyapi": {"key": "unit-store-key"}}))
        with self.assertRaisesRegex(AssertionError, "binding"):
            check.load_selected_route(self.root, {})

    def test_rejects_route_redirectors_and_nonloopback_http(self) -> None:
        for endpoint in ("http://route.invalid/v1", "https://user:pw@route.invalid/v1", "https://route.invalid/v1?key=unit", "https://route.invalid/v1#fragment"):
            with self.assertRaises(AssertionError):
                check.normalized_binding("easycliproxyapi", "openai_chat", endpoint)


class FixtureAndHistoryTests(unittest.TestCase):
    def test_synthetic_png_glyph_and_pressure_without_external_library(self) -> None:
        bodies = [check.png_bytes(letter, random_bytes=random.Random(index).randbytes) for index, letter in enumerate(("A", "B"))]
        self.assertNotEqual(hashlib.sha256(bodies[0]).digest(), hashlib.sha256(bodies[1]).digest())
        self.assertGreater(sum(4 * ((len(body) + 2) // 3) for body in bodies) * .75, check.MIN_LEGACY_PRESSURE)
        self.assertLess(sum(map(len, bodies)), 1_200_000)
        for body, letter in zip(bodies, ("A", "B")):
            self.assertTrue(body.startswith(b"\x89PNG\r\n\x1a\n"))
            offset, idat = 8, bytearray()
            while offset < len(body):
                size = struct.unpack(">I", body[offset:offset + 4])[0]
                kind, payload = body[offset + 4:offset + 8], body[offset + 8:offset + 8 + size]
                crc = struct.unpack(">I", body[offset + 8 + size:offset + 12 + size])[0]
                self.assertEqual(crc, zlib.crc32(kind + payload) & 0xffffffff)
                if kind == b"IDAT":
                    idat.extend(payload)
                offset += size + 12
            raw = zlib.decompress(bytes(idat))
            self.assertEqual(len(raw), 600 * 1801)
            # Each large grid cell's center must match the intended black/white glyph.
            for gy, row in enumerate(check.GLYPHS[letter]):
                for gx, bit in enumerate(row):
                    x, y = 220 + gx * 32 + 16, 38 + gy * 32 + 16
                    start = y * 1801 + 1 + x * 3
                    self.assertEqual(raw[start:start + 3], b"\x00" * 3 if bit == "1" else b"\xff" * 3)

    def test_history_replace_cannot_hide_discarded_images(self) -> None:
        image = {"type": "image", "data": base64.b64encode(b"synthetic").decode()}
        rows = [{"type": "message", "message": {"role": "user", "content": [image]}},
                {"type": "history_replace", "messages": []},
                {"type": "message", "message": {"role": "assistant", "content": [{"type": "text", "text": "done"}]}}]
        self.assertEqual(check.collect_image_hashes(check.active_history(rows)), {})

    def test_external_blob_verified_and_counted_without_returning_bytes(self) -> None:
        with tempfile.TemporaryDirectory() as folder:
            root, body = Path(folder), b"synthetic-png-bytes"
            sha = hashlib.sha256(body).hexdigest()
            (root / sha).write_bytes(body)
            value = {"type": "tool_result", "tool_result_blocks": [{"type": "image", "attachment_hash": sha}]}
            self.assertEqual(check.collect_image_hashes(value, root), {sha: 1})
            (root / sha).write_bytes(b"corrupt")
            with self.assertRaisesRegex(AssertionError, "corrupt"):
                check.collect_image_hashes(value, root)


def trace_events(paths: list[Path], session: str) -> list[dict]:
    events = []
    for index, path in enumerate(paths):
        identity = "round-call-" + str(index)
        events.extend([{"kind": "tool_start", "tool_name": "ViewImage", "text": json.dumps({"path": str(path)}), "trace_call_id": identity},
                       {"kind": "tool_result", "tool_name": "ViewImage", "trace_call_id": identity, "is_error": False}])
    events.extend([{"kind": "tokens", "text": "input=200 output=5 cache_write=0 cache_read=12"}, {"kind": "loop_done", "text": "end_turn"}])
    return [{**event, "session_id": session} for event in events]


class TraceTests(unittest.TestCase):
    def setUp(self) -> None:
        self.paths = [Path("/private/synthetic-a.png"), Path("/private/synthetic-b.png")]
        self.events = trace_events(self.paths, "private-session")

    def test_requires_fresh_successful_two_image_calls_and_counts_usage(self) -> None:
        proof = check.trace_summary(self.events, self.paths, "private-session")
        self.assertEqual(proof["usage"]["input"], 200)
        self.assertEqual(proof["auto_compaction_start_count"], 0)
        self.assertEqual(proof["successful_fresh_image_results"], 2)
        with self.assertRaises(AssertionError):
            check.trace_summary(self.events[2:], self.paths, "private-session")

    def test_detects_actual_and_future_auto_compaction_trace_names(self) -> None:
        for event in ({"kind": "compaction_start", "text": "auto"}, {"kind": "auto_compaction_start"}):
            with self.assertRaisesRegex(AssertionError, "automatic compaction"):
                check.trace_summary(self.events + [{**event, "session_id": "private-session"}], self.paths, "private-session")

    def test_rejects_failed_unmatched_and_other_image_calls(self) -> None:
        for change in ("failed", "unmatched", "other"):
            events = json.loads(json.dumps(self.events))
            if change == "failed":
                events[1]["is_error"] = True
            elif change == "unmatched":
                events[1]["trace_call_id"] = "different"
            else:
                events[0]["text"] = json.dumps({"path": "/private/user-image.png"})
            with self.assertRaises(AssertionError):
                check.trace_summary(events, self.paths, "private-session")


FAKE_CLI = r'''#!/usr/bin/env python3
import hashlib,json,os,pathlib,re,sys
args=sys.argv[1:]; home=pathlib.Path(os.environ['METIS_HOME'])
assert args[0]=='run' and '--bare' in args and args[args.index('--tools')+1]=='ViewImage'
assert args[args.index('--effort')+1]=='high' and args[args.index('--model')+1]=='gpt-6.1-sol'
flag='--session-id' if '--session-id' in args else '--resume'; sid=args[args.index(flag)+1]
assert os.environ['METIS_IMAGE_PRESSURE_ROUTE_KEY']=='unit-fake-key'
prompt=args[-1]; paths=[pathlib.Path(p) for p in re.findall(r'/\S+\.png',prompt)]
assert len(paths)==2
(home/'traces').mkdir(exist_ok=True); (home/'sessions').mkdir(exist_ok=True)
trace=home/'traces'/'private.jsonl'; session=home/'sessions'/(sid+'.jsonl')
attachments=pathlib.Path(str(session)+'.attachments'); attachments.mkdir(exist_ok=True)
number=int(re.search(r'Round (\d+)',prompt).group(1)); blocks=[]
with trace.open('a') as stream:
 for index,path in enumerate(paths):
  ident=str(number)+'-'+str(index); data=path.read_bytes(); sha=hashlib.sha256(data).hexdigest()
  (attachments/sha).write_bytes(data); blocks.append({'type':'image','media_type':'image/png','attachment_hash':sha})
  stream.write(json.dumps({'kind':'tool_start','tool_name':'ViewImage','text':json.dumps({'path':str(path)}),'trace_call_id':ident,'session_id':sid})+'\n')
  stream.write(json.dumps({'kind':'tool_result','tool_name':'ViewImage','is_error':False,'trace_call_id':ident,'session_id':sid})+'\n')
 stream.write(json.dumps({'kind':'tokens','text':'input=200 output=5 cache_read=100 cache_write=0','session_id':sid})+'\n')
 stream.write(json.dumps({'kind':'loop_done','text':'end_turn','session_id':sid})+'\n')
with session.open('a') as stream:
 stream.write(json.dumps({'type':'message','message':{'role':'user','content':[{'type':'tool_result','tool_result_blocks':blocks}]}})+'\n')
print('A B '+re.search(r'METIS_IMAGE_ROUND_\w+',prompt).group(0))
'''


class IsolatedHarnessTests(unittest.TestCase):
    def setUp(self) -> None:
        temporary = tempfile.TemporaryDirectory(prefix="metis-image-harness-unit-")
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name)
        self.source = self.root / "fake-source-home"
        (self.source / "cache").mkdir(parents=True)
        (self.source / "config.toml").write_text('[provider]\ndefault = "easycliproxyapi"\n'
            '[provider.custom.easycliproxyapi]\ntransport = "openai_chat"\nbase_url = "https://unit-route.invalid/v1"\n'
            'model = "old-model"\napi_key_env = "UNIT_SELECTED_ROUTE_KEY"\n')
        (self.source / "cache" / "models.json").write_text(json.dumps({"openai": {"models": {
            check.TARGET_MODEL: {"id": check.TARGET_MODEL, "limit": {"context": 1000000, "input": 900000, "output": 10000}}}}}))
        self.binary = self.root / "disposable-fake-cli"
        self.binary.write_text(FAKE_CLI)
        self.binary.chmod(0o700)
        self.output = self.root / "evidence.noindex" / "offline-only"
        self.args = argparse.Namespace(binary=self.binary, destroy_input=True, output=self.output,
            expected_sha256="", source_freeze=None, source_home=self.source, repo=self.root,
            max_output_tokens=2048, max_iterations=5, round_timeout=30)

    def execute(self) -> tuple[int, check.ImageContextPressureLiveCheck]:
        with patch.dict(os.environ, {"UNIT_SELECTED_ROUTE_KEY": "unit-fake-key"}), contextlib.redirect_stdout(io.StringIO()):
            harness = check.ImageContextPressureLiveCheck(self.args)
            result = harness.run()
        return result, harness

    def test_three_local_fake_runs_resume_hashes_and_destroy_every_private_resource(self) -> None:
        original = (self.source / "config.toml").read_bytes()
        code, harness = self.execute()
        report = json.loads((self.output / "summary.json").read_text())
        self.assertEqual(code, 0, report)
        self.assertEqual([row["mode"] for row in report["rounds"]], ["new_session", "resume", "resume"])
        for number, row in enumerate(report["rounds"], 1):
            self.assertEqual(sorted(row["durable_image_sha256_counts"].values()), [number, number])
        self.assertEqual((self.source / "config.toml").read_bytes(), original)
        self.assertFalse(self.binary.exists())
        self.assertFalse(harness.root.exists())
        retained = b"".join(path.read_bytes() for path in self.output.iterdir())
        self.assertNotIn(b"unit-fake-key", retained)
        self.assertNotIn(b"data:image", retained)
        self.assertEqual(set(path.name for path in self.output.iterdir()), {"progress.json", "summary.json",
            "round-1-diagnostics.json", "round-2-diagnostics.json", "round-3-diagnostics.json"})

    def test_preparation_failure_still_destroys_supplied_binary_and_private_root(self) -> None:
        self.args.expected_sha256 = "0" * 64
        code, harness = self.execute()
        self.assertEqual(code, 1)
        self.assertFalse(self.binary.exists())
        self.assertFalse(harness.root.exists())
        self.assertTrue(harness.report["cleanup"]["input_binary_removed"])

    def test_model_failure_still_cleans_raw_output_and_credential_environment(self) -> None:
        self.binary.write_text('#!/usr/bin/env python3\nimport os,sys\nprint(os.environ["METIS_IMAGE_PRESSURE_ROUTE_KEY"])\nsys.exit(7)\n')
        self.binary.chmod(0o700)
        code, harness = self.execute()
        self.assertEqual(code, 1)
        self.assertEqual(harness.report["rounds"][0]["exit_code"], 7)
        self.assertFalse(harness.root.exists())
        self.assertNotIn("unit-fake-key", (self.output / "summary.json").read_text())


if __name__ == "__main__":
    unittest.main()
