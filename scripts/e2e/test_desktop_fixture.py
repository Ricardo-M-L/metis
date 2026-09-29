"""Focused contract checks for the isolated Responses API test provider."""

from __future__ import annotations

import json
from pathlib import Path
import tempfile
import threading
import unittest
from http.server import ThreadingHTTPServer
from urllib.request import Request, urlopen

if __package__:
    from .desktop_fixture import Fixture, handler_for
else:
    from desktop_fixture import Fixture, handler_for


class DesktopFixtureResponsesTest(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix="metis-fixture-provider-test-")
        self.fixture = Fixture(Path(self.temp.name))
        self.server = ThreadingHTTPServer(("127.0.0.1", 0), handler_for(self.fixture))
        self.server.daemon_threads = True
        self.thread = threading.Thread(target=self.server.serve_forever, daemon=True)
        self.thread.start()

    def tearDown(self):
        self.fixture.stopped.set()
        self.server.shutdown()
        self.server.server_close()
        self.thread.join(timeout=5)
        self.temp.cleanup()

    def response(self, input_items, *, stream=True, instructions="private test instruction"):
        body = {"model": "desktop-fixture-model", "stream": stream, "input": input_items,
                "instructions": instructions}
        request = Request(f"http://127.0.0.1:{self.server.server_port}/v1/responses",
                          data=json.dumps(body).encode(), headers={"Content-Type": "application/json"})
        with urlopen(request, timeout=5) as response:
            raw = response.read().decode()
        if not stream:
            return json.loads(raw)
        return [json.loads(line.removeprefix("data: ")) for line in raw.splitlines()
                if line.startswith("data: {")]

    def test_agent_fixture_streams_child_and_only_finalizes_after_tool_result(self):
        initial = [{"type": "message", "role": "user", "content": [{"type": "input_text", "text": "[agent-fixture]"}]}]
        events = self.response(initial)
        done = next(event for event in events if event["type"] == "response.output_item.done")
        call = done["item"]
        self.assertEqual(call["name"], "Agent")
        self.assertEqual(json.loads(call["arguments"]),
                         {"prompt": "CHILD_FIXTURE_TASK_1", "name": "probe-1", "isolation": "none",
                          "subagent_type": "explore", "run_in_background": False})
        terminal = next(event for event in events if event["type"] == "response.completed")
        self.assertEqual(terminal["response"]["output"], [call])

        child = self.response([{"type": "message", "role": "user", "content": [
            {"type": "input_text", "text": "CHILD_FIXTURE_TASK_1"}]}])
        deltas = [event["delta"] for event in child if event["type"] == "response.output_text.delta"]
        self.assertEqual(len(deltas), 2)
        self.assertIn("CHILD_FIXTURE_STREAM_", "".join(deltas))
        self.assertIn("Child task complete.", "".join(deltas))

        followup = self.response(initial + [{"type": "function_call_output", "call_id": call["call_id"],
                                             "output": "Child task complete."}])
        final = next(event for event in followup if event["type"] == "response.completed")
        self.assertIn("FIXTURE_AGENT_FINAL_", final["response"]["output"][0]["content"][0]["text"])
        self.assertEqual([item["phase"] for item in self.fixture.calls],
                         ["parent_call", "child_stream", "parent_final"])
        evidence = (Path(self.temp.name) / "model-evidence.jsonl").read_text()
        self.assertNotIn("private test instruction", evidence)

    def test_regular_echo_and_nonstream_agent_call(self):
        regular = self.response([{"type": "message", "role": "user", "content": [
            {"type": "input_text", "text": "ordinary prompt"}]}])
        terminal = next(event for event in regular if event["type"] == "response.completed")
        self.assertIn("FIXTURE_REPLY_", terminal["response"]["output"][0]["content"][0]["text"])
        self.assertEqual(self.fixture.calls[0]["phase"], "echo")
        nonstream = self.response([{"type": "message", "role": "user", "content": [
            {"type": "input_text", "text": "[agent-fixture]"}]}], stream=False)
        self.assertEqual(nonstream["output"][0]["type"], "function_call")
        self.assertEqual(nonstream["output"][0]["name"], "Agent")
        gated = self.response([{"type": "message", "role": "user", "content": [
            {"type": "input_text", "text": "[agent-fixture] [gate:subagent]"}]}], stream=False)
        self.assertEqual(json.loads(gated["output"][0]["arguments"])["prompt"],
                         "CHILD_FIXTURE_TASK_1 [gate:subagent]")


if __name__ == "__main__":
    unittest.main()
