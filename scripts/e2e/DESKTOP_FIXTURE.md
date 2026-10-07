# Isolated Desktop verification

`desktop_fixture.py` compiles the real CLI into a fresh temporary directory and
starts its actual browser backend. It supplies a local Responses API SSE server,
an explicitly configured fake key, a new `METIS_HOME`, session store, and workspace.
It does not copy the operator's config or tasks, and drops ambient provider keys,
proxy settings and provider overrides from the child environment.

From the repository root:

```sh
python3 scripts/e2e/desktop_fixture.py --seed --duration 1800
```

The first JSON output gives `web_url`, `fixture_url`, `metis_home`, `workspace`,
`metis_bin`, `native_launcher`, and two seeded session IDs. Alpha and Beta have
distinct actual persisted histories and Markdown report files. `model-evidence.jsonl`
records actual upstream requests and terminal outcomes, excluding headers and
system prompts. All artifacts remain below the printed temporary root.
Each request is marked `agent_turn` or `memory_extraction`; extraction requests
receive `[]` and do not activate chat gates or count as additional foreground turns.

For repeated checks of an existing **test build**, pass `--binary /absolute/path`.
Do not use a shell alias, the installed user Desktop, or real user sessions as a
shortcut. To test the latest source, omit `--binary` and let the fixture rebuild.

## Controlled execution

Send `[gate:switching]` as part of a normal chat prompt. The fake provider emits
its first text delta, then waits. This creates a deterministic window to switch
sessions, check status, preserve drafts, navigate, or stop the turn. Release it:

```sh
curl -fsS -H 'Content-Type: application/json' \
  -d '{"gate":"switching"}' "$FIXTURE_URL/fixture/release"
```

`[slow:3]` delays the terminal events by three seconds. Both controls act on real
HTTP/SSE inference requests. `GET /fixture/state` reports started/completed/cancelled
calls; it does not synthesize application session state. `POST /fixture/stop`
terminates the fixture's owned CLI process and local model service. The default
maximum lifetime is 30 minutes; Ctrl-C also performs cleanup.

Send `[agent-fixture]` in an isolated chat to exercise a deterministic real
parent → `Agent` → child → parent Responses API exchange. The first model
response contains a finalized `Agent` function call with name `probe`,
`isolation: none`, and prompt `CHILD_FIXTURE_TASK_1`. The child returns two
streamed text deltas; the parent's next model request, after its
`function_call_output`, receives a final answer. The fixture-local config
allows `Agent`, `Read`, and `Artifact` without a permission prompt inside the
isolated fixture. These rules do not change the operator's configuration.
Add `[gate:subagent]` to the same prompt to pause the child's stream after its
first delta; release it through `/fixture/release` with `{"gate":"subagent"}`.
The child also accepts `[slow:3]` to make the live detail easier to inspect.
Other prompts retain the normal echo behavior. The evidence file records the
phase names (`parent_call`, `child_stream`, `parent_final`) and submitted test
input; it does not capture request headers, configured provider credentials,
system prompts, or tool output. To check this
contract without starting METIS, run
`python3 -m unittest discover -s scripts/e2e -p 'test_desktop_fixture.py'`.

## Turn disclosure and footer acceptance

Send `[activity-fixture]` to run three actual Responses requests. The first two
emit a progress message followed by `Read` of a fixture-owned Markdown file; the
third emits a final answer. The fixture allows `Read` as well as `Agent` and seeds
the two files only in its temporary workspace. Use this to check that collapsing
a completed turn hides all progress messages and tools while preserving the final
answer, and that expanding restores the original order. Every response supplies
128 input tokens (64 cached) and 16 output tokens through the normal provider
protocol; the UI must obtain statistics from METIS accounting, not injected DOM.
Elapsed times and context estimates are produced by the actual backend.

## Point edits on HTML Artifacts

Send `[artifact-edit-fixture]` to create a genuine static HTML Artifact titled
`点选修改验收` through the model's `Artifact.create` function call. The sample
contains a blue-violet `探索工作成果` CTA (`a` with `role="button"`), a title,
explanatory text, and three small information columns. It uses no scripts,
external assets, URLs, or active form controls. METIS sanitizes and persists the
document through its ordinary Artifact tool; the fixture never edits store files.

In the Artifact preview, enable point editing, select that CTA and request:

```text
把按钮改成青绿色，文案改为开始体验
```

The annotation backend reconstructs the selected target from the verified saved
version, then produces a canonical `metis_artifact_annotation` fenced JSON
reference. The controlled provider parses that stable JSON object and emits a
real `Artifact.read` call for the referenced version. Only after receiving the
actual tool output does it emit `Artifact.update` with `expected_version` equal
to the selected base version. It uses the returned HTML, changes the sample CTA
color to `#0f9d83` and text to `开始体验`, and waits for the update tool result
before announcing the new version. Failed reads or updates do not receive a
success answer. This is a deterministic sample edit, not a general-purpose model
that interprets arbitrary design instructions.

For this opt-in exchange, `model-evidence.jsonl` additionally records the canonical
annotation payload, emitted tool names/arguments, and the fixture-owned tool
results actually submitted in Responses API requests. This lets acceptance verify
the artifact ID, version, digest, target, user instruction, read/update order and
conditional version. Request headers and system prompts remain excluded.

To verify the HTTP/SSE provider contract without starting METIS:

```sh
python3 -m unittest discover -s scripts/e2e -p 'test_*fixture.py'
```

After starting a fixture with the latest source, verify the actual backend APIs:

```sh
python3 scripts/e2e/artifact_annotation_check.py /absolute/fixture/root/fixture.json
```

The checker rejects non-fixture state or non-loopback endpoints. It creates its own
session, invokes the real create/read/update tools through `/api/turns`, checks the
target/digest reference and preserved version 1, and verifies rejection of stale
references, unknown targets and cross-session reads. It leaves the test session
available for separate native Desktop visual checks and writes
`artifact-annotation-report.json` below the fixture root. The checker performs no
browser or native UI automation; an API pass does not replace clicking the target,
submitting feedback and inspecting both versions in Desktop.

For a negative compatibility probe, add `--omit-artifact-expected-version` when
starting a fresh fixture (also supported with `--no-web`). This opt-in flag removes
`expected_version` only from the provider's `Artifact.update` arguments. The
canonical annotation, selected base version, create/read calls and other markers
are unchanged; `fixture.json` records `omit_artifact_expected_version: true`.
Use the actual recorded update arguments and preserved versions to check that
METIS's host enforces the edit-context conditional version even when the model
omits that field. This flag is for negative compatibility acceptance only and
defaults to false. It does not simulate successful storage or bypass host guards.

## Native Desktop without Apple signing

Build the frontend and native wrapper using the repository's installed Wails CLI:

```sh
cd metis-desktop
/Users/yujun2/go/bin/wails build -m -nosyncgomod -skipbindings -platform darwin/arm64
```

This uses neither `package-macos-signed.sh` nor notarization. To avoid sharing a
backend with the browser test, start a **separate** fixture:

```sh
python3 scripts/e2e/desktop_fixture.py --no-web --duration 1800
```

Run its printed `native_launcher` in another terminal. That script launches the
built app executable directly with its isolated environment, explicit workspace,
and exact temporary CLI path. The native app chooses its own loopback backend
port. Quit that test window before stopping the fixture; do not terminate another
installed Desktop window. The launcher does not install or replace an application.

## Required behavior checks

- Stream in Alpha, switch to Beta, and verify Beta's history and artifact contain
  no Alpha fragments; finish Alpha while detached and return to its final history.
- Repeat stop and completion while switching; status must remain associated with
  the running session, and list status must agree with the persisted transcript.
- Create, rename, archive, restore, and delete disposable sessions before and
  after switching. Reload the page and confirm persisted results.
- Type different unsent drafts in Alpha and Beta. Navigate through session,
  settings, and automation views using back/forward controls. Verify drafts,
  selection and view agree after each transition.
- Create a short interval automation through the real application API/UI and
  wait for a due-time fire. Confirm actual provider evidence, run history and
  transcript. Pause, edit, resume, run-now and delete it; verify each persisted
  transition and no additional fire after deletion. A source-string assertion or
  manually invoking the callback is insufficient evidence of scheduling.
- Reopen native Desktop with the same isolated home and verify saved automation
  definitions and histories. Test shutdown while a controlled turn is running and
  ensure only fixture-owned child processes exit.

Fixture model responses are deterministic, so these tests verify application
behavior and transport, not production model quality or provider availability.
