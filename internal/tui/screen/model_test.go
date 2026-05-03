package screen

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

func sampleChoices() []ModelChoice {
	return []ModelChoice{
		{ID: "claude-opus-4-7", Description: "most capable", Provider: "anthropic"},
		{ID: "claude-sonnet-4-6", Description: "balanced", Provider: "anthropic"},
		{ID: "MiniMax-M2.7", Description: "192k window", Provider: "minimax"},
		{ID: "gpt-4o", Description: "openai flagship", Provider: "openai"},
	}
}

// TestModelScreen_RendersChoices — every choice ID appears in the view
// alongside its description and provider tag.
func TestModelScreen_RendersChoices(t *testing.T) {
	s := NewModelScreen("claude-opus-4-7", sampleChoices())
	s.Resize(100, 20)
	out := stripANSIEffort(s.View())
	for _, c := range sampleChoices() {
		if !strings.Contains(out, c.ID) {
			t.Errorf("model id %q missing:\n%s", c.ID, out)
		}
	}
	if !strings.Contains(out, "Pick a model") {
		t.Errorf("missing title:\n%s", out)
	}
	if !strings.Contains(out, "current") {
		t.Errorf("missing current-model header:\n%s", out)
	}
}

// TestModelScreen_InitialCursorOnCurrent — the cursor starts on the
// entry whose ID equals `current`.
func TestModelScreen_InitialCursorOnCurrent(t *testing.T) {
	cases := []struct {
		current string
		want    int
	}{
		{"claude-opus-4-7", 0},
		{"claude-sonnet-4-6", 1},
		{"MiniMax-M2.7", 2},
		{"gpt-4o", 3},
		{"unknown-model", 0}, // fallback
	}
	for _, tc := range cases {
		s := NewModelScreen(tc.current, sampleChoices())
		if s.cursor != tc.want {
			t.Errorf("current=%q: cursor = %d, want %d", tc.current, s.cursor, tc.want)
		}
	}
}

// TestModelScreen_ArrowNavWrapsAround — picker uses circular nav
// (claude-code parity, see palette wrap fix from earlier session).
func TestModelScreen_ArrowNavWrapsAround(t *testing.T) {
	s := NewModelScreen("gpt-4o", sampleChoices()) // cursor=3 (last)
	s.Resize(100, 20)

	s.Update(tea.KeyPressMsg{Code: tea.KeyDown}) // wrap to 0
	if s.cursor != 0 {
		t.Errorf("Down at last entry should wrap to 0; got %d", s.cursor)
	}
	s.Update(tea.KeyPressMsg{Code: tea.KeyUp}) // wrap to 3
	if s.cursor != 3 {
		t.Errorf("Up at first entry should wrap to last (3); got %d", s.cursor)
	}
}

// TestModelScreen_EnterApplies — Enter sets Applied() to the cursor's
// model ID and Done() = true.
func TestModelScreen_EnterApplies(t *testing.T) {
	s := NewModelScreen("claude-opus-4-7", sampleChoices()) // cursor=0
	s.Resize(100, 20)
	s.Update(tea.KeyPressMsg{Code: tea.KeyDown}) // → 1
	s.Update(tea.KeyPressMsg{Code: tea.KeyDown}) // → 2 (MiniMax)
	s.Update(tea.KeyPressMsg{Code: tea.KeyEnter})

	if !s.Done() {
		t.Errorf("Enter should mark Done()")
	}
	if got := s.Applied(); got != "MiniMax-M2.7" {
		t.Errorf("Applied() = %q, want %q", got, "MiniMax-M2.7")
	}
}

// TestModelScreen_EscCancels — Esc dismisses without committing.
func TestModelScreen_EscCancels(t *testing.T) {
	s := NewModelScreen("claude-opus-4-7", sampleChoices())
	s.Resize(100, 20)
	s.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	s.Update(tea.KeyPressMsg{Code: tea.KeyEsc})

	if !s.Done() {
		t.Errorf("Esc should mark Done()")
	}
	if got := s.Applied(); got != "" {
		t.Errorf("Applied() after Esc = %q, want empty", got)
	}
}
