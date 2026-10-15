# Long-running task checkpoints

Ordinary CLI and Desktop tasks use `session.max_iterations` (100 by default)
as a progress checkpoint interval. Reaching the interval does not finish the
task or require a new user message. The same execution keeps its conversation,
completed tool results, pending work, permissions, and verification state.

A checkpoint is crossed only after completed tool results have been persisted.
Continuation does not replay the previous tool batch. Context compaction still
runs when the context window needs it; a checkpoint does not force an extra
model request to summarize a small conversation.

Cancellation, configured time and dollar budgets, repeated-tool detection,
stuck-task detection, and persistence failures remain effective across all
checkpoints. The iteration counter and usage statistics remain cumulative.

An explicit `--max-iter N` or an agent profile's `max_turns` keeps the existing
finite-budget behavior, including its bounded compaction/grace/summary handling.
Child agents also retain their own finite budgets. A configured checkpoint
interval is not permission to change those explicit limits.

When a finite budget or another local guard stops an unfinished task, the CLI
reports a task outcome with a recovery hint. It reserves `API Error` for provider
failures. A checkpoint is not a promise of crash-safe, exactly-once execution;
normal resume and interrupted-tool safeguards still apply after a process crash.
