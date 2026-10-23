#!/usr/bin/env python3
"""Opt-in real CLI Computer Use acceptance over two owned disposable apps.

No model, credentials, GUI, TCC request or compilation is used by --self-test.
Live mode requires caller-built source-frozen CLI/helper binaries and destroys
both inputs on every outcome. It never substitutes the selected provider/model,
builds either product executable, screenshots other apps, or retries UI actions.
"""
from __future__ import annotations

import argparse
import ctypes
import json
import math
import os
from pathlib import Path
import plistlib
import re
import shutil
import signal
import stat
import subprocess
import sys
import time
import tomllib
from typing import Any
import uuid

from persistent_agent_live_check import PersistentAgentLiveCheck, atomic_json, digest, history_snapshots, require, toml_table, tool_calls, utc_now

PREFIX = "mcp__computer-use__"
BASE_TOOLS = ("activate_application", "screen_size", "native_ax_snapshot", "native_ax_set_value", "native_ax_press")
INPUT_TOOLS = ("left_click", "key", "type")
OUTCOME_SEPARATOR = "\n\nComputer Use execution evidence: "


class PreflightBlocked(AssertionError):
    pass


def native_json(text: str) -> dict[str, Any]:
    body = text.split(OUTCOME_SEPARATOR, 1)[0]
    value = json.loads(body)
    require(isinstance(value, dict), "Native result is not an object")
    return value


def identity(app: Any, fixture: dict[str, Any], instance: str | None = None) -> str:
    require(isinstance(app, dict) and app.get("pid") == fixture["pid"] and
            app.get("bundle_id") == fixture["bundle_id"] and app.get("name") == fixture["name"] and
            isinstance(app.get("instance_id"), str) and bool(app["instance_id"]),
            "Native result does not identify the exact owned fixture process")
    require(instance is None or app["instance_id"] == instance, "Fixture kernel process instance changed")
    return app["instance_id"]


def host_state(state: Any, fixture: dict[str, Any], marker: str, count: int, *, active: bool = False) -> None:
    require(isinstance(state, dict) and state.get("schema_version") == 1 and state.get("nonce") == fixture["nonce"] and
            state.get("pid") == fixture["pid"] and state.get("bundle_id") == fixture["bundle_id"] and
            state.get("name") == fixture["name"] and state.get("input") == marker and
            type(state.get("counter")) is int and state["counter"] == count and
            type(state.get("ticks")) is int and state["ticks"] > 0,
            "Independent native fixture state did not match the exact UI result")
    require(not active or state.get("active") is True, "Target fixture is not active after the query")


def permission_preflight(description: Any) -> None:
    require(isinstance(description, dict) and description.get("name") == "metis-cu" and
            description.get("protocolVersion") == 1 and description.get("platform") == "darwin",
            "Installed helper descriptor is incompatible")
    if description.get("permissions", {}).get("accessibility") != "granted":
        raise PreflightBlocked("Installed helper Accessibility permission is unavailable; no OS prompt or permission change was attempted")


def display_geometry(value: dict[str, Any]) -> dict[str, Any]:
    displays, active = value.get("displays"), value.get("active")
    require(isinstance(displays, list) and displays and type(active) is int and active >= 0,
            "Screen size omitted actual display geometry")
    indices = set()
    selected = None
    for display in displays:
        require(isinstance(display, dict) and all(type(display.get(field)) is int for field in ("index", "x", "y", "width", "height")) and
                display["index"] >= 0 and display["index"] not in indices and display["width"] > 0 and display["height"] > 0,
                "Screen size contains invalid logical display bounds")
        indices.add(display["index"])
        if display["index"] == active:
            selected = display
    require(selected is not None and value.get("display_width_px") == selected["width"] and
            value.get("display_height_px") == selected["height"] and value.get("display_origin_x") == selected["x"] and
            value.get("display_origin_y") == selected["y"] and "GLOBAL desktop logical pixels" in value.get("coord_space_note", ""),
            "Active display dimensions/origin disagree or omit the global logical coordinate contract")
    return {"active": active, "displays": displays, "active_bounds": selected, "screenshot_taken": False,
            "multi_monitor_workflow_tested": False}


def fixture_input_grants(source: str, fixtures: list[dict[str, Any]]) -> dict[str, str]:
    # Restrict built-in terminal/IDE click defaults as well as the configured
    # unknown-app fallback. Only these two disposable fixture names get input.
    table = re.search(r"var defaultTiers = map\[string\]AccessTier\{(.*?)\n\}", source, re.S)
    require(table is not None, "Cannot identify frozen helper built-in app policies")
    names = re.findall(r'"([^"\n]+)"\s*:\s*Tier(?:Read|Click|Full)\s*,', table[1])
    require(names and len(set(names)) == len(names), "Cannot bound built-in app policy restrictions")
    grants = {name: "read" for name in names}
    for fixture in fixtures:
        require(fixture["name"] not in grants, "Fixture name conflicts with a built-in app")
        grants[fixture["name"]] = "full"
    require(len(fixtures) == 2 and len([value for value in grants.values() if value == "full"]) == 2,
            "Exactly two owned fixtures may have input grants")
    return grants


def effective_private_session_parameters(config_text: str) -> dict[str, Any]:
    session = tomllib.loads(config_text).get("session", {})
    parameters = {key: session[key] for key in ("max_iterations", "auto_compact_threshold") if key in session}
    parameters["auto_compact_threshold_overridden"] = "auto_compact_threshold" in session
    return parameters


def fixture_focus_diagnostics(state: dict[str, Any], fixtures: list[dict[str, Any]]) -> dict[str, Any]:
    """Bounded read-only process metadata; never export foreign app content."""
    owned = {(fixture["pid"], fixture["bundle_id"]) for fixture in fixtures}
    raw = state.get("focus_events", [])
    require(isinstance(raw, list), "Invalid fixture focus diagnostic list")
    events, invalid = [], 0
    for row in raw[-64:]:
        if not isinstance(row, dict) or row.get("event") not in (
                "fixture_started", "fixture_activated", "fixture_deactivated", "workspace_activation"):
            invalid += 1
            continue
        uptime = row.get("uptime")
        if type(uptime) not in (int, float) or not math.isfinite(uptime) or uptime < 0:
            invalid += 1
            continue
        event = {"event": row["event"], "uptime": uptime}
        if type(row.get("fixture_active")) is bool:
            event["fixture_active"] = row["fixture_active"]
        for prefix in ("foreground", "activated"):
            pid, bundle = row.get(prefix + "_pid"), row.get(prefix + "_bundle_id")
            if type(pid) is int and pid >= 0 and isinstance(bundle, str) and len(bundle) <= 256:
                event[prefix + "_pid"], event[prefix + "_bundle_id"] = pid, bundle
                event[prefix + "_classification"] = ("owned_fixture" if (pid, bundle) in owned else
                    "nonowned_process" if pid > 0 else "no_foreground_application")
        events.append(event)
    dropped = state.get("focus_events_dropped", 0)
    return {"observation_only": True, "event_limit": 64, "events": events, "invalid_events_omitted": invalid,
            "earlier_events_dropped": dropped if type(dropped) is int and dropped >= 0 else None,
            "foreign_metadata_fields": ["pid", "bundle_id"], "foreign_window_content_recorded": False,
            "reactivation_performed": False}


def fixture_command_a_diagnostics(state: dict[str, Any]) -> dict[str, Any]:
    """Only the fixture's own bounded Command+A metadata, never typed text."""
    raw = state.get("command_a_events", [])
    require(isinstance(raw, list), "Invalid fixture Command+A diagnostic list")
    events = []
    for row in raw[-8:]:
        if not isinstance(row, dict) or type(row.get("keycode")) is not int or row["keycode"] != 0:
            continue
        flags = row.get("modifier_flags")
        if type(flags) is not int or flags < 0 or not flags & (1 << 20):
            continue
        event = {"keycode": 0, "modifier_flags": flags}
        for key in ("sequence", "selection_length_before", "selection_length_after"):
            if type(row.get(key)) is int and row[key] >= -1:
                event[key] = row[key]
        for key in ("uptime", "observed_after_uptime"):
            value = row.get(key)
            if type(value) in (int, float) and math.isfinite(value) and value >= 0:
                event[key] = value
        events.append(event)
    return {"local_to_owned_fixture_only": True, "event_limit": 8, "events": events,
            "typed_text_recorded": False, "event_modified_or_consumed": False}


def control(snapshot: dict[str, Any], name: str) -> dict[str, Any]:
    nodes = [node for node in snapshot.get("nodes", []) if node.get("name") == name]
    require(len(nodes) == 1 and nodes[0].get("enabled") is True and not nodes[0].get("secure") and
            bool(nodes[0].get("element_ref")), "Fresh fixture snapshot omitted a unique safe control")
    return nodes[0]


def hydrate_native_results(events: list[dict[str, Any]], sessions: dict[str, Path]) -> tuple[list[dict[str, Any]], list[dict[str, Any]]]:
    """Bind bounded trace previews to exact full persisted native tool results.

    Trace text is deliberately truncated by production. Its identities, usage,
    timing and ordering remain authoritative; full bytes come only from the
    matching model-emitted call and host tool-result pair in durable history.
    """
    pairs = {}
    transcripts = []
    for sid, path in sessions.items():
        source_sha = digest(path)
        records = [json.loads(line) for line in path.read_text().splitlines() if line]
        headers = [record["header"] for record in records if record.get("type") == "header"]
        require(headers and all(header.get("id") == sid for header in headers), "Full transcript session identity mismatch")
        histories = history_snapshots(path)
        require(histories, "Full transcript has no persisted history")
        calls, results = {}, {}
        ordinal = 0
        for message in histories[-1]:
            for block in message.get("content", []):
                ordinal += 1
                kind = block.get("type")
                require(kind != "image", "Fixture-only acceptance transcript contains an image")
                if kind not in ("tool_use", "tool_result"):
                    continue
                uid = block.get("tool_use_id")
                require(isinstance(uid, str) and uid, "Persisted tool block lacks an exact tool-use identity")
                if kind == "tool_use":
                    require(message.get("role") == "assistant" and uid not in calls, "Duplicate or non-model persisted tool call")
                    calls[uid] = {"name": block.get("name"), "arguments": block.get("input"),
                                  "trace_call_id": block.get("trace_call_id"), "call_order": ordinal}
                else:
                    require(message.get("role") in ("user", "tool") and uid in calls and uid not in results,
                            "Duplicate or orphan persisted tool result")
                    call = calls[uid]
                    require(not block.get("is_error") and call["trace_call_id"] and block.get("trace_call_id") == call["trace_call_id"] and
                            isinstance(block.get("content"), str) and ordinal > call["call_order"],
                            "Persisted tool result failed or disagrees with its call occurrence")
                    results[uid] = {**call, "text": block["content"], "result_order": ordinal}
        native = {uid: call for uid, call in calls.items() if isinstance(call.get("name"), str) and call["name"].startswith(PREFIX)}
        require(native and all(uid in results for uid in native), "Full transcript is missing a native tool result")
        for uid in native:
            pairs[(sid, uid)] = results[uid]
        require(digest(path) == source_sha, "Full transcript changed during oracle hydration")
        transcripts.append({"session_id": sid, "full_transcript_sha256": source_sha, "native_result_pairs": len(native)})
    hydrated = []
    pending, completed, invocations = set(), set(), {}
    trace_order = []
    for event in events:
        row = dict(event)
        if row.get("tool_name", "").startswith(PREFIX) and row.get("kind") in ("tool_args", "tool_start", "tool_result"):
            sid, uid = row.get("session_id"), row.get("tool_use_id")
            key = (sid, uid)
            require(key in pairs, "Native trace has no exact persisted session/tool-use pair")
            pair = pairs[key]
            invocation = row.get("trace_invocation_id")
            require(pair["name"] == row.get("tool_name") and pair["trace_call_id"] == row.get("trace_call_id") and invocation and
                    not row.get("subagent_of") and not row.get("trace_parent_invocation_id"),
                    "Native trace tool name, occurrence or root origin disagrees with full transcript")
            require(key not in invocations or invocations[key] == invocation, "Native trace call changed invocation ownership")
            invocations[key] = invocation
            if row["kind"] in ("tool_args", "tool_start"):
                require(json.loads(row.get("text", "")) == pair["arguments"] and key not in completed,
                        "Native trace arguments disagree or follow their result")
                if row["kind"] == "tool_args":
                    require(key not in pending, "Duplicate native trace arguments")
                    pending.add(key)
                    trace_order.append((sid, pair["call_order"]))
                else:
                    require(key in pending, "Native trace start preceded its arguments")
            else:
                require(key in pending and key not in completed and row.get("parent_id") == uid and not row.get("is_error"),
                        "Duplicate, orphan or failed native trace result")
                preview, full = row.get("text", ""), pair["text"]
                suffix = "...(truncated)"
                require(preview == full or (preview.endswith(suffix) and len(preview) > len(suffix) and
                        len(full) > len(preview[:-len(suffix)]) and full.startswith(preview[:-len(suffix)])),
                        "Native trace preview disagrees with the exact full tool result")
                row["text"] = full
                completed.add(key)
                trace_order.append((sid, pair["result_order"]))
        hydrated.append(row)
    require(completed == pending == set(pairs), "Native trace/full transcript tool occurrences are incomplete")
    last_order = {}
    for sid, ordinal in trace_order:
        require(ordinal > last_order.get(sid, -1), "Native trace/transcript call/result time ordering disagrees")
        last_order[sid] = ordinal
    for transcript in transcripts:
        transcript["hydrated_native_results"] = sum(1 for row in hydrated if row.get("session_id") == transcript["session_id"] and
            row.get("kind") == "tool_result" and row.get("tool_name", "").startswith(PREFIX))
    return hydrated, transcripts


def validate_query(events: list[dict[str, Any]], fixture: dict[str, Any], marker: str,
                   state: dict[str, Any], *, keyboard_mouse: bool = False) -> dict[str, Any]:
    """Validate paired native tool results, identity, fresh refs and host effects.

    Native press acknowledgement is not business success: both a later AX
    snapshot and the fixture's independent JSON must show exactly one increment.
    """
    names = (*BASE_TOOLS, *(INPUT_TOOLS if keyboard_mouse else ()))
    allowed = {PREFIX + name for name in names} | {"ToolSearch", "Skill"}
    require(all(row.get("tool_name") in allowed for row in events
                if row.get("kind") in ("tool_args", "tool_start", "tool_result")), "Query used a tool outside its fixture-only allowlist")
    require(not any(row.get("is_error") for row in events), "Native tool failed; actions must not be retried")
    # Pair results through the existing trace occurrence oracle, then restore
    # their result order. An orphan, duplicate, failed or prose-only call cannot
    # supply a successful mutation or observation.
    paired = {}
    for name in names:
        for call in tool_calls(events, PREFIX + name):
            key = (call["session_id"], call["invocation_id"], call["call_id"])
            require(key not in paired, "Duplicate native call identity")
            paired[key] = {"name": name, **call}
    ordered = []
    seen = set()
    for event in events:
        if event.get("kind") != "tool_result" or not event.get("tool_name", "").startswith(PREFIX):
            continue
        key = tuple(event.get(field, "") for field in ("session_id", "trace_invocation_id", "trace_call_id"))
        require(key in paired and key not in seen, "Orphan or duplicate native result")
        seen.add(key)
        ordered.append(paired[key])
    latest = None
    fresh = False
    instance = None
    snapshot_ids = set()
    counts = {name: 0 for name in names}
    geometry = None
    for call in ordered:
        name, args, result = call["name"], call["arguments"], call["result"]
        counts[name] += 1
        if name == "activate_application":
            require(counts[name] == 1 and args.get("pid") == fixture["pid"] and args.get("bundle_id") == fixture["bundle_id"] and
                    100 <= args.get("timeout_ms", 1500) <= 3000, "Activation did not target the exact fixture once")
            value = native_json(result)
            instance = identity(value.get("app"), fixture)
            require(value.get("dispatch") == "sent" and value.get("verification") == "passed" and
                    value.get("reason") == "frontmost_identity_matched", "Activation lacks verified frontmost readback")
            fresh = False
        elif name == "screen_size":
            require(instance is not None, "Display geometry preceded verified fixture activation")
            geometry = display_geometry(native_json(result))
        elif name == "native_ax_snapshot":
            require(instance is not None and 1 <= args.get("max_nodes", 100) <= 150 and
                    1 <= args.get("max_depth", 8) <= 12, "Unbounded AX snapshot or observation before activation")
            value = native_json(result)
            identity(value.get("app"), fixture, instance)
            sid = value.get("snapshot_id")
            require(sid and sid not in snapshot_ids and value.get("window_ref") and value.get("expires_at") and
                    value.get("truncated") is False and 0 < len(value.get("nodes", [])) <= 150,
                    "AX snapshot was reused, truncated or lacks freshness metadata")
            if not snapshot_ids:
                require(control(value, "Fixture plain input").get("value") == "fixture initial value" and
                        any(node.get("name") == "Fixture count 0" for node in value["nodes"]),
                        "First AX snapshot does not prove the initial fixture state")
            snapshot_ids.add(sid)
            latest, fresh = value, True
        else:
            require(instance is not None and latest is not None and fresh, "Mutation lacks a fresh completed owned snapshot")
            if name in ("native_ax_set_value", "native_ax_press"):
                require(args.get("snapshot_id") == latest["snapshot_id"], "Mutation used a consumed or stale snapshot")
                node = control(latest, "Fixture plain input" if name == "native_ax_set_value" else "Increment fixture counter")
                require(args.get("element_ref") == node["element_ref"], "Mutation used an unobserved element reference")
                value = native_json(result)
                require(value.get("dispatched") is True and not value.get("dispatch_unknown"), "AX mutation lacks a definite dispatch acknowledgement")
                if name == "native_ax_set_value":
                    require(not keyboard_mouse and counts[name] == 1 and args.get("value") == marker and
                            node.get("editable") is True and value.get("verified") is True,
                            "Input was repeated, wrong, not editable or not read back")
                else:
                    require(counts[name] == 1 and "AXPress" in node.get("actions", []), "Counter press was repeated or unsupported")
            elif name == "left_click":
                require(counts[name] == 1 and args.get("return_screenshot") is False, "Click repeated or requested an unbounded screenshot")
                bounds = control(latest, "Fixture plain input").get("bounds", {})
                low, high = bounds.get("Min", {}), bounds.get("Max", {})
                require(all(type(point) in (int, float) for point in (args.get("x"), args.get("y"), low.get("X"), low.get("Y"), high.get("X"), high.get("Y"))) and
                        low["X"] < args["x"] < high["X"] and low["Y"] < args["y"] < high["Y"],
                        "Click was not inside the freshly observed fixture input")
            elif name == "key":
                require(counts[name] == 1 and counts["left_click"] == 1 and args.get("combo") == "cmd+a" and
                        args.get("return_screenshot") is False, "Keyboard command was not the single safe select-all")
            elif name == "type":
                require(counts[name] == 1 and counts["key"] == 1 and args.get("text") == marker and
                        len(marker) < 80 and marker.isascii() and args.get("return_screenshot") is False,
                        "Typing repeated, changed marker or could enter the clipboard path")
            fresh = False
    require(instance and counts["activate_application"] == 1 and counts["screen_size"] >= 1 and
            counts["native_ax_snapshot"] >= 3 and counts["native_ax_press"] == 1 and latest and fresh,
            "Query lacks actual activation, geometry, mutations or final fresh verification")
    if keyboard_mouse:
        require(all(counts[name] == 1 for name in INPUT_TOOLS) and counts["native_ax_set_value"] == 0,
                "Keyboard/mouse proof is incomplete or was replaced by an AX write")
    else:
        require(counts["native_ax_set_value"] == 1, "Query did not execute the actual AX write")
    require(control(latest, "Fixture plain input").get("value") == marker and
            any(node.get("name") == "Fixture count 1" for node in latest["nodes"]),
            "Final AX snapshot does not prove the expected input and single increment")
    host_state(state, fixture, marker, 1, active=True)
    return {"exact_fixture": {key: fixture[key] for key in ("name", "pid", "bundle_id")},
            "kernel_instance_id": instance, "successful_native_calls": counts,
            "distinct_snapshots": len(snapshot_ids), "final_host_state": state,
            "keyboard_mouse": keyboard_mouse, "screenshots_taken": False, "display_geometry": geometry}


class CrossAppCheck(PersistentAgentLiveCheck):
    def __init__(self, args: argparse.Namespace) -> None:
        super().__init__(args)
        self.helper_source = args.helper_binary.expanduser().resolve()
        self.args.helper_repo = args.helper_repo.expanduser().resolve()
        self.helper_copy = self.root / "metis-cu"
        self.fixtures: list[dict[str, Any]] = []
        self.original_cu_paths = [Path.home() / ".metis-cu" / name for name in ("config.toml", "granted.json")]
        self.original_cu_hashes = {str(path): digest(path) if path.exists() else None for path in self.original_cu_paths}
        self.report["boundary"] = ("Two owned disposable macOS AppKit apps, exact PID/bundle/kernel-instance activation, "
            "fresh AX writes and independent fixture state. No third-party apps, real accounts, browser workflow, screenshots, "
            "OS permission changes, OS reboot, indefinite soak or native Linux/Windows execution. Keyboard/mouse only if explicitly requested.")

    def helper_freeze(self) -> None:
        snapshot = json.loads(self.args.helper_source_freeze.read_text())
        require(snapshot.get("files"), "Helper source freeze has no digests")
        changed = [name for name, sha in snapshot["files"].items()
                   if not (self.args.helper_repo / name).is_file() or digest(self.args.helper_repo / name) != sha]
        require(not changed, "Helper source changed: " + ", ".join(changed[:8]))
        current = digest(self.args.helper_source_freeze)
        require(current == getattr(self, "helper_freeze_sha", current), "Helper freeze manifest changed")
        self.helper_freeze_sha = current
        self.report["helper_source_freeze"] = {"sha256": current, "files_checked": len(snapshot["files"])}

    def prepare(self) -> None:
        require(sys.platform == "darwin", "Live cross-app acceptance requires native macOS")
        self.stage()
        self.helper_freeze()
        require(self.helper_source.is_file() and digest(self.helper_source) == self.args.helper_sha256,
                "Supplied helper does not match its frozen build hash")
        shutil.copyfile(self.helper_source, self.helper_copy)
        self.helper_copy.chmod(0o700)
        self.load_route()
        require(self.provider == self.args.expected_provider and self.model == self.args.expected_model,
                "Original selected provider/model differs; refusing substitution")
        self.report["helper_binary"] = {"input": str(self.helper_source), "sha256": digest(self.helper_copy), "recompiled_by_harness": False}
        allowed = ["ToolSearch", "Skill", *(PREFIX + name for name in (*BASE_TOOLS, *(INPUT_TOOLS if self.args.include_keyboard_mouse else ())))]
        text = toml_table(["provider"], {"default": self.provider}) + "\n" + toml_table(["provider", "custom", self.provider], self.settings)
        text += "\n" + toml_table(["session"], {"dir": str(self.home / "sessions"), "skill_dir": str(self.home / "skills"), "max_iterations": 24})
        text += "\n" + toml_table(["tools"], {"allowed": allowed})
        text += '\n[tools.bash.sandbox]\nmode = "permissions"\nnetwork = "block"\n\n[permission]\nmode = "bypassPermissions"\n'
        (self.home / "config.toml").write_text(text)
        (self.home / "config.toml").chmod(0o600)
        effective = effective_private_session_parameters((self.home / "config.toml").read_text())
        self.report["effective_private_session_parameters"] = effective
        self.report["original_route"]["private_parameter_overrides"] = {"max_tokens": self.settings["max_tokens"],
            **{key: value for key, value in effective.items() if key != "auto_compact_threshold_overridden"}}
        self.record("effective Computer Use private session configuration supersedes initial route-isolation summary",
            effective_private_session_parameters=effective, inherited_route_isolation_parameter_summary_superseded=True)
        installed, _ = self.command(["cu", "install", "--from", str(self.helper_copy), "--json"], "install-private-frozen-helper")
        require(installed.get("installed") is True and installed.get("path") and digest(Path(installed["path"])) == self.args.helper_sha256,
                "Managed installation did not preserve the exact helper bytes")
        self.installed_helper = Path(installed["path"])
        preflight = subprocess.run([str(self.installed_helper), "--describe", "--json"], cwd=self.workspace,
            env=self.env, capture_output=True, text=True, timeout=20, check=True)
        description = json.loads(preflight.stdout)
        atomic_json(self.output / "native-permission-preflight.json", description)
        self.report["native_permission_preflight"] = description
        try:
            permission_preflight(description)
        except PreflightBlocked:
            self.report["blocked"] = True
            raise
        self.record("installed exact helper and read-only native permission preflight", helper_sha256=self.args.helper_sha256,
                    accessibility="granted", screen_recording_required=False)

    def build_fixtures(self) -> None:
        swift = shutil.which("swiftc")
        require(swift, "Native Swift compiler is unavailable")
        source = Path(__file__).with_name("native-cross-app-fixture.swift")
        compiled = self.root / "fixture-program"
        result = subprocess.run([swift, str(source), "-o", str(compiled)], cwd=self.workspace, env=self.env,
                                capture_output=True, text=True, timeout=120)
        require(result.returncode == 0, "Disposable Swift fixture compilation failed: " + result.stderr[-1500:])
        for letter in ("A", "B"):
            nonce = uuid.uuid4().hex[:10]
            name, bundle = "Metis CU Cross" + letter + " " + nonce, "dev.metis.crossapp." + letter.lower() + nonce
            app = self.root / (name + ".app")
            executable = app / "Contents" / "MacOS" / name
            executable.parent.mkdir(parents=True, mode=0o700)
            shutil.copyfile(compiled, executable)
            executable.chmod(0o700)
            info = {"CFBundleIdentifier": bundle, "CFBundleName": name, "CFBundleDisplayName": name,
                    "CFBundleExecutable": name, "CFBundlePackageType": "APPL", "NSHighResolutionCapable": True}
            (app / "Contents" / "Info.plist").write_bytes(plistlib.dumps(info))
            subprocess.run(["/usr/bin/codesign", "--force", "--sign", "-", str(app)], capture_output=True, check=True, timeout=20)
            state = self.root / ("fixture-" + letter + "-state.json")
            process = subprocess.Popen([str(executable), str(state), nonce], cwd=self.workspace, env=self.env,
                stdin=subprocess.DEVNULL, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, start_new_session=True)
            self.processes.append(process)
            fixture = {"name": name, "bundle_id": bundle, "pid": process.pid, "nonce": nonce, "state_path": state,
                       "app_path": app, "executable_path": executable}
            self.fixtures.append(fixture)
            self.wait_state(fixture, "fixture initial value", 0)
        compiled.unlink()
        grants = self.user_home / ".metis-cu" / "granted.json"
        grants.parent.mkdir(mode=0o700)
        restrictions = fixture_input_grants((self.args.helper_repo / "pkg/platform/platform_darwin_app.go").read_text(), self.fixtures)
        atomic_json(grants, restrictions)
        private_config = grants.with_name("config.toml")
        private_config.write_text('[gate]\ndefault_app_tier = "read"\nhost_terminal_tier = "disabled"\n')
        private_config.chmod(0o600)
        self.report["private_input_policy"] = {"full_input_apps": [fixture["name"] for fixture in self.fixtures],
            "built_in_app_read_restrictions": len(restrictions) - 2, "unknown_app_fallback": "read", "host_terminal_override": "disabled",
            "private_grants_sha256": digest(grants), "private_config_sha256": digest(private_config)}
        (self.home / "mcp.toml").write_text('[[servers]]\nname = "computer-use"\ncommand = "@metis/computer-use"\ndisabled = false\n' +
            "enabled_tools = " + json.dumps([*BASE_TOOLS, *(INPUT_TOOLS if self.args.include_keyboard_mouse else ())]) + "\n")
        (self.home / "mcp.toml").chmod(0o600)
        self.record("two owned disposable native apps ready with fixture-only grants",
                    fixtures=[{key: value[key] for key in ("name", "pid", "bundle_id")} for value in self.fixtures])

    @staticmethod
    def wait_state(fixture: dict[str, Any], marker: str, count: int, *, active: bool = False) -> dict[str, Any]:
        deadline = time.monotonic() + 10
        while time.monotonic() < deadline:
            try:
                state = json.loads(fixture["state_path"].read_text())
                host_state(state, fixture, marker, count, active=active)
                return state
            except (OSError, ValueError, AssertionError):
                time.sleep(0.05)
        raise AssertionError("Owned fixture did not publish the required real UI state before its deadline")

    def query_fixture(self, fixture: dict[str, Any], letter: str, *, keyboard_mouse: bool = False) -> None:
        marker = "metis-cross-" + letter.lower() + "-" + uuid.uuid4().hex[:12]
        strategy = (f"From the current plain-input node bounds click its center once with return_screenshot=false. "
            f"Obtain a fresh snapshot, then key combo cmd+a once with return_screenshot=false. Obtain a fresh snapshot, "
            f"then type exactly {json.dumps(marker)} once with return_screenshot=false. This short ASCII marker must not use clipboard."
            if keyboard_mouse else f"Use native_ax_set_value exactly once on the fresh plain-input element_ref to set value {json.dumps(marker)}.")
        prompt = (f"Authorized owned disposable native acceptance. Only target {fixture['name']}, PID {fixture['pid']}, "
            f"bundle_id {fixture['bundle_id']}. Discover exact schemas using ToolSearch/Skill. First call activate_application with "
            f"this exact PID and bundle_id, timeout_ms=1500. Require sent/passed and exact name/bundle/PID/kernel instance readback. "
            f"Then call screen_size once; no screen capture. Obtain native_ax_snapshot(max_nodes=100,max_depth=8), verify exact "
            f"app identity and record initial Fixture count 0. {strategy} After every mutation obtain a NEW native_ax_snapshot before "
            f"any next action. Refs expire after 30 seconds and are consumed. Once the plain input equals the marker, from a fresh "
            f"snapshot call native_ax_press once on Increment fixture counter. A sent/unknown acknowledgement is delivered: "
            f"do not repeat it. Finally obtain another fresh snapshot proving plain input={json.dumps(marker)} and Fixture count 1. "
            f"Stop on wrong identity, missing controls or uncertain action; never retry mutations. Do not open files, use Bash, "
            f"screenshots, clipboard, browser, request_access, another app, batching, or any other tool. Report observed state briefly.")
        before = self.trace_snapshot()
        _, command = self.command(["run", "--no-auth-wizard", "--no-markdown", prompt], "real-cross-app-" + letter,
                                  json_output=False)
        required = ("activate_application", "screen_size", "native_ax_snapshot", "native_ax_press", *(INPUT_TOOLS if keyboard_mouse else ("native_ax_set_value",)))
        proof, rows = self.real_proof(before, "real-cross-app-" + letter, required_tools=tuple(PREFIX + name for name in required))
        paths = {}
        for sid in proof["session_ids"]:
            matches = [path for path in self.home.rglob(sid + ".jsonl") if path.parent.name == "sessions"]
            require(len(matches) == 1, "Full native transcript path is absent or ambiguous")
            paths[sid] = matches[0]
        rows, transcripts = hydrate_native_results(rows, paths)
        self.report.setdefault("native_full_transcript_bindings", []).extend(transcripts)
        state = self.wait_state(fixture, marker, 1, active=True)
        oracle = validate_query(rows, fixture, marker, state, keyboard_mouse=keyboard_mouse)
        fixture["expected_marker"] = marker
        atomic_json(self.output / ("fixture-" + letter + "-host-state.json"), state)
        self.record("real model query completed actual owned app " + letter, command=command, real_usage=proof, oracle=oracle)

    def cleanup(self) -> None:
        # Stop all known apps/CLI groups first, then identify surviving helper
        # descendants by their kernel executable path, never by a loose name.
        errors = []
        for process in self.processes:
            try:
                self.stop(process)
            except BaseException as error:
                errors.append(self.redact(str(error)))
        # Destruction is best effort for every independent artifact. A failure
        # to stop one process or inspect one helper must never skip CLI/auth
        # cleanup. Replaced caller inputs are preserved and reported as failure.
        for label, action in (("native_descendants", self.stop_native_descendants), ("private_helper", lambda: self.helper_copy.unlink(missing_ok=True)),
                              ("helper_input", self.destroy_helper_input), ("CLI_and_auth", super().cleanup)):
            try:
                action()
            except BaseException as error:
                errors.append(label + ": " + self.redact(str(error)))
        self.report.setdefault("cleanup", {}).update({"helper_input_destroyed": not self.helper_source.exists(),
            "helper_private_copy_destroyed": not self.helper_copy.exists(), "native_cleanup_errors": errors})
        require(not errors, "Cross-app cleanup was incomplete: " + "; ".join(errors))

    def stop_native_descendants(self) -> None:
        if sys.platform != "darwin":
            return
        libproc = ctypes.CDLL("/usr/lib/libproc.dylib")
        libproc.proc_pidpath.argtypes = (ctypes.c_int, ctypes.c_void_p, ctypes.c_uint32)
        libproc.proc_pidpath.restype = ctypes.c_int
        rows = subprocess.run(["ps", "-axo", "pid=,pgid="], capture_output=True, text=True, timeout=10).stdout.splitlines()
        descendants = []
        for row in rows:
            fields = row.split()
            if len(fields) != 2:
                continue
            pid, pgid = map(int, fields)
            if pid == os.getpid():
                continue
            buffer = ctypes.create_string_buffer(4096)
            if libproc.proc_pidpath(pid, buffer, len(buffer)) <= 0:
                continue
            executable = Path(os.fsdecode(buffer.value))
            if executable.is_relative_to(self.root):
                require(pgid != os.getpgrp(), "Owned helper shares the harness process group")
                descendants.append({"pid": pid, "pgid": pgid, "executable": str(executable)})
                try:
                    os.killpg(pgid, signal.SIGKILL)
                except ProcessLookupError:
                    pass
        deadline = time.monotonic() + 10
        while descendants and time.monotonic() < deadline:
            running = []
            for item in descendants:
                status = subprocess.run(["ps", "-p", str(item["pid"]), "-o", "stat="], capture_output=True, text=True, timeout=5).stdout.strip()
                if status and not status.startswith("Z"):
                    running.append(item)
            if not running:
                break
            time.sleep(0.05)
        else:
            running = descendants
        self.report["owned_surviving_descendants_stopped"] = descendants
        self.report["all_owned_native_descendants_stopped"] = not running
        if running:
            raise AssertionError("Owned native helper descendant survived cleanup")

    def destroy_helper_input(self) -> None:
        if self.helper_source.exists():
            require(digest(self.helper_source) == self.args.helper_sha256, "Refusing to destroy a replaced helper input")
            self.helper_source.unlink()

    def verify_original_cu_unchanged(self) -> None:
        current = {str(path): digest(path) if path.exists() else None for path in self.original_cu_paths}
        require(current == self.original_cu_hashes, "Original user Computer Use configuration/grants changed")
        self.report["original_computer_use_config_and_grants_unchanged"] = current

    def capture_failed_fixture_states(self) -> None:
        """Retain owned-app state before shutdown, including failed queries.

        Read only the already registered private fixture paths. No screenshots,
        UI actions, app discovery, credentials or arbitrary JSON fields enter
        this diagnostic. A corrupt/missing state reports a fixed safe error.
        """
        if self.report.get("passed"):
            return
        rows = []
        for fixture in self.fixtures:
            row = {"expected_fixture": {key: fixture[key] for key in ("name", "pid", "bundle_id")}, "available": False}
            descriptor = None
            try:
                path = fixture["state_path"]
                require(path.is_relative_to(self.root) and path.parent.resolve() == self.root,
                        "Fixture diagnostic path escaped its private root")
                descriptor = os.open(path, os.O_RDONLY | getattr(os, "O_NOFOLLOW", 0))
                info = os.fstat(descriptor)
                require(stat.S_ISREG(info.st_mode) and info.st_size <= 64 << 10, "Invalid bounded fixture state file")
                data = os.read(descriptor, (64 << 10) + 1)
                require(len(data) <= 64 << 10, "Fixture state grew beyond its diagnostic limit")
                state = json.loads(data)
                require(isinstance(state, dict), "Fixture state is not an object")
                fields = ("schema_version", "nonce", "pid", "bundle_id", "name", "input", "counter", "ticks", "active", "uptime", "field_editor_selection_length")
                row.update({"available": True, "state": {key: state[key] for key in fields if key in state},
                    "identity_matches_owned_fixture": all(state.get(key) == fixture[key] for key in ("name", "pid", "bundle_id", "nonce")),
                    "focus_diagnostics": fixture_focus_diagnostics(state, self.fixtures),
                    "command_a_diagnostics": fixture_command_a_diagnostics(state)})
            except Exception as error:
                row.update({"failure_type": type(error).__name__, "error": "Owned fixture state unavailable or invalid"})
            finally:
                if descriptor is not None:
                    os.close(descriptor)
            rows.append(row)
        evidence = {"captured_utc": utc_now(), "before_fixture_cleanup": True, "screenshots_taken": False, "fixtures": rows}
        evidence = json.loads(self.redact(json.dumps(evidence)))
        atomic_json(self.output / "failed-fixture-states-before-cleanup.json", evidence)
        self.report["failed_fixture_states_before_cleanup"] = evidence

    def run_cross_app(self) -> int:
        def interrupted(signum: int, frame: Any) -> None:
            raise TimeoutError("Cross-app acceptance interrupted or exceeded its bounded deadline")
        previous = {number: signal.signal(number, interrupted) for number in (signal.SIGALRM, signal.SIGTERM, signal.SIGINT)}
        signal.setitimer(signal.ITIMER_REAL, self.args.case_timeout)
        try:
            self.prepare()
            self.build_fixtures()
            self.query_fixture(self.fixtures[0], "A")
            self.query_fixture(self.fixtures[1], "B", keyboard_mouse=self.args.include_keyboard_mouse)
            # The second query must not overwrite the earlier fixture while
            # changing focus. This is independent of its claimed tool results.
            earlier = self.wait_state(self.fixtures[0], self.fixtures[0]["expected_marker"], 1)
            atomic_json(self.output / "fixture-A-state-after-B.json", earlier)
            self.record("app A business state preserved after actual app B query", host_state=earlier)
            self.report["passed"] = True
        except BaseException as error:
            self.report.update({"passed": False, "failure_type": type(error).__name__, "failure": self.redact(str(error))})
        finally:
            signal.setitimer(signal.ITIMER_REAL, 0)
            # Preserve only the owned fixtures' current JSON before shutdown.
            # Then destroy product executables/credentials before copying the
            # remaining evidence or allowing a source repair to start.
            for label, action in (("failed_fixture_states", self.capture_failed_fixture_states),
                                  ("cleanup", self.cleanup), ("evidence_copy", self.copy_evidence),
                                  ("source_freeze", self.verify_freeze), ("helper_source_freeze", self.helper_freeze),
                                  ("original_user_cu_config", self.verify_original_cu_unchanged)):
                try:
                    action()
                except BaseException as error:
                    self.report["passed"] = False
                    self.report[label + "_failure"] = self.redact(str(error))
            shutil.rmtree(self.root, ignore_errors=True)
            cleanup = self.report.setdefault("cleanup", {})
            cleanup["temporary_workspace_destroyed"] = not self.root.exists()
            cleanup["fixture_only_grants_removed_with_private_home"] = not self.root.exists()
            self.report["passed"] = self.report["passed"] and not self.root.exists()
            self.report.update({"finished_utc": utc_now(), "elapsed_seconds": round(time.monotonic() - self.started, 3)})
            self.report = json.loads(self.redact(json.dumps(self.report)))
            self.secrets.clear()
            cleanup = self.report.setdefault("cleanup", {})
            cleanup["redaction_secret_values_cleared_after_evidence"] = not self.secrets
            atomic_json(self.output / "verification-report.json", self.report)
            for number, handler in previous.items():
                signal.signal(number, handler)
        print(json.dumps({"passed": self.report["passed"], "blocked": self.report.get("blocked", False),
                          "report": str(self.output / "verification-report.json"), "failure": self.report.get("failure")}), flush=True)
        return 0 if self.report["passed"] else 1


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    mode = parser.add_mutually_exclusive_group(required=True)
    mode.add_argument("--self-test", action="store_true")
    mode.add_argument("--run-live", action="store_true", help="explicitly authorize real selected-route queries and owned disposable native app actions")
    parser.add_argument("--repo", type=Path, default=Path(__file__).resolve().parents[2])
    parser.add_argument("--binary", type=Path)
    parser.add_argument("--expected-sha256")
    parser.add_argument("--source-freeze", type=Path)
    parser.add_argument("--helper-binary", type=Path)
    parser.add_argument("--helper-sha256")
    parser.add_argument("--helper-repo", type=Path)
    parser.add_argument("--helper-source-freeze", type=Path)
    parser.add_argument("--output", type=Path)
    parser.add_argument("--destroy-input", action="store_true", help="required: destroy caller-supplied disposable CLI and helper after every outcome")
    parser.add_argument("--expected-provider", default="easycliproxyapi")
    parser.add_argument("--expected-model", default="gpt-6-sol")
    parser.add_argument("--include-keyboard-mouse", action="store_true", help="also require exact fixture-bounded click/key/short typing in app B")
    parser.add_argument("--query-timeout", type=float, default=300)
    parser.add_argument("--case-timeout", type=float, default=1000)
    args = parser.parse_args(argv)
    if args.self_test:
        import unittest
        suite = unittest.defaultTestLoader.discover(str(Path(__file__).parent), pattern="test_computer_use_cross_app_acceptance.py")
        result = unittest.TextTestRunner(verbosity=2).run(suite)
        return 0 if result.wasSuccessful() else 1
    require(all((args.binary, args.expected_sha256, args.source_freeze, args.helper_binary, args.helper_sha256,
                 args.helper_repo, args.helper_source_freeze, args.output, args.destroy_input)),
            "Live mode requires frozen CLI/helper binaries, both source freezes, output and --destroy-input")
    require(all(re.fullmatch(r"[a-f0-9]{64}", value) for value in (args.expected_sha256, args.helper_sha256)), "Invalid supplied executable digest")
    require(0 < args.query_timeout <= 600 and 0 < args.case_timeout <= 1800, "Live timeouts must be bounded")
    return CrossAppCheck(args).run_cross_app()


if __name__ == "__main__":
    raise SystemExit(main())
