package tui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/Ricardo-M-L/metis/internal/tui/screen"
)

// TestThemeWidget_BareSlashOpensCycle — typing /theme opens the cycle
// widget (claude-code parity for visual theme selection).
func TestThemeWidget_BareSlashOpensCycle(t *testing.T) {
	m := newSlashTestModel(t)
	m.input.SetValue("/theme")
	pressEnter(t, m)

	if m.activeScreen == nil {
		t.Fatalf("/theme should open ThemeScreen; activeScreen is nil")
	}
	if _, ok := m.activeScreen.(*screen.ThemeScreen); !ok {
		t.Errorf("activeScreen has wrong type: %T", m.activeScreen)
	}
	view := m.activeScreen.View()
	if !strings.Contains(view, "Theme") {
		t.Errorf("ThemeScreen view missing label; got:\n%s", view)
	}
	// The current theme name (default 'dark') must show.
	if !strings.Contains(view, currentTheme.Name) {
		t.Errorf("ThemeScreen view missing current theme name (%s):\n%s", currentTheme.Name, view)
	}
}

// TestThemeWidget_ExplicitArgStaysInline — /theme dark still works
// inline (cmdTheme path).
func TestThemeWidget_ExplicitArgStaysInline(t *testing.T) {
	m := newSlashTestModel(t)
	m.input.SetValue("/theme dark")
	pressEnter(t, m)

	if m.activeScreen != nil {
		t.Errorf("/theme dark should NOT open widget; got %T", m.activeScreen)
	}
}

// TestThemeWidget_ApplyChangesTheme — Enter on the picker swaps the
// active theme via SwitchTheme.
func TestThemeWidget_ApplyChangesTheme(t *testing.T) {
	// Snapshot starting theme so we can restore.
	startTheme := currentTheme.Name
	t.Cleanup(func() {
		SwitchTheme(startTheme)
	})

	// Force a known starting theme.
	SwitchTheme("dark")

	m := newSlashTestModel(t)
	m.input.SetValue("/theme")
	pressEnter(t, m)

	// Cursor on dark; press Right twice to reach dark-daltonized.
	m.Update(tea.KeyPressMsg{Code: tea.KeyRight})
	m.Update(tea.KeyPressMsg{Code: tea.KeyRight})
	m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})

	// SwitchTheme should have run; currentTheme name must have changed.
	if currentTheme.Name == "dark" {
		t.Errorf("expected theme to have changed from 'dark'; still 'dark'")
	}
	// Confirmation appended as success role.
	found := false
	for _, msg := range m.messages {
		if msg.Role == "success" && strings.Contains(msg.Content, "theme:") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected success-role 'theme: ...' confirmation; got: %+v", messageContents(m))
	}
}

// TestThemeWidget_EscPreservesTheme — Esc dismisses without changing
// the active theme.
func TestThemeWidget_EscPreservesTheme(t *testing.T) {
	startTheme := currentTheme.Name
	t.Cleanup(func() {
		SwitchTheme(startTheme)
	})
	SwitchTheme("dark")

	m := newSlashTestModel(t)
	m.input.SetValue("/theme")
	pressEnter(t, m)

	m.Update(tea.KeyPressMsg{Code: tea.KeyRight}) // visually moves
	m.Update(tea.KeyPressMsg{Code: tea.KeyEsc})

	if currentTheme.Name != "dark" {
		t.Errorf("Esc should not change theme; currentTheme = %q", currentTheme.Name)
	}
}
