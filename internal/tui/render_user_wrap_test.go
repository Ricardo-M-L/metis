package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

// TestRenderMessage_UserShortLineRendersOneRow — sanity baseline:
// a short prompt should still render as a single body row (no wrap).
func TestRenderMessage_UserShortLineRendersOneRow(t *testing.T) {
	msg := Message{Role: "user", Content: "hello world"}
	out := renderMessage(msg, 80, false)
	plain := ansi.Strip(out)
	body := strings.TrimSpace(plain)
	rows := strings.Split(body, "\n")
	if len(rows) != 1 {
		t.Errorf("short prompt should render as 1 body row; got %d: %q", len(rows), rows)
	}
	if !strings.Contains(rows[0], "hello world") {
		t.Errorf("body missing content: %q", rows[0])
	}
}

// TestRenderMessage_UserLongPathWrapsAtWidth — image #21 repro:
// a 200+ cell CJK + path prompt MUST wrap at the chat surface
// width instead of overflowing the right edge. Verifies (a) every
// visual row stays under the cell-width budget and (b) the prompt
// glyph appears on the FIRST row only.
func TestRenderMessage_UserLongPathWrapsAtWidth(t *testing.T) {
	// 200+ cells: 16 CJK chars (32 cells) + 60-char ASCII path
	// repeated; reliably trips the wrap at any sensible width.
	prompt := "帮我看看/Users/ricardo/Documents/加和科技/goalfyhub前后端/goalfyhub-front/docs/GoalfyHub技术文档.md这个里面写的啥总结下"
	msg := Message{Role: "user", Content: prompt}
	width := 80 // tight on purpose — repro screenshot used ~220, but bug
	// reproduces at any width because the original code didn't
	// wrap at all.

	out := renderMessage(msg, width, false)
	plain := ansi.Strip(out)
	rows := strings.Split(strings.Trim(plain, "\n"), "\n")

	if len(rows) < 2 {
		t.Fatalf("expected >1 wrapped row for a 200+ cell prompt at width %d; got %d row(s): %q", width, len(rows), rows)
	}
	// Every row's measured cell width must fit inside the budget
	// (width - 4 = body, plus the 4-cell prefix on row 0 or 4-cell
	// indent on continuation rows). Allow +1 slack for trailing
	// soft-wrap padding.
	for i, row := range rows {
		if w := ansi.StringWidth(row); w > width+1 {
			t.Errorf("row %d width %d exceeds terminal width %d: %q", i, w, width, row)
		}
	}
	// Prompt glyph (❯) must appear EXACTLY on row 0 — continuation
	// rows should NOT repeat it (would look like multiple prompts).
	first := rows[0]
	if !strings.Contains(first, "❯") {
		t.Errorf("row 0 missing prompt glyph; got %q", first)
	}
	for i, row := range rows[1:] {
		if strings.Contains(row, "❯") {
			t.Errorf("continuation row %d shouldn't carry prompt glyph: %q", i+1, row)
		}
	}
}

// TestRenderMessage_UserWrapsAtSlashes — paths should prefer
// breaking at "/" so the wrapped rows are readable (each row ends
// at a path component boundary, not mid-name). xansi.Wrap honors
// the breakpoints arg we pass; this test pins that we DO pass
// slash as a breakpoint.
func TestRenderMessage_UserWrapsAtSlashes(t *testing.T) {
	prompt := "/Users/ricardo/Documents/加和科技/goalfyhub前后端/goalfyhub-front/docs/file.md"
	msg := Message{Role: "user", Content: prompt}
	out := renderMessage(msg, 30, false) // narrow to force multiple wraps
	plain := ansi.Strip(out)
	rows := strings.Split(strings.Trim(plain, "\n"), "\n")

	if len(rows) < 2 {
		t.Fatalf("expected >1 wrapped row at width 30; got %d: %q", len(rows), rows)
	}
	// At least one row should end with a "/"-delimited segment —
	// proves the breakpoint logic kicked in. We don't assert ALL
	// rows do (CJK can force hard-break in the middle), just that
	// the path-component split is happening somewhere.
	sawSlashBreak := false
	for _, row := range rows[:len(rows)-1] { // not the last row
		trimmed := strings.TrimRight(row, " ")
		if strings.HasSuffix(trimmed, "/") {
			sawSlashBreak = true
			break
		}
	}
	if !sawSlashBreak {
		t.Logf("rows: %q", rows)
		t.Errorf("expected at least one row to break at a '/' boundary")
	}
}
