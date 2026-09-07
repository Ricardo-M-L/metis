"""Independent post-publication oracle, including the shared channel examples."""

import importlib.util
import json
from pathlib import Path
import unittest


SPEC = importlib.util.spec_from_file_location("cli_oracle", Path(__file__).with_name("verify_cli_default_channel.py"))
oracle = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(oracle)


class DefaultChannelTests(unittest.TestCase):
    def release(self, tag):
        return {"tag_name": tag, "draft": False, "prerelease": False, "assets": [
            {"name": name, "state": "uploaded", "size": 1,
             "browser_download_url": f"https://github.com/owner/metis/releases/download/{tag}/{name}"}
            for name in oracle.CLI_ASSETS]}

    def snapshot(self, tag):
        return {"schema_version": 1, "repository": "owner/metis", "selected_tag": tag}

    def test_shared_conformance_fixtures(self):
        fixture = json.loads((Path(__file__).resolve().parents[1] / "install/testdata/cli-releases.json").read_text())
        for case in fixture["cases"]:
            with self.subTest(case=case["name"]):
                selected = oracle.select_highest(case["releases"], fixture["repo"])
                self.assertEqual(selected, case.get("want_tag", ""))

    def test_all_pages_before_numeric_maximum(self):
        calls = []
        def fetch(repo, page, remaining):
            calls.append(page)
            return json.dumps(([{}] * 99 + [self.release("v0.4.9")]) if page == 1 else [self.release("v0.4.52")]).encode()
        self.assertEqual(oracle.discover("owner/metis", fetch=fetch)["selected_tag"], "v0.4.52")
        self.assertEqual(calls, [1, 2])

    def test_later_page_failure_is_not_partial_success(self):
        def fetch(repo, page, remaining):
            if page == 2:
                raise ValueError("failed second page")
            return json.dumps([self.release("v0.4.52")] * 100).encode()
        with self.assertRaises(ValueError):
            oracle.discover("owner/metis", fetch=fetch)

    def test_full_tenth_page_and_invalid_responses_fail(self):
        for payload in [b"{}", b"[]{}", b"null", b" " * (oracle.MAX_BYTES + 1),
                        json.dumps([self.release("v0.4.52")] * 101).encode(),
                        json.dumps([self.release("v0.4.52")] * 100).encode()]:
            with self.subTest(length=len(payload)):
                with self.assertRaises(ValueError):
                    oracle.discover("owner/metis", fetch=lambda *args: payload)

    def test_one_overall_deadline_even_when_a_fetch_returns(self):
        now = [0.0]
        def fetch(repo, page, remaining):
            now[0] += 31.0
            return json.dumps([self.release("v0.4.52")]).encode()
        with self.assertRaisesRegex(ValueError, "deadline"):
            oracle.discover("owner/metis", fetch=fetch, clock=lambda: now[0])

    def test_old_tags_and_prereleases_skip_new_default_path(self):
        self.assertFalse(oracle.supports_default_check("v0.4.51", False))
        self.assertTrue(oracle.supports_default_check("v0.4.52", False))
        self.assertTrue(oracle.supports_default_check("v1.0.0", False))
        self.assertFalse(oracle.supports_default_check("v0.4.60", True))

    def test_selected_before_or_after_release_race_is_valid(self):
        before, after = self.snapshot("v0.4.52"), self.snapshot("v0.4.53")
        for actual in ("v0.4.52", "v0.4.53"):
            result = oracle.verify_install("owner/metis", "v0.4.52", actual, before, after)
            self.assertEqual(result["actual_tag"], actual)
            self.assertEqual(result["expected_before"], "v0.4.52")
            self.assertEqual(result["expected_after"], "v0.4.53")

    def test_shared_latest_or_unselected_version_cannot_pass(self):
        for actual in ("v0.4.47", "v0.4.51", "v0.4.54"):
            with self.assertRaises(ValueError):
                oracle.verify_install("owner/metis", "v0.4.52", actual,
                                      self.snapshot("v0.4.52"), self.snapshot("v0.4.53"))

    def test_future_current_cli_latest_does_not_fail_historical_v52_reverify(self):
        oracle.verify_install("owner/metis", "v0.4.52", "v0.5.4",
                              self.snapshot("v0.5.4"), self.snapshot("v0.5.4"))

    def test_wrong_snapshot_repository_rejected(self):
        wrong = {**self.snapshot("v0.4.52"), "repository": "other/metis"}
        with self.assertRaises(ValueError):
            oracle.verify_install("owner/metis", "v0.4.52", "v0.4.52", wrong, self.snapshot("v0.4.52"))


if __name__ == "__main__":
    unittest.main()
