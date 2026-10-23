package runtime

import (
	"context"
	"fmt"
	"maps"
	"strings"

	"github.com/Ricardo-M-L/metis/internal/config"
	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/llm/anthropic"
	"github.com/Ricardo-M-L/metis/internal/llm/azure"
	"github.com/Ricardo-M-L/metis/internal/llm/bedrock"
	"github.com/Ricardo-M-L/metis/internal/llm/gemini"
	"github.com/Ricardo-M-L/metis/internal/llm/openai"
	"github.com/Ricardo-M-L/metis/internal/llm/vertex"
	pubprovider "github.com/Ricardo-M-L/metis/pkg/provider"
)

// Lookup uses the selected identifier verbatim. In particular, a constructor's
// normalization of a model suffix must never select another model's settings.
func lookupProviderModelRuntimeSettings(cfg *config.Config, id, selected string) (*config.ProviderModelSettings, error) {
	if cfg == nil {
		return nil, fmt.Errorf("provider %q: missing configuration", id)
	}
	var transportName, endpoint, configured string
	switch id {
	case "anthropic":
		transportName, endpoint, configured = "anthropic_messages", cfg.Provider.Anthropic.BaseURL, cfg.Provider.Anthropic.Model
	case "openai":
		transportName, endpoint, configured = "openai_chat", cfg.Provider.OpenAI.BaseURL, cfg.Provider.OpenAI.Model
		if strings.EqualFold(strings.TrimSpace(cfg.Provider.OpenAI.WireProtocol), "responses") {
			transportName = "openai_responses"
		}
	case "openai-codex":
		transportName, endpoint, configured = "openai_codex_responses", "https://chatgpt.com/backend-api/codex", cfg.Provider.OpenAICodex.Model
	case "gemini", "google":
		id = "gemini"
		transportName, endpoint, configured = "gemini_native", cfg.Provider.Gemini.BaseURL, cfg.Provider.Gemini.Model
	default:
		raw, ok := cfg.Provider.Custom[id]
		if !ok {
			return nil, nil // The normal constructor owns the unknown-provider error.
		}
		transportName, endpoint, configured = normalizedCustomTransport(raw), raw.BaseURL, raw.Model
		switch transportName {
		case "anthropic_messages", "anthropic":
			endpoint = endpointOrDefault(endpoint, "https://api.anthropic.com")
		case "openai_chat", "openai", "openai_responses":
			endpoint = endpointOrDefault(endpoint, "https://api.openai.com/v1")
		case "gemini_native", "gemini":
			endpoint = endpointOrDefault(endpoint, "https://generativelanguage.googleapis.com")
		default:
			// Cloud profiles are read-only in this store. Preserve their
			// constructors' existing endpoint/auth/error contracts unchanged.
			return nil, nil
		}
	}
	if err := validateCredentialEndpoint(endpoint); err != nil {
		return nil, fmt.Errorf("provider %q: %w", id, err)
	}
	if selected == "" {
		selected = configured
	}
	if selected == "" {
		selected = defaultWireModelForTransport(transportName)
		if id == "openai-codex" {
			selected = "gpt-5.5"
		}
	}
	settings, found, err := config.LookupProviderModelSettings(id, transportName, endpoint, selected)
	if err != nil {
		return nil, fmt.Errorf("provider %q model settings: %w", id, err)
	}
	if !found {
		return nil, nil
	}
	if settings.ContextWindow < 0 || settings.MaxOutputTokens < 0 || settings.OutputBudget < 0 {
		return nil, fmt.Errorf("provider %q: model limits and output budget must not be negative", id)
	}
	if settings.OutputBudget > 0 && id == "openai-codex" {
		return nil, fmt.Errorf("provider %q: outputBudget is not supported by the Codex subscription protocol", id)
	}
	if settings.SupportsVision != nil && *settings.SupportsVision && (transportName == "gemini_native" || transportName == "gemini") {
		return nil, fmt.Errorf("provider %q: supports_vision=true is not available for transport %q", id, transportName)
	}
	return &settings, nil
}

func finalizeProviderModelSettingsBuild(provider llm.Provider, model string, output int, settings *config.ProviderModelSettings) (*ProviderBuild, error) {
	if provider == nil || settings == nil || (settings.ContextWindow == 0 && settings.MaxOutputTokens == 0 && settings.OutputBudget == 0 && settings.SupportsVision == nil && settings.SupportsReasoning == nil && len(settings.ReasoningEfforts) == 0) {
		return finalizeProviderBuild(provider, model, output)
	}
	output = constructedProviderModelOutputBudget(provider, output)
	setProviderModelReasoning(provider, *settings)
	if settings.OutputBudget > 0 {
		if err := setProviderModelOutputBudget(provider, settings.OutputBudget); err != nil {
			return nil, err
		}
		output = settings.OutputBudget
	}
	if settings.SupportsVision != nil {
		provider = withVisionOverride(provider, *settings.SupportsVision)
	}
	if settings.ContextWindow > 0 || settings.MaxOutputTokens > 0 || settings.OutputBudget > 0 || settings.SupportsReasoning != nil || len(settings.ReasoningEfforts) > 0 {
		frozen := *settings
		frozen.ReasoningEfforts = maps.Clone(settings.ReasoningEfforts)
		if settings.SupportsReasoning != nil {
			value := *settings.SupportsReasoning
			frozen.SupportsReasoning = &value
		}
		base := providerModelSettingsProvider{Provider: provider, settings: frozen, defaultOutput: output}
		if policy, ok := provider.(llm.ContextHistoryPolicy); ok {
			provider = &providerModelSettingsHistoryProvider{providerModelSettingsProvider: base, historyPolicy: policy}
		} else {
			provider = &base
		}
	}
	return finalizeProviderBuild(provider, model, output)
}

func constructedProviderModelOutputBudget(provider llm.Provider, fallback int) int {
	switch p := provider.(type) {
	case *anthropic.Anthropic:
		return p.MaxTokens
	case *openai.OpenAI:
		return p.MaxTokens
	case *openai.Responses:
		return p.MaxTokens
	case *gemini.Gemini:
		return p.MaxTokens
	case *azure.Azure:
		return p.MaxTokens
	case *vertex.Vertex:
		return p.MaxTokens
	case *bedrock.Bedrock:
		return p.MaxTokens
	case visionOverrideProvider:
		return constructedProviderModelOutputBudget(p.Provider, fallback)
	case visionOverrideContextProvider:
		return constructedProviderModelOutputBudget(p.Provider, fallback)
	case visionOverrideHistoryProvider:
		return constructedProviderModelOutputBudget(p.Provider, fallback)
	case visionOverrideContextHistoryProvider:
		return constructedProviderModelOutputBudget(p.Provider, fallback)
	}
	return fallback
}

func setProviderModelReasoning(provider llm.Provider, settings config.ProviderModelSettings) {
	disabled := settings.SupportsReasoning != nil && !*settings.SupportsReasoning
	switch p := provider.(type) {
	case *openai.OpenAI:
		p.ReasoningEffortDisabled = disabled
		p.ReasoningEffortValues = maps.Clone(settings.ReasoningEfforts)
	case *openai.Responses:
		p.ReasoningEffortDisabled = disabled
		p.ReasoningEffortValues = maps.Clone(settings.ReasoningEfforts)
	case visionOverrideProvider:
		setProviderModelReasoning(p.Provider, settings)
	case visionOverrideContextProvider:
		setProviderModelReasoning(p.Provider, settings)
	case visionOverrideHistoryProvider:
		setProviderModelReasoning(p.Provider, settings)
	case visionOverrideContextHistoryProvider:
		setProviderModelReasoning(p.Provider, settings)
	}
}

// Mutate only the freshly constructed client, before publication. This keeps
// the actual encoder default, loop reservation and compactor allocation equal.
func setProviderModelOutputBudget(provider llm.Provider, output int) error {
	switch p := provider.(type) {
	case *anthropic.Anthropic:
		p.MaxTokens = output
	case *openai.OpenAI:
		p.MaxTokens = output
	case *openai.Responses:
		if p.Name() == "openai-codex" {
			return fmt.Errorf("provider %q: outputBudget is not supported by the Codex subscription protocol", p.Name())
		}
		p.MaxTokens = output
	case *gemini.Gemini:
		p.MaxTokens = output
	case *azure.Azure:
		p.MaxTokens = output
	case *vertex.Vertex:
		p.MaxTokens = output
	case *bedrock.Bedrock:
		p.MaxTokens = output
	case visionOverrideProvider:
		return setProviderModelOutputBudget(p.Provider, output)
	case visionOverrideContextProvider:
		return setProviderModelOutputBudget(p.Provider, output)
	case visionOverrideHistoryProvider:
		return setProviderModelOutputBudget(p.Provider, output)
	case visionOverrideContextHistoryProvider:
		return setProviderModelOutputBudget(p.Provider, output)
	default:
		return fmt.Errorf("provider %q: configurable outputBudget is not supported by this transport", provider.Name())
	}
	return nil
}

type providerModelSettingsProvider struct {
	llm.Provider
	settings      config.ProviderModelSettings
	defaultOutput int
}

func (p providerModelSettingsProvider) ContextCapabilities() llm.ContextCapabilities {
	base := llm.ContextCapabilities{Window: p.Provider.MaxContextTokens(), Source: "unknown"}
	if metadata, ok := p.Provider.(llm.ContextCapabilitiesProvider); ok {
		base = metadata.ContextCapabilities()
	}
	return applyProviderModelCapabilitySettings(base, p.settings)
}

func (p providerModelSettingsProvider) RefreshContextCapabilities(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if metadata, ok := p.Provider.(llm.ContextCapabilitiesProvider); ok {
		return metadata.RefreshContextCapabilities(ctx)
	}
	return nil
}

func (p providerModelSettingsProvider) MaxContextTokens() int { return p.ContextCapabilities().Window }
func (p providerModelSettingsProvider) VisionCapability() pubprovider.VisionCapability {
	return pubprovider.ProviderVisionCapability(p.Provider)
}
func (p providerModelSettingsProvider) SupportsVision() bool {
	return p.VisionCapability() == pubprovider.VisionSupported
}
func (p providerModelSettingsProvider) ManagesRecoverySession() bool {
	managed, ok := p.Provider.(interface{ ManagesRecoverySession() bool })
	return ok && managed.ManagesRecoverySession()
}

func (p providerModelSettingsProvider) prepareRequest(req llm.Request) (llm.Request, error) {
	if p.settings.SupportsReasoning != nil && !*p.settings.SupportsReasoning {
		req.Effort = ""
	}
	if p.settings.OutputBudget > 0 && (req.MaxTokens <= 0 || req.MaxTokens > p.settings.OutputBudget) {
		req.MaxTokens = p.settings.OutputBudget
	}
	effectiveOutput := req.MaxTokens
	if effectiveOutput <= 0 {
		effectiveOutput = p.defaultOutput
		if effectiveOutput <= 0 {
			effectiveOutput = constructedProviderModelOutputBudget(p.Provider, 0)
		}
	}
	if maximum := p.ContextCapabilities().MaxOutputTokens; maximum > 0 && effectiveOutput > maximum {
		return req, fmt.Errorf("%w: requested %d, maximum %d", llm.ErrContextOutputBudget, effectiveOutput, maximum)
	}
	return req, nil
}
func (p providerModelSettingsProvider) Complete(ctx context.Context, req llm.Request) (*llm.Response, error) {
	req, err := p.prepareRequest(req)
	if err != nil {
		return nil, err
	}
	return p.Provider.Complete(ctx, req)
}
func (p providerModelSettingsProvider) Stream(ctx context.Context, req llm.Request) (llm.StreamReader, error) {
	req, err := p.prepareRequest(req)
	if err != nil {
		return nil, err
	}
	return p.Provider.Stream(ctx, req)
}

type providerModelSettingsHistoryProvider struct {
	providerModelSettingsProvider
	historyPolicy llm.ContextHistoryPolicy
}

func (p providerModelSettingsHistoryProvider) ContextIncludesAssistantBlock(block llm.ContentBlock) bool {
	return p.historyPolicy.ContextIncludesAssistantBlock(block)
}

func smallestKnownModelLimit(values ...int) int {
	result := 0
	for _, value := range values {
		if value > 0 && (result == 0 || value < result) {
			result = value
		}
	}
	return result
}

func applyProviderModelCapabilitySettings(base llm.ContextCapabilities, settings config.ProviderModelSettings) llm.ContextCapabilities {
	out := base
	if settings.ContextWindow > 0 {
		// This is an explicit declaration, not a measured serving-route limit.
		// Keep narrower independently known limits authoritative.
		out.ModelWindow = smallestKnownModelLimit(base.ModelWindow, settings.ContextWindow)
		if base.ModelWindow <= 0 || settings.ContextWindow < base.ModelWindow {
			out.ModelSource = "user"
			if out.ModelOutputSource == "" && base.ModelMaxOutputTokens > 0 {
				out.ModelOutputSource = base.ModelSource
			}
		}
		out.MaxWindow = smallestKnownModelLimit(base.MaxWindow, base.ModelWindow, settings.ContextWindow)
		if out.MaxWindow == settings.ContextWindow {
			out.LimitSource = "user"
		} else if out.LimitSource == "" {
			out.LimitSource = base.ModelSource
		}
		out.Window = settings.ContextWindow
		if base.RequestedWindow > 0 {
			out.Window = base.RequestedWindow
		}
		out.Window = smallestKnownModelLimit(out.Window, out.MaxWindow)
		if base.WindowKind == llm.ContextWindowInput && base.MaxInputTokens > 0 {
			out.Window = smallestKnownModelLimit(out.Window, base.MaxInputTokens)
		} else {
			out.WindowKind = llm.ContextWindowTotal
		}
		if base.RequestedWindow > 0 {
			out.Source = "configured"
			if out.Window < base.RequestedWindow {
				out.Source = "configured_clamped"
			}
		} else {
			out.Source = "user"
			if out.Window < settings.ContextWindow {
				out.Source = out.LimitSource
				if out.Source == "" {
					out.Source = base.Source
				}
			}
		}
	}
	if settings.MaxOutputTokens > 0 {
		out.ModelMaxOutputTokens = smallestKnownModelLimit(base.ModelMaxOutputTokens, settings.MaxOutputTokens)
		if base.ModelMaxOutputTokens <= 0 || settings.MaxOutputTokens < base.ModelMaxOutputTokens {
			out.ModelOutputSource = "user"
		} else if out.ModelOutputSource == "" {
			out.ModelOutputSource = base.ModelSource
		}
		out.MaxOutputTokens = smallestKnownModelLimit(base.MaxOutputTokens, base.ModelMaxOutputTokens, settings.MaxOutputTokens)
	}
	return out
}
