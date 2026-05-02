package tui

import (
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/tui/screen"
)

// TestHelpWidget_OpensTabbedScreen — typing /help routes through the
// new HelpScreen widget instead of the old flat infobox. claude-code
// parity (image #7-9 in user feedback).
func TestHelpWidget_OpensTabbedScreen(t *testing.T) {
	m := newSlashTestModel(t)
	m.input.SetValue("/help")
	pressEnter(t, m)

	if m.activeScreen == nil {
		t.Fatalf("/help should open HelpScreen; activeScreen is nil")
	}
	if _, ok := m.activeScreen.(*screen.HelpScreen); !ok {
		t.Errorf("activeScreen has wrong type: %T", m.activeScreen)
	}
	view := m.activeScreen.View()
	for _, want := range []string{"general", "commands", "custom-commands"} {
		if !strings.Contains(view, want) {
			t.Errorf("HelpScreen view missing tab label %q:\n%s", want, view)
		}
	}
}

// TestHelpWidget_AliasesAlsoOpenScreen — /h and /? also route to the
// widget (they're documented aliases in REPLCommandRegistry).
func TestHelpWidget_AliasesAlsoOpenScreen(t *testing.T) {
	for _, alias := range []string{"/h", "/?"} {
		t.Run(alias, func(t *testing.T) {
			m := newSlashTestModel(t)
			m.input.SetValue(alias)
			pressEnter(t, m)
			if _, ok := m.activeScreen.(*screen.HelpScreen); !ok {
				t.Errorf("%s should open HelpScreen; got %T", alias, m.activeScreen)
			}
		})
	}
}

// TestHelpWidget_TabContentsArePopulated — sanity that the three tabs
// have non-empty bodies. Catches a regression where buildHelpTabs
// returned empty rows.
func TestHelpWidget_TabContentsArePopulated(t *testing.T) {
	m := newSlashTestModel(t)
	tabs := m.buildHelpTabs()
	if len(tabs) != 3 {
		t.Fatalf("buildHelpTabs: got %d tabs, want 3", len(tabs))
	}
	for _, tab := range tabs {
		if len(tab.Body) == 0 {
			t.Errorf("tab %q has empty body", tab.Name)
		}
	}
}
