package tui

// queue.go — Model.queuedPrompts shape + drain semantics.
//
// 2026-05-21 — extracted from the bare []string used since the
// queued-messages feature landed (image #21 trail). The bare-string
// queue was strict FIFO with no batching, so a user who typed three
// short follow-ups while metis was busy got three round-trips back
// (one assistant turn per dequeue) — wasted prompt-cache, wasted
// latency, wasted tokens. claude-code's `messageQueueManager.ts`
// solved the same problem with priority + same-mode batching:
//
//   - Priority Now > Next > Later (Now pops first regardless of
//     arrival order; Later is for background notifications).
//   - Default Next.
//   - dequeue picks the highest-priority bucket and drains EVERY
//     adjacent item in that bucket into one merged user message.
//
// metis adopts the same shape. v1 only emits Priority=Next (every
// keybind_submit Enter), so the priority dimension is dormant — but
// the schema is in place so a future `/now <text>` slash command
// or a background-task notification can opt into Now/Later without
// a struct migration.

import (
	"strings"
)

// QueuePriority enumerates the three priorities. Smaller value =
// higher priority (dequeue picks the min). 0 is RESERVED as
// "uninitialised" — `effective()` promotes it to Next so a fixture
// that just sets `queuedItem{Text: "x"}` doesn't accidentally
// inherit the highest priority.
type QueuePriority uint8

const (
	_ QueuePriority = iota
	// QueuePriorityNow pops first regardless of arrival order.
	// Reserved for explicit "skip the queue" paths (no UI today;
	// the field is in place so a future `/now <text>` slash
	// command or a critical system event can opt in).
	QueuePriorityNow
	// QueuePriorityNext is the default for user-typed mid-turn input.
	// Created via enqueueQueuedItem(text, QueuePriorityNext) in the
	// keybind_submit hot path.
	QueuePriorityNext
	// QueuePriorityLater is for low-priority background events
	// (cron tick, task-done notifications). Drained after every
	// Now/Next item has been processed.
	QueuePriorityLater
)

// effective returns the priority used for ordering. Maps the
// uninitialised zero value to QueuePriorityNext so naked
// `queuedItem{Text: "x"}` literals (mostly tests + future programmatic
// enqueue callers) get the user-default behaviour instead of
// inadvertently jumping the queue. Production code goes through
// enqueueQueuedItem which sets Priority explicitly, so this guard
// only matters for fixtures.
func (p QueuePriority) effective() QueuePriority {
	if p == 0 {
		return QueuePriorityNext
	}
	return p
}

// queuedItem is one queued mid-turn input. Text holds the raw user
// input verbatim (already trimmed by submit handler). Priority steers
// dequeue order; QueuedAt is a monotonic seq from Model.queueClock,
// used as a deterministic tiebreaker within a priority bucket.
type queuedItem struct {
	Text     string
	Priority QueuePriority
	QueuedAt uint64
}

// enqueueQueuedItem appends one item to the model's queue, stamping
// QueuedAt from the monotonic clock. Centralised here so the clock
// can't be skipped by a caller that forgets to increment.
func (m *Model) enqueueQueuedItem(text string, prio QueuePriority) {
	m.queueClock++
	m.queuedPrompts = append(m.queuedPrompts, queuedItem{
		Text:     text,
		Priority: prio,
		QueuedAt: m.queueClock,
	})
}

// drainNextQueuedBatch removes and returns the next group of items
// the loop should submit as one user message. The group is:
//
//   - the highest-priority bucket present in the queue, and
//   - every item of that priority, in arrival order,
//   - SUBJECT TO: slash commands are drained one at a time. A
//     slash command can't sensibly be merged into a multi-line
//     user message — the dispatcher matches "/<name>" at the
//     start of the input, so "/tasks\n\n/skills" parses as ONE
//     malformed call. claude-code makes the same carve-out
//     (queueProcessor.ts: "Slash commands and bash-mode commands
//     are processed individually"). 2026-05-21 image #33 repro:
//     user queued 3 slashes mid-turn and watched them merge into
//     gibberish.
//
// Returns (joinedText, count) — count==0 means the queue is empty
// and the caller should skip the dequeue path. Multi-item batches
// join with a blank line separator so the model can tell the
// messages apart (one paragraph per follow-up). Single items return
// as-is so the common case stays identical to the pre-batching
// behaviour.
//
// Mutates m.queuedPrompts — caller doesn't need to slice.
func (m *Model) drainNextQueuedBatch() (string, int) {
	if len(m.queuedPrompts) == 0 {
		return "", 0
	}
	// Find the highest-priority bucket (min effective Priority).
	best := m.queuedPrompts[0].Priority.effective()
	for _, it := range m.queuedPrompts[1:] {
		if ep := it.Priority.effective(); ep < best {
			best = ep
		}
	}
	// Slash carve-out: if the head of the chosen bucket is a slash
	// command, pop only that single item. Subsequent ticks will
	// drain the rest one-by-one so each slash hits the dispatcher
	// cleanly. We check the FIRST in-bucket item rather than scan
	// the whole bucket because mid-turn slashes typically arrive
	// in clusters of one (Ctrl+L, /tasks, /skills are not usually
	// followed by plain text in the same priority bucket); if it
	// turns out users do mix slash + text within Now/Next, the
	// fix is to split here on a per-item basis instead.
	for i, it := range m.queuedPrompts {
		if it.Priority.effective() != best {
			continue
		}
		if isSlashItem(it.Text) {
			// Drop just this one item, leave the rest in queue.
			text := it.Text
			m.queuedPrompts = append(m.queuedPrompts[:i], m.queuedPrompts[i+1:]...)
			return text, 1
		}
		break // first in-bucket item isn't a slash → fall through to batch
	}
	// Partition: keep `kept` for items not in this batch, collect
	// `batch` in arrival order. We iterate over the existing slice
	// once — no need for a separate sort because QueuedAt is
	// monotonically increasing as items were appended.
	batch := make([]string, 0, len(m.queuedPrompts))
	kept := m.queuedPrompts[:0]
	for _, it := range m.queuedPrompts {
		// Belt-and-suspenders: any slash that snuck past the
		// first-item check (e.g. a Next-priority slash sandwiched
		// between two Next plain-texts — rare but possible if a
		// user mixes) stays in the queue for next tick.
		if it.Priority.effective() == best && !isSlashItem(it.Text) {
			batch = append(batch, it.Text)
		} else {
			kept = append(kept, it)
		}
	}
	m.queuedPrompts = kept
	if len(batch) == 1 {
		return batch[0], 1
	}
	if len(batch) == 0 {
		// All same-priority items were slashes that we deferred.
		// Reuse the head-pop path by recursing once on the (now
		// reduced) queue.
		return m.drainNextQueuedBatch()
	}
	// Blank-line join: matches the way a user would paste-then-
	// continue in one message. The model sees N follow-ups as one
	// coherent block instead of N separate turns.
	return strings.Join(batch, "\n\n"), len(batch)
}

// isSlashItem reports whether a queued item is a slash command —
// recognised by the leading "/" after stripping leading whitespace.
// Used by the drain logic to skip the batching path for slashes.
func isSlashItem(s string) bool {
	t := strings.TrimLeft(s, " \t")
	return len(t) > 1 && t[0] == '/'
}

// parsePriorityCommand recognises the two UI shortcuts for setting
// a non-default queue priority:
//
//	/now    <text>  → QueuePriorityNow
//	/later  <text>  → QueuePriorityLater
//
// Returns (priority, body-after-the-command, true) when matched,
// (_, "", false) when the input is anything else. Tolerant of any
// whitespace between the command and the body (handles a trailing
// space-only line by returning body="").
//
// Deliberately NOT registered as full REPLCommand entries: those
// would dispatch via the slash registry which has its own mid-turn
// classification; we want this path to fire BEFORE the registry so
// it can never be shadowed by a user-defined command in
// ~/.metis/commands/ accidentally named "now" or "later".
func parsePriorityCommand(raw string) (QueuePriority, string, bool) {
	const (
		prefixNow   = "/now"
		prefixLater = "/later"
	)
	switch {
	case raw == prefixNow:
		return QueuePriorityNow, "", true
	case len(raw) > len(prefixNow) && raw[:len(prefixNow)] == prefixNow && isSpace(raw[len(prefixNow)]):
		return QueuePriorityNow, strings.TrimSpace(raw[len(prefixNow):]), true
	case raw == prefixLater:
		return QueuePriorityLater, "", true
	case len(raw) > len(prefixLater) && raw[:len(prefixLater)] == prefixLater && isSpace(raw[len(prefixLater)]):
		return QueuePriorityLater, strings.TrimSpace(raw[len(prefixLater):]), true
	}
	return 0, "", false
}

// prioCommandName returns the user-facing slash name for a priority.
// Only the two UI-bound priorities have names — Next is implicit
// (the default for plain text). Used in error messages so the hint
// text matches whatever the user typed.
func prioCommandName(p QueuePriority) string {
	switch p {
	case QueuePriorityNow:
		return "/now"
	case QueuePriorityLater:
		return "/later"
	}
	return ""
}

func isSpace(b byte) bool { return b == ' ' || b == '\t' }
