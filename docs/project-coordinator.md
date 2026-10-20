# Durable project coordinator

METIS has two levels of task state:

- `TaskCreate` / `TaskUpdate` are a small checklist for the current chat
  session.
- A project run is a workspace-bound dependency graph that survives a session
  switch, Desktop restart, and independent CLI worker process.

The durable graph lives under `~/.metis/project-coordinator/<workspace-hash>/`.
Its run files are private (`0600`) and their directories are private (`0700`).
The hash keeps project paths out of filenames; every record still verifies its
canonical workspace before METIS loads it.

## Lifecycle

Create a project run with a concrete objective:

```sh
metis coordinator create --cwd /absolute/project/path --goal "Ship the migration" \
  --check "go test ./..." --max-budget-usd 10 \
  --deadline "2026-12-01T18:00:00+08:00"
```

METIS creates a conservative four-stage graph:

```text
research → synthesis → implementation → verification
```

The initial plan is deliberately sequential. After research gives evidence, a
coordinator may add independent work with `metis coordinator add`; this avoids
inventing parallel edits before the codebase and file ownership are understood.

```sh
metis coordinator add <run-id> \
  --cwd /absolute/project/path \
  --phase implementation \
  --subject "Migrate the API client" \
  --prompt "Update the client and its focused tests." \
  --depends-on synthesis
```

`claim` atomically reserves one ready work item and returns its prompt. This
is suitable for an external worker or integration:

```sh
metis coordinator claim <run-id> --cwd /absolute/project/path \
  --worker ci-worker-1 --prompt --json
metis coordinator complete <run-id> research --cwd /absolute/project/path \
  --worker ci-worker-1 --lease-token <item.lease.token> \
  --output "Repository inspected; focused tests: ..."
```

The built-in worker bridge performs the same lifecycle around a real METIS
agent turn:

```sh
metis coordinator run <run-id> --cwd /absolute/project/path --until-idle
metis coordinator status <run-id> --cwd /absolute/project/path
```

Each claim has a lease. If a process dies, the next status, claim, or run
reclaims the expired item and records `worker_lease_expired`; it never remains
silently stuck in `running`.

`coordinator run` renews its claim during runtime startup, model work and host
acceptance. A renewal failure cancels that worker. External workers must call
`coordinator renew <run-id> <item-id> --worker NAME --lease-token TOKEN` before
their lease expires. Every new claim has a new token; `renew`, `complete` and
`fail` reject old tokens even if a replacement uses the same worker name.

Normal process cancellation records `worker_interrupted` and returns unfinished
work to the queue within its configured attempt budget. The cancelled process
does not claim another item. An explicit provider stream failure reporting
`stream closed before response.completed` records `provider_stream_interrupted`
and requeues unfinished work within the same attempt limit (two attempts by
default). This includes the precise completion-boundary message inside a gateway
SSE `invalid_request_error` envelope; that error type alone is not retryable.
Authentication/configuration/context-length errors, actual client HTTP errors,
generic EOF/disconnection messages, failed host acceptance and task limits
retain their failure semantics. Recovery preserves completed nodes, existing
files and cumulative estimated usage; it retries unfinished work rather than
restoring an in-flight provider inference or browser process.

State transactions use a stable OS file lock that is released when its holder
exits, including after `SIGKILL`. Stop workers built with the old directory-lock
implementation before upgrading: old and new binaries must not share a run.
If an old crash left a `.locks/<run-id>.lock` directory, the new binary refuses
it. Remove that specific legacy directory only after confirming all old workers
have stopped. New lock files must remain in place even while idle.

## Acceptance and deliverables

Every completion requires nonempty result evidence. Explicit blocked or
`ACCEPTANCE_INCOMPLETE:` responses cannot advance the graph. Verification nodes
also require host check commands, supplied through `create --check` or:

```sh
metis coordinator configure <run-id> --cwd /absolute/project/path \
  --check "go test ./..." --check "go build ./..."
```

Checks run in the canonical workspace with a restricted environment and a
two-minute timeout per command. Cancellation stops the command's process tree.
The host records exit codes, elapsed time, bounded redacted output and its
SHA-256. Checks are configured through the CLI, not model tool arguments. An
unconfigured, failed or cancelled verification remains incomplete.

`coordinator run` preserves its JSON result but exits with `11` when a worker
failed or the project is blocked/cancelled. Successful partial progress with
other ready work still exits with `0`; callers inspect the run status to decide
whether to continue. Shell success cannot mask failed acceptance.

`add --deliverable relative/file` records required output files. Completion
checks their existence and saves hashes and sizes. Dependent work revalidates
those hashes; checks that change a declared delivery require another validation.
Paths outside the workspace, including escaping symlinks, are rejected.

Ordinary work records `acceptance.status=recorded`; verification records
`passed` only after host checks succeed. A worker's prose is not a host verdict.

## Persistent limits and roles

`create` and `add` accept `--deadline` and `--max-budget-usd`; `add` also accepts
`--agent PROFILE`. Workers use the earlier project/item deadline and the smaller
remaining project/item budget. Parent and child usage share one tracker.
Estimated usage is journaled after each model response, including late usage
from a cancelled attempt, so restart and retry do not reset or double count it.
Pricing uses the existing model catalog; an unpriced model records zero estimated
USD, so this is not a billing guarantee. Reached limits block further work.

The selected profile is persisted on the work item and loaded by
`coordinator run`; event work can therefore return to the same saved role.

## External event ingestion

```sh
metis coordinator event <run-id> --cwd /absolute/project/path \
  --source github-ci --id build-123 --agent maintainer \
  --subject "Repair failed CI" --prompt "Inspect and fix build-123." \
  --deliverable src/client.go
metis coordinator run <run-id> --cwd /absolute/project/path --until-idle
```

Source plus event ID atomically deduplicates deliveries across processes and
restarts. Conflicting payloads with the same ID are rejected. Each new event
adds implementation and verification work, reopening a completed project while
preserving its prior evidence and cumulative limits. The external integration
can submit the event and start a worker explicitly, or use the resident event
service below. Vendor subscriptions and cloud provisioning remain external.

### Resident event service

For a foreground service that receives events and automatically claims work:

```sh
metis coordinator serve <run-id> --cwd /absolute/project/path \
  --agent event-writer --verify-agent event-verifier \
  --token-file /private/path/event-token --listen 127.0.0.1:9091 \
  --workers 2 --poll 500ms --lease 30s
```

Create the token file with mode `0600` and at least 32 non-whitespace characters.
Both named profiles must exist, and the project must have host acceptance checks.
The service executes existing ready nodes as well as new events, so finish the
initial plan first if it should start idle. Existing nodes retain their profiles;
new HTTP events use the implementation and verification profiles fixed at startup.

Send an authenticated `POST /v1/events` with this JSON shape:

```json
{
  "source": "ci-adapter",
  "id": "build-123",
  "subject": "Inspect the failed build",
  "prompt": "Inspect build-123 and produce the requested repair report.",
  "deliverables": ["repair-report.md"]
}
```

Use the `Authorization: Bearer <token>` header. The endpoint persists the event
before returning `202`; repeated identical deliveries reuse the same two nodes,
and conflicting payloads return `409`. HTTP callers cannot choose a role,
workspace, model or host command. The listener accepts loopback IPs only.
Authenticated `GET /v1/status` returns counts, active children and estimated usage
without exposing claim tokens or model output.

The service remains running while idle. It starts independent CLI workers, each
using the existing atomic claims and renewable leases. Task failures retain exit
`11` evidence without stopping event reception. Worker supervision uses
`coordinator run --summary-json`, a bounded response containing only outcome
count, run ID and status; normal `--json` output remains available separately.

Stop and restart the same command with the same `METIS_HOME`, workspace and run
ID to continue durable work. Normal shutdown joins its workers; crash recovery
waits for expired claims. If a service leader dies while a child survives, a new
service respects that child's valid lease. Multiple services can share a graph.
Recovery is bounded by each item's attempt budget. Events can be queued through
`coordinator event` while the HTTP service is stopped and are picked up on restart.
This command does not install an OS supervisor or subscribe to a vendor API;
an external adapter must deliver its events to the endpoint.

The service contract and acceptance matrix are in
[coordinator-service-design.md](coordinator-service-design.md). The real-model
CLI checks are reproducible with `scripts/e2e/project_coordinator_live_check.py`
and `scripts/e2e/project_coordinator_service_live_check.py`; the former's default
run requires at least two hours of continuous accepted work. Its `--quick` mode
does not qualify as the long-task acceptance test.

## Environment-aware recovery

Before an `Agent` creates a worktree, METIS probes the requested workspace for
Git state and linked-worktree state. A non-Git directory combined with
`isolation: "worktree"` is unambiguous: METIS records
`worktree_requires_git` in the private execution profile and continues once in
the same directory with direct execution. The project worker prompt receives
that rule on later work so another worker does not rediscover the same error.

Nested worktrees, invalid paths, unknown provider errors, and other ambiguous
failures remain blocked. They are recorded with a stable code and suggested
action, and require an explicit `recover` after the cause is fixed. Known
automatic recovery is bounded by each work item's attempt budget. After that
budget, `recover --force` permits one reviewed final retry; it cannot be used
repeatedly to make the same failure loop forever.

## Agent mode

`metis --coordinator chat` exposes the `ProjectCoordinator` tool to the
team-lead model. The model can create and expand the same project graph, claim
an item before it dispatches a teammate, and then record completion or a
specific failure. `claim_next` returns a bounded prompt containing only the
project goal, execution-environment rules, and direct prerequisite evidence.

This leaves the project graph as the source of truth while transcripts remain
the detailed reasoning record.

For resident job subscriptions, exact-message dispatch and host-backed business
completion receipts, see [automatic job assignments](job-handoff-dispatch.md).
