package agent

// dream_notify.go — drains DreamTask completion notifications from the
// auto-memory extractor at iteration boundaries and synthesizes a
// <memory_consolidation_done> system-reminder user message so the
// model knows long-term memory has been refreshed since its last
// reply (G.5, 2026-05-12). Mirrors job_notify.go / peer_notify.go.
//
// Why a synthetic system-reminder: the LLM only "sees" what's in the
// prompt; hook events are a side channel for tooling. A user-message
// envelope tells the model "this isn't a real user reply, it's a
// runtime fact you should incorporate next turn." Matches the
// <memory_consolidation_done> envelope pattern used by claude-code's
// DreamTask.ts.

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// injectDreamNotifications drains every pending DreamNotification on
// l.DreamNotify and appends one synthetic user message summarising
// the batch. Multiple completions within the same iter window
// collapse into a single envelope to keep prompt noise down.
//
// No-op when DreamNotify is nil (sub-agents, headless tests, auto-
// memory disabled) or empty.
func (l *Loop) injectDreamNotifications(out chan<- Event) {
	if l.DreamNotify == nil {
		return
	}
	notifs := drainChan(l.DreamNotify)
	if len(notifs) == 0 {
		return
	}
	l.appendInjectedMessage(formatDreamNotifications(notifs))
	// Mirror surface to the TUI so the user sees the same "memory
	// updated" banner the model is reacting to.
	emit(context.Background(), out, Event{
		Kind: EventInfo,
		Info: fmt.Sprintf("[auto-memory] %d consolidation(s) completed", len(notifs)),
	})
}

// formatDreamNotifications builds the <memory_consolidation_done>
// system-reminder. Lines are intentionally terse — the model just
// needs to know "memory was refreshed and these files changed" so it
// can adjust subsequent responses.
func formatDreamNotifications(notifs []DreamNotification) string {
	var b strings.Builder
	b.WriteString("<memory_consolidation_done>\n")
	if len(notifs) == 1 {
		b.WriteString("Long-term memory was just refreshed:\n")
	} else {
		fmt.Fprintf(&b, "Long-term memory was refreshed %d times:\n", len(notifs))
	}
	for _, n := range notifs {
		if n.Err != nil {
			fmt.Fprintf(&b, "  • [failed] %s after %s\n", n.Err, n.Duration.Truncate(time.Millisecond))
			continue
		}
		if len(n.FilesTouched) == 0 {
			fmt.Fprintf(&b, "  • no files changed (scanned, nothing new to save) — %s\n",
				n.Duration.Truncate(time.Millisecond))
			continue
		}
		fmt.Fprintf(&b, "  • files touched: %s (in %s, total this session: %d)\n",
			strings.Join(n.FilesTouched, ", "),
			n.Duration.Truncate(time.Millisecond),
			n.SessionCount,
		)
	}
	b.WriteString("These notes are already in your system prompt for the next turn. Use the refreshed context naturally; do not summarize what was saved unless the user asks.\n")
	b.WriteString("</memory_consolidation_done>")
	return b.String()
}
