package screen

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

func samplePickerItems() []PickerItem {
	return []PickerItem{
		{Key: "session-1", Label: "morning sprint", Description: "claude-opus-4-7", Hint: "10:30"},
		{Key: "session-2", Label: "debug rabbit hole", Description: "MiniMax-M2.7", Hint: "11:45"},
		{Key: "session-3", Label: "lunch chat", Description: "MiniMax-M2.7", Hint: "12:30"},
	}
}

// TestPickerScreen_RendersItems — every item label appears in the view
// with description and hint columns.
func TestPickerScreen_RendersItems(t *testing.T) {
	s := NewPickerScreen("/sessions", "3 recent sessions", samplePickerItems())
	s.Resize(100, 20)
	out := stripANSIEffort(s.View())
	for _, it := range samplePickerItems() {
		if !strings.Contains(out, it.Label) {
			t.Errorf("item label %q missing:\n%s", it.Label, out)
		}
		if !strings.Contains(out, it.Hint) {
			t.Errorf("item hint %q missing:\n%s", it.Hint, out)
		}
	}
	if !strings.Contains(out, "/sessions") {
		t.Errorf("missing command stripe '/sessions':\n%s", out)
	}
	if !strings.Contains(out, "3 recent sessions") {
		t.Errorf("missing subtitle:\n%s", out)
	}
}

// TestPickerScreen_EmptyShowsHint — empty items list renders the
// "(empty)" hint instead of a blank box.
func TestPickerScreen_EmptyShowsHint(t *testing.T) {
	s := NewPickerScreen("/sessions", "0 sessions", nil)
	s.Resize(100, 20)
	out := stripANSIEffort(s.View())
	if !strings.Contains(out, "(empty)") {
		t.Errorf("empty picker should render '(empty)' hint:\n%s", out)
	}
}

// TestPickerScreen_CursorWraps — ↑↓ wrap claude-code style.
func TestPickerScreen_CursorWraps(t *testing.T) {
	s := NewPickerScreen("/sessions", "", samplePickerItems()) // cursor=0
	s.Resize(100, 20)

	s.Update(tea.KeyPressMsg{Code: tea.KeyUp}) // wrap to last (2)
	if s.cursor != 2 {
		t.Errorf("Up at 0 should wrap to 2; got %d", s.cursor)
	}
	s.Update(tea.KeyPressMsg{Code: tea.KeyDown}) // wrap to 0
	if s.cursor != 0 {
		t.Errorf("Down at 2 should wrap to 0; got %d", s.cursor)
	}
}

// TestPickerScreen_EnterReturnsKey — Enter sets Selected() to the
// cursored item's Key (NOT Label).
func TestPickerScreen_EnterReturnsKey(t *testing.T) {
	s := NewPickerScreen("/sessions", "", samplePickerItems())
	s.Resize(100, 20)
	s.Update(tea.KeyPressMsg{Code: tea.KeyDown}) // → session-2
	s.Update(tea.KeyPressMsg{Code: tea.KeyEnter})

	if !s.Done() {
		t.Errorf("Enter should mark Done()")
	}
	if got := s.Selected(); got != "session-2" {
		t.Errorf("Selected() = %q, want %q", got, "session-2")
	}
}

// TestPickerScreen_EscCancels — Esc dismisses with empty Selected.
func TestPickerScreen_EscCancels(t *testing.T) {
	s := NewPickerScreen("/sessions", "", samplePickerItems())
	s.Resize(100, 20)
	s.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	s.Update(tea.KeyPressMsg{Code: tea.KeyEsc})

	if !s.Done() {
		t.Errorf("Esc should mark Done()")
	}
	if got := s.Selected(); got != "" {
		t.Errorf("Selected() after Esc = %q, want empty", got)
	}
}

// TestPickerScreen_CommandRoutingTag — Command() returns the label the
// caller passed in. Required by applyScreenResult to route on.
func TestPickerScreen_CommandRoutingTag(t *testing.T) {
	for _, cmd := range []string{"/sessions", "/skills", "/tools"} {
		s := NewPickerScreen(cmd, "", samplePickerItems())
		if got := s.Command(); got != cmd {
			t.Errorf("Command() = %q, want %q", got, cmd)
		}
	}
}
