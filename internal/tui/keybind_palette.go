package tui

// keybind_palette.go — slash-command palette key handling: navigation
// keys (↑↓ Tab Esc), filter sync, autocomplete (Tab), and the
// matchCommands filter that rebuilds m.palMatched.

import (
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/Ricardo-M-L/metis/internal/slash"
)

// dismissPalette clears both the visible dropdown and its cached selection.
// Clearing only showPalette/palFilter leaves the previous match slice/cursor
// alive, which can be painted again by the same Enter key update after a
// destructive command resets the input.
func (m *Model) dismissPalette() {
	m.showPalette = false
	m.palFilter = ""
	m.palCursor = 0
	m.palMatched = nil
}

func (m *Model) handlePaletteKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	// v2: KeyMsg is interface; switch on .String() and treat any
	// single-rune press as the "typed character" branch (replaces v1
	// case tea.KeyRunes).
	keyName := msg.String()
	switch keyName {
	case "esc":
		m.dismissPalette()
		m.input.Reset()
	case "up":
		// claude-code parity: ↑ at the top wraps to the bottom (and Tab
		// already wraps below). Long /help-style lists are dramatically
		// faster to navigate when wrapping; the previous "stop at edge"
		// behavior forced the user to scroll all the way back through
		// 50+ entries to reach a near-bottom command they overshot.
		if n := len(m.palMatched); n > 0 {
			m.palCursor = (m.palCursor - 1 + n) % n
			m.syncBufferToCursor()
		}
	case "down":
		// ↓ at the bottom wraps to the top — symmetric with KeyUp.
		if n := len(m.palMatched); n > 0 {
			m.palCursor = (m.palCursor + 1) % n
			m.syncBufferToCursor()
		}
	case "tab":
		if len(m.palMatched) > 0 {
			m.palCursor = (m.palCursor + 1) % len(m.palMatched)
			m.syncBufferToCursor()
		}
	case "backspace":
		if len(m.palFilter) > 0 {
			m.palFilter = m.palFilter[:len(m.palFilter)-1]
			m.matchCommands()
		} else {
			m.showPalette = false
		}
	default:
		// Treat any single-rune press as a typed character. v1's
		// `case tea.KeyRunes` is gone; printable runes arrive as
		// String() == "x" for letters/digits/etc.
		if len([]rune(keyName)) != 1 {
			return m, nil
		}
		// Only ASCII letters, digits, '-' and '_' belong in a slash
		// command name. Quietly drop anything else (paste of newlines,
		// backslashes from terminal mis-mapping, fullwidth glyphs from
		// IME) so we don't accidentally widen the palette filter into
		// nonsensical strings.
		s := keyName
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
		for _, cmd := range m.commandCatalog() {
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
	// Strip args before matching: when the user types "/eff high",
	// palFilter is "eff high" but the command name to match is "eff"
	// — anything past the first space is the args payload, not part
	// of the name. Without this trim the palette goes empty as soon
	// as the user hits space, breaking auto-promote on submit.
	if idx := strings.IndexByte(filter, ' '); idx >= 0 {
		filter = filter[:idx]
	}
	if filter == "" {
		m.palMatched = append(m.palMatched, m.commandCatalog()...)
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
	for _, cmd := range m.commandCatalog() {
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

// looksLikeSlashPath returns true when val starts with `/` but the
// head token (everything up to the first space) is NOT a valid
// slash-command head. In that case we suppress the slash palette —
// the user is typing a pasted path or file reference, not a command.
//
// Examples:
//
//	"/help"              false (real command)
//	"/model haiku"       false (real command + args)
//	"/Users/x/foo.go"    true  (path: second `/`)
//	"/.. is bad"         true  (head ".." contains `.`)
//	"/foo.bar"           true  (dot in name)
//
// The empty-head edge case (just `/`) returns false so the palette
// opens with the full command list — that's how the user discovers
// available commands.
//
// Delegates to slash.IsCommandShape so both this gate AND the slash
// registry's Parse() use the same definition of "command-shape".
// Without this shared rule the two paths could drift — e.g. typing
// shows the palette but submit emits "unknown" — exactly the bug
// from image #15.
func looksLikeSlashPath(val string) bool {
	if len(val) < 2 || val[0] != '/' {
		return false
	}
	head := val[1:]
	if idx := strings.IndexByte(head, ' '); idx >= 0 {
		head = head[:idx]
	}
	if head == "" {
		return false
	}
	return !slash.IsCommandShape(head)
}
