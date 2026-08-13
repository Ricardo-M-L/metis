# Changelog

All notable changes to Metis are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and the project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

## [0.4.21] - 2026-08-13

### Fixed

- **blank assistant rows from compatibility streams**: leading blank-line
  separators between reasoning and content are buffered across chunks and
  removed before live display or persistence; whitespace-only text before a
  tool call no longer renders as a standalone assistant bullet.
- **custom-provider image capability overrides**: OpenAI-compatible models can
  declare `supports_vision = true|false` when public catalogs do not know their
  private model id. SenseNova 6.8 Flash Lite is also recognized from its
  vendor-declared text-and-image input metadata, and capability warnings no
  longer label an unknown model as objectively text-only.
- **composer mouse selection**: dragging across the input now selects complete
  Unicode grapheme clusters, copies on release, and keeps a visible highlight;
  a bare click positions the caret, while `Ctrl+C` copies an active composer
  selection before falling back to interrupt or quit behavior.

## [0.4.20] - 2026-08-12

### Added

- **complete custom-provider first-run setup**: the interactive auth wizard
  now collects the wire protocol, base URL, model id, and API key, persists the
  non-secret profile separately from credentials, and accepts terminal paste
  in every text field.
- **optional terminal-native mouse handling**: `METIS_DISABLE_MOUSE=1` leaves
  TUI mouse capture off while preserving cell-motion tracking by default.

### Fixed

- **safe process control for model shell tools**: Bash, Workflow, and Monitor
  now reject direct kill-family shell commands, common wrapped and nested
  forms, and syntax the guard cannot inspect. Models must stop registered jobs
  through `BashKill(job_id)`, preventing broad `kill -9` commands from killing
  Metis; the OS sandbox remains the boundary for arbitrary executable code.
- **clean project-scoped resume lists**: resume pickers now hide header-only
  and sub-agent records, default to the current working directory, sort by
  recent activity, derive titles from the first user prompt, and treat Esc as
  cancellation instead of silently starting another empty session.
- **first-run provider activation**: a provider created by the wizard is now
  reloaded and selected in the same process instead of falling back to the
  pre-wizard default. API-key input is visible and editable for correction,
  and custom transports report their effective default model consistently.
- **credential-to-endpoint binding**: setup validates the final merged endpoint
  before storing a key, rejects ambiguous existing credentials, and no longer
  reuses third-party compatibility keys for Anthropic or OpenAI.

## [0.4.19] - 2026-08-12

### Changed

- **cleaner TUI transcript rhythm**: top-level chat entries now use one
  consistent blank-row gap while each tool invocation and its result remain a
  compact block. Bash timeouts render once as a semantic result and retain any
  useful output produced before the timeout.
- **canonical multi-agent view**: the live tree screen is now `/agents` with
  `/av` as its alias; the older text roster and `/agents-view` command were
  removed from the TUI command surface.
- **language guidance**: response and reasoning-language matching now lives in
  its own early, cached base-prompt section instead of being buried in style
  guidance.

### Fixed

- **compaction lifecycle and token state**: ending a compaction attempt is now
  distinct from successfully applying a smaller context. Failed/no-op attempts
  only clear progress UI, while successful automatic or manual compaction
  refreshes the context estimate and resets stale pre-compaction token/cost
  counters.
- **context percentage display**: status percentages are rounded and clamped
  to the conventional `0-100%` range.

## [0.4.18] - 2026-08-11

### Added

- **Windows CLI releases**: GitHub Releases now include checksummed
  `windows-amd64` and `windows-arm64` ZIPs. `install/install.ps1` resolves
  public releases anonymously, validates the SHA-256 sidecar and archive
  shape, stages the executable, and preserves the previous install on a
  failed replacement.
- **Windows CI**: native Windows vet/build/test, installer regression tests,
  CLI smoke checks, and an arm64 cross-build now gate changes. Tag releases
  also run anonymous Linux and Windows installation smokes.
- **cross-platform background updates**: interactive TTY chat now starts the
  native updater off the startup path, checks immediately and every 30 minutes,
  and installs verified releases for the next invocation on macOS, Linux and
  Windows. `METIS_NO_UPDATE_CHECK=1` disables the automatic loop without
  disabling explicit `metis update` commands.

### Changed

- **cross-platform runtime**: Unix-only process-group, signal, PID liveness,
  and terminal-drain behavior is isolated behind platform-specific files so
  the CLI builds cleanly for both Windows architectures.
- **documentation refresh**: public, contributor, security, architecture,
  CLI/env/slash, desktop/editor, and internal package documentation now tracks
  the current runtime and avoids unstable hand-maintained inventory counts.
- **managed native installs**: the curl and PowerShell bootstraps and
  `metis update` now share a staged, checksummed version lifecycle and stable
  launcher. Cleanup keeps the current version plus the two newest rollback
  versions, protects versions still used by running processes, and defers
  deletion of locked Windows launcher backups until a later cleanup.

### Fixed

- **public installation and self-update**: `install/install.sh`,
  `install/install.ps1`, background updates, and `metis update` now work
  anonymously against the public GitHub release. `METIS_GITHUB_TOKEN` and
  `GITHUB_TOKEN` remain optional for higher API rate limits.
- **config initialization**: `metis config init` now writes to the canonical
  `METIS_HOME`/`~/.metis` path and uses the current safe Bash output default.
- **prompt-dump redaction**: Gemini's `x-goog-api-key` header is now redacted
  before an explicitly enabled request dump is written to disk.

## [0.4.17] - 2026-08-11

### Fixed

- **sub-agent stability**: background sub-agents no longer crash when the
  parent turn ends. Two coupled bugs fixed in internal/tools/builtin/agent.go:
  - forwardSubAgentEvent now recovers from "send on closed channel" - the
    parent's per-turn event channel is closed when the turn's loop.Run
    returns, and a background agent outliving that turn used to panic on the
    next forwarded tool event.
  - background sub-agent contexts are detached from the parent turn with
    context.WithoutCancel, so turn teardown no longer kills them
    ("killed: context canceled"). They are now session-scoped, terminated
    only by their own timeout, SubAgentStop, or session exit
    (Roster.Reset / Roster.CancelAll).
- **TUI**: renderInfoBox no longer draws an empty cyan bordered box when
  its body has no visible content (was producing ~20-line blank rectangles
  in the transcript).

### Added — Bash auto-background + job pool (claude-code parity, 60s threshold)

- New `internal/jobs/` package: a process-wide pool tracking
  background bash commands. Each job has a `bg_<8hex>` ID, a status
  state machine (running/completed/failed/killed), disk-backed
  stdout/stderr at `~/.metis/jobs/<id>.out`, and a notification
  channel that the agent loop drains at iteration boundaries.
- `Bash` tool gains `run_in_background: bool` input. When true, the
  command spawns into the pool immediately and the model gets a
  `job_id` reply. Output keeps growing on disk; the model uses
  `BashOutput` to read it.
- 60-second auto-background timer for foreground bash commands. If a
  command outlives `AutoBackgroundThreshold = 60s`, the cmd is
  promoted into the pool (via `jobs.Registry.Adopt` — the process
  keeps running; nothing restarts) and the model gets a
  "moved to background" reply with the `job_id`. Mirrors claude-
  code's `ASSISTANT_BLOCKING_BUDGET_MS` pattern (15s on their side;
  60s here per user choice — npm-install / go-test sized commands
  shouldn't be misfired).
- Sleep blacklist: bare `sleep N` (N ≥ 2 seconds) and
  `sleep N && rest` are rejected at the tool boundary with a
  diagnostic. Sub-2s pacing, sleeps inside pipelines / subshells /
  loops are allowed. Mirrors claude-code's
  `detectBlockedSleepPattern`.
- 3 new model-facing tools wired by `AttachJobsRegistry`:
  - `BashList` — JSON snapshot of the pool (id, status, command,
    started, elapsed, exit_code on terminal jobs)
  - `BashOutput` — reads the on-disk capture of a job, with
    head-truncation for large logs (`tail_max` parameter, default
    50 KiB)
  - `BashKill` — SIGTERM with 2s grace, SIGKILL via context-cancel
- `<job_notification>` envelope: at every Run iteration boundary the
  loop drains the pool's notification channel and synthesizes a
  user-message system-reminder summarizing finished jobs. Mirrors
  claude-code's `<task_notification>` envelope. Multiple jobs that
  finish in the same window collapse into one message.
- TUI status bar: `⚙ N jobs` chip lights up while the pool has any
  job in StatusRunning, sourced via `m.loop.Jobs.List()`.
- 18 new tests:
  - `internal/jobs/jobs_test.go` (10): happy-path spawn, notification
    envelope, failed exit, kill state, unknown-id error, terminal
    no-op, stable list order, output truncation, multi-line desc
    folding, ID format
  - `internal/tools/builtin/bash_jobs_test.go` (8 + 10 sub-cases):
    each tool's required-input / unknown-id / happy path, plus the
    sleep blacklist matrix (10 cases covering bare / chained /
    pipeline / subshell / loop / sub-2s).
- Wiring: `cmd/metis/main.go` builds one `jobs.Registry` per process
  and threads it through `BuildToolRegistry` (for the three reader
  tools) AND `BuildAgentLoop` (for the notification drainer).
  Sub-agents and headless tests pass nil → feature gracefully
  disables.

NOT done in this round (deferred):
- `Ctrl+B` manual-promote keybinding. Cross-goroutine signal
  plumbing is non-trivial and the 60s auto-timer covers most cases;
  filed as follow-up.

### Fixed — `[provider.custom.<id>] api_key = "..."` was silently dropped

- `internal/config/config.go::ProviderRaw` was missing the `APIKey`
  field entirely (only `APIKeyEnv` existed). TOML parsers silently drop
  unknown keys, so users who put an inline `api_key = "..."` in their
  custom-provider block got "missing API key for provider X" later
  with no hint that the field they wrote wasn't being read. Built-in
  providers (anthropic / openai / gemini) had the inline path forever
  and the docstring claimed it was a 3-tier chain "applied uniformly"
  — only custom was the gap.
- Added `APIKey string \`toml:"api_key"\`` to `ProviderRaw` and the
  matching final-fallback branch in `ResolveAPIKey`. Order is
  unchanged: env (api_key_env) → ~/.metis/auth.json → inline api_key.
- Discovered while wiring DeepSeek (2026-05-09): inline key written
  in config.toml didn't take, only auth.json worked.
- Added 4 unit tests covering the custom-provider chain
  (env / inline-only / auth-beats-inline / all-empty error). README
  updated to document the 3-tier chain explicitly with a commented
  inline example next to api_key_env.

### Added — Desktop notification channel matrix (claude-code parity)

- Replaced the single OSC 9 emitter with a 5-channel matrix mirroring
  claude-code's services/notifier.ts + ink/useTerminalNotification.ts:
  iTerm2 / iTerm2+Bell / Kitty / Ghostty / raw BEL. Auto-detected
  from `$TERM_PROGRAM` + `$KITTY_WINDOW_ID` + `$ALACRITTY_LOG`, or
  forced via `METIS_NOTIFY_CHANNEL` env var.
- Apple Terminal users now get BEL only when their profile's audible
  bell is off (visual-bell mode). The probe shells out to osascript
  + `defaults read com.apple.Terminal "Window Settings"` and parses
  the named profile's `Bell` field. Any failure is conservative —
  notification suppressed, never an unexpected ding.
- New OSC 9;4 progress bar: spinner start emits indeterminate state,
  turn-end emits clear (or error → clear on failure). iTerm2 / Ghostty
  / WezTerm / ConEmu light up the dock icon while the turn runs.
- 6-second recent-interaction guard: keypresses inside the chat surface
  call `tui.MarkUserInteraction()`, and `SendNotification` skips
  emission while the user is actively typing. Fix: `lastInteractionAt`
  initializes to zero (treated as "never") instead of `time.Now()` —
  the latter silently swallowed every notification fired in the first
  6 seconds of process lifetime, including probes/tests.
- Notification hooks: when the turn-end banner fires, `Loop.Hooks`
  also receives an `EmitNotification` event so users can wire their
  own `[[hooks.notification]]` shell command (e.g. terminal-notifier
  for richer macOS banners).
- tmux / GNU screen DCS passthrough wrapping for all OSC sequences.
  Raw BEL is intentionally NOT wrapped so tmux's bell-action window
  flag still fires.
- Manual tmux case `notify_channel_smoke` (NOT in default CASES list,
  costs one >30s LLM turn) verifies real-process emission lands in
  stderr correctly under tmux.
- 28 new unit tests in `internal/tui/notify_test.go` + 6 in
  `notify_apple_test.go`: SelectChannel match table, per-channel OSC
  shape, tmux DCS wrap (BEL exception), recent-interaction guard
  including the zero-value regression, escapeOSCText, SendProgress,
  scanBellInBlock parser.

### Changed — ToolSearch lazy mode is now driven by ENABLE_TOOL_SEARCH

- Replaced the old `tools.lazy_threshold` / count-based trigger
  with openclaude's full `tst-auto` env-var matrix
  (`src/utils/toolSearch.ts:49,172-198`). The lazy-mode decision
  is now read fresh from `ENABLE_TOOL_SEARCH` every dispatch turn,
  so users can `export` to flip behavior without restarting.
- Match table (lower-cased + trimmed):
  - unset / `auto` → auto, fires at 10% of context window
  - `auto:N` → auto at N% (1..99)
  - `auto:0` → always lazy (alias for `true`)
  - `auto:100` → never lazy (alias for `false`)
  - `true` / `1` / `yes` / `on` → always lazy
  - `false` / `0` / `no` / `off` → never lazy
- Why scales-with-window beats fixed counts: a 16k-window model
  chokes on 6k of MCP schemas (37% of budget) but a 200k-window
  model wouldn't notice (3%). One static threshold can't satisfy
  both; a percentage tracks the model's actual budget.
- Removed: `Tools.LazyThreshold` + `Tools.LazyTokenPercentage`
  TOML knobs, `Loop.LazyToolThreshold` + `Loop.LazyTokenPercentage`
  Go fields, and the legacy `applyLazySchema(specs, threshold)`
  count-based function. `Loop.ContextWindow` stays — it's the
  budget input for auto mode.
- New: `internal/agent/lazy_tools.go::parseEnableToolSearch` +
  `LazyMode` (Standard / Auto / Always) tri-state mirroring
  openclaude's `ToolSearchMode`.
- `metis config show` now prints `tools.enable_tool_search` (the
  current env value) instead of two TOML fields.
- 22 new test cases in `parseEnableToolSearch` covering every row
  of the match table. 8 dispatch-level tests in
  `dispatch_lazy_precedence_test.go` covering env=true/false,
  custom percentages, auto:0 ≡ true, auto:100 ≡ false, and
  unknown-ContextWindow conservative no-strip.

### Fixed — `Snip` was not idempotent (caught by 2026-05-08 e2e test)

- `internal/agent/compact.go::Snip` would re-truncate a tool_result
  that already carried a `[snipped: N chars omitted]` marker — every
  subsequent call ate ~2 more chars off the marker. Discovered by
  `TestSnipE2E_RepeatedSnipIsIdempotent` while validating the new
  tier-aware activation. Added a fast-path: if the marker is already
  present, skip the block. The "slow rot" was small in practice (a
  few chars per Snip cycle) but matters now that small-window tiers
  re-Snip more often.

### Added — Compaction tier e2e activation tests

- `internal/agent/compact_tier_e2e_test.go` — 4 tests that confirm
  the tier knob actually changes the LLM-facing message slice (not
  just an internal field). Same input through 16k vs 200k tiers
  produces different output sizes; 16k tier truncates a 1500-char
  tool_result to 200+marker; 200k tier passes it through untouched;
  repeated Snip is idempotent (the regression test that found the
  bug above).
- `internal/runtime/agent_loop_tier_test.go` — 5 integration tests
  that confirm `BuildAgentLoop` real wiring: a stub provider with
  `MaxContextTokens()=N` produces a Compactor whose
  `SnipMaxToolResultChars` and `SnipThreshold` match the expected
  tier; including the boundary case where `max_tokens` shrinks the
  effective input cap below the next tier boundary.

### Added — Hook three-state decisioning + claude-code parity (PR-D)

- `pkg/hook/hooks.go::ModifiedPreToolUse` gains `Halt bool` and
  `HaltReason string` so a PreToolUse hook can reject the current
  tool AND stop the whole turn in one decision (claude-code's "veto
  chain" pattern).
- `internal/runtime/config_hooks.go::parsePreToolUseResponse` now
  accepts THREE stdout shapes from a subprocess hook, in order of
  precedence:
  - claude-code envelope: `{"hookSpecificOutput": {
    "hookEventName": "PreToolUse",
    "permissionDecision": "deny"|"allow",
    "permissionDecisionReason": "...",
    "modifiedInput": {...}}}`
  - metis flat: `{"decision":"deny|allow|halt", "reason":"...",
    "modified_input":{...}, "halt":true}`
  - empty body → no-op (misbehaving hook can't accidentally
    block the agent)
- `runHookCommandWithCode` exposes the subprocess exit code; **exit
  49** is treated as halt (a user can `exit 49` from a one-liner
  without emitting JSON) and **exit 2** as block-tool. Wraps the
  legacy `runHookCommand` so existing call sites are unchanged.
- `internal/agent/loop.go` carries the halt signal through the
  loop: new `haltRequested`/`haltReason` fields, `haltTurn(reason)`
  setter (first-reason-wins, blank-doesn't-overwrite), `Run` clears
  state at entry, the main loop emits a final tool_result message
  and `EventLoopDone{StopReason: "halted_by_hook"}` after the
  current batch.
- 18 new tests: 12 parser-unit (envelope vs flat vs malformed vs
  empty), 3 subprocess-end-to-end (`exit 49` / `decision: halt` /
  envelope), 3 loop state.
- **Net effect**: a user's claude-code `~/.claude/settings.json`
  PreToolUse hooks now drop into metis's `~/.metis/config.toml`
  `[[hooks.pre_tool_use]]` blocks unchanged. Halt-via-exit-49
  scripts work identically.

### Added — `[tools] lazy_threshold` config knob

- `internal/config/config.go::Tools.LazyThreshold` (TOML key
  `[tools] lazy_threshold = N`) is now configurable. metis already
  shipped a working ToolSearch lazy-MCP-schema implementation
  (`internal/agent/lazy_tools.go` + `dispatch.go`) but the threshold
  was hardcoded to 20; users can now tune it (set 0 for default,
  negative to disable, any positive value to override).
- 7 new tests in `internal/agent/lazy_tools_handle_test.go`
  covering the inline `handleToolSearch` resolver: known-tool schema
  return, missing-name error, unknown-tool error, tool_use_id
  preservation across three ID formats, ToolSearch entry appended
  last (not inserted middle, so the cache breakpoint stays placed).
- Config test coverage for `lazy_threshold` parse and 0-default.
- README documents the knob with rationale.

### Added — borrowed from crush / openclaude / minimax-cli (PR-A + PR-B + PR-C)

- **`mode:auto` safe-bash allowlist** (`internal/permission/safe_commands.go`):
  read-only commands (`ls`, `cat`, `pwd`, `whoami`, `id`, `uname`,
  `git status / log / diff / blame / show`, `ps`, `df`, etc.) clear
  the permission prompt under `mode:auto` instead of pestering the
  user every turn. Shell metacharacters (`&&`, `||`, `;`, `|`, `>`,
  `` ` ``, `$(`), `sudo` / `doas` / `su`, and mutating git flags
  (`-D`, `--delete`, `--global`, `--system`, `-f`, `--force`) all
  still bounce to the prompt. Inspired by crush's
  `internal/agent/tools/safe.go`.
- **Agent-marker env vars**
  (`internal/tools/builtin/bash_env.go::filterEnv`): every bash
  invocation now sees `AGENT=metis`, `AI_AGENT=metis`, and
  `METIS=1`. Mirrors crush's `internal/shell/shell.go:90-95` so
  user dotfiles / Makefiles can detect "I'm running under an
  agent, suppress interactive prompts" via `[[ -n "$AGENT" ]]`.
- **`auth.json` permission self-heal** (`internal/auth/auth.go::Load`):
  on every load we check the file's perm bits; anything looser
  than 0600 triggers a single stderr warning and an in-place
  chmod back to 0600. Inspired by minimax-cli's
  `auth/credentials.ts`. Skipped on Windows (NTFS uses ACLs).
- **Exit-code classification** (`internal/exitcode/`): new package
  with `Classify(err) int`. `cmd/metis/main.go::main` now exits
  with the classified code instead of a blanket 1, so wrappers can
  switch on `$?`. Codes (stable, do-not-renumber): OK=0, General=1,
  Usage=2, Auth=3, Quota=4, Timeout=5, Network=6, Permission=7,
  IO=8, ContentFilter=10, plus the standard SIGINT=130 / SIGTERM=143.
  Three matching layers: typed (`errors.As` / `errors.Is`) →
  network typed (`*net.DNSError`, `*net.OpError`) → string
  heuristic (HTTP status, "rate limit", "no such host", …).
- **MiniMax business-code hints**
  (`internal/llm/anthropic/minimax_codes.go`): the bare
  `(NNNN)` suffix MiniMax appends to error messages now resolves
  to a friendly hint (`1028` → "quota: insufficient credits — top
  up at minimaxi.com/account", `2061` → "Speech requires Plus,
  Video requires Max", etc). Wrapped so `errors.Is` chains still
  work; the hint embeds classifier-friendly keywords
  ("rate limit", "content_filter") so the new exitcode layer
  above auto-classifies them.
- **Compaction window tiering** (`internal/agent/compact_tier.go`):
  the existing Snip layer's per-block char cap and trigger
  threshold are now selected from a 7-bucket table keyed on the
  active provider's effective input cap (16k / 32k / 64k / 128k /
  200k / 500k+). DeepSeek-V2 16k users get 200-char snip caps and
  0.60 fill-fraction triggers; Anthropic 200k users keep the
  defaults loose (3000 / 0.80). `ApplyWindowTier` is called once
  at Loop construction in `runtime/agent_loop.go::BuildAgentLoop`
  right after `MaxOutputTokens` is set. Inspired by openclaude's
  `compressToolHistory.ts` 7-tier structure.

### Added — sliding-window signature loop detection (crush parity)

- `LoopDetector.RecordStep(toolUses, results)` folds each step's tool
  batch into a SHA-256 of `(toolName \x00 stableJSON(input) \x00 result)`
  triples and tracks them in a sliding window. When any one signature
  appears more than `SignatureMaxRepeats` times within
  `SignatureWindowSize` steps, `ShouldAbort` flips to true and the
  agent loop emits `loop_detected` (per-stop reason) with a message
  identifying signature-loop vs the older count-based circuit breaker.
- Defaults match crush's `internal/agent/loop_detection.go`: window
  10, repeats 5. Tunable via `[loop_detection].signature_window` and
  `[loop_detection].signature_max_repeats` in `~/.metis/config.toml`.
- Loop detector is now **on by default** — the previous opt-in via
  `[loop_detection].enabled = true` is replaced by an opt-out flag
  `[loop_detection].disabled = true`. Reason: a 2026-05-08 live
  session showed the agent retrying `cd … && git rebase --continue`
  for 1h 18m with no halt because the user hadn't enabled the
  detector and `MaxIters = 50` quietly hadn't triggered. The legacy
  `enabled = true` config field is kept as a no-op so older
  `config.toml` files don't surprise users.
- `GlobalThreshold` default raised 60 → 80 to leave more headroom for
  legitimate long sessions; the signature detector is the first line
  of defense now (catches dead loops earlier; the global counter is
  the unconditional ceiling).
- 9 new tests in `internal/agent/loop_signature_test.go` cover:
  basic trip on identical-call repetition, distinct inputs not
  tripping, progressive output (growing log tails) ignored, text-only
  steps not poisoning the window, sliding correctly under mixed
  signatures, surviving textual recaps between retries (the user's
  exact loop pattern), stable signature ordering across map keys,
  abort-reason priority (signature beats global), and message format.

### Fixed — bare `metis -r` now opens the picker

- `metis -r` (no UUID arg, no `chat` subcommand) used to print
  `metis: run: prompt is required` and exit. The dispatch default
  fallback in `cmd/metis/main.go` routed any unrecognized first arg
  to `cmdRun`, which stripped the `-r` flag and then complained
  there was no prompt. The bare-resume picker only ran inside the
  `chat` flag-parser so the dispatch never gave it a chance.
- New `hasInteractiveIntentFlag` checks for `-r` / `--resume` /
  `-c` / `--continue` (and the `--resume=xyz` long form) anywhere in
  args, and routes to chat instead of run. Tested in
  `cmd/metis/dispatch_test.go` and end-to-end in
  `scripts/e2e/tmux_drive.sh::bare_resume_opens_picker`.

### Fixed — caught by 2026-05-08 video session

- The "queued × N: <peek>" indicator above the input box was sticky
  bottom chrome, so when the user wheel-scrolled the chat list to look
  at history the pill stayed pinned at the bottom. Removed the sticky
  pill entirely; the in-stream `(queued × N · Ctrl+C to clear): <peek>`
  notice (now embedding the user's prompt text) scrolls with the
  message stream, and a compact `◷ N queued` chip in the status bar
  keeps the count visible regardless of scroll position. Files:
  `internal/tui/tui_render.go` (drop pill call), `keybind_submit.go`
  (notice + peek), `render_chrome.go` (status-bar chip),
  `queue_scroll_test.go` (regression suite).
- The spinner's elapsed clock (`executing · cd "/Users/ricardo/…"
  (1h 18m · ↓ 93 tokens)`) was always sourced from
  `m.spinnerStartedAt`, which is the **whole-turn** clock. While a tool
  was in flight the user reasonably read `1h 18m` as "this cd command
  has been running for 1h 18m", but it was the turn that had been
  looping for an hour and the cd just started. `renderSpinnerStatus`
  now switches to the **tool-local** start time (most recent
  `Kind: "start"` entry in `m.toolEvents`) whenever `spinnerSub` is
  non-empty. After the tool finishes the display falls back to the
  turn clock, so an idle "thinking" stretch is still honest about how
  long the model has been deliberating.

### Added — resume-hint + bare `-r` picker

- After every clean exit (`/quit`, `Ctrl+D`, `Ctrl+C` at idle), metis
  prints a dim gray hint to stderr matching claude-code's format:

  ```
  Resume this session with:
  metis --resume <full-uuid>
  ```

- Bare `metis -r` / `metis --resume` (with no UUID arg) now opens an
  interactive picker over recent sessions. Type the number to resume,
  Enter or `q` to bail to a fresh session. Mirrors `claude -r`.

### Fixed — caught by side-by-side comparison run (cmp_drive.sh)

- `/help` overlay header used to render `metis vv0.1.3-21-gab7a825-dirty`.
  `versionLabel()` was hand-prepending `v` to a string the linker
  already passed in tag form, AND the long git-describe suffix never
  got stripped. Both call sites (help header + bottom status bar) now
  share `version.Short()`.
- `metis version` (no flag) used to print the full git-describe form
  (`v0.1.3-21-gab7a825-dirty (Metis · ab7a825)`) while the bottom
  status bar correctly showed `current: v0.1.3`. The default form now
  matches the status bar; `metis version -V` still prints the full
  build fingerprint for debugging.
- `internal/tui/render_tool.go::renderEditDiff` and `countEditDiff`
  read `input["old_string"]` / `input["new_string"]`, but metis's
  Edit tool actually declares its inputs as `old` / `new`
  (`internal/tools/builtin/edit.go`). Real Edit tool calls produced
  no colored diff and a `Added 0 lines, removed 0 lines` summary.
  Both functions now read `old`/`new` first, falling back to the
  longer claude-code-style names. Discovered when running the live
  end-to-end Edit verification, NOT by the unit tests (which used the
  wrong field names too).

### Added — Claude Code parity push (60 items + UI follow-ups)

#### Slash commands

- `/mcp` subcommands beyond list/add/remove/start: `enable`, `disable`,
  `edit [<name>]` (opens `~/.metis/mcp.toml` in `$EDITOR` and re-validates
  on save), `test <name>` (one-shot connect + tool list), `logs <name>`
  (tails `~/.metis/mcp-logs/<name>.log` when present), `reload`.
- `/skills` is now a full subcommand surface: `list`, `install <ref>`,
  `remove <name>`, `info <name>`, `edit <name>`, `enable <name>`,
  `disable <name>`, `create <name>` (writes a SKILL.json skeleton),
  `search <query>` (local fuzzy match — `/skill search` still hits GitHub).
  Skills can now carry a `disabled: true` field; the loader filters them
  out without deleting the manifest.
- High-frequency commands: `/copy [N]`, `/commit-push-pr <msg>`,
  `/insights [--days=N]`, `/output-style full|streamlined|minimal`,
  `/break-cache` (documents how to flush prompt cache), `/security-review`,
  `/feedback` (alias of `/bug`).
- Discoverability: `/thinkback` (surfaces the most recent assistant
  turn's thinking trace), `/ultraplan` (deep-plan nudge — bumps effort
  to high), `/onboarding` (first-run setup recap).
- User-authored commands: drop `*.md` files under `~/.metis/commands/`
  or `<cwd>/.metis/commands/` — each becomes `/<filename>` with YAML
  frontmatter for description and `$ARGUMENTS` / `$1` / `$2` substitution.
- MCP server prompts auto-discover: any server that implements
  `prompts/list` registers as `/mcp__<server>__<prompt>` slashes.

#### CLI subcommands and flags

- New top-level subcommands: `metis ps` (lists sessions, newest first,
  with pid + size + title), `metis logs <id>`, `metis attach <id>`
  (alias of `chat -r`), `metis kill <id>` (SIGTERM via pidfile).
- Short / long flag pairs: `-c, --continue` (resume newest session),
  `-r, --resume <id>` (the original `-r` "flag provided but not defined"
  bug from the 2026-05-07 video is fixed), `-d, --debug` (mirror logs
  to `~/.metis/debug.log`), `-s, --scope <local|user|project>`.
- New flags: `--bare` (skip MCP + plugin loaders), `--name <text>`,
  `--agent-teams`, `--tmux`, `--input-format json`, `--output-format
  json|stream-json`, `--dangerously-skip-permissions` (alias of
  `--mode bypass`).

#### Keybindings

- `Ctrl+G` — open the current input draft in `$VISUAL` / `$EDITOR` /
  `vi`; save and exit reads the file back into the textarea.
- `Ctrl+X` — toggle shell mode (next submission runs as bash).
- Enter mid-turn now **queues** the input (claude-code parity) instead
  of steer-injecting into the running turn. The queue auto-drains when
  the current turn finishes; `Ctrl+C` clears it. Slash commands keep
  their existing mid-turn semantics. A queued-pill (`◷ queued × N: …`)
  appears above the input box when the queue is non-empty.
- Ranges single-line `↑`/`↓` jump to start / end of input (in addition
  to history recall when the input is empty).

#### UI

- Welcome banner redesigned: ASCII robot icon in a rounded blue
  border, model + mode + cwd, no session UUID. The compact strip
  (`✻ metis · model · mode · cwd`) is now sticky above the chat list,
  so the working directory stays visible during long agent runs.
- Bottom status bar version trimmed from
  `current: v0.1.3-21-gab7a825-dirty` to `current: v0.1.3` via a
  `shortSemver` helper. Full build fingerprint stays in `metis version -V`.
- Edit / Write tool results render unified diffs with red-bg `-` and
  green-bg `+` lines (word-level highlights for paired delete + insert).
  Tested via `internal/tui/render_diff_test.go` asserting SGR codes.

#### Tests

- `scripts/e2e/tmux_drive.sh` — full-coverage e2e harness using
  `tmux send-keys` + `capture-pane`. Replaces the macOS osascript
  driver for headless / CI runs. 8 cases ship today (banner_renders,
  slash_help, input_repeat, arrow_jump, slash_skills, slash_mcp_list,
  double_esc_clear, ctrl_c_quit).

## [0.4.16] - 2026-08-09

### Changed

- The base prompt's language rule now covers internal reasoning: thinking is
  written in the same language as the reply, which mirrors the user's language
  (`internal/runtime/prompts/base/03_style.md`). Previously models reasoned in
  English even when replying in Chinese.

## [0.4.15] - 2026-08-09

### Added

- Added one runtime-owned OS sandbox policy across Bash, Git, workflow,
  skill-shell, and custom-command process launchers. macOS uses Seatbelt and
  Linux uses Bubblewrap; unavailable backends and unsafe policy gaps fail
  closed instead of silently executing on the host.
- Added the inline `/effort` selector, argument-aware `/compact`, real
  Research → Plan → Execute `/batch` behavior, and a unified command catalog
  used by the palette and help views.
- Added request-local `allowed-tools` support for trusted custom commands;
  permissions expire when that command turn finishes.

### Changed

- `/agents` now reads the live sub-agent roster, `/reload` invalidates and
  reloads the skill catalog, and list-command aliases enter the same picker as
  their canonical commands.
- Custom-command `model:` metadata now validates the selected model and gives
  an actionable `/model` instruction instead of pretending to switch models.
- Conversation exports use a readable text transcript while session JSONL
  remains the internal resume format.

### Fixed

- Kept internal `/review` prompts out of chat, history, resume, and exports;
  `/undo` and `/retry` now target the visible user turn rather than hidden
  system reminders.
- Restored provider-aware image handling and vision-model recovery, including
  configured OpenAI-compatible, Vertex, and Bedrock transports.
- Prevented mid-turn rewind from racing active file-writing tools, corrected
  bypass-permission handling, and stabilized command submission, overlays,
  history rendering, and elapsed-time updates under busy agent output.

## [0.4.14] - 2026-08-09

### Added

- OpenAI-compatible providers now surface DeepSeek-style
  `reasoning_content` (with `reasoning` fallback) as live TUI thinking,
  including non-streaming responses and restored session history.
- Added `[ui] thinking_display = "show|auto|hide"`; `/thinking show` now
  changes the active TUI immediately without sending text to the model.

### Changed

- Full thinking display renders a UTF-8-safe live tail to keep the TUI clock
  responsive during long reasoning traces while retaining the complete trace
  in conversation history.
- `/history` now includes provider reasoning, while redacted thinking remains
  protected behind a non-sensitive placeholder. `/export` continues to omit
  thinking by design.

### Fixed

- Preserved thinking and redacted-thinking rows across session resume, and
  replaced the misleading Ctrl+O expansion hint with `/thinking show`.
- Kept mixed reasoning, answer text, and tool-call deltas in provider order and
  prevented duplicate output when both compatible reasoning aliases appear.

## [0.4.13] - 2026-08-09

### Added

- Added `METIS_OPENAI_MAX_CONCURRENCY` to bound shared OpenAI-compatible
  requests across the parent agent and sub-agents (default: 4).

### Changed

- OpenAI-compatible requests now share concurrency slots, rate-limit cooldowns,
  and bounded exponential backoff for transient EOF, DNS, connection, and 429
  failures. WebFetch applies the same bounded recovery policy to transient GET
  and retryable HTTP failures.
- TUI agent events are drained in bounded batches, recovered-error rendering is
  cached, and full-screen agent views continue receiving spinner ticks, keeping
  elapsed time smooth during large multi-agent runs.
- The slash-command palette now uses the compact Claude Code-style inline
  footer layout with a six-row viewport and centered selection scrolling.

### Fixed

- `/export` executes immediately during an active or closing turn instead of
  entering the prompt queue, prevents duplicate Enter submissions, and no
  longer reports success for an empty conversation.
- Preserved queued prompts and image attachments across provider failures and
  active turns; text-only models no longer receive a bare `[Image #N]` marker
  or invent a stale desktop path for an unavailable attachment.
- Propagated truncated HTTP 200 response-body errors into the retry layer and
  prevented the agent loop from replaying a provider request after its internal
  retry budget is exhausted.
- Delayed turn finalization until all trailing agent events are applied, so the
  last response text is not lost under event backlog.

## [0.4.12] - 2026-08-08

### Added

- Added a deterministic `Skill plan_install` flow and a unified skill catalog
  covering project, plugin, `~/.agents/skills`, and `~/.metis/skills` roots, so
  agents can resolve requested skills before attempting downloads or writes.

### Changed

- Reduced TUI noise by collapsing transient spinner redraws, recovered
  intermediate failures, and no-match searches while keeping real failures,
  authentication errors, permission decisions, and security warnings visible.
- Bash output is normalized before entering model context, and `find`/`rg`
  no-match exit codes are treated as neutral without weakening fail-closed
  handling for malformed or complex shell commands.
- `bypassPermissions` now permits safe read-only access to sensitive paths but
  continues to require protection for credential reads and sensitive writes.

### Fixed

- Prevented output-truncated or name-only tool calls from persisting
  `arguments: null`; live streams, restored sessions, and outgoing
  OpenAI-compatible requests now canonicalize missing inputs to `{}`, avoiding
  cascading provider 400 errors on every later turn.
- Fixed Ctrl+O traversal of earlier collapsed TUI errors and improved restored
  error matching so unrelated or genuinely failed actions remain inspectable.

## [0.4.11] - 2026-08-08

### Changed

- `/export` now writes a Claude Code-style, readable `.txt` transcript to
  `~/.metis/exports/`, shows the command and destination in the TUI, and omits
  system prompts, hidden thinking, internal reminders, binary image data, and
  detected secrets.

### Fixed

- Restored mouse-wheel navigation of Metis chat history so iTerm2 and tmux no
  longer mix stale terminal scrollback with the active alternate-screen frame.
- Fixed Ctrl+S copy mode so the full transcript is written only after leaving
  the alternate screen, preserving earlier lines in native terminal scrollback.
- Sub-agent status and `/agents-view` now prefer each agent's short
  `description`, avoiding duplicate labels when prompts share the same prefix.

## [0.1.1] - 2026-05-01

### Added

- `metis update` subcommand — self-update against the private GitHub release
  (atomic replace, sha256-verified). Refuses to clobber a `go install`-managed
  binary.
- Daily startup notice when a newer release is available (terminal only;
  throttled to one network call per 24h; `METIS_NO_UPDATE_CHECK=1` disables).
- Cross-compile release workflow (`.github/workflows/release.yml`): tag push
  or manual dispatch builds darwin/linux × amd64/arm64 tarballs and uploads
  them with sha256 sidecars.
- `install/install.sh` rewritten for the private-release token model — set
  `METIS_GITHUB_TOKEN` (fine-grained PAT, Contents: Read-only) and
  `curl … | bash` works.

### Fixed

- `internal/tui` bridge startup race: `startBridge` now waits for `/health`
  to respond before returning, eliminating EOF flakes on slower runners.

## [0.1.0] - 2026-04-30

### Added

- Initial public release.
- Agent loop: streaming message → tool → message cycle with cancel support.
- 16 built-in tools: Bash, Read, Edit, Write, Glob, LS, Grep, WebFetch,
  WebSearch, TaskCreate / TaskList / TaskUpdate, NotebookEdit, ExitPlanMode,
  AskUserQuestion, Agent.
- Multi-provider LLM support: Anthropic, OpenAI, Gemini, custom OpenAI-compatible.
- Memory system: Core / Archival / Recall + Daily journal.
- Bubbletea TUI chat surface with permission prompts (allow / deny / ask).
- Plugin and skill registry under `pkg/`.
- Agent Client Protocol (ACP) JSON-RPC server in `acp/`.
- Compaction: automatic conversation compression with configurable triggers.
- Config: `~/.metis/config.toml` with `api_key_env` for keeping secrets out of
  the file.

[Unreleased]: https://github.com/Ricardo-M-L/metis/compare/v0.4.21...HEAD
[0.4.21]: https://github.com/Ricardo-M-L/metis/compare/v0.4.20...v0.4.21
[0.4.20]: https://github.com/Ricardo-M-L/metis/compare/v0.4.19...v0.4.20
[0.4.19]: https://github.com/Ricardo-M-L/metis/compare/v0.4.18...v0.4.19
[0.4.18]: https://github.com/Ricardo-M-L/metis/compare/v0.4.17...v0.4.18
[0.4.17]: https://github.com/Ricardo-M-L/metis/compare/v0.4.16...v0.4.17
[0.4.16]: https://github.com/Ricardo-M-L/metis/compare/v0.4.15...v0.4.16
[0.4.15]: https://github.com/Ricardo-M-L/metis/compare/v0.4.14...v0.4.15
[0.4.14]: https://github.com/Ricardo-M-L/metis/compare/v0.4.13...v0.4.14
[0.4.13]: https://github.com/Ricardo-M-L/metis/compare/v0.4.12...v0.4.13
[0.4.12]: https://github.com/Ricardo-M-L/metis/compare/v0.4.11...v0.4.12
[0.4.11]: https://github.com/Ricardo-M-L/metis/compare/v0.4.10...v0.4.11
[0.1.1]: https://github.com/Ricardo-M-L/metis/releases/tag/v0.1.1
[0.1.0]: https://github.com/Ricardo-M-L/metis/releases/tag/v0.1.0
