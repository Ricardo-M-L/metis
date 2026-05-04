package runtime

// Coverage for the custom-provider dispatch path added when wiring
// [provider.custom.<id>] profiles to BuildProvider. Pure config-side
// shape verification — no network calls, no model invocations.

import (
	"testing"

	"github.com/Ricardo-M-L/metis/internal/config"
)

func newCfgWithCustom(name string, raw config.ProviderRaw) *config.Config {
	cfg := &config.Config{}
	cfg.Provider.Custom = map[string]config.ProviderRaw{name: raw}
	return cfg
}

func TestBuildProvider_Custom_AnthropicTransport(t *testing.T) {
	t.Setenv("FAKE_KEY", "sk-test")
	cfg := newCfgWithCustom("minimax-anthropic", config.ProviderRaw{
		Transport:   "anthropic_messages",
		APIKeyEnv:   "FAKE_KEY",
		BaseURL:     "https://api.example.com/anthropic",
		Model:       "MiniMax-M2.7",
		MaxTokens:   8192,
		TimeoutSecs: 60,
	})

	pb, err := BuildProvider(cfg, "minimax-anthropic", "")
	if err != nil {
		t.Fatalf("BuildProvider: %v", err)
	}
	if pb == nil || pb.Provider == nil {
		t.Fatal("nil provider")
	}
	if pb.Provider.Name() != "anthropic" {
		t.Errorf("transport routing: provider.Name() = %q, want anthropic", pb.Provider.Name())
	}
	if pb.Model != "MiniMax-M2.7" {
		t.Errorf("model: got %q, want MiniMax-M2.7", pb.Model)
	}
}

func TestBuildProvider_Custom_OpenAITransport(t *testing.T) {
	t.Setenv("FAKE_KEY", "sk-test")
	cfg := newCfgWithCustom("deepseek", config.ProviderRaw{
		Transport: "openai_chat",
		APIKeyEnv: "FAKE_KEY",
		BaseURL:   "https://api.deepseek.com/v1",
		Model:     "deepseek-chat",
	})

	pb, err := BuildProvider(cfg, "deepseek", "")
	if err != nil {
		t.Fatalf("BuildProvider: %v", err)
	}
	if pb.Provider.Name() != "openai" {
		t.Errorf("provider.Name() = %q, want openai", pb.Provider.Name())
	}
}

func TestBuildProvider_Custom_GeminiTransport(t *testing.T) {
	t.Setenv("FAKE_KEY", "AIza-test")
	cfg := newCfgWithCustom("vertex-shim", config.ProviderRaw{
		Transport: "gemini_native",
		APIKeyEnv: "FAKE_KEY",
		BaseURL:   "https://generativelanguage.googleapis.com/v1beta",
		Model:     "gemini-2.0-flash",
	})

	pb, err := BuildProvider(cfg, "vertex-shim", "")
	if err != nil {
		t.Fatalf("BuildProvider: %v", err)
	}
	if pb.Provider.Name() != "gemini" {
		t.Errorf("provider.Name() = %q, want gemini", pb.Provider.Name())
	}
}

func TestBuildProvider_Custom_DefaultsToAnthropicWhenTransportEmpty(t *testing.T) {
	// Empty transport is the legacy/permissive default — most users
	// pointing at "an Anthropic-compatible gateway" don't think to set
	// transport, and that's the historically common case.
	t.Setenv("FAKE_KEY", "sk-test")
	cfg := newCfgWithCustom("legacy", config.ProviderRaw{
		APIKeyEnv: "FAKE_KEY",
		BaseURL:   "https://example.com/anthropic",
		Model:     "claude-3.5-sonnet",
	})

	pb, err := BuildProvider(cfg, "legacy", "")
	if err != nil {
		t.Fatalf("BuildProvider with empty transport: %v", err)
	}
	if pb.Provider.Name() != "anthropic" {
		t.Errorf("empty transport should default to anthropic; got %q", pb.Provider.Name())
	}
}

func TestBuildProvider_Custom_UnknownTransport(t *testing.T) {
	t.Setenv("FAKE_KEY", "sk-test")
	cfg := newCfgWithCustom("weird", config.ProviderRaw{
		Transport: "websocket-rpc-v3",
		APIKeyEnv: "FAKE_KEY",
		BaseURL:   "https://example.com",
		Model:     "x",
	})

	_, err := BuildProvider(cfg, "weird", "")
	if err == nil {
		t.Fatal("expected error for unknown transport")
	}
}

func TestBuildProvider_Custom_ModelOverride(t *testing.T) {
	t.Setenv("FAKE_KEY", "sk-test")
	cfg := newCfgWithCustom("ds", config.ProviderRaw{
		Transport: "openai_chat",
		APIKeyEnv: "FAKE_KEY",
		BaseURL:   "https://api.deepseek.com/v1",
		Model:     "deepseek-chat",
	})

	pb, err := BuildProvider(cfg, "ds", "deepseek-reasoner")
	if err != nil {
		t.Fatalf("BuildProvider with override: %v", err)
	}
	if pb.Model != "deepseek-reasoner" {
		t.Errorf("override: got %q, want deepseek-reasoner", pb.Model)
	}
}

func TestBuildProvider_UnknownProfileGivesHelpfulError(t *testing.T) {
	cfg := &config.Config{}
	cfg.Provider.Custom = map[string]config.ProviderRaw{
		"deepseek": {Transport: "openai_chat"},
		"minimax":  {Transport: "anthropic_messages"},
	}
	_, err := BuildProvider(cfg, "no-such-profile", "")
	if err == nil {
		t.Fatal("expected error for unknown profile")
	}
	msg := err.Error()
	for _, want := range []string{"no-such-profile", "deepseek", "minimax", "anthropic", "openai", "gemini"} {
		if !contains(msg, want) {
			t.Errorf("error message %q should mention %q so the user can tab-complete", msg, want)
		}
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || (len(s) > 0 && stringIndex(s, sub) >= 0))
}

func stringIndex(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
