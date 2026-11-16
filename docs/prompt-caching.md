# Prompt caching

Prompt caching is an upstream reuse of an unchanged request prefix, not a local
cache of final answers. A cache marker or routing key does not prove a cache hit;
use provider-reported cache-read and cache-write token usage to verify one.

## Warm Fork

The built-in `Fork` tool uses the parent request that actually completed, rather
than reconstructing a system prompt from only `Loop.System`. Its first request
retains the parent's system sections, model, reasoning effort, upstream cache
affinity, and the definitions and order of tools available to the child. Root
permission and plan controls stay excluded by the child registry, so the entire
tools array need not match the parent's byte for byte. The focused task is
appended after inherited history and correctly paired tool results.

The snapshot is detached from mutable history and schemas. After the first child
response, ordinary tool discovery and registry updates remain live. Changing
history, provider, plan, or permission invalidates the first-request pin; starting a different chat
session also clears inherited affinity. A shared routing key never guarantees
that the upstream has retained the corresponding prefix.

Cold `Agent` jobs intentionally start independently. Background memory work may
filter history and expose a restricted tool set for safety; it does not promise
byte-identical reuse of the parent's complete request.

## Ordered runtime context

Local Responses replay on native OpenAI/Codex routes, or an explicitly selected
`responses_profile = "openai"`, persists host context updates in canonical
history. An unchanged snapshot adds nothing. Changed and removed named sections
are appended once, before the response that consumes them; resume keeps their
original positions. Compaction discards the old context ledger and establishes a
full current baseline in the new history window. CLI, TUI, REPL and WebUI session
owners synchronously checkpoint before sending. A checkpoint failure stops new
model work; if storage fails after a request starts, its completed answer and
cancellation evidence remain in memory for recovery. Preflight pressure uses the
same pure projection and counts an existing baseline only once.

Only confirmed developer-context capability enables this projection.
Provider-managed Responses state and other/unknown transports keep their
existing system projection; they do not receive new or guessed wire roles.
Memory and retrieved passages are reference data, not permission grants, and
execution-time authorization is unchanged. Ambiguous or oversized snapshots
fall back without truncating their content.

## Provider boundaries

Anthropic Messages uses explicit cache breakpoints. OpenAI-compatible Chat and
Responses routes use upstream automatic caching, with optional cache routing
fields on supported endpoints. Do not send unsupported protocol extensions to a
compatible gateway solely because its model name resembles a first-party model.

Mutable system state, tool discovery, compaction, model changes and cache expiry
can reduce reuse. `Volatile` means a section does not receive an explicit marker;
it does not make changes in the middle of a prefix invisible to the server.
`metis run --cache-ttl` controls local answer caching, not provider prompt-cache
retention. Responses continuation and connection reuse are also separate from
prompt-cache hits.

## Tool discovery and observations

`ENABLE_TOOL_SEARCH` defaults to `auto` with a 10% Deferred-schema budget.
Small tool sets are sent completely from the first request; large sets retain
the existing `ToolSearch select:` handshake. Explicit `true`, `false`, `auto`
and `auto:N` settings keep their precedence. An unknown context window defaults
to complete schemas unless the user explicitly requests Always. This is a
client-side compatibility strategy, not a claim of native deferred-tool support.

`MetisInfo({"section":"cache"})` reads detached observations from the invoking
loop. It reports token buckets, the request fingerprint and whether it changed.
A fingerprint change is not labeled a proven cache miss. If neither cache-read
nor cache-write tokens are reported, the ratio is `unknown` rather than an
invented zero-percent hit rate. No system prompt, tool body or credential is
included in this diagnostic section.

## Exercise the actual CLI and TUI locally

Build the source without changing version metadata:

```sh
make build
python3 scripts/e2e/cache_prefix_smoke.py \
  --binary ./bin/metis --out "$HOME/.cache/metis-tests/cache-cli"
python3 scripts/e2e/cache_prefix_smoke.py \
  --binary ./bin/metis --out "$HOME/.cache/metis-tests/cache-tui" --tmux
python3 -B scripts/e2e/cache_prefix_smoke.py \
  --binary ./bin/metis --out "$HOME/.cache/metis-tests/cache-responses-fork" \
  --transport openai_responses
python3 -B scripts/e2e/context_replay_smoke.py \
  --binary ./bin/metis --out "$HOME/.cache/metis-tests/context-resume"
python3 -B scripts/e2e/context_replay_smoke.py \
  --binary ./bin/metis --out "$HOME/.cache/metis-tests/context-tui" --tmux
python3 -B scripts/e2e/tool_discovery_smoke.py \
  --binary ./bin/metis --out "$HOME/.cache/metis-tests/tools-default"
python3 -B scripts/e2e/tool_discovery_smoke.py \
  --binary ./bin/metis --out "$HOME/.cache/metis-tests/tools-always" --mode always
```

The tool-discovery driver also launches an actual local stdio MCP echo server;
add `--tmux` to exercise its interaction in the TUI.

The driver creates an isolated home/workspace and a local scripted SSE endpoint.
It makes the real Metis executable call `Fork`, checks the resulting HTTP request
prefix and final answer, and optionally drives the terminal in an owned tmux
session. The TUI run approves only the newly created empty fixture workspace's
trust prompt. Synthetic request captures are written under `--out`; no real
credentials or existing conversation history are loaded. This validates client
behavior, not real-model latency or server cache hit rate.

## Optional live gateway smoke

This opt-in check calls the selected local Anthropic-compatible gateway and can
consume model credits. It resolves the existing endpoint-bound credential through
Metis's auth API, passes it only in a child environment, and uses an isolated home
with one synthetic prompt, restricted tools, output limits and a deadline. It
never reads existing conversations or writes a credential into the evidence.

```sh
go run ./scripts/e2e/live_smoke.go --binary ./bin/metis \
  --out "$HOME/.cache/metis-tests/live-new-directory"
```

The output directory must not already exist. This is a connectivity smoke, not
a cache benchmark or a comparison against an earlier binary.

A curl-managed release installation is protected from `make install`. Run
`./bin/metis`, or install to a separate unmanaged directory with
`make install LOCAL_INSTALL_DIR=/your/test/bin`, instead of replacing its launcher.
