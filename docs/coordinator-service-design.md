# Coordinator event service contract

The service links authenticated event delivery to durable coordinator claims and
real CLI execution. Acceptance results are recorded by the test scripts; this
document defines the contract rather than asserting that every check has passed.

## Startup and roles

```sh
metis coordinator serve <run-id> --cwd /absolute/project/path \
  --agent event-writer --verify-agent event-verifier \
  --token-file /private/path/event-token --listen 127.0.0.1:9091 \
  --workers 2 --poll 500ms --lease 30s
```

The run must exist and have host checks. Before starting workers or HTTP handlers,
the service sets its process cwd to the canonical workspace so profile and project
configuration resolve consistently. New HTTP event
implementation and verification nodes persist these profiles. Existing nodes
retain their profiles and normal default behavior. The service processes every
ready node; finish the initial plan first when it should start idle.

Workers are independent CLI processes, with concurrency 1–16. Polling is at least
50 ms and renewable leases are 1 second–24 hours. `serve` runs in the foreground;
restarting that command with the same private home, workspace and run ID continues
the durable graph. Host supervision is a separate explicit command below. Neither
command installs a public tunnel, subscribes to a vendor event API or wakes a
sleeping machine. A platform adapter must deliver its events to the local endpoint.

## macOS host supervision

```sh
metis coordinator service install <run-id> --id project-events \
  --cwd /absolute/project/path --agent event-writer --verify-agent event-verifier \
  --token-file /private/path/event-token --listen 127.0.0.1:9091 \
  --workers 2 --poll 500ms --lease 30s
metis coordinator service status project-events --json
metis coordinator service uninstall project-events --json
```

Installation currently supports macOS user LaunchAgents. It validates the existing
run and profiles, requires a fixed nonzero loopback port, and copies the CLI and
bearer token into a private managed directory under
`METIS_HOME/coordinator-services/<id>`. The LaunchAgent uses that stable executable
with `RunAtLoad` and `KeepAlive`, so launchd starts it at user login and restarts it
after exit. The original temporary CLI is not its runtime dependency. The ID is
1–64 letters, digits, underscores or hyphens; an existing ID is rejected rather
than overwritten.

`status` reports the managed manifest and launchd state. `uninstall` stops the
registered service and confirms that launchd no longer has it before removing its
plist and managed files. A stop or ownership check failure preserves those files.
Uninstall retains the project graph and usage/operation journals. This is a user
login service, not a system boot daemon or a Linux/Windows supervisor installer.
Its installation and automatic restart need native launchd acceptance evidence;
the presence of the plist template alone does not establish that result.

## HTTP admission

Listen only on an explicit loopback IP. The token must be in a regular private
file (0600 on Unix), at least 32 non-whitespace characters, and is read once at
startup. Every API needs `Authorization: Bearer <token>`. Constant-time comparison,
cross-origin rejection and a 1 MiB body limit apply. Tokens are never logged or
passed into worker environment variables.

`POST /v1/events` accepts `source`, `id`, `subject`, `prompt`, and optional
`deliverables` and `job_id`. Source and ID are trimmed and bounded to 128/256 bytes. Unknown
fields, trailing JSON, empty required fields and unsafe delivery paths are
rejected. HTTP callers cannot supply a role definition, workspace, model,
command or limit. A `job_id` must match a saved identity explicitly allowed by
the host's repeatable `--job ID` flags; its immutable definition supplies the
execution role. Omitting `job_id` preserves the profile-based event behavior.

A successful response is `202` after the event pair is durably stored. Identical
source/ID/payload/profile delivery is idempotent across processes and restart;
conflicting content returns `409`. Storage failure never produces an accepted
acknowledgement. An event received just before shutdown remains in the graph
for the next service process.

Authenticated `GET /v1/status` and `/v1/health` expose run ID, aggregate status,
item counts, active children and estimated usage. They exclude prompts, worker
output, credentials and lease tokens.

## Execution and restart

Explicit provider completion-boundary interruptions use
`provider_stream_interrupted` and may retry unfinished work within the existing
attempt budget (two attempts by default). The coordinator accepts only the
precise missing `response.completed` messages, including the matching SSE error
envelope. Authentication, configuration, context-length, actual client HTTP
errors, generic EOF/disconnection and host acceptance failures retain their
terminal behavior. Retry preserves prior files, completed nodes and cumulative
estimated usage, and a replacement claim fences the interrupted token.

Writing tools use the durable operation journal described below. A confirmed
matching operation result can be replayed without invoking the tool again. An
operation whose external result is unknown holds its entire work item for trusted
resolution rather than automatically retrying a potentially applied write.

Environment discovery uses the service's parent context and one shared
five-second deadline for the entire Git probe, with an additional bounded
one-second pipe-drain allowance. Cancellation stops the owned Git process group.
A cancelled, timed out or unjoined observation returns an error and leaves the
previous project state intact; it does not reclassify a repository as non-Git.
Child workers open independent Stores so service cancellation does not disable
their usage or interrupted-claim bookkeeping.

The pool scans persisted ready work and starts the same executable with
`coordinator run --max-items 1 --summary-json`. Its response contains only
`outcomes_count`, `run_id`, and `status`, independent of graph size. Atomic claims
decide ownership when services race. An idle loser exits normally and backs off;
unexpected worker/start/storage failures use bounded backoff. An idle completed
graph creates no model requests.

Task exit 11 does not stop HTTP reception. New independent events can be accepted
and executed while prior failed acceptance stays visible. Project/item budgets,
deadlines, delivery hashes and attempt limits remain authoritative.

On normal stop, HTTP admission closes and children receive cancellation, get
five seconds to finish shutdown, and are joined after forced group termination
when needed. On Darwin and Linux, service children watch the owning parent's PID
and process birth identity; parent death or PID reuse cancels the child. This
watch fails closed when the supplied parent identity cannot be validated. A
standalone `coordinator run` without service parent metadata retains its ordinary
context lifetime.

Unfinished claims without an unresolved writing operation use
`worker_interrupted` and requeue within their attempt budget. On a combined crash,
a replacement service reconciles expired claims and unknown operations before
dispatch. A surviving valid claim is respected until cancellation or lease expiry;
an unresolved operation prevents automatic replay of the item. Completed nodes,
confirmed operation results and persisted usage are retained. Browser processes
and in-flight model inference are not restored. Local `coordinator event` adapters
can enqueue while HTTP is stopped; a vendor must retry deliveries it could not
submit while the listener was unavailable.

## Writing operation journal

The worker reserves a durable operation before invoking a guarded tool, then
stores the complete successful `pkg/tool.Result` before treating that operation
as confirmed. The identity binds the work item, tool name, normalized arguments
and a per-signature occurrence. Bash also binds its effective working directory.
Normalization removes only known presentation/time-budget fields; it preserves
command/code bytes, execution scope and unknown tool fields.

Within one claim, an intentional repeated call advances the occurrence and may
execute again. A replacement claim starts each signature's occurrence sequence
at one, allowing the corresponding confirmed results to replay. Replay retains
the full result, including metadata and presentation fields; it does not execute
the tool again. Read-only tools run fresh. `Agent`, `Fork` and `Ralph` containers
propagate the executor into child calls rather than caching a container result.
Bash and RunCode are guarded conservatively because their read-only heuristics
cannot prove that arbitrary code has no side effects.

Guarded Bash stays in the foreground until the actual command exits; automatic
promotion and wait-pattern detachment are disabled for coordinator contexts.
Explicit background/await-job flags are rejected before preparing or executing
a write. A tool that nevertheless returns only a `background_job` acknowledgement
is uncertain rather than confirmed. Ordinary interactive Bash keeps its existing
background behavior.

A started operation left by a crash, a tool error, a missing result, an
`IsError` result or failure to persist the successful result creates an uncertain
outcome. The item is held with `operation_uncertain`, and its worker context is
cancelled. Changing the arguments does not bypass that hold. After checking the
external system, a trusted operator can inspect and resolve the operation:

```sh
metis coordinator operation list <run-id> --cwd /absolute/project/path --json
metis coordinator operation resolve <run-id> <operation-id> \
  --cwd /absolute/project/path --outcome applied \
  --result-file /private/path/confirmed-tool-result.json --note "Checked remote receipt"
# Use this alternative only when the action was confirmed not to have applied.
metis coordinator operation resolve <run-id> <operation-id> \
  --cwd /absolute/project/path --outcome not_applied --note "Checked remote absence"
metis coordinator recover <run-id> <item-id> --cwd /absolute/project/path \
  --note "Operation outcome reviewed"
```

`applied` requires a private JSON file containing the complete successful tool
result, so the replacement worker can replay it. `not_applied` forbids a result
file and permits the operation to execute on a later claim. Resolution requires
an evidence note and an inactive original claim; it records the decision without
requeuing work. Explicit `recover` remains necessary and does not reset attempts
or budgets. The existing single forced-recovery allowance is separate from
operation resolution. List/resolve responses omit stored results, claim tokens
and resolution notes.

The journal reduces repeated matching writes and stops unknown outcomes. It does
not infer that differently spelled shell commands or changed arguments represent
the same external action, and a tool's successful result is not a distributed
transaction with its target. Arbitrary external exactly-once semantics still
require a target-side idempotency key, an authoritative receipt or a transaction.

## Physical request usage and receipts

Anthropic Messages, OpenAI Chat Completions and OpenAI Responses requests with the
coordinator observer reserve a random 128-bit client request ID durably before
network dispatch. Each actual HTTP attempt, including transport retries and
nonstream requests, has its own record. Response request/response IDs and reported
usage are persisted before provider consumers receive those observations. An
observer failure stops processing rather than silently bypassing accounting.
Unsupported observed wire protocols, including Gemini/AWS protocols, fail closed;
their native observer support is a separate implementation boundary.

Usage snapshots are absolute cumulative counters, not increments. Cache-read
tokens are normalized separately from uncached input. `usage_reported: false`
means the provider has not supplied usage; it differs from an explicit reported
zero. Repeated snapshots and receipts do not add the same consumption twice. The
attempt budget uses the maximum of its legacy aggregate floor and request-ledger
total, and the tracker observes that persisted spend rather than double charging
both paths. The active context snapshot still keeps the final request's usage.

A forced kill leaves reserved request identities and already persisted snapshots
available for later reconciliation. A receipt can supply final usage that was
not available to the killed process without another inference request. No missing
tokens or costs are guessed. Catalogue-based `estimated_usd` and receipt-backed
`actual_usd` remain separate; absent actual cost is reported as unknown, not zero.
The budget retains its monotonic spend floor even when a later bill is lower than
the catalogue estimate.

```sh
metis coordinator usage list <run-id> --cwd /absolute/project/path --json
metis coordinator usage import <run-id> --cwd /absolute/project/path \
  --receipt-file /private/path/provider-receipt.json --json
metis coordinator usage reconcile <run-id> --cwd /absolute/project/path \
  --receipt-dir /private/path/receipt-inbox \
  --command /private/path/provider-usage-adapter --json
```

`serve` reconciles on startup and every 30 seconds. Its default private inbox is
`METIS_HOME/coordinator-usage-receipts/<run-id>`; `--usage-receipts DIR` selects
another absolute private directory outside the workspace. Receipt files and the
optional `--usage-reconcile-command FILE` executable must be private host-owned
files outside that workspace. Service installation accepts and persists those
same flags.

The optional executable receives whitelisted JSON metadata on stdin with
`schema: 1`, `run_id` and `requests`. Request metadata includes client/provider/
response IDs, provider, endpoint SHA-256, model, request SHA-256, status and usage;
it excludes prompts, authentication headers and claim tokens. The executable
returns one receipt object or an array of receipt objects on stdout. Its run has
a 20-second deadline, a 2 MiB output bound and owned process-group termination
with a bounded join. Returned receipt evidence is stored before import so a
restart can repeat the import safely.

A receipt contains `receipt_id`, `evidence_sha256`, `authority`, `provider`,
`endpoint_sha256`, `model`, at least one matching request/response ID, and explicit
absolute `usage` fields `input`, `output`, `cache_read` and `cache_write`.
`actual_usd` is optional. Matching binds the receipt to the provider, endpoint,
model and unique request; unrelated, ambiguous, decreasing or conflicting
evidence is rejected. Reimporting identical evidence is a no-op. `authority` names
the evidence source; it is not a cryptographic signature or proof by itself.
The operator must supply a trusted feed for the bound provider account.

These commands provide the reconciliation mechanism, not an authoritative feed
for every gateway. An existing gateway with statistics disabled has not been
changed to enable them. If its account cannot supply a final receipt or usage
lookup, orphan usage remains explicitly unresolved; the CLI does not fabricate
provider billing evidence.

## State compatibility

State transactions use stable OS advisory locks. Current graphs use state
version 5, including immutable operation archive references.
Version 1–4 states are upgraded atomically under the run lock; older writers
must reject version 5 rather than
rewrite it without the new journals. Stop old workers before upgrading and do
not mix writer generations.

Directory-lock-era binaries must also be stopped before upgrading. A legacy lock
directory is refused and preserved until the operator confirms old workers have
stopped. Stable lock files must never be removed to unlock a run.

## Verification protocol

| Case | Required evidence |
|---|---|
| HTTP auto dispatch | Actual model tokens and Read/Bash/Write output, declared hash and host acceptance, without manual worker launch. |
| HTTP admission | Correct 401/400/413 responses and no graph pollution for invalid requests; public listen rejected. |
| Replay | Identical event retains exactly two nodes across service restart; conflict is 409. |
| Parallel work | Two real workers execute distinct files concurrently with one attempt per node. |
| SIGTERM restart | Service joins old workers; new process claims unfinished work with a new token, preserves usage and completes it. |
| Combined crash | Owned workers and leader die; expired claims and writing-operation holds are reconciled, and old completion/renewal/failure writes are rejected. |
| Parent death | On Darwin/Linux, killing only the service leader cancels its child by PID and birth identity; no new tool or provider dispatch escapes that lifetime. |
| Host supervision | Native macOS install copies a stable CLI/token; launchd starts/restarts it; status confirms state; uninstall stops it before removing managed files and preserves the graph. |
| Confirmed write replay | A replacement claim replays the full result of a matching occurrence without repeating the external write; deliberate same-claim repetition uses a new occurrence. |
| Guarded Bash completion | Finite commands are awaited through their actual exit; a background job acknowledgement is not journaled as confirmed success, and explicit detached work cannot escape the claim. |
| Uncertain write | Crash/error/IsError holds the whole item, including changed-argument calls; trusted applied/not_applied resolution and explicit recover preserve attempt/budget limits. |
| Request ledger | Every real transport attempt reserves a distinct ID before dispatch; header/usage snapshots persist, cache buckets normalize, and aggregate/tracker accounting does not duplicate spend. |
| Receipt recovery | After a killed real request, bound authoritative receipt evidence supplies missing absolute usage; replay is idempotent, conflicts fail and missing evidence stays unresolved. |
| Adapter lifetime | A private usage adapter receives only metadata, returns bounded receipt JSON, and is killed/joined on timeout or service cancellation. |
| Offline queue | Local adapter stores an event while HTTP is stopped; restarted service consumes it automatically. |
| Failed acceptance | Host exit 7 yields task exit 11; service stays available and preserves the failed evidence while processing new work. |
| Multi-hour runtime | One coordinator process continuously performs real accepted work for at least two hours, then drains the queue. |
| Cleanup | Every round destroys its test CLI, temporary provider and event credentials, and owned processes. |

Use `scripts/e2e/project_coordinator_service_live_check.py` for real service tests,
`scripts/e2e/project_coordinator_live_check.py` for real concurrency/recovery and
two-hour continuous work, and
`scripts/e2e/project_coordinator_durability_live_check.py` for the newer journal,
request-receipt and host-supervision cases. Script availability or prior-round
results do not establish acceptance of the current source. Each claimed result
requires its current report and native/model evidence. `--quick` does not satisfy
the long-task duration. Unit/helper tests verify contracts separately from real
provider queries. Linux/Windows host supervision and Windows native runtime are
separate acceptance boundaries.
