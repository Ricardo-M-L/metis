package tui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/Ricardo-M-L/metis/internal/llm"
)

// TestEffortWidget_BareSlashOpensSlider — typing `/effort` keeps the chat
// mounted and renders the selector directly below the input.
func TestEffortWidget_BareSlashOpensSlider(t *testing.T) {
	m := newSlashTestModel(t)
	m.input.SetValue("/effort")
	pressEnter(t, m)

	if m.activeScreen != nil {
		t.Fatalf("/effort must not replace chat with activeScreen; got %T", m.activeScreen)
	}
	if m.effortPicker == nil {
		t.Fatal("/effort should open the inline effort picker")
	}
	if got := m.input.Value(); got != "/effort" {
		t.Fatalf("input should retain /effort while picker is open; got %q", got)
	}
	viewState := m.View()
	view := viewState.Content
	if !strings.Contains(view, "Faster") || !strings.Contains(view, "Smarter") {
		t.Errorf("inline picker missing Faster/Smarter labels:\n%s", view)
	}
	if !strings.Contains(view, "(test session)") {
		t.Errorf("opening /effort hid the existing transcript:\n%s", view)
	}
	if viewState.Cursor != nil {
		t.Error("textarea cursor should be hidden while the effort picker owns input")
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
// the loop effort to the chosen level via applyScreenResult.
func TestEffortWidget_ApplyUpdatesLoop(t *testing.T) {
	m := newSlashTestModel(t)
	m.loop.SetEffort(llm.EffortMedium)
	m.input.SetValue("/effort")
	pressEnter(t, m)

	// Cursor starts on the live medium setting. Move right to high, apply.
	m.Update(tea.KeyPressMsg{Code: tea.KeyRight}) // → high
	m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})

	if m.effortPicker != nil {
		t.Error("Enter should dismiss the inline effort picker")
	}
	if got := m.loop.EffortValue(); got != llm.EffortHigh {
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
// the loop effort even if the cursor moved during the session.
func TestEffortWidget_EscPreservesLoop(t *testing.T) {
	m := newSlashTestModel(t)
	m.loop.SetEffort(llm.EffortLow)
	before := len(m.messages)
	m.input.SetValue("/effort")
	pressEnter(t, m)

	// Move cursor visually but cancel.
	m.Update(tea.KeyPressMsg{Code: tea.KeyRight})
	m.Update(tea.KeyPressMsg{Code: tea.KeyRight})
	m.Update(tea.KeyPressMsg{Code: tea.KeyEsc})

	if m.effortPicker != nil {
		t.Error("Esc should dismiss inline effort picker")
	}
	if got := m.loop.EffortValue(); got != llm.EffortLow {
		t.Errorf("Esc should preserve loop.Effort; got %q, want %q (low)", got, llm.EffortLow)
	}
	if len(m.messages) != before {
		t.Errorf("quiet cancel should not append transcript noise: before=%d after=%d", before, len(m.messages))
	}
}

func TestEffortWidget_FreshSessionKeepsWelcomeVisible(t *testing.T) {
	m := newSlashTestModel(t)
	m.messages = nil
	m.input.SetValue("/effort")
	pressEnter(t, m)

	view := stripANSI(m.View().Content)
	if !strings.Contains(view, "metis v") || !strings.Contains(view, "Effort") {
		t.Fatalf("fresh-session effort picker should keep welcome + inline chooser visible:\n%s", view)
	}
}

func TestEffortWidget_BlocksPasteWhilePickerOwnsKeyboard(t *testing.T) {
	m := newSlashTestModel(t)
	m.input.SetValue("/effort")
	pressEnter(t, m)

	m.Update(tea.PasteMsg{Content: "must-not-leak"})
	if got := m.input.Value(); got != "/effort" {
		t.Fatalf("paste leaked into picker-owned input: %q", got)
	}
}

func TestEffortWidget_TurnActiveBareCommandStaysLocal(t *testing.T) {
	m := newSlashTestModel(t)
	m.turnActive = true
	m.loop.SetEffort(llm.EffortMedium)
	m.input.SetValue("/effort")
	pressEnter(t, m)

	if m.effortPicker == nil {
		t.Fatal("turn-active /effort should open the local inline picker")
	}
	if got := m.loop.SteerInjectDrainForTest(); got != "" {
		t.Fatalf("turn-active /effort leaked into model steering: %q", got)
	}
	m.Update(tea.KeyPressMsg{Code: tea.KeyRight})
	m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if got := m.loop.EffortValue(); got != llm.EffortHigh {
		t.Fatalf("turn-active effort choice = %q, want high", got)
	}
}

func TestEffortWidget_TurnActiveExplicitCommandStaysLocal(t *testing.T) {
	m := newSlashTestModel(t)
	m.turnActive = true
	m.input.SetValue("/effort low")
	pressEnter(t, m)

	if got := m.loop.EffortValue(); got != llm.EffortLow {
		t.Fatalf("turn-active explicit effort = %q, want low", got)
	}
	if got := m.loop.SteerInjectDrainForTest(); got != "" {
		t.Fatalf("turn-active explicit /effort leaked into steering: %q", got)
	}
}
