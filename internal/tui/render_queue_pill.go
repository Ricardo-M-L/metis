package tui

// render_queue_pill.go — sticky indicator for queued prompts (task
// #37). Appears above the input box whenever m.queuedPrompts is
// non-empty. Style: gray italic with a "queued × N" prefix and the
// peek of the head item, truncated to fit the row.
//
// Why above the input (not in the status bar): the user's next gesture
// is typing or hitting Enter, both of which involve looking at the
// input. Putting the pill adjacent to where their eye is keeps the
// indication noticeable without demanding scrolling.

import (
	"fmt"
	"strings"

	"charm.land/lipgloss/v2"
)

// renderQueuePill formats the queue indicator. Empty string when no
// items are queued so the chrome stays minimal in the common case.
func renderQueuePill(m *Model) string {
	if m == nil || len(m.queuedPrompts) == 0 {
		return ""
	}
	dim := lipgloss.NewStyle().Foreground(textMuted).Italic(true)
	accent := lipgloss.NewStyle().Foreground(accentBlue)

	prefix := fmt.Sprintf("queued × %d", len(m.queuedPrompts))
	peek := strings.TrimSpace(m.queuedPrompts[0].Text)
	if peek == "" {
		// Defensive — empty strings shouldn't reach the queue but
		// some programmatic path could push one. Skip the peek.
		return "  " + accent.Render("◷ ") + dim.Render(prefix) + "\n"
	}
	// Truncate to roughly the input line width so a long queued
	// prompt doesn't bleed into the next row.
	const maxPeek = 60
	if w := m.width - 18; w > 12 && w < maxPeek {
		peek = truncatePeek(peek, w)
	} else {
		peek = truncatePeek(peek, maxPeek)
	}
	return "  " + accent.Render("◷ ") +
		dim.Render(prefix+": "+peek) + "\n"
}

// truncatePeek shortens `s` to `n` runes with an ellipsis. Counts
// runes so a CJK paste doesn't get sliced mid-codepoint.
func truncatePeek(s string, n int) string {
	if n <= 1 {
		return ""
	}
	rs := []rune(s)
	if len(rs) <= n {
		return s
	}
	return string(rs[:n-1]) + "…"
}
