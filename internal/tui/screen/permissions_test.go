package screen

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

func sampleRules() []PermRule {
	return []PermRule{
		{Verb: "allow", Match: "Bash", Source: "session"},
		{Verb: "allow", Match: "Read", Source: "~/.metis/config.toml"},
		{Verb: "deny", Match: "WebFetch", Source: "session"},
	}
}

// TestPermissionsScreen_RendersModeAndRules — both panels appear in the
// view with the active mode highlighted and rules listed below.
func TestPermissionsScreen_RendersModeAndRules(t *testing.T) {
	s := NewPermissionsScreen("auto", sampleRules())
	s.Resize(100, 20)
	out := stripANSIEffort(s.View())

	if !strings.Contains(out, "Mode") {
		t.Errorf("missing Mode label:\n%s", out)
	}
	if !strings.Contains(out, "auto") {
		t.Errorf("active mode 'auto' missing:\n%s", out)
	}
	for _, r := range sampleRules() {
		if !strings.Contains(out, r.Match) {
			t.Errorf("rule match %q missing:\n%s", r.Match, out)
		}
	}
	if !strings.Contains(out, "Rules (3)") {
		t.Errorf("missing rule count header:\n%s", out)
	}
}

// TestPermissionsScreen_EmptyRulesShowsHint — when no rules, an italic
// hint replaces the (empty) list.
func TestPermissionsScreen_EmptyRulesShowsHint(t *testing.T) {
	s := NewPermissionsScreen("auto", nil)
	s.Resize(100, 20)
	out := stripANSIEffort(s.View())

	if !strings.Contains(out, "no explicit rules") {
		t.Errorf("missing empty-state hint:\n%s", out)
	}
	if !strings.Contains(out, "Rules (0)") {
		t.Errorf("missing zero rule count:\n%s", out)
	}
}

// TestPermissionsScreen_InitialModeCursor — cursor seeded by `currentMode`.
func TestPermissionsScreen_InitialModeCursor(t *testing.T) {
	cases := []struct {
		current string
		want    int
	}{
		{"ask", 0},
		{"auto", 1},
		{"bypass", 2},
		{"plan", 3},
		{"deny", 4},
		{"unknown", 1}, // fallback to auto
	}
	for _, tc := range cases {
		s := NewPermissionsScreen(tc.current, nil)
		if s.modeCursor != tc.want {
			t.Errorf("currentMode=%q: cursor = %d, want %d", tc.current, s.modeCursor, tc.want)
		}
	}
}

// TestPermissionsScreen_ModeCyclesWraparound — ←/→ cycle the mode set.
func TestPermissionsScreen_ModeCyclesWraparound(t *testing.T) {
	s := NewPermissionsScreen("ask", nil) // cursor=0
	s.Resize(100, 20)

	s.Update(tea.KeyMsg{Type: tea.KeyLeft}) // wrap to 4 (deny)
	if s.modeCursor != 4 {
		t.Errorf("Left at 0 should wrap to 4; got %d", s.modeCursor)
	}
	s.Update(tea.KeyMsg{Type: tea.KeyRight}) // wrap back to 0
	if s.modeCursor != 0 {
		t.Errorf("Right at 4 should wrap to 0; got %d", s.modeCursor)
	}
}

// TestPermissionsScreen_RuleListScrollsIndependently — ↑/↓ scrolls rules
// without touching the mode cursor.
func TestPermissionsScreen_RuleListScrollsIndependently(t *testing.T) {
	rules := make([]PermRule, 30)
	for i := range rules {
		rules[i] = PermRule{Verb: "allow", Match: "Tool", Source: "session"}
	}
	s := NewPermissionsScreen("auto", rules)
	s.Resize(100, 14)

	startMode := s.modeCursor
	s.Update(tea.KeyMsg{Type: tea.KeyDown})
	if s.modeCursor != startMode {
		t.Errorf("KeyDown changed mode cursor from %d to %d", startMode, s.modeCursor)
	}
	if s.rulesScroll != 1 {
		t.Errorf("KeyDown should scroll rules; rulesScroll = %d, want 1", s.rulesScroll)
	}
}

// TestPermissionsScreen_EnterApplies — Enter sets Applied() to mode name.
func TestPermissionsScreen_EnterApplies(t *testing.T) {
	s := NewPermissionsScreen("auto", nil)
	s.Resize(100, 20)
	s.Update(tea.KeyMsg{Type: tea.KeyRight}) // → bypass
	s.Update(tea.KeyMsg{Type: tea.KeyEnter})

	if !s.Done() {
		t.Errorf("Enter should mark Done()")
	}
	if got := s.Applied(); got != "bypass" {
		t.Errorf("Applied() = %q, want %q", got, "bypass")
	}
}

// TestPermissionsScreen_EscCancels — Esc dismisses without committing.
func TestPermissionsScreen_EscCancels(t *testing.T) {
	s := NewPermissionsScreen("auto", nil)
	s.Resize(100, 20)
	s.Update(tea.KeyMsg{Type: tea.KeyRight})
	s.Update(tea.KeyMsg{Type: tea.KeyEsc})

	if !s.Done() {
		t.Errorf("Esc should mark Done()")
	}
	if got := s.Applied(); got != "" {
		t.Errorf("Applied() after Esc = %q, want empty", got)
	}
}
