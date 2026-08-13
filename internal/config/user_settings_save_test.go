package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"
)

func TestSaveUserSettingsPreservesUnrelatedAndUnknownFields(t *testing.T) {
	home := t.TempDir()
	t.Setenv("METIS_HOME", home)
	path := filepath.Join(home, "config.toml")
	original := "# keep me\n[provider]\ndefault = \"custom-provider\"\n\n[ui]\nmarkdown = true # preserved comment\nfuture_flag = \"untouched\"\n\n[provider.openai]\napi_key = \"secret-must-stay-unread-and-untouched\"\n"
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := SaveUserSettings([]UserSetting{
		{Key: "ui.markdown", Value: "false"},
		{Key: "ui.thinking_display", Value: "hide"},
		{Key: "session.max_iterations", Value: "250"},
	}); err != nil {
		t.Fatalf("SaveUserSettings: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(got)
	for _, want := range []string{
		"# keep me", `default = "custom-provider"`, `future_flag = "untouched"`,
		`api_key = "secret-must-stay-unread-and-untouched"`, "markdown = false # preserved comment",
		`thinking_display = "hide"`, "[session]", "max_iterations = 250",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("updated config missing %q:\n%s", want, text)
		}
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("config mode = %v err=%v, want 0600", info.Mode().Perm(), err)
	}
}

func TestSaveUserSettingsNeverSerializesProjectOverlay(t *testing.T) {
	home := t.TempDir()
	project := t.TempDir()
	t.Setenv("METIS_HOME", home)
	oldWD, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldWD) })
	if err := os.MkdirAll(filepath.Join(project, ".metis"), 0o700); err != nil {
		t.Fatal(err)
	}
	projectConfig := "[provider.openai]\napi_key = \"project-secret\"\nmodel = \"project-only-model\"\n"
	if err := os.WriteFile(filepath.Join(project, ".metis", "config.toml"), []byte(projectConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(project); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Load(); err != nil {
		t.Fatalf("load merged config: %v", err)
	}
	if err := SaveUserSettings([]UserSetting{{Key: "ui.show_tokens", Value: "false"}}); err != nil {
		t.Fatal(err)
	}
	userBytes, err := os.ReadFile(filepath.Join(home, "config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(userBytes), "project-secret") || strings.Contains(string(userBytes), "project-only-model") {
		t.Fatalf("project overlay leaked into user config:\n%s", userBytes)
	}
}

func TestSaveUserSettingsRejectsUnsafeOrInvalidChangesWithoutWriting(t *testing.T) {
	home := t.TempDir()
	t.Setenv("METIS_HOME", home)
	path := filepath.Join(home, "config.toml")
	original := []byte("[ui]\nmarkdown = true\n")
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, settings := range [][]UserSetting{
		{{Key: "provider.openai.api_key", Value: "leak"}},
		{{Key: "ui.markdown", Value: "maybe"}},
		{{Key: "session.auto_compact_threshold", Value: "2.0"}},
		{{Key: "session.auto_compact_threshold", Value: "NaN"}},
		{{Key: "session.auto_compact_threshold", Value: "+Inf"}},
		{{Key: "session.auto_compact_threshold", Value: "-Inf"}},
		{{Key: "ui.markdown", Value: "false"}, {Key: "ui.markdown", Value: "true"}},
	} {
		if err := SaveUserSettings(settings); err == nil {
			t.Fatalf("SaveUserSettings(%+v) unexpectedly succeeded", settings)
		}
		got, err := os.ReadFile(path)
		if err != nil || string(got) != string(original) {
			t.Fatalf("failed update changed file: got=%q err=%v", got, err)
		}
	}
}

func TestFloatRangeRejectsEveryNonFiniteValue(t *testing.T) {
	validate := floatRange(0.1, 1)
	for _, value := range []string{"NaN", "+Inf", "-Inf", "Inf"} {
		if err := validate(value); err == nil {
			t.Errorf("floatRange accepted %q", value)
		}
	}
}

func TestSaveUserSettingsRejectsProjectOverriddenTargetWithoutWriting(t *testing.T) {
	home := t.TempDir()
	project := t.TempDir()
	t.Setenv("METIS_HOME", home)
	oldWD, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldWD) })
	if err := os.MkdirAll(filepath.Join(project, ".metis"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(project, ".metis", "config.toml"), []byte("[ui]\nmarkdown = false\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(project); err != nil {
		t.Fatal(err)
	}
	userPath := filepath.Join(home, "config.toml")
	original := []byte("[ui]\nmarkdown = true\n")
	if err := os.WriteFile(userPath, original, 0o600); err != nil {
		t.Fatal(err)
	}

	err = SaveUserSettings([]UserSetting{{Key: "ui.markdown", Value: "true"}})
	if err == nil || !strings.Contains(err.Error(), "project") {
		t.Fatalf("overridden save error = %v, want project override error", err)
	}
	got, readErr := os.ReadFile(userPath)
	if readErr != nil || string(got) != string(original) {
		t.Fatalf("overridden save changed user config: got=%q err=%v", got, readErr)
	}
}

func TestSaveUserSettingsRejectsMalformedHigherPrecedenceConfigBeforeReplace(t *testing.T) {
	home := t.TempDir()
	project := t.TempDir()
	t.Setenv("METIS_HOME", home)
	oldWD, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldWD) })
	if err := os.MkdirAll(filepath.Join(project, ".metis"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(project, ".metis", "config.toml"), []byte("[ui\nmarkdown = false\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(project); err != nil {
		t.Fatal(err)
	}
	userPath := filepath.Join(home, "config.toml")
	original := []byte("[ui]\nshow_tokens = true\n")
	if err := os.WriteFile(userPath, original, 0o600); err != nil {
		t.Fatal(err)
	}

	if err := SaveUserSettings([]UserSetting{{Key: "ui.show_tokens", Value: "false"}}); err == nil {
		t.Fatal("save unexpectedly succeeded with malformed project config")
	}
	got, readErr := os.ReadFile(userPath)
	if readErr != nil || string(got) != string(original) {
		t.Fatalf("failed candidate validation changed disk: got=%q err=%v", got, readErr)
	}
}

func TestSaveUserSettingsCandidateMatchesLoadValidationBeforeReplace(t *testing.T) {
	home := t.TempDir()
	t.Setenv("METIS_HOME", home)
	path := filepath.Join(home, "config.toml")
	original := []byte("[bash.sandbox]\nmode = \"permissions\"\n\n[ui]\nshow_tokens = true\n")
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := SaveUserSettings([]UserSetting{{Key: "ui.show_tokens", Value: "false"}}); err == nil || !strings.Contains(err.Error(), "[bash.sandbox]") {
		t.Fatalf("legacy invalid table error = %v", err)
	}
	got, readErr := os.ReadFile(path)
	if readErr != nil || string(got) != string(original) {
		t.Fatalf("Load-incompatible candidate changed disk: got=%q err=%v", got, readErr)
	}
}

func TestSaveUserSettingsDoesNotMatchTablesInsideMultilineStrings(t *testing.T) {
	for _, quote := range []string{`"""`, `'''`} {
		t.Run(quote, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("METIS_HOME", home)
			path := filepath.Join(home, "config.toml")
			original := "note = " + quote + "\n[ui]\nmarkdown = true\n" + quote + "\n\n[ui]\nshow_tokens = true\n"
			if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
				t.Fatal(err)
			}

			if err := SaveUserSettings([]UserSetting{{Key: "ui.markdown", Value: "false"}}); err != nil {
				t.Fatalf("SaveUserSettings: %v", err)
			}
			got, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			text := string(got)
			if !strings.Contains(text, "note = "+quote+"\n[ui]\nmarkdown = true\n"+quote) {
				t.Fatalf("multiline string body was modified:\n%s", text)
			}
			var cfg Config
			if _, err := toml.Decode(text, &cfg); err != nil {
				t.Fatal(err)
			}
			if cfg.UI.Markdown {
				t.Fatalf("requested typed value was not written to the real [ui] table:\n%s", text)
			}
		})
	}
}

func TestSaveUserSettingsProducesTypedTOML(t *testing.T) {
	home := t.TempDir()
	t.Setenv("METIS_HOME", home)
	settings := []UserSetting{
		{Key: "permission.mode", Value: "plan"},
		{Key: "ui.theme", Value: "dark"},
		{Key: "ui.performance.mouse_wheel_lines", Value: "3"},
		{Key: "session.auto_compact_threshold", Value: "0.8"},
	}
	if err := SaveUserSettings(settings); err != nil {
		t.Fatal(err)
	}
	var cfg Config
	if _, err := toml.DecodeFile(filepath.Join(home, "config.toml"), &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.Permission.Mode != "plan" || cfg.UI.Theme != "dark" || cfg.UI.Performance.MouseWheelLines != 3 || cfg.Session.AutoCompactThreshold != 0.8 {
		t.Fatalf("typed values not persisted: %+v", cfg)
	}
}
