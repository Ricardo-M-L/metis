package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

// TestTableShrinksToContent verifies that a small table doesn't get
// stretched to the full terminal width (image #11 user feedback —
// short tables were padded to 200 cells and looked airy/empty).
func TestTableShrinksToContent(t *testing.T) {
	mdRendererMu.Lock()
	mdRendererNarrow = nil
	mdRendererWide = nil
	mdRendererMu.Unlock()

	content := "| a | b | c |\n|---|---|---|\n| 1 | 2 | 3 |\n"
	out := renderAssistantBody(content, 200)

	// Find the widest line of the rendered table.
	maxW := 0
	for _, ln := range strings.Split(out, "\n") {
		w := ansi.StringWidth(stripANSI(ln))
		if w > maxW {
			maxW = w
		}
	}
	// 3 columns, each cell content 1 cell wide, 2 padding cells each
	// column, plus 4 vertical borders. Expect well under 50 cells; we
	// give some slack for header indentation that renderAssistantBody
	// adds upstream.
	if maxW > 60 {
		t.Fatalf("small table inflated to %d cells (terminal=200); should shrink to content. output:\n%s",
			maxW, out)
	}
	if maxW < 10 {
		t.Fatalf("table collapsed to %d cells (something else broke):\n%s", maxW, out)
	}
}

// TestTableCapsAtTerminalWidth verifies the other direction: a table
// whose natural width exceeds the terminal is capped at the terminal
// width (lipgloss will wrap cell contents inside).
func TestTableCapsAtTerminalWidth(t *testing.T) {
	mdRendererMu.Lock()
	mdRendererNarrow = nil
	mdRendererWide = nil
	mdRendererMu.Unlock()

	// A 3-column row whose cells are each ~80 cells of CJK — natural
	// width far above the terminal cap.
	longCJK := strings.Repeat("中", 80)
	content := "| 维度 | a | b |\n|------|---|---|\n| " + longCJK + " | " + longCJK + " | " + longCJK + " |\n"
	out := renderAssistantBody(content, 100)

	maxW := 0
	for _, ln := range strings.Split(out, "\n") {
		w := ansi.StringWidth(stripANSI(ln))
		if w > maxW {
			maxW = w
		}
	}
	if maxW > 100 {
		t.Fatalf("wide table exceeded terminal cap (100): widest line = %d cells. output:\n%s",
			maxW, out)
	}
}
