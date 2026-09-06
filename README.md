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
- **Local-first** — persistent state stays on local disk, primarily in
  `~/.metis/` with opt-in project-local `.metis/` data. Model requests,
  configured MCP/web/channel tools, plugin marketplaces, and the
  background GitHub release check and native auto-updater can use the
  network. Set `METIS_NO_UPDATE_CHECK=1` to disable both the automatic
  check and install loop.
- **Multi-provider** — native Anthropic Messages, OpenAI Chat Completions,
  and Google Gemini transports; custom profiles support compatible
  gateways plus Azure OpenAI, Vertex Anthropic, and Bedrock Anthropic
  cloud-auth transports.
- **MCP-native** — stdio + Streamable HTTP/SSE clients; tools auto-
  registered and namespaced.
- **Permission-aware** — Claude Code's 5 public modes (`default` /
  `acceptEdits` / `plan` / `dontAsk` / `bypassPermissions`) plus METIS
  `fullAccess` (no approvals and no process sandbox), cascading
  authority from managed policy > CLI > in-session approvals > config >
  persisted approvals, plus an input-dependent bash classifier.
- **Streaming-first** — text deltas + tool input deltas render as they
  arrive; safe tools fan out in parallel, queueable tools run FIFO,
  exclusive tools serialize, and background tools return immediately.
- **Memory-aware** — three-tier (Core block / Archival JSONL / Recall
  history) with frozen-snapshot semantics borrowed from Hermes and
  context-fencing borrowed from Claude Code.

## Install

### macOS / Linux

Install the latest public release (`arm64` or `amd64`):

```sh
curl -fsSL https://raw.githubusercontent.com/Ricardo-M-L/metis/main/install/install.sh | bash
"$HOME/.local/bin/metis" version
```

### Windows (PowerShell)

```powershell
irm https://raw.githubusercontent.com/Ricardo-M-L/metis/main/install/install.ps1 | iex
& "$env:LOCALAPPDATA\Programs\Metis\bin\metis.exe" version
```

### Metis Desktop

Install the CLI first, then download the Desktop archive for your platform
from the [latest release](https://github.com/Ricardo-M-L/metis/releases/latest):

- macOS: `metis-desktop-darwin-universal.zip` (unpack it and move the app to
  **Applications** before launching it).
- Windows: `metis-desktop-windows-amd64.zip`.
- Linux: `metis-desktop-linux-amd64.tar.gz`.

Each archive has a matching `.sha256` file. The macOS ZIP is also the artifact
used by the verified in-app updater.

The Windows installer writes the stable command to
`%LOCALAPPDATA%\Programs\Metis\bin` by default and tells you how to add
that directory to your user `PATH` when necessary. Both installers verify
the release SHA-256 before switching the stable command to the downloaded
version.

Public releases do not require a GitHub token. `METIS_GITHUB_TOKEN` (or
`GITHUB_TOKEN`) is optional and only raises GitHub's API rate limit. To
pin a release or choose another directory, set `METIS_VERSION` and
`METIS_INSTALL_DIR` before running the installer. For example:

```sh
curl -fsSL https://raw.githubusercontent.com/Ricardo-M-L/metis/main/install/install.sh | \
  env METIS_VERSION=vX.Y.Z METIS_INSTALL_DIR="$HOME/.local/bin" bash
```

```powershell
$env:METIS_VERSION = "vX.Y.Z"
$env:METIS_INSTALL_DIR = "$env:LOCALAPPDATA\Programs\Metis\bin"
irm https://raw.githubusercontent.com/Ricardo-M-L/metis/main/install/install.ps1 | iex
```

The bootstrap command is only needed for the first install. A native install
uses a managed, versioned layout:

- macOS/Linux: `~/.local/bin/metis` points to
  `~/.local/share/metis/versions/<version>/metis`.
- Windows: `%LOCALAPPDATA%\Programs\Metis\bin\metis.exe` is the stable
  launcher; immutable copies live under
  `%LOCALAPPDATA%\Programs\Metis\versions\<version>\metis.exe`.

When interactive `metis` starts on a TTY, it immediately starts a release
check off the startup path and repeats the check every 30 minutes while that
process is running. A new release is downloaded, verified and installed in
the background on macOS, Linux and Windows. The current process keeps using
the version it started with; the next invocation uses the newly installed
version. Set `METIS_NO_UPDATE_CHECK=1` to disable this automatic loop. The
explicit `metis update` and `metis update --check` commands remain available
and are not disabled by that variable.

Cleanup normally keeps the active version plus the two newest rollback
versions. A version still used by a running process is protected until a
later cleanup, so there can temporarily be more than three version
directories. On Windows, a renamed running launcher is likewise removed
by a later cleanup after the process releases it.

For local development, build from source:

```sh
make build              # versioned source build at ./bin/metis
./bin/metis
```

`make install` installs one local-source binary at `~/.local/bin/metis` only
when that path is not owned by the curl installer. It refuses to replace a
managed release and never writes a second copy to `~/go/bin`; this prevents a
stale source build from shadowing native automatic updates.

## Usage

```sh
metis                         # interactive chat (full bubbletea TUI)
metis run "<prompt>"          # one-shot, prints reply, exits
metis login                   # choose a provider and API-key/OAuth sign-in
metis login openai            # choose ChatGPT browser sign-in or API key
metis logout openai-codex     # remove stored credentials for a provider
metis config show             # effective config + which files were read
metis models [provider] [model] # browse models.dev and print config snippets
metis sessions list           # recent saved sessions
metis skills list             # discover available skills
metis plugin list             # installed plugins
metis cron list               # scheduled jobs
metis diag [--llm] [--tool-smoke] [--json]   # non-interactive health check
metis update --check          # check the public GitHub release
metis version [-V]            # short semver (-V for full build fingerprint)
metis help                    # complete current command surface
```

Run `metis help` for the current top-level command and common-flag overview;
subcommand-specific help and `metis env` cover the detailed surfaces.

### Provider login

`metis login [provider]` is the canonical credential setup command. With no
provider it opens an interactive picker; when a provider supports more
than one method it then asks for API key or subscription OAuth. You can skip
those choices explicitly. Choose OpenAI, then **Sign in with ChatGPT** to open
the OpenAI authorization page. After authorization, METIS saves the refreshable
credential and selects `openai-codex` for subsequent chats; you do not need to
enter an API key. The API key option continues to use the separate OpenAI
Platform provider and billing. Model access and usage follow your account's
available entitlements. In `/model` and the Desktop model menu, one ChatGPT
login unlocks the curated `openai-codex` GPT/Codex catalog, while models from
providers without usable credentials remain hidden. OpenAI Platform API-key
models stay separate.

```sh
metis login anthropic --method api-key
metis login anthropic --method oauth
metis login openai                   # select Sign in with ChatGPT
metis login openai --method oauth    # open browser sign-in directly
metis login openai --device-code     # headless ChatGPT login
metis login openai --method api-key  # OpenAI Platform API key
metis login gemini                    # Google AI Studio API key
metis login anthropic --method oauth --manual  # SSH/headless code-paste flow
```

The `metis login openai-codex` spelling remains supported, including its
browser/device-code picker. After signing in, run `metis` or
`metis run "Reply with hello"` to use the selected account.

Anthropic subscription OAuth is experimental because Anthropic does not
publish a compatibility contract for third-party CLIs. METIS uses its truthful
application identity and does not copy Pi's Claude Code impersonation headers,
system identity, or tool-name rewriting. Use an Anthropic API key when you need
a supported production path. Pi also documents third-party harness traffic as
per-token "extra usage" rather than Claude plan allowance; METIS has not
independently verified that billing behavior, so check
`https://claude.ai/settings/usage` before using this path.

API-key input and manual OAuth codes are masked. Stored credentials are never
printed by `metis auth list`; it reports only the configured provider and
method. The older `metis auth login` and `metis auth oauth` spellings remain
available for compatibility.

Managed API keys for custom providers are bound to that provider's transport
and normalized base URL. Changing either value makes the old key unavailable
until you run `metis login <provider>` again; this prevents a stale credential
from being sent to a newly configured endpoint. Model-only changes do not
require another login.

Provider routing is workspace-trust-sensitive. Until a project has been
explicitly trusted, its `[provider]` tables are ignored by chat, `run`, login,
Desktop switching/probing, and TUI provider hot reload; user-level routing
remains active. `METIS_NO_TRUST_PROMPT=1` suppresses the prompt but does not
grant that trust. Custom endpoints must use HTTPS, except loopback HTTP
(`localhost`, `127.0.0.0/8`, or `::1`) for local runtimes such as Ollama.

Before the first upgrade from a legacy release, quit older METIS CLI/Desktop
processes so they cannot rewrite the retired credential files while the new
process migrates them into `~/.metis/.credentials/`.

### Flags

| Flag | What |
|------|------|
| `-m, --model <id>` | override model |
| `-p, --provider <id>` | `anthropic` / `openai` / `openai-codex` / `gemini` / any custom |
| `--mode <id>` | permission mode (`default` / `acceptEdits` / `plan` / `dontAsk` / `bypassPermissions` / `fullAccess`) |
| `--dangerously-skip-permissions` | alias of `--mode bypassPermissions` |
| `--dangerously-bypass-approvals-and-sandbox` | alias of `--mode fullAccess`; disables approval prompts and the process sandbox |
| `-c, --continue` | resume the most recently modified session |
| `-r, --resume [<id>]` | resume a session by full UUID OR any unambiguous prefix (e.g. the 12-char id the picker prints). Bare `-r` opens the picker; ambiguous prefix errors with the candidate list |
| `-d, --debug` | mirror logs into `~/.metis/debug.log` |
| `--bare` | skip MCP / plugin loaders for fastest cold start |
| `--no-markdown` | disable glamour markdown rendering |
| `--streamlined` | `metis run`: drop thinking and collapse tool calls into summaries |
| `--max-iter <n>` | cap tool iterations per turn |
| `--max-budget-usd <x>` | stop once cumulative LLM spend reaches x USD (sub-agents share the pool) |
| `--output-schema <file>` | `metis run`: final reply must conform to this JSON Schema (2 retries, then exit 11) |
| `--system <text>` | override system prompt |
| `--effort low\|medium\|high` | reasoning intensity (Anthropic thinking, OpenAI reasoning_effort) |
| `--fast` | one-shot fast turn (effort=low + halved max_tokens) |
| `--add-dir <path>` | add a directory to the agent's accessible scope (repeatable) |
| `--agent <name>` | load an agent profile from `~/.metis/agents/<name>.md` |
| `--worktree <slug>` / `-W` | chat only: use the given worktree slug; bare `-W` generates one |
| `--name <text>` | human-friendly session label shown in session listings |
| `--coordinator` | restrict the main loop to coordination-oriented tools and prompts |
| `--tui` | chat only: force the TUI (default when stdout is a TTY) |
| `--no-auth-wizard` | skip the first-run auth wizard |
| `--tools <list>` | allowlist (CSV or space-separated): only expose these tools to the model. Empty = use config + all registered tools. |
| `--disallow-tools <list>` | blocklist (CSV/space): hide these tools from the model. Supports MCP server prefix — `mcp__office-word` mutes the whole server; `mcp__` mutes every MCP tool. |

### Tool-pool filtering

`metis chat --tools "Read,Edit,Bash"` — only those three are visible to the model this session. Useful for sandboxed audits or constrained sub-agents.

`metis chat --disallow-tools "mcp__office-word,WebFetch"` — every other tool stays available; that MCP server's tools and `WebFetch` are stripped before reaching the prompt. Saves substantial cache tokens.

Persistent equivalents live in `~/.metis/config.toml`:

```toml
[tools]
allowed    = ["Read", "Edit", "Bash"]            # session inherits unless --tools overrides
disallowed = ["mcp__office-word", "WebFetch"]    # CLI --disallow-tools is unioned in
```

Pattern grammar (allow + disallow):
- `Bash` — exact tool name
- `mcp__office-word` — every tool registered as `mcp__office-word__*` (the whole MCP server)
- `mcp__` or `mcp__*` — every MCP tool, all servers

CLI `--tools` REPLACES `cfg.Tools.Allowed` if set. CLI `--disallow-tools` UNIONS with `cfg.Tools.Disallowed` (CLI can tighten, never loosen).

### Slash commands (in chat)

Run `/help` inside chat for the live slash-command registry and current
syntax. This avoids pinning the README to handlers that may change between
releases.

In the interactive TUI, `/config` opens a searchable settings panel; press
`e` there to edit the raw TOML in `$EDITOR`. In the plain readline REPL,
`/config` opens `$EDITOR` directly.

User-authored: drop `*.md` files under `~/.metis/commands/` or
`<cwd>/.metis/commands/`. Each becomes `/<filename>`. YAML frontmatter
sets the description; `$ARGUMENTS` / `$1` / `$2` get substituted.

MCP servers that advertise prompts/list register automatically as
`/mcp__<server>__<prompt>` slashes.

### Skill sources

The runtime merges skill sources in increasing priority: bundled skills,
`~/.metis/optional-skills/`, the cross-agent `~/.agents/skills/` directory,
`~/.metis/skills/`, `<cwd>/.metis/skills/`, and plugin-contributed skills.
A later source wins when two skills have the same name.

### Keybindings (in chat)

| Key | What |
|------|------|
| `Shift+Tab` | cycle permission mode (default → acceptEdits → plan → bypassPermissions → fullAccess) |
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
| `Ctrl+C` | copy and clear an active input selection; otherwise interrupt a running turn + clear queued prompts / single-tap idle = quit |
| `Ctrl+D` | quit |
| `↑` / `↓` (single-line) | jump to start / end of input (also recall history when empty) |
| `Esc Esc` | clear current input (no submit) |
| `PgUp` / `PgDn` | scroll transcript |
| `@filename` | live file-picker dropdown — `↑↓` select, `Tab` accept |
| `!cmd` | bash mode — runs `cmd` in shell without invoking the LLM |
| Enter mid-turn | queue input; runs as the next turn after the current one finishes |

`fullAccess` is the final, red-marked step in the `Shift+Tab` cycle. It can also
be selected with `/permissions`, `/fullAccess`, Desktop settings, or the
dangerous CLI flag. Explicit policy denials, hooks, argument validation, OS
permissions, timeouts, and tool/provider errors still apply. When leaving
`fullAccess`, METIS stops background jobs and sub-agents and disconnects
MCP/Computer Use processes because an already-running unsandboxed process
cannot be made safe retroactively; reconnect those services if needed.
Mode switching remains available while a turn is running. A tool already in
`Execute` finishes under the posture that admitted it; the requested mode is
applied atomically before the next tool batch and the TUI remains responsive
while it waits for that boundary.

Mouse capture defaults to cell-motion mode so Metis can scroll the transcript,
copy clicked or dragged transcript text, open rendered links, and drag-select
the input at Unicode grapheme boundaries. Input selection copies on mouse-up
and stays highlighted; dragging beyond the input's top or bottom edge scrolls
long drafts, a bare click moves the caret, and `Ctrl+C` copies and clears the
retained selection. This is a screen-copy range, so typing dismisses the
highlight rather than replacing the selected draft text. Set
`METIS_DISABLE_MOUSE=1` before launch to leave mouse handling to the terminal.
With capture disabled, Metis does not receive wheel, click, drag, or link-click
events: use `PgUp`/`PgDn` and `Home`/`End` to navigate, `Ctrl+Y` or
`Ctrl+Shift+Y` to copy replies, and `Ctrl+S` for native selection (including
copying a URL to open manually).

## Configuration

Edit `~/.metis/config.toml`. Project-local `./.metis/config.toml` overrides.
On first run, the interactive setup can also create a custom API-key profile:
choose its wire protocol, paste either a base URL or a recognized full endpoint,
then enter the model id and editable API key. The key is stored separately in
`~/.metis/.credentials/auth.json`, never in the generated provider block.
Older root-level credential files are migrated automatically on first use.

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
# wire_protocol = "responses"          # use POST /responses instead of Chat Completions
# responses_state_mode = "local"       # local | provider | auto
# responses_profile = "openai"         # auto | openai | compatible
# hosted_tools = ["web_search"]        # optional provider-hosted search

[provider.gemini]
api_key_env = "GEMINI_API_KEY"
model = "gemini-2.5-pro"

# Custom profiles — unlimited, each names its own transport so the same
# upstream service can be configured under multiple wire formats. Useful
# when a vendor exposes both Anthropic-compatible and OpenAI-compatible
# endpoints (MiniMax, OpenRouter, GLM, …).
[provider.custom.minimax-openai]
transport   = "openai_chat"            # API-key transports: anthropic_messages | openai_chat | openai_responses | gemini_native
api_key_env = "MINIMAX_API_KEY"
base_url    = "https://api.minimaxi.com/v1"
model       = "MiniMax-M2.7"
context_window = 192000
# supports_vision = true               # optional vendor-confirmed override;
                                       # unset = auto/unknown models are tried,
                                       # false = force image attachments off

# Cloud-auth custom transports are also available: azure_openai,
# vertex_anthropic, and bedrock_anthropic. Their required profile fields
# differ; use `metis config schema` and command help as the source of truth.

[provider.custom.deepseek]
transport   = "openai_chat"
api_key_env = "DEEPSEEK_API_KEY"        # 1st preference: env var
# api_key   = "sk-..."                  # 3rd preference: inline (lowest, after the credential store)
base_url    = "https://api.deepseek.com/v1"
model       = "deepseek-chat"
context_window = 1000000

# GLM Coding Plan Responses API (validated with GLM 5.3). This endpoint is
# Responses-only; use /api/coding/paas/v4 with openai_chat for Chat Completions.
[provider.custom.bigmodel-responses]
transport = "openai_responses"
api_key_env = "BIGMODEL_API_KEY"
base_url = "https://open.bigmodel.cn/api/v1"
model = "glm-5.3"
responses_state_mode = "auto"
hosted_tools = ["web_search"]

# Auth chain for both built-in (anthropic/openai/gemini) and custom
# providers — first non-empty wins:
#   1. env var named in api_key_env
#   2. ~/.metis/.credentials/auth.json entry (`metis login <name>`)
#   3. inline api_key field in this block

# Switch profiles at run time:
#   metis -p minimax-openai chat
#   metis -p deepseek run "..."

# Don't know the right base_url / model name / env var? Run:
#   metis models                      # browse providers from models.dev
#   metis models deepseek             # all DeepSeek models + cost + context
#   metis models deepseek deepseek-chat  # ready-to-paste config snippet

[permission]
mode = "default"
[[permission.allow]]
tool = "Read"
[[permission.allow]]
tool = "Bash"
# Match grammar (claude-code parity):
#   "git status:*"  → command prefix; never matches a chained command
#                     ("git status; rm -rf /" falls through to ASK)
#   "/etc/**"       → path glob (** crosses directories, * stays in one)
#   "git status"    → legacy substring (matches anywhere — prefer :* )
match = "git status:*"

# Layers: ~/.metis/config.toml < .metis/config.toml < .metis/config.local.toml
# (gitignored, per-checkout). /etc/metis/policy.toml (or METIS_POLICY_FILE)
# is the managed tier — its rules outrank config, CLI flags and the TUI's
# "always allow", so a policy deny can't be overridden.

[ui]
theme = "auto"
markdown = true
show_tokens = true
thinking_display = "auto"   # show=full provider reasoning; auto=compact; hide=off

[ui.performance]
tick_ms = 40                  # 25fps default; 16=60fps, 100=10fps
event_buffer_size = 256       # agent.Event channel depth
mouse_wheel_lines = 1         # 1=pixel-precise, 3=jumpy
reduced_motion = false        # accessibility: 500ms tick + no shimmer

[session]
auto_compact_threshold = 0.85       # fraction of effective input budget
auto_compact_minimum_tokens = 50000 # do not compact below this estimate
max_iterations = 100

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
#
# MCP server process startup is ALSO lazy by default (P7,
# kimi-cli `defer_mcp_tool_loading` parity). Cached schemas live in
# ~/.metis/mcp-cache/<server>.json so the subprocess only spawns when
# the model invokes a tool. Controlled by METIS_LAZY_MCP env var:
#
#   (unset) / auto → use cache when fingerprint matches; spawn-and-cache on miss
#   always         → never spawn at startup, even without cache (most aggressive)
#   never          → eager spawn for every entry (legacy behavior)
#
# The cache fingerprint covers (command, args, url, headers) so any
# edit to mcp.toml that changes the launch identity auto-invalidates
# the cache. Run `rm -rf ~/.metis/mcp-cache` to flush everything.

[tools.bash]
timeout_seconds = 120
max_output_bytes = 32768
# Re-include these baseline patterns if you replace the denylist.
denylist = ["rm -rf /", "dd of=/dev", ":(){:|:&};:"]

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
global                = 0   # disabled; set a positive total-call ceiling to opt in
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

Direct shell invocations of `kill`, `pkill`, `killall`, `taskkill`, and
`Stop-Process` are blocked in model-controlled Bash, Workflow, and Monitor
execution, including common wrapped or nested forms. Commands the guard cannot
parse are rejected rather than executed. Use `BashKill(job_id)` so process
termination stays scoped to jobs Metis registered for the current process;
the OS sandbox remains the boundary for arbitrary interpreter or binary code.

Output is captured to `~/.metis/jobs/<id>.out` (mode 0600). The TUI
status bar shows `⚙ N jobs` while jobs are alive.

Sleep blacklist: `sleep N` (N ≥ 2s) standalone or `sleep N && rest`
are rejected — they're polling primitives that shouldn't burn the
foreground turn. Sub-2s pacing, pipeline / subshell / loop sleeps
are fine.

### Desktop notifications

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

Metis combines a static base registry with tools added by the selected
runtime: file/search/edit/shell operations, jobs, memory/history,
workflows, plan mode, cron, skills, sub-agents, MCP resources, and channel
messaging. Availability depends on mode, config, platform, and runtime
dependencies, so a hand-maintained table is not authoritative.

```sh
metis tools       # built-in/static contract after visibility filters
metis schema      # current machine-readable tool schemas
```

Each call is classified from its input as `Safe`, `Queue`, `Exclusive`,
or `Background`. Bash first applies hard safety rules, then the permission
gate; a permissive user mode cannot bypass hard destructive-command blocks.

## Scheduling

Two ways to schedule, plus a daemon to fire unattended jobs. Details in
[`docs/ARCHITECTURE.md#scheduling--continuous-execution`](docs/ARCHITECTURE.md#scheduling--continuous-execution).

### Conversational (claude-code style)

Just ask in chat — the model calls `CronCreate`:

> "every 5 minutes, check the build and tell me if it breaks"
> "remind me in 20 minutes to push the branch"

By default these are **session-only**: they fire *in the current chat while
it's open and idle* (the in-session scheduler injects the prompt as a new
turn), and disappear when you exit. You see each fire inline and approve any
tool use live — no daemon needed. Ask for it to "persist" / "keep running
across restarts" and the model sets `durable: true`, which writes it to disk
to be fired unattended by `metis cron start` (and pre-authorizes the tools it
needs, since no one's watching). `CronList` / `CronDelete` manage them.

### CLI

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

# silent — fire without printing to chat, transcript lands in audit log
# (hermes SILENT_MARKER pattern: "I want to know it ran, not be spammed")
metis cron add --every 10m --silent --prompt "ping internal /healthz, log failures"
metis cron audit <id>          # list silent fires for a job (newest first)
metis cron audit <id> latest   # print the most recent transcript

metis cron list
metis cron pause <id>
metis cron resume <id>
metis cron rm <id>
metis cron run <id>            # fire immediately, ignore schedule
metis cron start               # foreground daemon (Ctrl+C to stop)
```

**Unattended permissions (pre-authorization).** A cron daemon has no human
to answer a mid-fire permission prompt, so — mirroring claude-code's
background-agent model — the decision is made entirely from a per-job
allow-list set ahead of time. A tool call that isn't pre-authorized is
denied and recorded; **dangerous-pattern commands (`rm -rf /`, fork bombs)
stay blocked even if allow-listed.** Without any `--allow`, a job can only
run read-only tools (writes/exec/network are blocked) — so a job that needs
to write files or run commands *must* be granted them, or it silently does
nothing.

```sh
# pre-authorize specific tools (repeatable, `Tool(content)` form)
metis cron add --every 1h --silent \
  --prompt "git pull and summarize new commits" \
  --allow 'Bash(git pull:*)' --allow 'Bash(git log:*)' --allow Write

metis cron denied <id>         # what a fire tried to do but wasn't allowed,
                               # with a ready-to-paste approval line per call
metis cron allow <id> 'Bash(git pull:*)'   # authorize it for next time
```

`SessionMode` choices:

- `isolated` (default) — every fire starts with empty history
- `persistent` — per-job rolling thread (history accumulates across firings)
- `main` — append to a shared named session (`--session main` default)

Inside chat the LLM has the `ScheduleWakeup` tool — it can decide on its
own when to re-enter (e.g. "check if the build is done in 5 min"). The
wakeup persists as a one-shot cron job, so `metis cron list` shows what
the agent has scheduled itself. The status bar shows a `↻ wake 18m`
chip when a wakeup is pending; a `◐ cron silent N/24h` badge counts
silent-cron fires in the last 24h so a stuck health-check is visible
at a glance.

## Multi-agent (Phase G — claude-code parity)

Metis runs multiple sub-agents concurrently with peer messaging,
named teammates, per-task isolation, and resumable state. The headline
surface:

```sh
# Spawn a focused, isolated sub-agent
Agent({prompt: "summarize internal/agent/loop.go", name: "alice"})

# Background sub-agent — parent keeps working while it runs
Agent({prompt: "...", run_in_background: true})

# Per-invocation worktree isolation
Agent({prompt: "...", isolation: "worktree"})

# Per-invocation permission mode override (parent stays in default)
Agent({prompt: "...", permission_mode: "bypassPermissions"})

# Per-invocation tool narrowing (intersection with profile filter)
Agent({prompt: "...", allowed_tools: ["Read", "Grep"]})

# Resume a paused sub-agent from its on-disk transcript
Agent({prompt: "continue", resume_from: "agt-d3a91b07"})

# Team-lead mode: the main loop becomes an orchestrator
METIS_COORDINATOR_MODE=1 metis chat
# or: metis --coordinator chat
```

Eight bundled agent profiles ship via `//go:embed`: `explore`, `plan`,
`verify`, `general`, `go-reviewer`, `mcp-debugger`, `coordinator`, and
`teammate`. User overrides at `~/.metis/agents/<name>.md` always win.
Use `/help` in chat for the live agent-management slash commands.

Cross-cutting:

- Separate concurrency caps for named teammates and anonymous workers
  (`agents.max_concurrent_named = 20` and
  `agents.max_concurrent_anon = 40` by default); the old combined cap
  remains only for backward-compatible configuration.
- Timeout budget (`config.Agents.DefaultTimeoutSeconds`, default 600s)
  with per-invocation override via `timeout_seconds`
- Sub-agent transcripts persisted to
  `<session-dir>/subagents/<agent_id>.jsonl` for `resume_from`
- Memory uses `./.metis/memory/` only when that directory exists in the
  process's exact current working directory; otherwise it uses
  `<session-dir>/memory`. Memory lookup does not walk parent directories.
- Per-agent `permission.Gate.Clone()` so a child's mode flip can't
  leak back to the parent
- DreamTask phase model (idle/starting/extracting/writing/done)
  with `<memory_consolidation_done>` notifications back to the LLM
- Panic recovery + ctx-aware drain on sub-agent abort so a buggy
  child can't pin the parent turn

## Subdirectory hints + SKILL.md inline shell (claude-code parity)

Two narrow-but-high-leverage features from claude-code's prompt layer:

- **Subdirectory hints (down-walk attachment)** — when a user message
  `@`-mentions a path below cwd, metis collects per-directory
  `CLAUDE.md` / `AGENTS.md` / `METIS.md` along the descent and
  prepends them as a `<subdirectory_hints>` block on the LLM-facing
  user message (the transcript stays clean). Pairs with the existing
  up-walk in `loadProjectContext` so a monorepo's nested
  service-level conventions surface even when cwd sits at the repo
  root. **Attachment path, not system-prompt mutation** — keeps the
  Anthropic prompt cache warm. Implementation:
  `internal/runtime/subdir_hints.go`; wired in TUI submit + `metis run`.

- **SKILL.md template vars + inline shell** — when the Skill tool
  invokes a skill, the body is expanded with
  `${METIS_SKILL_DIR}` / `${METIS_SESSION_ID}` (and their
  `${CLAUDE_*}` aliases for paste-compat with claude-code), then any
  `` !`cmd` `` (single-line) or ```` ```! ```` (fenced multi-line)
  blocks are executed and replaced with their stdout. Bounded per
  invocation: **10s timeout, 8 KiB stdout cap, `[shell error: …]`
  sentinel on failure**. Trust gate: only skills at
  `builtin` / `trusted` / `user` / `project` trust can run inline
  shell — `community` skills get the raw text (claude-
  code parity, prevents third-party manifests from smuggling shell
  at invoke time). Implementation:
  `internal/agent/skills/{expand,inline_shell}.go`.

## Eval (deterministic LLM scoring)

`metis eval` runs a markdown scenario pack against a metis binary and
scores each scenario's output via deterministic assertions. Inspired
by Atropos / Terminal-Bench-2 / openclaw qa-lab — same shape: a folder
of `.md` files, one scenario each, with a YAML header + `# Prompt` +
`# Reward` sections.

```sh
metis eval                                # run every scenario under eval/scenarios
metis eval --tag smoke                    # filter by header tag
metis eval --dir ./my_scenarios           # custom scenario directory
metis eval --out results.jsonl            # write per-scenario JSONL + summary
metis eval --binary ./build/metis         # test another binary
metis eval --provider deepseek            # override [provider.default]
metis eval --verbose                      # show every assertion's note
```

A scenario file:

```markdown
---
id: count_files
description: Count .go files under ./internal
tags: [smoke]
timeout_seconds: 30
---

# Prompt
Count the .go files in this project. Reply with just the integer.

# Reward
contains_all: ["internal"]              weight=1.0
used_tool: Grep                         weight=0.5
regex: \d+                              weight=0.3
max_input_tokens: 8000                  weight=0.5
max_output_tokens: 200                  weight=0.3
```

Assertion types supported by `# Reward`:

| Keyword | Pass when |
|---|---|
| `contains_all: [a, b]` | every token is in the response |
| `contains_any: [a, b]` | at least one token is in the response |
| `not_contains: [a]` | none of the tokens appear |
| `used_tool: Name` | the tool got called at least once |
| `regex: PATTERN` | the response matches a Go regexp |
| `length: 10..200` | response length in `[min, max]` chars |
| `max_input_tokens: N` | `tokens.in + cache_read + cache_create ≤ N` |
| `max_output_tokens: N` | `tokens.out ≤ N` |

Token-budget assertions read the `[metrics] tokens.in=… tokens.out=…
tokens.cache_read=… tokens.cache_create=…` line metis prints on
LoopDone — useful for catching prompt-engineering regressions where a
refactor accidentally bloats per-call spend. When the metrics line is
missing (older binary), the assertion passes neutrally so the suite
doesn't fail on noise.

The JSONL output (`--out`) emits one `{type: "score", id, total, passed,
tokens_in, tokens_out, …}` line per scenario plus a final
`{type: "summary", pass_rate, avg_score, …}` — pipe through `jq` for
custom rollups.

## Channels (chat-platform adapters)

The runtime wires six configurable `SendMessage` adapters: **Slack,
Telegram, Discord, DingTalk, Feishu, and WeChat**. An adapter activates
only when the credential environment variable named in its
`[channels.<name>]` config is present. Use cases include ops automation
and scheduled reports.

## Plugins

Drop a directory under `~/.metis/plugins/<name>/` with a `plugin.toml`:

```toml
manifest_version = 1
name = "browser-mcp"
version = "0.3.1"
description = "Browser automation"
skills = ["skills/screenshot.md"]

[mcp_server]
command = "node"
args = ["index.js"]
```

```sh
metis plugin marketplace list
metis plugin marketplace add <name> github:<owner>/<repo>
metis plugin search <query>
metis plugin install <plugin>[@<marketplace>]
metis plugin list
metis plugin info <name>
metis plugin remove <name> --yes
```

Plugin MCP tools register as `mcp__plugin:<name>__<tool>`; plugin skills are
namespaced as `<plugin>:<skill>` so they don't collide with bundled or
user skills. Both surface to the LLM through the regular tool / skill
discovery paths — no extra wiring needed.

METIS exposes ecosystem adapters separately from catalog sources. It understands
Claude marketplace manifests, Codex `.codex-plugin/plugin.json` bundles, and the
real npm dependencies mounted by local DeepSeek Harness profiles. Codex skills
are imported with namespace isolation and Codex MCP declarations are translated
into METIS multi-server MCP configuration. Codex apps/hooks and DeepSeek Harness
Cordis services, slots, events, HMR, and fibers remain in their original
runtimes; portable `SKILL.md` files inside those bundles can still be imported.
The Desktop labels every component as native, translated, portable-only, or
original-runtime instead of presenting Codex or DeepSeek Harness themselves as
installable plugins or marketplaces.

In-repository paths install directly; pinned HTTPS `url` / `git-subdir` entries
from GitHub, GitLab, or Codeberg are fetched into a temporary checkout and
inspected before installation. The installer rejects path escapes, symlinks,
unsafe Git refs, oversized bundles, and entries that contain no compatible
component.

## Local Artifacts

METIS can create durable, session-owned HTML deliverables through the
model-facing `Artifact` tool. Each update appends an immutable version under
`$METIS_HOME/artifacts`; Desktop shows a structured card, a per-session gallery,
version switching, download, ZIP export, and a confirmed delete action. Deleting
the owning session also deletes every attached artifact version.

The current local implementation is deliberately static: executable elements,
external URLs, forms, frames, event handlers, and network-loading CSS are
removed. Desktop previews the result in an empty-sandbox iframe on a separate,
short-lived loopback capability origin, so artifact content never shares the
privileged WebUI origin.

Use `/artifact <request>` or ordinary language in chat. CLI management requires
an explicit owning session ID:

```sh
metis artifacts list --session <id>
metis artifacts show <artifact-id> --session <id>
metis artifacts create page.html --session <id> --title "Release dashboard"
metis artifacts update <artifact-id> page.html --session <id>
metis artifacts export <artifact-id> --session <id> --out /absolute/path/page.html
metis artifacts delete <artifact-id> --session <id> --yes
```

This local feature does not manufacture a public URL. Public hosting, audience
controls, authentication, and shared persistence require a separately operated
publishing service.

## Computer use (driving the desktop)

Companion binary [`metis-cu`](https://github.com/Ricardo-M-L/metis-cu)
exposes desktop-control tools (screenshot / mouse / keyboard /
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
cu: enabled — computer-use; binary=/Users/.../go/bin/metis-cu
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

See [`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md).

```
cmd/metis/                       CLI composition and subcommands
pkg/                             public SDK contracts
internal/llm/                    provider implementations and streaming
internal/llm/transport/          shared HTTP, retry, dump, and logging
internal/tools/                  tool registry and visibility helpers
internal/tools/builtin/          first-party tools
internal/tools/builtin/bash/     Bash jobs, classification, and safety rules
internal/permission/             rule gate and public permission modes
internal/agent/                  loop, dispatch, compaction, hooks, and cron
internal/agent/skills/           SKILL.md loader: bundled, optional,
                                 universal, user, project, and plugin sources
internal/agent/transcript/       in-memory transcript helpers
internal/mcp/                    stdio and Streamable HTTP/SSE clients
internal/channels/               wired chat-platform adapters
internal/memory/                 Core, Archival, Recall, and daily notes
internal/session/                JSONL persistence, branching, and snapshots
internal/runtime/                top-level runtime composition
internal/runtime/mcp/            MCP registry, cache, and prompt collection
internal/slash/                  slash-command registry and handlers
internal/tui/                    Bubble Tea TUI
internal/tui/screen/             full-screen overlays
internal/exitcode/               typed errors mapped to shell exit codes
internal/jobs/                   background process pool
install/                         shell, PowerShell, and npm installers
```

Several internal packages carry a `README.md` documenting their
file-naming convention and "where to find X" pointers — see
`internal/tui/`, `internal/agent/`, `internal/tools/builtin/`,
`internal/runtime/`, `internal/llm/transport/`, `internal/slash/`,
and `internal/agent/skills/`.

## License

MIT — see LICENSE.
