package tui

import (
	"strings"

	tea "github.com/charmbracelet/bubbletea"

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
	return m.overlays.Push(o)
}
