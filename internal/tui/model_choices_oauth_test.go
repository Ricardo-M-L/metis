package tui

import (
	"testing"

	"github.com/Ricardo-M-L/metis/internal/config"
	pubprovider "github.com/Ricardo-M-L/metis/pkg/provider"
)

func TestOpenAICodexIsAvailableToCLIModelAndProviderPickers(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	cfg, _, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}

	provider, model, ok := configuredProviderModel(cfg, "OPENAI-CODEX")
	if !ok || provider != "openai-codex" || model != cfg.Provider.OpenAICodex.Model {
		t.Fatalf("configuredProviderModel = (%q, %q, %v)", provider, model, ok)
	}

	found := false
	for _, choice := range configuredModelChoices(cfg) {
		if choice.Provider != "openai-codex" {
			continue
		}
		found = true
		if choice.ID != cfg.Provider.OpenAICodex.Model {
			t.Fatalf("Codex model = %q, want %q", choice.ID, cfg.Provider.OpenAICodex.Model)
		}
		if got := modelChoiceVisionCapability(cfg, choice); got != pubprovider.VisionSupported {
			t.Fatalf("Codex vision capability = %v, want supported", got)
		}
	}
	if !found {
		t.Fatal("configured model choices omitted openai-codex")
	}
}

func TestSplitConfiguredProviderModelAcceptsOpenAICodex(t *testing.T) {
	cfg := &config.Config{}
	provider, model := splitConfiguredProviderModel(cfg, "openai-codex/gpt-5.5")
	if provider != "openai-codex" || model != "gpt-5.5" {
		t.Fatalf("split provider/model = %q/%q", provider, model)
	}
}
