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
