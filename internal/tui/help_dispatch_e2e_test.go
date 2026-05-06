package tui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/Ricardo-M-L/metis/internal/tui/screen"
)

// TestHelpDispatch_EnterRunsPickedCommand — claude-code parity (image #14).
// Open /help, switch to commands tab, ↓ to a command, Enter must
// dispatch that command (and the activeScreen flips to whatever the
// dispatched command produces).
func TestHelpDispatch_EnterRunsPickedCommand(t *testing.T) {
	m := newSlashTestModel(t)
	m.input.SetValue("/help")
	pressEnter(t, m)

	hs, ok := m.activeScreen.(*screen.HelpScreen)
	if !ok {
		t.Fatalf("expected HelpScreen; got %T", m.activeScreen)
	}
	// Switch to commands tab where rows are selectable.
	m.Update(tea.KeyPressMsg{Code: tea.KeyRight})
	// Cursor lands on first selectable. Press Enter to fire it.
	pressEnter(t, m)

	if hs.Selected() == "" {
		t.Fatalf("HelpScreen.Selected() should be non-empty after Enter on a selectable row")
	}
	// activeScreen got cleared (HelpScreen done) and the dispatched
	// command may have opened its own modal. Either way HelpScreen
	// should not be active anymore.
	if _, stillHelp := m.activeScreen.(*screen.HelpScreen); stillHelp {
		t.Errorf("HelpScreen should be cleared after Enter; activeScreen still HelpScreen")
	}
}

// TestHelpDispatch_EscNoDispatch — Esc on /help dismisses without
// dispatching a command. As of 2026-05-05 the dismiss writes a
// trace info ("(help dialog dismissed)") so the user sees their
// action landed; the test now asserts that trace appears AND no
// command was fired.
func TestHelpDispatch_EscNoDispatch(t *testing.T) {
	m := newSlashTestModel(t)
	m.input.SetValue("/help")
	pressEnter(t, m)
	beforeMsgs := len(m.messages)

	m.Update(tea.KeyPressMsg{Code: tea.KeyRight})
	m.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	m.Update(tea.KeyPressMsg{Code: tea.KeyEsc})

	if m.activeScreen != nil {
		t.Errorf("Esc should clear activeScreen")
	}
	// Exactly one new info row — the dismiss trace. More than one would
	// suggest a command was also fired.
	added := len(m.messages) - beforeMsgs
	if added != 1 {
		t.Errorf("Esc should add exactly the dismiss-trace info row; before=%d after=%d", beforeMsgs, len(m.messages))
	}
	if added == 1 {
		last := m.messages[len(m.messages)-1]
		if last.Role != "info" || !strings.Contains(last.Content, "dismissed") {
			t.Errorf("expected info row with `dismissed`; got role=%q content=%q", last.Role, last.Content)
		}
	}
}

// TestPickerDispatch_SessionsOpensPicker — /sessions now opens a
// PickerScreen (was BodyScreen).
func TestPickerDispatch_SessionsOpensPicker(t *testing.T) {
	m := newSlashTestModel(t)
	m.input.SetValue("/sessions")
	pressEnter(t, m)
	if _, ok := m.activeScreen.(*screen.PickerScreen); !ok {
		t.Errorf("/sessions should open PickerScreen; got %T", m.activeScreen)
	}
}

// TestPickerDispatch_SkillsOpensPicker — /skills now opens a PickerScreen.
func TestPickerDispatch_SkillsOpensPicker(t *testing.T) {
	m := newSlashTestModel(t)
	m.input.SetValue("/skills")
	pressEnter(t, m)
	if _, ok := m.activeScreen.(*screen.PickerScreen); !ok {
		t.Errorf("/skills should open PickerScreen; got %T", m.activeScreen)
	}
}

// TestPickerDispatch_ToolsOpensPicker — /tools now opens a PickerScreen.
func TestPickerDispatch_ToolsOpensPicker(t *testing.T) {
	m := newSlashTestModel(t)
	m.input.SetValue("/tools")
	pressEnter(t, m)
	if _, ok := m.activeScreen.(*screen.PickerScreen); !ok {
		t.Errorf("/tools should open PickerScreen; got %T", m.activeScreen)
	}
}
