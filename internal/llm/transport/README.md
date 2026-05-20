# internal/llm/transport

The transport-layer plumbing shared across all LLM providers: the
init-time registry that maps a transport name → constructor, the HTTP
client with retry / dump / log instrumentation, output-overflow
guards, and the model-suffix parser. Provider subpackages
(`anthropic/`, `openai/`, `gemini/`, …) call `transport.Register`
in their `init()` so `runtime/provider.go` can dispatch by name
without an ever-growing switch.

## File-naming convention

| File | What it owns |
|---|---|
| `registry.go` | The transport-name → constructor map; `Register` / `Lookup`. Provider subpackages populate via init() side-effects. |
| `httpclient.go` | Shared `*http.Client` with sensible timeouts, retries, and dump/log hooks wired in. |
| `retry.go` | Backoff / jitter / max-attempts policy. Plugged into the HTTP client. |
| `dump.go` | Optional request/response dump to disk for debugging (gated on env var). |
| `log.go` | Structured logging of HTTP traffic (status / latency / token counts). |
| `overflow.go` | Guards against provider-side overflow modes (truncated streams, partial JSON). |
| `model_suffix.go` | Parses model-id suffixes like `:thinking` / `:high` into structured flags. |

Each file pairs with an `_test.go` of the same name. 7 prod + 6 test
= 13 files, all transport-mechanism rather than per-provider logic.

## Where do I find...

- **Adding a new provider's transport** → write `RegisterX()` in your
  subpackage's `init()`, point `runtime/provider.go` at it with a
  blank import (`_ "github.com/Ricardo-M-L/metis/internal/llm/x"`).
- **Retry policy** → `retry.go` (timeouts, max attempts, backoff curve).
- **Why a request dumped to disk** → `dump.go` + the `METIS_LLM_DUMP`
  env var.
- **HTTP latency / status logging** → `log.go`.
- **What's a model suffix and how is it parsed** → `model_suffix.go`.

## Design invariants

- Registration is **init-time only**. Adding `_ "<pkg>"` to
  `runtime/provider.go` is what wires a provider in — easy to miss
  during refactors, so that file maintains a blank-import block listing
  every blessed provider explicitly.
- The shared HTTP client is the **only** path that should call out to
  provider APIs. Providers that bypass it lose retry, dump, log, and
  overflow protection.
