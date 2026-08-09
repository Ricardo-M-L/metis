# Environment variables

metis honors the following environment variables. Most have a sensible
default; set them only when you need to override behavior. All names
start with `METIS_` to avoid collisions; the exceptions (`EDITOR`,
`SHELL`, `HOME`, etc) are standard POSIX variables metis reads to
respect existing user preferences.

## Core paths

| Variable | Default | Description |
|---|---|---|
| `METIS_HOME` | `~/.metis` | Override the root directory for sessions, memory, cache, config. Useful for sandbox / CI / per-project isolation. |

## Debug & tracing

| Variable | Default | Description |
|---|---|---|
| `METIS_DEBUG` | off | Set to `1` to enable verbose trace logging to `~/.metis/debug.log` (or the path in `METIS_DEBUG_LOG`). |
| `METIS_DEBUG_LOG` | `~/.metis/debug.log` | Override the debug log path. |
| `METIS_DEBUG_GEMINI` | off | Set to `1` for Gemini-provider-specific verbose traces. |
| `METIS_DEBUG_OPENAI` | off | Set to `1` for OpenAI-provider-specific verbose traces. |
| `METIS_DUMP_PROMPTS` | off | Set to `1` (or use `DUMP_PROMPTS=1`) to dump every assembled system prompt to `~/.metis/dump-prompts/`. Inspect to verify section ordering / cache boundaries. |
| `METIS_PASTE_DEBUG` | off | Set to `1` to log clipboard paste handler events (useful when @-mentions or pasted images misbehave). |

## Auto-memory & dream

| Variable | Default | Description |
|---|---|---|
| `METIS_AUTO_MEMORY` | off | Set to `1` to enable on-turn-end memory extraction (writes to `~/.metis/memory/<topic>.md`). Same as the `--auto-memory` CLI flag. |
| `METIS_AUTO_MEMORY_DEBUG` | off | Set to `1` to log auto-memory extractor decisions / failures verbatim. Pair with `METIS_AUTO_MEMORY=1`. |
| `METIS_AUTO_RETRIEVE` | off | Auto memory retrieval policy. `1` / `on` to enable; can also accept other tokens recognized by the retrieval policy parser. |
| `METIS_AUTO_RETRIEVE_RERANK` | off | Set to `1` / `true` to enable rerank for retrieved memories. |
| `METIS_DREAM_INTERVAL_HOURS` | (gate default) | Override the wall-clock interval between dream-extractor passes. Integer hours. |
| `METIS_DREAM_MIN_SESSIONS` | (gate default) | Minimum sessions since the last dream before another can fire. Integer count. |

## TUI / display

| Variable | Default | Description |
|---|---|---|
| `METIS_THEME` | (config / auto) | Force a specific theme (`dark`, `light`, etc). Overrides `[ui] theme` in config.toml. |
| `METIS_LANG` | (locale) | Override the UI language for translated strings. |
| `METIS_REDUCED_MOTION` | off | Set to `1` (or `NO_MOTION=1`) to disable progress animations. |
| `METIS_TICK_MS` | (perf default) | TUI refresh tick interval in milliseconds. Lower for snappier UI; higher to reduce CPU. |
| `METIS_MOUSE_WHEEL_LINES` | (perf default) | Lines scrolled per mouse-wheel tick. |
| `METIS_EVENT_BUFFER` | (perf default) | Event channel buffer size. |

## Models / providers

| Variable | Default | Description |
|---|---|---|
| `METIS_MODELS_URL` | (built-in catalog) | Custom URL for the model catalog JSON. Used by `metis models` to fetch the list of available models. |
| `METIS_CATALOG_DISABLE` | off | Set to `1` to skip the remote catalog fetch entirely (use cached / built-in list only). |
| `METIS_SIMPLE` | off | Set to `1` for a stripped-down system prompt (no advanced sections). Equivalent to `--simple`. |
| `METIS_OPENAI_MAX_CONCURRENCY` | `4` | Maximum simultaneous OpenAI-compatible requests per provider instance, shared by the parent and sub-agents. Set `0` to disable the gate. |

## Sessions / persistence

| Variable | Default | Description |
|---|---|---|
| `METIS_RESUME_MAX_MB` | (default cap) | Maximum size (MB) of a session JSONL file metis will attempt to resume. Larger files are refused to avoid load-time OOM. |
| `METIS_MICROCOMPACT` | on | Set to `0` to disable per-turn micro-compaction (the lightweight summarizer that runs between full compactions). |
| `METIS_COMPACT_RESERVE_FULL_MAX_TOKENS` | off | Set to `1` to reserve the full `max_tokens` headroom even when shrinking would be safe. Useful for providers that hard-fail on header math. |

## Sub-agent / loop budgets

| Variable | Default | Description |
|---|---|---|
| `METIS_LAZY_MCP` | off | Lazy-load MCP servers (`lazy` / `eager`). Lazy mode defers MCP startup until a tool is actually called. |
| `METIS_RUN_CACHE` | off | Set to `1` to enable on-disk response cache for `metis run`. Same as the `--run-cache` flag. Tool-use turns are never cached. |

## Network / updates

| Variable | Default | Description |
|---|---|---|
| `METIS_NO_UPDATE_CHECK` | off | Set to `1` to suppress the periodic "new version available" prompt. |
| `METIS_REPO` | `Ricardo-M-L/metis` | Override the GitHub repo metis checks for updates. |
| `METIS_GITHUB_TOKEN` | (none) | Personal access token for higher GitHub API rate limits during `metis update`. |

## Notifications

| Variable | Default | Description |
|---|---|---|
| `METIS_NOTIFY_CHANNEL` | (none) | Default channel route for tool-driven notifications (`slack:#chan`, `email:user@x`, etc). |

## Safety / trust

| Variable | Default | Description |
|---|---|---|
| `METIS_NO_TRUST_PROMPT` | off | Set to `1` to skip the cwd trust prompt on first launch in a new directory (use cautiously — only on machines you fully control). |
| `METIS_DISABLE_INJECTION_SCAN` | off | Set to `1` to disable the prompt-injection scanner. Don't disable unless you know exactly why. |

## Standard POSIX vars metis honors

`HOME` · `EDITOR` · `VISUAL` · `SHELL` · `TERM` · `TERM_PROGRAM` · `TERM_PROGRAM_VERSION` · `TMUX` · `XDG_CONFIG_HOME` · `LANG` · `LC_TERMINAL` · `NO_COLOR` · `CI` · `DEBUG` · `SSH_CONNECTION` · `KITTY_WINDOW_ID` · `ALACRITTY_LOG` · `WT_SESSION` · `STY` · `GOPATH` · `GOBIN`

## Provider credentials

These are read by the LLM providers when their built-in auth flow runs:

`ANTHROPIC_API_KEY` · `OPENAI_API_KEY` · `GOOGLE_API_KEY` · `GITHUB_TOKEN` · `AWS_REGION` · `AWS_ACCESS_KEY_ID` · `AWS_SECRET_ACCESS_KEY` · `AWS_SESSION_TOKEN`

Prefer `[provider.*]` blocks in `~/.metis/config.toml` for production; env vars are the fastest path for one-off testing.

## Internal — don't set unless you're debugging metis itself

`ENABLE_TOOL_SEARCH` · `C` (test fixtures) · `METIS_NO_UPDATE_CHECK` (CI)
