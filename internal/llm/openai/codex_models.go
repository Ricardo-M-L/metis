package openai

import (
	"strings"

	"github.com/Ricardo-M-L/metis/pkg/provider"
)

// CodexModel describes one model exposed by the ChatGPT subscription-backed
// OpenAI Codex provider. The backend does not provide a stable public model
// enumeration endpoint, so this catalog intentionally mirrors the explicit
// allowlist used by Pi rather than mixing in OpenAI Platform API-key models.
type CodexModel struct {
	ID            string
	Name          string
	ContextWindow int
	SupportsImage bool
}

var codexModels = [...]CodexModel{
	{ID: "gpt-6-astra", Name: "GPT-6 Astra", ContextWindow: 272_000, SupportsImage: true},
	{ID: "gpt-5.3-codex-spark", Name: "GPT-5.3 Codex Spark", ContextWindow: 128_000},
	{ID: "gpt-5.4", Name: "GPT-5.4", ContextWindow: 272_000, SupportsImage: true},
	{ID: "gpt-5.4-mini", Name: "GPT-5.4 mini", ContextWindow: 272_000, SupportsImage: true},
	{ID: "gpt-5.5", Name: "GPT-5.5", ContextWindow: 272_000, SupportsImage: true},
	{ID: "gpt-5.6-luna", Name: "GPT-5.6 Luna", ContextWindow: 272_000, SupportsImage: true},
	{ID: "gpt-5.6-sol", Name: "GPT-5.6 Sol", ContextWindow: 272_000, SupportsImage: true},
	{ID: "gpt-5.6-terra", Name: "GPT-5.6 Terra", ContextWindow: 272_000, SupportsImage: true},
}

// CodexModels returns a copy so callers cannot mutate the runtime catalog.
func CodexModels() []CodexModel {
	return append([]CodexModel(nil), codexModels[:]...)
}

func codexModel(model string) (CodexModel, bool) {
	model = strings.TrimSpace(model)
	for _, candidate := range codexModels {
		if strings.EqualFold(candidate.ID, model) {
			return candidate, true
		}
	}
	return CodexModel{}, false
}

// CodexModelContextWindow returns the model-specific subscription limit.
func CodexModelContextWindow(model string) (int, bool) {
	candidate, ok := codexModel(model)
	return candidate.ContextWindow, ok
}

func codexModelVisionCapability(model string) (provider.VisionCapability, bool) {
	candidate, ok := codexModel(model)
	if !ok {
		return provider.VisionUnknown, false
	}
	if candidate.SupportsImage {
		return provider.VisionSupported, true
	}
	return provider.VisionUnsupported, true
}
