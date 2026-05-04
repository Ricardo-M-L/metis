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
metis plugin list             # installed plugins
metis plugin info <name>      # show one plugin's manifest details
metis plugin remove <name> [--yes]  # delete a plugin (--yes to actually rm -rf)
metis cron <list|add|...>     # scheduled-job CRUD (see Scheduling section)
metis acp [--addr ADDR]       # JSON-RPC server (stdio default; TCP for Zed/etc.)
metis auth login              # opencode-style provider wizard (writes ~/.metis/auth.json)
metis version
```

### Flags

| Flag | What |
|------|------|
| `-m, --model <id>` | override model |
| `-p, --provider <id>` | `anthropic` / `openai` / `gemini` / any custom |
| `--mode <id>` | permission mode |
| `--no-markdown` | disable glamour markdown rendering |
| `--max-iter <n>` | cap tool iterations per turn |
| `--system <text>` | override system prompt |
| `--resume <id>` | resume a saved session |
| `--effort low\|medium\|high` | reasoning intensity (Anthropic thinking, OpenAI reasoning_effort) |
| `--fast` | one-shot fast turn (effort=low + halved max_tokens) |

### Slash commands (in chat)

Session: `/new` `/clear` `/retry` `/undo` `/history` `/save` `/title`
`/branch` `/sessions`

Mode: `/plan` `/auto` `/bypass` `/compact` `/effort` `/fast`

Info: `/status` `/session` `/model` `/tools` `/skills` `/memory`
`/version` `/help`

Tooling: `/loop` `/cron` `/edit` `/agents` `/skill <name>` `/mcp add`
`/mcp remove` `/mcp start` `/cu enable` `/cu disable`

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
| `Ctrl+L` | redraw screen |
| `Ctrl+V` | paste clipboard (text → input, image → `[Image #N]` placeholder) |
| `Ctrl+J` | newline (alt to Alt+Enter) |
| `Ctrl+C` | interrupt running turn / single-tap idle = quit |
| `Ctrl+D` | quit |
| `PgUp` / `PgDn` | scroll transcript |
| `@filename` | live file-picker dropdown — `↑↓` select, `Tab` accept |
| `!cmd` | bash mode — runs `cmd` in shell without invoking the LLM |

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
api_key_env = "DEEPSEEK_API_KEY"
base_url    = "https://api.deepseek.com"
model       = "deepseek-chat"
context_window = 1000000

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

[tools.bash]
timeout_seconds = 120
max_output_bytes = 1048576
denylist = ["rm -rf /", "shutdown"]
```

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
