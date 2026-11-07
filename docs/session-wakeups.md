# Session wakeups

`ScheduleWakeup` is a continuation of the session that requested it, not an unattended cron command. Creation must establish a concrete session owner and a consumer; a persisted JSON job alone is not evidence of scheduling delivery.

## Required behavior

- A live CLI, terminal UI or Desktop session consumes its own due wakeups without requiring a separately launched `metis cron start` service.
- A fired prompt is appended as host-originated context and resumed with the owning session's history and current permission checks. It is not presented as a human submission or run through a bypass gate.
- Busy sessions queue wakeups between turns; they do not run concurrent provider loops. A workspace/session switch cannot deliver one session's prompt or result into another session.
- Completion, failure and cancellation have distinct durable receipts. Claimed/delivered work must not be reported as successful execution. The original session can inspect these receipts after reconnect/resume.
- When no live consumer exists, the limitation and overdue state are visible. Do not silently start an unattended daemon or invent tool pre-authorization.
- Old jobs without a verified origin cannot be automatically replayed into a guessed session. They must be diagnosed and explicitly rescheduled.
- Status distinguishes scheduled, overdue, running, succeeded and failed work. A known failed result is not still 'waiting'.

## Acceptance

Use deterministic-clock tests for due time and claim behavior, and actual entry-point tests for tool creation, due delivery, model continuation and persisted receipts. Cover busy queues, session changes, restart/resume, duplicate consumers, cancellation, provider failure and persistence failure.

A release smoke must invoke the installed `metis` command, request a wakeup through a real model query, wait for its due time without another human prompt, and verify a later provider response in the same saved session. Record the exact binary version/commit/hash, tool result/job identity, timestamps, exit code and success/failure receipt. A successful `CronCreate`/`ScheduleWakeup` call alone does not pass this smoke. Synthetic fixtures must not expose private user memory or credentials.

Cache retention varies by endpoint. Wakeup delays must be chosen for the actual task; no universal five-minute prompt-cache guarantee is implied.
