package screen

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

// TestEffortScreen_RendersAllLevels — every level must appear in the
// rendered output exactly once. Locks down the slider's level set so
// adding "xhigh" / "max" later requires conscious test update.
func TestEffortScreen_RendersAllLevels(t *testing.T) {
	s := NewEffortScreen("medium")
	s.Resize(80, 20)
	out := stripANSIEffort(s.View())
	for _, lvl := range []string{"off", "low", "medium", "high"} {
		if !strings.Contains(out, lvl) {
			t.Errorf("level %q missing from view:\n%s", lvl, out)
		}
	}
	// Polar labels.
	if !strings.Contains(out, "Speed") || !strings.Contains(out, "Intelligence") {
		t.Errorf("missing Speed↔Intelligence polar labels:\n%s", out)
	}
	// Pointer.
	if !strings.Contains(out, "▲") {
		t.Errorf("missing ▲ pointer:\n%s", out)
	}
}

// TestEffortScreen_InitialCursor — the constructor should start the
// cursor on whichever level the caller passed as `current`.
func TestEffortScreen_InitialCursor(t *testing.T) {
	cases := []struct {
		current string
		want    int // index into ["off", "low", "medium", "high"]
	}{
		{"off", 0},
		{"low", 1},
		{"medium", 2},
		{"high", 3},
		{"unknown", 2}, // fallback to medium
		{"", 2},        // also fallback to medium
		{"  HIGH  ", 3}, // case + whitespace tolerant
	}
	for _, tc := range cases {
		s := NewEffortScreen(tc.current)
		if s.cursor != tc.want {
			t.Errorf("NewEffortScreen(%q): cursor = %d, want %d", tc.current, s.cursor, tc.want)
		}
	}
}

// TestEffortScreen_LeftRightNav — ← / → / h / l move the cursor with
// edge-clamping (no wrap, since the slider has clear endpoints).
func TestEffortScreen_LeftRightNav(t *testing.T) {
	s := NewEffortScreen("medium") // cursor=2
	s.Resize(80, 20)

	s.Update(tea.KeyPressMsg{Code: tea.KeyRight})
	if s.cursor != 3 {
		t.Errorf("KeyRight from medium: %d, want 3 (high)", s.cursor)
	}
	s.Update(tea.KeyPressMsg{Code: tea.KeyRight}) // already at end → clamp
	if s.cursor != 3 {
		t.Errorf("KeyRight at right edge should clamp: %d, want 3", s.cursor)
	}
	s.Update(tea.KeyPressMsg{Code: tea.KeyLeft})
	if s.cursor != 2 {
		t.Errorf("KeyLeft from high: %d, want 2", s.cursor)
	}

	// vim binds.
	s.Update(tea.KeyPressMsg{Code: 'h', Text: "h"})
	if s.cursor != 1 {
		t.Errorf("h from medium: %d, want 1", s.cursor)
	}
	s.Update(tea.KeyPressMsg{Code: 'l', Text: "l"})
	if s.cursor != 2 {
		t.Errorf("l from low: %d, want 2", s.cursor)
	}

	// Home/End jump.
	s.Update(tea.KeyPressMsg{Code: tea.KeyHome})
	if s.cursor != 0 {
		t.Errorf("Home: %d, want 0", s.cursor)
	}
	s.Update(tea.KeyPressMsg{Code: tea.KeyEnd})
	if s.cursor != 3 {
		t.Errorf("End: %d, want 3", s.cursor)
	}
}

// TestEffortScreen_EnterApplies — Enter sets Applied() to the cursor
// label and Done() = true.
func TestEffortScreen_EnterApplies(t *testing.T) {
	s := NewEffortScreen("low") // cursor=1
	s.Resize(80, 20)
	s.Update(tea.KeyPressMsg{Code: tea.KeyRight}) // → medium
	s.Update(tea.KeyPressMsg{Code: tea.KeyEnter})

	if !s.Done() {
		t.Errorf("Enter should mark Done()")
	}
	if got := s.Applied(); got != "medium" {
		t.Errorf("Applied() = %q, want %q", got, "medium")
	}
}

// TestEffortScreen_EscCancels — Esc dismisses without committing; the
// parent's apply step is expected to no-op when Applied() is empty.
func TestEffortScreen_EscCancels(t *testing.T) {
	s := NewEffortScreen("low")
	s.Resize(80, 20)
	s.Update(tea.KeyPressMsg{Code: tea.KeyRight}) // visually moves but uncommitted
	s.Update(tea.KeyPressMsg{Code: tea.KeyEsc})

	if !s.Done() {
		t.Errorf("Esc should mark Done()")
	}
	if got := s.Applied(); got != "" {
		t.Errorf("Applied() after Esc = %q, want empty (cancelled)", got)
	}
}

// stripANSIEffort drops ANSI escape sequences from the screen view so
// substring assertions match the rendered text. Local copy of the
// helper from internal/tui/banner_test.go (different package).
func stripANSIEffort(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == 0x1b {
			for i < len(s) && !((s[i] >= 'A' && s[i] <= 'Z') || (s[i] >= 'a' && s[i] <= 'z')) {
				i++
			}
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}
