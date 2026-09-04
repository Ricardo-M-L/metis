package tui

// render_sticky_tasks.go — a compact, always-on "what is the model
// working on right now" strip pinned above the status bar. Adds the
// claude-code-style live todo display (image #1 user request
// 2026-05-17) that the existing Ctrl+T overlay lacked: the overlay
// only shows when the user holds Ctrl+T, the model's progress is
// invisible the rest of the time. This strip is a smaller-footprint
// version of renderTaskPanel that's persistently visible whenever
// the session has at least one tracked todo.
//
// Trade-off vs Ctrl+T overlay:
//   - Overlay = bordered box, full content per row, all rows. Right
//     when the user wants to inspect.
//   - Strip = no border, truncated content, at most a handful of
//     rows. Right for at-a-glance progress while the model runs.
//
// We deliberately keep the strip slim so it doesn't compete with
// the chat content for vertical screen space. A long todo list
// collapses into the most-relevant subset:
//   - all in_progress items (typically 0-1; the model puts the
//     current focus here)
//   - the next 3 pending items (lookahead — "what's coming")
//   - a "+N done" rollup line replacing completed history so they
//     don't push the active row off-screen
//
// Data source is the same on-disk JSON the Ctrl+T overlay reads:
// ~/.metis/tasks/<sid>.json, populated by the TodoWrite tool. Every
// TodoWrite call updates the file → the next View() rebuild picks
// up the new state → the strip refreshes. No subscription needed;
// metis already re-renders on every Update message, which is
// triggered by the tool result event the model sees.

import (
	"fmt"
	"strings"

	"charm.land/lipgloss/v2"
)

// stickyMaxPending caps how many pending todos we show after the
// in-progress ones. Looking 3 items ahead is usually enough for the
// user to see where the model is going without the strip eating
// half the chat surface.
const stickyMaxPending = 3

// stickyMaxCompleted bounds how many DONE rows the sticky bar shows
// inline before collapsing the rest into a "+N more done" trailer.
// 5 keeps the strip visible on a typical 24-row terminal even when
// every cap is hit (3 in-progress + 3 pending + 5 completed + 1
// trailer + label = 13 rows worst case, leaves room for chat). User
// feedback 2026-05-24 (image 63): the old "+N done" roll-up hid
// what specifically was done; inline strikethrough rows mirror
// claude-code's behaviour.
const stickyMaxCompleted = 5

// stickyContentWidth — character budget for a todo's content text
// in the strip. Renderer truncates and adds an ellipsis if longer.
// Held below renderTaskPanel's 40 so the strip stays visually
// distinct (it's intentionally less detailed than the overlay).
const stickyContentWidth = 60

// renderStickyTaskStrip draws the always-on live todo summary that
// sits between the input hints and the status bar. Empty string
// when there's nothing to show (no todos, OR the Ctrl+T overlay is
// already up — no point doubling).
//
// Output shape (3 in-progress + 2 pending + 5 completed example):
//
//	tasks  ◼ Refactor auth gate · ◼ Wire MFA fallback
//	       ◻ Add session timeout tests
//	       ◻ Update docs
//	       ✔ 5 done
//
// Single-line label "  tasks " prefixes the first row so the eye
// can chunk it as a unit; subsequent rows are aligned with leading
// whitespace to match.
func renderStickyTaskStrip(m *Model) string {
	// Don't double-render when the user has the Ctrl+T overlay up;
	// that overlay carries the full list already.
	if m.showTaskPanel {
		return ""
	}
	tasks := tasksFullList(m.sessionID)
	if len(tasks) == 0 {
		return ""
	}

	// Partition by status. Order preserved within each bucket so the
	// model's natural ordering survives (newest task last typically).
	var inProgress, pending, completed []TaskItem
	for _, t := range tasks {
		switch t.Status {
		case "in_progress":
			inProgress = append(inProgress, t)
		case "pending":
			pending = append(pending, t)
		case "completed":
			completed = append(completed, t)
		}
	}

	if len(inProgress)+len(pending)+len(completed) == 0 {
		return ""
	}

	// Cap pending lookahead — too many trailing items push the
	// active row off-screen.
	if len(pending) > stickyMaxPending {
		pending = pending[:stickyMaxPending]
	}
	// 2026-05-24: switched from "+N done" roll-up to claude-code-style
	// inline strikethrough on the actual completed task contents.
	// User feedback "得像 claude code 一样在原来的任务上画横线变灰"
	// (image 63). Keep a screen-safety cap so a 40-task session doesn't
	// flood the chat — show the most recent stickyMaxCompleted and
	// roll any further excess into a small "+N more done" trailer.
	completedTrailer := 0
	if len(completed) > stickyMaxCompleted {
		completedTrailer = len(completed) - stickyMaxCompleted
		// Most-recent-N is the tail of the list (TodoList iteration
		// order is insertion order; completed ones get pushed to the
		// end when status flips).
		completed = completed[len(completed)-stickyMaxCompleted:]
	}

	inProgStyle := lipgloss.NewStyle().Foreground(accentOrange).Bold(true)
	pendingStyle := lipgloss.NewStyle().Foreground(textSecondary)
	completedStyle := lipgloss.NewStyle().Foreground(accentGreen)
	contentInProg := lipgloss.NewStyle().Foreground(textPrimary).Bold(true)
	contentPending := styleText
	// Completed content: dim + strikethrough — claude-code parity.
	// textSecondary keeps the row readable but visually "done"; the
	// strikethrough is the unambiguous "finished" signal.
	contentCompleted := lipgloss.NewStyle().Foreground(textSecondary).Strikethrough(true)
	label := styleMuted.Render("  tasks ")

	var s strings.Builder

	firstLineWritten := false
	writeRow := func(prefix string, body string) {
		if !firstLineWritten {
			s.WriteString(label)
			firstLineWritten = true
		} else {
			s.WriteString("        ") // align under the "  tasks " prefix width
		}
		s.WriteString(prefix)
		s.WriteString(body)
		s.WriteString("\n")
	}

	for _, t := range inProgress {
		writeRow(
			inProgStyle.Render(glyphTaskInProgress+" "),
			contentInProg.Render(truncateTodoContent(taskItemLabel(t))),
		)
	}
	for _, t := range pending {
		writeRow(
			pendingStyle.Render(glyphTaskPending+" "),
			contentPending.Render(truncateTodoContent(taskItemLabel(t))),
		)
	}
	// Completed: claude-code-style inline rows with strikethrough on
	// the original content. Capped above to stickyMaxCompleted; any
	// excess collapses into a "+N more done" trailer.
	for _, t := range completed {
		writeRow(
			completedStyle.Render(glyphTaskCompleted+" "),
			contentCompleted.Render(truncateTodoContent(taskItemLabel(t))),
		)
	}
	if completedTrailer > 0 {
		writeRow(
			completedStyle.Render(glyphTaskCompleted+" "),
			styleMuted.Render(fmt.Sprintf("+%d more done (use /todos for full list)", completedTrailer)),
		)
	}

	styled := s.String()
	// 2026-05-24: cache the strip's plain (ANSI-stripped) per-line
	// text on the Model so MouseClickMsg / MouseReleaseMsg can derive
	// the clipboard payload for drag-selected rows. The trailing
	// newline (writeRow always appends "\n") becomes a stray empty
	// line after Split — trim before storing so line-index math
	// matches the visible row count.
	plainBody := strings.TrimRight(ansiCSIInTUI.ReplaceAllString(styled, ""), "\n")
	m.stripPlainLines = strings.Split(plainBody, "\n")

	// Apply selection overlay on top of the styled output: each row
	// whose line index falls inside [stripSelStart, stripSelEnd]
	// gets wrapped in inverse-video (`\x1b[7m...\x1b[27m`). Inverse
	// video swaps fg/bg at the terminal level so it composes
	// cleanly with the per-cell foreground/strikethrough styling the
	// row itself already carries — no need to rebuild the row with
	// a fresh bg color attribute.
	if m.stripSelStart >= 0 && m.stripSelEnd >= 0 {
		styled = applyStripSelectionOverlay(styled, m.stripSelStart, m.stripSelEnd)
	}
	return styled
}

// applyStripSelectionOverlay wraps each line in selectedRange with
// inverse-video ANSI so the user sees their drag-selected rows
// highlighted. selStart/selEnd are line indices (0-based) — order-
// agnostic (drag can go either direction). Out-of-range indices are
// clamped so a partial drag past the end doesn't blow up.
func applyStripSelectionOverlay(styled string, selStart, selEnd int) string {
	lines := strings.Split(strings.TrimRight(styled, "\n"), "\n")
	lo, hi := selStart, selEnd
	if lo > hi {
		lo, hi = hi, lo
	}
	if lo < 0 {
		lo = 0
	}
	if hi >= len(lines) {
		hi = len(lines) - 1
	}
	for i := lo; i <= hi; i++ {
		lines[i] = "\x1b[7m" + lines[i] + "\x1b[27m"
	}
	return strings.Join(lines, "\n") + "\n"
}

// truncateTodoContent shortens a todo's free-text content to the
// strip's width budget, suffixing an ellipsis when cut. Rune-safe
// so CJK and emoji boundaries don't split — claude-code-go round-4
// found a UTF-8 truncation bug in a sibling renderer; preempt the
// same shape here.
func truncateTodoContent(content string) string {
	runes := []rune(content)
	if len(runes) <= stickyContentWidth {
		return content
	}
	return string(runes[:stickyContentWidth-1]) + "…"
}
