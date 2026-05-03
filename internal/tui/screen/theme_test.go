package screen

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

func sampleThemes() []ThemeChoice {
	return []ThemeChoice{
		{Name: "dark", Swatches: []string{"#000000", "#ffffff", "#bd93f9"}},
		{Name: "light", Swatches: []string{"#ffffff", "#000000", "#5a4a78"}},
		{Name: "dark-daltonized", Swatches: []string{"#1a1a2e", "#e8e8e8", "#a594f9"}},
	}
}

// TestThemeScreen_RendersCurrentName — the active theme name appears in
// the Theme row (highlighted between ◀ ▶ arrows).
func TestThemeScreen_RendersCurrentName(t *testing.T) {
	s := NewThemeScreen("light", sampleThemes())
	s.Resize(80, 20)
	out := stripANSIEffort(s.View())
	if !strings.Contains(out, "light") {
		t.Errorf("active theme 'light' missing:\n%s", out)
	}
	if !strings.Contains(out, "◀") || !strings.Contains(out, "▶") {
		t.Errorf("missing cycle arrows:\n%s", out)
	}
	// Inactive themes shouldn't appear in the visible name row (their
	// names only show as we cycle to them).
	if strings.Count(out, "dark-daltonized") != 0 {
		t.Errorf("inactive theme name leaked into view:\n%s", out)
	}
}

// TestThemeScreen_InitialCursorOnCurrent — cursor seeded by `current`.
func TestThemeScreen_InitialCursorOnCurrent(t *testing.T) {
	cases := []struct {
		current string
		want    int
	}{
		{"dark", 0},
		{"light", 1},
		{"dark-daltonized", 2},
		{"unknown", 0}, // fallback
	}
	for _, tc := range cases {
		s := NewThemeScreen(tc.current, sampleThemes())
		if s.cursor != tc.want {
			t.Errorf("current=%q: cursor = %d, want %d", tc.current, s.cursor, tc.want)
		}
	}
}

// TestThemeScreen_ArrowCyclesWraparound — ←/→ wrap around the theme set.
func TestThemeScreen_ArrowCyclesWraparound(t *testing.T) {
	s := NewThemeScreen("dark", sampleThemes()) // cursor=0
	s.Resize(80, 20)

	s.Update(tea.KeyPressMsg{Code: tea.KeyLeft}) // wrap to last
	if s.cursor != 2 {
		t.Errorf("Left at index 0 should wrap to last (2); got %d", s.cursor)
	}
	s.Update(tea.KeyPressMsg{Code: tea.KeyRight}) // wrap back to 0
	if s.cursor != 0 {
		t.Errorf("Right at last should wrap to 0; got %d", s.cursor)
	}
}

// TestThemeScreen_EnterApplies — Enter sets Applied() to the cursor's
// theme name and Done() = true.
func TestThemeScreen_EnterApplies(t *testing.T) {
	s := NewThemeScreen("dark", sampleThemes())
	s.Resize(80, 20)
	s.Update(tea.KeyPressMsg{Code: tea.KeyRight}) // → light
	s.Update(tea.KeyPressMsg{Code: tea.KeyEnter})

	if !s.Done() {
		t.Errorf("Enter should mark Done()")
	}
	if got := s.Applied(); got != "light" {
		t.Errorf("Applied() = %q, want %q", got, "light")
	}
}

// TestThemeScreen_EscCancels — Esc dismisses without applying.
func TestThemeScreen_EscCancels(t *testing.T) {
	s := NewThemeScreen("dark", sampleThemes())
	s.Resize(80, 20)
	s.Update(tea.KeyPressMsg{Code: tea.KeyRight})
	s.Update(tea.KeyPressMsg{Code: tea.KeyEsc})

	if !s.Done() {
		t.Errorf("Esc should mark Done()")
	}
	if got := s.Applied(); got != "" {
		t.Errorf("Applied() after Esc = %q, want empty", got)
	}
}
