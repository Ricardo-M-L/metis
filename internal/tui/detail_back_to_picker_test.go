package tui

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/Ricardo-M-L/metis/internal/tui/screen"
)

// TestDetailScreen_EscReturnsToParentPicker — claude-code stack
// semantics: Esc on a /skills detail re-opens the /skills picker
// rather than dropping back to chat.
func TestDetailScreen_EscReturnsToParentPicker(t *testing.T) {
	m := newSlashTestModel(t)
	m.input.SetValue("/skills")
	pressEnter(t, m)

	if _, ok := m.activeScreen.(*screen.PickerScreen); !ok {
		t.Fatalf("/skills should open PickerScreen; got %T", m.activeScreen)
	}
	// Drill into a detail.
	m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	ds, ok := m.activeScreen.(*screen.DetailScreen)
	if !ok {
		t.Fatalf("Enter should open DetailScreen; got %T", m.activeScreen)
	}
	if got := ds.ParentCommand(); got != "skills" {
		t.Errorf("DetailScreen ParentCommand() = %q, want %q", got, "skills")
	}

	// Esc on detail — should re-dispatch /skills, NOT clear to chat.
	m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if _, ok := m.activeScreen.(*screen.PickerScreen); !ok {
		t.Errorf("after Esc on DetailScreen, should return to PickerScreen; got %T", m.activeScreen)
	}
}

// TestDetailScreen_NoParentEscToChat — a DetailScreen built without
// WithParent (e.g. opened directly without a picker) Esc returns to
// chat.
func TestDetailScreen_NoParentEscToChat(t *testing.T) {
	m := newSlashTestModel(t)
	// Construct a parentless DetailScreen and install it directly.
	ds := screen.NewDetailScreen("/manual", "", []screen.DetailSection{
		{Heading: "Test", Lines: []string{"hello"}},
	})
	ds.Resize(m.width, m.height)
	m.activeScreen = ds

	m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if m.activeScreen != nil {
		t.Errorf("parentless DetailScreen Esc should clear to chat; got %T", m.activeScreen)
	}
}
