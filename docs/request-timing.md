# Request latency timing

Metis appends `kind=request_timing` rows to the existing
`~/.metis/traces/<session-id>.jsonl` trace. SessionEventRead / SessionEventSearch
can read/search them (search e.g. `first_nonempty_text_delta` or a request ID).
No command or UI TTFT definition changes. A newly built/restarted Metis is needed;
editing the source does not instrument an already running process.

```sh
jq 'select(.kind=="request_timing") | {session_id,turn,trace_invocation_id,ts,request_timing}' ~/.metis/traces/<session-id>.jsonl
```

`request_timing` contains `phase`, locally generated `request_id`, `provider_call`,
`attempt`, `elapsed_ms`, occurrence timestamp `at`, and optional numeric HTTP
`status_code` / fixed `outcome`. The existing trace envelope supplies immutable
session/turn/invocation ownership. An attempt of 0 means no instrumented HTTP
attempt yet (or a custom/uninstrumented provider), not HTTP attempt 1.

* `pre_request_start`: first request starts at Loop.Run entry, including initial
  memory/capability preparation. Later requests start at the next iteration.
  This excludes UI submission, scheduling and persistence **before** Run.
* `run_start`: Run entry marker on the first request.
* `capability_refresh_start/end`: both initial and per-request refresh intervals.
* `request_build_start/end`: agent request construction (including overflow rebuild).
* `provider_stream_start/return`: each concrete Provider.Stream invocation. A
  loop/recovery retry gets a new provider_call under the same logical request.
* `request_slot_wait_start/end`, `rate_cooldown_wait_start/end`: Chat slot and
  shared cooldown boundaries, also recorded when they complete immediately.
* `http_do_start`: just before client.Do; attempt increments for each Do call.
* `http_wrote_request`: composed httptrace WroteRequest callback, not Do entry.
* `http_first_response_byte`: httptrace GotFirstResponseByte, not first SSE delta.
* `http_response_headers_observed`: client.Do returned response headers. This
  includes outer usage/debug-wrapper overhead; it is not pure upstream latency.
  Redirects / net/http internal connection replays remain owned by net/http and
  may produce multiple httptrace callbacks within one Do attempt.
* `http_error`: Do returned an error without a response; only a fixed error class.
* `retry_backoff_start/end`: existing RetryWithBackoff wait, without policy changes.
* `first_reasoning_delta`: first nonempty decoded plaintext thinking delta.
* `first_nonempty_text_delta`: first decoded text delta containing non-whitespace.
  Both are observed before UI delivery, leading-line filtering and trace burst
  coalescing; opaque reasoning, tool arguments and usage do not count. Each
  returned stream captures its successful HTTP attempt, including after retry.
* `stream_end`: once per returned stream, EOF=ok, provider event/error classified,
  explicit early Close=closed (or canceled/deadline/error if known). Missing text
  or reasoning phases are absent, not a synthesized zero.

All elapsed values use Go monotonic time relative to that request's start;
legitimate `elapsed_ms:0` is retained. Compute durations by subtracting relevant
phase offsets: capability end-start, build end-start, first response byte minus
wrote-request, first text minus response-headers-observed. Pre-request preparation
up to provider_stream_start and slot/cooldown time before http_do_start can be
examined separately. Intervals are nested and must **not** simply be added.

Timing records retain no prompt, body, key, URL, header values or error strings.
They are buffered in memory until Run exits (success, error or cancellation),
then appended via the existing buffered trace writer. They do not trigger burst
flushes or synchronous first-token fsync. Existing trace shutdown/turn lifecycle
flushes persist them; a crash before Run exits or before a normal flush can lose
these diagnostics. Their appended sequence is terminal-batch order, while `ts`
and `at` preserve the actual occurrence time.

The current HTTP instrumentation covers OpenAI Chat (Stream and Complete); the
shared transport helper is reusable, but no broad provider rewrite was made.
Other providers still get agent request/capability/provider/decoded-delta phases,
with attempt 0 until they adopt the HTTP helper. Compaction's independent provider
calls are not assigned their own timing request by this change.
