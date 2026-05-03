package screen

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

func cursorTabs() []HelpTab {
	return []HelpTab{
		{Name: "general", Body: []HelpRow{
			{Heading: "Shortcuts"},
			{Value: "free-form note"},
			{Key: "!", Value: "shell"}, // Key doesn't start with /, NOT selectable
		}},
		{Name: "commands", Body: []HelpRow{
			{Heading: "Built-in"},
			{Value: "Type any of these"},
			{Key: "/agents", Value: "list sub-agents"},
			{Key: "/clear", Value: "clear history"},
			{Key: "/cost", Value: "show cost"},
		}},
	}
}

// TestHelpCursor_StartsOnFirstSelectable — cursor opens on the first
// selectable row (Key starts with "/"), not the heading or free-form
// rows above it.
func TestHelpCursor_StartsOnFirstSelectable(t *testing.T) {
	s := NewHelpScreen("v1", cursorTabs())
	s.Resize(80, 30)
	// First selectable in tab 0 is index 2 ("!"... wait, "!" doesn't
	// start with "/"). So tab 0 has NO selectable rows; cursor stays at 0.
	if s.cursor != 0 {
		t.Errorf("first tab has no selectable rows; cursor should be 0, got %d", s.cursor)
	}
	if got := s.Selected(); got != "" {
		t.Errorf("Selected() should be empty when no selectable; got %q", got)
	}

	// Switch to commands tab — first selectable is /agents at index 2.
	s.switchTab(1)
	if s.cursor != 2 {
		t.Errorf("commands tab: first selectable cursor should be 2 (/agents); got %d", s.cursor)
	}
}

// TestHelpCursor_DownSkipsNonSelectable — pressing ↓ from the cursor
// jumps to the NEXT selectable row, skipping headings and free-form.
func TestHelpCursor_DownSkipsNonSelectable(t *testing.T) {
	s := NewHelpScreen("v1", cursorTabs())
	s.Resize(80, 30)
	s.switchTab(1) // cursor now on /agents (idx 2)

	s.Update(tea.KeyMsg{Type: tea.KeyDown})
	if s.cursor != 3 {
		t.Errorf("Down from /agents should jump to /clear (3); got %d", s.cursor)
	}
	s.Update(tea.KeyMsg{Type: tea.KeyDown})
	if s.cursor != 4 {
		t.Errorf("Down from /clear should jump to /cost (4); got %d", s.cursor)
	}
}

// TestHelpCursor_EnterSelectsCommand — Enter on a selectable row sets
// Selected() to the command name (without leading /).
func TestHelpCursor_EnterSelectsCommand(t *testing.T) {
	s := NewHelpScreen("v1", cursorTabs())
	s.Resize(80, 30)
	s.switchTab(1)
	s.Update(tea.KeyMsg{Type: tea.KeyDown}) // /clear

	s.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if !s.Done() {
		t.Errorf("Enter should mark Done()")
	}
	if got := s.Selected(); got != "clear" {
		t.Errorf("Selected() = %q, want %q", got, "clear")
	}
}

// TestHelpCursor_EnterOnNonSelectableLeavesEmpty — Enter when cursor
// happens to be on a non-selectable row (e.g. general tab where there
// are no commands) returns empty Selected.
func TestHelpCursor_EnterOnNonSelectableLeavesEmpty(t *testing.T) {
	s := NewHelpScreen("v1", cursorTabs())
	s.Resize(80, 30)
	// Stay on tab 0 (no selectable rows). cursor=0 (first row, Heading).
	s.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if got := s.Selected(); got != "" {
		t.Errorf("Selected() on non-selectable should be empty; got %q", got)
	}
}

// TestHelpCursor_TabSwitchResetsCursor — switching tabs places cursor
// on the new tab's first selectable row, not somewhere stale.
func TestHelpCursor_TabSwitchResetsCursor(t *testing.T) {
	s := NewHelpScreen("v1", cursorTabs())
	s.Resize(80, 30)
	s.switchTab(1) // commands, cursor=2
	s.Update(tea.KeyMsg{Type: tea.KeyDown}) // cursor=3 (/clear)
	s.switchTab(0) // general — no selectable
	if s.cursor != 0 {
		t.Errorf("after switch back to general (no selectable), cursor should reset to 0; got %d", s.cursor)
	}
}

// TestHelpCursor_RenderShowsMarker — the cursor's row gets the ▸ marker
// in the rendered view.
func TestHelpCursor_RenderShowsMarker(t *testing.T) {
	s := NewHelpScreen("v1", cursorTabs())
	s.Resize(80, 30)
	s.switchTab(1)
	out := stripANSIEffort(s.View())
	if !strings.Contains(out, "▸") {
		t.Errorf("view missing cursor ▸ marker:\n%s", out)
	}
}

// TestHelpCursor_FooterAdaptsForSelectableTab — when the active tab
// has selectable rows, the footer shows "Enter run" hint.
func TestHelpCursor_FooterAdaptsForSelectableTab(t *testing.T) {
	s := NewHelpScreen("v1", cursorTabs())
	s.Resize(80, 30)

	s.switchTab(1) // selectable tab
	out := stripANSIEffort(s.View())
	if !strings.Contains(out, "Enter run") {
		t.Errorf("selectable tab footer should mention 'Enter run'; got:\n%s", out)
	}

	s.switchTab(0) // non-selectable tab
	out = stripANSIEffort(s.View())
	if strings.Contains(out, "Enter run") {
		t.Errorf("non-selectable tab footer should NOT mention 'Enter run'; got:\n%s", out)
	}
}
