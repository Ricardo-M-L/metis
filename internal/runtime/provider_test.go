package runtime

import (
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/config"
)

// configWithKey builds a minimal config that resolves an api key for the
// requested provider. Tests don't actually call the provider — they just
// verify model resolution + provider client construction.
func configWithKey(t *testing.T, name, key, model string) *config.Config {
	t.Helper()
	cfg := &config.Config{}
	switch name {
	case "anthropic":
		cfg.Provider.Anthropic.APIKey = key
		cfg.Provider.Anthropic.Model = model
		cfg.Provider.Anthropic.MaxTokens = 8192
		cfg.Provider.Anthropic.TimeoutSecs = 30
		cfg.Provider.Anthropic.BaseURL = "https://api.anthropic.com"
	case "openai":
		cfg.Provider.OpenAI.APIKey = key
		cfg.Provider.OpenAI.Model = model
		cfg.Provider.OpenAI.MaxTokens = 8192
		cfg.Provider.OpenAI.TimeoutSecs = 30
		cfg.Provider.OpenAI.BaseURL = "https://api.openai.com/v1"
	}
	return cfg
}

func TestBuildProvider_AnthropicResolvesModelFromConfig(t *testing.T) {
	cfg := configWithKey(t, "anthropic", "sk-x", "claude-opus-4-7")
	got, err := BuildProvider(cfg, "anthropic", "")
	if err != nil {
		t.Fatalf("BuildProvider: %v", err)
	}
	if got.Model != "claude-opus-4-7" {
		t.Errorf("Model = %q, want claude-opus-4-7", got.Model)
	}
	if got.Provider == nil {
		t.Error("Provider should be non-nil")
	}
}

func TestBuildProvider_ModelOverrideWins(t *testing.T) {
	cfg := configWithKey(t, "anthropic", "sk-x", "claude-default")
	got, err := BuildProvider(cfg, "anthropic", "claude-fast-model")
	if err != nil {
		t.Fatalf("BuildProvider: %v", err)
	}
	if got.Model != "claude-fast-model" {
		t.Errorf("override should win; got %q", got.Model)
	}
}

func TestBuildProvider_OpenAI(t *testing.T) {
	cfg := configWithKey(t, "openai", "sk-y", "gpt-4o")
	got, err := BuildProvider(cfg, "openai", "")
	if err != nil {
		t.Fatalf("BuildProvider: %v", err)
	}
	if got.Model != "gpt-4o" {
		t.Errorf("Model = %q, want gpt-4o", got.Model)
	}
}

func TestBuildProvider_RejectsUnknown(t *testing.T) {
	cfg := configWithKey(t, "anthropic", "sk-x", "claude-x")
	_, err := BuildProvider(cfg, "fictional-llm", "")
	if err == nil {
		t.Fatal("unknown provider should error")
	}
	if !strings.Contains(err.Error(), "fictional-llm") {
		t.Errorf("error should mention provider name; got %v", err)
	}
}

func TestBuildProvider_PropagatesMissingKeyError(t *testing.T) {
	// No API key configured anywhere → ResolveAPIKey errors → BuildProvider
	// surfaces it rather than building a provider that will fail later.
	// Scope METIS_HOME so the dev-machine auth.json (which may have
	// a real minimax key that compat-fallbacks to "anthropic") doesn't
	// leak into this test.
	t.Setenv("METIS_HOME", t.TempDir())
	cfg := &config.Config{}
	cfg.Provider.Anthropic.Model = "claude-x"
	_, err := BuildProvider(cfg, "anthropic", "")
	if err == nil {
		t.Error("missing key should error from BuildProvider")
	}
}
