package tui

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"

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
	m.Update(tea.KeyMsg{Type: tea.KeyRight})
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
// firing any command.
func TestHelpDispatch_EscNoDispatch(t *testing.T) {
	m := newSlashTestModel(t)
	m.input.SetValue("/help")
	pressEnter(t, m)
	beforeMsgs := len(m.messages)

	m.Update(tea.KeyMsg{Type: tea.KeyRight})
	m.Update(tea.KeyMsg{Type: tea.KeyDown})
	m.Update(tea.KeyMsg{Type: tea.KeyEsc})

	if m.activeScreen != nil {
		t.Errorf("Esc should clear activeScreen")
	}
	// Esc shouldn't have produced a confirmation message.
	if len(m.messages) > beforeMsgs {
		t.Errorf("Esc should not append messages; before=%d after=%d", beforeMsgs, len(m.messages))
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
