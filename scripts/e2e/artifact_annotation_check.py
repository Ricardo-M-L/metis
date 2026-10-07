#!/usr/bin/env python3
"""Verify Artifact annotation backend APIs only against an isolated fixture.

No UI input is automated. Leaves its test session available for separate native
or browser visual acceptance, and writes evidence below the fixture root.
"""

from __future__ import annotations

import argparse
import hashlib
import json
from pathlib import Path
import time
from urllib.error import HTTPError
from urllib.parse import urlencode, urlparse
from urllib.request import Request, urlopen


def request(base, path, body=None, *, method=None, expected=(200,), binary=False):
    req = Request(base + path, data=json.dumps(body).encode() if body is not None else None,
                  method=method or ("POST" if body is not None else "GET"),
                  headers={"Content-Type": "application/json"})
    try:
        response = urlopen(req, timeout=40)
    except HTTPError as error:
        response = error
    with response:
        status = response.status
        data = response.read()
    if status not in expected:
        raise AssertionError(f"{req.method} {path}: expected {expected}, received {status}: {data[:500]!r}")
    return data if binary else json.loads(data) if data else None


def check_loopback(address):
    parsed = urlparse(address)
    if parsed.scheme != "http" or parsed.hostname != "127.0.0.1" or not parsed.port:
        raise RuntimeError("Only isolated loopback HTTP fixture services are supported")


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("manifest", type=Path, help="desktop_fixture.py's printed fixture.json")
    args = parser.parse_args()
    manifest = json.loads(args.manifest.read_text())
    root = Path(manifest["root"]).resolve()
    if (not root.name.startswith("metis-desktop-e2e-") or args.manifest.resolve().parent != root
            or Path(manifest["metis_home"]).resolve() != root / "home"):
        raise RuntimeError("Refusing anything except an isolated desktop_fixture manifest")
    environment = json.loads((root / "environment.json").read_text())
    if (environment.get("METIS_HOME") != manifest["metis_home"]
            or environment.get("METIS_E2E_API_KEY") != "fixture-only-not-a-real-key"):
        raise RuntimeError("Fixture environment does not match its isolated manifest")
    base, provider = manifest["web_url"], manifest["fixture_url"]
    check_loopback(base)
    check_loopback(provider)
    request(provider, "/fixture/health")
    report = {"started": time.time(), "kind": "controlled-provider-backend-acceptance", "checks": []}
    try:
        session = request(base, "/api/sessions", {}, expected=(200, 201))
        sid = session.get("id", session.get("sessionId"))
        assert sid, "Session ID missing"
        report["sessionId"] = sid
        request(base, "/api/sessions/rename", {"id": sid, "title": "点选修改验收"})
        request(base, "/api/turns", {"sessionId": sid, "input": "[artifact-edit-fixture] 创建点选修改验收页面"})
        query = "?" + urlencode({"sessionId": sid})
        listed = request(base, "/api/artifacts" + query)["artifacts"]
        assert len(listed) == 1 and listed[0]["title"] == "点选修改验收", listed
        artifact_id = listed[0]["id"]
        report["artifactId"] = artifact_id
        assert listed[0]["current_version"] == 1, listed[0]
        prefix = "/api/artifacts/" + artifact_id
        original = request(base, prefix + "/download" + query + "&version=1", binary=True)
        assert "探索工作成果" in original.decode() and b"#536dfe" in original
        report["checks"].append("Real Artifact.create produced verified version 1")

        preview = request(base, prefix + "/annotation-preview" + query + "&version=1")
        assert preview["digest"] == hashlib.sha256(original).hexdigest(), preview
        check_loopback(preview["url"])
        target = next(item for item in preview["targets"]
                      if item["tag"] == "a" and item["text"] == "探索工作成果")
        body = {"version": 1, "digest": preview["digest"], "targetId": target["id"],
                "instruction": "把按钮改成青绿色，文案改为开始体验"}
        prepared = request(base, prefix + "/annotate" + query, body)
        reference = prepared["reference"]
        assert reference == {"artifactId": artifact_id, "version": 1,
                             "digest": preview["digest"], "targetId": target["id"]}, reference
        assert "metis_artifact_annotation" in prepared["prompt"] and "expected_version=1" in prepared["prompt"]
        assert request(base, prefix + query)["artifact"]["current_version"] == 1
        report["reference"] = reference
        report["checks"].append("Canonical target and SHA-256 reference created without mutating the artifact")

        request(base, "/api/turns", {"sessionId": sid, "input": prepared["prompt"]})
        updated = request(base, prefix + query)["artifact"]
        assert updated["current_version"] == 2 and len(updated["versions"]) == 2, updated
        latest = request(base, prefix + "/download" + query + "&version=2", binary=True)
        assert "开始体验" in latest.decode() and b"#0f9d83" in latest
        assert request(base, prefix + "/download" + query + "&version=1", binary=True) == original
        report["versions"] = updated["versions"]
        report["checks"].append("Real read→conditional update produced version 2 and preserved immutable version 1")

        request(base, prefix + "/annotate" + query, body, expected=(409,))
        fresh_preview = request(base, prefix + "/annotation-preview" + query + "&version=2")
        invalid = {"version": 2, "digest": fresh_preview["digest"],
                   "targetId": "target-9999", "instruction": "修改按钮"}
        request(base, prefix + "/annotate" + query, invalid, expected=(400,))
        other = request(base, "/api/sessions", {}, expected=(200, 201))
        other_id = other.get("id", other.get("sessionId"))
        request(base, prefix + "/annotation-preview?" + urlencode({"sessionId": other_id}), expected=(403,))
        request(base, "/api/sessions/activate", {"id": sid})
        report["checks"].append("Stale version, unknown target and cross-session reads are rejected")

        evidence = [json.loads(line) for line in Path(manifest["evidence"]).read_text().splitlines()]
        calls = [row for row in evidence if row["event"] == "started" and row.get("annotation", {}).get("artifactId") == artifact_id]
        assert [row["phase"] for row in calls] == ["artifact_edit_read", "artifact_edit_update", "artifact_edit_final"], calls
        assert calls[0]["annotation"]["selection"] == target
        tools = [call for row in calls for call in row["tool_calls"]]
        assert [item["arguments"]["action"] for item in tools] == ["read", "update"], tools
        if manifest.get("omit_artifact_expected_version", False):
            assert "expected_version" not in tools[1]["arguments"], tools[1]
            report["checks"].append("Host enforced the selected base although the provider omitted expected_version")
        else:
            assert tools[1]["arguments"]["expected_version"] == 1
        transcript = request(base, "/api/sessions/" + sid)
        assert "已创建版本 2" in json.dumps(transcript, ensure_ascii=False), "Persisted transcript lacks final answer"
        report["checks"].append("Actual Responses requests include the selected target; transcript contains real tools and final result")
        report["passed"] = True
    except Exception as error:
        report["passed"] = False
        report["error"] = str(error)
        raise
    finally:
        report["finished"] = time.time()
        output = root / "artifact-annotation-report.json"
        output.write_text(json.dumps(report, indent=2, ensure_ascii=False))
        print(json.dumps({"report": str(output), **report}, ensure_ascii=False), flush=True)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
