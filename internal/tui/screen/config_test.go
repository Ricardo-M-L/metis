package screen

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

func configScreenFixture() *ConfigScreen {
	return NewConfigScreen([]ConfigSetting{
		{Key: "ui.markdown", Label: "Markdown", Description: "Render markdown", Value: "true", Options: []string{"true", "false"}},
		{Key: "ui.thinking_display", Label: "Thinking", Description: "Reasoning visibility", Value: "auto", Options: []string{"show", "auto", "hide"}},
		{Key: "session.max_iterations", Label: "Max iterations", Description: "Agent turn cap", Value: "100"},
	})
}

func pressConfigKey(t *testing.T, s *ConfigScreen, key tea.KeyPressMsg) {
	t.Helper()
	next, _ := s.Update(key)
	if next != s {
		t.Fatalf("Update returned %T, want same *ConfigScreen", next)
	}
}

func TestConfigScreenSearchPreviewAndApply(t *testing.T) {
	s := configScreenFixture()
	s.Resize(80, 24)
	pressConfigKey(t, s, tea.KeyPressMsg{Text: "/", Code: '/'})
	for _, r := range "thinking" {
		pressConfigKey(t, s, tea.KeyPressMsg{Text: string(r), Code: r})
	}
	if len(s.filtered) != 1 || s.settings[s.filtered[0]].Key != "ui.thinking_display" {
		t.Fatalf("search result = %+v", s.filtered)
	}
	pressConfigKey(t, s, tea.KeyPressMsg{Code: tea.KeyEnter})
	pressConfigKey(t, s, tea.KeyPressMsg{Code: tea.KeyRight})
	if got := s.values["ui.thinking_display"]; got != "hide" {
		t.Fatalf("preview value = %q, want hide", got)
	}
	pressConfigKey(t, s, tea.KeyPressMsg{Code: tea.KeyEnter})
	if !s.Done() || !s.Applied() {
		t.Fatal("Enter from list must apply and dismiss")
	}
	changes := s.Changes()
	if len(changes) != 1 || changes[0] != (ConfigChange{Key: "ui.thinking_display", Value: "hide"}) {
		t.Fatalf("Changes = %+v", changes)
	}
}

func TestConfigScreenEscapeCancelsAllStagedChanges(t *testing.T) {
	s := configScreenFixture()
	pressConfigKey(t, s, tea.KeyPressMsg{Code: tea.KeyRight})
	if s.values["ui.markdown"] == "true" {
		t.Fatal("test did not stage preview")
	}
	pressConfigKey(t, s, tea.KeyPressMsg{Code: tea.KeyEsc})
	if !s.Done() || s.Applied() || len(s.Changes()) != 0 {
		t.Fatalf("cancel state: done=%v applied=%v changes=%+v", s.Done(), s.Applied(), s.Changes())
	}
}

func TestConfigScreenTextEditAndDiscard(t *testing.T) {
	s := configScreenFixture()
	pressConfigKey(t, s, tea.KeyPressMsg{Code: tea.KeyDown})
	pressConfigKey(t, s, tea.KeyPressMsg{Code: tea.KeyDown})
	pressConfigKey(t, s, tea.KeyPressMsg{Text: "i", Code: 'i'})
	for range 3 {
		pressConfigKey(t, s, tea.KeyPressMsg{Code: tea.KeyBackspace})
	}
	for _, r := range "250" {
		pressConfigKey(t, s, tea.KeyPressMsg{Text: string(r), Code: r})
	}
	pressConfigKey(t, s, tea.KeyPressMsg{Code: tea.KeyEnter})
	if got := s.values["session.max_iterations"]; got != "250" {
		t.Fatalf("edited value = %q", got)
	}

	s2 := configScreenFixture()
	pressConfigKey(t, s2, tea.KeyPressMsg{Code: tea.KeyDown})
	pressConfigKey(t, s2, tea.KeyPressMsg{Code: tea.KeyDown})
	pressConfigKey(t, s2, tea.KeyPressMsg{Text: "i", Code: 'i'})
	pressConfigKey(t, s2, tea.KeyPressMsg{Code: tea.KeyBackspace})
	pressConfigKey(t, s2, tea.KeyPressMsg{Code: tea.KeyEsc})
	if got := s2.values["session.max_iterations"]; got != "100" {
		t.Fatalf("Esc from editor staged %q, want original", got)
	}
}

func TestConfigScreenEditorSecondaryAction(t *testing.T) {
	s := configScreenFixture()
	pressConfigKey(t, s, tea.KeyPressMsg{Text: "e", Code: 'e'})
	if !s.Done() || !s.OpenEditorRequested() || s.Applied() {
		t.Fatalf("editor action: done=%v editor=%v applied=%v", s.Done(), s.OpenEditorRequested(), s.Applied())
	}
}

func TestConfigScreenViewDoesNotContainSecrets(t *testing.T) {
	s := configScreenFixture()
	s.Resize(80, 24)
	view := s.View()
	for _, forbidden := range []string{"api_key", "token", "secret"} {
		if strings.Contains(strings.ToLower(view), forbidden) {
			t.Fatalf("view unexpectedly contains %q:\n%s", forbidden, view)
		}
	}
}

func TestConfigScreenLocksProjectOverrideAndShowsSource(t *testing.T) {
	s := NewConfigScreen([]ConfigSetting{{
		Key:          "ui.markdown",
		Label:        "Markdown",
		Description:  "Render markdown",
		Value:        "false",
		Options:      []string{"true", "false"},
		LockedReason: "controlled by project config",
	}})
	s.Resize(80, 24)
	pressConfigKey(t, s, tea.KeyPressMsg{Code: tea.KeyRight})
	if got := s.values["ui.markdown"]; got != "false" {
		t.Fatalf("locked setting changed to %q", got)
	}
	if view := s.View(); !strings.Contains(view, "controlled by project config") {
		t.Fatalf("locked source missing from view:\n%s", view)
	}
}

func TestConfigScreenMarksRestartRequiredAndShowsRunningValue(t *testing.T) {
	s := NewConfigScreen([]ConfigSetting{{
		Key:             "session.max_iterations",
		Label:           "Max iterations",
		Description:     "Agent turn cap",
		Value:           "250",
		EffectiveValue:  "100",
		RestartRequired: true,
	}})
	s.Resize(80, 24)
	view := s.View()
	for _, want := range []string{"250", "running: 100", "restart required"} {
		if !strings.Contains(view, want) {
			t.Fatalf("config view missing %q:\n%s", want, view)
		}
	}
}
