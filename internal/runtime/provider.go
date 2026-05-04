package runtime

import (
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/Ricardo-M-L/metis/internal/config"
	"github.com/Ricardo-M-L/metis/internal/llm"
)

// isAnthropicOrigin reports whether baseURL points at the real
// Anthropic API (or empty, which falls back to the default Anthropic
// endpoint). Used to gate the anti_distillation startup warning so
// users on real Anthropic don't see a false-positive nag.
func isAnthropicOrigin(baseURL string) bool {
	if baseURL == "" {
		return true // empty → NewAnthropic defaults to api.anthropic.com
	}
	u, err := url.Parse(baseURL)
	if err != nil {
		return false
	}
	host := strings.ToLower(u.Hostname())
	return host == "api.anthropic.com" || strings.HasSuffix(host, ".anthropic.com")
}

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
//     gateways like MiniMax via Anthropic.BaseURL)
//   - "openai"     → /v1/chat/completions (Together, Groq, Ollama, ...)
//   - "gemini"     → /v1beta/models/<model>:streamGenerateContent
//     (Google Generative Language API, native protocol)
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
		prov.AntiDistillation = cfg.Provider.Anthropic.AntiDistillation
		prov.ClientSideDecoys = cfg.Provider.Anthropic.ClientSideDecoys
		// If the user enabled anti_distillation but is talking to a
		// non-Anthropic gateway, the opt-in is a no-op (the gateway's
		// backend doesn't implement the server-side countermeasure).
		// Warn loudly so they don't think it's protecting them — and
		// suggest the client_side_decoys alternative which DOES work
		// against third-party traffic recorders without polluting
		// the model's prompt.
		if prov.AntiDistillation && !isAnthropicOrigin(cfg.Provider.Anthropic.BaseURL) {
			fmt.Fprintf(os.Stderr,
				"warning: [provider.anthropic] anti_distillation = true but base_url=%q is not the real Anthropic API. The flag is sent on the wire but the gateway's backend (MiniMax / OpenRouter / etc.) ignores it. No actual anti-distillation occurs. For third-party gateways, set client_side_decoys = true instead — that adds a non-standard top-level field with fake tool defs that pollute traffic recordings WITHOUT affecting model output.\n",
				cfg.Provider.Anthropic.BaseURL)
		}
		// Warm up TCP+TLS to the API origin in parallel with the rest
		// of init (slash registry build, hooks load, etc.). Saves
		// 100-300ms on the first message — the user perceives this as
		// "instant first reply" instead of "spinner sits there before
		// the first token arrives".
		Preconnect(cfg.Provider.Anthropic.BaseURL)
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
		Preconnect(cfg.Provider.OpenAI.BaseURL)
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
		Preconnect(cfg.Provider.Gemini.BaseURL)
		return &ProviderBuild{Provider: prov, Model: model}, nil
	}
	return nil, fmt.Errorf("provider %q not supported in this build (try 'anthropic', 'openai', or 'gemini')", name)
}
