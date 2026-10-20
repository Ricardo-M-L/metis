#!/usr/bin/env python3
"""Opt-in real-CLI workflow fixture acceptance for the shared live checker.

The caller owns one fresh CLI, original selected route, private authentication,
process cleanup and final evidence. This helper neither builds an executable nor
selects a model. --self-test performs only local Python fixture/oracle checks.
"""
from __future__ import annotations

import argparse
import ast
import hashlib
import json
from pathlib import Path
import re
import subprocess
import tempfile
from types import SimpleNamespace
from typing import Any
import uuid

from persistent_agent_live_check import atomic_json, digest, require


def sha(text: str) -> str:
    return hashlib.sha256(text.encode()).hexdigest()


def canonical_json_sha(value: Any) -> str:
    """Match learning.EvaluationFixtureDigest's semantic Go JSON encoding."""
    text = json.dumps(value, ensure_ascii=False, sort_keys=True, separators=(",", ":"))
    for before, after in (("<", "\\u003c"), (">", "\\u003e"), ("&", "\\u0026"),
                          ("\u2028", "\\u2028"), ("\u2029", "\\u2029")):
        text = text.replace(before, after)
    return sha(text)


def fixture_inputs(nonce: str) -> tuple[dict[str, Any], dict[str, Any], str, str, dict[str, Any]]:
    initial = "def multiply(a, b):\n    return a + b\n"
    repaired = "def multiply(a, b):\n    return a * b\n"
    expected = {"answer": 323, "fixture": nonce, "test_cases": 3}
    tests = '''import json, os, sys, unittest
sys.path.insert(0, os.getcwd())
from calculator import multiply
class CalculatorTests(unittest.TestCase):
    def test_positive(self): self.assertEqual(multiply(17, 19), 323)
    def test_zero(self): self.assertEqual(multiply(7, 0), 0)
    def test_negative(self): self.assertEqual(multiply(-4, 5), -20)
result = unittest.TextTestRunner().run(unittest.defaultTestLoader.loadTestsFromTestCase(CalculatorTests))
if not result.wasSuccessful(): sys.exit(1)
with open("result.json", "w") as f:
    json.dump({"answer": multiply(17, 19), "fixture": FIXTURE_NONCE, "test_cases": result.testsRun}, f)
'''.replace("FIXTURE_NONCE", repr(nonce))
    fixture = {"schema_version": 1, "cases": {"repair-python-multiply": {
        "files": {"calculator.py": initial}, "writable_files": ["calculator.py"],
        "test_python": tests, "expected_artifacts": {"result.json": expected}}}}
    suite = {"name": "isolated-python-repair-" + nonce, "cases": [{"id": "repair-python-multiply",
        "prompt": "Execute the selected named workflow exactly once. Its final fixture-test invokes immutable host tests. "
                  "If tests fail, preserve the failure and finish without retrying or changing the workflow.",
        "expected_json": {"tests_passed": True}}], "max_tokens": 100000, "max_duration_seconds": 600}
    return fixture, suite, initial, repaired, expected


def assert_fixture_evaluation(checker: Any, value: dict[str, Any], name: str,
                              candidate: str, baseline: str, fixture: dict[str, Any],
                              initial: str, repaired: str, expected: dict[str, Any]) -> dict[str, Any]:
    require(value.get("kind") == "workflow" and value.get("skill_name") == name and value.get("passed") is True and
        value.get("inputs_stable") is True and value.get("binary_sha256") == checker.input_sha and
        value.get("provider") == checker.provider and value.get("model") == checker.model and
        value.get("candidate_version") == candidate and value.get("baseline_version") == baseline,
        "Code fixture evaluation changed CLI, route, recipe identity or passing gate")
    fixture_sha = canonical_json_sha(fixture)
    expected_test_sha = sha(fixture["cases"]["repair-python-multiply"]["test_python"])
    snapshots = {}
    for label, version, source, passed in (("baseline", baseline, initial, False), ("candidate", candidate, repaired, True)):
        group = value.get(label, {})
        require(group.get("version") == version and len(group.get("cases", [])) == 1 and
            group.get("passed_cases") == int(passed) and group.get("score") == float(passed),
            "Code fixture lost its failing baseline / repaired candidate comparison")
        case = group["cases"][0]
        require(case.get("exit_code") == 0 and not case.get("error") and case.get("workflow_invoked") is True and
            case.get("usage_reported") is True and case.get("model_requests", 0) > 0 and case.get("passed") is passed,
            "Code fixture query did not complete with actual model usage and a concrete Workflow invocation")
        usage = case.get("usage", {})
        require(set(usage) == {"input", "output", "cache_read", "cache_create"} and
            all(type(amount) is int and amount >= 0 for amount in usage.values()) and sum(usage.values()) > 0,
            "Code fixture has incomplete or fabricated usage")
        evidence = case.get("evidence", {})
        require(evidence.get("provider") == checker.provider and evidence.get("model") == checker.model and
            evidence.get("session_id") and evidence.get("manifest_sha256") == version and
            re.fullmatch(r"[a-f0-9]{64}", evidence.get("trace_sha256", "")) and
            len(evidence.get("token_event_ids", [])) == case["model_requests"] and
            evidence.get("fixture_sha256") == fixture_sha,
            "Code fixture omitted its exact real-query / frozen fixture binding")
        calls = evidence.get("workflow_invocations", [])
        require(len(calls) == 1, "Code fixture did not invoke Workflow exactly once")
        call = calls[0]
        arguments = call.get("arguments", {})
        if isinstance(arguments, str):
            arguments = json.loads(arguments)
        require(arguments.get("operation") == "run_named" and arguments.get("name") == name and
            call.get("invocation_id") and call.get("call_id") and call.get("recipe_sha256") == version and
            call.get("result_sha256") == sha(call.get("result", "")),
            "Model-supplied prose replaced concrete named Workflow evidence")
        actual = json.loads(call["result"])
        require(actual.get("name") == name and actual.get("recipe_sha256") == version and actual.get("steps"),
            "Workflow tool result lost its pinned recipe and actual steps")
        final = actual["steps"][-1]
        require(final.get("command") == "fixture-test" and
            (final.get("status"), final.get("exit_code")) == (("ok", 0) if passed else ("failed", 1)),
            "Code fixture did not run genuine passing/failing tests")
        report = evidence.get("host_checks_report", {})
        if isinstance(report, str):
            report = json.loads(report)
        require(report and evidence.get("host_checks_sha256") == canonical_json_sha(report) and
            evidence.get("host_checks_passed", False) is passed and report.get("fixture_sha256") == fixture_sha and
            report.get("tests_sha256") == expected_test_sha and report.get("source_sha256", {}).get("calculator.py") == sha(source) and
            report.get("tests_passed") is passed and report.get("artifacts_passed") is passed and
            report.get("test_exit_code") == (0 if passed else 1),
            "Independent host retest evidence is missing, altered or detached from actual source")
        if passed:
            require(not report.get("error") and json.loads(case["output"]) == {"tests_passed": True} and
                report.get("artifact_sha256", {}).get("result.json") == sha(json.dumps(expected)),
                "Host accepted a wrong business JSON artifact")
        else:
            require("AssertionError" in final.get("output", "") and "AssertionError" in report.get("error", ""),
                "Baseline failure was not a real Python assertion failure")
        snapshots[label] = {"passed": passed, "source_sha256": sha(source), "model_requests": case["model_requests"],
            "usage": usage, "host_checks": report, "workflow_call": call}
    return {"fixture_sha256": fixture_sha, "test_cases_per_run": 3, "versions": snapshots}


def check_workflow_fixture(checker: Any) -> dict[str, Any]:
    """Use only the caller's disposable CLI and original selected provider."""
    nonce = uuid.uuid4().hex[:12]
    name = "live-code-repair-" + nonce
    fixture, suite, initial, repaired, expected = fixture_inputs(nonce)
    suite["max_duration_seconds"] = int(min(checker.args.query_timeout, 600))
    recipes = checker.home / "workflows"
    recipes.mkdir(mode=0o700, exist_ok=True)
    baseline_recipe = {"name": name, "steps": [{"name": "run-host-tests", "command": "fixture-test"}]}
    candidate_recipe = {"name": name, "steps": [
        {"name": "repair-python-source", "command": "printf '%s\\n' 'def multiply(a, b):' '    return a * b' > calculator.py"},
        {"name": "run-host-tests", "command": "fixture-test"}]}
    baseline_path = recipes / (name + ".json")
    atomic_json(baseline_path, baseline_recipe)
    baseline = digest(baseline_path)
    candidate_path, fixture_path, suite_path = (checker.root / (name + suffix) for suffix in
        ("-candidate.json", "-fixture.json", "-suite.json"))
    atomic_json(candidate_path, candidate_recipe)
    atomic_json(fixture_path, fixture)
    atomic_json(suite_path, suite)
    candidate, _ = checker.command(["learning", "stage", "--kind", "workflow", "--name", name,
        "--file", str(candidate_path)], "stage-isolated-code-repair-candidate")
    require(candidate.get("id") == digest(candidate_path) and candidate.get("parent") == baseline and digest(baseline_path) == baseline,
        "Staging code repair altered its active baseline or exact recipe content")
    policy, _ = checker.command(["learning", "configure", "--kind", "workflow", "--name", name,
        "--suite", str(suite_path), "--fixture", str(fixture_path), "--max-evaluations", "2",
        "--max-total-tokens", "400000", "--auto-promote"], "freeze-code-fixture-and-automatic-learning-policy")
    identity = policy.get("identity", {})
    require(policy.get("kind") == "workflow" and policy.get("name") == name and policy.get("suite") == suite and
        policy.get("workflow_fixture") == fixture and policy.get("auto_promote") is True and
        identity.get("binary_sha256") == checker.input_sha and identity.get("provider") == checker.provider and identity.get("model") == checker.model,
        "Code fixture watcher did not freeze host tests and unchanged CLI/provider/model")
    # Prove the watcher does not depend on mutable fixture or suite file paths.
    fixture_path.unlink()
    suite_path.unlink()
    evaluation, tick = checker.controller_evaluate("workflow", name, candidate["id"], "promoted",
        "real-model-automatic-code-fixture-evaluate-and-promote")
    require(evaluation.get("profile_sha256") == sha("workflow-code-fixture-v1\n" +
        identity["provider_config_sha256"] + "\n" + canonical_json_sha(fixture)),
        "Automatic evaluator identity omitted its frozen fixture authority")
    proof = assert_fixture_evaluation(checker, evaluation, name, candidate["id"], baseline,
        fixture, initial, repaired, expected)
    checker.controller_idle("workflow", name, "code-fixture-watch-idle-after-promotion")
    state, _ = checker.command(["learning", "list", "--kind", "workflow", "--name", name], "reopen-promoted-code-repair-version")
    require(state.get("active_version") == candidate["id"] and not state.get("suspended") and
        json.loads(state.get("versions", {}).get(candidate["id"], {}).get("content", "null")) == candidate_recipe,
        "Automatic watcher did not persist the exact host-tested code repair")
    checker.report.setdefault("workflow_fixture_evaluations", []).append(evaluation)
    checker.record("automatic workflow learning repairs real Python code and passes immutable tests plus business artifacts",
        artifact_name=name, baseline=baseline, candidate=candidate["id"], fixed_suite=suite,
        frozen_fixture_survived_source_file_removal=True, policy=policy, automatic_tick=tick, **proof,
        boundary="One isolated system-Python/stdlib repair fixture with 3 tests per execution; no arbitrary Go builds or external dependencies were tested.")
    return proof


def self_test() -> dict[str, Any]:
    fixture, suite, initial, repaired, expected = fixture_inputs("offline-only")
    spec = fixture["cases"][suite["cases"][0]["id"]]
    ast.parse(initial)
    ast.parse(repaired)
    ast.parse(spec["test_python"])
    with tempfile.TemporaryDirectory(prefix="metis-code-fixture-oracle-") as temporary:
        root = Path(temporary)
        tests = root / "checks.py"
        source = root / "calculator.py"
        tests.write_text(spec["test_python"])
        source.write_text(initial)
        failed = subprocess.run(["/usr/bin/python3", "-I", "-B", str(tests)], cwd=root,
            capture_output=True, text=True, timeout=10)
        require(failed.returncode == 1 and "AssertionError" in failed.stderr, "Offline buggy code did not fail real tests")
        source.write_text(repaired)
        passed = subprocess.run(["/usr/bin/python3", "-I", "-B", str(tests)], cwd=root,
            capture_output=True, text=True, timeout=10)
        require(passed.returncode == 0 and "Ran 3 tests" in passed.stderr and json.loads((root / "result.json").read_text()) == expected,
            "Offline repaired source / host artifact oracle failed")
        require(digest(root / "result.json") == sha(json.dumps(expected)), "Artifact byte hash expectation is wrong")
    _record_contract_self_test()
    tree = ast.parse(Path(__file__).read_text())
    conflicts = [node.lineno for node in ast.walk(tree) if isinstance(node, ast.Call) and
        isinstance(node.func, ast.Attribute) and node.func.attr == "record" and node.args and
        any(keyword.arg == "name" for keyword in node.keywords)]
    require(not conflicts, "Helper record(name, **fields) keyword collision at lines " + str(conflicts))
    return {"passed": True, "offline_only": True, "real_provider_verified": False,
        "checks": ["fixture AST", "3 real failing Python tests", "3 real passing Python tests", "JSON artifact and hash",
            "complete helper record signature contract", "no duplicate record keywords"]}


def _record_contract_self_test() -> None:
    """Exercise the complete helper with a signature-compatible offline double.

    Only local Python tests run here. All model/CLI fields below are explicit
    synthetic unit-test inputs; no provider claim or live report is produced.
    """
    class OfflineChecker:
        def __init__(self, root: Path):
            self.root, self.home = root, root / "home"
            self.home.mkdir()
            self.args = SimpleNamespace(query_timeout=60, case_timeout=600)
            self.provider, self.model, self.input_sha = "offline-test-provider", "offline-test-model", "a" * 64
            self.report: dict[str, Any] = {}
            self.records: list[dict[str, Any]] = []
            self.idle_checks = 0

        def command(self, arguments: list[str], _label: str, **_options: Any):
            def flag(name: str) -> str:
                return arguments[arguments.index(name) + 1]
            if arguments[:2] == ["learning", "stage"]:
                self.name = flag("--name")
                candidate_file = Path(flag("--file"))
                self.content = candidate_file.read_text()
                self.recipe = json.loads(self.content)
                self.candidate = digest(candidate_file)
                self.baseline = digest(self.home / "workflows" / (self.name + ".json"))
                return {"id": self.candidate, "parent": self.baseline}, {}
            if arguments[:2] == ["learning", "configure"]:
                self.fixture = json.loads(Path(flag("--fixture")).read_text())
                self.suite = json.loads(Path(flag("--suite")).read_text())
                self.policy = {"kind": "workflow", "name": self.name, "suite": self.suite,
                    "workflow_fixture": self.fixture, "auto_promote": True,
                    "identity": {"binary_sha256": self.input_sha, "provider": self.provider, "model": self.model,
                        "provider_config_sha256": "b" * 64}}
                return self.policy, {}
            if arguments[:2] == ["learning", "list"]:
                return {"active_version": self.candidate, "versions": {self.candidate: {"content": self.content}}}, {}
            raise AssertionError("Unexpected offline checker command: " + str(arguments))

        def controller_evaluate(self, _kind: str, _name: str, _candidate: str, _status: str, _label: str):
            spec = self.fixture["cases"]["repair-python-multiply"]
            initial = spec["files"]["calculator.py"]
            repaired = "def multiply(a, b):\n    return a * b\n"
            fixture_sha = canonical_json_sha(self.fixture)
            evaluation = {"kind": "workflow", "skill_name": self.name, "passed": True, "inputs_stable": True,
                "binary_sha256": self.input_sha, "provider": self.provider, "model": self.model,
                "candidate_version": self.candidate, "baseline_version": self.baseline,
                "profile_sha256": sha("workflow-code-fixture-v1\n" + "b" * 64 + "\n" + fixture_sha)}
            for label, version, source in (("baseline", self.baseline, initial), ("candidate", self.candidate, repaired)):
                workspace = self.root / label
                workspace.mkdir()
                tests = workspace / "checks.py"
                tests.write_text(spec["test_python"])
                (workspace / "calculator.py").write_text(source)
                process = subprocess.run(["/usr/bin/python3", "-I", "-B", str(tests)], cwd=workspace,
                    capture_output=True, text=True, timeout=10)
                passed = label == "candidate"
                require(process.returncode == (0 if passed else 1), "Offline contract fixture test result changed")
                output = '{"tests_passed":true}\n' if passed else process.stderr
                steps = [{"name": "run-host-tests", "command": "fixture-test", "exit_code": process.returncode,
                    "status": "ok" if passed else "failed", "output": output}]
                if passed:
                    steps.insert(0, {**self.recipe["steps"][0], "exit_code": 0, "status": "ok", "output": ""})
                result = json.dumps({"name": self.name, "recipe_sha256": version, "steps": steps}, separators=(",", ":"))
                host = {"fixture_sha256": fixture_sha, "tests_sha256": sha(spec["test_python"]),
                    "source_sha256": {"calculator.py": sha(source)}, "artifact_sha256": {},
                    "tests_passed": passed, "test_exit_code": process.returncode, "artifacts_passed": passed}
                if passed:
                    host["artifact_sha256"]["result.json"] = digest(workspace / "result.json")
                else:
                    host["error"] = process.stderr
                evidence = {"provider": self.provider, "model": self.model, "session_id": "offline-unit-" + label,
                    "manifest_sha256": version, "trace_sha256": "c" * 64, "token_event_ids": ["offline-unit-token-" + label],
                    "fixture_sha256": fixture_sha, "host_checks_report": host, "host_checks_sha256": canonical_json_sha(host),
                    "host_checks_passed": passed, "workflow_invocations": [{"invocation_id": "offline-unit-invocation",
                        "call_id": "offline-unit-call", "recipe_sha256": version,
                        "arguments": {"operation": "run_named", "name": self.name}, "result": result, "result_sha256": sha(result)}]}
                case = {"exit_code": 0, "workflow_invoked": True, "usage_reported": True, "model_requests": 1,
                    "passed": passed, "usage": {"input": 1, "output": 1, "cache_read": 0, "cache_create": 0},
                    "evidence": evidence, "output": output}
                evaluation[label] = {"version": version, "cases": [case], "passed_cases": int(passed), "score": float(passed)}
            return evaluation, {"offline_synthetic_contract_input": True}

        def controller_idle(self, *_args: Any) -> None:
            self.idle_checks += 1

        # Exactly the production base checker signature: a duplicate keyword
        # name must fail here even when every model/fixture assertion succeeds.
        def record(self, name: str, **fields: Any) -> None:
            self.records.append({"name": name, **fields})

    with tempfile.TemporaryDirectory(prefix="metis-record-contract-") as temporary:
        checker = OfflineChecker(Path(temporary))
        check_workflow_fixture(checker)
        require(checker.idle_checks == 1 and len(checker.records) == 1 and
            checker.records[0].get("artifact_name") == checker.name,
            "Complete fixture helper did not record its artifact with the production method signature")


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--self-test", action="store_true", required=True)
    parser.parse_args()
    print(json.dumps(self_test(), ensure_ascii=False, indent=2))
