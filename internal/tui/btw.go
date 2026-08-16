package tui

import (
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/Ricardo-M-L/metis/internal/tui/overlay"
)

// startBtwQuery is a thin bridge from the slash-command dispatcher to
// the overlay-stack-backed modal. We pre-2026-05-01, this function held
// 5+ btw* fields directly on Model; now it just constructs an Overlay
// and Pushes it.
//
// `question` is the user-typed text after `/btw `. Empty input is
// filtered out by the slash registry, but we double-check here.
func (m *Model) startBtwQuery(question string) tea.Cmd {
	question = strings.TrimSpace(question)
	if question == "" {
		return nil
	}
	o := overlay.NewBtwOverlay(m.ctx, question, m.ext.BtwAsk)
	cmd := m.overlays.Push(o)
	// Reset the input after pushing the overlay so the next Enter (e.g.
	// the user closes the modal with Esc and immediately presses Enter
	// again) does not re-invoke handleSubmit with the original "/btw …"
	// text still sitting in the textarea. Without this the user would
	// accidentally send "/btw …" as a regular user message instead of
	// opening the side-question again.
	m.input.Reset()
	return cmd
}
