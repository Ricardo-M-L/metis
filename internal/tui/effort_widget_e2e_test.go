package tui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/tui/screen"
)

// TestEffortWidget_BareSlashOpensSlider — typing `/effort` (no args)
// must open the EffortScreen widget rather than inlining a one-line
// confirmation. claude-code parity (image #6 in user feedback).
func TestEffortWidget_BareSlashOpensSlider(t *testing.T) {
	m := newSlashTestModel(t)
	m.input.SetValue("/effort")
	pressEnter(t, m)

	if m.activeScreen == nil {
		t.Fatalf("/effort with no args should open EffortScreen; activeScreen is nil")
	}
	if _, ok := m.activeScreen.(*screen.EffortScreen); !ok {
		t.Errorf("activeScreen has wrong type: %T", m.activeScreen)
	}
	view := m.activeScreen.View()
	if !strings.Contains(view, "Speed") || !strings.Contains(view, "Intelligence") {
		t.Errorf("EffortScreen view missing Speed/Intelligence labels:\n%s", view)
	}
}

// TestEffortWidget_ExplicitArgStaysInline — typing `/effort high` keeps
// the inline confirmation path so scripted use / palette autocomplete
// don't suddenly hijack the screen.
func TestEffortWidget_ExplicitArgStaysInline(t *testing.T) {
	m := newSlashTestModel(t)
	m.input.SetValue("/effort high")
	pressEnter(t, m)

	if m.activeScreen != nil {
		t.Errorf("/effort high should NOT open a screen; got: %T", m.activeScreen)
	}
	// The inline path appends a confirmation containing "high".
	found := false
	for _, msg := range m.messages {
		if strings.Contains(msg.Content, "high") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected inline confirmation containing 'high'; got: %+v", messageContents(m))
	}
}

// TestEffortWidget_ApplyUpdatesLoop — Enter on the slider sets
// m.loop.Effort to the chosen level via applyScreenResult.
func TestEffortWidget_ApplyUpdatesLoop(t *testing.T) {
	m := newSlashTestModel(t)
	m.input.SetValue("/effort")
	pressEnter(t, m)

	// Cursor starts on whatever m.loop.Effort was at construction time
	// — for newSlashTestModel that's empty, which falls back to medium.
	// Move right to "high" then Enter.
	m.Update(tea.KeyPressMsg{Code: tea.KeyRight}) // → high
	m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})

	if m.activeScreen != nil {
		t.Errorf("Enter should dismiss the EffortScreen; activeScreen still: %T", m.activeScreen)
	}
	if got := m.loop.Effort; got != llm.EffortHigh {
		t.Errorf("after Enter on 'high', loop.Effort = %q, want %q", got, llm.EffortHigh)
	}
	// Confirmation message uses success role (Phase B integration).
	found := false
	for _, msg := range m.messages {
		if msg.Role == "success" && strings.Contains(msg.Content, "high") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected success-role confirmation containing 'high'; got: %+v", messageContents(m))
	}
}

// TestEffortWidget_EscPreservesLoop — Esc dismisses without changing
// m.loop.Effort even if the cursor moved during the session.
func TestEffortWidget_EscPreservesLoop(t *testing.T) {
	m := newSlashTestModel(t)
	m.loop.Effort = llm.EffortLow
	m.input.SetValue("/effort")
	pressEnter(t, m)

	// Move cursor visually but cancel.
	m.Update(tea.KeyPressMsg{Code: tea.KeyRight})
	m.Update(tea.KeyPressMsg{Code: tea.KeyRight})
	m.Update(tea.KeyPressMsg{Code: tea.KeyEsc})

	if m.activeScreen != nil {
		t.Errorf("Esc should dismiss EffortScreen")
	}
	if got := m.loop.Effort; got != llm.EffortLow {
		t.Errorf("Esc should preserve loop.Effort; got %q, want %q (low)", got, llm.EffortLow)
	}
}
