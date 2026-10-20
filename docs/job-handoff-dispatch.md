# Automatic job assignments and business receipts

An existing persistent job can receive work without a separate `job run` or
`coordinator add` for each message. The host explicitly subscribes a resident
service to saved jobs and retains control of the verification commands:

```sh
metis coordinator serve <run-id> --cwd /absolute/project/path \
  --agent event-writer --verify-agent event-verifier \
  --job <job-id> --token-file /private/path/event-token
```

`--job` is repeatable. The run must already contain trusted host acceptance
checks. Finish its initial plan first if the service should begin idle. Native
service installation persists the same allowlist. An empty allowlist retains
the existing event-only service behavior.

Submit one structured task to the offline or online job:

```sh
metis handoff send --to job:<job-id> --key order-review-42 \
  --body "Review the attached orders and write the declared result." \
  --task-file /private/path/task.json
metis handoff status <message-id>
```

The task JSON contains `id`, `objective`, optional input `artifacts` with
`path`/`sha256`, output `deliverables` as workspace-relative paths, and optional
natural-language `acceptance` requirements. Input artifacts are checked on
admission, before paid runtime startup and on completion. Outputs cannot replace
immutable input artifacts. Message acceptance text never becomes an executable
host check. Ordinary messages without a task remain notifications.

The service reserves one durable assignment before atomically creating its
implementation and verification nodes. Two services cannot assign the same
message to different runs. Restarting the selected service repairs an interrupted
reservation, admission or result projection. Paused/busy jobs and unresolved
usage retain the existing principal admission controls. Resuming a paused job
allows the service to continue queued work without a second submission.

Only the implementation worker receives that exact message. Other pending
tasks for the same job remain separate assignments; a generic job event worker
does not drain them. The implementation uses the saved job's immutable profile,
stable transcript, permissions and lifetime budget. A separately configured
verification worker performs the existing host acceptance path.

Delivery and business status are independent:

| Field | Meaning |
| --- | --- |
| `state=acknowledged` | The recipient durably checkpointed the message. |
| `business.state=accepted` | One service owns the queued assignment. |
| `business.state=running` | The graph is executing the assigned work. |
| `business.state=blocked/failed/rejected` | Work has no successful business result; inspect its recorded cause. |
| `business.state=completed` | The exact message is acknowledged, implementation finished, and its designated verifier passed trusted host checks. |
| `business.state=cancelled` | The owning project was cancelled; the service does not dispatch replacement work. |

A completed receipt includes output and artifact hashes and host acceptance
evidence. Receipt projection follows the graph revision and its durable explicit
recovery marker. A later stale worker cannot overwrite the selected assignment
or completed result. Fixing a failed artifact/check still requires explicit graph
recovery; receipt projection does not silently erase attempts, budgets or unknown
external outcomes.

`MessageTeammate` sends the same structured task using its existing `task`
argument, now including `deliverables`. The sender can query
`{"operation":"status","message_id":"<id>"}` from the same sender session to
read a bounded business result. That query is read-only and does not create an
operation reservation or replay a previous status. Another sender scope cannot
inspect the message by guessing its ID. The model-facing result excludes
internal claim credentials and host commands.

HTTP events may include `job_id` to choose an allowlisted saved identity. The
service obtains its binding from the host store; an HTTP caller cannot replace
the job's permissions, provider or budget. Identical event retries preserve the
same assignment; a different job or payload under the same event ID conflicts.

Graph storage uses schema 5 and mailbox records use schema 3. Stop old writers
before upgrading. Existing records are read compatibly; old binaries must not
rewrite the new binding or receipt fields.

Queued tasks that cannot enter the graph have a separate `admission` record.
It reports a stable waiting reason, last check time, and recovery hint without
claiming that the business task has failed. Reason changes are saved immediately;
unchanged reasons are refreshed at most once per minute. Successful admission
clears the waiting record. The Desktop Jobs page shows this separately from
delivery acknowledgement and the final business receipt.

`scripts/e2e/job_handoff_dispatch_live_check.py` defines the real-query acceptance
cases. Its offline self-test validates the oracles only. A successful report
must come from a freshly compiled CLI and actual provider/tool evidence, followed
by destruction of both passing and failing test CLIs and private credentials.
