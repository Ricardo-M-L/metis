"""HTTP/SSE contracts for the deterministic Artifact point-edit provider.

This exercises only a local controlled provider; it does not launch METIS or
write its artifact store. The end-to-end Desktop acceptance uses the same
responses to execute the real Artifact tools.
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
    from .desktop_fixture import Fixture, handler_for
else:
    from desktop_fixture import Fixture, handler_for


class ArtifactEditFixtureTest(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix="metis-artifact-provider-test-")
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

    def response(self, items, *, stream=True):
        request = Request(f"http://127.0.0.1:{self.server.server_port}/v1/responses",
                          data=json.dumps({"model": "desktop-fixture-model", "stream": stream,
                                           "input": items, "instructions": "private instruction"}).encode(),
                          headers={"Content-Type": "application/json"})
        with urlopen(request, timeout=5) as response:
            raw = response.read().decode()
        if not stream:
            return json.loads(raw)["output"]
        events = [json.loads(line.removeprefix("data: ")) for line in raw.splitlines()
                  if line.startswith("data: {")]
        output = next(event["response"]["output"] for event in events
                      if event["type"] == "response.completed")
        calls = [item for item in output if item["type"] == "function_call"]
        for call in calls:
            self.assertTrue(any(event["type"] == "response.output_item.done"
                                and event["item"] == call for event in events))
        return output

    @staticmethod
    def user(text):
        return {"type": "message", "role": "user", "content": [{"type": "input_text", "text": text}]}

    @staticmethod
    def result(call, output):
        return {"type": "function_call_output", "call_id": call["call_id"],
                "output": json.dumps(output) if isinstance(output, dict) else output}

    @staticmethod
    def annotation(version=1):
        context = {"artifactId": "fixture-artifact", "version": version, "digest": "a" * 64,
                   "targetId": "target-1", "instruction": "把按钮改成青绿色，文案改为开始体验",
                   "title": "点选修改验收",
                   "selection": {"tag": "a", "selector": "html > body > main > a:nth-of-type(1)",
                                 "text": "探索工作成果"}}
        return "Edit the selected item.\n```json\n" + json.dumps({"metis_artifact_annotation": context}) + "\n```"

    def create(self):
        items = [self.user("[artifact-edit-fixture]")]
        output = self.response(items)
        call = next(item for item in output if item["type"] == "function_call")
        self.assertEqual(call["name"], "Artifact")
        arguments = json.loads(call["arguments"])
        self.assertEqual(arguments["action"], "create")
        self.assertEqual(arguments["title"], "点选修改验收")
        self.assertIn('id="fixture-cta"', arguments["html"])
        self.assertNotIn("<script", arguments["html"])
        return items, output, call, arguments["html"]

    def test_real_tool_exchange_create_read_update_and_evidence(self):
        items, output, create, html = self.create()
        items.extend(output)
        items.append(self.result(create, {"action": "created", "artifact": {
            "id": "fixture-artifact", "current_version": 1}}))
        final = self.response(items, stream=False)
        self.assertIn("版本 1", final[0]["content"][0]["text"])

        items.extend(final)
        items.append(self.user(self.annotation()))
        output = self.response(items)
        read = next(item for item in output if item["type"] == "function_call")
        self.assertEqual(json.loads(read["arguments"]),
                         {"action": "read", "id": "fixture-artifact", "version": 1})
        items.extend(output)
        items.append(self.result(read, {"artifact": {"id": "fixture-artifact", "current_version": 1},
                                        "version": {"number": 1}, "html": html}))
        output = self.response(items)
        update = next(item for item in output if item["type"] == "function_call")
        arguments = json.loads(update["arguments"])
        self.assertEqual(arguments["action"], "update")
        self.assertEqual(arguments["id"], "fixture-artifact")
        self.assertEqual(arguments["expected_version"], 1)
        self.assertIn("开始体验", arguments["html"])
        self.assertIn("#0f9d83", arguments["html"])
        self.assertIn("探索工作成果", html)
        items.extend(output)
        items.append(self.result(update, {"action": "updated", "artifact": {
            "id": "fixture-artifact", "current_version": 2}}))
        final = self.response(items)
        self.assertIn("版本 2", final[0]["content"][0]["text"])
        evidence = (Path(self.temp.name) / "model-evidence.jsonl").read_text()
        self.assertNotIn("private instruction", evidence)
        entries = [json.loads(line) for line in evidence.splitlines()]
        annotation = next(row["annotation"] for row in entries if row.get("phase") == "artifact_edit_read")
        self.assertEqual(annotation["targetId"], "target-1")
        self.assertEqual(annotation["selection"]["text"], "探索工作成果")
        tool_calls = [call for row in entries if row["event"] == "started"
                      for call in row.get("tool_calls", [])]
        self.assertEqual([call["arguments"]["action"] for call in tool_calls], ["create", "read", "update"])

    def test_failed_update_does_not_claim_new_version(self):
        _, _, _, html = self.create()
        items = [self.user(self.annotation())]
        read_output = self.response(items)
        read = next(item for item in read_output if item["type"] == "function_call")
        items.extend(read_output)
        items.append(self.result(read, {"artifact": {"id": "fixture-artifact", "current_version": 1},
                                        "version": {"number": 1}, "html": html}))
        update_output = self.response(items)
        update = next(item for item in update_output if item["type"] == "function_call")
        items.extend(update_output)
        items.append(self.result(update, "Artifact: artifact: version changed (expected 1, current 2)"))
        final = self.response(items)
        self.assertFalse(any(item["type"] == "function_call" for item in final))
        self.assertNotIn("已创建版本 2", final[0]["content"][0]["text"])
        self.assertIn("未更新", final[0]["content"][0]["text"])

    def test_tool_result_scope_restarts_at_latest_user_and_unknown_prompt_echoes(self):
        _, _, call, _ = self.create()
        items = [self.user("[artifact-edit-fixture]"), self.result(call, "failure"),
                 self.user("[artifact-edit-fixture]")]
        output = self.response(items, stream=False)
        self.assertEqual(json.loads(output[1]["arguments"])["action"], "create")
        final = self.response([self.user("ordinary prompt ```json\n{\"selection\":{}}\n```")], stream=False)
        self.assertIn("FIXTURE_REPLY_", final[0]["content"][0]["text"])

    def test_opt_in_omission_changes_only_update_arguments(self):
        self.server.RequestHandlerClass = handler_for(self.fixture, omit_artifact_expected_version=True)
        _, _, _, html = self.create()
        items = [self.user(self.annotation())]
        output = self.response(items)
        read = next(item for item in output if item["type"] == "function_call")
        self.assertEqual(json.loads(read["arguments"]),
                         {"action": "read", "id": "fixture-artifact", "version": 1})
        items.extend(output)
        items.append(self.result(read, {"artifact": {"id": "fixture-artifact", "current_version": 1},
                                        "version": {"number": 1}, "html": html}))
        output = self.response(items)
        update = next(item for item in output if item["type"] == "function_call")
        arguments = json.loads(update["arguments"])
        self.assertEqual(arguments["action"], "update")
        self.assertEqual(arguments["id"], "fixture-artifact")
        self.assertNotIn("expected_version", arguments)
        self.assertIn("开始体验", arguments["html"])
        self.assertEqual(self.fixture.calls[-1]["annotation"]["version"], 1)
        self.assertEqual(self.fixture.calls[-1]["annotation"]["instruction"],
                         "把按钮改成青绿色，文案改为开始体验")
        self.assertNotIn("expected_version", self.fixture.calls[-1]["tool_calls"][0]["arguments"])
        regular = self.response([self.user("ordinary prompt")], stream=False)
        self.assertIn("FIXTURE_REPLY_", regular[0]["content"][0]["text"])


if __name__ == "__main__":
    unittest.main()
