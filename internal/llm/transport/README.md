# internal/llm/transport

Shared construction and HTTP mechanisms used by LLM provider packages. This
package supplies the custom-transport constructor registry, instrumented HTTP
clients, explicit retry helpers, diagnostic dumps/logging, context-overflow
parsing, and model context-window suffix parsing. Provider wire protocols stay
in sibling packages such as `anthropic`, `openai`, and `gemini`.

## Files

| File | What it owns |
|---|---|
| `registry.go` | Mutex-protected transport-name → constructor registry and `MustBuild` error reporting |
| `httpclient.go` | HTTP transport wrapping, response-header timeout, whole-request hard cap, and session-id plumbing |
| `retry.go` | Explicit context-aware retry loop, retryable error types, network classification, jitter, and `Retry-After` parsing |
| `dump.go` | Opt-in redacted request/response JSONL dumps, including streaming-safe SSE capture |
| `log.go` | Request-id injection and opt-in method/path/status/latency debug lines |
| `overflow.go` | Parsing provider context-length errors and computing a one-shot adjusted `max_tokens` budget or actionable hint |
| `model_suffix.go` | Parsing terminal `-Nk` / `-Nm` context-window hints from model IDs |

## Registry scope

The registry is not the universal provider factory. In
`internal/runtime/provider.go`, primary built-in profiles are constructed
directly because they have provider-specific auth, configuration, and startup
plumbing. Configured `[provider.custom.<id>]` profiles use
`transport.MustBuild` with the constructor named by their `transport` field.

Transport packages normally register constructors from `init()`, so the
runtime must import each supported custom transport (often with a blank
import). Registration is concurrency-safe; registering the same name again
replaces the previous constructor, which is useful in tests but should be
deliberate in production.

## HTTP, timeout, and retry semantics

`NewHTTPClient` wraps a cloned base transport as:

```text
loggingTransport → dumpTransport → http.Transport
```

The constructor argument becomes `ResponseHeaderTimeout`, limiting the wait
for response headers. A separate `http.Client.Timeout` caps the whole request,
including streaming, at 20 minutes by default; positive
`METIS_HTTP_TIMEOUT_SECS` overrides that hard cap.

The HTTP client does **not** retry automatically. Each provider explicitly
wraps its replayable request path in `RetryWithBackoff`. That helper retries
only `RetryableError` values and classified transient network failures,
respects context cancellation, applies exponential backoff with jitter, and
honors `Retry-After` up to 60 seconds. Exhaustion returns a typed
`RetryExhaustedError`, preventing an outer agent loop from multiplying the
same provider retry budget.

Long-task recovery is **opt-in**. The Responses adapter and the agent's stream
recovery gate share one `RecoverySession` per logical model response:

| Setting | Default | Meaning |
|---|---|---|
| `METIS_RECOVERY_MAX_SECONDS` | `0` | Recovery disabled; positive integer enables a fault window measured from the first observed transient failure |
| `METIS_RECOVERY_MAX_ATTEMPTS` | `30` | Responses: total HTTP attempts for that logical response, including the initial request, not extra rounds of three |
| `METIS_RECOVERY_MAX_BACKOFF_SECONDS` | `30` | Positive exponential-backoff and `Retry-After` cap |

Invalid, negative, fractional and overflowing values fail rather than silently
falling back. Attempt/backoff values must be positive even when explicitly
configured alongside a disabled window. Subsequent credential refresh, dial,
HTTP work and backoff all consume the window. The initial failing request is
still governed by the ordinary HTTP timeout until a fault is observed. Parent
cancellation and task deadlines always win.

Typed network failures, 429 and 5xx can recover. Known quota failures and
400/401/403 are terminal, including a truncated error body after an authoritative
4xx status. Header success stops the dial-recovery timer so a healthy live SSE
stream is not killed by that timer; it does **not** reset the shared attempts or
first-fault timestamp. Another interruption of that logical response uses only
the remaining budget. Its normal HTTP stream timeout still applies.

The agent discards interrupted drafts before replaying the same model request.
It never executes a tool from a failed stream, nor reruns a previously completed
tool to retry the following model response. Already displayed text deltas may
appear again after the explicit discard notification. This is not an upstream
exactly-once inference guarantee: ambiguous HTTP failures may incur extra model
usage. Providers with their own recovery must forward `ManagesRecoverySession`;
other providers returning an already-exhausted error retain that terminal
boundary. Successful streaming callers release the attempt context on Close.

The strict individual-HTTP-attempt cap is currently guaranteed for **Responses
(including OpenAI Codex)**. For non-managed providers the agent wrapper counts
`Provider.Stream` calls; their SDK/internal three-attempt behavior is unchanged
and can issue more than one HTTP request per outer call. This work does not
claim a universal per-HTTP cap or universal long-window recovery for those
other providers. Their existing exhausted error does not receive another outer
retry round.

The optional `WithRecoveryObserver` callback and debug log expose only numeric
attempt/window/delay fields and state labels, never payloads or credentials.
Agent UI recovery notices are non-blocking, best-effort telemetry so a full
event channel cannot extend the fault window. Required tool/permission events
retain their existing delivery semantics.

Overflow recovery is also provider-driven: provider code may parse a
recognized context-length error with `ParseContextOverflow`, compute one
smaller completion budget with `ComputeAdjustedMaxTokens`, and retry once. It
does not repair truncated SSE or partial JSON.

## Diagnostics and streaming

Debug logging is enabled by `METIS_DEBUG` or `DEBUG` and writes to
`~/.metis/debug.log` unless `METIS_DEBUG_LOG` overrides the path. Every
instrumented request receives `X-Metis-Request-Id`; log lines include the
provider, method, URL path, status/error, latency, and request ID. They do not
contain request bodies, headers, or token counts.

Full dumps are separately enabled by `METIS_DUMP_PROMPTS` or `DUMP_PROMPTS`
and are appended to `~/.metis/dump-prompts/<session>.jsonl`. Redaction is
best-effort, not a complete secret scanner: known authentication header names
(including Gemini's `x-goog-api-key`), headers containing `token`/`secret`,
and recognized secret-bearing body values are redacted; URL credentials and
query strings are removed. Unknown vendor-specific names can still escape the
matcher, so treat every dump file as sensitive and never attach it to an issue
without reviewing it.

SSE dump capture uses a tee: the caller consumes the stream normally while a
bounded buffer records at most 16 MiB. The response entry is queued when the
body is closed, so provider code must close response bodies. Dump writes are
asynchronous; normal shutdown paths call `FlushDumps` to wait for writes and
close the per-session file handles. Forced `os.Exit` paths, including the
TUI's timed force-quit fallback, can bypass that flush and lose pending dump
entries.

`ParseModelWindowSuffix` recognizes only a final vendor-style `-32k`, `-1m`,
and analogous suffix. Reasoning-mode strings such as `:thinking` or `:high`
are outside this helper.
