package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// TestWidgetHint_AllPhaseCWidgetsCovered — Phase C ports five widgets
// (effort / help / model / theme / permissions). Each one's slash name
// must surface a non-empty hint so the palette user sees that pressing
// Enter opens a UI rather than dumping text.
func TestWidgetHint_AllPhaseCWidgetsCovered(t *testing.T) {
	for _, name := range []string{"effort", "help", "model", "theme", "permissions"} {
		if got := widgetHint(name); got == "" {
			t.Errorf("widgetHint(%q) returned empty; widget commands must surface a hint", name)
		}
	}
}

// TestWidgetHint_AliasesAlsoCovered — h/?/m/perms should also resolve.
func TestWidgetHint_AliasesAlsoCovered(t *testing.T) {
	for _, alias := range []string{"h", "?", "m", "perms"} {
		if got := widgetHint(alias); got == "" {
			t.Errorf("widgetHint(%q) returned empty; alias should resolve", alias)
		}
	}
}

// TestWidgetHint_NonWidgetCommandsReturnEmpty — non-widget commands like
// /quit / /clear must NOT get a hint (so the existing Description shows).
func TestWidgetHint_NonWidgetCommandsReturnEmpty(t *testing.T) {
	for _, name := range []string{"quit", "clear", "title", "save", "tag", "compact"} {
		if got := widgetHint(name); got != "" {
			t.Errorf("widgetHint(%q) = %q, want empty (non-widget)", name, got)
		}
	}
}

// TestPalette_RendersWidgetHint — end-to-end through the palette
// renderer: typing /eff opens the palette and shows the widget hint
// for /effort instead of the registered Description.
func TestPalette_RendersWidgetHint(t *testing.T) {
	m := newSlashTestModel(t)
	for _, r := range "/eff" {
		m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
	if !m.showPalette {
		t.Fatalf("palette did not open on /eff")
	}
	out := stripANSI(renderPalette(m))
	want := "→ slider"
	if !strings.Contains(out, want) {
		t.Errorf("palette should contain %q for /effort; got:\n%s", want, out)
	}
}
