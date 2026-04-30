# metis — Architecture

This document describes the moving parts and the rationale behind each
choice. For the comparative study that informed these choices, see
[`../../COMPARISON.md`](../../COMPARISON.md).

> Status as of 2026-04-30. The earlier "v1 / v2" scoping in this doc is
> obsolete — most of what was deferred has shipped (MCP, hooks, plugin,
> auto-compaction, full bubbletea TUI, sub-agent, multi-provider).

## Top-level data flow

```
                    ┌─────────────────┐
                    │   cmd/metis     │  flag parsing →
                    │   main.go       │  internal/runtime composers →
                    └────────┬────────┘  cmdChat / cmdRun / cmdConfig
                             │
                             ▼
                ┌─────────────────────────┐
                │   internal/runtime/*    │  per-feature wiring helpers
                │   (provider, mcp,       │  (composer-only — no business
                │    plugin, channels,    │   logic; main.go delegates)
                │    system_prompt, …)    │
                └────────────┬────────────┘
                             │
                             ▼
                ┌─────────────────────────┐         ┌──────────────────┐
                │   internal/agent.Loop   │ ◀──────▶│ internal/permission
                │   Run(ctx, eventChan)   │ asks    │  Gate (5 modes,  │
                │                         │         │  cascading rules)│
                │  ┌───────────────────┐  │         └──────────────────┘
                │  │ streaming.go      │  │
                │  │ dispatch.go       │  │   ┌──────────────────────┐
                │  │ compaction_check  │  │ ◀─│ internal/llm Provider │
                │  │ permission_ask    │  │   │  Anthropic / OpenAI / │
                │  │ plan_emit / hooks │  │   │  Gemini (native each) │
                │  └───────────────────┘  │   └──────────────────────┘
                └────────────┬────────────┘
                             │
                             ▼
                ┌─────────────────────────┐
                │   internal/tools        │   Registry + Tool interface
                │   ┌──────────────────┐  │   • 16 builtin tools
                │   │  builtin/        │  │   • MCP-loaded tools (auto-
                │   └──────────────────┘  │     namespaced as mcp__name__tool)
                │   • Concurrency: Safe   │   • Plugin-contributed tools
                │     / Queue / Exclusive │
                │   • Bash classifier     │
                │     (input-dependent)   │
                └────────────┬────────────┘
                             │ events
                             ▼
                ┌─────────────────────────┐
                │   internal/tui          │   bubbletea Model + Update
                │   (split across ~50     │   + View. Split into render_*,
                │    files; one Model)    │   keybind_*, screen overlays.
                └─────────────────────────┘
```

## Module boundaries

### `pkg/` — Public SDK

Third-party plugin authors compile against `pkg/` only; nothing here
imports `internal/`. Sub-packages: `provider`, `tool`, `hook`, `channel`,
`skill`, `memory`, `session`, `plugin`, `llm` (Effort).

`internal/` packages re-export `pkg/` types via type aliases so existing
internal callers don't break when the surface moves to `pkg/`.

### `internal/llm` — Provider abstraction

`Provider` is the surface every backend implements:

```go
type Provider interface {
    Name() string
    Complete(ctx, Request) (*Response, error)
    Stream(ctx, Request) (StreamReader, error)
    MaxContextTokens() int   // for auto-compaction threshold
}
```

`StreamReader.Recv()` returns one `StreamEvent` per call (`text_delta`,
`thinking_delta`, `tool_use_start`, `tool_input_delta`, `tool_use_stop`,
`message_delta`, `message_stop`, `error`) — a normalized vocabulary the
agent loop consumes regardless of provider.

Three native adapters, all hand-rolled HTTP + SSE (no upstream SDKs, to
avoid version skew + reduce binary size):

- **Anthropic** (`anthropic.go`) — `/v1/messages` with extended-thinking
  support. Forwards `input_tokens` from both `message_start` and
  `message_delta` so Anthropic-compatible gateways (MiniMax, OpenRouter)
  that report usage at message_delta still get counted.
- **OpenAI** (`openai.go`) — `/v1/chat/completions` with
  `stream_options.include_usage=true`. Tool calls accumulate by index
  across chunks (Together / Groq / Ollama compatible).
- **Gemini** (`gemini.go`) — `/v1beta/models/<model>:streamGenerateContent`
  native protocol with `x-goog-api-key` header. Forwards both
  `promptTokenCount` and `candidatesTokenCount`.

Each provider exposes a `ContextWindow` field that overrides the model-
prefix lookup table — required for third-party gateways whose served
model name doesn't match a known prefix.

### `internal/tools` — Tool interface and registry

```go
type Tool interface {
    Name() string
    Description() string
    InputSchema() map[string]any
    Concurrency(input map[string]any) Concurrency  // Safe / Queue / Exclusive
    CanUse(ctx, input) (Permission, source)
    Execute(ctx, input) (*Result, error)
}
```

Three concurrency tiers (Claude Code-inspired):

- **Safe** — fans out in parallel goroutines (Read, LS, Glob, Grep,
  WebFetch, …)
- **Queue** — runs concurrent with safe but FIFO among queue tools
  (Search, Git read-only, Agent sub-spawn)
- **Exclusive** — serializes all execution while it runs (Edit, Write,
  Bash mutating, TodoWrite)

`Concurrency()` takes the input map so a tool can classify per-call —
**Bash** does this: `ls -la` is Safe, `git status` is Safe, `git push`
or `rm -rf` is Exclusive (`bash_policy.go` + `classifier.go`).

Built-in tools register through `builtin.Register(reg, cfg, gate)` —
explicit DI, no `init()` self-registration. MCP-loaded tools register
through `mcp.Client.RegisterTools(reg)`. Plugin tools register through
`plugin.Loader.RegisterTools(reg)`.

### `internal/permission` — Permission gate

Five modes (`ask` / `auto` / `bypass` / `plan` / `deny`), cascading
`Rule` stack. Sources appended at startup (default → user config →
project config → CLI flags); rules evaluated in **reverse order** —
last appended wins.

`auto` auto-allows read-only Safe tools and asks otherwise; `plan`
denies anything that mutates state and forces `Loop.PlanMode=true` so
the loop emits `EventPlan` instead of executing.

The interactive TUI promotes "always" responses to in-memory rules so
the agent doesn't re-ask the same question in the session.

### `internal/agent` — The orchestrator

`Loop.Run(ctx, eventChan)` is split across 9 sibling files for focus:

| File | Responsibility |
|------|----------------|
| `loop.go` | Lifecycle (NewLoop / AppendUser / History / Reset / Undo) + main `for` driver |
| `streaming.go` | `consumeStream` — drains `StreamReader`, assembles assistant blocks |
| `dispatch.go` | `executeBatch` — three-phase Safe-parallel + Queue-FIFO + Exclusive-serial |
| `permission_ask.go` | mid-turn permission prompt with reply channel |
| `compaction_check.go` | `maybeCompact` (preflight) + `tryRecoverOverflow` (retry on 4xx) |
| `plan_emit.go` | PlanMode short-circuit |
| `hooks.go` | PreToolUse / PostToolUse / Session* / Turn* / LoopEnd / Error |
| `loopdetection.go` | repeated-call patterns → abort |
| `loop_skill.go` | `/loop` driver (continuous self-paced execution) |

Per-iteration:

1. `maybeCompact` — when `estimateTokens(messages) >= ctxWindow * 0.85`,
   summarize old turns into a synthetic boundary message
2. `buildRequest` (under `l.mu`) — assemble system + memory context +
   messages + tool specs + effort/fast knobs
3. `Provider.Stream(ctx, req)` → if 4xx with "context window exceeds
   limit" / "too many tokens" / etc., `tryRecoverOverflow` force-
   compacts and retries once (the surface MiniMax + OpenAI / Gemini
   wraps differ; we string-match across families)
4. `consumeStream` → assistant blocks + stop reason + usage
5. If `stop != "tool_use"` → emit `LoopDone`, return
6. Filter tool_uses; PlanMode → `emitPlan` and exit
7. `executeBatch`:
   - Group inputs by `Concurrency(input)` classification
   - Run Safe + Queue concurrently (Safe parallel, Queue FIFO)
   - Wait, then run Exclusives serially in source order
   - Append results in **source order** (Anthropic requires this)
8. Append assistant + tool_results, increment turn counter
9. Loop-detection check; budget check (with grace call)

### `internal/agent/skills` — SKILL.md system

Five-layer loader (Claude-Code-inspired) — priority high wins:

```
mcp(40)  >  plugin(30)  >  project(20)  >  user(10)  >  bundled(0)
```

Each layer's `Scan() ([]Skill, error)` returns parsed skills.
Skills are `SKILL.md` files with YAML frontmatter:

```markdown
---
name: code-review
description: Review staged diff for bugs, style, security
when_to_use: User says "review this PR" or after `git add`
allowed_tools: [Read, Bash, Grep]
model_override: claude-opus-4-7
version: 1.0.0
---
You are a senior code reviewer. Walk the staged diff:
1. `git diff --cached` to see changes
…
```

22 bundled skills embedded via `//go:embed builtin/*.md`.

### `internal/mcp` — MCP client

Two transports:

- **stdio** (`client.go`) — spawns the server as a subprocess; line-
  delimited JSON-RPC framing
- **Streamable HTTP/SSE** (`http.go`) — POST a request, server replies
  with either an immediate JSON or an SSE stream; one SSE stream is
  long-lived for server-initiated notifications

Tools loaded from MCP servers register as `mcp__<server>__<tool>` so
collisions with built-ins are impossible. Auth headers (`x-goog-api-key`,
`Authorization`, …) forwarded through.

### `internal/channels` — Chat-platform adapters

11 adapters: DingTalk, Discord, Feishu, iMessage, Mattermost, Signal,
Slack, Telegram, WeChat, WhatsApp, plus the `pkg/channel` interface for
custom ones. Each implements `Send(ctx, msg) error` + `Listen(ctx)
<-chan Message`. The `SendMessage` builtin tool routes by channel name.

Use case: a `/cron` job runs daily and pushes a markdown report to a
Telegram chat or Feishu group.

### `acp/` — Agent Client Protocol server

Exposes the same `agent.Loop` over JSON-RPC 2.0 (stdio or TCP) so
external tools — Zed editor, custom scripts, automated test harnesses
— can drive metis without spawning a TUI. Borrowed from Hermes'
`acp_adapter`.

Wire shape (one connection = one ACP session):

```
client ──► server          server ──► client
─────────                  ─────────
prompt(text)               session_update(kind=text_delta, text_delta=...)
                           session_update(kind=tool_start, tool_name=...)
                           session_update(kind=tool_result, ...)
                           session_update(kind=permission_request, ...)
                           ...
                           response(id=N, result={done: true})

permission_reply(          response(id=N, result={ok: true})
  tool_use_id,
  decision)

abort(prompt_id)           response(id=N, result={aborted: true})
```

Translation table `acp/server.go::eventKind()` maps every internal
`agent.EventKind` (text/tool/thinking/sub-agent/stream/context/rate-
limit/channel/hook — 22 total) to a stable wire string so JSON-schema-
oriented clients can switch on `kind` without falling into "unknown".

Distinct from a daemon (openclaw style): the ACP server's lifecycle is
bound to the client's. Stdio mode runs as the client's child process;
TCP mode listens on `127.0.0.1:<port>` until the client disconnects.
Single-process metis stays single-process — ACP is just an embedding
surface, not a sharing layer.

### `internal/memory` — Three-tier memory

```
Core (Block-based, ~10K char budget)
   ├─ user / system / working / summary blocks
   ├─ Frozen-snapshot (Hermes pattern): captured at Load(),
   │  immune to mid-session UpdateBlock writes
   └─ Context-fenced when rendered: <memory-context>...</memory-context>
      with a system note so the model doesn't read it as user input

Archival (passages.jsonl)
   └─ secondary store with security scan (injection/credential/backdoor)

Recall (conversation history compaction)
   └─ 50-message threshold trigger, keeps last 2 + summary
```

Plus daily notes (`memories/daily/YYYY-MM-DD-<slug>.md`) on `/new` and
freshness tracking (mtime → stale warning >24h).

### `internal/session` — Persistence

JSONL per session: one `header` line, then one line per message. Append-
only writes. Branch creates a new session UUID with a copy of history.
Snapshot persists a named point-in-time.

### `internal/slash` — Slash command router

Registry of `name → handler` with alias support. Handler returns
`(displayText, signal)` where signal is one of `SignalQuit`,
`SignalClear`, `SignalNew`, `SignalUndo`, `SignalHistory`, `SignalTitle`,
`SignalBranch`, `SignalSave`, `SignalTools`, `SignalSessions`,
`SignalSession`, `SignalSkills`, `SignalVersion` — the TUI Update reacts
to each.

### `internal/tui` — bubbletea TUI

Single `Model` struct, but rendering and key handling are split across
~50 files for focus:

```
tui.go / tui_styles.go / themes.go            Model + style palette
tui_update.go / tui_events.go                 Update loop + agent.Event drain
tui_render.go / render_chrome.go              View + bottom chrome (input,
                                              spinner, status bar, hints)
render_message.go / render_tool.go            Per-row painters
render_overlay.go                             Palette / permission /
                                              task panel / scrollbar
render_welcome.go / figures.go                Welcome banner + glyphs
keybind_*.go                                  Per-mode key handlers
                                              (main, palette, permission,
                                              session, submit)
auth_wizard.go / clipboard.go                 Sub-flows
spinner / token / perf_config / tick          UI tunables
error_format.go                               Provider error → readable
                                              one-liner + recovery hint
bridge.go                                     Read-only state snapshot
                                              for external observers
                                              (ACP-like)
screen/                                       Full-window overlays
                                              (history, file picker, …)
```

Spinner glyph advance is **time-gated** (120ms steps), not tick-gated —
otherwise a 40ms tick made the asterisk flicker (claude-code parity).

## Streaming + concurrency model

Two goroutines per turn:

- **Background** — `Loop.Run` reads from `Provider.Stream`, drives tool
  execution, pushes events to `chan agent.Event` (buffered, default 256
  configurable via `ui.performance.event_buffer_size`).
- **Foreground** — bubbletea's Update drains the event channel on every
  spinner tick (40ms / 25fps default). Tool execution itself uses
  goroutines + `sync.WaitGroup` inside `executeBatch`.

Cancellation flows from the parent context — Ctrl+C in the TUI:

- Idle: single tap → quit
- Mid-turn: first tap arms a 2s quit-timer, second tap kills the turn
  (graceful: cancels the request, lets in-flight tool results land)

`finalizeTurn` flushes any remaining streaming text + thinking trace,
appends the thought-summary + recap rows, writes a learning record to
`~/.metis/iteration-log.md`, and persists the session tail.

## Auto-compaction

Triggered preflight (size threshold) **and** reactively (4xx with
context-window phrasing). `estimateTokens` counts text body + tool input
JSON + tool result content + tool name (every byte that goes on the
wire), so tool-heavy turns are no longer invisible to the threshold
check. The summary itself is generated via a streaming LLM call (same
provider) so the user sees progress, not a frozen UI.

The cut point honors **tool-pair safety**: the kept tail is walked back
until it doesn't start with an orphan `tool_result` whose `tool_use`
lives in the discarded middle (Anthropic rejects orphaned pairs with
422).

## Scheduling & continuous execution

Three layers, each borrowed from a different reference project:

### 1. `metis cron` — file-backed scheduler (OpenClaw-flavored)

`internal/agent/cron.go` persists each job as
`~/.metis/sessions/cron/<id>.json`. The scheduler goroutine
(`schedulerLoop`) sleeps until the earliest `NextRun` and fires `onFire`.

`CronSchedule.Kind`:

- `every` — fixed interval; `EveryMs`
- `at` — single RFC3339 timestamp; advances 24h after firing
- `cron` — robfig/cron/v3 expression: 5/6 fields + descriptors
  (`@daily`, `@every 1h30m`); evaluated in `Schedule.TZ` or `time.Local`

`Schedule.JitterMs` adds uniform `[-jitter, +jitter]` noise via
`crypto/rand` so multiple `:00`-aligned jobs don't fire simultaneously
(hard-coded "thundering-herd against rate-limited APIs" defense).

Validation runs at `Create()` time — bad expressions are rejected
synchronously rather than silently mis-firing forever (the previous
`hand-rolled fallback`-then-`now.Add(1 * time.Hour)` was a quiet bug).

### 2. `CronJob.SessionMode` — three history strategies (OpenClaw)

| Mode | History |
|------|---------|
| `isolated` (default) | every fire starts empty; `Loop.Reset()` + `AppendUser(Prompt)` |
| `persistent` | per-job rolling thread; `cmdCronStart` keeps `map[jobID][]llm.Message` |
| `main` | named shared session; `map[SessionRef][]llm.Message`, default `"main"` |

Histories are process-lifetime — restarting `metis cron start` resets
them. Disk persistence would require wiring through `session.Store`;
left as TODO since cron daemons typically run continuously.

`Loop.Restore(messages)` is the new method that swaps history without
re-allocating the loop. Iteration counters reset because the next call
is a new turn, not a resumption.

### 3. `ScheduleWakeup` tool — LLM self-pacing (Claude Code)

Borrowed from claude-code's `bundled/loop.ts`. Lets the LLM say
"wake me in N seconds with this prompt" instead of polling at a fixed
interval. Rationale:

- **Cache alignment** — Anthropic prompt cache TTL is 5 minutes, so
  delays under 270s keep the cache warm; jumping to 1200s+ amortizes
  one cache miss across a long quiet period. The tool's schema text
  gives these breakpoints to the LLM.
- **No blind polling** — mid-build the agent can wake every 60s; idle
  it can sleep 30 min. A static cron can't.

Implementation reuses cron: a wakeup is a one-shot job with `Kind=at`
+ `Repeat=1`. `internal/tools/builtin/wakeup.go` constructs the
`CronJob`, the existing scheduler fires it, `runJob` flips
`Enabled=false` after `RunCount >= Repeat` so wakeups self-clean.

Range clamped to `[30s, 24h]` — sub-30s feels like a hot-loop typo,
24h+ should be a real `/cron` the user can audit.

### 4. `CronJob.DisabledTools` — per-job tool blacklist (Hermes)

Borrowed from Hermes's `enabled_toolsets`. Each name in the list gets a
temporary `permission.DecisionDeny` rule appended to the gate before
the fire and popped via `Gate.PopRules(N)` after, regardless of error.
Right for cron'd jobs where you want to prevent the agent from
accidentally invoking expensive tools (`Agent` sub-spawn, `WebFetch`
loops) without disabling them globally.

CLI:

```sh
metis cron add --cron "0 9 * * 1-5" --tz "America/Los_Angeles" \
  --jitter 30s --mode persistent \
  --disable-tools "WebFetch,Agent" \
  --prompt "post weekday metrics summary"
```

## Permission flow

```
agent.Loop sees tool_use block
     │
     ▼
tool.CanUse(ctx, input)              ← consults permission.Gate
     │                                  + bash classifier for Bash
     ├── PermissionAllow  ───────▶   Execute
     ├── PermissionDeny   ───────▶   Synthesize tool_result with error
     └── PermissionAsk    ───────▶   emit EventPermissionRequest
                                          │
                                          ▼
                              TUI renders dialog (rounded border,
                              ↑↓ navigate, y/n/a shortcuts)
                                          │
                                          ▼
                              if AlwaysAllow: append rule to Gate
                              if Allow:      execute
                              if Deny:       synthesize error
```

## Plugin model

`~/.metis/plugins/<name>/plugin.toml`:

```toml
manifest_version = 1
name = "browser-mcp"
version = "0.3.1"

[mcp_server]
command = "node"
args = ["index.js"]

skills = ["skills/screenshot.md"]

[[hooks]]
events = ["PreToolUse"]
command = "./guard"
match = "browser_*"
```

Loader (`internal/runtime/plugin.go`):

1. Scan `~/.metis/plugins/*/plugin.toml`
2. Validate schema; reject if `name` doesn't match dir
3. `[mcp_server]` → reuse `internal/runtime/mcp.go`'s LaunchAllMCP path,
   tools register as `plugin__<name>__<tool>`
4. `skills` array → register at the plugin layer of the skill loader,
   namespace `<plugin>:<skill>`
5. `hooks` → spawn a long-lived stdio child, register a thin hook
   handler that pipes JSON over stdin/stdout

`metis plugin {install,list,info,remove,reload}` for management.

## Distribution

Two install paths share the same tarballs from `make dist`:

- `install/install.sh` — detect OS+arch, fetch
  `metis-{os}-{arch}.tar.gz` + `.sha256` from
  `$METIS_RELEASE_BASE/$METIS_VERSION/`, verify, untar to
  `$METIS_INSTALL_DIR` (default `~/.local/bin`).
- `install/npm/` — `bin/metis` Node shim that spawns the binary
  downloaded by `scripts/postinstall.js`.

`METIS_RELEASE_BASE` defaults to `file://$HOME/.metis-releases` — no
hardcoded public URL anywhere.

## Trade-offs deliberately accepted

- **No daemon, no Docker, no K8s** — single static binary, single
  process. If you need scheduled work, `metis cron` is just a goroutine
  with a parser.
- **No upstream SDKs** — Anthropic / OpenAI / Gemini SDKs change shape
  often and bloat the binary. Hand-rolled SSE parsers are ~150 LoC each
  and pin protocol exactly.
- **TUI is a god-Model with file split** — bubbletea encourages one
  Model. We keep it (no nested Models), just split the rendering and
  key-handling into ~50 focused files.
- **Local-first** — no telemetry, no remote release, no auto-update.
  `metis plugin install` accepts git URLs and local dirs only.
