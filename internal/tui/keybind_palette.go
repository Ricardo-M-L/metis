package tui

// keybind_palette.go — slash-command palette key handling: navigation
// keys (↑↓ Tab Esc), filter sync, autocomplete (Tab), and the
// matchCommands filter that rebuilds m.palMatched.

import (
	"strings"

	tea "github.com/charmbracelet/bubbletea"
)

func (m *Model) handlePaletteKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.Type {
	case tea.KeyEscape:
		m.showPalette = false
		m.palFilter = ""
		m.input.Reset()
	case tea.KeyUp:
		if m.palCursor > 0 {
			m.palCursor--
			m.syncBufferToCursor()
		}
	case tea.KeyDown:
		if m.palCursor < len(m.palMatched)-1 {
			m.palCursor++
			m.syncBufferToCursor()
		}
	case tea.KeyTab:
		if len(m.palMatched) > 0 {
			m.palCursor = (m.palCursor + 1) % len(m.palMatched)
			m.syncBufferToCursor()
		}
	case tea.KeyBackspace:
		if len(m.palFilter) > 0 {
			m.palFilter = m.palFilter[:len(m.palFilter)-1]
			m.matchCommands()
		} else {
			m.showPalette = false
		}
	case tea.KeyRunes:
		m.palFilter += msg.String()
		m.matchCommands()
	}
	return m, nil
}

// syncBufferToCursor copies the currently-highlighted palette entry back
// into the input buffer so Tab/↑/↓ feel like text completion. Pressing
// Enter then submits whatever's in the buffer through the regular path.
func (m *Model) syncBufferToCursor() {
	if m.palCursor < 0 || m.palCursor >= len(m.palMatched) {
		return
	}
	name := m.palMatched[m.palCursor].Name
	m.input.SetValue("/" + name)
	m.input.CursorEnd()
	m.palFilter = name
}

func (m *Model) doAutocomplete() {
	val := m.input.Value()

	// Slash command completion (cursor at start, "/" prefix). Walks
	// the registered command list and inserts the first prefix match.
	if strings.HasPrefix(val, "/") {
		partial := strings.ToLower(strings.TrimPrefix(val, "/"))
		for _, cmd := range m.cmds.All() {
			if strings.HasPrefix(strings.ToLower(cmd.Name), partial) {
				m.input.SetValue("/" + cmd.Name + " ")
				m.input.CursorEnd()
				return
			}
		}
		return
	}

	// @filename completion — claude-code's pattern. Detect a trailing
	// @-token in the input and replace with the best fuzzy match
	// from the file index.
	if filter, ok := detectAtMention(val); ok {
		matches := matchAtMention(filter)
		if len(matches) > 0 {
			m.input.SetValue(applyAtMention(val, matches[0]))
			m.input.CursorEnd()
		}
	}
}

// matchCommands populates m.palMatched from the registered slash
// commands using a 3-tier match: exact name → prefix → contains.
// Description matching was removed because fuzzy-matching against
// long help text produced false positives (typing "rena" matched
// /voice because its description happened to contain r,e,n,a in
// order).
//
// When filter is empty, returns ALL registered commands (palette
// scrolls within paletteMaxRows). Previously capped at 15 which
// hid commands like /rename that were registered later in the
// table.
func (m *Model) matchCommands() {
	m.palMatched = nil
	m.palCursor = 0
	filter := strings.ToLower(m.palFilter)
	if filter == "" {
		m.palMatched = append(m.palMatched, m.cmds.All()...)
		return
	}
	var prefixHits, containsHits []REPLCommand
	for _, cmd := range m.cmds.All() {
		name := strings.ToLower(cmd.Name)
		switch {
		case name == filter:
			m.palMatched = append([]REPLCommand{cmd}, m.palMatched...)
		case strings.HasPrefix(name, filter):
			prefixHits = append(prefixHits, cmd)
		case strings.Contains(name, filter):
			containsHits = append(containsHits, cmd)
		}
		// Aliases also count for prefix.
		for _, a := range cmd.Aliases {
			al := strings.ToLower(a)
			if al == filter {
				m.palMatched = append([]REPLCommand{cmd}, m.palMatched...)
				break
			} else if strings.HasPrefix(al, filter) {
				prefixHits = append(prefixHits, cmd)
				break
			}
		}
	}
	m.palMatched = append(m.palMatched, prefixHits...)
	m.palMatched = append(m.palMatched, containsHits...)
}
