package tui

import (
	"time"

	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/tui/screen"
)

// applyScreenResult bridges Phase C interactive widgets to the live
// agent loop. Called from tui_update.go when an activeScreen reports
// Done(); type-asserts to each widget and applies any committed result
// before the parent clears the screen reference.
//
// Cancellation is screen-internal: each widget exposes a Result-style
// method that returns the empty string (or other zero value) when the
// user pressed Esc, so the apply step naturally no-ops.
func (m *Model) applyScreenResult(s screen.Screen) {
	switch w := s.(type) {
	case *screen.EffortScreen:
		applied := w.Applied()
		if applied == "" {
			return // user cancelled
		}
		// Map widget label → llm.Effort. "off" maps to EffortDefault
		// so the next request lets the provider decide.
		var e llm.Effort
		switch applied {
		case "off":
			e = llm.EffortDefault
		case "low":
			e = llm.EffortLow
		case "medium":
			e = llm.EffortMedium
		case "high":
			e = llm.EffortHigh
		default:
			return // unknown label — defensive, NewEffortScreen never produces this
		}
		m.loop.Effort = e
		// Mirror to the legacy package var so /effort (the inline
		// query path) keeps reading the right value.
		effortState = applied
		// Confirmation message uses the success role so the user gets
		// the green ✓ from Phase B.
		m.messages = append(m.messages, Message{
			Role:      "success",
			Content:   "effort: " + applied,
			Timestamp: time.Now(),
		})

	case *screen.ModelScreen:
		applied := w.Applied()
		if applied == "" {
			return // user cancelled
		}
		m.model = applied
		m.loop.Model = applied
		m.messages = append(m.messages, Message{
			Role:      "success",
			Content:   "model: " + applied,
			Timestamp: time.Now(),
		})
	}
}
