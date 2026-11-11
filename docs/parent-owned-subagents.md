# Parent-managed sub-agents

Sub-agents belong to the conversation that delegated them. Their Desktop pages
show the task, live progress, tools and result. They do not accept independent
user turns. Continue a child by asking its parent in the main conversation.

The parent calls `Agent` with the existing `resume_from` ID and a follow-up
`prompt`. Active children receive the follow-up at a loop boundary; completed
children resume the same transcript. A different parent cannot take over the
child. Direct CLI main-session resume and Desktop main-turn submission reject
parent-managed child IDs.

New child transcripts retain their provider and model identity, reasoning
effort, working directory, system/profile instructions, permission mode and
effective tool restrictions without storing provider credentials. Resume uses
the currently configured credentials for that saved provider. It must report
missing or incompatible execution settings instead of silently using the
parent's new defaults. Plan-mode and current parent restrictions remain
authoritative.

Ordinary legacy records without an execution descriptor cannot establish their
original role and tool limits and require a new child. Existing durable mailbox
records have a compatibility migration with an explicit warning: it checks the
known header identity and uses the current parent's tool boundary, rather than
claiming to reconstruct missing historical profile restrictions.

The original workspace must still exist. An isolated worktree that was removed
by the existing end-of-task cleanup cannot be cold-resumed in its old directory;
continuation reports that failure instead of choosing another directory.

The transcript is appended under a producer lease. A child has one writer, so
concurrent continuation cannot start two producers or replace its recorded
history. A successful live-delivery receipt acknowledges durable input; task
completion is reported separately by the child lifecycle and output.

## Output budgets and interrupted answers

`Agent.max_output_tokens` optionally lowers the child's per-response output
budget. It does not increase the inherited provider budget, change the parent's
client, or replace the model's output capability. An explicit provider or model
budget remains authoritative when an answer is continued.

A child that reaches an output-length limit with a plain-text answer may append
at most three text-only continuations in the same transcript. They retain the
child's original deadline, cancellation, usage accounting and iteration limits.
Continuation does not rerun completed tools. Truncated tool calls, protocol
errors, filtering and other failures do not enter this automatic path; a
continuation cannot execute tools or recover tools from text. No progress or an
exhausted recovery allowance leaves an incomplete result rather than inventing
successful completion.

The parent receives the stop reason, a diagnostic and any partial answer as
separate information. The stored terminal state preserves the reason and
continuation count. Desktop distinguishes a failure to start from an answer
that started but could not finish, and exposes the saved partial result.

## Inspecting saved children

`SubAgentList` and `SubAgentOutput` can read saved child records after the live
roster has been cleared or the parent process has restarted. Reading a saved
record does not resume or execute it. Both live and saved lookup remain scoped
to the direct parent conversation; an agent ID from another parent cannot be
used to read its output. Repeated names require an explicit agent ID.

Queries are bounded and paginated. A truncated scan or unreadable history is
reported explicitly, and an unloaded record without a terminal state is not
proof that the child completed. CLI summaries count child rows separately from
pagination and diagnostic metadata.
