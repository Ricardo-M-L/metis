package runtime

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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

func TestBuildProviderWithoutPreconnectStaysLocal(t *testing.T) {
	requests := make(chan struct{}, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests <- struct{}{}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	cfg := configWithKey(t, "openai", "sk-local-validation", "gpt-test")
	cfg.Provider.OpenAI.BaseURL = server.URL
	if _, err := BuildProviderWithoutPreconnect(cfg, "openai", ""); err != nil {
		t.Fatal(err)
	}
	select {
	case <-requests:
		t.Fatal("local provider construction unexpectedly contacted the endpoint")
	case <-time.After(150 * time.Millisecond):
	}

	if _, err := BuildProvider(cfg, "openai", ""); err != nil {
		t.Fatal(err)
	}
	select {
	case <-requests:
	case <-time.After(2 * time.Second):
		t.Fatal("regular provider construction no longer performs its warm-up")
	}
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

func TestBedrockCredentialStoreFailureDoesNotFallBackToAWSEnv(t *testing.T) {
	home := t.TempDir()
	t.Setenv("METIS_HOME", home)
	t.Setenv("AWS_ACCESS_KEY_ID", "must-not-be-used")
	credentialDir := filepath.Join(home, ".credentials")
	if err := os.MkdirAll(credentialDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(credentialDir, "auth.json"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{}
	raw := config.ProviderRaw{Transport: "bedrock_anthropic"}
	cfg.Provider.Custom = map[string]config.ProviderRaw{"bedrock-claude": raw}
	if _, err := resolveCustomProviderAPIKey(cfg, "bedrock-claude", raw); err == nil || !strings.Contains(err.Error(), "parse credential store") {
		t.Fatalf("resolveCustomProviderAPIKey error = %v, want credential-store error", err)
	}
}
