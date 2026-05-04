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
	// Custom provider profiles. Users define unlimited entries under
	// [provider.custom.<id>] in config.toml, picking a transport
	// (anthropic_messages | openai_chat | gemini_native) per profile.
	// This is what lets the same upstream service (e.g. MiniMax) be
	// configured twice with different wire formats — useful when the
	// vendor exposes both an Anthropic-compatible and OpenAI-compatible
	// endpoint and one of them has a bug the other doesn't.
	if raw, ok := cfg.Provider.Custom[name]; ok {
		return buildCustomProvider(cfg, name, raw, modelOverride)
	}
	known := []string{"anthropic", "openai", "gemini"}
	for k := range cfg.Provider.Custom {
		known = append(known, k)
	}
	return nil, fmt.Errorf("provider %q not configured. Known profiles: %s", name, strings.Join(known, ", "))
}

// buildCustomProvider routes a custom-profile entry through the
// transport its `transport` field names. Defaults match the per-
// transport profile's defaults (so a barebones [provider.custom.foo]
// with just transport+base_url+api_key_env+model works).
func buildCustomProvider(cfg *config.Config, id string, raw config.ProviderRaw, modelOverride string) (*ProviderBuild, error) {
	key, err := cfg.ResolveAPIKey(id)
	if err != nil {
		return nil, err
	}
	model := modelOverride
	if model == "" {
		model = raw.Model
	}
	timeout := time.Duration(raw.TimeoutSecs) * time.Second
	if raw.TimeoutSecs == 0 {
		timeout = 120 * time.Second
	}
	maxTokens := raw.MaxTokens
	if maxTokens == 0 {
		maxTokens = 8192
	}
	switch raw.Transport {
	case "anthropic_messages", "anthropic", "":
		// Empty transport defaults to anthropic_messages — the most
		// common case for "I'm pointing at an Anthropic-format gateway"
		// (MiniMax, OpenRouter, yunwu, …). Errors users hit if their
		// gateway is actually OpenAI-format will surface as 4xx on the
		// first call, which is recoverable; vs picking openai_chat as
		// the default would silently break the historically common
		// anthropic-compat use case.
		prov := llm.NewAnthropic(key, raw.BaseURL, model, maxTokens, timeout, "")
		Preconnect(raw.BaseURL)
		return &ProviderBuild{Provider: prov, Model: model}, nil
	case "openai_chat", "openai":
		prov := llm.NewOpenAI(key, raw.BaseURL, model, maxTokens, timeout, 0)
		Preconnect(raw.BaseURL)
		return &ProviderBuild{Provider: prov, Model: model}, nil
	case "gemini_native", "gemini":
		prov := llm.NewGemini(key, raw.BaseURL, model, maxTokens, timeout, 0)
		Preconnect(raw.BaseURL)
		return &ProviderBuild{Provider: prov, Model: model}, nil
	case "azure_openai", "azure":
		// Azure routes by deployment, not model. base_url = the
		// resource subdomain (or full URL); model = deployment name;
		// api_version = the required Azure version string.
		prov := llm.NewAzure(key, raw.BaseURL, raw.Model, raw.APIVersion, raw.Model, maxTokens, timeout)
		prov.ContextWindow = raw.ContextWindow
		Preconnect(raw.BaseURL)
		return &ProviderBuild{Provider: prov, Model: model}, nil
	case "vertex_anthropic", "vertex":
		// Vertex needs a GCP service-account JSON file path + project
		// + region. The model id goes through normally.
		if raw.ServiceAccountFile == "" {
			return nil, fmt.Errorf("provider %q: vertex transport requires service_account_file", id)
		}
		if raw.Project == "" {
			return nil, fmt.Errorf("provider %q: vertex transport requires project", id)
		}
		region := raw.Region
		if region == "" {
			region = raw.BaseURL // legacy: people may put region in base_url
		}
		prov, err := llm.NewVertex(raw.ServiceAccountFile, raw.Project, region, model, maxTokens, timeout)
		if err != nil {
			return nil, fmt.Errorf("provider %q: %w", id, err)
		}
		prov.ContextWindow = raw.ContextWindow
		return &ProviderBuild{Provider: prov, Model: model}, nil
	case "bedrock_anthropic", "bedrock":
		// Bedrock: api_key_env → AWS_ACCESS_KEY_ID (or env fallback);
		// secret_key_env → AWS_SECRET_ACCESS_KEY; session_token_env
		// for STS-issued creds; region in base_url or AWS_REGION env.
		secret := ""
		if raw.SecretKeyEnv != "" {
			secret = osGetenv(raw.SecretKeyEnv)
		}
		sessionTok := ""
		if raw.SessionTokenEnv != "" {
			sessionTok = osGetenv(raw.SessionTokenEnv)
		}
		region := raw.Region
		if region == "" {
			region = raw.BaseURL
		}
		prov, err := llm.NewBedrock(key, secret, sessionTok, region, model, maxTokens, timeout)
		if err != nil {
			return nil, fmt.Errorf("provider %q: %w", id, err)
		}
		prov.ContextWindow = raw.ContextWindow
		return &ProviderBuild{Provider: prov, Model: model}, nil
	default:
		return nil, fmt.Errorf("provider %q: unknown transport %q (want anthropic_messages | openai_chat | gemini_native | azure_openai | vertex_anthropic | bedrock_anthropic)", id, raw.Transport)
	}
}

// osGetenv is a thin wrapper around os.Getenv kept here so the
// imports section doesn't need to change every time we add a new
// cloud-auth field. (We could just inline os.Getenv calls — kept
// the helper for symmetry.)
func osGetenv(name string) string { return os.Getenv(name) }
