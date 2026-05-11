# Metis

Local-first agent CLI in Go. Single static binary, full bubbletea TUI,
multi-provider streaming, MCP-ready, with persistent memory and a small
plugin system.

> Naming history: `talon` → `Delphi` (2026-04-26) → **`Metis`** (2026-04-29).
> Greek goddess of cunning intelligence, mother of Athena. Zeus swallowed
> her to internalize her wisdom; the metaphor for an LLM agent that
> ingests knowledge and embodies action.

## Why another agent CLI?

Most options are heavy ("AI OS" with frontend + daemon + Docker), Python-
only (slow startup, hard to ship), or tightly coupled to one provider's
quirks. `Metis` aims for:

- **Fast** — single static Go binary, sub-100ms cold start, no Node /
  Python runtime in the loop.
- **Local-first** — state lives in `~/.metis/` (sessions, memory, plans,
  tasks, history). Nothing phones home.
- **Multi-provider** — Anthropic Messages API, OpenAI Chat Completions,
  Google Gemini native, plus any OpenAI-compatible or Anthropic-
  compatible gateway (MiniMax, Together, Groq, Ollama, OpenRouter).
- **MCP-native** — stdio + Streamable HTTP/SSE clients; tools auto-
  registered and namespaced.
- **Permission-aware** — 5 modes (`ask` / `auto` / `bypass` / `plan` /
  `deny`), cascading rules from CLI > project > user > defaults, "always
  allow" remembered for the session, input-dependent bash classifier.
- **Streaming-first** — text deltas + tool input deltas render as they
  arrive; safe tools fan out in parallel, queueable tools FIFO, exclusive
  tools serialize.
- **Memory-aware** — three-tier (Core block / Archival JSONL / Recall
  history) with frozen-snapshot semantics borrowed from Hermes and
  context-fencing borrowed from Claude Code.

## Install

This project is local-only — there's no public release URL. Build locally:

```sh
make install            # builds + installs to ~/.local/bin/metis and ~/go/bin/metis
metis version
```

Or via the curl one-liner against a local release dir:

```sh
make dist               # produces dist/metis-{darwin,linux}-{arm64,amd64}.tar.gz
mkdir -p ~/.metis-releases/$(cat VERSION)
cp dist/*.tar.gz dist/*.sha256 ~/.metis-releases/$(cat VERSION)/

METIS_VERSION=$(cat VERSION) bash install/install.sh
```

`METIS_RELEASE_BASE` defaults to `file://$HOME/.metis-releases` — there
is **no hardcoded public URL**.

## Usage

```sh
metis                         # interactive chat (full bubbletea TUI)
metis chat                    # same
metis run "<prompt>"          # one-shot, prints reply, exits
metis config show             # effective config + which files were read
metis config init             # write starter config to ~/.metis/config.toml
metis tools                   # list registered tools (built-in + MCP + plugin)
metis sessions list           # recent saved sessions
metis skills list             # built-in skills library
metis skills install <ref>    # install a skill (bundled name or owner/repo:name)
metis skills info <name>      # show one skill's manifest fields
metis skills uninstall <name> # remove a skill
metis ps [--limit N]          # list recent sessions (newest first; pid + size + title)
metis logs <session-id>       # print a session's transcript (compact role/peek format)
metis attach <session-id>     # alias of `metis chat -r <id>` (tmux-attach parity)
metis kill <session-id>       # SIGTERM the metis pid backing this session
metis daemon [--idle 10m]     # KAIROS-style file-watcher (~/.metis/inbox → outbox)
metis plugin list             # installed plugins
metis plugin info <name>      # show one plugin's manifest details
metis plugin remove <name> [--yes]  # delete a plugin (--yes to actually rm -rf)
metis cron <list|add|...>     # scheduled-job CRUD (see Scheduling section)
metis acp [--addr ADDR]       # JSON-RPC server (stdio default; TCP for Zed/etc.)
metis auth login              # opencode-style provider wizard (writes ~/.metis/auth.json)
metis update [--check]        # self-update from the private GitHub release
metis version [-V]            # short semver (-V for full build fingerprint)
```

### Flags

| Flag | What |
|------|------|
| `-m, --model <id>` | override model |
| `-p, --provider <id>` | `anthropic` / `openai` / `gemini` / any custom |
| `--mode <id>` | permission mode (`ask` / `auto` / `bypass` / `plan` / `deny`) |
| `--dangerously-skip-permissions` | alias of `--mode bypass` (named for Claude Code muscle memory) |
| `-c, --continue` | resume the most recently modified session |
| `-r, --resume [<id>]` | resume a specific session id; bare `-r` opens an interactive picker |
| `-d, --debug` | mirror logs into `~/.metis/debug.log` |
| `--bare` | skip MCP / plugin loaders for fastest cold start |
| `-s, --scope <local\|user\|project>` | config scope (today only `user` is honored) |
| `--input-format json` | `metis run`: read NDJSON prompts from stdin |
| `--output-format json\|stream-json` | `metis run`: emit structured events |
| `--no-markdown` | disable glamour markdown rendering |
| `--no-stream` | wait for the full reply before printing |
| `--streamlined` | thinking dropped, tool calls collapsed into summaries |
| `--max-iter <n>` | cap tool iterations per turn |
| `--system <text>` | override system prompt |
| `--effort low\|medium\|high` | reasoning intensity (Anthropic thinking, OpenAI reasoning_effort) |
| `--fast` | one-shot fast turn (effort=low + halved max_tokens) |
| `--add-dir <path>` | add a directory to the agent's accessible scope (repeatable) |
| `--agent <name>` | load an agent profile from `~/.metis/agents/<name>.md` |
| `--worktree <slug>` / `-W` | spin up the session inside a git worktree |
| `--name <text>` | human-friendly session label (visible in `/sessions`) |
| `--agent-teams` | start in agent-teams mode (alias for `/batch` entry path) |
| `--tmux` | when starting in a worktree, also wrap in a tmux pane |
| `--tui` | force the TUI (default when stdout is a TTY) |
| `--no-auth-wizard` | skip the first-run auth wizard |

### Slash commands (in chat)

Session: `/new` `/clear` `/retry` `/undo` `/history` `/save` `/title`
`/rename` `/tag` `/branch` `/sessions` `/export`

Mode: `/plan` `/auto` `/bypass` `/compact` `/effort` `/fast` `/output-style`

Info: `/status` `/session` `/model` `/tools` `/skills` `/memory`
`/cost` `/usage` `/tokens` `/context` `/stats` `/keybindings` `/permissions`
`/hooks` `/doctor` `/version` `/help` `/onboarding`

Productivity: `/copy [N]` `/share` `/export` `/files` `/recap` `/replay`
`/insights [--days N]` `/lessons` `/break-cache` `/statusline`

Git / review: `/diff` `/git` `/commit` `/log` `/checkout` `/stash` `/fetch`
`/review` `/security-review` `/commit-push-pr` `/feedback`

Tooling: `/loop` `/cron` `/edit` `/agents` `/batch` `/btw` `/abort`
`/voice` `/thinkback` `/ultraplan`

MCP: `/mcp list` `/mcp add <name> <cmd>` `/mcp remove <name>`
`/mcp enable <name>` `/mcp disable <name>` `/mcp edit [<name>]`
`/mcp test <name>` `/mcp logs <name>` `/mcp reload` `/mcp start <name>`
`/cu enable` `/cu disable`

Skills: `/skills list` `/skills install <name>` `/skills remove <name>`
`/skills info <name>` `/skills edit <name>` `/skills enable <name>`
`/skills disable <name>` `/skills create <name>` `/skills search <query>`

User-authored: drop `*.md` files under `~/.metis/commands/` or
`<cwd>/.metis/commands/`. Each becomes `/<filename>`. YAML frontmatter
sets the description; `$ARGUMENTS` / `$1` / `$2` get substituted.

MCP servers that advertise prompts/list register automatically as
`/mcp__<server>__<prompt>` slashes.

### Keybindings (in chat)

| Key | What |
|------|------|
| `Shift+Tab` | cycle permission mode (ask → auto → bypass → plan) |
| `Ctrl+T` | toggle todo overlay |
| `Ctrl+O` | expand the last tool result |
| `Ctrl+P` | session picker |
| `Ctrl+R` | reverse history search (fuzzy match prior prompts) |
| `Ctrl+S` | toggle copy mode (exit alt-screen so you can mouse-select the whole transcript) |
| `Ctrl+Y` | yank last assistant reply to clipboard (OSC 52 + `~/.metis/clipboard.txt` fallback) |
| `Ctrl+Shift+Y` | yank full transcript (filtered to user/assistant/bash) |
| `Ctrl+L` | redraw screen |
| `Ctrl+V` | paste clipboard (text → input, image → `[Image #N]` placeholder) |
| `Ctrl+G` | open the input draft in `$VISUAL`/`$EDITOR`/`vi`; saves on exit |
| `Ctrl+X` | toggle shell mode (next input runs as `bash -c <input>`) |
| `Ctrl+J` | newline (alt to Alt+Enter) |
| `Ctrl+C` | interrupt running turn + clear queued prompts / single-tap idle = quit |
| `Ctrl+D` | quit |
| `↑` / `↓` (single-line) | jump to start / end of input (also recall history when empty) |
| `Esc Esc` | clear current input (no submit) |
| `PgUp` / `PgDn` | scroll transcript |
| `@filename` | live file-picker dropdown — `↑↓` select, `Tab` accept |
| `!cmd` | bash mode — runs `cmd` in shell without invoking the LLM |
| Enter mid-turn | queue input; runs as the next turn after the current one finishes |

## Configuration

Edit `~/.metis/config.toml`. Project-local `./.metis/config.toml` overrides.

```toml
[provider]
default = "anthropic"

[provider.anthropic]
api_key_env = "ANTHROPIC_API_KEY"
model = "claude-opus-4-7"
max_tokens = 64000
context_window = 200000     # override per-model lookup; required for compat gateways
timeout_seconds = 120

[provider.openai]
api_key_env = "OPENAI_API_KEY"
base_url = "https://api.openai.com/v1"
model = "gpt-4o"

[provider.gemini]
api_key_env = "GEMINI_API_KEY"
model = "gemini-2.5-pro"

# Custom profiles — unlimited, each names its own transport so the same
# upstream service can be configured under multiple wire formats. Useful
# when a vendor exposes both Anthropic-compatible and OpenAI-compatible
# endpoints (MiniMax, OpenRouter, GLM, …).
[provider.custom.minimax-openai]
transport   = "openai_chat"            # anthropic_messages | openai_chat | gemini_native
api_key_env = "MINIMAX_API_KEY"
base_url    = "https://api.minimaxi.com/v1"
model       = "MiniMax-M2.7"
context_window = 192000

[provider.custom.deepseek]
transport   = "openai_chat"
api_key_env = "DEEPSEEK_API_KEY"        # 1st preference: env var
# api_key   = "sk-..."                  # 3rd preference: inline (lowest, after auth.json)
base_url    = "https://api.deepseek.com/v1"
model       = "deepseek-chat"
context_window = 1000000

# Auth chain for both built-in (anthropic/openai/gemini) and custom
# providers — first non-empty wins:
#   1. env var named in api_key_env
#   2. ~/.metis/auth.json entry (`metis auth login <name>`)
#   3. inline api_key field in this block

# Switch profiles at run time:
#   metis -p minimax-openai chat
#   metis -p deepseek run "..."

# Don't know the right base_url / model name / env var? Run:
#   metis models                      # list 117 providers from models.dev
#   metis models deepseek             # all DeepSeek models + cost + context
#   metis models deepseek deepseek-chat  # ready-to-paste config snippet

[permission]
mode = "auto"
[[permission.allow]]
tool = "Read"
[[permission.allow]]
tool = "Bash"
match = "git status"

[ui]
theme = "auto"
markdown = true
show_tokens = true

[ui.performance]
tick_ms = 40                  # 25fps default; 16=60fps, 100=10fps
event_buffer_size = 256       # agent.Event channel depth
mouse_wheel_lines = 1         # 1=pixel-precise, 3=jumpy
reduced_motion = false        # accessibility: 500ms tick + no shimmer

[session]
auto_compact_threshold = 0.85   # fraction of context window
max_iterations = 50

[tools]
# ToolSearch lazy MCP schema is controlled by the ENABLE_TOOL_SEARCH
# env var. Defaults match claude-code's `tst` (always-defer); set
# explicit "auto" to opt into openclaude's `tst-auto` budget mode.
#
#   (unset)     → always strip mcp__* schemas (default since 2026-05-11)
#   true        → same as unset
#   auto        → fires when MCP schemas exceed 10% of context window
#   auto:N      → auto, fires at N% (1..99)
#   auto:0      → always lazy (alias for "true")
#   auto:100    → never lazy (alias for "false")
#   false       → never strip (debug — sends full schemas every turn)
#
# Once stripped, the model calls the synthetic ToolSearch meta-tool
# to fetch a schema on demand. ToolSearch accepts:
#   {"query": "select:n1,n2"} — exact multi-fetch (returns schemas)
#   {"query": "screenshot"}   — keyword search (returns names+descs)
#   {"query": "+slack send"}  — required term (+) + ranking terms
#
# Schemas previously fetched in this session are remembered and kept
# intact across compaction so the model doesn't re-pay the lookup
# round-trip — cross-compaction stability mirrors openclaude's
# tool_reference scanning. See internal/agent/lazy_tools_discovered.go.
#
# Examples:
#   ENABLE_TOOL_SEARCH=auto:5  metis chat   # opt into auto + 5% threshold
#   ENABLE_TOOL_SEARCH=false   metis chat   # always full schemas (debug)

[tools.bash]
timeout_seconds = 120
max_output_bytes = 1048576
denylist = ["rm -rf /", "shutdown"]

[loop_detection]
# On by default since 2026-05-08. Set `disabled = true` to opt out.
# Sliding-window signature detector (crush-parity): SHA-256 over
# (tool_name, JSON(input), result) for each step's batch; if any
# signature appears past `signature_max_repeats` times within
# `signature_window` steps the loop aborts.
signature_window      = 10  # steps to keep in the sliding window
signature_max_repeats = 5   # same-signature count that trips the abort
warning               = 10  # legacy per-tool consecutive-call warning
critical              = 20  # legacy per-tool consecutive-call critical
global                = 80  # absolute ceiling on tool calls per Run
```

### Background bash + job pool

Long-running shell commands have two paths:

- **Auto-background**: any foreground `Bash` that runs longer than
  60 seconds gets promoted to a background job. The process keeps
  running; the model gets a `job_id` reply and the conversation
  continues. When the job finishes, the next iteration's prompt
  carries a `<job_notification>` system-reminder.
- **Explicit background**: pass `run_in_background: true` to `Bash`
  (e.g. dev servers, watchers, multi-minute builds). Same pool, same
  notification envelope.

Three tools work the pool:

| Tool | Purpose |
|---|---|
| `BashList` | JSON snapshot of all jobs (id, status, command, elapsed, exit_code) |
| `BashOutput` | Read a job's captured stdout/stderr; `tail_max` caps return size (default 50 KiB) |
| `BashKill` | SIGTERM + 2s grace + SIGKILL escalation |

Output is captured to `~/.metis/jobs/<id>.out` (mode 0600). The TUI
status bar shows `⚙ N jobs` while jobs are alive.

Sleep blacklist: `sleep N` (N ≥ 2s) standalone or `sleep N && rest`
are rejected — they're polling primitives that shouldn't burn the
foreground turn. Sub-2s pacing, pipeline / subshell / loop sleeps
are fine.

### Desktop notifications (5-channel matrix)

When a turn runs longer than 30 seconds and you haven't pressed a key
in the last 6, metis pops a desktop notification so you can switch back
in. The channel is auto-detected from `$TERM_PROGRAM` and other env
markers, or you can force one:

```sh
METIS_NOTIFY_CHANNEL=auto             # default — pick by terminal
METIS_NOTIFY_CHANNEL=iterm2           # OSC 9 (iTerm2 / WezTerm / Alacritty)
METIS_NOTIFY_CHANNEL=iterm2_with_bell # OSC 9 + raw BEL on top
METIS_NOTIFY_CHANNEL=kitty            # OSC 99 3-step protocol
METIS_NOTIFY_CHANNEL=ghostty          # OSC 777;notify;<title>;<body>
METIS_NOTIFY_CHANNEL=bell             # raw \x07 only (Apple Terminal w/ audible bell off)
METIS_NOTIFY_CHANNEL=off              # disable entirely
```

Auto rules:
- `iTerm.app` / `WezTerm` / `Alacritty` → iTerm2 (OSC 9)
- `ghostty` → Ghostty (OSC 777)
- `KITTY_WINDOW_ID` set → kitty (OSC 99 with title / body / focus)
- `Apple_Terminal` → only emits BEL when the active profile has
  audible bell off (visual-bell-only) — otherwise stays silent so we
  don't spam the user's speakers
- nothing recognized → off

Inside tmux / GNU screen, OSC sequences are wrapped in DCS passthrough
so they reach the outer terminal (requires `set -g allow-passthrough on`
in `.tmux.conf`). Raw BEL is intentionally never wrapped — it preserves
tmux's bell-action window flag.

iTerm2 / Ghostty / WezTerm also get a dock-icon progress bar (OSC 9;4)
that lights up while the turn is running and clears on completion.

## Built-in tools

| Tool | Concurrency | Notes |
|------|-------------|-------|
| `Read` | safe | line-numbered, offset+limit, image-aware |
| `LS` | safe | filters dot-dirs |
| `Glob` | safe | doublestar globs, sorted by mtime |
| `Grep` | safe | Go regex, skips `.git`/`node_modules`/`vendor` |
| `WebFetch` | safe | bounded body, configurable timeout |
| `Search` | queue | web search via DuckDuckGo HTML |
| `Git` | queue | safe git subcommands; mutating ops downgrade to exclusive |
| `Bash` | input-dep | classified per command — `ls`/`grep` safe, `rm`/`git push` exclusive |
| `Edit` | exclusive | unique-match enforced, structured diff render |
| `Write` | exclusive | absolute paths only |
| `Memory` | exclusive | persistent memory CRUD |
| `Skill` | safe | invoke a registered skill |
| `Ask` | safe | mid-turn user clarification |
| `TodoWrite` | exclusive | task list, persisted per-session |
| `Agent` | queue | spawn sub-agent with isolated history |
| `SendMessage` | safe | send to a registered channel (Telegram, Slack, …) |
| `ScheduleWakeup` | safe | LLM self-pacing — schedule a future re-entry with a prompt |

## Scheduling

Three layers; details in [`docs/ARCHITECTURE.md#scheduling--continuous-execution`](docs/ARCHITECTURE.md#scheduling--continuous-execution).

```sh
# 5-field cron (or @daily / @hourly / @every 1h30m descriptors)
metis cron add --cron "0 9 * * 1-5" --tz "America/Los_Angeles" \
  --prompt "post weekday metrics" --mode persistent --jitter 30s

# fixed interval
metis cron add --every 5m --prompt "tail nginx errors" --mode isolated

# one-shot reminder (auto-disables after firing)
metis cron add --at "2026-05-01T09:00:00Z" --prompt "release reminder" --repeat 1

# limit which tools cron can use (Hermes-style blacklist)
metis cron add --every 1h --prompt "scan for new bugs" --disable-tools "Agent,WebFetch"

metis cron list
metis cron pause <id>
metis cron resume <id>
metis cron rm <id>
metis cron run <id>           # fire immediately, ignore schedule
metis cron start              # foreground daemon (Ctrl+C to stop)
```

`SessionMode` choices:

- `isolated` (default) — every fire starts with empty history
- `persistent` — per-job rolling thread (history accumulates across firings)
- `main` — append to a shared named session (`--session main` default)

Inside chat the LLM has the `ScheduleWakeup` tool — it can decide on its
own when to re-enter (e.g. "check if the build is done in 5 min"). The
wakeup persists as a one-shot cron job, so `metis cron list` shows what
the agent has scheduled itself.

## Channels (chat-platform adapters)

`internal/channels/*` ships adapters for **DingTalk, Discord, Feishu,
iMessage, Mattermost, Signal, Slack, Telegram, WeChat, WhatsApp**.
Configured via `[channels.<name>]` in config; `SendMessage` tool routes to
them. Use cases: ops automations, scheduled cron-driven reports.

## Plugins

Drop a directory under `~/.metis/plugins/<name>/` with a `plugin.toml`:

```toml
manifest_version = 1
name = "browser-mcp"
version = "0.3.1"
description = "Browser automation"

[mcp_server]
command = "node"
args = ["index.js"]

skills = ["skills/screenshot.md"]
```

```sh
metis plugin list                       # show installed
metis plugin info <name>                # manifest details
metis plugin remove <name>              # dry-run (just prints path)
metis plugin remove <name> --yes        # actually delete (rm -rf the dir)
```

Plugin tools register as `plugin__<name>__<tool>`; plugin skills are
namespaced as `<plugin>:<skill>` so they don't collide with bundled or
user skills. Both surface to the LLM through the regular tool / skill
discovery paths — no extra wiring needed.

There's no `metis plugin install` (no remote registry yet) — to "install"
a plugin from elsewhere, just clone or copy its directory under
`~/.metis/plugins/`.

## Computer use (driving the desktop)

Companion binary [`metis-cu`](https://github.com/Ricardo-M-L/metis-cu)
exposes 24 desktop-control tools (screenshot / mouse / keyboard /
clipboard / window) over the standard MCP stdio transport. The tool
names and parameter shapes mirror Anthropic's `mcp__computer-use__*`
namespace exactly, so prompts and traces written for Claude Code's
built-in computer-use server work without translation.

Install + register:

```sh
# Install the binary (Go 1.24+, cgo required for the host platform)
git clone https://github.com/Ricardo-M-L/metis-cu && cd metis-cu
make install                # writes ~/go/bin/metis-cu + ~/.local/bin/metis-cu

# Register with metis (one liner — writes ~/.metis/mcp.toml + hot-loads)
metis chat
> /cu enable
cu: enabled — computer-use (24 tools); binary=/Users/.../go/bin/metis-cu
```

Then in chat the tools appear as `mcp__computer-use__screenshot`,
`mcp__computer-use__left_click`, etc. — fully discoverable to the LLM.

Slash commands:

| Command | Effect |
|---|---|
| `/cu enable` | Add `metis-cu` to `mcp.toml`, hot-load tools into the live session |
| `/cu disable` | Remove the entry; tools persist this session, gone after restart |
| `/cu status` | Report whether enabled, where the binary lives |

### Tier-based safety gate

`metis-cu` classifies the frontmost app into one of three tiers and
refuses input the LLM shouldn't be sending:

| Tier | Allowed | Apps (defaults) |
|---|---|---|
| `read` | screenshot, read_clipboard | browsers (Chrome / Safari / Firefox / Edge / Brave) |
| `click` | + left_click, mouse_move, scroll | terminals + IDEs (iTerm2 / VS Code / Cursor / GoLand / IntelliJ) |
| `full` | + key, type, write_clipboard, … | everything else |

Override via the in-tool `request_access` call — approvals persist to
`~/.metis-cu/granted.json` so the same approval survives MCP server
restarts.

### Platforms

`metis-cu` ships full implementations for **macOS** (CGEvent +
osascript), **Linux X11/XWayland** (XTEST + xdotool), and **Windows**
(SendInput + user32 GetForegroundWindow). All three are tested by the
metis-cu repo's CI matrix; native Wayland (no XWayland) is the only
known gap.

## ACP server (Zed / IDE integration)

Run metis as a JSON-RPC 2.0 server over stdio or TCP. Lets editors and
custom scripts drive the agent without spawning a TUI.

```sh
metis acp                                # stdio mode (Zed-compatible)
metis acp --addr 127.0.0.1:18765         # TCP mode (one connection per client)
```

Protocol:

- Request `prompt` with `{prompt: "..."}` → server streams `session_update`
  notifications (kind = `text_delta` / `thinking_delta` / `tool_start` /
  `tool_result` / `tokens` / `loop_done` / `permission_request` / …) and
  finally responds with `{done: true}`.
- Request `abort` with `{prompt_id: "..."}` to cancel an in-flight prompt.
- Request `permission_reply` with `{tool_use_id, decision: allow|deny|always}`
  to answer a `permission_request` notification.

Same agent core as `metis chat` — every tool / skill / MCP server you
configured shows up in the ACP session too.

## Architecture

See [`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md). Cross-project
positioning lives in [`../COMPARISON.md`](../COMPARISON.md).

```
cmd/metis/                main, auth, plugin, trust subcommands
pkg/                      public SDK (provider, tool, hook, channel,
                          skill, memory, session, plugin, llm.Effort)
internal/llm/             Anthropic / OpenAI / Gemini stream parsers
internal/tools/           Tool interface + registry
internal/tools/builtin/   16 first-party tools
internal/permission/      Cascading rule gate + 5 modes
internal/agent/           Loop, dispatch, streaming, compaction,
                          hooks, plan, skills, cron, loop-detection
internal/agent/skills/    SKILL.md loader (5 layers: bundled / user /
                          project / plugin / mcp), 22 bundled skills
internal/mcp/             stdio + Streamable HTTP/SSE clients
internal/channels/        11 chat-platform adapters
internal/memory/          Core/Archival/Recall + daily notes + freshness
internal/session/         JSONL persistence, branch + snapshot
internal/runtime/         composer helpers (provider, channels, mcp,
                          plugin, system_prompt, plan_archive, …)
internal/tui/             bubbletea TUI (split into ~50 files)
install/                  curl + npm installer wrappers
```

## License

MIT — see LICENSE.
