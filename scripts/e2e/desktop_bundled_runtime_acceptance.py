#!/usr/bin/env python3
"""Native macOS .app acceptance using its actual bundled CLI and real model.

The parent builds/signs one disposable .app and supplies its CLI SHA256 and a
source freeze. No build, installation, model substitution or fake completion is
performed here. --self-test checks only offline oracles and explicitly proves
no native application or provider. The outer driver removes the remaining .app
directory after this checker destroys its two disposable CLI files.
"""
from __future__ import annotations

import argparse
import ctypes
import hashlib
import http.client
import json
import os
from pathlib import Path
import re
import shutil
import signal
import socketserver
import subprocess
import tempfile
import threading
import time
from typing import Any
import uuid

from job_handoff_dispatch_live_check import JobHandoffDispatchLiveCheck
from persistent_agent_live_check import (atomic_json, digest, history_snapshots,
    json_sha, require, tool_calls, trace_proof, utc_now)

SAFE_PATH = "/usr/bin:/bin:/usr/sbin:/sbin"


def disposable_app_path(app: Path) -> Path:
    app=app.expanduser().resolve()
    require(app.suffix.lower()==".app", "Native acceptance requires a disposable .app directory")
    for installed in (Path("/Applications"),Path("/System/Applications"),Path.home()/"Applications"):
        require(not app.is_relative_to(installed), "Refusing to operate on an installed application directory")
    return app


def sha(data: bytes) -> str:
    return hashlib.sha256(data).hexdigest()


def process_table(text: str) -> dict[int, dict[str, Any]]:
    """ps args preserve spaces; never request or parse process environments."""
    rows = {}
    for line in text.splitlines():
        fields = line.split(None, 8)
        if len(fields) != 9 or not all(value.isdigit() for value in fields[:3]):
            continue
        pid, ppid, pgid = map(int, fields[:3])
        rows[pid] = {"pid": pid, "ppid": ppid, "pgid": pgid,
            "birth": " ".join(fields[3:8]), "command": fields[8]}
    return rows


def read_process_table() -> dict[int, dict[str, Any]]:
    result = subprocess.run(["/bin/ps", "-axo", "pid=,ppid=,pgid=,lstart=,args="],
        capture_output=True, text=True, timeout=5)
    require(result.returncode == 0, "Cannot inspect owned native process identities")
    return process_table(result.stdout)


def executable_path(pid: int) -> Path:
    library = ctypes.CDLL("/usr/lib/libproc.dylib", use_errno=True)
    library.proc_pidpath.argtypes = [ctypes.c_int, ctypes.c_void_p, ctypes.c_uint32]
    library.proc_pidpath.restype = ctypes.c_int
    buffer = ctypes.create_string_buffer(4096)
    count = library.proc_pidpath(pid, buffer, len(buffer))
    require(count > 0, "Cannot resolve the actual executable for owned PID " + str(pid))
    return Path(os.fsdecode(buffer.value)).resolve()


def backend_identity(rows: dict[int, dict[str, Any]], native_pid: int,
                     bundled_cli: Path, paths: dict[int, Path]) -> dict[str, Any] | None:
    matches = []
    for pid, row in rows.items():
        if row["ppid"] != native_pid or paths.get(pid) != bundled_cli:
            continue
        command = row["command"]
        prefix=str(bundled_cli)+" "
        require(command.startswith(prefix), "Bundled PID argv path differs from its verified executable")
        # ps does not quote spaces inside argv[0]. Strip the known complete
        # executable path first; parse only the remaining fixed CLI flags.
        tail=command[len(prefix):]
        if not re.match(r"^desktop\s+--web(?:\s|$)", tail):
            continue
        ports = re.findall(r"(?:^|\s)--port(?:=|\s+)([0-9]+)(?:\s|$)", tail)
        require(len(ports) == 1 and 0 < int(ports[0]) < 65536, "Owned bundled backend has an invalid port")
        matches.append({**row, "executable": str(bundled_cli), "port": int(ports[0])})
    require(len(matches) <= 1, "Native shell has multiple indistinguishable bundled backends")
    return matches[0] if matches else None


def assert_runtime_check(value: dict[str, Any], cli: Path, manifest: dict[str, Any], expected_sha: str) -> None:
    require(value.get("source") == "bundled" and Path(value.get("path", "")).resolve() == cli and
        value.get("sha256") == expected_sha == manifest.get("sha256") and value.get("version") == manifest.get("version"),
        "Native runtime check did not select the exact signed bundled CLI/version")


def assert_http(status: int, body: bytes, expected_status: int, label: str, *, json_body: bool = True) -> Any:
    require(status == expected_status, label + " returned HTTP " + str(status) + ": " + body[:300].decode(errors="replace"))
    if json_body:
        value = json.loads(body)
        require(isinstance(value, dict), label + " did not return a JSON object")
        return value
    require(b"<html" in body.lower() or b"<!doctype html" in body.lower(), label + " is not the embedded frontend")
    return {"sha256": sha(body), "bytes": len(body)}


def native_launch_options(workspace: Path, finder_style: bool) -> tuple[list[str],Path]:
    return ([],Path("/")) if finder_style else (["--workspace",str(workspace)],workspace)


def assert_finder_workspace(session: dict[str,Any], home: Path) -> Path:
    actual=Path(session.get("workDir",session.get("work_dir",""))).resolve()
    require(actual==home.resolve() and actual!=Path("/"),
        "Finder-style native startup did not replace root cwd with the private user HOME")
    return actual


def assert_query_evidence(header: dict[str, Any], history: list[dict[str, Any]], events: list[dict[str, Any]],
                          provider: str, model: str, workspace: Path, task: Path, artifact: Path,
                          expected: dict[str, Any], cost: dict[str, Any]) -> dict[str, Any]:
    require(header.get("provider") == provider and header.get("model") == model and
        Path(header.get("work_dir", "")).resolve() == workspace,
        "Persisted native session changed the selected provider/model/private workspace")
    require(artifact.is_file() and not artifact.is_symlink() and json.loads(artifact.read_text()) == expected,
        "Native real-model artifact differs from independent host arithmetic")
    proof = trace_proof(events, provider, model)
    require(proof["model_responses"] > 0 and not proof["errors"] and
        sum(sum(row[key] for key in ("input", "output", "cache_read", "cache_write")) for row in proof["usage"]) > 0,
        "Native query has no complete successful real usage trace")
    observed_tools = {row.get("tool_name") for row in events if row.get("kind") in ("tool_start", "tool_result")}
    require(observed_tools and observed_tools <= {"Read", "Write"}, "Native acceptance used tools beyond Read/Write")
    reads, writes = tool_calls(events, "Read"), tool_calls(events, "Write")
    def path(call: dict[str, Any]) -> Path:
        raw = call.get("arguments", {}).get("path", call.get("arguments", {}).get("file_path", ""))
        require(isinstance(raw, str) and Path(raw).is_absolute(), "Tool call lacks an absolute fixture path")
        return Path(raw).resolve()
    require(any(path(call) == task for call in reads), "Model did not actually Read its private task")
    accepted = [call for call in writes if path(call) == artifact and
        json.loads(call.get("arguments", {}).get("content", "null")) == expected]
    require(len(accepted) == 1, "Model did not Write the host-accepted JSON exactly once")
    write_index = next(index for index, row in enumerate(events) if row.get("kind") == "tool_result" and
        row.get("trace_call_id") == accepted[0]["call_id"])
    require(any(path(call) == artifact for call in tool_calls(events[write_index+1:], "Read")),
        "Model did not actually Read its artifact after the successful Write")
    blocks = [block for message in history for block in message.get("content", [])]
    uses = [block for block in blocks if block.get("type") == "tool_use"]
    require(uses and {block.get("name") for block in uses} <= {"Read", "Write"}, "Saved transcript lost its actual Read/Write calls")
    persisted_write = [block for block in uses if block.get("name") == "Write" and
        Path(block.get("input", {}).get("path", "")).resolve() == artifact and
        json.loads(block.get("input", {}).get("content", "null")) == expected]
    require(persisted_write and any(block.get("type") == "tool_result" and not block.get("is_error") and
        block.get("tool_use_id") == persisted_write[0].get("tool_use_id") for block in blocks),
        "Saved transcript lacks a paired successful Write occurrence")
    fields = ("input_tokens", "output_tokens", "cache_read_tokens", "cache_create_tokens")
    require(all(type(cost.get(key)) is int and cost[key] >= 0 for key in fields) and sum(cost[key] for key in fields) > 0,
        "Native session has no persisted nonzero token usage")
    return {"provider": provider, "model": model, "actual_model_responses": proof["model_responses"],
        "actual_tool_results": proof["tool_results"], "usage_trace": proof["usage"], "persisted_cost": cost,
        "artifact_sha256": digest(artifact), "artifact": expected, "accepted_write": accepted[0],
        "event_values_sha256": json_sha(events), "transcript_values_sha256": json_sha(history)}


class UpdateProxyTrap:
    """Network tripwire only; it never serves model or release responses."""
    def __init__(self):
        self.requests: list[str] = []
        owner = self
        class Handler(socketserver.BaseRequestHandler):
            def handle(self):
                self.request.settimeout(2)
                data = self.request.recv(4096)
                owner.requests.append(data.split(b"\r\n", 1)[0].decode(errors="replace"))
                self.request.sendall(b"HTTP/1.1 502 Tripwire\r\nContent-Length: 0\r\nConnection: close\r\n\r\n")
        self.server = socketserver.ThreadingTCPServer(("127.0.0.1", 0), Handler)
        self.server.daemon_threads = True
        self.thread = threading.Thread(target=self.server.serve_forever, daemon=True)

    def __enter__(self):
        self.thread.start()
        return self

    def __exit__(self, *_args):
        self.server.shutdown()
        self.server.server_close()
        self.thread.join(timeout=5)

    @property
    def url(self):
        return "http://127.0.0.1:" + str(self.server.server_address[1])


def owned_spawn(checker: Any, executable: Path, arguments: list[str], label: str,
                environment: dict[str, str], *, cwd: Path | None = None) -> tuple[subprocess.Popen, Path, Path]:
    checker.index += 1
    stem = checker.output / (f"{checker.index:03d}-" + label)
    out, err = stem.with_suffix(".stdout.log"), stem.with_suffix(".stderr.log")
    handles = out.open("w"), err.open("w")
    try:
        process = subprocess.Popen([str(executable), *arguments], cwd=cwd if cwd is not None else checker.workspace, env=environment,
            stdin=subprocess.DEVNULL, stdout=handles[0], stderr=handles[1], start_new_session=True)
    except BaseException:
        for handle in handles:
            handle.close()
        raise
    checker.processes.append(process)
    checker.handles[process.pid] = handles
    checker.report["commands"].append({"label": label, "pid": process.pid, "executable": str(executable),
        "arguments": arguments, "cwd":str(cwd if cwd is not None else checker.workspace),"stdout": out.name, "stderr": err.name})
    checker.progress(label, owned_pid=process.pid)
    return process, out, err


def native_http(checker: Any, port: int, method: str, path: str, body: Any = None,
                *, timeout: float = 10) -> tuple[int, bytes]:
    connection = http.client.HTTPConnection("127.0.0.1", port, timeout=timeout)
    checker.active_http.add(connection)
    try:
        connection.request(method, path, None if body is None else json.dumps(body).encode(),
            {"Content-Type": "application/json", "Connection": "close"})
        response = connection.getresponse()
        data = response.read(8 * 1024 * 1024 + 1)
        require(len(data) <= 8 * 1024 * 1024, "Native HTTP response exceeds the evidence bound")
        return response.status, data
    finally:
        connection.close()
        checker.active_http.discard(connection)


def quit_exact_native(checker: Any, native: subprocess.Popen, identity: dict[str, Any],
                      backend: dict[str, Any], native_executable: Path) -> dict[str, Any]:
    rows = read_process_table()
    current = rows.get(native.pid)
    require(current and current["birth"] == identity["birth"] and executable_path(native.pid) == native_executable,
        "Native PID identity changed before the owned-app quit request")
    # NSRunningApplication targets one verified PID, never bundle ID/name (the
    # user may already have another installed Desktop with that bundle ID).
    script = ('ObjC.import("AppKit"); const a=$.NSRunningApplication.runningApplicationWithProcessIdentifier(' +
        str(native.pid) + '); if (!a || !a.terminate) { throw new Error("owned native quit rejected"); }')
    result = subprocess.run(["/usr/bin/osascript", "-l", "JavaScript", "-e", script],
        capture_output=True, text=True, timeout=15, env={"PATH": SAFE_PATH})
    proof = {"native_pid": native.pid, "backend_pid": backend["pid"], "quit_method": "NSRunningApplication.terminate-by-verified-PID",
        "quit_request_exit_code": result.returncode, "quit_request_stderr_sha256": sha(result.stderr.encode()),
        "quit_request_error":checker.redact(result.stderr[:500]),
        "graceful_native_quit_verified": False, "fallback_required": False}
    deadline = time.monotonic() + 45
    while result.returncode == 0 and time.monotonic() < deadline:
        rows = read_process_table()
        backend_live = backend["pid"] in rows and rows[backend["pid"]]["birth"] == backend["birth"]
        if native.poll() is not None and not backend_live:
            proof.update({"native_exit_code": native.returncode, "backend_reaped": True,
                "graceful_native_quit_verified": native.returncode == 0})
            return proof
        time.sleep(.25)
    proof["fallback_required"] = True
    checker.report["native_quit"] = proof
    # The owner's outer finally block performs identity-checked cleanup. A
    # fallback must never be reported as proof that Wails OnShutdown ran.
    raise AssertionError("Owned native app did not gracefully quit and reap its bundled backend")


def launch_owned_native(checker: Any, native_executable: Path, cli: Path,
                        environment: dict[str, str], label: str, *, finder_style: bool = False):
    arguments,cwd=native_launch_options(checker.workspace,finder_style)
    child = owned_spawn(checker, native_executable, arguments, label, environment,
        cwd=cwd)
    native = child[0]
    deadline = time.monotonic() + 90
    backend = identity = None
    while time.monotonic() < deadline:
        require(native.poll() is None, "Real native application exited before its frontend started WebUI")
        rows = read_process_table()
        identity = rows.get(native.pid)
        if identity:
            require(executable_path(native.pid) == native_executable, "Native PID executes a different binary")
            paths = {}
            for pid, row in rows.items():
                if row["ppid"] == native.pid:
                    try:
                        paths[pid] = executable_path(pid)
                    except AssertionError:
                        pass
            backend = backend_identity(rows, native.pid, cli, paths)
            if backend:
                try:
                    status, data = native_http(checker, backend["port"], "GET", "/api/health", timeout=1)
                    if status == 200 and json.loads(data).get("status") == "ok":
                        checker.progress(label+"-backend-ready",app_path=str(native_executable.parents[2]),
                            native_pid=native.pid,backend_pid=backend["pid"],base_url="http://127.0.0.1:"+str(backend["port"]))
                        return child, identity, backend
                except (OSError, ValueError):
                    pass
        time.sleep(.25)
    raise AssertionError("Native frontend did not start one healthy verified bundled CLI child")


def assert_blank_state(home: Path) -> dict[str, Any]:
    events = [json.loads(line) for path in (home/"traces").glob("*.jsonl")
        for line in path.read_text().splitlines() if line]
    rows = [json.loads(line) for path in (home/"sessions").glob("*.jsonl")
        for line in path.read_text().splitlines() if line]
    require(not any(row.get("kind")=="tokens" for row in events), "Fresh install unexpectedly invoked a model")
    require(not any(row.get("type")=="message" and row.get("message",{}).get("role") in ("user","assistant") for row in rows),
        "Credential-free settings startup unexpectedly submitted a conversation turn")
    costs=[json.loads(path.read_text()) for path in (home/"sessions").glob("*.cost.json")]
    require(all(sum(cost.get(key,0) for key in ("input_tokens","output_tokens","cache_read_tokens","cache_create_tokens"))==0 for cost in costs),
        "Credential-free settings startup has nonzero model usage")
    return {"model_usage_events":0,"conversation_turns":0,"saved_header_records":sum(row.get("type")=="header" for row in rows),
        "raw_event_values_sha256":json_sha(events)}


def classify_blank_requests(requests: list[str]) -> dict[str,Any]:
    # Startup may load the existing public context/model directory. It is not
    # an inference endpoint. The local tripwire still returns 502, so this
    # phase proves settings remain usable while that directory is unavailable.
    catalog=[]
    for request in requests:
        require(request=="CONNECT models.dev:443 HTTP/1.1",
            "Credential-free Desktop attempted a non-catalog HTTP request: "+request)
        catalog.append(request)
    return {"external_http_requests":len(requests),"provider_model_requests":0,
        "public_catalog_requests":catalog,"public_catalog_request_count":len(catalog),
        "public_catalog_response_status":502 if catalog else None,
        "public_catalog_available":False,"external_requests_intercepted_by_tripwire":True}


def check_blank_first_run(checker: Any, native_executable: Path, cli: Path) -> dict[str, Any]:
    blank = checker.root / "first run without credentials"
    home, metis_home = blank / "home", blank / "metis-home"
    for directory in (home, metis_home):
        directory.mkdir(parents=True, mode=0o700)
        require(not any(directory.iterdir()), "Fresh-install credential/config directory is not empty")
    with UpdateProxyTrap() as trap:
        environment = {"PATH": SAFE_PATH, "HOME": str(home), "METIS_HOME": str(metis_home), "LANG": "C",
            "METIS_NO_UPDATE_CHECK": "1", "METIS_AUTO_MEMORY": "0", "NO_PROXY": "127.0.0.1,localhost",
            "HTTP_PROXY": trap.url, "HTTPS_PROXY": trap.url, "ALL_PROXY": trap.url}
        child, identity, backend = launch_owned_native(checker, native_executable, cli, environment,
            "launch-native-finder-style-fresh-install-without-config-or-credentials",finder_style=True)
        status, data = native_http(checker, backend["port"], "GET", "/")
        frontend = assert_http(status, data, 200, "fresh-install embedded frontend", json_body=False)
        status, data = native_http(checker, backend["port"], "GET", "/api/providers")
        settings = assert_http(status, data, 200, "fresh-install provider settings")
        providers = settings.get("providers")
        require(isinstance(providers, list) and providers and
            all(provider.get("credentialConfigured") is False for provider in providers),
            "Fresh native settings falsely require or report existing credentials")
        status,data=native_http(checker,backend["port"],"POST","/api/sessions",{})
        session=assert_http(status,data,201,"Finder-style fresh workspace")
        actual_workspace=assert_finder_workspace(session,home)
        assert_blank_state(metis_home)
        quit_proof = quit_exact_native(checker, child[0], identity, backend, native_executable)
        blank_state=assert_blank_state(metis_home)
    network=classify_blank_requests(trap.requests)
    proof = {"blank_home_started_empty": True, "blank_metis_home_started_empty": True,
        "native_pid": child[0].pid, "backend_pid": backend["pid"], "backend_executable": str(cli),
        "frontend": frontend, "provider_settings": providers,**network,
        "finder_style_launch":True,"launch_working_directory":"/","workspace_argument_supplied":False,
        "actual_workspace":str(actual_workspace),"expected_private_home":str(home.resolve()),
        "quit": quit_proof,**blank_state}
    checker.record("fresh install opens native frontend and provider settings without credentials or model traffic", **proof)
    checker.report["native_blank_first_run"] = proof
    return proof


def check_native_bundled_runtime(checker: Any, app: Path, expected_cli_sha256: str) -> dict[str, Any]:
    app = disposable_app_path(app)
    cli = app / "Contents/Resources/bin/metis"
    native_executable = app / "Contents/MacOS/metis-desktop"
    manifest_path = app / "Contents/Resources/bin/metis-runtime.json"
    require(" " in str(app), "Supply the movable test .app in a path containing spaces")
    require(all(path.is_file() and not path.is_symlink() for path in (cli, native_executable, manifest_path)),
        "Test .app lacks regular bundled CLI/native executable/manifest files")
    require(digest(cli) == expected_cli_sha256, "Bundled CLI differs from the fresh parent build")
    manifest = json.loads(manifest_path.read_text())
    require(manifest.get("schema_version") == 1 and manifest.get("sha256") == expected_cli_sha256 and
        manifest.get("size_bytes") == cli.stat().st_size and manifest.get("goos") == "darwin" and
        manifest.get("goarch") in ("arm64", "universal") and manifest.get("version") == (checker.repo / "VERSION").read_text().strip(),
        "Bundled manifest does not match current source version, architecture or binary bytes")
    before = {"cli": digest(cli), "manifest": digest(manifest_path), "native": digest(native_executable)}
    environment = {key: value for key, value in checker.env.items() if key != "METIS_BIN"}
    environment["PATH"] = SAFE_PATH
    require(not shutil.which("metis", path=SAFE_PATH) and "METIS_BIN" not in environment,
        "Native acceptance still exposes an external CLI override")
    runtime, _ = checker.finish(owned_spawn(checker, native_executable, ["--runtime-check"],
        "native-runtime-check-selects-bundled-cli", environment), "native-runtime-check-selects-bundled-cli")
    assert_runtime_check(runtime, cli, manifest, expected_cli_sha256)
    checker.record("native runtime check resolves signed bundled CLI with no external PATH launcher", runtime=runtime,
        bundle_manifest_sha256=before["manifest"], path=SAFE_PATH, app_path_has_spaces=True)
    with UpdateProxyTrap() as trap:
        blocked_env = {**environment, "HTTP_PROXY": trap.url, "HTTPS_PROXY": trap.url, "ALL_PROXY": trap.url, "NO_PROXY": ""}
        _, update_proof = checker.finish(owned_spawn(checker, cli, ["update", "--force"],
            "bundled-cli-refuses-independent-update", blocked_env), "bundled-cli-refuses-independent-update",
            expect_success=False, json_output=False, operation_timeout=20,
            rejection_contains="Metis Desktop manages this runtime")
        time.sleep(.1)
        require(not trap.requests and all(digest(path) == before[key] for key, path in
            (("cli", cli), ("manifest", manifest_path), ("native", native_executable))),
            "Bundled self-update contacted the release proxy or changed the signed .app")
        checker.record("bundled CLI update is rejected before any release HTTP request", exit_code=update_proof["exit_code"],
            proxy_requests=0, no_model_or_release_response_mock=True, bundle_hashes_unchanged=True)
    blank_first_run = check_blank_first_run(checker, native_executable, cli)
    nonce = uuid.uuid4().hex
    task, artifact = checker.workspace / "task.json", checker.workspace / "native-result.json"
    values = [137, 205, 319, 41, 80, 67, 19, 53]
    source = {"nonce": nonce, "rows": [{"amount": amount} for amount in values]}
    expected = {"nonce": nonce, "count": len(values), "amount": sum(values)}
    atomic_json(task, source)
    atomic_json(artifact, {})
    child, identity, backend = launch_owned_native(checker, native_executable, cli, environment,
        "launch-real-native-app-with-bundled-backend")
    native = child[0]
    status, data = native_http(checker, backend["port"], "GET", "/api/health")
    health = assert_http(status, data, 200, "native backend health")
    require(health.get("status") == "ok", "Native backend is not healthy")
    status, data = native_http(checker, backend["port"], "GET", "/")
    frontend = assert_http(status, data, 200, "embedded native frontend", json_body=False)
    checker.record("actual Wails frontend starts bundled CLI as its direct native child", native_pid=native.pid,
        native_executable=str(native_executable), backend=backend, health=health, frontend=frontend,
        external_cli_override=False, actual_backend_cli_sha256=digest(cli))
    status, data = native_http(checker, backend["port"], "POST", "/api/sessions", {})
    created = assert_http(status, data, 201, "create native session")
    session_id = created.get("id", "")
    require(re.fullmatch(r"[A-Za-z0-9_-]+", session_id) and created.get("model") == checker.model and
        Path(created.get("workDir", created.get("work_dir", ""))).resolve() == checker.workspace,
        "Native session creation lost its selected model/private workspace")
    prompt = ("Use only Read and Write. Read " + str(task) + " and " + str(artifact) +
        ". Compute the number of rows and sum of their integer amount values yourself. Write exactly one JSON object "
        "with fields nonce (copied from the input), count, amount into " + str(artifact) +
        ". Read that output back and return DONE. Do not use Bash, agents, skills, workflows, other files, or external services.")
    status, data = native_http(checker, backend["port"], "POST", "/api/turns",
        {"sessionId": session_id, "input": prompt}, timeout=checker.args.query_timeout)
    turn = assert_http(status, data, 200, "real native model turn")
    require(turn.get("sessionId") == session_id and not turn.get("stopped"), "Native turn was interrupted or changed session identity")
    transcript = checker.home / "sessions" / (session_id + ".jsonl")
    trace = checker.home / "traces" / (session_id + ".jsonl")
    cost_path = checker.home / "sessions" / (session_id + ".cost.json")
    rows = [json.loads(line) for line in transcript.read_text().splitlines() if line]
    headers = [row["header"] for row in rows if row.get("type") == "header"]
    require(headers and headers[0].get("id") == session_id, "Native session has no durable header")
    events = [json.loads(line) for line in trace.read_text().splitlines() if line]
    proof = assert_query_evidence(headers[0], history_snapshots(transcript)[-1], events, checker.provider, checker.model,
        checker.workspace, task, artifact, expected, json.loads(cost_path.read_text()))
    proof.update({"session_id": session_id, "transcript_sha256": digest(transcript), "trace_sha256": digest(trace),
        "cost_sha256": digest(cost_path), "task_sha256": digest(task), "native_pid": native.pid,
        "backend_pid": backend["pid"], "backend_executable": str(executable_path(backend["pid"]))})
    require(proof["backend_executable"] == str(cli), "Actual model turn ran in a replacement external CLI")
    checker.record("native bundled backend completes real selected-model Read Write arithmetic and persisted session", **proof)
    checker.report["native_real_query"] = proof
    checker.report["native_quit"] = quit_exact_native(checker, native, identity, backend, native_executable)
    require(transcript.is_file() and artifact.is_file() and json.loads(artifact.read_text()) == expected,
        "Native app shutdown lost its session or accepted business artifact")
    require(all(digest(path) == before[key] for key, path in
        (("cli", cli), ("manifest", manifest_path), ("native", native_executable))), "Native run mutated its signed bundled runtime")
    checker.record("normal native quit reaps bundled backend and retains its saved session", **checker.report["native_quit"],
        transcript_sha256_after_quit=digest(transcript), bundle_hashes_unchanged=True)
    return {"runtime": runtime, "manifest": manifest, "bundle_hashes": before, "blank_first_run": blank_first_run, "real_query": proof,
        "quit": checker.report["native_quit"]}


class NativeBundledRuntimeLiveCheck(JobHandoffDispatchLiveCheck):
    def __init__(self, args):
        super().__init__(args)
        self.report["boundary"] = ("A disposable signed native .app resolves its own bundled CLI without a PATH launcher, opens credential-free settings, "
            "then runs one real original-route arithmetic task with Read/Write and durable host evidence. "
            "This does not prove production deployment, arbitrary external writes, OS reboot recovery, or supplier financial billing.")

    def prepare_native(self):
        self.stage()
        self.load_route()
        config_path = self.home / "config.toml"
        text = re.sub(r"(?m)^allowed = \[.*\]$", 'allowed = ["Read", "Write"]', config_path.read_text())
        config_path.write_text(text)
        config_path.chmod(0o600)
        (self.home / "system.md").write_text("This private arithmetic acceptance task uses only Read and Write. Follow the current task exactly.\n")
        self.env["PATH"] = SAFE_PATH
        original_auth = self.original_config.parent / ".credentials/auth.json"
        self.original_auth_snapshot = {str(original_auth): digest(original_auth) if original_auth.exists() else None}
        self.monitor = threading.Thread(target=self.monitor_loop, daemon=True)
        self.monitor.start()

    def verify_freeze(self):
        super().verify_freeze()
        if hasattr(self,"original_auth_snapshot"):
            for name, before in self.original_auth_snapshot.items():
                path = Path(name)
                require((digest(path) if path.exists() else None)==before, "Original selected-route authentication file changed")
            self.report["original_auth_sha256_unchanged"]=True

    def copy_evidence(self):
        super().copy_evidence()
        manifest_path=self.output/"evidence-manifest.json"
        manifest=json.loads(manifest_path.read_text())
        blank=self.root/"first run without credentials"/"metis-home"
        for directory in ("sessions","traces"):
            for source in (blank/directory).glob("*.json*"):
                if not source.is_file() or source.is_symlink():
                    continue
                target=self.output/"evidence"/"native-first-run"/directory/source.name
                target.parent.mkdir(parents=True,exist_ok=True,mode=0o700)
                target.write_text(self.redact(source.read_text()))
                target.chmod(0o600)
                manifest.append({"path":str(target.relative_to(self.output)),"sha256":digest(target)})
        atomic_json(manifest_path,manifest)

    def run(self) -> int:
        signal.setitimer(signal.ITIMER_REAL, self.args.case_timeout)
        try:
            self.prepare_native()
            self.report["native_bundle"] = check_native_bundled_runtime(self, self.args.app, self.args.expected_cli_sha256)
            self.report["passed"] = True
        except BaseException as error:
            self.report.update({"failure_type": type(error).__name__, "failure": self.redact(str(error))})
        finally:
            signal.setitimer(signal.ITIMER_REAL, 0)
            for label, action in (("cleanup", self.cleanup), ("evidence_copy", self.copy_evidence), ("source_freeze", self.verify_freeze)):
                try:
                    action()
                except BaseException as error:
                    self.report["passed"] = False
                    self.report[label + "_failure"] = self.redact(str(error))
            shutil.rmtree(self.root, ignore_errors=True)
            self.report.setdefault("cleanup", {})["temporary_workspace_destroyed"] = not self.root.exists()
            self.report["passed"] = self.report["passed"] and not self.root.exists()
            self.report.update({"finished_utc": utc_now(), "elapsed_seconds": round(time.monotonic() - self.started, 3)})
            self.report = json.loads(self.redact(json.dumps(self.report)))
            self.secrets.clear()
            atomic_json(self.output / "verification-report.json", self.report)
            self.progress("finished", passed=self.report["passed"], failure=self.report.get("failure"))
        print(json.dumps({"passed": self.report["passed"], "report": str(self.output / "verification-report.json"),
            "failure": self.report.get("failure")}), flush=True)
        return 0 if self.report["passed"] else 1


def self_test() -> dict[str, Any]:
    checked = []
    def rejects(action, label):
        try:
            action()
        except (AssertionError, ValueError, KeyError):
            checked.append(label)
            return
        raise AssertionError("Offline native oracle accepted " + label)
    cli = Path("/private/test app/Metis.app/Contents/Resources/bin/metis")
    rows = process_table("101 1 101 Fri Oct 16 21:30:00 2026 /private/test app/Metis.app/Contents/MacOS/metis-desktop --workspace /tmp/owned\n102 101 101 Fri Oct 16 21:30:01 2026 " + str(cli) + " desktop --web --port 31001\n103 999 103 Fri Oct 16 21:30:01 2026 /usr/local/bin/metis desktop --web --port 31002\n")
    match = backend_identity(rows, 101, cli, {102: cli, 103: Path("/usr/local/bin/metis")})
    require(match and match["pid"] == 102 and match["port"] == 31001 and "test app" in match["command"], "Owned backend parsing lost paths containing spaces")
    require(backend_identity(rows, 999, cli, {103: Path("/usr/local/bin/metis")}) is None, "External CLI accepted as bundle")
    rejects(lambda: backend_identity({102: {**rows[102], "command": str(cli)+" desktop --web --port 0"}}, 101, cli, {102: cli}), "invalid backend port")
    rejects(lambda: backend_identity({102: {**rows[102], "command": "/usr/local/bin/metis desktop --web --port 31001"}}, 101, cli, {102: cli}), "changed argv executable path")
    manifest = {"version": "1.2.3", "sha256": "a" * 64}
    assert_runtime_check({"source": "bundled", "path": str(cli), **manifest}, cli, manifest, "a" * 64)
    rejects(lambda: assert_runtime_check({"source": "path", "path": "/usr/local/bin/metis", **manifest}, cli, manifest, "a" * 64), "external runtime source")
    assert_http(200, b'{"status":"ok"}', 200, "health")
    assert_http(200, b"<!doctype html><html>Metis</html>", 200, "frontend", json_body=False)
    rejects(lambda: assert_http(503, b'{"error":"unavailable"}', 200, "health"), "unhealthy HTTP")
    rejects(lambda: assert_http(200, b'{"status":"ok"}', 200, "frontend", json_body=False), "JSON substituted for frontend")
    rejects(lambda:disposable_app_path(Path("/Applications/Metis Desktop.app")),"installed app path")
    catalog=classify_blank_requests(["CONNECT models.dev:443 HTTP/1.1"])
    require(catalog["public_catalog_requests"]==["CONNECT models.dev:443 HTTP/1.1"] and catalog["provider_model_requests"]==0,
        "Public catalog lookup was confused with a credential-free model request")
    require(catalog["external_http_requests"]==1 and catalog["public_catalog_response_status"]==502 and
        classify_blank_requests([])["external_http_requests"]==0,"Catalog traffic was hidden or its response fabricated")
    for request in ("CONNECT api.openai.com:443 HTTP/1.1","CONNECT api.anthropic.com:443 HTTP/1.1",
        "CONNECT models.dev.evil.invalid:443 HTTP/1.1","CONNECT models.dev:444 HTTP/1.1",
        "GET https://models.dev/api.json HTTP/1.1","CONNECT other.invalid:443 HTTP/1.1"):
        rejects(lambda request=request:classify_blank_requests([request]),"non-catalog request "+request)
    require(native_launch_options(Path("/private/test-workspace"),True)==([],Path("/")),"Finder-style launch accidentally supplied --workspace or a fixture cwd")
    require(native_launch_options(Path("/private/test-workspace"),False)==(["--workspace","/private/test-workspace"],Path("/private/test-workspace")),"Real query phase lost its explicit fixture workspace")
    with UpdateProxyTrap() as trap:
        connection=http.client.HTTPConnection("127.0.0.1",trap.server.server_address[1],timeout=2)
        try:
            connection.request("CONNECT","offline.invalid:443")
            response=connection.getresponse();require(response.status==502,"Proxy tripwire did not reject the offline probe");response.read()
        finally:
            connection.close()
        require(len(trap.requests)==1 and trap.requests[0].startswith("CONNECT offline.invalid:443"),"Proxy tripwire missed the offline HTTP attempt")
    with tempfile.TemporaryDirectory(prefix="metis-native-oracle-") as temporary:
        workspace = Path(temporary).resolve();task, artifact = workspace / "task.json", workspace / "result.json"
        require(assert_finder_workspace({"workDir":str(workspace)},workspace)==workspace,"Private HOME workspace rejected")
        rejects(lambda:assert_finder_workspace({"workDir":"/"},workspace),"Finder root cwd not replaced")
        expected = {"nonce": "offline-only", "count": 2, "amount": 42}
        atomic_json(task, {"rows": [{"amount": 20}, {"amount": 22}]});atomic_json(artifact, expected)
        sid, invocation = "offline-unit-session", "offline-unit-invocation"
        events = []
        blocks = []
        for index, (tool, arguments, result) in enumerate((("Read", {"path": str(task)}, task.read_text()),
            ("Read", {"path": str(artifact)}, "{}"), ("Write", {"path": str(artifact), "content": json.dumps(expected)}, "written"),
            ("Read", {"path":str(artifact)},json.dumps(expected)))):
            call = "call-" + str(index)
            common = {"session_id": sid, "trace_invocation_id": invocation, "trace_call_id": call, "tool_name": tool}
            events += [{**common, "id": "start-"+str(index), "kind": "tool_start", "text": json.dumps(arguments)},
                {**common, "id": "result-"+str(index), "kind": "tool_result", "text": result}]
            blocks += [{"type": "tool_use", "name": tool, "tool_use_id": call, "input": arguments},
                {"type": "tool_result", "tool_use_id": call, "content": result}]
        events.append({"id": "offline-token", "kind": "tokens", "session_id": sid, "trace_invocation_id": invocation,
            "text": "input=10 output=2 cache_read=0 cache_write=0"})
        header = {"id": sid, "provider": "offline-unit-provider", "model": "offline-unit-model", "work_dir": str(workspace)}
        cost = {"input_tokens": 10, "output_tokens": 2, "cache_read_tokens": 0, "cache_create_tokens": 0}
        history = [{"role": "assistant", "content": blocks}]
        proof = assert_query_evidence(header, history, events, header["provider"], header["model"], workspace, task, artifact, expected, cost)
        require(proof["event_values_sha256"] == json_sha(events) and proof["artifact_sha256"] == digest(artifact), "Original event/artifact hash changed")
        rejects(lambda: assert_query_evidence({**header, "model": "wrong"}, history, events, header["provider"], header["model"], workspace, task, artifact, expected, cost), "wrong persisted model")
        rejects(lambda: assert_query_evidence(header, history, events[:-1], header["provider"], header["model"], workspace, task, artifact, expected, cost), "missing actual usage")
        rejects(lambda: assert_query_evidence(header, history, events[:6]+events[-1:], header["provider"], header["model"], workspace, task, artifact, expected, cost), "missing post-write Read")
        atomic_json(artifact, {**expected, "amount": 43})
        rejects(lambda: assert_query_evidence(header, history, events, header["provider"], header["model"], workspace, task, artifact, expected, cost), "wrong arithmetic artifact")
        blank=workspace/"blank-home";(blank/"sessions").mkdir(parents=True);(blank/"traces").mkdir()
        (blank/"sessions/header.jsonl").write_text(json.dumps({"type":"header","header":{"id":"offline-empty"}})+"\n")
        require(assert_blank_state(blank)["model_usage_events"]==0, "Empty startup header was treated as a model query")
        (blank/"traces/unexpected.jsonl").write_text(json.dumps({"kind":"tokens","text":"input=1 output=1"})+"\n")
        rejects(lambda:assert_blank_state(blank),"fresh install model traffic")
    checker = object.__new__(NativeBundledRuntimeLiveCheck)
    checker.secrets = ["offline-secret-value"]
    require(checker.redact("key=offline-secret-value") == "key=[REDACTED]", "Inherited evidence redaction failed")
    checked += ["spaces in exact owned executable path", "parent PID and executable matching", "runtime/HTTP assertions",
        "Read Write transcript/token oracle", "original event hash", "redaction"]
    return {"passed": True, "offline_only": True, "real_native_verified": False, "real_provider_verified": False, "checks": checked}


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("--app", type=Path)
    parser.add_argument("--repo", type=Path, default=Path(__file__).resolve().parents[2])
    parser.add_argument("--expected-cli-sha256")
    parser.add_argument("--source-freeze", type=Path)
    parser.add_argument("--output", type=Path)
    parser.add_argument("--destroy-input", action="store_true")
    parser.add_argument("--query-timeout", type=float, default=600)
    parser.add_argument("--case-timeout", type=float, default=1200)
    parser.add_argument("--self-test", action="store_true")
    args = parser.parse_args()
    os.umask(0o077)
    if args.self_test:
        print(json.dumps(self_test(), ensure_ascii=False, indent=2));return 0
    require(args.app and args.expected_cli_sha256 and args.source_freeze and args.output and args.destroy_input,
        "Live mode requires parent-built .app, exact CLI SHA, source freeze, fresh output and --destroy-input")
    require(re.fullmatch(r"[a-f0-9]{64}", args.expected_cli_sha256), "Expected bundled CLI SHA is invalid")
    require(0 < args.query_timeout <= 600 and 0 < args.case_timeout <= 1800, "Native live deadlines must remain bounded")
    args.app=disposable_app_path(args.app)
    args.binary = args.app / "Contents/Resources/bin/metis"
    args.expected_sha256 = args.expected_cli_sha256
    def interrupted(signum, _frame):
        raise KeyboardInterrupt("Native acceptance interrupted by " + signal.Signals(signum).name)
    def timed_out(_signum, _frame):
        raise TimeoutError("Bounded native acceptance exceeded its deadline")
    signal.signal(signal.SIGINT, interrupted);signal.signal(signal.SIGTERM, interrupted);signal.signal(signal.SIGALRM, timed_out)
    try:
        return NativeBundledRuntimeLiveCheck(args).run()
    finally:
        # A constructor failure still must destroy the explicitly disposable
        # bundled input CLI; the parent owns removing the rest of the .app.
        binary = args.binary.expanduser().resolve()
        if args.destroy_input and binary.exists():
            require(digest(binary) == args.expected_cli_sha256, "Refusing to destroy a replaced bundled CLI")
            binary.unlink()


if __name__ == "__main__":
    raise SystemExit(main())
