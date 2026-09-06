package openai

import (
	"reflect"
	"testing"

	"github.com/Ricardo-M-L/metis/pkg/provider"
)

func TestCodexModelsMatchSubscriptionCatalog(t *testing.T) {
	want := []string{
		"gpt-6-astra",
		"gpt-5.3-codex-spark",
		"gpt-5.4",
		"gpt-5.4-mini",
		"gpt-5.5",
		"gpt-5.6-luna",
		"gpt-5.6-sol",
		"gpt-5.6-terra",
	}
	models := CodexModels()
	got := make([]string, 0, len(models))
	for _, model := range models {
		got = append(got, model.ID)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Codex model ids = %v, want %v", got, want)
	}

	models[0].ID = "mutated"
	if fresh := CodexModels()[0].ID; fresh != "gpt-6-astra" {
		t.Fatalf("CodexModels exposed mutable catalog: %q", fresh)
	}
}

func TestCodexModelsCarryModelSpecificCapabilities(t *testing.T) {
	for _, model := range CodexModels() {
		t.Run(model.ID, func(t *testing.T) {
			p := NewCodexResponses(model.ID, 0, 0, 0, nil)
			if got := p.MaxContextTokens(); got != model.ContextWindow {
				t.Fatalf("MaxContextTokens() = %d, want %d", got, model.ContextWindow)
			}
			wantVision := provider.VisionSupported
			if !model.SupportsImage {
				wantVision = provider.VisionUnsupported
			}
			if got := p.VisionCapability(); got != wantVision {
				t.Fatalf("VisionCapability() = %v, want %v", got, wantVision)
			}
		})
	}
}

func TestCodexContextWindowExplicitOverrideWins(t *testing.T) {
	p := NewCodexResponses("gpt-5.3-codex-spark", 0, 0, 0, nil)
	p.ContextWindow = 64_000
	if got := p.MaxContextTokens(); got != 64_000 {
		t.Fatalf("explicit context override = %d, want 64000", got)
	}
}
