package runtime

import (
	"strings"

	"github.com/Ricardo-M-L/metis/internal/config"
	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/llm/openai"
)

// This is a working reservation, not the model's maximum output capacity. Long
// reasoning responses need room for both reasoning and the visible answer, but
// reserving the entire advertised model capacity would unnecessarily squeeze
// the input window. Other models and explicit user budgets keep their defaults.
const deepSeekReasoningOutputReservation = 64 * 1024

func applyCustomAutomaticOutputBudget(provider llm.Provider, raw config.ProviderRaw, transportName string, settings *config.ProviderModelSettings, current int) (int, error) {
	if raw.MaxTokens != 0 || settings != nil && (settings.OutputBudget > 0 || settings.SupportsReasoning != nil && !*settings.SupportsReasoning) {
		return current, nil
	}
	if transportName != "openai_chat" && transportName != "openai" && transportName != "openai_responses" {
		return current, nil
	}
	model := provider.ModelID()
	if !strings.HasPrefix(strings.ToLower(model), "deepseek-") {
		return current, nil
	}
	var resolver *llm.ContextResolver
	switch p := provider.(type) {
	case *openai.OpenAI:
		resolver = p.ContextResolver
	case *openai.Responses:
		if p.Name() == "openai-codex" {
			return current, nil
		}
		resolver = p.ContextResolver
	}
	maximum, found := resolver.CatalogReasoningOutputLimit("deepseek")
	if !found {
		return current, nil
	}
	budget := min(deepSeekReasoningOutputReservation, maximum)
	if metadata, ok := provider.(llm.ContextCapabilitiesProvider); ok {
		caps := metadata.ContextCapabilities()
		if settings != nil {
			caps = applyProviderModelCapabilitySettings(caps, *settings)
		}
		if caps.MaxOutputTokens > 0 {
			budget = min(budget, caps.MaxOutputTokens)
		}
		if caps.ModelMaxOutputTokens > 0 {
			budget = min(budget, caps.ModelMaxOutputTokens)
		}
		// A smaller user working window must retain input room. This only
		// clamps an already known output recommendation; the window itself
		// never establishes an output capability for an unknown model.
		if caps.Window > 0 && caps.WindowKind != llm.ContextWindowInput && budget >= caps.Window {
			budget = min(budget, caps.Window/2)
		}
	}
	if budget <= 0 {
		return current, nil
	}
	if err := setProviderModelOutputBudget(provider, budget); err != nil {
		return 0, err
	}
	return budget, nil
}
