#!/usr/bin/env python3
"""Real scoped image recall across native compaction and private web restart.

The caller freezes/builds a disposable --binary. This harness never builds,
installs, opens a native app, changes real config/session state or retries a
model turn. Five bounded user turns and one explicit native summary are the
maximum planned flow. The final user turn is an explicitly labeled private
negative fixture: foreign ownership, missing blob and checksum failure.

InitialContext is deliberately not used as proof of the later summary wire.
Protocol unit tests plus an unseen visual footer observed in the real summary,
and fresh native-image tool SHA checks, provide the stated evidence together.
Only numeric/hash/category reports survive; all images, transcripts, key env,
private processes and both supplied/copied CLIs are destroyed in finally.
"""

from __future__ import annotations

import argparse
import base64
from collections import Counter
import hashlib
import json
import os
from pathlib import Path
import re
import secrets
import signal
import struct
import sys
import threading
import time
from typing import Any
from urllib.parse import quote
import zlib

from image_context_pressure_live_check import GLYPHS, active_history, collect_image_hashes, read_jsonl
from project_coordinator_live_check import atomic_json, require, utc_now
from trace_presentation_live_check import EventReader, TracePresentationLiveCheck, stable_digest


PROFILE = "trace-presentation-probe"  # Parent lifecycle uses this private preset.
ALL_GLYPHS = {**GLYPHS, "C": ("01111", "11000", "11000", "11000", "11000", "11000", "01111")}
FOOTER_LENGTH = 6


def response_metadata(value: Any) -> dict[str, Any]:
    text = json.dumps(value, ensure_ascii=False).lower()
    vocabulary = ("timeout", "timed out", "truncated", "max_tokens", "too large", "invalid", "permission", "unauthorized", "not referenced", "checksum", "compact failed")
    return {"sha256": stable_digest(value), "diagnostic_keyword_counts": {word: text.count(word) for word in vocabulary if word in text}}


def validate_turn_completion(result: dict[str, Any], events: list[dict[str, Any]]) -> None:
    require(not result.get("stopped"), "Real image turn was stopped before normal completion")
    endings = [event for event in events if event.get("kind") == "loop_done" and event.get("depth", 0) == 0]
    require(endings and endings[-1].get("text") == "end_turn", "Real image turn lacks normal root end_turn completion")


def synthetic_png(letter: str, footer: str, *, random_bytes: Any = secrets.token_bytes) -> bytes:
    """A large top glyph, unseen six-letter visual footer, then random pixels."""
    require(letter in ALL_GLYPHS and len(footer) == FOOTER_LENGTH and set(footer) <= {"A", "B"}, "Invalid synthetic glyph fixture")
    width, height, white_rows = 600, 640, 320
    rows = [bytearray(b"\xff" * (width * 3)) for _ in range(white_rows)]

    def draw(character: str, left: int, top: int, scale: int) -> None:
        for gy, row in enumerate(ALL_GLYPHS[character]):
            for gx, bit in enumerate(row):
                if bit == "1":
                    for y in range(top + gy * scale, top + (gy + 1) * scale):
                        start = (left + gx * scale) * 3
                        rows[y][start:start + scale * 3] = b"\0" * (scale * 3)

    draw(letter, 240, 24, 24)
    total_width = FOOTER_LENGTH * 50 + (FOOTER_LENGTH - 1) * 12
    for index, character in enumerate(footer):
        draw(character, (width - total_width) // 2 + index * 62, 224, 10)
    raw_rows = [b"\0" + bytes(row) for row in rows]
    for _ in range(height - white_rows):
        row = random_bytes(width * 3)
        require(len(row) == width * 3, "Invalid synthetic noise row")
        raw_rows.append(b"\0" + row)

    def chunk(kind: bytes, payload: bytes) -> bytes:
        return struct.pack(">I", len(payload)) + kind + payload + struct.pack(">I", zlib.crc32(kind + payload) & 0xffffffff)

    return b"\x89PNG\r\n\x1a\n" + chunk(b"IHDR", struct.pack(">IIBBBBB", width, height, 8, 2, 0, 0, 0)) + chunk(b"IDAT", zlib.compress(b"".join(raw_rows), 6)) + chunk(b"IEND", b"")


def walk_blocks(value: Any) -> Any:
    if isinstance(value, dict):
        if value.get("type") in ("image", "text", "tool_result"):
            yield value
        for child in value.values():
            yield from walk_blocks(child)
    elif isinstance(value, list):
        for child in value:
            yield from walk_blocks(child)


def validate_call_segment(events: list[dict[str, Any]], expected_success: list[str],
                          expected_errors: dict[str, tuple[str, ...]] | None = None) -> dict[str, Any]:
    expected_errors = expected_errors or {}
    starts = [event for event in events if event.get("kind") == "tool_start"]
    require(all(event.get("toolName") == "ViewImage" for event in starts), "Turn used a tool outside ViewImage-only profile")
    expected = Counter(expected_success + list(expected_errors))
    actual: Counter[str] = Counter()
    results = {event.get("traceCallID"): event for event in events if event.get("kind") == "tool_result"}
    matched_success, matched_errors = {}, {}
    for event in starts:
        data = json.loads(event.get("text", "{}"))
        require(isinstance(data.get("attachment_id"), str) and not data.get("path"), "ViewImage did not use only a stable attachment_id")
        attachment = data["attachment_id"]
        actual[attachment] += 1
        call = event.get("traceCallID")
        require(call and call in results, "ViewImage lacks an exact traceCallID result")
        result = results[call]
        if attachment in expected_errors:
            require(result.get("isError") is True, "Negative image fixture unexpectedly produced a successful image")
            text = result.get("text", "").lower()
            require(any(word in text for word in expected_errors[attachment]), "Negative image error lacks an explicit expected category")
            matched_errors[call] = attachment
        else:
            require(not result.get("isError"), "Stable-ID image recall returned a tool error")
            matched_success[call] = attachment
    require(actual == expected, "Turn skipped, repeated or substituted a required attachment_id call")
    require(not any(event.get("kind") == "error" for event in events), "Agent failed outside expected negative tool results")
    return {"success_calls": matched_success, "error_calls": matched_errors,
            "event_type_counts": dict(Counter(event.get("kind") for event in events))}


def native_result_proof(rows: list[dict[str, Any]], attachments: Path, calls: dict[str, str]) -> list[dict[str, Any]]:
    """Fresh exact call identities must carry original native image bytes."""
    results: dict[str, list[dict[str, Any]]] = {}
    for block in walk_blocks(rows):
        if block.get("type") == "tool_result" and block.get("trace_call_id") in calls:
            results.setdefault(block["trace_call_id"], []).append(block)
    proof = []
    for call, expected_sha in calls.items():
        require(call in results, "Fresh ViewImage result is absent from durable conversation ledger")
        valid = False
        for block in results[call]:
            hashes = collect_image_hashes(block.get("tool_result_blocks", []), attachments)
            if hashes == {expected_sha: 1}:
                require(any(part.get("type") == "image" and part.get("media_type") == "image/png" for part in walk_blocks(block)),
                        "Fresh ViewImage result does not retain native PNG MIME")
                valid = True
        require(valid, "Fresh stable-ID tool result lost or replaced the original native image SHA")
        proof.append({"trace_call_id_sha256": hashlib.sha256(call.encode()).hexdigest(), "native_image_sha256": expected_sha})
    return proof


def checkpoint_recall_proof(rows: list[dict[str, Any]], attachments: Path, withheld_id: str, unseen_footer: str) -> dict[str, Any]:
    active = active_history(rows)
    hashes = collect_image_hashes(active, attachments)
    require(withheld_id not in hashes, "Withheld C image remained native in active context; old-image recall has not been exercised")
    checkpoint = "\n".join(block.get("text", "") for message in active if message.get("role") == "assistant"
        for block in message.get("content", []) if block.get("type") == "text" and block.get("synthetic") and block.get("text", "").startswith("[Earlier conversation summarized:"))
    require(checkpoint and withheld_id in checkpoint, "Compacted checkpoint lost canonical ID of C removed from active images")
    require(unseen_footer in checkpoint, "Real native summary did not recover C's previously untranscribed visual footer")
    return {"checkpoint_sha256": hashlib.sha256(checkpoint.encode()).hexdigest(), "removed_old_native_image_sha256": withheld_id,
        "canonical_ID_retained_in_checkpoint": True, "previously_untranscribed_visual_footer_verified": True,
        "remaining_active_native_image_sha256_counts": dict(hashes)}


class ImageRecallCompactionLiveCheck(TracePresentationLiveCheck):
    def __init__(self, args: Any) -> None:
        super().__init__(args)
        self.report.update({"mode": "image-recall-compaction-live", "planned_user_turns": 5, "planned_manual_summary_operations": 1,
            "summary_wire_capture": False, "boundary": "Actual native-summary protocol has separate unit coverage. This live check verifies unseen visual content in the real checkpoint, removal of C native image from active context, then fresh same-session native image SHA recall and restart. Negative ledger/blob fixtures are private synthetic setup, never mocked model/tool responses."})
        self.rounds: list[dict[str, Any]] = []
        self.report["rounds"] = self.rounds
        self.compact_thread: threading.Thread | None = None

    def prepare(self) -> None:
        super().prepare()
        config = self.home / "config.toml"
        config.write_text(config.read_text().replace('"Read"', '"ViewImage"'))
        config.chmod(0o600)
        self.text_path.unlink()
        self.system_canary = "IMAGE_RECALL_SYSTEM_" + secrets.token_hex(12)
        (self.home / "agents" / (PROFILE + ".md")).write_text("---\nname: " + PROFILE + "\nmodel: gpt-6.1-sol\neffort: high\ntools: [ViewImage]\npermission_mode: dontAsk\nmax_turns: 6\nomit_claude_md: true\n---\n" + self.system_canary + "\n"
            "Follow only the current user's bounded synthetic image verification request. On upload-only turns, use no tool and return the requested marker. "
            "On recall turns, freshly invoke ViewImage exactly once for each supplied attachment_id, including after compaction or restart. Never answer from remembered descriptions. "
            "Do not use filesystem paths or any other tool. Reply only in the exact short response shape requested. "
            "For explicitly named negative fixtures, call each ID once even if an earlier one errors, do not retry/repair, and report the final requested marker after all results.\n")
        footers: list[str] = []
        while len(footers) < 3:
            value = "".join(secrets.choice("AB") for _ in range(FOOTER_LENGTH))
            if len(set(value)) == 2 and value not in footers:
                footers.append(value)
        self.letters = ["A", "B", "C"]
        self.footers = footers
        self.images = [synthetic_png(letter, footer) for letter, footer in zip(self.letters, footers)]
        self.image_ids = [hashlib.sha256(body).hexdigest() for body in self.images]
        self.image_by_hash = dict(zip(self.image_ids, self.images))
        self.source_paths = [self.workspace / ("synthetic-upload-" + str(index) + ".png") for index in range(3)]
        for path, body in zip(self.source_paths, self.images):
            path.write_bytes(body)
            path.chmod(0o600)
        self.report["startup"]["tools"] = ["ViewImage"]
        self.report["fixtures"] = [{"attachment_id": sha, "bytes": len(body), "width": 600, "height": 640,
            "visual_footer_sha256": hashlib.sha256(footer.encode()).hexdigest()} for sha, body, footer in zip(self.image_ids, self.images, self.footers)]
        self.progress("private stable-ID image fixtures prepared")

    def ledger(self, session_id: str | None = None) -> tuple[Path, list[dict[str, Any]]]:
        sid = session_id or self.session_id
        path = self.home / "sessions" / (sid + ".jsonl")
        require(path.is_file() and not path.is_symlink(), "Exact private session ledger is unavailable")
        return path, read_jsonl(path)

    def trace(self, session_id: str) -> list[dict[str, Any]]:
        code, result, _ = self.http("GET", "/api/trace?sessionId=" + quote(session_id) + "&limit=2000")
        require(code == 200 and result.get("enabled") is True and not result.get("hasMore"), "Private bounded trace is unavailable")
        return result.get("events", [])

    def run_turn(self, label: str, prompt: str, success_ids: list[str], *, session_id: str | None = None,
                 upload: bool = False, errors: dict[str, tuple[str, ...]] | None = None) -> tuple[str, str]:
        before_ids = {event["id"] for event in self.trace(session_id)} if session_id else set()
        body: dict[str, Any] = {"input": prompt}
        if session_id:
            body["sessionId"] = session_id
        if upload:
            body["images"] = [{"mediaType": "image/png", "data": base64.b64encode(path.read_bytes()).decode()} for path in self.source_paths]
        self.report["model_turn_submissions"] += 1
        started = time.monotonic()
        code, result, _ = self.http("POST", "/api/turns", body, timeout=self.args.query_timeout)
        row: dict[str, Any] = {"phase": label, "http_status": code, "elapsed_seconds": round(time.monotonic() - started, 3)}
        self.rounds.append(row)
        row["HTTP_response_metadata"] = response_metadata(result)
        require(code == 200 and isinstance(result, dict), "Bounded real image turn did not return HTTP 200")
        sid, text = result.get("sessionId", ""), result.get("text", "")
        require(isinstance(sid, str) and bool(re.fullmatch(r"[A-Za-z0-9_.-]+", sid)) and (not session_id or sid == session_id), "Real image turn changed private session identity")
        row["response_sha256"] = hashlib.sha256(text.encode()).hexdigest()
        events = [event for event in self.trace(sid) if event["id"] not in before_ids]
        validate_turn_completion(result, events)
        row["normal_root_end_turn"] = True
        calls = validate_call_segment(events, success_ids, errors)
        row["event_type_counts"] = calls["event_type_counts"]
        path, records = self.ledger(sid)
        row["fresh_native_results"] = native_result_proof(records, Path(str(path) + ".attachments"), calls["success_calls"])
        row["expected_negative_tool_errors"] = len(calls["error_calls"])
        self.assert_no_auto_compaction(sid)
        self.progress(label + " native tool results verified")
        return sid, text

    def assert_no_auto_compaction(self, session_id: str) -> None:
        events = self.trace(session_id)
        count = sum(event.get("kind") == "auto_compaction_start" or
            (event.get("kind") == "compaction_start" and event.get("text") == "auto") for event in events)
        require(count == 0, "Repeated native image recall caused erroneous automatic compaction")
        self.report["auto_compaction_start_count"] = 0

    def upload(self) -> None:
        marker = "IMAGE_UPLOAD_ACK_" + secrets.token_hex(8)
        self.session_id, text = self.run_turn("upload-user-images", "These are three new independent synthetic user images. Do not use any tool yet and do not describe glyphs or the small footer. Reply exactly " + marker, [], upload=True)
        require(text.strip() == marker, "Upload-only turn did not return bounded ACK without visual leakage")
        path, rows = self.ledger()
        hashes = collect_image_hashes(active_history(rows), Path(str(path) + ".attachments"))
        require(set(hashes) == set(self.image_ids), "Submitted user image IDs differ from SHA256 of decoded PNG bytes")
        for source in self.source_paths:
            source.unlink()
        require(all(not source.exists() for source in self.source_paths), "Original synthetic upload paths were not removed")
        self.report["original_synthetic_image_paths_removed_before_ID_recall"] = True

    def recall(self, label: str, indices: list[int], *, footer: bool = False) -> None:
        marker = "IMAGE_RECALL_DONE_" + secrets.token_hex(8)
        ids = [self.image_ids[index] for index in indices]
        request = "Freshly call ViewImage once for each attachment_id in this exact order: " + ", ".join(ids) + ". Original local upload paths are gone. Do not use remembered text or skip any fresh tool call. "
        if footer:
            request += 'After the calls, return compact JSON {"glyphs":[the upper large glyph for each image],"footers":[the six-letter lower white-band code for each image],"marker":"' + marker + '"}. Do not add any other text.'
        else:
            request += 'After the calls, return compact JSON {"glyphs":[the upper large glyph for each image],"marker":"' + marker + '"}. Do not describe or quote the lower white-band footer.'
        _, text = self.run_turn(label, request, ids, session_id=self.session_id)
        response = json.loads(text.strip())
        require(response.get("marker") == marker and response.get("glyphs") == [self.letters[index] for index in indices], "Fresh image recall returned incorrect glyphs or marker")
        if footer:
            require(response.get("footers") == [self.footers[index] for index in indices], "Fresh image recall returned incorrect unseen visual footer")
        self.rounds[-1]["visual_details_verified"] = True

    def compact(self) -> None:
        path, before_rows = self.ledger()
        before_active = active_history(before_rows)
        require(len(before_active) > 2, "Real history is too short for native summary")
        before_text = "\n".join(block.get("text", "") for block in walk_blocks(before_active) if block.get("type") == "text")
        require(not any(code in before_text for code in self.footers), "Visual footer was exposed as model-visible text before summary")
        code, _, _ = self.http("POST", "/api/sessions/activate", {"id": self.session_id})
        require(code == 200, "Could not bind the private session before compact status sampling")
        code, before_status, _ = self.http("GET", "/api/status")
        require(code == 200 and type(before_status.get("contextUsed")) is int, "Pre-summary canonical context status unavailable")
        self.reader = EventReader(self.port, self.args.query_timeout)
        require(self.reader.ready.wait(5) and not self.reader.error_type, "Compaction SSE reader unavailable")
        outcome: dict[str, Any] = {}

        def request() -> None:
            try:
                outcome["code"], outcome["body"], _ = self.http("POST", "/api/compact", {"sessionId": self.session_id,
                    "instructions": "Summarize the older conversation and its native images. For each supplied image, preserve its full canonical attachment_id and visually read the six-letter A/B code in the lower white band. The code was deliberately never transcribed earlier. State it exactly next to its attachment ID; do not infer it from an upper glyph, filename, or marker. Keep unrelated summary short."}, timeout=self.args.query_timeout)
            except BaseException as error:
                outcome["error_type"] = type(error).__name__

        self.report["manual_summary_submissions"] = 1
        self.compact_thread = threading.Thread(target=request, name="owned-native-compaction", daemon=True)
        self.compact_thread.start()
        samples = []
        deadline = time.monotonic() + self.args.query_timeout + 5
        while self.compact_thread.is_alive() and time.monotonic() < deadline:
            try:
                code, status, _ = self.http("GET", "/api/status", timeout=2)
                if code == 200:
                    stage = status.get("contextCompactionStage", "")
                    if stage:
                        require(stage in ("summarizing", "checking", "saving"), "Unknown compaction context display stage")
                        require(status.get("contextUsed") == before_status["contextUsed"], "Context gauge exposed uncommitted candidate usage")
                    samples.append({"stage": stage, "contextUsed": status.get("contextUsed")})
            except (OSError, ValueError):
                pass
            time.sleep(.15)
        self.compact_thread.join(timeout=1)
        self.report["manual_compact_HTTP_metadata"] = {"http_status": outcome.get("code"), "error_type": outcome.get("error_type"),
            "response": response_metadata(outcome.get("body"))}
        require(not self.compact_thread.is_alive() and not outcome.get("error_type"), "Native summary HTTP operation did not finish within budget")
        result = outcome.get("body", {})
        require(outcome.get("code") == 200 and result.get("compacted") is True, "Manual native-image summary was not successfully committed")
        _, rows = self.ledger()
        require(sum(row.get("type") == "history_replace" for row in rows) > sum(row.get("type") == "history_replace" for row in before_rows), "Native compact did not persist a replacement checkpoint")
        checkpoint_proof = checkpoint_recall_proof(rows, Path(str(path) + ".attachments"), self.image_ids[2], self.footers[2])
        code, after_status, _ = self.http("GET", "/api/status")
        require(code == 200 and not after_status.get("contextCompactionStage"), "Committed compaction left a stale context stage")
        self.report["compaction"] = {"api_counts": {name: result.get(name) for name in ("before", "after", "beforeTokens", "afterTokens")},
            **checkpoint_proof, "context_before": before_status["contextUsed"],
            "context_after": after_status.get("contextUsed"), "sampled_context_stages": dict(Counter(sample["stage"] for sample in samples)),
            "frozen_uncommitted_context_samples": sum(bool(sample["stage"]) for sample in samples), "context_stage_cleared_after_commit": True}
        self.assert_no_auto_compaction(self.session_id)
        self.progress("native summary committed, old C native block removed and unseen visual footer preserved")

    def negative_fixtures(self) -> None:
        code, created, _ = self.http("POST", "/api/sessions", {})
        require(code == 201 and isinstance(created.get("id"), str), "Could not create second private negative-fixture session")
        sid = created["id"]
        path, rows = self.ledger(sid)
        require(not active_history(rows), "Negative fixture session must be new with no real conversation")
        missing = hashlib.sha256(secrets.token_bytes(32)).hexdigest()
        expected_corrupt = synthetic_png("A", "ABABAB", random_bytes=lambda size: b"\0" * size)
        corrupt = hashlib.sha256(expected_corrupt).hexdigest()
        references = {"type": "message", "message": {"role": "user", "content": [
            {"type": "image", "media_type": "image/png", "attachment_hash": missing},
            {"type": "image", "media_type": "image/png", "attachment_hash": corrupt}]}}
        with path.open("a", encoding="utf-8") as stream:
            stream.write(json.dumps(references) + "\n" + json.dumps({"type": "history_replace", "messages": []}) + "\n")
            stream.flush()
            os.fsync(stream.fileno())
        attachments = Path(str(path) + ".attachments")
        attachments.mkdir(mode=0o700)
        (attachments / corrupt).write_bytes(b"owned negative fixture: intentionally wrong checksum")
        (attachments / corrupt).chmod(0o600)
        errors = {self.image_ids[2]: ("not referenced",), missing: ("no such file", "not exist", "not found", "missing"), corrupt: ("checksum",)}
        marker = "IMAGE_NEGATIVES_DONE_" + secrets.token_hex(8)
        prompt = "This is a bounded negative-fixture tool test in a separate private session. Call ViewImage exactly once for each attachment_id: " + ", ".join(errors) + ". These should respectively fail current-session ownership, referenced-but-missing blob, and referenced-but-corrupt checksum. Continue to the remaining IDs after each expected error; do not retry, repair or substitute paths. After all three calls reply exactly " + marker
        _, text = self.run_turn("private-negative-image-cases", prompt, [], session_id=sid, errors=errors)
        require(text.strip() == marker, "Negative-fixture turn did not complete its bounded marker")
        self.report["negative_fixture"] = {"private_synthetic_setup": True, "foreign_scope_denied": True,
            "missing_blob_reported": True, "checksum_corruption_reported": True, "primary_real_test_attachments_modified": False}

    def run(self) -> int:
        try:
            self.prepare()
            self.start_server("first")
            self.upload()
            self.recall("original-paths-gone-recall-AB", [0, 1])
            self.compact()
            self.recall("removed-old-image-C-recall-after-summary", [2], footer=True)
            self.stop_server()
            self.start_server("restarted")
            self.recall("same-session-stable-ID-recall-after-restart", [0, 1, 2], footer=True)
            self.negative_fixtures()
            code, _, _ = self.http("POST", "/api/sessions/activate", {"id": self.session_id})
            require(code == 200, "Could not restore main private session for browser inspection")
            require(self.report["model_turn_submissions"] == 5, "Unexpected extra model-turn submission")
            self.hold()
            self.report["passed"] = True
        except BaseException as error:
            self.report["failure_type"] = type(error).__name__
            if type(error) is AssertionError:
                self.report["failure"] = str(error)[:300]
        finally:
            try:
                self.cleanup()
            except BaseException as error:
                self.report["cleanup_failure_type"] = type(error).__name__
                self.report["passed"] = False
            if self.compact_thread is not None:
                self.compact_thread.join(timeout=5)
                if self.compact_thread.is_alive():
                    self.report["compaction_HTTP_thread_stopped"] = False
                    self.report["passed"] = False
                else:
                    self.report["compaction_HTTP_thread_stopped"] = True
            if self.report.get("original_config_unchanged") is False:
                self.report["passed"] = False
            self.report["finished_utc"] = utc_now()
            atomic_json(self.output / "summary.json", self.report)
        print(json.dumps({"passed": self.report["passed"], "summary": str(self.output / "summary.json")}), flush=True)
        return 0 if self.report["passed"] else 1


def arguments(argv: list[str] | None = None) -> argparse.Namespace:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--binary", type=Path, required=True)
    parser.add_argument("--destroy-input", action="store_true", required=True)
    parser.add_argument("--expected-sha256", required=True)
    parser.add_argument("--source-home", type=Path, default=Path(os.environ.get("METIS_HOME", str(Path.home() / ".metis"))))
    parser.add_argument("--output", type=Path, required=True)
    parser.add_argument("--query-timeout", type=int, default=300)
    parser.add_argument("--max-output-tokens", type=int, default=8192)
    parser.add_argument("--hold-seconds", type=int, default=0)
    parser.add_argument("--stop-file", type=Path)
    args = parser.parse_args(argv)
    if not re.fullmatch(r"[a-f0-9]{64}", args.expected_sha256):
        parser.error("--expected-sha256 must be a SHA256 hex digest")
    if not 30 <= args.query_timeout <= 900 or not 2048 <= args.max_output_tokens <= 16384 or not 0 <= args.hold_seconds <= 3600:
        parser.error("Timeout must be 30..900, output budget 2048..16384, hold 0..3600")
    return args


if __name__ == "__main__":
    def terminate(_signal: int, _frame: Any) -> None:
        raise InterruptedError("Private image recall check interrupted")
    signal.signal(signal.SIGTERM, terminate)
    try:
        raise SystemExit(ImageRecallCompactionLiveCheck(arguments()).run())
    except (AssertionError, OSError, ValueError) as error:
        print(json.dumps({"passed": False, "startup_failure_type": type(error).__name__}), file=sys.stderr)
        raise SystemExit(1)
