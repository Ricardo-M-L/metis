package screen

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

func sampleTabs() []HelpTab {
	return []HelpTab{
		{Name: "general", Body: []HelpRow{
			{Heading: "Shortcuts"},
			{Key: "!", Value: "shell mode"},
			{Key: "/", Value: "commands"},
		}},
		{Name: "commands", Body: []HelpRow{
			{Heading: "Built-in commands"},
			{Key: "/help", Value: "show help"},
			{Key: "/quit", Value: "exit"},
			{Key: "/cost", Value: "show cost"},
		}},
		{Name: "custom-commands", Body: []HelpRow{
			{Heading: "Custom commands (skills)"},
			{Key: "/code-review", Value: "review code"},
			{Key: "/debug", Value: "debug bisect"},
		}},
	}
}

// TestHelpScreen_RendersAllTabs — all tab labels appear in the tabs row;
// the active tab's body content shows below; inactive tabs' content
// does NOT show (only one tab body is visible at a time).
func TestHelpScreen_RendersAllTabs(t *testing.T) {
	s := NewHelpScreen("v0.1.1", sampleTabs())
	s.Resize(80, 30)
	out := stripANSIEffort(s.View())

	for _, tabName := range []string{"general", "commands", "custom-commands"} {
		if !strings.Contains(out, tabName) {
			t.Errorf("tab label %q missing:\n%s", tabName, out)
		}
	}
	// Default active tab is index 0 (general). Its content must show.
	if !strings.Contains(out, "shell mode") {
		t.Errorf("first-tab body missing:\n%s", out)
	}
	// commands-tab content shouldn't leak into the view.
	if strings.Contains(out, "show help") {
		t.Errorf("inactive tab content (commands) leaked into view:\n%s", out)
	}
}

// TestHelpScreen_RightCyclesTab — → switches to next tab; ← back.
// At the right edge → is a no-op (clamps).
func TestHelpScreen_RightCyclesTab(t *testing.T) {
	s := NewHelpScreen("v0.1.1", sampleTabs())
	s.Resize(80, 30)

	if s.active != 0 {
		t.Fatalf("initial active = %d, want 0", s.active)
	}
	s.Update(tea.KeyMsg{Type: tea.KeyRight})
	if s.active != 1 {
		t.Errorf("Right: active = %d, want 1 (commands)", s.active)
	}
	s.Update(tea.KeyMsg{Type: tea.KeyRight})
	if s.active != 2 {
		t.Errorf("Right: active = %d, want 2 (custom-commands)", s.active)
	}
	s.Update(tea.KeyMsg{Type: tea.KeyRight}) // already at end
	if s.active != 2 {
		t.Errorf("Right at right-edge should clamp; got %d", s.active)
	}
	s.Update(tea.KeyMsg{Type: tea.KeyLeft})
	if s.active != 1 {
		t.Errorf("Left: active = %d, want 1", s.active)
	}
}

// TestHelpScreen_TabAlsoCycles — Tab key cycles forward with wrap (so
// repeatedly pressing Tab walks the entire help; useful at the right
// edge where → would clamp).
func TestHelpScreen_TabAlsoCycles(t *testing.T) {
	s := NewHelpScreen("v0.1.1", sampleTabs())
	s.Resize(80, 30)

	s.Update(tea.KeyMsg{Type: tea.KeyTab})
	if s.active != 1 {
		t.Errorf("Tab: active = %d, want 1", s.active)
	}
	s.Update(tea.KeyMsg{Type: tea.KeyTab})
	s.Update(tea.KeyMsg{Type: tea.KeyTab}) // 1->2->0 (wrap)
	if s.active != 0 {
		t.Errorf("Tab wrap: active = %d, want 0", s.active)
	}
}

// TestHelpScreen_TabSwitchResetsScroll — switching tab resets scroll
// to top (otherwise the new tab would render mid-content).
func TestHelpScreen_TabSwitchResetsScroll(t *testing.T) {
	tabs := sampleTabs()
	// Pad the first tab with many entries so scroll is meaningful.
	for i := 0; i < 30; i++ {
		tabs[0].Body = append(tabs[0].Body, HelpRow{Key: "/x", Value: "filler"})
	}
	s := NewHelpScreen("v0.1.1", tabs)
	s.Resize(80, 14)

	s.Update(tea.KeyMsg{Type: tea.KeyEnd})
	if s.scroll == 0 {
		t.Fatalf("precondition: End should set scroll > 0")
	}
	s.Update(tea.KeyMsg{Type: tea.KeyRight})
	if s.scroll != 0 {
		t.Errorf("tab switch should reset scroll to 0; got %d", s.scroll)
	}
}

// TestHelpScreen_EscDismisses — Esc / q / Ctrl-C all close the screen.
func TestHelpScreen_EscDismisses(t *testing.T) {
	cases := []tea.KeyMsg{
		{Type: tea.KeyEsc},
		{Type: tea.KeyRunes, Runes: []rune{'q'}},
		{Type: tea.KeyCtrlC},
	}
	for _, key := range cases {
		s := NewHelpScreen("v0.1.1", sampleTabs())
		s.Resize(80, 30)
		s.Update(key)
		if !s.Done() {
			t.Errorf("key %v should dismiss", key)
		}
	}
}

// TestHelpScreen_ScrollWithinTab — ↑↓ / j/k / PgUp/Dn / g/G work within
// the active tab without affecting the tab selection.
func TestHelpScreen_ScrollWithinTab(t *testing.T) {
	tabs := sampleTabs()
	// Many lines on first tab.
	for i := 0; i < 30; i++ {
		tabs[0].Body = append(tabs[0].Body, HelpRow{Key: "/x", Value: "filler"})
	}
	s := NewHelpScreen("v0.1.1", tabs)
	s.Resize(80, 14)

	s.Update(tea.KeyMsg{Type: tea.KeyDown})
	if s.scroll != 1 {
		t.Errorf("Down: scroll = %d, want 1", s.scroll)
	}
	s.Update(tea.KeyMsg{Type: tea.KeyEnd})
	if s.scroll == 0 {
		t.Errorf("End should jump to bottom; scroll = %d", s.scroll)
	}
	s.Update(tea.KeyMsg{Type: tea.KeyHome})
	if s.scroll != 0 {
		t.Errorf("Home: scroll = %d, want 0", s.scroll)
	}
	if s.active != 0 {
		t.Errorf("scroll keys should not change active tab; got %d", s.active)
	}
}
