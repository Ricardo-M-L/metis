package config

import (
	"strconv"
	"testing"
)

func TestPromptSuggestionsDefaultAndUserSettingRoundTrip(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	t.Chdir(t.TempDir())
	cfg, _, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.UI.PromptSuggestions {
		t.Fatal("prompt suggestions should be enabled by default")
	}
	for _, enabled := range []bool{false, true} {
		cfg, err = SaveUserSettingsAndLoad([]UserSetting{{Key: "ui.prompt_suggestions", Value: strconv.FormatBool(enabled)}})
		if err != nil {
			t.Fatal(err)
		}
		if cfg.UI.PromptSuggestions != enabled {
			t.Fatalf("saved suggestions = %v, want %v", cfg.UI.PromptSuggestions, enabled)
		}
		if got, ok := effectiveUserSettingValue(cfg, "ui.prompt_suggestions"); !ok || got != strconv.FormatBool(enabled) {
			t.Fatalf("effective setting = %q, %v", got, ok)
		}
	}
	if err := SaveUserSettings([]UserSetting{{Key: "ui.prompt_suggestions", Value: "maybe"}}); err == nil {
		t.Fatal("invalid suggestion toggle was accepted")
	}
}
