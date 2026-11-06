# Context budgets and prompt-cache stability

Context reduction and provider prompt caching are separate concerns. Metis limits new model-visible content before its first request, then keeps that retained projection stable. A cache key or a locally unchanged prefix is not proof of a provider cache hit; provider-reported usage is authoritative.

## Invariants

- Budget newly recalled memory and new tool observations, not previously transmitted history on every turn.
- Deduplicate memory only against the actual text or fragment retained in active history. A previous retrieval ID or a lossy summary does not prove that a fact is still available.
- Compute content identity without access timestamps or retrieval counters. Access accounting must not invalidate semantic memory versions.
- Preserve tool-call/result pairing, user steering, permission denials and safety diagnostics. If protected content alone exceeds a budget, report the condition rather than silently deleting it.
- Save recoverable content before replacing it with a preview. A truncated download or an already bounded upstream tool response is not an original full result.
- Persist the exact retained text, block order and attachment references. Resume must not regenerate old previews with a new template.
- Revocation takes priority over cache reuse. Revalidate recalled memory at acceptance and request construction; a replacement history must not continue an obsolete provider-managed response chain. For repositories supporting the recall guard, repository locking protects validation and request-snapshot construction; this immutable snapshot is the acceptance linearization point. A change completing after that point applies to the following request, not retroactive cancellation of an already accepted snapshot.
- A request already sent to a provider cannot be recalled by removing local history. Disconnecting a response chain is not a claim that the provider erased stored data.
- Host-created compaction summaries carry source identities/hashes through repeated compaction and persistence. If a dependency is withdrawn or cannot be validated, invalidate the entire derived narrative and its iterative summary seed rather than deleting matching strings. The replacement explicitly warns that earlier task details may be missing and must be recovered from authorized sources or clarified with the user. This is not a promise of semantic erasure from arbitrary user/assistant prose or from requests already sent.

## Transport boundaries

The provider adapters have different system/message layouts and capabilities. Anthropic cache markers protect prefixes; marking one block volatile does not isolate later tokens from changes to it. Chat Completions and Responses cannot be treated as the same transport. Provider-managed Responses continuation also differs from full local replay.

Keep tool ordering and schema serialization deterministic. Do not switch the whole tool set on each turn merely to lower schema size. Discovering a tool can still invalidate a suffix or an endpoint-specific continuation; report that separately from measured cache usage.

On a transport switch, durable Metis checkpoints remain history data, but request projection removes obsolete checkpoints from endpoints that cannot replay ordered privileged tail messages. The current complete state stays in the endpoint's supported privileged system/instructions position; it is never downgraded to a user message. Managed/local transitions rebuild an obsolete continuation once, then resume the new chain. Execution, vision-override and model-settings wrappers forward the underlying live boolean capability. Provider usage anchors refer to live history, even when a request projection omits host-only checkpoints.

## History replacement

Normal hot-session optimization must not repeatedly shorten old tool results or remove old memory blocks. Existing pressure-triggered compaction remains the fallback for bounded windows. Manual compression, model-budget changes, revocation and overflow recovery can require a new checkpoint and provider-state reset.

There is no universal cache expiry time. In the absence of reliable endpoint-specific retention information, elapsed wall-clock time alone must not rewrite history. Provider-specific cache editing is not assumed available.

## Configuration and diagnostics

These budgets are local estimates, not exact provider tokenizer limits. Metadata, retained warnings and recovery pointers participate in the relevant budget.

| Setting | Default | Meaning |
| --- | --- | --- |
| `METIS_AUTO_RETRIEVE_TARGET_TOKENS` | 800 | Target for newly admitted recall in one human turn |
| `METIS_AUTO_RETRIEVE_MAX_TOKENS` | 1500 | Maximum estimated new recall, including its wrapper and references |
| `METIS_TOOL_RESULT_TOKENS` | 8000 | First-ingestion budget for one tool result |
| `METIS_TOOL_BATCH_TOKENS` | 24000 | Combined new tool batch budget, including mandatory context |
| WebFetch/WebSearch `max_output_tokens` | 4096 | Model-visible output estimate, clamped to 1024–16384 |
| WebFetch `max_bytes` | 1 MiB | Download ceiling, maximum 16 MiB; distinct from output tokens |
| WebFetch `mode` | `body` | Local cleaned HTML conversion; `raw` returns a budgeted original preview |
| WebSearch `fresh` | false | Bypass brief successful-result reuse |
| OpenAI-compatible provider `extra.tail_system_messages` | false | Explicit endpoint opt-in to privileged dynamic checkpoints for Chat Completions |

Embedders may set the corresponding Loop recall and tool-budget fields. Existing `METIS_AUTO_RETRIEVE` still controls Top-K/enabling; it is not itself a token budget. Oversized whole memory passages are skipped rather than arbitrarily truncated. Third-party memory repositories without the optional snapshot capability retain legacy behavior and cannot promise external revocation detection.

WebSearch queries configured API sources and the keyless backend concurrently by default. `sources` can select a narrower set. Duplicate URLs are merged with source attribution; a session-scoped 15-second reuse window applies only to successful, nonempty searches with no source failures, and configuration changes invalidate reuse. Complete downloaded/search content is retained before previewing. Explicit source errors remain visible. No extra paid extractor or secondary model is required.

Tool-batch budgets preserve user steering, trusted safety guidance and visual content. Native images use the same local vision estimate as active context; base64 bytes are not text tokens. File-size and attachment limits remain separate storage/transport protections. When storage fails or mandatory material alone cannot fit, Metis checkpoints the retained batch where possible and stops with an explicit error before further provider work. Raise the relevant budget or resolve the storage problem; do not silently remove safety context to continue.

Session images are content-addressed private attachments, with a 32 MiB per-blob ceiling and hash verification. JSONL records reference saved blobs rather than repeating base64. Missing/corrupt blobs fail resume explicitly. `Store.Export` emits self-contained JSONL with hydrated images; import externalizes them again. If manually copying a raw session ledger instead of using Export, copy its attachment directory too. Deleting a session removes its owned attachment sidecars and verified owned subagent attachments; failures remain explicit and retryable.

`MetisInfo` with `section: cache` reports actual provider usage alongside `local_request_estimate` diagnostics. The logical common-prefix estimate is not the wire token sequence and is not a measured cache hit. Changes to fingerprints or tool definitions must not be described as proof of a full cache miss. Unknown cache TTL never triggers idle-only history rewriting.

## Regression acceptance matrix

Context changes must pass the actual entry points, not only the underlying manager or budget helper. The regression scenarios below are required; a green pre-existing suite alone is not sufficient acceptance evidence.

| Boundary | Required scenario |
| --- | --- |
| Desktop memory binding | Recall, revoke through the bound workspace repository, then inspect the next provider request; wrapped and direct repositories enforce the same authority |
| Compaction-derived memory | Run real compaction, revoke a contributing source, and ensure the derived summary is invalidated or safely rebuilt; repeat across compaction and resume |
| Native image budget | A small-dimension, large-encoded PNG uses native vision accounting, not base64-as-text accounting, while ordinary text remains budgeted |
| Transport switch | A persisted Responses checkpoint is projected legally for Anthropic and Chat; obsolete plan/cwd snapshots do not override current state |
| Error hook | A Go-error tool result retains trusted hook diagnostics after output projection |
| Denied plan transition | Rejected plan entry follows the same final batch budget and durable boundary as normal tool results |
| Session reset | A failed pending revocation cannot write any former session material to the new session ledger |
| Redirected HTML | Relative links resolve against the final successful response URL |
| Hidden tables | Hidden rows, cells, sections and ancestors obey the same visibility filter as ordinary HTML |
| Execution binding | Provider wrappers preserve dynamic-checkpoint capability without losing execution cancellation/accounting |

## Exact summary sources and projected accounting

Summary provenance follows the content actually admitted to the summary request, not the entire pre-compaction history. A source present only in the retained tail or completely omitted by local fitting is not a dependency. A partially retained source remains a dependency. The previous summary's dependencies propagate when that summary is used as input, and also when it is reused directly without a model call; direct reuse does not depend on the iterative-summary switch.

Response attribution and token estimation are distinct: history revisions and live-history positions establish which request owns a response; projected model-visible content establishes local token estimates and usage plausibility floors. Filtering host checkpoints must not inflate compression pressure or reject plausible provider usage. Neither local projection nor incremental network transmission proves that provider-managed history stopped consuming context.

Required regressions cover: retained-tail-only memory; a genuinely summarized source; a completely omitted source; a partially retained source; direct reuse of an old summary; and transport-switch accounting with unchanged response-attribution checks. Evidence must capture the actual summary request or adapter projection, not infer participation from the original untrimmed transcript.

## Validation

Use local fixtures and deterministic HTTP servers for functional tests; never require a paid extraction service for ordinary web cleanup. Exercise normal, failed, interrupted and hook-halted tool batches. Test memory content changes while retrieval is in flight, not just sequential updates.

For attachment resume, compare the model-visible image bytes, text, ordering and visual options through each relevant adapter. File existence alone and a JSON round trip alone are insufficient. Missing or corrupt attachments must produce an explicit failure, not silently drop visual input.

A provider A/B evaluation should keep model, endpoint, task fixtures and initial state comparable and report uncached input, cached input, cache creation where provided, output, summary overhead, first-token latency, wire bytes and task-quality outcomes separately. Do not infer token savings from response byte counts or promise a cache-hit improvement from unit tests.
