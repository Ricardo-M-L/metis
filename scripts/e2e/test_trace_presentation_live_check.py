"""Offline presentation contracts; no real model or user authentication read."""

import argparse
import contextlib
import copy
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import io
import json
from pathlib import Path
import tempfile
import threading
import time
import unittest

import trace_presentation_live_check as check


class TraceProofTests(unittest.TestCase):
    def setUp(self) -> None:
        self.text = "trace complete line\n" * 160
        self.sha = "a" * 64
        self.fixture = {
            "enabled": True, "hasMore": False,
            "initialContext": {"available": True, "source": "logical-request", "system": "actual-system-canary",
                "capturedAt": "2026-11-09T12:00:00Z", "model": check.TARGET_MODEL,
                "tools": [{"name": "Read", "description": "Read fixture", "inputSchema": {"type": "object"}}]},
            "events": [
                {"kind": "user", "images": [{"mediaType": "image/png", "attachmentHash": self.sha,
                    "url": "/api/trace/image?session_id=private&hash=" + self.sha, "bytes": 1234, "width": 600, "height": 600, "name": "attachment-a.png"}]},
                {"kind": "tool_start", "toolName": "Read", "traceCallID": "call-1", "text": '{"path":"/private/synthetic.txt"}'},
                {"kind": "tool_result", "toolName": "Read", "traceCallID": "call-1", "isError": False},
                {"kind": "text", "streamID": "actual-stream-1", "text": self.text[:2000]},
                {"kind": "text", "streamID": "actual-stream-1", "text": self.text[2000:]},
                {"kind": "request_timing", "requestTiming": {"request_id": "request-1", "phase": "pre_request_start",
                    "provider_call": 0, "attempt": 0, "elapsed_ms": 0, "at": "2026-11-09T12:00:00Z"}},
            ]}

    def validate(self, fixture: dict | None = None) -> dict:
        return check.validate_trace(fixture or self.fixture, "actual-system-canary", self.text, {self.sha}, Path("/private/synthetic.txt"))[0]

    def test_complete_real_shape_preserves_split_stream_and_zero_timing(self) -> None:
        proof = self.validate()
        self.assertGreater(proof["complete_text_characters"], 2000)
        self.assertEqual(proof["text_stream_count"], 1)
        self.assertEqual(proof["timing_phases"], {"pre_request_start": 1})
        self.assertEqual(proof["tool_names"], ["Read"])

    def test_rejects_fabricated_or_missing_initial_context(self) -> None:
        for patch in ({"source": "header"}, {"available": False}, {"system": "another system"}, {"tools": []}, {"truncated": True}):
            fixture = copy.deepcopy(self.fixture)
            fixture["initialContext"].update(patch)
            with self.assertRaises(AssertionError):
                self.validate(fixture)

    def test_detects_truncation_and_lost_stream_identity(self) -> None:
        for change in ("truncated", "stream", "text"):
            fixture = copy.deepcopy(self.fixture)
            if change == "truncated":
                fixture["events"][3]["truncated"] = True
            elif change == "stream":
                fixture["events"][3].pop("streamID")
            else:
                fixture["events"][4]["text"] = "missing tail"
            with self.assertRaises(AssertionError):
                self.validate(fixture)

    def test_rejects_external_image_urls_wrong_dimensions_and_unpaired_tools(self) -> None:
        for change in ("external", "dimensions", "pair", "path"):
            fixture = copy.deepcopy(self.fixture)
            if change == "external":
                fixture["events"][0]["images"][0]["url"] = "https://unrelated.invalid/image"
            elif change == "dimensions":
                fixture["events"][0]["images"][0]["height"] = 42
            elif change == "pair":
                fixture["events"][2]["traceCallID"] = "other"
            else:
                fixture["events"][1]["text"] = '{"path":"/private/user-file"}'
            with self.assertRaises(AssertionError):
                self.validate(fixture)

    def test_structured_timing_is_required(self) -> None:
        fixture = copy.deepcopy(self.fixture)
        fixture["events"][-1]["requestTiming"] = {"request_id": "request-1", "phase": "old text only"}
        with self.assertRaises(AssertionError):
            self.validate(fixture)


class EventReaderTests(unittest.TestCase):
    def test_real_local_sse_parser_ready_delta_and_cleanup(self) -> None:
        class Handler(BaseHTTPRequestHandler):
            def do_GET(self) -> None:
                self.send_response(200)
                self.send_header("Content-Type", "text/event-stream")
                self.end_headers()
                self.wfile.write(b'event: ready\ndata: {"kind":"ready","session":"initial"}\n\n')
                self.wfile.flush()
                self.wfile.write(b'id: 1\nevent: text_delta\ndata: {"session":"private","delta":"complete text"}\n\n')
                self.wfile.flush()

            def log_message(self, *_args: object) -> None:
                pass

        server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        thread = threading.Thread(target=server.serve_forever, daemon=True)
        thread.start()
        reader = check.EventReader(server.server_address[1], 30)
        try:
            self.assertTrue(reader.ready.wait(2))
            deadline = time.monotonic() + 2
            while len(reader.events) < 2 and time.monotonic() < deadline:
                time.sleep(.01)
            self.assertEqual([name for name, _ in reader.events], ["ready", "text_delta"])
            self.assertEqual(reader.events[1][1]["delta"], "complete text")
        finally:
            reader.close()
            server.shutdown()
            server.server_close()
            thread.join(timeout=2)
        self.assertFalse(reader.thread.is_alive())


class CleanupTests(unittest.TestCase):
    def test_preparation_failure_still_removes_binary_and_private_resources(self) -> None:
        with tempfile.TemporaryDirectory(prefix="metis-trace-unit-") as folder:
            root = Path(folder)
            binary = root / "disposable-unexecuted-binary"
            binary.write_bytes(b"not executed")
            args = argparse.Namespace(binary=binary, destroy_input=True, expected_sha256="0" * 64,
                output=root / "evidence.noindex" / "offline", source_home=root / "never-read", query_timeout=30,
                max_output_tokens=4096, hold_seconds=0, stop_file=None)
            with contextlib.redirect_stdout(io.StringIO()):
                harness = check.TracePresentationLiveCheck(args)
                code = harness.run()
            self.assertEqual(code, 1)
            self.assertFalse(binary.exists())
            self.assertFalse(harness.root.exists())
            self.assertTrue(harness.report["cleanup"]["credential_environment_cleared"])
            self.assertEqual(harness.report["model_turn_submissions"], 0)


if __name__ == "__main__":
    unittest.main()
