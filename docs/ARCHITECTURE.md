# Metis Architecture

> Documentation baseline: v0.4.18 target, checked against the source tree on
> 2026-08-11. This document names stable boundaries and runtime behavior rather
> than file or tool counts: several registries are configuration-dependent and
> exact counts become stale quickly.

## Top-level data flow

```text
 CLI chat/run ─┐
 TUI           │
 ACP stdio/TCP ├──> cmd/metis + internal/runtime ───> internal/agent.Loop
 local Web UI  │             composition/services          │
 daemon/cron ──┘                                           ├── Provider
                                                          ├── Permission Gate
 native Wails Desktop ── spawns `metis run` per turn       ├── Tool Registry
                                                          └── Session/Memory/
                                                              Compaction

 Tool Registry = built-ins + runtime-only tools + MCP tools + plugin MCP tools
 Agent events  = TUI / headless output / ACP translation / hooks
```

The default interactive command is a local process. That is a default, not a
global single-process invariant: MCP servers may be child processes, and
`daemon`, `cron start`, coordinator workers, ACP TCP, the Web UI, and the native
Desktop are explicit long-running or separate-process surfaces.

## Package boundaries

### `pkg/` — public contracts

External callers use `pkg/provider`, `pkg/tool`, `pkg/hook`, `pkg/channel`,
`pkg/skill`, `pkg/memory`, `pkg/session`, `pkg/plugin`, `pkg/client`, and
`pkg/llm`. Public packages do not import `internal/`. Internal compatibility
aliases keep older in-repository call sites on the same types where needed.

### `internal/runtime/` — composition and runtime services

`internal/runtime` builds providers, permission gates, tools, channels, MCP,
plugins, system prompts, memory, sandboxing and agent loops for each command.
It also contains real services such as MCP lifecycle support, the file-queue
daemon and coordinator helpers; it is therefore more than a “composer-only”
directory. `cmd/metis` owns command parsing and process lifecycle.

## Providers

The stable provider contract lives in `pkg/provider` (and is aliased by
`internal/llm`):

```go
type Provider interface {
    Name() string
    Complete(context.Context, Request) (*Response, error)
    Stream(context.Context, Request) (StreamReader, error)
    MaxContextTokens() int
    ModelID() string
}
```

`StreamReader` normalizes text, thinking, tool calls, usage, stop and error
signals so the agent loop does not depend on a vendor's SSE shape. `ModelID`
is the model identifier actually sent on the wire; `MaxContextTokens` drives
context-pressure decisions.

Provider configuration has two levels:

- Primary profile shortcuts: `anthropic`, `openai`, and `gemini` (`google` is
  accepted as a Gemini alias).
- Arbitrarily named custom profiles routed through registered wire transports:
  `anthropic_messages`, `openai_chat`, `gemini_native`, `azure_openai`,
  `bedrock_anthropic`, and `vertex_anthropic` (with documented short aliases
  for the cloud transports).

The first three transports also support compatible gateways through a custom
base URL. Azure, Bedrock and Vertex are not additional primary profile names;
they are transport choices for custom profiles with transport-specific auth.
Provider implementations live under `internal/llm/<transport-family>/`, while
`internal/runtime/provider.go` resolves profiles and constructs them.

## Agent loop, tools and permissions

### Loop lifecycle

`internal/agent.Loop` owns one active transcript. A normal iteration:

1. preflights full request pressure and, when required, runs the unified
   checkpoint-compaction pipeline;
2. assembles system sections, memory context, transcript and current tool specs;
3. streams a normalized provider response and persists assistant blocks;
4. runs pre-tool hooks and permission checks for the whole tool batch;
5. dispatches approved calls by their per-input concurrency class;
6. appends every `tool_result` in the model's original source order; and
7. continues until the provider stops without tool use, a budget/safety gate
   ends the run, or the caller cancels it.

Known provider “context too large” responses take a force-compaction recovery
path and retry once. Loop detection, iteration budgets, todo reminders,
checkpoints, hooks and auto-memory are independent guards around this core.
Exit and restore paths repair interrupted/orphaned `tool_use` blocks so
persisted history remains API-valid.
The implementation is split by responsibility; no documentation should depend
on the number of sibling files.

### Tool contract and four concurrency classes

The public `pkg/tool.Tool` contract includes `IsEnabled()` in addition to name,
description, schema, input-aware `Concurrency`, `CanUse`, and `Execute`.
Unavailable tools are filtered before their schemas reach the model.

Concurrency is chosen for each call, not just each tool type:

- `Safe`: run concurrently as a fan-out.
- `Queue`: run FIFO on one worker, concurrently with the Safe fan-out.
- `Background`: perform the tool's fast spawn/handshake path and return a job
  identifier while work continues through the job registry.
- `Exclusive`: run serially after Safe and Queue complete.

For example, Bash can classify a read-only command differently from a mutating
one, and Agent uses Background when requested. The dispatcher preserves result
ordering across all four classes even when execution order differs.

The registry is intentionally dynamic. `internal/tools/builtin` installs the
base tools; `internal/runtime/tools.go` adds tools that need live provider,
session, jobs, roster, cron, memory or plan-mode references; command setup adds
late-bound slash/MCP-resource tools; MCP and plugins can add more. Configuration
and `IsEnabled()` can remove tools. Do not document a single fixed tool total.

### Permission resolution and plan mode

The modes are `default`, `acceptEdits`, `plan`, `dontAsk`, and
`bypassPermissions`. Matching rules resolve by source authority first:

```text
managed policy > CLI > interactive/session > config > persistent approval
```

Only equal-authority matches use most-recently-appended as the tie-breaker.
Rule content supports guarded command prefixes, path globs and legacy
substrings. Dangerous-pattern, secret-read and scope checks remain separate
safety boundaries; “bypass” is not a general way to override managed policy.

Plan mode allows read-only exploration and permitted delegation, returns normal
denial results for state-changing tools, and uses `ExitPlanMode` as the approval
boundary before implementation. `EnterPlanMode` and `ExitPlanMode` are regular
runtime-registered tools; plan mode is not a blanket short-circuit after every
assistant response.

## Extension surfaces

### Skills

The production skill loader merges these layers, with the later/higher-priority
source winning a name collision:

```text
bundled(0) < optional(5) < universal ~/.agents/skills(8)
           < user ~/.metis/skills(10) < project .metis/skills(20)
           < plugin(30)
```

An MCP skill priority is reserved in comments/types but is not wired as a
production loader layer. Bundled skills are embedded; their exact count is not
an architectural contract.

### MCP

`internal/mcp/client.go` implements both stdio JSON-RPC and Streamable HTTP/SSE,
including `initialize` followed by `notifications/initialized`. There is no
separate `internal/mcp/http.go`. Runtime configuration, lazy launch/cache,
OAuth, tool allow/deny filters, prompts and resources live under
`internal/runtime/mcp`; result/image conversion and registry wrappers live in
`internal/tools/mcp`.

MCP tools are exposed as `mcp__<server>__<tool>`. Server launch can be eager or
cache-backed/lazy (`METIS_LAZY_MCP`), while tool-schema deferral is a separate
prompt-size feature (`ENABLE_TOOL_SEARCH`). IDE discovery reuses the HTTP MCP
client and exposes the selected editor bridge under the `mcp__ide__*` namespace.

### Plugins

A plugin manifest can currently contribute an MCP server and skills. Because a
plugin MCP server is registered internally as `plugin:<name>`, its actual tool
name is `mcp__plugin:<name>__<tool>`; `plugin__<name>__<tool>` is not the runtime
namespace.

The public manifest schema also parses `[[hooks]]`, but `LoadPlugins` does not
instantiate or register those hook subprocesses. Plugin MCP and skill loading
are live; plugin hook execution remains declared-but-unwired and must not be
documented as shipped behavior.

## Multi-agent and automation

### Sub-agents, teammates and “agent teams”

The live multi-agent substrate is the process-wide `Roster` plus explicit tools:
`Agent`, `Fork`, `SubAgentList`, `SubAgentOutput`, `SubAgentStop`, and
`MessageTeammate`. Named teammates are addressable and receive bounded
mailboxes. Anonymous research sub-agents deliberately have no peer mailbox.
Named and anonymous work use separate configurable capacity pools, with config
and environment overrides.

`/agents`, `/teammate` and the multi-agent TUI screen expose roster state.
`/batch <task>` rewrites the request into a bounded research → plan → parallel
worktree-Agent workflow; it is a prompt recipe over the existing Agent tools,
not a second scheduler.

The CLI parser currently accepts `--agent-teams`, but no execution path reads
the parsed value. In v0.4.18 it is a reserved/no-op flag, not an alias that
automatically opens `/batch` or enables another runtime mode.

Two coordinator surfaces are distinct:

- `--coordinator` / `METIS_COORDINATOR_MODE=1` adds a team-lead system overlay
  and filters the current loop's tool palette to orchestration/read tools.
- `metis coordinator dispatch|worker` is a separate filesystem-mailbox MVP.
  The dispatch command currently sends one task and waits; workers poll and
  execute tasks. It is not a full multi-phase fleet scheduler.

### Cron and daemon processes

Interactive chat can host ephemeral cron jobs in-process. Durable jobs are
stored for `metis cron start`, which is a separate long-running process with
pre-authorized permissions.

`metis daemon` is also implemented. It polls text files from the Metis inbox,
runs them through one reused agent Loop, writes replies to the outbox, and may
run memory distillation after an idle interval. PID/status files make its state
observable. It is an optional file-queue worker, not a mandatory central daemon
or a transport shared by normal CLI/Desktop sessions.

## User interfaces and protocols

### Terminal UI

`internal/tui` uses one Bubble Tea v2 `Model`: input events and agent events
converge through `Update`, and `View` is pure rendering. Rendering, keymaps,
overlays and screens are organized into focused file families (`render_*`,
`keybind_*`, `screen/*`, etc.); their file count is not part of the design.

The main Model owns the active session UI state and serializes mutations on the
Bubble Tea update goroutine. Background agent/job/cron events arrive as messages
and are folded into that state. The tmux/PTY first-frame mitigation is documented
in [`bubbletea-blank-first-frame.md`](bubbletea-blank-first-frame.md).

### ACP

`acp/` exposes an `agent.Loop` through JSON-RPC 2.0 over stdio or TCP. Stdio
serving ends with its input stream. TCP accepts multiple connections until the
server context is cancelled or the process receives Ctrl-C; it does not stop
when one client disconnects. Connection-local prompt cancellation and pending
permission replies are separate, but all connections on one `Server` point at
the same Loop, so ACP is not a multi-tenant session-sharing layer.

The ACP `eventKind` translation intentionally has a compatibility boundary.
Mapped lifecycle/text/tool/context/rate/channel/hook events receive stable wire
strings. Events added later without a case fall back to `"unknown"`. At this
baseline the unmapped set is:

```text
EventCompactionStart, EventCompactionProgress, EventRedactedThinking,
EventToolArgsDelta, EventDreamingStart, EventDreamingProgress,
EventDreamingEnd, EventAskUser
```

Clients must tolerate `unknown`, and adding an internal `EventKind` requires an
explicit ACP mapping before it can be advertised as a stable protocol event.

### Web and native Desktop

`metis desktop` launches a separately installed native Wails application by
default. The `metis-desktop/` directory is its own Go/Wails module. The native
application does not embed or connect to the file-queue daemon: each chat turn
invokes `metis run`, using `--session-id` or `--resume` for thread
continuity. Session/cron/config operations similarly go through the CLI.

`metis desktop --web` starts the legacy browser UI on loopback. That process
owns one live Loop and serializes turns because the transcript is not safe for
simultaneous conversations.

Stable tag releases publish checksummed Wails Desktop archives alongside the
CLI assets: a universal macOS DMG installer plus the ZIP retained for atomic
in-app updates, one Linux amd64 binary and one
Windows amd64 executable. The native launcher and updater resolve those exact
release artifacts; they are application archives rather than OS-native package
manager formats.

### Chat channels

Implementation packages exist for DingTalk, Discord, Feishu, iMessage,
Mattermost, Signal, Slack, Telegram, WeChat and WhatsApp. Production runtime
wiring currently constructs Slack, Telegram, Discord, DingTalk, Feishu and
WeChat when their configured credential environment variables are present.
iMessage, Mattermost, Signal and WhatsApp are implementation packages but are
not yet connected by `BuildChannelRegistry`.

## State, memory and context control

### Sessions and memory

Sessions are append-oriented JSONL transcripts with a header plus message
records. Branching copies history into a new session; snapshots/checkpoints are
separate recovery surfaces.

Persistent memory has core blocks, archival passages, daily notes and a recall
store. Core memory is rendered into a fenced system context from a session
snapshot so mid-turn writes do not mutate the already-built request. The recall
type contains a conversation-summary helper, but production code does not call
that helper automatically. Do not describe it as the active “50-message
compactor”; token-driven agent compaction below is the live conversation-window
mechanism.

### Compaction pipeline

Automatic pressure, manual `/compact`, provider-overflow recovery and the
iteration-budget second wind all enter `Loop.CompactNow`. This is one heavy
checkpoint pipeline: it owns trigger policy, hooks and lifecycle events,
candidate construction, durable persistence and final in-memory installation.
There is no separate automatic pre-pass that can reduce the estimate and then
cancel the checkpoint that was already required.

The production default triggers at 85% of the effective input cap. The cap is
the model context window minus the response-token reservation, rather than the
nominal context window. The configured absolute minimum defaults to 50,000
tokens, but a minimum above the model-specific percentage boundary is clamped
to that boundary so it cannot disable compaction on a smaller window. The
decision uses full request pressure, not transcript size alone: current system
sections, memory/retrieval context, plan and volatile runtime state, and current
tool schemas are included in the preflight estimate.

After the boundary is crossed, the same transaction prunes older images and
microcompacts oversized recoverable tool results, then produces one final
summary checkpoint. The old prefix is summarized while a token-budgeted recent
tail is retained verbatim; on normal model windows that tail targets 8% of the
effective input cap, clamped to 16K–32K tokens. Tool-use/result pairs remain
balanced. The two latest real user requests are preserved exactly, either in
the tail or in the deterministic checkpoint anchor, so intervening tool output
cannot displace the active request.

The summary prompt has eight stable sections: Primary Request & Intent,
Current State, Files & Changes, Technical Context, Errors & Fixes, Pending
Tasks, Strategy & Approach, and Exact Next Steps. `Memory` and `Skill` results
are protected from preparatory rewriting; ordinary file reads can be recovered
from disk when needed. A circuit breaker stops repeated failed summaries, and
post-compaction guards reject a checkpoint that makes no token progress.

Session persistence uses `Store.CheckpointCompaction`: it first appends any raw
pre-compaction tail that has not reached the JSONL ledger, then appends a
`history_replace` record for the compacted history. A crash before the replace
therefore resumes the complete raw history, while a crash after it resumes the
exact checkpoint. Persistence failure rolls the live Loop back instead of
leaving memory and disk on different histories.

## Local-first behavior, telemetry and network boundaries

No telemetry is exported by default. `internal/telemetry` enables OTLP only when
an OTLP endpoint is configured. “Local-first” also does not mean “never uses the
network”: selected providers, MCP HTTP servers, channel adapters, Web tools,
plugin installation/MCP, remote skills and release checks do so explicitly.

Optional long-running processes do not form a mandatory control plane. Normal
CLI sessions keep their Loop and transcript state in-process and persist state
under the configured Metis home.

## Distribution and update behavior

`make dist` and the GitHub release workflow build checksummed CLI assets for
macOS, Linux and Windows on amd64 and arm64. Unix assets are `.tar.gz`; Windows
assets are `.zip` archives containing `metis.exe`. A stable tag push also builds
and publishes checksummed Desktop archives for macOS universal, Linux amd64 and
Windows amd64.

- `install/install.sh` is the macOS/Linux bootstrap. It resolves a public
  GitHub release, downloads the matching archive and `.sha256`, verifies it,
  stages an immutable version under `~/.local/share/metis/versions/<version>`
  by default, and atomically switches `~/.local/bin/metis` to that binary.
  `METIS_INSTALL_DIR` changes the launcher directory; the managed data root is
  the sibling `share/metis` directory below the same prefix.
- `install/install.ps1` is the Windows bootstrap. It verifies and stages the
  matching Windows zip under `<install-root>\versions\<version>`, keeps
  `<install-root>\bin\metis.exe` as the stable launcher, and records the
  selected version in `<install-root>\current-version`. If the launcher is
  already running, Windows permits it to be renamed before the verified new
  launcher is copied into place; the old copy is deleted immediately when
  possible or by a later cleanup after the process exits.
- Public installation and update work anonymously. `METIS_GITHUB_TOKEN` or
  `GITHUB_TOKEN` is optional and is used only when a higher GitHub API rate
  limit or an authenticated repository override is needed.
- The bootstrap installers and `metis update` share the versioned lifecycle:
  checksum and binary-version verification, staging before activation, a
  cross-process install lock, atomic/best-effort rollback-safe launcher
  switching, and post-switch cleanup. A recognized legacy flat Metis binary is
  migrated into the managed store as a rollback version; an unrelated or
  ambiguous launcher is not overwritten.
- Cleanup keeps the current version plus the two newest inactive rollback
  versions. Process-lifetime version locks protect binaries still in use;
  ambiguous launcher or lock state causes cleanup to retain rather than
  delete. Protected running versions can therefore temporarily raise the
  on-disk count above three.
- Interactive TTY chat starts the updater asynchronously, checks immediately,
  then checks every 30 minutes while the process remains alive. Successful
  downloads are activated for the next invocation, so the running process is
  never replaced underneath itself. Errors remain non-fatal and do not block
  TUI startup. `METIS_NO_UPDATE_CHECK=1` disables this automatic check/install
  loop but does not disable the explicit `metis update` and
  `metis update --check` commands.
- `install/npm/` remains a private/local-development wrapper and is not the
  documented public install path.

The Desktop build matrix runs for stable tag pushes and is required before that
release is published. A manual release-workflow dispatch remains a CLI-only
rebuild path; it does not synthesize Desktop assets for an existing tag.
