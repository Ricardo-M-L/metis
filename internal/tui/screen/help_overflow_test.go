package screen

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

// makeLongTab returns a tab with `n` rows so we can exercise overflow.
func makeLongTab(name string, n int) HelpTab {
	body := make([]HelpRow, n)
	for i := range body {
		body[i] = HelpRow{Key: "/cmd", Value: "filler"}
	}
	return HelpTab{Name: name, Body: body}
}

// TestHelpScreen_FooterAdaptsToContentSize — when content fits the
// viewport, the footer must not promise "↑/↓ scroll" — it would mislead
// the user (the keys are no-ops in that state). Footer should drop the
// scroll keybind when canScroll is false.
func TestHelpScreen_FooterAdaptsToContentSize(t *testing.T) {
	// Short tab — fits viewport.
	short := []HelpTab{
		{Name: "single", Body: []HelpRow{{Value: "one line"}, {Value: "two lines"}}},
	}
	s := NewHelpScreen("v1", short)
	s.Resize(80, 30)
	out := stripANSIEffort(s.View())
	if strings.Contains(out, "↑/↓") {
		t.Errorf("footer should NOT show ↑/↓ scroll when content fits viewport; got:\n%s", out)
	}
	if !strings.Contains(out, "Esc") {
		t.Errorf("footer should always show Esc to close:\n%s", out)
	}

	// Long tab — overflows.
	long := []HelpTab{makeLongTab("long", 50)}
	s = NewHelpScreen("v1", long)
	s.Resize(80, 14) // bodyHeight = 14-5 = 9, < 50 → overflow
	out = stripANSIEffort(s.View())
	if !strings.Contains(out, "↑/↓") {
		t.Errorf("footer SHOULD show ↑/↓ scroll when content overflows; got:\n%s", out)
	}
}

// TestHelpScreen_OverflowIndicators — when scrolled past the top and
// before the end, "↑ N more above" and "↓ N more below" must appear at
// top and bottom of the visible body. Mirrors claude-code's chevron
// arrows in images #8/#9.
func TestHelpScreen_OverflowIndicators(t *testing.T) {
	s := NewHelpScreen("v1", []HelpTab{makeLongTab("long", 40)})
	s.Resize(80, 14) // bodyHeight = 9

	// Initial: scroll=0 → only "↓ N below" should appear.
	out := stripANSIEffort(s.View())
	if !strings.Contains(out, "more below") {
		t.Errorf("initial render should show '↓ N more below' indicator; got:\n%s", out)
	}
	if strings.Contains(out, "more above") {
		t.Errorf("at scroll=0, should NOT show 'more above'; got:\n%s", out)
	}

	// Scroll to middle: both indicators present.
	s.scroll = 15
	s.clampScroll()
	out = stripANSIEffort(s.View())
	if !strings.Contains(out, "more above") {
		t.Errorf("at scroll=15, should show 'more above'; got:\n%s", out)
	}
	if !strings.Contains(out, "more below") {
		t.Errorf("at scroll=15, should show 'more below'; got:\n%s", out)
	}

	// Scroll to end: only "↑ N above" — no more "below" since all
	// remaining content fits.
	s.Update(tea.KeyPressMsg{Code: tea.KeyEnd})
	out = stripANSIEffort(s.View())
	if !strings.Contains(out, "more above") {
		t.Errorf("at end, should show 'more above'; got:\n%s", out)
	}
	if strings.Contains(out, "more below") {
		t.Errorf("at end, should NOT show 'more below'; got:\n%s", out)
	}
}

// TestHelpScreen_BodyCappedAtMax — even on a huge terminal, body height
// is capped so the modal feels modal-sized (and ↑↓ scroll is meaningful
// when tab content is long).
func TestHelpScreen_BodyCappedAtMax(t *testing.T) {
	s := NewHelpScreen("v1", []HelpTab{makeLongTab("long", 50)})
	s.Resize(200, 200) // way beyond the cap

	if got := s.bodyHeight(); got > helpMaxBody {
		t.Errorf("bodyHeight() on huge terminal = %d, want <= %d", got, helpMaxBody)
	}
	// Verify scroll is meaningful: jump cursor to end forces scroll
	// past 0 (otherwise the cap had no effect).
	s.Update(tea.KeyPressMsg{Code: tea.KeyEnd})
	if s.scroll == 0 {
		t.Errorf("End on capped body should advance scroll; got 0")
	}
}
