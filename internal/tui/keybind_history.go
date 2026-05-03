package tui

// keybind_history.go — Ctrl+R fuzzy history search overlay.
//
// Mirrors shell `Ctrl+R`: opens an inline filter, types narrow the
// candidate list, ↑↓ select, Enter copies the chosen prompt back into
// the input box (you can edit before submitting), Esc cancels.
//
// Backed by ~/.metis/history.jsonl which `runtime.AppendHistory` writes
// every user submit — that file already exists, this overlay is the
// last-mile UI to actually surface it. Before this, history was only
// visible via `cat ~/.metis/history.jsonl | jq '.input'` from a
// separate terminal.

import (
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/Ricardo-M-L/metis/internal/runtime"
)

// openHistorySearch opens the overlay, lazy-loading history on first
// open. We dedup adjacent duplicates (typical when the user re-runs
// the same prompt several times in a row) so the candidate list
// surfaces unique prompts rather than 50 copies of "show me the diff".
func (m *Model) openHistorySearch() {
	if m.histAll == nil {
		entries, _ := runtime.LoadRecentHistory(500)
		seen := map[string]bool{}
		for _, e := range entries {
			if seen[e.Input] {
				continue
			}
			seen[e.Input] = true
			m.histAll = append(m.histAll, e.Input)
		}
	}
	m.showHistory = true
	m.histFilter = ""
	m.histCursor = 0
	m.histMatched = append([]string(nil), m.histAll...)
}

// closeHistorySearch resets all overlay state. Cursor reset to 0 so the
// next reopen starts fresh.
func (m *Model) closeHistorySearch() {
	m.showHistory = false
	m.histFilter = ""
	m.histCursor = 0
	m.histMatched = nil
}

// rebuildHistoryMatches re-applies the current histFilter as a simple
// case-insensitive substring filter. Cursor clamps to a valid row so
// hitting backspace doesn't leave the highlight off-screen.
//
// We use plain substring (not Levenshtein / fzf-style) because the
// dataset is small (typical user has <500 prompts) and substring is
// what `Ctrl+R` in bash does — least surprise.
func (m *Model) rebuildHistoryMatches() {
	q := strings.ToLower(m.histFilter)
	m.histMatched = m.histMatched[:0]
	if q == "" {
		m.histMatched = append(m.histMatched, m.histAll...)
	} else {
		for _, p := range m.histAll {
			if strings.Contains(strings.ToLower(p), q) {
				m.histMatched = append(m.histMatched, p)
			}
		}
	}
	if m.histCursor >= len(m.histMatched) {
		m.histCursor = len(m.histMatched) - 1
	}
	if m.histCursor < 0 {
		m.histCursor = 0
	}
}

// handleHistoryKey is the per-key dispatcher while the overlay is
// active. Returns (model, cmd) following the bubbletea convention.
//
// Key contract — kept tight on purpose, this overlay should feel like
// a focused search box, not a second editor:
//
//	Enter   → copy the highlighted entry into the main input box,
//	          close the overlay. User can hit Enter again to submit.
//	Esc     → cancel without modifying input.
//	↑/↓     → move highlight (clamped, no wraparound — claude-code parity).
//	Ctrl+C  → close overlay; the parent handler turns Ctrl+C into a
//	          quit if the input is empty, but in-overlay we treat it
//	          as cancel.
//	Backspace → trim filter.
//	any rune  → append to filter.
func (m *Model) handleHistoryKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	// v2: tea.KeyMsg is interface, .Type / .Runes gone. Match by String()
	// for named keys; fall through to "type a character into the filter"
	// when the press is a printable rune (String() == single rune) or
	// the literal "space" name.
	s := msg.String()
	switch s {
	case "esc", "ctrl+c":
		m.closeHistorySearch()
		return m, nil
	case "up":
		if m.histCursor > 0 {
			m.histCursor--
		}
		return m, nil
	case "down":
		if m.histCursor < len(m.histMatched)-1 {
			m.histCursor++
		}
		return m, nil
	case "enter":
		if m.histCursor < len(m.histMatched) {
			m.input.SetValue(m.histMatched[m.histCursor])
			m.input.CursorEnd()
		}
		m.closeHistorySearch()
		return m, nil
	case "backspace":
		if len(m.histFilter) > 0 {
			m.histFilter = m.histFilter[:len(m.histFilter)-1]
			m.rebuildHistoryMatches()
		}
		return m, nil
	case "space":
		m.histFilter += " "
		m.rebuildHistoryMatches()
		return m, nil
	}
	// Typed printable rune — String() returns the character itself.
	// Single-rune length filter avoids picking up multi-key sequences
	// like "ctrl+x" that we haven't explicitly named above.
	if len([]rune(s)) == 1 {
		m.histFilter += s
		m.rebuildHistoryMatches()
	}
	return m, nil
}
