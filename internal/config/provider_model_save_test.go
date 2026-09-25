package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSaveUserProviderModelPreservesOtherConfigAndDefault(t *testing.T) {
	home := t.TempDir()
	t.Setenv("METIS_HOME", home)
	t.Chdir(t.TempDir())
	path := filepath.Join(home, "config.toml")
	original := "# keep this comment\n[provider]\ndefault = \"openai-codex\"\n\n[provider.openai]\nmodel = \"old-model\" # current model\napi_key_env = \"MY_PRIVATE_KEY\"\n"
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := SaveUserProviderModel("openai", "new-model"); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{"# keep this comment", `default = "openai-codex"`, `model = "new-model" # current model`, `api_key_env = "MY_PRIVATE_KEY"`} {
		if !strings.Contains(string(data), text) {
			t.Fatalf("model save lost %q: %s", text, data)
		}
	}
	if err := SaveUserProviderModel("openai-codex", "gpt-6-astra"); err != nil {
		t.Fatal(err)
	}
	cfg, _, err := Load()
	if err != nil || cfg.Provider.Default != "openai-codex" || cfg.Provider.OpenAICodex.Model != "gpt-6-astra" {
		t.Fatalf("built-in model save = %+v, err=%v", cfg, err)
	}
}

func TestSaveUserProviderModelRejectsUnsafeValuesAndUnknownCustom(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	for _, model := range []string{"", "two models", "bad\nmodel"} {
		if err := SaveUserProviderModel("openai", model); err == nil {
			t.Fatalf("accepted invalid model %q", model)
		}
	}
	if err := SaveUserProviderModel("missing-custom", "model-id"); err == nil {
		t.Fatal("created a custom provider from a model selection")
	}
}
