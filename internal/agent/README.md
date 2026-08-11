# internal/agent

The conversation execution engine. This package owns `Loop`, the event
stream emitted by a run, tool dispatch, compaction, loop/progress detectors,
sub-agent coordination, plan state, background notifications, and the
large-run verification contract. `loop.go` is the control-flow entry point;
the surrounding files keep each policy concern separate from the loop body.

## Browse by concern

| Concern | Main files |
|---|---|
| Loop lifecycle and events | `loop.go`, `loop_skill.go`, `event.go`, `streaming.go` |
| Tool permission and dispatch | `dispatch.go`, `permission_ask.go`, `turn_permissions.go` |
| Context pressure and compaction | `compact.go`, `compact_tier.go`, `compact_images.go`, `compaction_check.go` |
| Dream and automatic memory | `dream_gates.go`, `dream_registry.go`, `dream_notify.go`, `auto_memory.go`, `auto_mem_gate.go` |
| Loop/stuck/progress detection | `loopdetection.go`, `stuck_detector.go`, `progress_detector.go` |
| Verification contract | `contract.go` |
| Sub-agents and collaboration | `fork.go`, `sub_prompt.go`, `subagent_transcript.go`, `teammates.go`, `tasks.go`, `peer_notify.go` |
| Background and scheduled work | `monitor.go`, `monitor_inject.go`, `job_notify.go`, `cron*.go` |
| Plan mode | `plan.go`, `plan_emit.go`, `plan_trigger.go` |
| Tool-result hygiene | `orphan_repair.go`, `touched_files.go`, `turn_diffs.go` |

## Sub-packages

| Path | Purpose |
|---|---|
| `skills/` | Multi-source skill loading, manifests, installation, safety scanning, usage, curation, and synthesis support |
| `transcript/` | Pure in-memory transcript helpers such as undo, snapshot, visible-user-text, and turn counting; session JSONL persistence lives outside this sub-package |

## Where do I find...

- **The main loop** → `loop.go` (`func (l *Loop) Run`)
- **Tool dispatch and concurrency** → `dispatch.go`
- **Loop, stuck, and diminishing-return detection** → `loopdetection.go`,
  `stuck_detector.go`, and `progress_detector.go`
- **Context compaction** → `compact.go`, `compact_tier.go`, and
  `compaction_check.go`
- **The verify-subagent gate** → `contract.go`
- **Automatic memory extraction** → `dream_*.go` and `auto_*.go`
- **Sub-agent prompts and transcripts** → `sub_prompt.go` and
  `subagent_transcript.go`
- **OpenAI-strict tool-result adjacency repair** → `orphan_repair.go` and
  `contract_reminder_adjacency_test.go`

## Dispatch and permission semantics

`Loop.Run` advances conversation state serially, but a tool batch is not
single-threaded. `executeBatch` first runs the pre-tool hook, resolves
`CanUse`, and handles all `PermissionAsk` decisions as a batch. It then calls
`Tool.Concurrency(input)` and executes four tiers:

- `Safe`: fan out concurrently.
- `Queue`: run FIFO in one worker, concurrently with the safe fan-out.
- `Background`: obtain the tool's immediate handshake while the tool/job
  subsystem owns the long-running lifecycle and later notification.
- `Exclusive`: preserve request order and run only after safe and queued work
  has completed.

The result slice remains in the model's original tool-call order regardless
of completion order. See `dispatch.go` and `pkg/tool/tool.go` before changing
these guarantees.

## Other design invariants

- The verification contract applies only after substantial work: at least
  five direct `Write`/`Edit`/`MultiEdit` calls or ten `Agent` dispatches.
  `METIS_CONTRACT_DISABLE=1` disables it, `OVERRIDE CONTRACT:` is the audited
  escape hatch, and each dispatch/verdict gate is capped at two attempts.
- A verify result is parsed from the last `VERDICT: PASS|FAIL|PARTIAL` marker.
  A missing marker becomes `MISSING`; for a triggered contract, only `PASS`
  releases the verdict gate before its retry cap. The required verifier
  prompt is `internal/runtime/builtin_profiles/verify.md`.
- The CLI classifies `diminishing_returns`, `max_iterations`,
  `loop_detected`, `stuck_after_reset`, `turn_wall_clock`, and `budget_usd`
  as incomplete outcomes and maps them to `exitcode.Incomplete`. When adding
  a stop reason, make its CLI classification an explicit decision.
