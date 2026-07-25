# internal/agent

The agent execution loop. Owns `Loop` (the per-conversation state
machine), the dispatch contract, all detectors (loop / stuck /
progress), context compaction, dream/auto-memory, and the verify-
subagent gate. `loop.go` is the heart; everything else either feeds
data into `Loop.Run()` or consumes events out of it.

## File-naming convention (browse by prefix)

The package has ~50 production files (plus ~75 tests). They organise
by domain prefix:

| Prefix | Files | What it owns | Entry file |
|---|---|---|---|
| `loop*` | 3 | The agent loop core: `loop.go` (main `Run`), `loopdetection.go` (signature-based loop detection) | `loop.go` |
| `compact*` + `dream*` + `auto*` | 8 | Context compaction + dream / auto-memory extractor (consolidates session learning into archival memory across runs) | `compact.go`, `dream_runtime.go`, `auto_memory.go` |
| `dispatch*` | 1 | The 4-tier tool concurrency dispatcher (Safe / Queue / Background / Exclusive) called from inside `Loop.Run` | `dispatch.go` |
| `contract*` | 2 | Dispatch contract + verify-VERDICT gate (Phase B) — gates `end_turn` until verify dispatched + returned PASS | `contract.go` |
| `stuck*` + `progress*` | 3 | Stuck-loop detector (signature×4 OR no-green×8, 2-reset budget) + progress detector (diminishing returns) | `stuck_detector.go`, `progress_detector.go` |
| `cron*` | 2 | Recurring-prompt scheduler used by `metis cron` | `cron.go` |
| `lazy*` | 2 | Lazy-load wrappers for tools (LSP, memory) deferring init until first use | `lazy_tools.go` |
| `monitor*` | 2 | Background-task monitor; emits `<task_notification>` system reminders into next turn | `monitor.go` |
| `plan*` | 2 | Plan-mode bookkeeping (read-only restriction state) | `plan_mode_state.go` |
| singletons | ~25 | One-concept files: `event.go`, `subagent.go`, `teammates.go`, `tasks.go`, `streaming_args.go`, `memory.go`, `orphan_repair.go`, `permission_loop.go`, `peer_dispatch.go`, `nudge.go`, `touched_files.go`, `turn_diffs.go`, etc. |

## Sub-packages

| Path | Purpose |
|---|---|
| `skills/` | Skill loader, manifest parser, marketplace, safety filter, search index, synth-tool generator |
| `transcript/` | Per-run transcript persistence + JSONL load/save |

## Where do I find...

- **The main agent loop** → `loop.go` (`func (l *Loop) Run`)
- **Tool dispatch / concurrency** → `dispatch.go`
- **Detector chain** (loop / stuck / progress) → `loopdetection.go` + `stuck_detector.go` + `progress_detector.go`
- **Contract gate** (verify dispatched? verify returned PASS?) → `contract.go`
- **Context compaction** (token budget runs out) → `compact.go`, `compaction_check.go`
- **Memory extraction at end of run** (the "dream" phase) → `dream_runtime.go`, `auto_memory.go`
- **Tool-result invariants** (orphan tool_use repair, OpenAI-strict adjacency) → `orphan_repair.go`, `contract_reminder_adjacency_test.go`
- **Stop reasons** → `loop.go` `stopReason = "..."` assignments

## Design invariants

- `Loop.Run` is **single-threaded**: detectors / contract / dispatch all
  run sequentially per iter. Goroutines (e.g., subagent dispatch) emit
  results via channels back to the loop.
- Stop reasons in `{max_iterations, loop_detected,
  stuck_after_reset}` are **incomplete**: `cmd/metis/main.go` maps them
  to `exitcode.Incomplete = 11`. Don't add new stop reasons without
  deciding whether they're complete or incomplete.
- The verify-subagent contract REQUIRES a `VERDICT: PASS/FAIL/PARTIAL`
  trailing line — `contract.go::extractVerdict` parses for it; missing
  line is treated as MISSING and gates end. See
  `internal/runtime/builtin_profiles/verify.md` for the subagent
  prompt.
