"""Controlled model exchange for pause/resume with an actual Bash counter."""

from pathlib import Path
import json
import tempfile
import threading
import unittest
from http.server import ThreadingHTTPServer
from urllib.request import Request, urlopen

from .desktop_fixture import (Fixture, PAUSE_CALL_PREFIX, PAUSE_CHECK_COMMAND,
                              PAUSE_WRITE_COMMAND, handler_for)


class PauseFixtureTest(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix="metis-pause-provider-test-")
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

    def response(self, items, stream=False):
        request = Request(f"http://127.0.0.1:{self.server.server_port}/v1/responses",
                          data=json.dumps({"input": items, "stream": stream,
                                           "model": "desktop-fixture-model"}).encode(),
                          headers={"Content-Type": "application/json"})
        with urlopen(request, timeout=5) as response:
            raw = response.read().decode()
        if not stream:
            return json.loads(raw)["output"]
        events = [json.loads(line[6:]) for line in raw.splitlines() if line.startswith("data: {")]
        return next(e["response"]["output"] for e in events if e["type"] == "response.completed")

    @staticmethod
    def user(text="[pause-resume-fixture] [pause-gate:native] 执行一次后暂停并恢复"):
        return {"type": "message", "role": "user", "content": [{"type": "input_text", "text": text}]}

    @staticmethod
    def call(output):
        return next(item for item in output if item["type"] == "function_call")

    @staticmethod
    def result(call, value="PAUSE_COUNTER= 1\n"):
        return {"type": "function_call_output", "call_id": call["call_id"], "output": value}

    def test_resume_keeps_existing_result_and_only_checks_counter(self):
        items = [self.user()]
        prepared = self.response(items, stream=True)
        write = self.call(prepared)
        self.assertEqual(write["name"], "Bash")
        self.assertEqual(json.loads(write["arguments"])["command"], PAUSE_WRITE_COMMAND)
        items.extend(prepared)
        items.append(self.result(write))
        waiting = self.response(items)
        self.assertFalse(any(item["type"] == "function_call" for item in waiting))
        self.assertEqual(self.fixture.calls[-1]["phase"], "pause_wait")
        # The resumed invocation carries identical history, no fabricated user.
        resumed = self.response(items, stream=True)
        read = self.call(resumed)
        self.assertTrue(read["call_id"].startswith(PAUSE_CALL_PREFIX + "check_"))
        self.assertEqual(json.loads(read["arguments"])["command"], PAUSE_CHECK_COMMAND)
        self.assertNotIn(">>", PAUSE_CHECK_COMMAND)
        items.extend(resumed)
        items.append(self.result(read))
        final = self.response(items, stream=True)
        self.assertIn("恢复验收完成", final[0]["content"][0]["text"])
        self.assertFalse(any(item["type"] == "function_call" for item in final))
        self.assertEqual([entry["phase"] for entry in self.fixture.calls],
                         ["pause_prepare", "pause_wait", "pause_check", "pause_final"])
        self.assertTrue(all(entry["user_messages"] == 1 for entry in self.fixture.calls))

    def test_actual_error_or_duplicate_counter_cannot_claim_success(self):
        for value in ("PAUSE_COUNTER=2\n", "permission denied", "PAUSE_COUNTER=0", "fake 1"):
            with self.subTest(value=value):
                result = self.result({"call_id": PAUSE_CALL_PREFIX + "check_1"}, value)
                output = self.response([self.user(), result])
                self.assertEqual(self.fixture.calls[-1]["phase"], "pause_failed")
                self.assertIn("验收未通过", output[0]["content"][0]["text"])
                self.assertFalse(any(item["type"] == "function_call" for item in output))

    def test_new_user_scope_does_not_reuse_prior_tool_result(self):
        result = self.result({"call_id": PAUSE_CALL_PREFIX + "prepare_old"})
        items = [self.user(), result, self.user("[pause-resume-fixture] [pause-gate:new] 新任务")]
        self.assertEqual(json.loads(self.call(self.response(items))["arguments"])["command"], PAUSE_WRITE_COMMAND)
        self.assertEqual(self.fixture.calls[-1]["tool_results"], [])
        output = self.response(items + [self.user("ordinary request")])
        self.assertEqual(self.fixture.calls[-1]["phase"], "echo")
        self.assertIn("FIXTURE_REPLY_", output[0]["content"][0]["text"])

    def test_runtime_reminder_does_not_discard_completed_tool_result(self):
        items = [self.user(), self.result({"call_id": PAUSE_CALL_PREFIX + "prepare_1"}),
                 self.user("<system-reminder>CONTRACT REMINDER: verify completed file mutation.</system-reminder>")]
        output = self.response(items)
        self.assertEqual(self.fixture.calls[-1]["phase"], "pause_wait")
        self.assertEqual(self.fixture.calls[-1]["user_messages"], 1)
        self.assertFalse(any(item["type"] == "function_call" for item in output))
        output = self.response(items)
        self.assertEqual(json.loads(self.call(output)["arguments"])["command"], PAUSE_CHECK_COMMAND)


if __name__ == "__main__":
    unittest.main()
