"""Contracts for an opt-in workspace cwd probe using a local controlled provider.

This does not launch METIS or execute Bash. Native acceptance executes the same
constant read-only command in different workspaces and inspects real results.
"""

from __future__ import annotations

import json
from pathlib import Path
import tempfile
import threading
import unittest
from http.server import ThreadingHTTPServer
from urllib.request import Request, urlopen

if __package__:
    from .desktop_fixture import WORKSPACE_CALL_PREFIX, WORKSPACE_PROBE_COMMAND, Fixture, handler_for
else:
    from desktop_fixture import WORKSPACE_CALL_PREFIX, WORKSPACE_PROBE_COMMAND, Fixture, handler_for


class WorkspaceFixtureTest(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix="metis-workspace-provider-test-")
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

    def response(self, items, *, stream=False):
        request = Request(f"http://127.0.0.1:{self.server.server_port}/v1/responses",
                          data=json.dumps({"model": "desktop-fixture-model", "stream": stream,
                                           "input": items, "instructions": "private system instruction"}).encode(),
                          headers={"Content-Type": "application/json", "Authorization": "private header"})
        with urlopen(request, timeout=5) as response:
            raw = response.read().decode()
        if not stream:
            return json.loads(raw)["output"]
        events = [json.loads(line.removeprefix("data: ")) for line in raw.splitlines()
                  if line.startswith("data: {")]
        output = next(event["response"]["output"] for event in events
                      if event["type"] == "response.completed")
        for call in self.calls(output):
            self.assertTrue(any(event["type"] == "response.output_item.done"
                                and event["item"] == call for event in events))
        return output

    @staticmethod
    def user(text):
        return {"type": "message", "role": "user", "content": [{"type": "input_text", "text": text}]}

    @staticmethod
    def calls(output):
        return [item for item in output if item["type"] == "function_call"]

    @staticmethod
    def result(call, text):
        return {"type": "function_call_output", "call_id": call["call_id"], "output": text}

    def test_workspace_exchange_uses_constant_bash_command_and_actual_result(self):
        items = [self.user("[workspace-fixture] [workspace:B] 核对当前目录")]
        first_output = self.response(items, stream=True)
        first = self.calls(first_output)[0]
        arguments = json.loads(first["arguments"])
        self.assertEqual(first["name"], "Bash")
        self.assertEqual(arguments["command"], "pwd && cat ./fixture-notes.md ./fixture-checks.md")
        self.assertEqual(arguments["command"], WORKSPACE_PROBE_COMMAND)
        self.assertEqual(arguments["timeout_ms"], 10000)
        self.assertIn("workspace", arguments["description"])
        self.assertEqual(self.fixture.calls[-1]["tool_calls"], [{"name": "Bash",
                         "call_id": first["call_id"], "arguments": arguments}])
        # Assistant calls alone cannot advance the exchange; METIS must supply
        # a function_call_output from the actual tool execution.
        repeated = self.response(items + first_output)
        self.assertEqual(json.loads(self.calls(repeated)[0]["arguments"]), arguments)
        first_result = self.result(first, "/tmp/selected-workspace-b\nWORKSPACE_B_NOTES\nWORKSPACE_B_CHECKS\n")
        items.extend(first_output)
        items.append(first_result)
        final = self.response(items, stream=True)
        self.assertEqual(self.calls(final), [])
        text = final[0]["content"][0]["text"]
        self.assertIn("本地受控 provider", text)
        self.assertIn("不代表真实模型评测结果", text)
        self.assertIn("/tmp/selected-workspace-b", text)
        self.assertIn("WORKSPACE_B", text)
        self.assertEqual(self.fixture.calls[-1]["tool_results"], [first_result])
        self.assertEqual(self.fixture.calls[-1]["workspace_result"],
                         {"cwd": "/tmp/selected-workspace-b", "marker": "B"})
        evidence = (Path(self.temp.name) / "model-evidence.jsonl").read_text()
        self.assertIn("WORKSPACE_B_NOTES", evidence)
        self.assertIn("WORKSPACE_B_CHECKS", evidence)
        self.assertNotIn("private header", evidence)
        self.assertNotIn("private system instruction", evidence)

    def test_exchange_scope_ignores_old_turn_and_unrelated_tool_outputs(self):
        old_call = {"call_id": WORKSPACE_CALL_PREFIX + "old"}
        unrelated = {"call_id": "call_unrelated"}
        items = [self.user("[workspace-fixture] previous"),
                 self.result(old_call, "WORKSPACE_A_NOTES"), self.result(old_call, "WORKSPACE_A_CHECKS"),
                 self.user("[workspace-fixture] current"),
                 self.result(unrelated, "not a fixture Read"),
                 {"role": "tool", "content": "not a Responses tool result"}]
        output = self.response(items)
        self.assertEqual(json.loads(self.calls(output)[0]["arguments"])["command"], WORKSPACE_PROBE_COMMAND)
        self.assertEqual(self.fixture.calls[-1]["tool_results"], [])
        self.assertEqual(self.fixture.calls[-1]["phase"], "workspace_probe")

    def test_activity_marker_keeps_absolute_paths_and_existing_final(self):
        items = [self.user("[activity-fixture]")]
        expected_workspace = Path(self.temp.name) / "workspace"
        for filename in ("fixture-notes.md", "fixture-checks.md"):
            output = self.response(items)
            call = self.calls(output)[0]
            self.assertEqual(json.loads(call["arguments"]), {"path": str(expected_workspace / filename)})
            self.assertNotIn("tool_calls", self.fixture.calls[-1])
            self.assertNotIn("tool_results", self.fixture.calls[-1])
            items.extend(output)
            items.append(self.result(call, "activity test result"))
        final = self.response(items)
        self.assertEqual(self.calls(final), [])
        self.assertIn("底栏展示执行统计", final[0]["content"][0]["text"])
        self.assertIn("本地测试模型", final[0]["content"][0]["text"])
        self.assertNotIn("tool_calls", self.fixture.calls[-1])
        self.assertNotIn("tool_results", self.fixture.calls[-1])

    def test_failed_or_mismatched_results_cannot_claim_success_and_ordinary_echoes(self):
        call = {"call_id": WORKSPACE_CALL_PREFIX + "probe"}
        cases = ["Bash: permission denied", "relative/path\nWORKSPACE_B_NOTES\nWORKSPACE_B_CHECKS",
                 "/tmp/b\nWORKSPACE_B_NOTES\ncat: ./fixture-checks.md: No such file or directory\n[exit status 1]",
                 "/tmp/b\nWORKSPACE_B_NOTES\nWORKSPACE_A_CHECKS",
                 "/tmp/a\nWORKSPACE_A_NOTES\nWORKSPACE_A_CHECKS",
                 "/tmp/b\nWORKSPACE_B_NOTES\nWORKSPACE_B_CHECKS\n[exit status 2]"]
        for result in cases:
            with self.subTest(output=result):
                output = self.response([self.user("[workspace-fixture] [workspace:B]"), self.result(call, result)])
                self.assertEqual(self.calls(output), [])
                self.assertEqual(self.fixture.calls[-1]["phase"], "workspace_failed")
                self.assertNotIn("workspace_result", self.fixture.calls[-1])
                self.assertIn("验收未通过", output[0]["content"][0]["text"])
                self.assertNotIn("验收完成", output[0]["content"][0]["text"])
        output = self.response([self.user("ordinary workspace request")])
        self.assertEqual(self.calls(output), [])
        self.assertIn("FIXTURE_REPLY_", output[0]["content"][0]["text"])
        self.assertNotIn("tool_calls", self.fixture.calls[-1])
        self.assertNotIn("tool_results", self.fixture.calls[-1])


if __name__ == "__main__":
    unittest.main()
