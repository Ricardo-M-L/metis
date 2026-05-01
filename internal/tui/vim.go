package tui

// vim.go implements optional vim-style modal input for the
// chat-surface textarea. Two modes:
//
//	INSERT — default; behaves identically to non-vim mode
//	NORMAL — hjkl moves cursor, i/a/A/o re-enter INSERT, 0/$/x edit
//
// We translate vim keystrokes into bubbletea/textarea key messages
// (KeyLeft, KeyHome, KeyDelete, etc) rather than touching the
// textarea's internal cursor — keeps us decoupled from bubbles
// internals and reuses its existing rune-aware editing.
//
// Reference: openclaude/src/types/textInputTypes.ts uses the same
// 'INSERT' | 'NORMAL' two-mode split. Full vim has a dozen modes
// (visual, command, replace, etc.) — we cover the 90% use case
// without the keymap complexity.

import (
	tea "github.com/charmbracelet/bubbletea"
)

const (
	vimOff    = ""       // vim disabled (default)
	vimInsert = "insert" // typing happens normally
	vimNormal = "normal" // hjkl etc; ESC switches here from INSERT
)

// vimModeState is the package-level mode flag. Lives at package
// scope (not on Model) so the /vim command — invoked through the
// REPL handler which doesn't carry a Model reference — can flip
// it. metis is single-instance so global state is fine.
var vimModeState = vimOff

// toggleVimMode flips between off and insert; called by /vim.
// In NORMAL mode the user already has Esc to leave; the slash
// command isn't there for runtime mode-switching.
func toggleVimMode() {
	if vimModeState == vimOff {
		vimModeState = vimInsert
	} else {
		vimModeState = vimOff
	}
}

// vimModeStatus returns a human label for the current state.
func vimModeStatus() string {
	switch vimModeState {
	case vimOff:
		return "(vim mode: off)"
	case vimInsert:
		return "(vim mode: on — press Esc in input to enter NORMAL)"
	case vimNormal:
		return "(vim mode: NORMAL — i/a/o to insert)"
	default:
		return "(vim mode: " + vimModeState + ")"
	}
}

// handleVimNormalKey interprets one keypress while in NORMAL mode.
// Returns (handled, cmd): handled=true means the key was consumed;
// false lets the caller fall through (or drop it). Most NORMAL-mode
// keys translate to a bubbles-textarea key event, which we hand to
// m.input.Update directly so the existing rune-aware cursor logic
// keeps working.
func (m *Model) handleVimNormalKey(msg tea.KeyMsg) (bool, tea.Cmd) {
	// Explicit "i" / "a" / "A" / "o" — switch back to INSERT mode,
	// optionally with a positioning keystroke first to mimic vim's
	// append/append-end semantics.
	switch msg.String() {
	case "i":
		vimModeState = vimInsert
		return true, nil
	case "a":
		// append: cursor right then INSERT
		var cmd tea.Cmd
		m.input, cmd = m.input.Update(tea.KeyMsg{Type: tea.KeyRight})
		vimModeState = vimInsert
		return true, cmd
	case "A":
		// append-end-of-line: KeyEnd then INSERT
		var cmd tea.Cmd
		m.input, cmd = m.input.Update(tea.KeyMsg{Type: tea.KeyEnd})
		vimModeState = vimInsert
		return true, cmd
	case "I":
		// insert-start-of-line: KeyHome then INSERT
		var cmd tea.Cmd
		m.input, cmd = m.input.Update(tea.KeyMsg{Type: tea.KeyHome})
		vimModeState = vimInsert
		return true, cmd
	case "o":
		// open-line-below: KeyEnd, KeyEnter, INSERT. The textarea
		// inserts a literal newline on Enter when not intercepted
		// here; we craft the keystrokes so the user lands on a fresh
		// row in INSERT mode.
		var cmd tea.Cmd
		m.input, cmd = m.input.Update(tea.KeyMsg{Type: tea.KeyEnd})
		m.input.InsertRune('\n')
		vimModeState = vimInsert
		return true, cmd

	// Movement
	case "h":
		var cmd tea.Cmd
		m.input, cmd = m.input.Update(tea.KeyMsg{Type: tea.KeyLeft})
		return true, cmd
	case "j":
		var cmd tea.Cmd
		m.input, cmd = m.input.Update(tea.KeyMsg{Type: tea.KeyDown})
		return true, cmd
	case "k":
		var cmd tea.Cmd
		m.input, cmd = m.input.Update(tea.KeyMsg{Type: tea.KeyUp})
		return true, cmd
	case "l":
		var cmd tea.Cmd
		m.input, cmd = m.input.Update(tea.KeyMsg{Type: tea.KeyRight})
		return true, cmd
	case "0":
		var cmd tea.Cmd
		m.input, cmd = m.input.Update(tea.KeyMsg{Type: tea.KeyHome})
		return true, cmd
	case "$":
		var cmd tea.Cmd
		m.input, cmd = m.input.Update(tea.KeyMsg{Type: tea.KeyEnd})
		return true, cmd

	// Edit ops
	case "x":
		// delete char under cursor — bubbles textarea KeyDelete
		// removes the rune at cursor (forward delete).
		var cmd tea.Cmd
		m.input, cmd = m.input.Update(tea.KeyMsg{Type: tea.KeyDelete})
		return true, cmd
	case "X":
		// X — delete char before cursor (vim convention)
		var cmd tea.Cmd
		m.input, cmd = m.input.Update(tea.KeyMsg{Type: tea.KeyBackspace})
		return true, cmd
	}
	// Unhandled NORMAL-mode keys (q, w, e, b, dd, yy, p, etc.) — drop
	// silently so they don't get inserted as literal text. Future
	// expansion can map these as needed.
	return true, nil
}

// vimModeLabel returns the visible mode tag for the status bar:
// "-- NORMAL --", "-- INSERT --", or "" when vim is off. Mimics
// vim's bottom-line mode indicator.
func (m *Model) vimModeLabel() string {
	switch vimModeState {
	case vimNormal:
		return "-- NORMAL --"
	case vimInsert:
		return "-- INSERT --"
	}
	return ""
}
