# Native Desktop verification — 2026-09-20

The acceptance target was the macOS arm64 Wails application at
`metis-desktop/build/bin/METIS.app`, launched with the newly built root CLI.
The checks below used the actual native window and its embedded WKWebView.

## Isolation and scope

`scripts/e2e/desktop_fixture.py --no-web` supplied a temporary `METIS_HOME`,
workspace, real session/artifact stores, and deterministic local Responses API
SSE provider. No installed application or real user sessions were replaced.
The fake provider controls stream timing; it does not mock the application,
its cron scheduler, run records, or persisted conversations.

Local Codex reference source was fast-forwarded to `5c5308fc9` for comparison.
The public repository does not include the complete Desktop frontend.

## Observed in the native application

- Settings occupy the same window with a settings sidebar. Back/forward moves
  through General and Appearance; Return to application restores the previous
  conversation. An unsent draft survives these transitions.
- Light and dark theme changes apply to the native shell and embedded content.
  The empty-conversation navigation header remains at the top of the window.
- While Beta streams, selecting Alpha shows only Alpha's chat and trajectory.
  The running indicator stays on Beta; completed sessions use neutral icons.
  The stop control explicitly names the other running session.
- A task created through the native form actually fires at its due time, creates
  a completed run record and a real conversation. Editing its name and prompt,
  manual execution and opening the newly created conversation were exercised.
  Opening a new run refreshes its sidebar entry and conversation title.
- Restarting the native application preserves the task, scheduler preference
  and run records. The model is labelled as the task workspace's default model.
  The automatic execution limit is distinguished from extra manual runs.
- In the final artifact fix, Beta continues streaming while Alpha's artifact
  opens successfully. Selecting v1 and navigating back to v3 and forward to v1
  displays the correct persisted contents. Switching directly from that preview
  to Beta shows Beta's chat with a matching title and selected tab.

These native checks exposed additional failures that were fixed during testing:
historical artifact reads during another active turn; missing `sessionId` on
artifact detail/preview/download/export requests; missing sidebar metadata for
cron-created sessions; a stale artifact-view title after switching sessions;
and an inherited completed-status background that obscured the new icon.

## Automated validation

Commands run successfully:

```sh
go test ./internal/webui ./internal/desktop ./cmd/metis
go test -race ./internal/agent ./internal/webui ./cmd/metis \
  -run 'Test(CronRuns|Automations?|CronRunArgs|RecordedCron|ExecuteCronJobSavesActualConversation|CronFinalOutput)' -count=1
make build
(cd metis-desktop && go test ./...)
(cd metis-desktop && wails build -m -nosyncgomod -skipbindings -platform darwin/arm64)
git diff --check
```

The final scoped suite reported 879 passing tests/subtests and 7 skips. The
macOS bundle also passed `codesign --verify --deep --strict`.

Coverage includes asynchronous session/trace/artifact selection, navigation
history, automation form state, real JavaScript-to-HTTP artifact ownership,
per-job cross-process exclusion, crash recovery, bounded process shutdown,
workspace binding, private run storage and output redaction. The artifact
regression checks detail, multiple previews, download, export and external URL
resolution while another session runs; non-active deletion still returns 409.

A separate real CLI/HTTP end-to-end fixture passed session CRUD, due-time
execution after an edit, manual run admission, duplicate/delete protection,
durable history and absence of further scheduled execution after deletion.
This is supplementary backend evidence, not a replacement for native acceptance.

## Limits

- No production model credentials or external inference provider were tested.
- Seven environment-dependent live/sample tests were skipped by the scoped Go
  suite; no claim is made that the complete repository test suite ran.
- Native acceptance was on macOS arm64, not Windows or Linux.
- Wails used local ad-hoc signing; Apple certificate signing and notarization
  were not performed.
- Local scheduled execution requires the Desktop backend to be running. There
  is no new system daemon or guarantee of execution during sleep or after quit.
- The Mac locked during the final extra Beta-gallery check. That final UI click
  could not be completed; it does not invalidate the native checks listed above.
  The subsequent small tooltip change (using the same live status label as the
  icon) passed a renderer regression and the scoped suite, but could not receive
  another native visual check while the Mac remained locked.

No commit, remote push or release publication was performed for this change.

## Sidebar refinement after the native checks

Following visual feedback, resting sessions (including completed and manually
stopped sessions) now keep an empty fixed gutter instead of a persistent glyph.
Only running or attention states draw a 12px outline icon, with no checkbox,
colored background or halo. The row's accessible name and hover details retain
the status even when no icon is drawn.

The existing session-selection/navigation tests, JavaScript syntax check,
`git diff --check` and `make build` passed after this refinement. The Mac was
still locked, so the revised appearance has not yet received a native visual
check; the screenshots from the earlier acceptance apply to the earlier style.
