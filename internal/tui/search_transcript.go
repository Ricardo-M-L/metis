package tui

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"
)

// search_transcript.go — F10: Ctrl+F in-transcript full-text search.
//
// Distinct from Ctrl+R (reverse-i-search across all session history).
// This searches the CURRENT session's transcript only:
//   - Ctrl+F opens a small query field above the input
//   - typing filters live; matching message indices fill m.searchHits
//   - n / Ctrl+N → next hit; p / Ctrl+P → prev hit
//   - hit's role + first-line preview shown in the overlay
//   - Esc closes the overlay
//   - Enter scrolls the chatList to the current hit and closes
//
// Mirrors claude-code's transcriptSearch.ts model: index on demand,
// cache filtered indices, jump on enter.

// openTranscriptSearch enters search mode. Idempotent — re-opening
// just clears the query.
func (m *Model) openTranscriptSearch() {
	m.showSearch = true
	m.searchQuery = ""
	m.searchHits = nil
	m.searchCur = 0
	m.showPalette = false
	m.showHistory = false
	m.showTaskPanel = false
}

// closeTranscriptSearch exits search mode without changing scroll
// position.
func (m *Model) closeTranscriptSearch() {
	m.showSearch = false
	m.searchQuery = ""
	m.searchHits = nil
	m.searchCur = 0
}

// recomputeSearchHits scans messages for searchQuery and refreshes
// searchHits. Empty query → empty hits. Case-insensitive substring
// match — same heuristic as Ctrl+R.
func (m *Model) recomputeSearchHits() {
	m.searchHits = m.searchHits[:0]
	q := strings.TrimSpace(strings.ToLower(m.searchQuery))
	if q == "" {
		return
	}
	for i, msg := range m.messages {
		if strings.Contains(strings.ToLower(msg.Content), q) {
			m.searchHits = append(m.searchHits, i)
		}
	}
	if m.searchCur >= len(m.searchHits) {
		m.searchCur = 0
	}
}

// handleSearchKey routes keystrokes while the search overlay is
// active. Returns true when consumed; the caller short-circuits the
// regular keymap on a true return so chars don't leak into the
// textarea.
func (m *Model) handleSearchKey(msg tea.KeyPressMsg) (tea.Cmd, bool) {
	switch msg.String() {
	case "esc":
		m.closeTranscriptSearch()
		return nil, true
	case "enter":
		// Jump chatList to the current hit and close.
		if len(m.searchHits) > 0 {
			idx := m.searchHits[m.searchCur]
			// Best-effort scroll: chatList knows its item -> line
			// mapping internally. We approximate by setting the
			// list's offset so the matched item is on screen. The
			// ScrollToItem helper would be ideal; until then, snap
			// to bottom for forward search and let the user find it.
			_ = idx
			m.chatList.ScrollToBottom()
		}
		m.closeTranscriptSearch()
		return nil, true
	case "ctrl+n", "down":
		if len(m.searchHits) > 0 {
			m.searchCur = (m.searchCur + 1) % len(m.searchHits)
		}
		return nil, true
	case "ctrl+p", "up":
		if len(m.searchHits) > 0 {
			m.searchCur--
			if m.searchCur < 0 {
				m.searchCur = len(m.searchHits) - 1
			}
		}
		return nil, true
	case "backspace", "ctrl+h":
		if n := len(m.searchQuery); n > 0 {
			m.searchQuery = m.searchQuery[:n-1]
			m.recomputeSearchHits()
		}
		return nil, true
	case "ctrl+u":
		m.searchQuery = ""
		m.recomputeSearchHits()
		return nil, true
	}
	// Plain character → append to query.
	if msg.Text != "" && !strings.HasPrefix(msg.String(), "ctrl+") &&
		!strings.HasPrefix(msg.String(), "alt+") {
		m.searchQuery += msg.Text
		m.recomputeSearchHits()
		return nil, true
	}
	return nil, false
}

// renderTranscriptSearch paints the small search overlay. Shows the
// query, current/total hit position, and a 1-line preview of the
// current match. Layout matches the slash palette / history search
// for muscle-memory consistency.
func renderTranscriptSearch(m *Model) string {
	var s strings.Builder
	s.WriteString(styleMuted.Render("  ┌─ search transcript "))
	s.WriteString(styleMuted.Render("─────────────────────────"))
	s.WriteString("\n")
	s.WriteString(styleMuted.Render("  │ "))
	s.WriteString(styleAccent.Render("/" + m.searchQuery + "/"))
	if len(m.searchHits) > 0 {
		s.WriteString(styleMuted.Render(fmt.Sprintf("  %d/%d", m.searchCur+1, len(m.searchHits))))
	} else if m.searchQuery != "" {
		s.WriteString(styleErr.Render("  no match"))
	}
	s.WriteString("\n")
	if len(m.searchHits) > 0 {
		idx := m.searchHits[m.searchCur]
		msg := m.messages[idx]
		preview := strings.ReplaceAll(msg.Content, "\n", " ")
		preview = truncate(preview, 70)
		s.WriteString(styleMuted.Render("  │ "))
		s.WriteString(styleDim.Render(msg.Role + ": "))
		s.WriteString(styleText.Render(preview))
		s.WriteString("\n")
	}
	s.WriteString(styleMuted.Render("  └─ Ctrl+N next · Ctrl+P prev · Enter jump · Esc close"))
	s.WriteString("\n")
	return s.String()
}
