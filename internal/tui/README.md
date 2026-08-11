# internal/tui

The Bubble Tea terminal UI. `Model` owns interactive state and the Bubble Tea
program goroutine owns its mutation. Most writes happen in `Update`; the
current `View` path also performs legacy lazy ID backfilling through
`ensureIDs`. Agent/provider work runs asynchronously from snapshots and
communicates back through messages or channels. `repl.go` is the non-TUI
fallback and does not share the Bubble Tea model lifecycle.

## Browse by concern

| Family | What it owns | Entry files |
|---|---|---|
| `tui*` | Model, update loop, event application, rendering, spinner, and styles | `tui.go`, `tui_update.go`, `tui_events.go` |
| `render_*` | Transcript rows, tools, overlays, status chrome, queues, and welcome UI | `render_chrome.go`, `render_message.go`, `render_tool.go` |
| `keybind_*` | Root-screen input dispatch and submit/permission/session behavior | `keybind_main.go`, `keybind_submit.go` |
| `cmd_*` | Slash-command implementations | `commands.go`, `command_catalog.go` |
| `model_*` | Model choices, live state, and switching | `model_choices.go`, `model_state.go`, `model_switch.go` |
| `screen/` | Full-screen help, history, picker, detail/diff, permissions, effort, model, multi-agent, resume, theme, and related screens | `screen/screen.go` |
| `overlay/` | Stackable modal overlays | `overlay/overlay.go` |
| `list/` | Generic focused/scrollable list primitives | `list/list.go` |
| `keybind/` | Vim-style key parsing and resolution | `keybind/resolver.go` |
| `i18n/` | Chat-side localization catalog | `i18n/i18n.go` |

Other root files are feature-oriented: image paste/mentions, queueing,
conversation export, session resume/switching, transcript search, terminal
reset/progress, voice, clipboard/yank, and REPL support. Use `rg --files
internal/tui` rather than relying on a static file count.

## Where do I find...

- **The Model struct** → `tui.go`
- **The Bubble Tea dispatcher and turn finalization** → `tui_update.go`
- **Agent event application** (thinking/text deltas, tool args/results,
  permissions, AskUser, usage) → `tui_events.go`
- **Submit and async turn launch** → `keybind_submit.go` and
  `runTurnAsync` in `tui_update.go`
- **Slash-command registration** → `commands.go` and `command_catalog.go`
- **Model switching** → `model_state.go` and `model_switch.go`
- **Transcript search and session resume** → `search_transcript.go`,
  `resume_hydrate.go`, and `screen/resume.go`
- **Terminal lifecycle** → `terminal_reset.go`, `terminal_drain_*.go`, and
  `term_progress.go`
- **Image input** → `image_paste.go` and `at_file_image.go`

## UI-thread ownership and async-turn invariants

- Background goroutines must receive every needed value as a launch-time
  snapshot and must not read or write `m.*`. Model mutations stay on Bubble
  Tea's program goroutine.
- `View` is not currently referentially pure: `tui_render.go` calls
  `ensureIDs`, which may assign IDs in `messages`, `toolEvents`, and `msgSeq`.
  Do not add more render-time mutation; new state transitions belong in
  `Update`, and removing this lazy-backfill exception is preferable.
- `runTurnAsync` is intentionally a free function. Its caller snapshots the
  context, loop, event channel, and done channel before launch. Clearing the
  Model's `turnCancel` field and `finalizeTurn` remain on the Bubble Tea
  thread; the worker may invoke its copied cancel function when it exits.
- After cancellation, `runTurnAsync` stops forwarding new UI events but keeps
  draining the loop's private event stream. This lets blocked emitters exit
  and prevents a leaked turn goroutine.
- The done signal is sent only after the loop event stream closes. The update
  path drains the stable event tail before `finalizeTurn`, so completion
  cannot overtake already-forwarded deltas.

Run `go test -race ./internal/tui/...` when changing these boundaries; normal
unit tests cannot prove the absence of cross-goroutine Model access.

## Streaming and backpressure

One `Update` call drains at most 64 agent events. If the channel still has a
backlog, it schedules a 1 ms continuation so queued keyboard and mouse input
can run between bursts. A ready done signal is deferred until the backlog and
final stable tail are applied.

Streaming tool arguments are tracked in `toolArgsStreams` by `ToolUseID`, not
in one global buffer. A result clears only its own preview, preventing
parallel tool-call JSON fragments from mixing. Preserve that keying for any
new streamed tool event.
