#!/usr/bin/env python3
"""Real CLI acceptance for parent-owned child continuation.

The caller provides a frozen disposable CLI and an already isolated API-key
profile. No model response is mocked, rewritten or forced through a proxy.
Prompts describe the required workflow; host assertions inspect actual tools,
child journals, opaque fixture contents and unchanged files. This harness never
uses an installed CLI, reads the user's default profile, installs an App or
rebuilds a failing executable. Both success and failure destroy its CLI copy,
copied credentials and (with required --destroy-input) the supplied test CLI.
"""

from __future__ import annotations

import argparse
import copy
import json
import os
from pathlib import Path
import re
import secrets
import shlex
import signal
import time
import tomllib
from typing import Any
import uuid

from model_context_live_check import digest
from persistent_agent_live_check import (PersistentAgentLiveCheck, final_assistant,
    history_snapshots, message_text, nested_trace_proof, tool_calls, trace_origins, trace_proof)
from project_coordinator_live_check import atomic_json, require, root_trace_identity


TOOLS = "Agent,SubAgentOutput,SubAgentList,Read,Write,Bash"


def validate_inputs(binary: Path, profile: Path) -> None:
    """Reject the user's installed executables and default credential store."""
    require(not binary.is_symlink() and binary.is_file(), "Supply a regular disposable test CLI, not a launcher/symlink")
    resolved = binary.expanduser().resolve()
    protected = (Path("/Applications"), Path("/usr"), Path("/opt/homebrew"), Path.home() / ".local")
    require(not any(resolved.is_relative_to(path) for path in protected), "Installed CLI paths cannot be acceptance inputs")
    require(profile.is_dir() and not profile.is_symlink(), "--profile-home must be an existing private test profile")
    require(profile.expanduser().resolve() != (Path.home() / ".metis").resolve(), "The user default Metis profile is not a disposable input")
    require((profile / "config.toml").is_file() and not (profile / "config.toml").is_symlink(), "Private profile config must be a regular file")


def agent_calls(events: list[dict[str, Any]]) -> list[dict[str, Any]]:
    """Match root Agent arguments/results, including pre-launch rejections."""
    nested, boundaries = trace_origins(events)
    anchors: dict[tuple[str, str, str, str], dict[str, Any]] = {}
    results = []
    seen = set()
    for index, row in enumerate(events):
        if row.get("tool_name") != "Agent":
            continue
        key = tuple(row.get(field, "") for field in ("session_id", "trace_invocation_id", "trace_call_id", "tool_use_id"))
        if row.get("kind") == "tool_args" and all(key) and root_trace_identity(row) and (key[0], key[1]) not in nested:
            try:
                args = json.loads(row.get("text", ""))
            except (TypeError, ValueError):
                continue
            if isinstance(args, dict):
                anchors[key] = args
        if row.get("kind") != "tool_result":
            continue
        boundary = boundaries.get(index)
        if boundary:
            args = boundary["arguments"]
            matched = (boundary["session_id"], boundary["root_invocation_id"], boundary["call_id"], boundary["tool_use_id"])
        else:
            args = anchors.get(key)
            matched = key
        if args is None or matched in seen:
            continue
        seen.add(matched)
        results.append({"arguments": args, "is_error": bool(row.get("is_error")), "result": row.get("text", ""),
            "session_id": matched[0], "trace_call_id": matched[2]})
    return results


def owned_child(directory: Path, parent: str, name: str, expected_id: str = "") -> tuple[Path, list[dict[str, Any]]]:
    matches = []
    for path in directory.glob("*.jsonl"):
        require(path.is_file() and not path.is_symlink(), "Child journal must be an owned regular file")
        rows = [json.loads(line) for line in path.read_text().splitlines() if line.strip()]
        header = rows[0].get("header", {}) if rows and rows[0].get("type") == "header" else {}
        if header.get("sub_agent_of") == parent and header.get("teammate_name") == name:
            require(header.get("id") == path.stem, "Child ID differs from its journal filename")
            matches.append((path, rows))
    require(len(matches) == 1, "No unique child bound to the real parent and Chinese name")
    path, rows = matches[0]
    require(not expected_id or path.stem == expected_id, "Continuation created a different child")
    return path, rows


def execution_record(rows: list[dict[str, Any]]) -> dict[str, Any]:
    values = [row.get("execution") for row in rows if row.get("type") == "execution"]
    require(values and all(isinstance(value, dict) for value in values), "Child has no durable execution descriptor")
    require(all(value == values[0] for value in values), "Continuation changed the child's saved execution identity")
    return values[0]


def field(value: dict[str, Any], *names: str) -> Any:
    return next((value[name] for name in names if name in value), None)


def assert_execution(value: dict[str, Any], *, provider: str, model: str, cwd: Path, effort: str) -> None:
    require(field(value, "provider", "providerName", "provider_name") == provider, "Child descriptor changed provider")
    require(value.get("model") == model, "Child descriptor changed model")
    require(field(value, "effort", "reasoningEffort", "reasoning_effort") == effort, "Child descriptor did not preserve reasoning effort")
    require(field(value, "workDir", "work_dir", "workingDirectory") == str(cwd), "Child descriptor did not preserve its original cwd")
    tools = field(value, "tools", "allowedTools", "allowed_tools")
    require(isinstance(tools, list) and set(tools) == {"Read", "Bash"}, "Child descriptor did not preserve its effective tool set")


def active_followup_recorded_before_terminal(rows: list[dict[str, Any]], marker: str) -> None:
    received = next((index for index, row in enumerate(rows) if marker in json.dumps(row, ensure_ascii=False)), None)
    terminal = next((index for index, row in enumerate(rows) if row.get("type") == "terminal"), None)
    require(received is not None and terminal is not None and received < terminal,
            "Follow-up was not durably accepted before the original child completed")


def accepted_continuation(events: list[dict[str, Any]], child_id: str) -> dict[str, Any] | None:
    return next((call for call in agent_calls(events) if call["arguments"].get("resume_from") == child_id and not call["is_error"]), None)


def assert_delegated_execution(parent: dict[str, Any], child: dict[str, Any], *, require_bash: bool) -> None:
    """Opaque values must be obtained by the child, never fetched by its parent."""
    require(not any(parent["tool_results"].get(tool) for tool in ("Read", "Write", "Bash")),
            "Parent fetched or changed fixture data instead of delegating actual file work")
    require(child["child_tool_results"].get("Read", 0) > 0, "Child did not actually Read the opaque fixture")
    if require_bash:
        require(child["child_tool_results"].get("Bash", 0) > 0, "Active child did not actually execute the bounded Bash gate")


def assert_plan_continuation_rejected(events: list[dict[str, Any]], identifier: str, *, child_before: bytes,
        child_after: bytes, protected_before: bytes, protected_after: bytes) -> list[dict[str, Any]]:
    calls = agent_calls(events)
    target = [call for call in calls if call["arguments"].get("resume_from") == identifier]
    require(target and all(call["is_error"] for call in calls), "Plan check requires an actual same-child Agent rejection")
    exact_reasons = {"denied: mode:plan",
        "plan mode cannot continue an implementation sub-agent; stop it and create a read-only planning child"}
    cold_reason = (f'resume_from={identifier} refused: saved child permission mode '
        r'"[^"]+" cannot be enforced under current parent mode "plan"; restore the original parent mode or start a new child')
    require(any(call["result"].strip() in exact_reasons or re.fullmatch(cold_reason, call["result"].strip()) for call in target),
        "Plan Agent rejection did not come from a known backend permission boundary")
    require(child_after == child_before, "Plan rejection mutated the original child journal")
    require(protected_after == protected_before, "Plan rejection changed the protected fixture")
    child = nested_trace_proof(events)
    require(not child["child_model_responses"] and not child["child_tool_results"], "Plan continuation actually executed a child")
    require(not tool_calls(events, "Write"), "Plan continuation successfully invoked Write")
    return target


def restore_selected_output_limit(text: str, provider: str, requested_limit: int) -> str:
    """Change only the parsed selected route's known shared-harness cap."""
    before = tomllib.loads(text)
    selected = before.get("provider", {}).get("custom", {}).get(provider, {})
    require(selected.get("max_tokens") == 2048, "Shared harness selected output cap changed")
    pattern = r'''(?m)^([ \t]*(?:max_tokens|"max_tokens"|'max_tokens')[ \t]*=[ \t]*)2048([ \t]*(?:#[^\r\n]*)?)$'''
    replaced, count = re.subn(pattern, lambda match: match[1] + str(requested_limit) + match[2], text)
    require(count == 1, "Shared harness output cap is not one unambiguous assignment")
    expected = copy.deepcopy(before)
    expected["provider"]["custom"][provider]["max_tokens"] = requested_limit
    require(tomllib.loads(replaced) == expected, "Output cap replacement changed another configuration field")
    return replaced


def live_child_id(directory: Path, parent: str, name: str) -> str:
    """Poll only complete first headers, never a concurrently appended tail."""
    matches = []
    for path in directory.glob("*.jsonl"):
        require(path.is_file() and not path.is_symlink(), "Child journal must be a regular owned file")
        with path.open("rb") as source:
            first = source.readline(64 << 10)
        if not first.endswith(b"\n"):
            continue
        row = json.loads(first)
        header = row.get("header", {}) if row.get("type") == "header" else {}
        if header.get("sub_agent_of") == parent and header.get("teammate_name") == name:
            require(header.get("id") == path.stem, "Live child header ID differs from its filename")
            matches.append(path.stem)
    require(len(matches) <= 1, "Parent spawned more than one child with the expected identity")
    return matches[0] if matches else ""


class ParentOwnedSubagentLiveCheck(PersistentAgentLiveCheck):
    def __init__(self, args: argparse.Namespace) -> None:
        validate_inputs(args.binary.expanduser(), args.profile_home.expanduser())
        super().__init__(args)
        self.report["boundary"] = "Finite real-model CLI checks of parent-owned live input, same-ID cold resume, original execution settings and rejection boundaries. No Desktop/native UI, process-kill recovery or long-running soak is claimed."
        self.report.update({"model_output_rewritten": False, "tool_response_forcing": False,
            "prompts_prescribe_workflow": True, "natural_delegation_without_tool_argument_JSON": True})

    def record(self, name: str, **fields: Any) -> None:
        # The inherited loader temporarily caps output at 2048. Record the
        # isolated route only after this suite has restored the caller's cap.
        if getattr(self, "_loading_private_route", False) and name == "original selected route isolated with a private credential and fixed model":
            return
        super().record(name, **fields)

    def load_route(self) -> None:
        # Reuse the audited minimal credential/config copy, but make its source
        # an explicit caller-created private profile, never ~/.metis.
        previous = os.environ.get("METIS_HOME")
        supplied_home = self.args.profile_home.expanduser().resolve()
        supplied = tomllib.loads((supplied_home / "config.toml").read_text())
        provider = supplied.get("provider", {}).get("default", "")
        requested_limit = supplied.get("provider", {}).get("custom", {}).get(provider, {}).get("max_tokens", 8192)
        require(isinstance(requested_limit, int) and requested_limit >= 4096, "High-effort live profile needs max_tokens >= 4096")
        os.environ["METIS_HOME"] = str(supplied_home)
        self._loading_private_route = True
        try:
            super().load_route()
        finally:
            self._loading_private_route = False
            if previous is None:
                os.environ.pop("METIS_HOME", None)
            else:
                os.environ["METIS_HOME"] = previous
        private_config = self.home / "config.toml"
        text = private_config.read_text()
        private_config.write_text(restore_selected_output_limit(text, provider, requested_limit))
        self.settings["max_tokens"] = requested_limit
        self.report["original_route"]["private_parameter_overrides"]["max_tokens"] = requested_limit
        with private_config.open("a") as target:
            for tool in ("SubAgentOutput", "SubAgentList"):
                target.write('\n[[permission.allow]]\ntool = "' + tool + '"\n')
        self.record("original selected route isolated with a private credential and fixed model", route=self.report["original_route"])

    def query(self, sid: str, prompt: str, label: str, *, resume: bool = False, cwd: Path | None = None,
              mode: str = "dontAsk", effort: str = "high", expect_rejection: bool = False,
              active_gate: Path | None = None, child_name: str = "") -> tuple[list[dict[str, Any]], dict[str, Any]]:
        before = self.trace_snapshot()
        previous = self.workspace
        self.workspace = cwd or previous
        try:
            args = ["run", "--bare", "--mode", mode, "--effort", effort, "--tools", TOOLS,
                "--max-iter", "18", "--resume" if resume else "--session-id", sid, prompt]
            if active_gate is None:
                _, command = self.command(args, label, json_output=False)
            else:
                require(not active_gate.exists(), "Active continuation fixture gate already exists")
                child = self.spawn(args, label)
                deadline = time.monotonic() + min(self.args.query_timeout, 120)
                accepted = None
                while child[0].poll() is None and time.monotonic() < deadline:
                    identifier = live_child_id(self.home / "sessions/subagents", sid, child_name)
                    if identifier:
                        try:
                            accepted = accepted_continuation(self.trace_events(before), identifier)
                        except json.JSONDecodeError:
                            # A live trace reader can observe a producer's final
                            # partial write; durable evidence is parsed again
                            # strictly after the CLI has joined.
                            accepted = None
                        if accepted is not None:
                            active_gate.write_text("released after exact-child successful continuation receipt\n")
                            self.report["active_continuation_receipt"] = accepted
                            break
                    time.sleep(0.05)
                require(accepted is not None, "No successful exact-child continuation receipt arrived before the bounded fixture gate expired")
                _, command = self.finish(child, label, json_output=False)
        finally:
            self.workspace = previous
        rows = self.trace_events(before)
        proof = trace_proof(rows, self.provider, self.model)
        require(proof["model_responses"] > 0 and proof["usage"], label + " has no actual provider token receipt")
        require(agent_calls(rows), label + " did not actually invoke Agent")
        require(not proof["errors"] or (expect_rejection and all(error.get("tool") == "Agent" for error in proof["errors"])),
                label + " has unexpected provider/root tool errors: " + json.dumps(proof["errors"]))
        child_proof = nested_trace_proof(rows)
        if not expect_rejection:
            require(child_proof["child_model_responses"] > 0 and not child_proof["child_errors"], label + " has no successful actual child execution")
            assert_delegated_execution(proof, child_proof, require_bash=active_gate is not None)
        atomic_json(self.output / (label + "-trace-proof.json"), {"parent": proof, "children": child_proof, "Agent": agent_calls(rows)})
        return rows, command

    def persistent_jobs(self) -> None:
        parent = str(uuid.uuid4())
        name = "中文核对"
        followup_marker = "FOLLOWUP_" + secrets.token_hex(12)
        values = {"phase-one.txt": "ONE_" + secrets.token_hex(24), "phase-two.txt": "TWO_" + secrets.token_hex(24),
                  "phase-three.txt": "THREE_" + secrets.token_hex(24)}
        for filename, text in values.items():
            (self.workspace / filename).write_text(text + "\n")
        protected = self.workspace / "plan-protected.txt"
        protected.write_text("ORIGINAL\n")
        gate = self.workspace / ".active-child-release"
        gate_quoted = shlex.quote(str(gate))
        wait_command = f"i=0; while [ ! -f {gate_quoted} ] && [ \"$i\" -lt 1200 ]; do sleep 0.1; i=$((i+1)); done; test -f {gate_quoted}"
        prompt = (f"把文件核对委派给一个名为{name}的后台子代理，子代理工作目录是{self.workspace}，"
            f"继承当前角色，不额外切换预设，只给它 Read 和 Bash 两个工具。它先通过 Bash 执行这个最多等待120秒的测试命令：{wait_command}。"
            "测试宿主会释放这个gate，你和子代理都不要创建或修改它。等待命令成功后，完整读取 phase-one.txt 并逐字报告内容。"
            f"启动后拿到该子代理的 agent_id，立即向同一个仍在运行的子代理追加任务：{followup_marker}，"
            "先完成原来的 phase-one.txt 核对，再完整读取 phase-two.txt 并逐字报告内容，不要丢掉原任务。复用原子代理，不要另建代理，不要停止它。"
            "然后等待它完成这两个任务，汇总结果。你自己不要读取文件、执行命令或编造文件内容。")
        rows, command = self.query(parent, prompt, "01-live-parent-delegation-and-followup", active_gate=gate, child_name=name)
        child, entries = owned_child(self.home / "sessions/subagents", parent, name)
        identifier = child.stem
        calls = agent_calls(rows)
        require(any(not call["arguments"].get("resume_from") and not call["is_error"] for call in calls), "Initial child was not spawned by the real parent")
        require(any(call["arguments"].get("resume_from") == identifier and followup_marker in call["arguments"].get("prompt", "") and not call["is_error"] for call in calls),
                "Parent did not successfully direct the follow-up to the original child")
        active_followup_recorded_before_terminal(entries, followup_marker)
        history = history_snapshots(child)[-1]
        assistant = "\n".join(message_text(message) for message in history if message.get("role") == "assistant")
        require(values["phase-one.txt"] in assistant and values["phase-two.txt"] in assistant, "Original child did not return both opaque fixture values")
        execution = execution_record(entries)
        assert_execution(execution, provider=self.provider, model=self.model, cwd=self.workspace, effort="high")
        prefix = child.read_bytes()
        self.record("natural Chinese child delegation and live same-child follow-up", parent_id=parent, child_id=identifier,
            command=command, child_journal_sha256=digest(child), execution=execution, received_before_first_terminal=True)

        alternate = self.root / "alternate-parent-cwd"
        alternate.mkdir(mode=0o700)
        (alternate / "phase-three.txt").write_text("WRONG_CWD\n")
        prompt = (f"继续让原子代理 {identifier} 使用 Read 工具完整读取它原工作目录里的相对路径 phase-three.txt 并逐字报告内容，不要用 Bash 代读。"
            "通过原 agent_id 恢复它，不要新建子代理，不要覆盖它的目录、工具权限或思考档位，你自己不要读取文件。")
        rows, command = self.query(parent, prompt, "02-parent-cold-resume-original-child", resume=True, cwd=alternate, effort="low")
        resumed, entries = owned_child(self.home / "sessions/subagents", parent, name, identifier)
        require(resumed.read_bytes().startswith(prefix), "Cold resume rewrote existing child journal bytes")
        require(execution_record(entries) == execution, "Cold resume replaced the saved child configuration")
        require(any(call["arguments"].get("resume_from") == identifier and not call["is_error"] for call in agent_calls(rows)), "Second parent query did not resume its original child")
        require(values["phase-three.txt"] in final_assistant(history_snapshots(resumed)[-1]), "Cold child did not read from its original directory")
        self.record("same-parent cold resume preserves identity cwd effort tools and history", parent_id=parent, child_id=identifier,
            command=command, original_parent_effort="high", changed_parent_effort="low", child_journal_sha256=digest(resumed))

        before = resumed.read_bytes()
        prompt = (f"请实际尝试通过 Agent 的 resume_from 恢复子代理 {identifier}，给它追加“读取 phase-one.txt”。"
            "这是父子归属拒绝检查：即使预期拒绝也必须提交一次工具调用，由后端判定。拒绝后停止，不要新建代理或读取文件。")
        rows, command = self.query(str(uuid.uuid4()), prompt, "03-unrelated-parent-rejected", expect_rejection=True)
        rejected = [call for call in agent_calls(rows) if call["arguments"].get("resume_from") == identifier and call["is_error"]]
        require(rejected and any(re.search(r"parent|owner|归属|父", call["result"], re.I) for call in rejected), "No actual cross-parent Agent ownership rejection")
        require(resumed.read_bytes() == before, "Rejected foreign parent mutated the child journal")
        self.record("another parent cannot resume or rewrite this child", child_id=identifier, command=command, rejected_calls=rejected)

        before_plan, protected_before = resumed.read_bytes(), protected.read_bytes()
        prompt = (f"当前是计划模式。这是后端父子权限边界检查：请实际调用一次 Agent，通过 resume_from 恢复原子代理 {identifier}，"
            "只追加这个只读任务：使用原来的 Read 工具完整读取原工作目录的 phase-one.txt 并报告内容，不执行命令、不写文件。"
            "必须提交实际工具调用，由后端决定原实现子线程能否在计划模式继续；不要自行将预期拒绝当作实际结果。"
            "如果工具拒绝，返回后端的准确拒绝原因并停止；如果工具接受，也报告实际结果。"
            "不要新建子代理、不要由你自己读取文件、不要更换任何执行设置、不要退出计划模式或请求自动批准。")
        rows, command = self.query(parent, prompt, "04-parent-plan-cannot-upgrade-child", resume=True, mode="plan", expect_rejection=True)
        rejected = assert_plan_continuation_rejected(rows, identifier, child_before=before_plan, child_after=resumed.read_bytes(),
            protected_before=protected_before, protected_after=protected.read_bytes())
        child_proof = nested_trace_proof(rows)
        self.record("parent Plan mode cannot upgrade resumed child to write", child_id=identifier, command=command,
            protected_fixture_sha256=digest(protected), child_journal_sha256=digest(resumed),
            readonly_followup_backend_rejected=True, rejected_calls=rejected, child_execution=child_proof)

    def durable_handoffs(self) -> None:
        pass  # The inherited runner calls these; this focused suite owns four queries only.

    def learning_loop(self) -> None:
        pass

    def workflow_learning_loop(self) -> None:
        pass


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--repo", type=Path, default=Path(__file__).resolve().parents[2])
    parser.add_argument("--binary", type=Path, required=True)
    parser.add_argument("--expected-sha256", required=True)
    parser.add_argument("--source-freeze", type=Path, required=True)
    parser.add_argument("--profile-home", type=Path, required=True)
    parser.add_argument("--output", type=Path, required=True)
    parser.add_argument("--destroy-input", action="store_true", required=True)
    parser.add_argument("--query-timeout", type=float, default=360)
    parser.add_argument("--case-timeout", type=float, default=1800)
    args = parser.parse_args()
    require(re.fullmatch(r"[a-f0-9]{64}", args.expected_sha256), "Expected CLI digest must be exact SHA-256")
    require(0 < args.query_timeout <= 600 and 0 < args.case_timeout <= 3600, "Live timeouts must be bounded")
    os.umask(0o077)
    def interrupted(signum: int, _frame: Any) -> None:
        raise KeyboardInterrupt("Harness interrupted by " + signal.Signals(signum).name)
    def timed_out(_signum: int, _frame: Any) -> None:
        raise TimeoutError("Parent-owned acceptance exceeded its bounded deadline")
    signal.signal(signal.SIGINT, interrupted)
    signal.signal(signal.SIGTERM, interrupted)
    signal.signal(signal.SIGALRM, timed_out)
    return ParentOwnedSubagentLiveCheck(args).run()


if __name__ == "__main__":
    raise SystemExit(main())
