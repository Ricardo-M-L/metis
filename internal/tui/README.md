# internal/tui

bubbletea-based terminal UI. `Model` owns all interactive state; `Update`
is the sole writer. Goroutines snapshot what they need at the call site
and return values via `tea.Msg`. See `tui.go` for the Model struct,
`tui_update.go` for the central dispatcher, `tui_events.go` for the
agent-event handler, `repl.go` for the non-TUI fallback path.

## File-naming convention (browse by prefix)

The package has ~80 production files. They organise by prefix:

| Prefix | Count | What it owns | Entry file |
|---|---|---|---|
| `render_*` | 11 | Visible output: status bar, transcript rows, code blocks, banners, chrome | `render_chrome.go` |
| `keybind_*` | 8 | Per-screen key handling that dispatches to handlers | `keybind_main.go`, `keybind_submit.go` |
| `cmd_*` | 8 | slash-command implementations the registry routes to | `commands.go` (registry) + `cmd_phase_c.go` |
| `tui_*` | 5 | Model + Update + event dispatcher + persistTail | `tui.go`, `tui_update.go`, `tui_events.go` |
| `statusline_*` | 2 | Status-bar composition | `statusline.go` |
| `model_*` | 2 | Model-switching modal / per-model UI tweaks | `model.go` |
| singletons | ~50 | Per-concern widgets / small features: `vim.go`, `voice.go`, `yank.go`, `picker.go`, `terminal.go`, `search.go`, `resume.go`, `reload.go`, `repl.go`, `image_paste.go`, `at_file_image.go`, etc. | one file per concept |

When a feature gets >2 files it earns a prefix.

## Sub-packages

| Path | Purpose |
|---|---|
| `screen/` | 11 full-screen overlays: help / history / picker / theme / detail / info / permissions / effort / body / model |
| `keybind/` | vim-mode key bindings (the heavy bit; root `keybind_*` is the light dispatchers) |
| `overlay/` | Modal overlays (BtwOverlay etc.) — stacked on top of the main view |
| `list/` | Generic scrollable list component used by pickers |
| `i18n/` | i18n catalog for chat-side strings |

## Where do I find...

- **The Model struct** → `tui.go`
- **The Update dispatcher** → `tui_update.go` (`func (m *Model) Update`)
- **Agent event handling** (text deltas, tool results, etc.) → `tui_events.go`
- **handleSubmit / runTurnAsync** (the message loop + async turn) → `keybind_submit.go`, `tui_update.go`
- **Slash-command registration** → `commands.go`
- **A specific render piece** (token counter, spinner, banner) → grep `render_*` then the concrete file name
- **REPL / non-interactive mode** → `repl.go`
- **Image paste handling** → `image_paste.go` + `at_file_image.go`

## Design invariants

- `Update` is the **sole writer** of `Model` state. Goroutines that need
  state pass it by value at launch (snapshot) and return updates via
  `tea.Msg`. Violations get caught by `go test -race ./internal/tui/...`
  — see `runTurnAsync` comment for the canonical example.
- Long-running work (LLM streams, agent ticks) lives in goroutines that
  forward through `m.eventCh` / `m.doneCh`. `finalizeTurn` is where the
  main thread reconciles their results.
