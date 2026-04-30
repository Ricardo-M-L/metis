package runtime

import (
	"fmt"
	"time"

	"github.com/Ricardo-M-L/metis/internal/config"
	"github.com/Ricardo-M-L/metis/internal/llm"
)

// ProviderBuild is the result of constructing an LLM provider client.
// `Model` is the resolved model id (after applying flag overrides + cfg
// defaults). Callers downstream — agent loop, compactor, builtin Agent —
// often need both the provider client AND the chosen model, so we return
// them together rather than letting setupRuntime re-derive the model.
type ProviderBuild struct {
	Provider llm.Provider
	Model    string
}

// BuildProvider constructs the LLM client for `name` using cfg + an
// optional model override. Returns an error for unknown providers so
// main.go doesn't have to keep its own switch in sync.
//
// Built-in transports:
//   - "anthropic"  → /v1/messages (also covers anthropic-compatible
//                     gateways like MiniMax via Anthropic.BaseURL)
//   - "openai"     → /v1/chat/completions (Together, Groq, Ollama, ...)
//   - "gemini"     → /v1beta/models/<model>:streamGenerateContent
//                     (Google Generative Language API, native protocol)
func BuildProvider(cfg *config.Config, name, modelOverride string) (*ProviderBuild, error) {
	switch name {
	case "anthropic":
		key, err := cfg.ResolveAPIKey("anthropic")
		if err != nil {
			return nil, err
		}
		model := modelOverride
		if model == "" {
			model = cfg.Provider.Anthropic.Model
		}
		prov := llm.NewAnthropic(
			key,
			cfg.Provider.Anthropic.BaseURL,
			model,
			cfg.Provider.Anthropic.MaxTokens,
			time.Duration(cfg.Provider.Anthropic.TimeoutSecs)*time.Second,
			cfg.Provider.Anthropic.AnthropicBeta,
		)
		prov.ContextWindow = cfg.Provider.Anthropic.ContextWindow
		return &ProviderBuild{Provider: prov, Model: model}, nil
	case "openai":
		key, err := cfg.ResolveAPIKey("openai")
		if err != nil {
			return nil, err
		}
		model := modelOverride
		if model == "" {
			model = cfg.Provider.OpenAI.Model
		}
		prov := llm.NewOpenAI(
			key,
			cfg.Provider.OpenAI.BaseURL,
			model,
			cfg.Provider.OpenAI.MaxTokens,
			time.Duration(cfg.Provider.OpenAI.TimeoutSecs)*time.Second,
			cfg.Provider.OpenAI.Temperature,
		)
		prov.ContextWindow = cfg.Provider.OpenAI.ContextWindow
		return &ProviderBuild{Provider: prov, Model: model}, nil
	case "gemini", "google":
		// Accept "google" as an alias since users sometimes type the
		// brand instead of the model family. Same provider either way.
		key, err := cfg.ResolveAPIKey("gemini")
		if err != nil {
			return nil, err
		}
		model := modelOverride
		if model == "" {
			model = cfg.Provider.Gemini.Model
		}
		prov := llm.NewGemini(
			key,
			cfg.Provider.Gemini.BaseURL,
			model,
			cfg.Provider.Gemini.MaxTokens,
			time.Duration(cfg.Provider.Gemini.TimeoutSecs)*time.Second,
			cfg.Provider.Gemini.Temperature,
		)
		prov.ContextWindow = cfg.Provider.Gemini.ContextWindow
		return &ProviderBuild{Provider: prov, Model: model}, nil
	}
	return nil, fmt.Errorf("provider %q not supported in this build (try 'anthropic', 'openai', or 'gemini')", name)
}
