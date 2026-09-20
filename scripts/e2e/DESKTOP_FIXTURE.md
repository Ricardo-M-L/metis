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
