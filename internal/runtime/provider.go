package runtime

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Ricardo-M-L/metis/internal/config"
	"github.com/Ricardo-M-L/metis/internal/llm"
	// Primary-profile providers — direct imports because BuildProvider
	// constructs them with custom anti-distillation / preconnect plumbing.
	"github.com/Ricardo-M-L/metis/internal/llm/anthropic"
	"github.com/Ricardo-M-L/metis/internal/llm/gemini"
	"github.com/Ricardo-M-L/metis/internal/llm/openai"
	// Cloud-auth providers — blank-imported so their init() side-effect
	// registers them with the transport registry. BuildProvider's
	// custom-profile path looks them up via transport.MustBuild;
	// adding a new transport just means dropping a blank import here.
	_ "github.com/Ricardo-M-L/metis/internal/llm/azure"
	_ "github.com/Ricardo-M-L/metis/internal/llm/bedrock"
	"github.com/Ricardo-M-L/metis/internal/llm/transport"
	_ "github.com/Ricardo-M-L/metis/internal/llm/vertex"
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

func isOpenAIOrigin(baseURL string) bool {
	if baseURL == "" {
		return true
	}
	u, err := url.Parse(baseURL)
	if err != nil {
		return false
	}
	host := strings.ToLower(u.Hostname())
	return host == "api.openai.com" || strings.HasSuffix(host, ".openai.com")
}

// ProviderBuild is the result of constructing an LLM provider client.
// `Model` is the resolved model id (after applying flag overrides + cfg
// defaults). Callers downstream — agent loop, compactor, builtin Agent —
// often need both the provider client AND the chosen model, so we return
// them together rather than letting setupRuntime re-derive the model.
type ProviderBuild struct {
	Provider        llm.Provider
	Model           string
	MaxOutputTokens int
}

func finalizeProviderBuild(provider llm.Provider, model string, maxOutputTokens int) (*ProviderBuild, error) {
	if provider == nil {
		return nil, fmt.Errorf("provider %q: constructor returned nil", model)
	}
	if maxOutputTokens <= 0 {
		maxOutputTokens = transport.DefaultMaxOutputTokens
	}
	if window := provider.MaxContextTokens(); window > 0 && maxOutputTokens >= window {
		return nil, fmt.Errorf("provider %q: max_tokens (%d) must be smaller than context_window (%d)",
			model, maxOutputTokens, window)
	}
	return &ProviderBuild{Provider: provider, Model: model, MaxOutputTokens: maxOutputTokens}, nil
}

// visionOverrideProvider preserves the core Provider contract while replacing
// only the optional vision capability decision. Keep optional interfaces off
// this base wrapper: claiming a capability that the wrapped provider does not
// implement changes agent behavior (for example, active-context accounting).
// Custom gateways frequently use private model ids that a public catalog
// cannot identify.
type visionOverrideProvider struct {
	llm.Provider
	supportsVision bool
}

func (p visionOverrideProvider) SupportsVision() bool { return p.supportsVision }
func (p visionOverrideProvider) VisionCapability() llm.VisionCapability {
	if p.supportsVision {
		return llm.VisionSupported
	}
	return llm.VisionUnsupported
}

// visionOverrideHistoryProvider is selected only when the wrapped provider
// really implements ContextHistoryPolicy. This preserves that optional
// capability without making every vision override appear policy-aware.
type visionOverrideHistoryProvider struct {
	visionOverrideProvider
	historyPolicy llm.ContextHistoryPolicy
}

func (p visionOverrideHistoryProvider) ContextIncludesAssistantBlock(block llm.ContentBlock) bool {
	return p.historyPolicy.ContextIncludesAssistantBlock(block)
}

func withVisionOverride(provider llm.Provider, supportsVision bool) llm.Provider {
	base := visionOverrideProvider{Provider: provider, supportsVision: supportsVision}
	if policy, ok := provider.(llm.ContextHistoryPolicy); ok {
		return visionOverrideHistoryProvider{
			visionOverrideProvider: base,
			historyPolicy:          policy,
		}
	}
	return base
}

// ProviderHasCredentials reports whether a configured provider has the
// authentication material its transport actually consumes. Most transports
// use an API key, but Vertex and Bedrock deliberately do not share that auth
// shape: Vertex reads a service-account file, while Bedrock needs an AWS
// access-key/secret-key pair. Keeping this check transport-aware prevents the
// vision recovery picker from hiding usable cloud profiles (or offering an AWS
// profile whose secret half is absent).
//
// This is a local, non-networking preflight. The transport constructor remains
// authoritative and returns the detailed error if a credential file is
// malformed or another required profile field is missing.
func ProviderHasCredentials(cfg *config.Config, name string) bool {
	if cfg == nil || strings.TrimSpace(name) == "" {
		return false
	}

	if name == "google" {
		name = "gemini"
	}
	raw, custom := cfg.Provider.Custom[name]
	if !custom {
		_, err := cfg.ResolveAPIKey(name)
		return err == nil
	}

	switch normalizedCustomTransport(raw) {
	case "vertex_anthropic", "vertex":
		path := strings.TrimSpace(raw.ServiceAccountFile)
		if path == "" {
			return false
		}
		info, err := os.Stat(filepath.Clean(path))
		return err == nil && !info.IsDir()
	case "bedrock_anthropic", "bedrock":
		accessKey, err := resolveCustomProviderAPIKey(cfg, name, raw)
		return err == nil && strings.TrimSpace(accessKey) != "" && bedrockSecretKey(raw) != ""
	default:
		_, err := cfg.ResolveAPIKey(name)
		return err == nil
	}
}

func normalizedCustomTransport(raw config.ProviderRaw) string {
	transportName := strings.ToLower(strings.TrimSpace(raw.Transport))
	if transportName == "" {
		return "anthropic_messages"
	}
	return transportName
}

// resolveCustomProviderAPIKey returns the value carried in BuildOpts.APIKey.
// Vertex has no API-key concept, and Bedrock accepts the standard AWS env var
// even when api_key_env was not repeated in config.toml. All other transports
// retain the existing ResolveAPIKey chain (env -> auth.json -> inline config).
func resolveCustomProviderAPIKey(cfg *config.Config, id string, raw config.ProviderRaw) (string, error) {
	switch normalizedCustomTransport(raw) {
	case "vertex_anthropic", "vertex":
		return "", nil
	case "bedrock_anthropic", "bedrock":
		if key, err := cfg.ResolveAPIKey(id); err == nil && strings.TrimSpace(key) != "" {
			return key, nil
		}
		if key := os.Getenv("AWS_ACCESS_KEY_ID"); strings.TrimSpace(key) != "" {
			return key, nil
		}
		return "", fmt.Errorf("%w for provider %q (set api_key_env or AWS_ACCESS_KEY_ID)", config.ErrMissingAPIKey, id)
	default:
		return cfg.ResolveAPIKey(id)
	}
}

func bedrockSecretKey(raw config.ProviderRaw) string {
	if envName := strings.TrimSpace(raw.SecretKeyEnv); envName != "" {
		if secret := os.Getenv(envName); secret != "" {
			return secret
		}
	}
	return os.Getenv("AWS_SECRET_ACCESS_KEY")
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
		prov := anthropic.New(
			key,
			cfg.Provider.Anthropic.BaseURL,
			model,
			cfg.Provider.Anthropic.MaxTokens,
			time.Duration(cfg.Provider.Anthropic.TimeoutSecs)*time.Second,
			cfg.Provider.Anthropic.AnthropicBeta,
		)
		prov.ContextWindow = cfg.Provider.Anthropic.ContextWindow
		prov.CatalogProvider = strings.ToLower(strings.TrimSpace(cfg.Provider.Anthropic.CatalogProvider))
		if prov.CatalogProvider == "" && isAnthropicOrigin(cfg.Provider.Anthropic.BaseURL) {
			prov.CatalogProvider = "anthropic"
		}
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
		return finalizeProviderBuild(prov, model, cfg.Provider.Anthropic.MaxTokens)
	case "openai":
		key, err := cfg.ResolveAPIKey("openai")
		if err != nil {
			return nil, err
		}
		model := modelOverride
		if model == "" {
			model = cfg.Provider.OpenAI.Model
		}
		if strings.EqualFold(cfg.Provider.OpenAI.WireProtocol, "responses") {
			prov := openai.NewResponses(
				key,
				cfg.Provider.OpenAI.BaseURL,
				model,
				cfg.Provider.OpenAI.MaxTokens,
				time.Duration(cfg.Provider.OpenAI.TimeoutSecs)*time.Second,
				cfg.Provider.OpenAI.Temperature,
			)
			if err := prov.ConfigureStateMode(cfg.Provider.OpenAI.ResponsesStateMode); err != nil {
				return nil, err
			}
			if err := prov.ConfigureCapabilityProfile(cfg.Provider.OpenAI.ResponsesProfile); err != nil {
				return nil, err
			}
			prov.PromptCacheKey = strings.TrimSpace(cfg.Provider.OpenAI.PromptCacheKey)
			prov.HostedTools = append([]string(nil), cfg.Provider.OpenAI.HostedTools...)
			prov.ContextWindow = cfg.Provider.OpenAI.ContextWindow
			Preconnect(cfg.Provider.OpenAI.BaseURL)
			return finalizeProviderBuild(prov, model, cfg.Provider.OpenAI.MaxTokens)
		}
		prov := openai.New(
			key,
			cfg.Provider.OpenAI.BaseURL,
			model,
			cfg.Provider.OpenAI.MaxTokens,
			time.Duration(cfg.Provider.OpenAI.TimeoutSecs)*time.Second,
			cfg.Provider.OpenAI.Temperature,
		)
		prov.ContextWindow = cfg.Provider.OpenAI.ContextWindow
		prov.CatalogProvider = strings.ToLower(strings.TrimSpace(cfg.Provider.OpenAI.CatalogProvider))
		if prov.CatalogProvider == "" && isOpenAIOrigin(cfg.Provider.OpenAI.BaseURL) {
			prov.CatalogProvider = "openai"
		}
		Preconnect(cfg.Provider.OpenAI.BaseURL)
		return finalizeProviderBuild(prov, model, cfg.Provider.OpenAI.MaxTokens)
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
		prov := gemini.New(
			key,
			cfg.Provider.Gemini.BaseURL,
			model,
			cfg.Provider.Gemini.MaxTokens,
			time.Duration(cfg.Provider.Gemini.TimeoutSecs)*time.Second,
			cfg.Provider.Gemini.Temperature,
		)
		prov.ContextWindow = cfg.Provider.Gemini.ContextWindow
		Preconnect(cfg.Provider.Gemini.BaseURL)
		return finalizeProviderBuild(prov, model, cfg.Provider.Gemini.MaxTokens)
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
// transport registry. Each provider subpackage's init() registers a
// constructor under its transport name; this function flattens the
// ProviderRaw fields into transport.BuildOpts and delegates.
//
// Adding a new transport is now "create internal/llm/<name>/, call
// transport.Register in init(), blank-import it from this file" —
// no need to grow the case list here.
func buildCustomProvider(cfg *config.Config, id string, raw config.ProviderRaw, modelOverride string) (*ProviderBuild, error) {
	key, err := resolveCustomProviderAPIKey(cfg, id, raw)
	if err != nil {
		return nil, err
	}
	model := modelOverride
	if model == "" {
		model = raw.Model
	}
	transportName := raw.Transport
	if transportName == "" {
		// Empty transport defaults to anthropic_messages — the most
		// common case for "I'm pointing at an Anthropic-format gateway"
		// (MiniMax, OpenRouter, yunwu, …).
		transportName = "anthropic_messages"
	}

	opts := transport.BuildOpts{
		APIKey:          key,
		BaseURL:         raw.BaseURL,
		Model:           model,
		CatalogProvider: strings.ToLower(strings.TrimSpace(raw.CatalogProvider)),
		MaxTokens:       raw.MaxTokens,
		Timeout:         raw.TimeoutSecs,
		ContextWindow:   raw.ContextWindow,
		Extra: map[string]string{
			// Azure
			"api_version": raw.APIVersion,
			// Bedrock
			"secret_key_env":    raw.SecretKeyEnv,
			"session_token_env": raw.SessionTokenEnv,
			// Vertex
			"service_account_file": raw.ServiceAccountFile,
			"project":              raw.Project,
			"region":               raw.Region,
			"responses_state_mode": strings.ToLower(strings.TrimSpace(raw.ResponsesStateMode)),
			"responses_profile":    strings.ToLower(strings.TrimSpace(raw.ResponsesProfile)),
			"prompt_cache_key":     strings.TrimSpace(raw.PromptCacheKey),
			"hosted_tools":         strings.Join(raw.HostedTools, ","),
		},
	}
	if opts.CatalogProvider == "" {
		opts.CatalogProvider = id
	}

	res, err := transport.MustBuild(transportName, opts)
	if err != nil {
		return nil, fmt.Errorf("provider %q: %w", id, err)
	}
	provider := res.Provider
	if raw.SupportsVision != nil {
		// The native Gemini request encoder does not yet serialize Metis image
		// blocks. Refuse a misleading positive override instead of passing the
		// TUI gate and silently dropping the attachment on the wire.
		if *raw.SupportsVision && (transportName == "gemini_native" || transportName == "gemini") {
			return nil, fmt.Errorf("provider %q: supports_vision=true is not available for transport %q", id, transportName)
		}
		provider = withVisionOverride(provider, *raw.SupportsVision)
	}
	Preconnect(raw.BaseURL)
	return finalizeProviderBuild(provider, res.Model, res.MaxOutputTokens)
}
