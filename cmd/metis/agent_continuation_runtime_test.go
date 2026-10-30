package main

import (
	"testing"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/config"
	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

func TestAgentContinuationRuntimeResolvesSavedProviderAndLiveDials(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	cfg := &config.Config{Provider: config.ProviderSet{Default: "openai", OpenAI: config.ProviderOpenAI{APIKey: "fixture-key", BaseURL: "https://example.invalid/v1", Model: "parent-model"}}}
	loop := agent.NewLoop(nil, tools.NewRegistry(), permission.New(permission.ModePlan), nil, "system", 5)
	state := agentContinuationRuntime(cfg, loop)
	provider, err := state.ResolveProvider("openai", "saved-child-model")
	if err != nil {
		t.Fatal(err)
	}
	if provider.ModelID() != "saved-child-model" {
		t.Fatalf("saved child model replaced by parent model: %q", provider.ModelID())
	}
	loop.SetEffort(llm.EffortHigh)
	loop.SetFast(true)
	if state.Effort() != llm.EffortHigh || !state.Fast() {
		t.Fatal("child dials did not follow live parent settings")
	}
	loop.SetEffort(llm.EffortLow)
	loop.SetFast(false)
	if state.Effort() != llm.EffortLow || state.Fast() {
		t.Fatal("child callbacks retained stale effort/fast values")
	}
	if _, err := state.ResolveProvider("missing-provider", "saved-child-model"); err == nil {
		t.Fatal("missing saved provider silently used parent provider")
	}
}
