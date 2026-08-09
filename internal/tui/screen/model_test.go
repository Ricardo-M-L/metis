package screen

import (
	"fmt"
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

func TestModelScreen_CurrentProviderDisambiguatesDuplicateID(t *testing.T) {
	choices := []ModelChoice{
		{ID: "gpt-4o", Provider: "openai"},
		{ID: "gpt-4o", Provider: "relay"},
	}
	s := NewModelScreen("gpt-4o", choices)
	s.SetCurrentProvider("relay")
	s.Update(tea.KeyPressMsg{Code: tea.KeyEnter})

	choice, ok := s.AppliedChoice()
	if !ok || choice.Provider != "relay" {
		t.Fatalf("duplicate model ID selected provider=%q, want active relay", choice.Provider)
	}
}

func TestModelScreen_HeightBoundsAndFollowsCursor(t *testing.T) {
	choices := make([]ModelChoice, 20)
	for i := range choices {
		choices[i] = ModelChoice{ID: fmt.Sprintf("model-%02d", i+1), Provider: "test"}
	}
	s := NewModelScreen("model-01", choices)
	s.Resize(80, 10) // six chrome rows + four visible choices
	s.Update(tea.KeyPressMsg{Code: tea.KeyEnd})
	out := stripANSIEffort(s.View())

	if lines := strings.Count(out, "\n") + 1; lines > 10 {
		t.Fatalf("picker rendered %d rows into height 10:\n%s", lines, out)
	}
	if !strings.Contains(out, "model-20") || !strings.Contains(out, "model-17") || strings.Contains(out, "model-16") {
		t.Fatalf("cursor-centred window did not follow End selection:\n%s", out)
	}
	if !strings.Contains(out, "20/20") {
		t.Fatalf("clipped picker lacks position indicator:\n%s", out)
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
	choice, ok := s.AppliedChoice()
	if !ok || choice.ID != "MiniMax-M2.7" || choice.Provider != "minimax" {
		t.Errorf("AppliedChoice() = %+v, %v; want MiniMax/minimax", choice, ok)
	}
}

func TestModelScreen_CustomTitle(t *testing.T) {
	s := NewModelScreen("", sampleChoices())
	s.SetTitle("Choose a vision model · prompt kept")
	out := stripANSIEffort(s.View())
	if !strings.Contains(out, "Choose a vision model") {
		t.Fatalf("custom title missing:\n%s", out)
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
