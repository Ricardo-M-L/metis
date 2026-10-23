"""Offline acceptance-oracle tests. No credentials, providers or user files."""
from __future__ import annotations

import argparse
import io
import json
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile
import unittest
from unittest.mock import patch

sys.path.insert(0, str(Path(__file__).resolve().parent))
from checkpoint_multirepo_live_check import (AUTH_COMMAND, CheckpointMultiRepoLiveCheck,
    assert_fixture, guard_decision, managed_oracle, owned_file, readonly_diagnostic,
    readonly_managed_oracle, run_guard)
from persistent_agent_live_check import PersistentAgentLiveCheck


class FixtureCase(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix="checkpoint-oracle-")
        self.root = Path(self.temp.name).resolve()
        self.cwd = self.root / "workspace"
        self.cwd.mkdir()
        self.fixture = self.cwd / "metis-checkpoint-live-offline"
        self.fixture.mkdir()
        for name in ("project-a", "project-b"):
            (self.fixture / name).mkdir()
        self.a = self.fixture / "project-a/existing.txt"
        self.b = self.fixture / "project-b/existing.txt"
        self.a.write_text("a-before\n")
        self.b.write_text("b-before\n")
        self.plan = {"session_id": "offline-session", "cwd": str(self.cwd), "fixture": str(self.fixture),
            "paths": [str(self.a), str(self.b)], "commands": [AUTH_COMMAND, "EXACT_OWNED_MUTATION"],
            "write_path": str(self.a), "write_content": "a-after\n", "edit_path": str(self.b),
            "edit_old": "b-before\n", "edit_new": "b-after\n"}

    def tearDown(self):
        self.temp.cleanup()

    def event(self, tool, **arguments):
        return {"hook_event_name": "PreToolUse", "session_id": "offline-session", "tool_name": tool,
                "tool_input": arguments}

    def test_exact_readonly_and_mutating_bash_allowed(self):
        for command in self.plan["commands"]:
            guard_decision(self.event("Bash", command=command, description="owned acceptance check"), self.plan)

    def test_unlisted_bash_suffix_rejected(self):
        for command in (AUTH_COMMAND + "; touch /tmp/other", "ls", "pwd", "metis auth keys list > out"):
            with self.subTest(command=command), self.assertRaises(AssertionError):
                guard_decision(self.event("Bash", command=command), self.plan)

    def test_background_and_alternate_cwd_rejected(self):
        for fields in ({"run_in_background": True}, {"await_completion": True},
                       {"required_for_completion": True}, {"cwd": "/tmp"}, {"working_dir": "/tmp"},
                       {"shell": "/bin/zsh"}, {"env": {"HOME": "/tmp"}}):
            with self.subTest(fields=fields), self.assertRaises(AssertionError):
                guard_decision(self.event("Bash", command=AUTH_COMMAND, **fields), self.plan)

    def test_wrong_tool_event_or_session_rejected(self):
        for override in ({"hook_event_name": "PostToolUse"}, {"session_id": "other"},
                         {"tool_name": "Agent"}, {"tool_input": None}):
            event = self.event("Read", path=str(self.a))
            event.update(override)
            with self.subTest(override=override), self.assertRaises(AssertionError):
                guard_decision(event, self.plan)

    def test_exact_read_write_edit_allowed(self):
        for event in (self.event("Read", path=str(self.a)),
                      self.event("Write", path=str(self.a), content="a-after\n"),
                      self.event("Edit", path=str(self.b), old="b-before\n", new="b-after\n"),
                      self.event("Edit", file_path=str(self.b), old_string="b-before\n", new_string="b-after\n")):
            guard_decision(event, self.plan)

    def test_wrong_contents_and_unlisted_path_rejected(self):
        for event in (self.event("Read", path=str(self.root / "unrelated.txt")),
                      self.event("Read", path=str(self.a), file_path=str(self.b)),
                      self.event("Write", path=str(self.b), content="a-after\n"),
                      self.event("Write", path=str(self.a), content="not-authorized"),
                      self.event("Edit", path=str(self.b), old="bad", new="b-after\n"),
                      self.event("Edit", path=str(self.b), old="b-before\n", new="b-after\n", all=True)):
            with self.subTest(event=event), self.assertRaises(AssertionError):
                guard_decision(event, self.plan)

    def test_symlink_file_rejected_for_both_file_tools_and_bash(self):
        self.a.unlink()
        target = self.root / "user.txt"
        target.write_text("untouched")
        self.a.symlink_to(target)
        for event in (self.event("Read", path=str(self.a)), self.event("Bash", command=AUTH_COMMAND)):
            with self.assertRaises(AssertionError):
                guard_decision(event, self.plan)
        self.assertEqual(target.read_text(), "untouched")

    def test_symlink_parent_rejected(self):
        self.a.unlink()
        self.a.parent.rmdir()
        outside = self.root / "external"
        outside.mkdir()
        (outside / self.a.name).write_text("original")
        self.a.parent.symlink_to(outside, target_is_directory=True)
        with self.assertRaises(AssertionError):
            owned_file(str(self.a), self.fixture, set(self.plan["paths"]))

    def test_guard_protocol_allows_and_records_hash_without_arguments(self):
        path = self.root / "policy.json"
        path.write_text(json.dumps(self.plan))
        output = io.StringIO()
        with patch("sys.stdin", io.StringIO(json.dumps(self.event("Bash", command=AUTH_COMMAND)))), patch("sys.stdout", output):
            self.assertEqual(run_guard(path), 0)
        self.assertEqual(json.loads(output.getvalue())["decision"], "allow")
        row = json.loads(path.with_suffix(".audit.jsonl").read_text())
        self.assertEqual(row["decision"], "allow")
        self.assertEqual(len(row["input_sha256"]), 64)
        self.assertNotIn(AUTH_COMMAND, json.dumps(row))

    def test_malformed_guard_plan_fails_closed(self):
        path = self.root / "policy.json"
        path.write_text("invalid")
        output = io.StringIO()
        with patch("sys.stdin", io.StringIO("{}")), patch("sys.stdout", output):
            self.assertEqual(run_guard(path), 0)
        self.assertEqual(json.loads(output.getvalue())["decision"], "halt")

    def test_unexpected_guard_command_halts(self):
        path = self.root / "policy.json"
        path.write_text(json.dumps(self.plan))
        output = io.StringIO()
        with patch("sys.stdin", io.StringIO(json.dumps(self.event("Bash", command="rm -rf /")))), patch("sys.stdout", output):
            run_guard(path)
        self.assertEqual(json.loads(output.getvalue())["decision"], "halt")

    def test_managed_paths_must_all_belong_to_two_fixture_subprojects(self):
        valid = {self.fixture.name + "/project-a/existing.txt": "sha-a", self.fixture.name + "/project-b/new.txt": ""}
        self.assertEqual(managed_oracle(valid, self.fixture.name), valid)
        for key in ("code/user.go", str(self.a), self.fixture.name + "/../user.txt",
                    self.fixture.name + "/project-a", self.fixture.name + "/unowned/file.txt"):
            with self.subTest(path=key), self.assertRaises(AssertionError):
                managed_oracle({**valid, key: "sha"}, self.fixture.name)
        with self.assertRaises(AssertionError):
            managed_oracle([], self.fixture.name)

    def test_readonly_requires_no_managed_changes_even_when_fixture_owned(self):
        readonly_managed_oracle({})
        for value in ({"code/user.go": "sha"}, {self.fixture.name + "/project-a/existing.txt": "sha"},
                      [], {"file": None}):
            with self.subTest(value=value), self.assertRaises(AssertionError):
                readonly_managed_oracle(value)

    def test_readonly_diagnostic_exports_only_safe_aggregate_counts(self):
        managed = {"private/secret-name.py": "fingerprint", "private/key.secret-token": "fingerprint",
                   "user/README": "fingerprint", "user/settings.json": "fingerprint"}
        proof = readonly_diagnostic(managed, {"private/secret-name.py", "user/README"}, {"user/settings.json"})
        self.assertEqual(proof["path_count"], 4)
        self.assertEqual(proof["missing_from_HEAD_count"], 2)
        self.assertEqual(proof["gitignored_count"], 1)
        self.assertEqual(proof["extension_counts"], {".py": 1, ".json": 1, "<none>": 1, "<other>": 1})
        for private in (*managed, "secret-token", "fingerprint"):
            self.assertNotIn(private, json.dumps(proof))

    def test_fixture_oracle_checks_exact_files_and_content(self):
        expected = {"project-a/existing.txt": "a-before\n", "project-b/existing.txt": "b-before\n"}
        self.assertEqual(len(assert_fixture(self.fixture, expected)), 2)
        self.a.write_text("wrong")
        with self.assertRaises(AssertionError): assert_fixture(self.fixture, expected)
        self.a.write_text("a-before\n")
        (self.fixture / "extra.txt").write_text("unplanned")
        with self.assertRaises(AssertionError): assert_fixture(self.fixture, expected)

    def test_cleanup_does_not_delete_rejected_input_binary_or_preexisting_fixture(self):
        checker = object.__new__(CheckpointMultiRepoLiveCheck)
        checker.args = argparse.Namespace(destroy_input=True)
        checker.destroy_source_allowed = False
        checker.fixture_created = False
        checker.fixture, checker.workspace, checker.nonce = self.fixture, self.cwd, "offline"
        checker.report = {}
        observed = []
        def cleanup_base(instance):
            observed.append(instance.args.destroy_input)
        with patch.object(PersistentAgentLiveCheck, "cleanup", cleanup_base):
            checker.cleanup()
        self.assertEqual(observed, [False])
        self.assertTrue(checker.args.destroy_input)
        self.assertEqual(self.a.read_text(), "a-before\n")

    def test_cleanup_removes_only_explicitly_created_owned_fixture(self):
        checker = object.__new__(CheckpointMultiRepoLiveCheck)
        checker.args = argparse.Namespace(destroy_input=True)
        checker.destroy_source_allowed = True
        checker.fixture_created = True
        checker.fixture, checker.workspace, checker.nonce = self.fixture, self.cwd, "offline"
        checker.report = {}
        sibling = self.cwd / "user-original.txt"
        sibling.write_text("must survive")
        with patch.object(PersistentAgentLiveCheck, "cleanup", lambda _: None):
            checker.cleanup()
        self.assertFalse(self.fixture.exists())
        self.assertEqual(sibling.read_text(), "must survive")

    def test_shadow_baseline_oracle_requires_real_commit_blob_and_managed_delta(self):
        if not shutil.which("git"):
            self.skipTest("git unavailable")
        checker = object.__new__(CheckpointMultiRepoLiveCheck)
        checker.fixture, checker.home, checker.sid = self.fixture, self.root / "home", "offline-session"
        checker.env = {"PATH": "/usr/bin:/bin", "HOME": str(self.root)}
        checker.output = self.root / "evidence"
        checker.output.mkdir()
        checker.initial = {"project-a/existing.txt": "a-before\n"}
        shadow = checker.home / "checkpoints" / checker.sid
        shadow.mkdir(parents=True)
        relative = self.fixture.name + "/project-a/existing.txt"
        (shadow / relative).parent.mkdir(parents=True)
        (shadow / relative).write_text("a-before\n")
        for args in (("init",), ("add", "."), ("-c", "user.name=Offline", "-c", "user.email=offline@example.invalid", "commit", "-m", "offline baseline")):
            subprocess.run(["git", "-C", str(shadow), *args], capture_output=True, check=True, env=checker.env)
        journal = shadow / ".git/metis-managed-paths.json"
        journal.write_text(json.dumps({relative: "after-fingerprint"}))
        proof = checker.checkpoint_proof(["project-a/existing.txt"], "offline")
        self.assertEqual(proof["unrelated_restore_paths"], 0)
        self.assertEqual(len(proof["baseline_blobs"]), 1)
        journal.write_text(json.dumps({relative: "after", "code/unrelated.go": "other"}))
        with self.assertRaises(AssertionError):
            checker.checkpoint_proof(["project-a/existing.txt"], "bad")
        journal.write_text(json.dumps({relative: "after"}))
        checker.initial["project-a/existing.txt"] = "unmatched original"
        with self.assertRaises(AssertionError):
            checker.checkpoint_proof(["project-a/existing.txt"], "bad-blob")

    def test_readonly_false_creation_persists_safe_gitignore_diagnostic_before_failure(self):
        if not shutil.which("git"):
            self.skipTest("git unavailable")
        checker = object.__new__(CheckpointMultiRepoLiveCheck)
        checker.home, checker.sid = self.root / "home", "offline-session"
        checker.env = {"PATH": "/usr/bin:/bin", "HOME": str(self.root)}
        checker.output, checker.report = self.root / "evidence", {}
        checker.output.mkdir()
        shadow = checker.home / "checkpoints" / checker.sid
        shadow.mkdir(parents=True)
        (shadow / ".gitignore").write_text("*.json\n")
        (shadow / "private-tracked.py").write_text("never export this content")
        (shadow / "private-ignored.json").write_text("never export this content either")
        for args in (("init",), ("add", "."), ("-c", "user.name=Offline", "-c", "user.email=offline@example.invalid", "commit", "-m", "baseline")):
            subprocess.run(["git", "-C", str(shadow), *args], capture_output=True, check=True, env=checker.env)
        # No managed journal is the normal no-change state.
        self.assertEqual(checker.readonly_checkpoint_proof()["path_count"], 0)
        journal = shadow / ".git/metis-managed-paths.json"
        journal.write_text(json.dumps({"private-tracked.py": "sha", "private-ignored.json": "sha"}))
        with self.assertRaisesRegex(AssertionError, "Read-only Bash incorrectly attributed"):
            checker.readonly_checkpoint_proof()
        proof = json.loads((checker.output / "readonly-checkpoint-diagnostic.json").read_text())
        self.assertEqual(proof["path_count"], 2)
        self.assertEqual(proof["missing_from_HEAD_count"], 1)
        self.assertEqual(proof["gitignored_count"], 1)
        self.assertEqual(proof["extension_counts"], {".json": 1, ".py": 1})
        self.assertNotIn("private-", json.dumps(proof))
        self.assertNotIn("never export", json.dumps(proof))
        self.assertEqual(checker.report["readonly_checkpoint_diagnostic"], proof)


if __name__ == "__main__":
    unittest.main()
