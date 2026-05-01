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
		// Only ASCII letters, digits, '-' and '_' belong in a slash
		// command name. Quietly drop anything else (paste of newlines,
		// backslashes from terminal mis-mapping, fullwidth glyphs from
		// IME) so we don't accidentally widen the palette filter into
		// nonsensical strings.
		s := msg.String()
		var clean strings.Builder
		for _, r := range s {
			if (r >= 'a' && r <= 'z') ||
				(r >= 'A' && r <= 'Z') ||
				(r >= '0' && r <= '9') ||
				r == '-' || r == '_' {
				clean.WriteRune(r)
			}
		}
		if clean.Len() == 0 {
			return m, nil
		}
		m.palFilter += clean.String()
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
	// added[name] tracks which commands have already been queued so an
	// alias prefix match doesn't double-add the same /help when its
	// alias 'h' also matches filter 'h' (b2-13 reproducer: typing "/h"
	// + Tab showed /help twice in the palette). Source-of-truth key is
	// the canonical command name, since aliases all point to the same
	// REPLCommand.
	added := map[string]bool{}
	queue := func(dst *[]REPLCommand, cmd REPLCommand) {
		key := strings.ToLower(cmd.Name)
		if added[key] {
			return
		}
		added[key] = true
		*dst = append(*dst, cmd)
	}
	var prefixHits, containsHits []REPLCommand
	for _, cmd := range m.cmds.All() {
		name := strings.ToLower(cmd.Name)
		switch {
		case name == filter:
			if !added[name] {
				added[name] = true
				m.palMatched = append([]REPLCommand{cmd}, m.palMatched...)
			}
		case strings.HasPrefix(name, filter):
			queue(&prefixHits, cmd)
		case strings.Contains(name, filter):
			queue(&containsHits, cmd)
		}
		// Aliases also count, but only if the canonical name didn't
		// already match — otherwise we'd double-add.
		if added[name] {
			continue
		}
		for _, a := range cmd.Aliases {
			al := strings.ToLower(a)
			if al == filter {
				added[name] = true
				m.palMatched = append([]REPLCommand{cmd}, m.palMatched...)
				break
			} else if strings.HasPrefix(al, filter) {
				queue(&prefixHits, cmd)
				break
			}
		}
	}
	m.palMatched = append(m.palMatched, prefixHits...)
	m.palMatched = append(m.palMatched, containsHits...)
}
