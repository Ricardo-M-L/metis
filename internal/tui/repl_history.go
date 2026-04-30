package tui

import (
	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/tui/screen"
)

// renderHistoryForREPL renders the conversation transcript for the readline
// fallback REPL. Output goes straight to stdout — no scrolling, no overlay.
//
// We share the body renderer with the bubbletea HistoryScreen so the two
// surfaces don't visually drift over time. The screen's chrome (title,
// scroll hints) is stripped here because the REPL has no scroll affordance.
func renderHistoryForREPL(messages []llm.Message) string {
	if len(messages) == 0 {
		return "(history is empty)"
	}
	return screen.RenderHistoryBody(messages, 100)
}
