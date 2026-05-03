package screen

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

func sampleDetailSections() []DetailSection {
	return []DetailSection{
		{Heading: "Description", Lines: []string{"a brief description of the resource"}},
		{Heading: "Allowed tools", Lines: []string{"Read · Edit · Bash"}},
		{Heading: "Body", Lines: []string{
			"You are a helpful assistant.",
			"Follow these rules:",
			"  - Be concise",
			"  - Always cite sources",
		}},
	}
}

// TestDetailScreen_RendersSections — every heading + body line appears.
func TestDetailScreen_RendersSections(t *testing.T) {
	s := NewDetailScreen("/skills", "code-review", sampleDetailSections())
	s.Resize(120, 30)
	out := stripANSIEffort(s.View())

	if !strings.Contains(out, "code-review") {
		t.Errorf("subtitle 'code-review' missing:\n%s", out)
	}
	for _, want := range []string{"Description", "Allowed tools", "Body", "a brief", "Read", "concise"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q:\n%s", want, out)
		}
	}
}

// TestDetailScreen_EscDismisses — Esc / q / Ctrl-C all set Done().
func TestDetailScreen_EscDismisses(t *testing.T) {
	cases := []tea.KeyPressMsg{
		{Code: tea.KeyEsc},
		{Code: 'c', Mod: tea.ModCtrl},
		{Code: 'q', Text: "q"},
	}
	for _, key := range cases {
		s := NewDetailScreen("/x", "y", sampleDetailSections())
		s.Resize(80, 20)
		s.Update(key)
		if !s.Done() {
			t.Errorf("key %v should dismiss", key)
		}
	}
}

// TestDetailScreen_Scrolls — long content scrolls and the overflow
// indicators appear.
func TestDetailScreen_Scrolls(t *testing.T) {
	long := []DetailSection{{Heading: "Long", Lines: make([]string, 50)}}
	for i := range long[0].Lines {
		long[0].Lines[i] = "filler line"
	}
	s := NewDetailScreen("/x", "y", long)
	s.Resize(80, 14)

	if s.scroll != 0 {
		t.Fatalf("initial scroll=%d, want 0", s.scroll)
	}
	s.Update(tea.KeyPressMsg{Code: tea.KeyEnd})
	if s.scroll == 0 {
		t.Errorf("End should advance scroll")
	}
	s.Update(tea.KeyPressMsg{Code: tea.KeyHome})
	if s.scroll != 0 {
		t.Errorf("Home should reset scroll")
	}
}

// TestDetailScreen_FooterAdaptiveOnContentSize — short body hides
// scroll keys; long body shows them.
func TestDetailScreen_FooterAdaptiveOnContentSize(t *testing.T) {
	short := NewDetailScreen("/x", "y", []DetailSection{
		{Heading: "Brief", Lines: []string{"one"}},
	})
	short.Resize(80, 30)
	out := stripANSIEffort(short.View())
	if strings.Contains(out, "PgUp") {
		t.Errorf("short detail footer should hide PgUp; got:\n%s", out)
	}

	long := NewDetailScreen("/x", "y", []DetailSection{{Heading: "Long", Lines: make([]string, 50)}})
	long.Resize(80, 14)
	for i := range long.sections[0].Lines {
		long.sections[0].Lines[i] = "filler"
	}
	out = stripANSIEffort(long.View())
	if !strings.Contains(out, "PgUp") {
		t.Errorf("long detail footer should show PgUp; got:\n%s", out)
	}
}
