# Environment variables

Metis reads the variables below at process startup or at the point where the
related feature is used. Unless noted otherwise, an unset or malformed value
falls back to the configured or built-in default. This page documents the
supported operational surface first; implementation and test escape hatches
are kept in a separate section at the end.

## Paths and desktop launch

| Variable | Default | Description |
|---|---|---|
| `METIS_HOME` | `~/.metis` | Root for config, sessions, memory, caches, skills, and user commands. Useful for CI or isolated profiles. |
| `METIS_PORT` | `8080` | Port for `metis desktop --web`. An explicit `--port` / `-p` wins. Valid range: `1`-`65535`. |
| `METIS_DESKTOP_APP` | auto-discovered | Override the native desktop application path used by `metis desktop`. |
| `METIS_BIN` | auto-discovered | Override the Metis CLI executable used by the desktop client. |

## Debugging and prompt inspection

| Variable | Default | Description |
|---|---|---|
| `METIS_DEBUG` | off | Set to `1` for verbose diagnostics. LLM transport traces are appended to `METIS_DEBUG_LOG`; some runtime diagnostics also go to stderr. |
| `METIS_DEBUG_LOG` | `$METIS_HOME/debug.log` | Override the LLM transport debug-log path. |
| `METIS_DEBUG_GEMINI` | off | Set to `1` for Gemini-provider-specific traces. Read at process startup. |
| `METIS_DEBUG_OPENAI` | off | Set to `1` for OpenAI-provider-specific traces. Read at process startup. |
| `METIS_DUMP_PROMPTS` | off | Set to a truthy value (normally `1`) to write assembled prompts under `$METIS_HOME/dump-prompts/`. `DUMP_PROMPTS` is also accepted. |
| `METIS_PASTE_DEBUG` | off | Set to `1` to write clipboard-paste diagnostics to `$METIS_HOME/paste-debug.log`. |

## Memory and dream extraction

| Variable | Default | Description |
|---|---|---|
| `METIS_AUTO_MEMORY` | off | Set to `1` to run memory extraction at turn boundaries. Equivalent to `--auto-memory`. |
| `METIS_AUTO_MEMORY_DEBUG` | off | Set to `1` to log extractor decisions and failures. Has practical value only with auto-memory enabled. |
| `METIS_AUTO_RETRIEVE` | off | Positive integer top-K for archival memory retrieval on every turn. Values above `50` are clamped to `50`; non-positive or non-numeric values leave retrieval off. |
| `METIS_AUTO_RETRIEVE_RERANK` | off | Set to `1` or `true` to rerank the retrieved candidates with the active model. Adds one model call per retrieval. |
| `METIS_DREAM_INTERVAL_HOURS` | `12` | Minimum hours between dream passes. Fractional values are accepted; `0` or a negative value disables dreaming. |
| `METIS_DREAM_MIN_SESSIONS` | `3` | Minimum distinct sessions touched since the previous dream. Values below `1` are clamped to `1`. |

## TUI, locale, and notifications

| Variable | Default | Description |
|---|---|---|
| `METIS_THEME` | terminal auto-detection | Theme name: `dark`, `light`, or `dark-daltonized`. An unknown value is ignored. |
| `METIS_LANG` | `$LANG`, then `en` | UI locale override. The shipped locales are `en` and `zh-CN`. |
| `METIS_REDUCED_MOTION` | off | Set to `1` to reduce animations and use a `500ms` TUI tick. `NO_MOTION=1` is also accepted. |
| `METIS_TICK_MS` | `40` | TUI tick interval in milliseconds, valid from `1` to `1000`. Reduced-motion mode takes precedence. |
| `METIS_DISABLE_MOUSE` | off | Set to a truthy value (any non-empty value except `0`, `false`, `no`, `off`, or `n`) to use `MouseModeNone` instead of the default cell-motion capture. Metis then receives no wheel, click, input/transcript drag-selection, or link-click events; use `PgUp`/`PgDn`, `Home`/`End`, `Ctrl+Y`/`Ctrl+Shift+Y`, and `Ctrl+S` for navigation and copying. |
| `METIS_MOUSE_WHEEL_LINES` | `1` | Transcript lines scrolled per wheel event, valid from `1` to `50`. |
| `METIS_EVENT_BUFFER` | `256` | TUI event-channel capacity, valid from `16` to `16384`. |
| `METIS_NOTIFY_CHANNEL` | `auto` | Terminal notification protocol. Canonical values: `auto`, `iterm2`, `iterm2_with_bell`, `kitty`, `ghostty`, `bell`, or `off`. This is not a Slack or email route. |

## Models and providers

| Variable | Default | Description |
|---|---|---|
| `METIS_MODELS_URL` | `https://models.dev/api.json` | Model-catalog JSON endpoint used by catalog clients, including `metis models`. |
| `METIS_SIMPLE` | off | Set to `1` for the short system prompt and short tool descriptions. Equivalent to `--simple`. |
| `METIS_OPENAI_MAX_CONCURRENCY` | `4` | Per-provider cap for simultaneous OpenAI-compatible requests, shared by parent and sub-agents. `0` or a negative value disables the gate. |
| `METIS_GEMINI_THINKING_BUDGET` | `0` | Gemini thinking-token budget: `0` disables thinking, `-1` lets the model decide, and a positive integer sets a cap. |

## Sessions and context management

| Variable | Default | Description |
|---|---|---|
| `METIS_RESUME_MAX_MB` | `8` | Maximum session JSONL size, in MiB, accepted for resume or branch. `0` or a negative value disables the size check. |
| `METIS_MICROCOMPACT` | on | Set to `0` to disable lossless offloading of large historical tool results into the session microcompact cache. |
| `METIS_SPILL` | on | Set to `0` to disable ingestion-time spill of oversized tool results. This switch is independent of `METIS_MICROCOMPACT`. |
| `METIS_COMPACT_RESERVE_FULL_MAX_TOKENS` | off | Set to `1` to reserve the provider's full configured `max_tokens` during compaction instead of capping the reply reserve at 20K. |

## Agent coordination

| Variable | Default | Description |
|---|---|---|
| `METIS_COORDINATOR_MODE` | off | Set to `1` or `true` to enable team-lead mode. Equivalent to `--coordinator`. |
| `METIS_COORDINATOR_EXTRA_TOOLS` | empty | Comma-separated tool names added back to the coordinator-mode allowlist. |
| `METIS_MAX_SUBAGENTS` | unset | Combined named + anonymous concurrency cap. It is split approximately `1:2`; per-kind variables below take precedence. `0` or a negative value means unlimited. |
| `METIS_MAX_SUBAGENTS_NAMED` | `20` | Named-teammate concurrency cap. `0` or a negative value means unlimited. |
| `METIS_MAX_SUBAGENTS_ANON` | `40` | Anonymous sub-agent concurrency cap. `0` or a negative value means unlimited. |

## MCP and tool discovery

| Variable | Default | Description |
|---|---|---|
| `METIS_LAZY_MCP` | `auto` | MCP startup policy. Unset / `auto` uses a valid cache and starts a server on cache miss; `always` (also `true`, `1`, `yes`) requires lazy startup; `never` (also `false`, `0`, `no`, `off`) starts eagerly. Unknown values fall back to `auto`. |
| `ENABLE_TOOL_SEARCH` | always defer MCP schemas | Controls deferred MCP tool-schema discovery. Unset / `true` always defers, `false` sends full schemas, `auto` uses a 10% context threshold, and `auto:N` uses an `N` percent threshold (`1`-`99`). |
| `MCP_CONNECT_TIMEOUT` | `30s` | MCP startup, handshake, and initial-list timeout. Accepts Go duration syntax such as `45s` or `2m`. |
| `MCP_REQUEST_TIMEOUT` | `60s` | Timeout for non-tool MCP RPCs. Accepts Go duration syntax. |
| `MCP_TOOL_TIMEOUT` | `100000s` | Timeout for MCP `tools/call` (about 27.8 hours). Accepts Go duration syntax. |

## Runtime limits, cache, and telemetry

| Variable | Default | Description |
|---|---|---|
| `METIS_RUN_CACHE` | off | Set exactly to `1` to enable the on-disk response cache for `metis run`. Equivalent to `--cache`; tool-use turns are not cached. |
| `METIS_RUN_MAX_SECONDS` | `1800` | Whole-invocation wall-clock cap for `metis run` and Metis MCP tool calls. Only positive integers override the default. |
| `METIS_TURN_MAX_SECONDS` | `2700` | Per-agent-turn wall-clock cap. It is checked between loop iterations; an in-flight operation is allowed to return first. |
| `METIS_HTTP_TIMEOUT_SECS` | `1200` | Whole-request timeout for model HTTP clients. Only positive integers override the default. |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | unset | Enables OTLP/HTTP JSON metrics export. A base endpoint gets `/v1/metrics` appended automatically. |

## Updates

| Variable | Default | Description |
|---|---|---|
| `METIS_NO_UPDATE_CHECK` | off | Set to `1` to disable the interactive TTY background update loop, which checks at startup and about every 30 minutes; a successful install takes effect on the next invocation. Explicit `metis update` commands remain enabled. |
| `METIS_REPO` | `Ricardo-M-L/metis` | Override the GitHub `owner/repo` used for release checks and updates. |
| `METIS_GITHUB_TOKEN` | unset | Preferred GitHub token for update requests. Resolution then falls back to `GITHUB_TOKEN` and finally `gh auth token`; public releases work anonymously. |

## Safety and managed policy

| Variable | Default | Description |
|---|---|---|
| `METIS_POLICY_FILE` | `/etc/metis/policy.toml` | Override the machine-managed permission-policy file. Policy denies have higher authority than user or project config. |
| `METIS_NO_TRUST_PROMPT` | off | Set to `1` to skip the first-use current-directory trust prompt. Use only in controlled CI or on directories you already trust. |
| `METIS_DISABLE_INJECTION_SCAN` | off | Set to `1` to bypass the prompt-injection scanner. Use only when deliberately accepting that risk. |

## Credentials

Built-in providers and tools read these conventional variables:

| Variable | Used by |
|---|---|
| `ANTHROPIC_API_KEY` | Anthropic |
| `OPENAI_API_KEY` | OpenAI and OpenAI-compatible configuration when selected as its key environment variable |
| `GEMINI_API_KEY` | Gemini (preferred) |
| `GOOGLE_API_KEY` | Gemini fallback |
| `AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY`, `AWS_SESSION_TOKEN`, `AWS_REGION` | Amazon Bedrock; the session token is optional and the region falls back to `us-east-1` |
| `TAVILY_API_KEY`, `BRAVE_SEARCH_API_KEY`, `SERPER_API_KEY` | Web search, tried in that order |

Custom provider blocks may name a different credential variable through their
`api_key_env` (and, where applicable, `secret_key_env`) setting. Prefer the
auth store or provider configuration for long-lived credentials; environment
variables are convenient for CI and one-off runs.

## Standard runtime variables

Metis also respects standard shell, terminal, locale, and toolchain variables,
including `HOME`, `PATH`, `EDITOR`, `VISUAL`, `SHELL`, `TERM`, `TERM_PROGRAM`,
`TERM_PROGRAM_VERSION`, `TMUX`, `XDG_CONFIG_HOME`, `XDG_RUNTIME_DIR`, `LANG`,
`LC_TERMINAL`, `NO_COLOR`, `CI`, `SSH_CONNECTION`, `KITTY_WINDOW_ID`,
`ALACRITTY_LOG`, `WT_SESSION`, `STY`, `GOPATH`, and `GOBIN`.

## Internal debugging and test escape hatches

These are implementation aids, not a stable user-facing configuration API:

| Variable | Default | Description |
|---|---|---|
| `METIS_CATALOG_DISABLE` | off | Set to `1` to make the process-wide background catalog singleton unavailable. This does **not** disable the explicit fetch performed by `metis models`. |
| `METIS_CONTRACT_DISABLE` | off | Set to `1` to disable the verification contract. Intended for focused tests and runtime debugging. |
| `METIS_DEBUG_IMG_PRUNE` | off | Any non-empty value emits per-iteration image-pruning diagnostics. |
