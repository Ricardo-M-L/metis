#!/usr/bin/env python3
"""Bounded real-CLI/isolated-Desktop iteration continuation acceptance.

Uses only owned localhost model responses, private METIS_HOME and real Bash
append effects. Never builds, installs, reads credentials or operates a user app.
Supply a freshly frozen executable with --binary and a new --output directory.
"""
from __future__ import annotations

import argparse
from concurrent.futures import ThreadPoolExecutor
import hashlib
from http.client import HTTPConnection
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import json
import os
from pathlib import Path
import re
import shlex
import signal
import socket
import subprocess
import threading
import time
from urllib.error import HTTPError, URLError
from urllib.parse import urlsplit
from urllib.request import ProxyHandler, Request, build_opener
import uuid


LOCAL_HTTP = build_opener(ProxyHandler({}))

def require(value, message):
    if not value:
        raise AssertionError(message)


def atomic_json(path, data):
    temp = path.with_suffix(path.suffix + '.tmp')
    temp.write_text(json.dumps(data, ensure_ascii=False, indent=2) + '\n')
    temp.replace(path)


def http(url, body=None, timeout=90):
    request = Request(url, data=json.dumps(body).encode() if body is not None else None,
                      headers={'Content-Type': 'application/json'})
    try:
        with LOCAL_HTTP.open(request, timeout=timeout) as response:
            return response.status, json.load(response)
    except HTTPError as error:
        return error.code, json.load(error)


def wait_for(check, label, seconds=30):
    deadline = time.monotonic() + seconds
    while time.monotonic() < deadline:
        result = check()
        if result:
            return result
        time.sleep(.03)
    raise AssertionError('timeout: ' + label)


def free_port():
    with socket.socket() as sock:
        sock.bind(('127.0.0.1', 0))
        return sock.getsockname()[1]


def read_history(path):
    header, messages = {}, []
    for line in path.read_text().splitlines():
        entry = json.loads(line)
        if entry.get('type') == 'header':
            header.update(entry.get('header', {}))
        elif entry.get('type') == 'history_replace':
            messages = entry.get('messages', [])
        elif entry.get('type') == 'message':
            messages.append(entry['message'])
    return header, messages


class Case:
    def __init__(self, name, root, steps):
        self.name, self.root, self.steps = name, root, steps
        self.requests, self.auxiliary, self.errors = [], 0, []
        self.gate_entered, self.gate_release = threading.Event(), threading.Event()
        self.lock = threading.Lock()
        self.workspace = root / 'workspace'
        self.workspace.mkdir(parents=True)
        self.home = root / 'home'
        self.home.mkdir()
        (self.home / 'skills').mkdir()
        self.effects = self.workspace / 'steps.log'

    def tool_id(self, step):
        return f'call_iter_{self.name}_{step:04d}'

    def effect_lines(self):
        return self.effects.read_text().splitlines() if self.effects.exists() else []


class Model:
    def __init__(self, root):
        self.root, self.cases, self.stopped = root, {}, threading.Event()
        self.auxiliary_requests = 0
        self.log_lock = threading.Lock()
        model = self

        class Handler(BaseHTTPRequestHandler):
            protocol_version = 'HTTP/1.1'

            def log_message(self, *_):
                pass

            def send_json(self, data, status=200):
                encoded = json.dumps(data).encode()
                self.send_response(status)
                self.send_header('Content-Type', 'application/json')
                self.send_header('Content-Length', str(len(encoded)))
                self.send_header('Connection', 'close')
                self.end_headers()
                self.wfile.write(encoded)
                self.close_connection = True

            def do_HEAD(self):
                self.send_response(200)
                self.send_header('Content-Length', '0')
                self.end_headers()

            def do_GET(self):
                if urlsplit(self.path).path == '/v1/models':
                    self.send_json({'data': [{'id': 'iteration-fixture-model',
                        'context_window': 1_000_000, 'max_input_tokens': 900_000,
                        'max_output_tokens': 64_000}]})
                else:
                    self.send_json({'error': 'unknown fixture route'}, 404)

            def do_POST(self):
                if self.path != '/v1/responses':
                    self.send_json({'error': 'unknown fixture route'}, 404)
                    return
                try:
                    size = int(self.headers.get('Content-Length', '0'))
                    require(0 < size < 8 * 1024 * 1024, 'request size outside fixture bound')
                    body = json.loads(self.rfile.read(size))
                    case = None
                    for item in body.get('input', []):
                        if item.get('role') != 'user':
                            continue
                        content = item.get('content', [])
                        texts = [content] if isinstance(content, str) else [b.get('text', '') for b in content]
                        for text in texts:
                            match = re.match(r'ITERATION_CASE=([a-z0-9_-]+)\b', text.strip())
                            if match:
                                case = model.cases.get(match.group(1))
                    if case is None:
                        model.auxiliary_requests += 1
                        model.record({'auxiliaryRequest': True, 'requestSha256': hashlib.sha256(json.dumps(body, sort_keys=True).encode()).hexdigest()})
                        self.reply(body, [], 'No durable facts to retain.', 'auxiliary')
                        return
                    results = [item for item in body.get('input', []) if item.get('type') == 'function_call_output'
                               and str(item.get('call_id', '')).startswith(f'call_iter_{case.name}_')]
                    ids = [item['call_id'] for item in results]
                    expected = [case.tool_id(n) for n in range(1, len(results) + 1)]
                    require(ids == expected, f'{case.name}: missing, duplicate or reordered tool results')
                    for number, item in enumerate(results, 1):
                        require(f'STEP_OK=step-{number:04d}' in str(item.get('output', '')),
                                f'{case.name}: actual Bash result missing step {number}')
                    reminders = sum(json.dumps(item, ensure_ascii=False).count('<iteration-continuation>')
                                    for item in body.get('input', []))
                    tool_schema = any(tool.get('name') == 'Bash' for tool in body.get('tools', []))
                    record = {'case': case.name, 'completedResults': len(results), 'continuationReminders': reminders,
                              'bashAvailable': tool_schema, 'requestSha256': hashlib.sha256(json.dumps(body, sort_keys=True).encode()).hexdigest()}
                    with case.lock:
                        case.requests.append(record)
                    model.record(record)
                    if case.name.startswith('error'):
                        status = int(case.name[-3:])
                        self.send_json({'error': {'message': f'controlled fixture HTTP {status}', 'type': 'fixture_error'}}, status)
                        return
                    if case.name == 'cancel' and len(results) == 3:
                        require(reminders >= 1, 'cancel gate was reached before the continuation seam')
                        case.gate_entered.set()
                        self.reply(body, [], '', 'cancel_gate', case)
                        return
                    if not tool_schema:
                        self.reply(body, [], f'FINITE_RESCUE case={case.name} completed_steps={len(results)}', 'finite_rescue')
                        return
                    if len(results) >= case.steps:
                        self.reply(body, [], f'ITERATION_COMPLETE case={case.name} steps={case.steps}', 'final')
                        return
                    step = len(results) + 1
                    command = (f"printf '%s\\n' {shlex.quote(f'step-{step:04d}')} >> {shlex.quote(str(case.effects))}; "
                               f"printf '%s\\n' {shlex.quote(f'STEP_OK=step-{step:04d}')}")
                    call = {'type': 'function_call', 'id': f'fc_{case.name}_{step}', 'call_id': case.tool_id(step),
                            'name': 'Bash', 'arguments': json.dumps({'command': command, 'description': f'Actual ordered append step {step}', 'timeout_ms': 10000}),
                            'status': 'completed'}
                    self.reply(body, [call], '', 'tool')
                except (BrokenPipeError, ConnectionResetError):
                    return
                except BaseException as error:
                    model.record({'fixtureError': repr(error)})
                    try:
                        self.send_json({'error': {'message': str(error)}}, 400)
                    except (BrokenPipeError, ConnectionResetError):
                        pass

            def reply(self, body, calls, text, phase, case=None):
                response_id, message_id = 'resp_' + uuid.uuid4().hex, 'msg_' + uuid.uuid4().hex
                output = calls or [{'type': 'message', 'id': message_id, 'role': 'assistant', 'status': 'completed',
                                    'content': [{'type': 'output_text', 'text': text, 'annotations': []}]}]
                response = {'id': response_id, 'object': 'response', 'model': body.get('model'), 'status': 'completed',
                            'output': output, 'usage': {'input_tokens': 128, 'output_tokens': 16, 'total_tokens': 144}}
                if not body.get('stream'):
                    self.send_json(response)
                    return
                self.send_response(200)
                self.send_header('Content-Type', 'text/event-stream')
                self.send_header('Connection', 'close')
                self.end_headers()
                self.close_connection = True
                sequence = 0

                def emit(kind, **payload):
                    nonlocal sequence
                    sequence += 1
                    self.wfile.write(('data: ' + json.dumps({'type': kind, 'sequence_number': sequence, **payload}) + '\n\n').encode())
                    self.wfile.flush()

                emit('response.created', response={**response, 'output': [], 'status': 'in_progress'})
                if phase == 'cancel_gate':
                    while not model.stopped.is_set() and not case.gate_release.wait(.05):
                        self.wfile.write(b': pending cancellation\n\n')
                        self.wfile.flush()
                    return
                for index, item in enumerate(output):
                    pending = {**item, 'status': 'in_progress'}
                    if item['type'] == 'function_call':
                        pending['arguments'] = ''
                    else:
                        pending['content'] = []
                    emit('response.output_item.added', output_index=index, item=pending)
                    if item['type'] == 'message':
                        emit('response.output_text.delta', output_index=index, item_id=message_id, content_index=0, delta=text)
                        emit('response.output_text.done', output_index=index, item_id=message_id, content_index=0, text=text)
                    emit('response.output_item.done', output_index=index, item=item)
                emit('response.completed', response=response)
                self.wfile.write(b'data: [DONE]\n\n')
                self.wfile.flush()

        self.server = ThreadingHTTPServer(('127.0.0.1', 0), Handler)
        self.server.daemon_threads = True
        self.thread = threading.Thread(target=self.server.serve_forever, daemon=True)
        self.thread.start()
        self.url = f'http://127.0.0.1:{self.server.server_port}/v1'

    def record(self, record):
        with self.log_lock:
            with (self.root / 'model-requests.jsonl').open('a') as stream:
                stream.write(json.dumps(record, ensure_ascii=False) + '\n')

    def close(self):
        self.stopped.set()
        for case in self.cases.values():
            case.gate_release.set()
        self.server.shutdown()
        self.server.server_close()
        self.thread.join(timeout=5)


class EventStream:
    """A real SSE client with an owned socket, so cleanup never waits for ping."""
    def __init__(self, url, path):
        parsed = urlsplit(url)
        self.connection = HTTPConnection(parsed.hostname, parsed.port, timeout=120)
        self.connection.request('GET', '/api/events')
        self.response = self.connection.getresponse()
        require(self.response.status == 200, 'Desktop SSE endpoint failed')
        self.socket = self.connection.sock
        self.events, self.error = [], None
        self.ready, self.stopped = threading.Event(), threading.Event()
        self.path, self.lock = path, threading.Lock()
        self.thread = threading.Thread(target=self.read, daemon=True)
        self.thread.start()
        require(self.ready.wait(10), 'Desktop SSE did not become ready')

    def read(self):
        kind, data = '', []
        try:
            with self.path.open('w') as log:
                while not self.stopped.is_set():
                    line = self.response.readline()
                    if not line:
                        return
                    line = line.decode().rstrip('\r\n')
                    if line.startswith('event: '):
                        kind = line[7:]
                    elif line.startswith('data: '):
                        data.append(line[6:])
                    elif not line and data:
                        payload = json.loads('\n'.join(data))
                        event = {'event': kind, 'data': payload}
                        with self.lock:
                            self.events.append(event)
                        log.write(json.dumps(event, ensure_ascii=False) + '\n'); log.flush()
                        if kind == 'ready':
                            self.ready.set()
                        kind, data = '', []
        except BaseException as error:
            if not self.stopped.is_set():
                self.error = repr(error)

    def session_events(self, sid, kind):
        with self.lock:
            return [event['data'] for event in self.events
                    if event['event'] == kind and event['data'].get('session') == sid]

    def close(self):
        self.stopped.set()
        if self.socket:
            try:
                self.socket.shutdown(socket.SHUT_RDWR)
            except OSError:
                pass
        self.connection.close()
        self.thread.join(timeout=5)
        require(not self.thread.is_alive(), 'owned SSE reader did not stop')


class Acceptance:
    def __init__(self, args):
        self.args = args
        self.binary = args.binary.expanduser().resolve()
        self.output = args.output.expanduser().resolve()
        self.output.mkdir(parents=True, exist_ok=False)
        self.model = Model(self.output)
        self.processes, self.streams, self.ports = [], [], [self.model.server.server_port]
        self.report = {'controlledModel': True, 'paidProviderUsed': False, 'installedApplicationTouched': False,
                       'binary': str(self.binary), 'binarySha256': hashlib.sha256(self.binary.read_bytes()).hexdigest(),
                       'steps': args.steps, 'cases': {}, 'passed': False}
        if args.source_freeze:
            self.report['sourceFreeze'] = json.loads(args.source_freeze.read_text())
            expected = self.report['sourceFreeze'].get('binarySha256')
            require(not expected or expected == self.report['binarySha256'], 'binary does not match parent source-freeze provenance')

    def environment(self, case, checkpoint=100):
        config = f'''[provider]\ndefault = "iteration-fixture"\n[provider.custom.iteration-fixture]\ntransport = "openai_responses"\nbase_url = "{self.model.url}"\napi_key_env = "METIS_ITER_FIXTURE_KEY"\nmodel = "iteration-fixture-model"\nmax_tokens = 512\ntimeout_seconds = 120\n[session]\ndir = "{case.home}/sessions"\nskill_dir = "{case.home}/skills"\nmax_iterations = {checkpoint}\nauto_compact_threshold = 0.95\nauto_compact_minimum_tokens = 900000\n[permission]\nmode = "default"\n[[permission.allow]]\ntool = "Bash"\n[tools]\nallowed = ["Bash"]\n'''
        (case.home / 'config.toml').write_text(config)
        (case.home / 'config.toml').chmod(0o600)
        environment = {k: os.environ[k] for k in ('PATH', 'HOME', 'TMPDIR', 'SHELL', 'LANG', 'LC_ALL') if k in os.environ}
        environment.update({'METIS_HOME': str(case.home), 'METIS_BIN': str(self.binary), 'METIS_ITER_FIXTURE_KEY': 'fixture-only-not-a-real-key',
                            'METIS_AUTO_MEMORY': '0', 'METIS_NO_UPDATE_CHECK': '1', 'METIS_CATALOG_DISABLE': '1', 'METIS_MICROCOMPACT': '0',
                            'METIS_RECOVERY_MAX_SECONDS': '4', 'METIS_RECOVERY_MAX_ATTEMPTS': '2', 'METIS_RECOVERY_MAX_BACKOFF_SECONDS': '1',
                            'NO_PROXY': '127.0.0.1,localhost'})
        return environment

    def case(self, name):
        case = Case(name, self.output / name, self.args.steps)
        self.model.cases[name] = case
        return case

    def start(self, case, args, environment):
        stdout = (case.root / 'stdout.log').open('w')
        stderr = (case.root / 'stderr.log').open('w')
        process = subprocess.Popen([str(self.binary), *args], cwd=case.workspace, env=environment,
                                   stdin=subprocess.PIPE, stdout=stdout, stderr=stderr, start_new_session=True)
        self.processes.append((process, stdout, stderr))
        return process

    def verify_effects(self, case, count):
        lines = case.effect_lines()
        require(lines == [f'step-{n:04d}' for n in range(1, count + 1)], f'{case.name}: actual side effects duplicated/missing/reordered: {len(lines)}')
        return hashlib.sha256(case.effects.read_bytes()).hexdigest() if case.effects.exists() else None

    def verify_history(self, case, sid, count, completed=True):
        header, messages = read_history(case.home / 'sessions' / (sid + '.jsonl'))
        uses, results, tasks, reminders = [], [], 0, 0
        for message in messages:
            for block in message.get('content', []):
                if block.get('type') == 'tool_use' and str(block.get('tool_use_id', '')).startswith('call_iter_'):
                    uses.append(block['tool_use_id'])
                if block.get('type') == 'tool_result' and str(block.get('tool_use_id', '')).startswith('call_iter_'):
                    results.append(block['tool_use_id'])
                if block.get('type') == 'text':
                    tasks += int(message.get('role') == 'user' and block.get('text', '').startswith('ITERATION_CASE='))
                    reminders += block.get('text', '').count('<iteration-continuation>')
        expected = [case.tool_id(n) for n in range(1, count + 1)]
        require(uses == expected and results == expected, f'{case.name}: durable history tool pairing mismatch: {len(uses)}/{len(results)}')
        require(tasks == 1, f'{case.name}: original user task count={tasks}')
        if completed:
            require(any(f'ITERATION_COMPLETE case={case.name} steps={count}' in b.get('text', '') for m in messages for b in m.get('content', [])), 'final reply missing from durable session')
        return {'userTaskCount': tasks, 'toolUses': len(uses), 'toolResults': len(results), 'continuationReminders': reminders,
                'ledgerSha256': hashlib.sha256((case.home / 'sessions' / (sid + '.jsonl')).read_bytes()).hexdigest()}

    def cli(self, name='cli'):
        case = self.case(name)
        sid = 'iter-' + name + '-' + uuid.uuid4().hex
        flags = ['run', '--bare', '--tools', 'Bash', '--session-id', sid, '--metrics-log', str(case.root / 'metrics.jsonl'),
                 '--system', 'Use the provided tools to finish the original task and preserve completed work.']
        if name == 'explicit':
            flags += ['--max-iter', '3']
        flags += ['--', f'ITERATION_CASE={name} Complete {self.args.steps} ordered tool steps.']
        process = self.start(case, flags, self.environment(case))
        code = process.wait(timeout=self.args.case_timeout)
        for proc, stdout, stderr in self.processes:
            if proc is process:
                stdout.flush(); stderr.flush()
        output = (case.root / 'stdout.log').read_text()
        error_output = (case.root / 'stderr.log').read_text()
        count = len(case.effect_lines())
        metrics = [json.loads(line) for line in (case.root / 'metrics.jsonl').read_text().splitlines()]
        terminal = [record for record in metrics if record['stop_reason'] != 'tool_use']
        proof = {'exitCode': code, 'sideEffectCount': count, 'requests': len(case.requests), 'terminalMetrics': terminal,
                 'stderrMetricsCount': len(re.findall(r'^\[metrics\]', error_output, re.M))}
        if name == 'cli':
            require(code == 0 and f'ITERATION_COMPLETE case=cli steps={self.args.steps}' in output, 'plain CLI did not complete: ' + error_output[-1500:])
            require(len(terminal) == 1 and proof['stderrMetricsCount'] == 1, 'plain CLI emitted duplicate terminal completion')
            proof['effectsSha256'] = self.verify_effects(case, self.args.steps)
            proof['history'] = self.verify_history(case, sid, self.args.steps)
            exported = subprocess.run([str(self.binary), 'sessions', 'export', sid], cwd=case.workspace,
                                      env=self.environment(case), input='', text=True, capture_output=True, timeout=15)
            (case.root / 'session-export.jsonl').write_text(exported.stdout)
            (case.root / 'session-export.stderr.log').write_text(exported.stderr)
            require(exported.returncode == 0 and case.tool_id(self.args.steps) in exported.stdout, 'actual CLI session export failed after completion')
            proof['sessionExportExitCode'] = exported.returncode
            proof['sessionExportSha256'] = hashlib.sha256(exported.stdout.encode()).hexdigest()
            seams = [record['completedResults'] for record in case.requests if record['completedResults'] in [100, 200, 300]]
            require(seams == [100, 200, 300], f'expected three sequential seams, got {seams}')
            require(proof['history']['continuationReminders'] == self.args.steps // 100, 'durable continuation reminders mismatch')
            proof['seams'] = seams
            wire_reminders = [record['continuationReminders'] for record in case.requests if record['completedResults'] in seams]
            require(wire_reminders == [1, 2, 3], 'continuation reminders did not reach the next actual model request')
            proof['wireReminderCountsAtSeams'] = wire_reminders
        elif name == 'explicit':
            require(0 < count < self.args.steps and len(case.requests) <= 12, 'explicit max-iter became unbounded')
            self.verify_effects(case, count)
            require('ITERATION_COMPLETE' not in output, 'finite cap falsely claimed full completion')
            require(not any(record['continuationReminders'] for record in case.requests), 'explicit finite cap injected soft continuation')
        else:
            require(code != 0 and count == 0 and not any(record['continuationReminders'] for record in case.requests), 'HTTP error was hidden by continuation')
            require(1 <= len(case.requests) <= 3, 'HTTP recovery exceeded controlled bound')
        self.report['cases'][name] = proof
        atomic_json(self.output / 'report.json', self.report)
        print(f'PASS {name}: tools={count} requests={len(case.requests)} exit={code}', flush=True)

    def desktop(self, name='desktop'):
        case = self.case(name)
        port = free_port()
        self.ports.append(port)
        web = f'http://127.0.0.1:{port}'
        process = self.start(case, ['desktop', '--web', '--port', str(port)],
                             self.environment(case, checkpoint=3 if name == 'cancel' else 100))
        def health():
            require(process.poll() is None, 'Desktop backend exited before readiness')
            try:
                return http(web + '/api/health', timeout=2)[0] == 200
            except (URLError, TimeoutError, OSError):
                return False
        wait_for(health, 'Desktop backend startup')
        stream = EventStream(web, case.root / 'sse-events.jsonl')
        self.streams.append(stream)
        code, session = http(web + '/api/sessions', {})
        require(code == 201, 'Desktop session allocation failed')
        sid = session['id']
        with ThreadPoolExecutor(max_workers=1) as executor:
            pending = executor.submit(http, web + '/api/turns', {'sessionId': sid,
                'input': f'ITERATION_CASE={name} Complete {self.args.steps} ordered tool steps.'}, self.args.case_timeout)
            wait_for(lambda: bool(case.requests), 'isolated worker first actual request')
            status = http(web + '/api/status?sessionId=' + sid)[1]
            require(status.get('isolatedTurnsEnabled') and sid in status.get('isolatedTurnSessions', []),
                    'Desktop turn did not use a real isolated CLI worker')
            require(process.stdin is not None and not process.stdin.closed, 'Desktop parent stdin PIPE closed early')
            if name == 'cancel':
                require(case.gate_entered.wait(30), 'cancel did not reach post-seam fourth model request')
                self.verify_effects(case, 3)
                stop_code, stopped = http(web + '/api/stop', {'sessionId': sid})
                require(stop_code in [200, 202], 'Desktop cancel was rejected')
                code, result = pending.result(timeout=45)
                require(code == 200 and result.get('stopped') is True, 'cancelled turn did not settle as stopped')
                before = len(case.requests)
                time.sleep(.2)
                require(len(case.requests) == before == 4, 'cancelled continuation made another model request')
                self.verify_effects(case, 3)
                history = self.verify_history(case, sid, 3, completed=False)
                require(history['continuationReminders'] == 1, 'cancel case did not cross exactly one seam')
            else:
                code, result = pending.result(timeout=self.args.case_timeout)
                require(code == 200 and not result.get('stopped') and
                        f'ITERATION_COMPLETE case={name} steps={self.args.steps}' in result.get('text', ''),
                        f'Desktop did not return the normal final reply: {code} {result}')
                self.verify_effects(case, self.args.steps)
                history = self.verify_history(case, sid, self.args.steps)
                require(history['continuationReminders'] == self.args.steps // 100,
                        'Desktop durable continuation reminder count mismatch')
        turn_http_status = code
        done = wait_for(lambda: stream.session_events(sid, 'loop_done'), 'single Desktop terminal SSE')
        require(len(done) == 1, 'Desktop repeated loop_done')
        require(done[0].get('stopReason') == ('stopped' if name == 'cancel' else 'end_turn') and
                not done[0].get('incomplete'), 'Desktop normal/cancel terminal reason corrupted')
        checkpoints = [event for event in stream.session_events(sid, 'context_injected')
                       if event.get('source') == 'iteration_continuation']
        require(len(checkpoints) == (1 if name == 'cancel' else self.args.steps // 100),
                'Desktop lost source metadata or rendered a partial checkpoint stream')
        code, loaded = http(web + '/api/sessions/' + sid)
        require(code == 200 and loaded.get('messages'), 'durable Desktop session is not readable over HTTP')
        require(loaded['session']['status'] == ('stopped' if name == 'cancel' else 'completed'),
                'Desktop persistent status does not match terminal result')
        if name == 'desktop':
            wire_reminders = [record['continuationReminders'] for record in case.requests if record['completedResults'] in [100, 200, 300]]
            require(wire_reminders == [1, 2, 3] and len(case.requests) == self.args.steps + 1, 'Desktop model wire missed a seam or repeated a request')
        else:
            wire_reminders = [case.requests[-1]['continuationReminders']]
        proof = {'turnHttpStatus': turn_http_status, 'sessionReadHttpStatus': code, 'finalText': result.get('text', ''),
                 'storedSessionStatus': loaded['session']['status'],
                 'effectsSha256': hashlib.sha256(case.effects.read_bytes()).hexdigest(),
                 'wireReminderCountsAtSeams': wire_reminders, 'sideEffectCount': len(case.effect_lines()), 'requests': len(case.requests),
                 'history': history, 'loopDoneCount': len(done), 'loopDone': done[0],
                 'checkpointSSECount': len(checkpoints), 'actualIsolatedWorker': True,
                 'parentStdinPipeHeldOpen': not process.stdin.closed, 'sessionId': sid}
        self.report['cases'][name] = proof
        atomic_json(self.output / 'report.json', self.report)
        print(f'PASS {name}: tools={proof["sideEffectCount"]} requests={proof["requests"]} loop_done={len(done)}', flush=True)

    def cleanup(self):
        errors = []
        for stream in self.streams:
            try:
                stream.close()
            except BaseException as error:
                errors.append('SSE cleanup: ' + repr(error))
        for process, stdout, stderr in self.processes:
            try:
                if process.poll() is None:
                    os.killpg(process.pid, signal.SIGINT)
                    try:
                        process.wait(timeout=15)
                    except subprocess.TimeoutExpired:
                        os.killpg(process.pid, signal.SIGKILL)
                        process.wait(timeout=5)
                if process.stdin:
                    process.stdin.close()
            except BaseException as error:
                errors.append('process cleanup: ' + repr(error))
            finally:
                stdout.close(); stderr.close()
        try:
            self.model.close()
        except BaseException as error:
            errors.append('model cleanup: ' + repr(error))
        closed = {}
        for port in self.ports:
            with socket.socket() as sock:
                sock.settimeout(.5)
                closed[str(port)] = sock.connect_ex(('127.0.0.1', port)) != 0
        self.report['auxiliaryModelRequests'] = self.model.auxiliary_requests
        self.report['cleanup'] = {'errors': errors, 'listenersClosed': closed,
            'sseReadersStopped': all(not stream.thread.is_alive() for stream in self.streams),
            'ownedProcessExitCodes': [process.returncode for process, _, _ in self.processes],
            'allOwnedProcessesStopped': all(process.poll() is not None for process, _, _ in self.processes),
            'modelThreadStopped': not self.model.thread.is_alive()}
        if errors or not all(closed.values()) or not self.report['cleanup']['sseReadersStopped'] or not self.report['cleanup']['allOwnedProcessesStopped']:
            self.report['passed'] = False
            raise AssertionError('owned test resource cleanup was incomplete: ' + repr(self.report['cleanup']))

    def run(self):
        try:
            for name in self.args.cases.split(','):
                require(name in ['cli', 'desktop', 'cancel', 'explicit', 'error401', 'error500'], 'unknown case: ' + name)
                if name in ['desktop', 'cancel']:
                    self.desktop(name)
                else:
                    self.cli(name)
            self.report['passed'] = True
            return 0
        except BaseException as error:
            self.report['error'] = repr(error)
            raise
        finally:
            try:
                self.cleanup()
            finally:
                atomic_json(self.output / 'report.json', self.report)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--binary', type=Path, required=True)
    parser.add_argument('--output', type=Path, required=True)
    parser.add_argument('--steps', type=int, default=305)
    parser.add_argument('--cases', default='cli,desktop,cancel,explicit,error401,error500')
    parser.add_argument('--case-timeout', type=int, default=240)
    parser.add_argument('--source-freeze', type=Path, help='parent-owned source/binary build provenance JSON')
    args = parser.parse_args()
    require(300 < args.steps <= 1000, 'steps must cross three 100-iteration seams and remain bounded')
    return Acceptance(args).run()


if __name__ == '__main__':
    raise SystemExit(main())
