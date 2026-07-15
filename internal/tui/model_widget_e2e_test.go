package tui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/Ricardo-M-L/metis/internal/config"
	"github.com/Ricardo-M-L/metis/internal/tui/screen"
)

func configureModelWidgetAnthropic(m *Model) {
	m.providerName = "anthropic"
	m.cfg.Provider.Default = "anthropic"
	m.cfg.Provider.Anthropic.APIKey = "test-anthropic-key"
	m.cfg.Provider.Anthropic.Model = "claude-sonnet-4-6"
	m.cfg.Provider.Anthropic.ContextWindow = 200_000
}

// TestModelWidget_BareSlashOpensPicker — typing /model opens the picker
// widget (claude-code parity for browseable model selection).
func TestModelWidget_BareSlashOpensPicker(t *testing.T) {
	m := newSlashTestModel(t)
	m.input.SetValue("/model")
	pressEnter(t, m)

	if m.activeScreen == nil {
		t.Fatalf("/model should open ModelScreen; activeScreen is nil")
	}
	if _, ok := m.activeScreen.(*screen.ModelScreen); !ok {
		t.Errorf("activeScreen has wrong type: %T", m.activeScreen)
	}
	view := m.activeScreen.View()
	if !strings.Contains(view, "Pick a model") {
		t.Errorf("ModelScreen view missing title; got:\n%s", view)
	}
}

// TestModelWidget_AliasOpensPicker — /m alias also opens the picker.
func TestModelWidget_AliasOpensPicker(t *testing.T) {
	m := newSlashTestModel(t)
	m.input.SetValue("/m")
	pressEnter(t, m)
	if _, ok := m.activeScreen.(*screen.ModelScreen); !ok {
		t.Errorf("/m alias should open ModelScreen; got %T", m.activeScreen)
	}
}

// TestModelWidget_ExplicitArgStaysInline — /model <id> still works
// inline so scripted usage is unchanged.
func TestModelWidget_ExplicitArgStaysInline(t *testing.T) {
	m := newSlashTestModel(t)
	configureModelWidgetAnthropic(m)
	m.input.SetValue("/model claude-opus-4-7")
	pressEnter(t, m)

	if m.activeScreen != nil {
		t.Errorf("/model with arg should NOT open picker; got %T", m.activeScreen)
	}
	if m.loop.Model != "claude-opus-4-7" {
		t.Errorf("/model claude-opus-4-7 should set loop.Model; got %q", m.loop.Model)
	}
}

// TestModelWidget_ApplyUpdatesModel — Enter on the picker commits the
// chosen model to both m.model and m.loop.Model.
func TestModelWidget_ApplyUpdatesModel(t *testing.T) {
	m := newSlashTestModel(t)
	configureModelWidgetAnthropic(m)
	m.input.SetValue("/model")
	pressEnter(t, m)

	// Cursor starts wherever m.model matches; for newSlashTestModel
	// it's "claude-sonnet-4-6" which is index 1 in builtinModelChoices.
	// Move up once to Opus; the configured Anthropic profile makes this a
	// real successful Provider rebuild rather than a string-only test path.
	m.Update(tea.KeyPressMsg{Code: tea.KeyUp})
	m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})

	if m.activeScreen != nil {
		t.Errorf("Enter should dismiss picker; activeScreen still %T", m.activeScreen)
	}
	if m.loop.Model == "claude-sonnet-4-6" {
		t.Errorf("model should have changed from initial; got %q", m.loop.Model)
	}
	// Confirmation appended as success role.
	found := false
	for _, msg := range m.messages {
		if msg.Role == "success" && strings.Contains(msg.Content, "model:") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected success-role 'model: ...' confirmation; got: %+v", messageContents(m))
	}
}

func TestModelWidget_BuildFailurePreservesModelAndDoesNotReportSuccess(t *testing.T) {
	m := newSlashTestModel(t)
	m.providerName = "missing-profile"
	m.cfg = &config.Config{}
	oldProvider := m.loop.Provider
	oldModel := m.model
	oldLoopModel := m.loop.Model
	before := len(m.messages)

	picker := screen.NewModelScreen(oldModel, []screen.ModelChoice{{ID: "new-model", Provider: "missing-profile"}})
	picker.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	m.applyScreenResult(picker)

	if m.model != oldModel || m.loop.Model != oldLoopModel || m.loop.Provider != oldProvider {
		t.Fatalf("failed picker rebuild changed live state: model=%q loopModel=%q provider=%T", m.model, m.loop.Model, m.loop.Provider)
	}
	var warning bool
	for _, msg := range m.messages[before:] {
		if msg.Role == "success" && strings.Contains(msg.Content, "model:") {
			t.Fatalf("failed model switch reported success: %+v", m.messages[before:])
		}
		if msg.Role == "warning" && strings.Contains(msg.Content, "previous model remains active") {
			warning = true
		}
	}
	if !warning {
		t.Fatalf("failed model switch did not report an actionable warning: %+v", m.messages[before:])
	}
}
