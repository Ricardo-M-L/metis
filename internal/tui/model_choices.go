package tui

import (
	"sort"
	"strings"

	"github.com/Ricardo-M-L/metis/internal/config"
	"github.com/Ricardo-M-L/metis/internal/llm/anthropic"
	"github.com/Ricardo-M-L/metis/internal/llm/openai"
	"github.com/Ricardo-M-L/metis/internal/runtime"
	"github.com/Ricardo-M-L/metis/internal/tui/screen"
	pubprovider "github.com/Ricardo-M-L/metis/pkg/provider"
)

// builtinModelChoices is the curated list shown in the /model picker.
// Hand-maintained rather than auto-discovered because the providers
// don't expose an enumerate endpoint and the model identifier strings
// have meaningful aliases (e.g. "claude-opus-4-7" vs the internal
// "claude-opus-4-7@20251015"). Each entry pairs the canonical model
// ID metis sends to the API with a human-friendly description.
//
// Adding a new model: append to this slice. Removing: drop the entry.
// Custom IDs not in this list are still settable via the inline form
// `/model <id>` — the picker is just for browsing the curated set.
var builtinModelChoices = func() []screen.ModelChoice {
	choices := []screen.ModelChoice{
		// Anthropic — current generation.
		{ID: "claude-opus-4-7", Description: "most capable, best for hard tasks", Provider: "anthropic"},
		{ID: "claude-sonnet-4-6", Description: "fast + smart, balanced", Provider: "anthropic"},
		{ID: "claude-haiku-4-5-20251001", Description: "cheapest, near-instant", Provider: "anthropic"},

		// MiniMax via Anthropic-compatible gateway (yunwu.ai etc.).
		{ID: "MiniMax-M2.7", Description: "open-weight, 192k window, low-cost", Provider: "minimax"},

		// Gemini.
		{ID: "gemini-2.5-pro", Description: "Google's flagship, 1M+ context", Provider: "gemini"},
		{ID: "gemini-2.0-flash", Description: "fast Gemini for high-throughput", Provider: "gemini"},

		// OpenAI Platform API-key models. ChatGPT OAuth models stay under the
		// separate openai-codex provider appended below.
		{ID: "gpt-4o", Description: "OpenAI flagship, multimodal", Provider: "openai"},
		{ID: "gpt-4o-mini", Description: "cheap OpenAI, good for simple tasks", Provider: "openai"},
	}
	for _, model := range openai.CodexModels() {
		description := "ChatGPT subscription"
		if !model.SupportsImage {
			description += " · text only"
		}
		choices = append(choices, screen.ModelChoice{
			ID:          model.ID,
			Description: description,
			Provider:    "openai-codex",
		})
	}
	return choices
}()

// configuredModelChoices returns the concrete provider/model pairs present in
// config.toml. Unlike builtinModelChoices, every entry here has a real profile
// name that switchModel can rebuild, including arbitrary [provider.custom.*]
// profiles. Stable sorting keeps the picker deterministic.
func configuredModelChoices(cfg *config.Config) []screen.ModelChoice {
	if cfg == nil {
		return nil
	}
	var out []screen.ModelChoice
	add := func(providerName, model, transport string) {
		model = strings.TrimSpace(model)
		if model == "" {
			return
		}
		desc := "configured profile"
		if transport != "" {
			desc += " · " + transport
		}
		out = append(out, screen.ModelChoice{
			ID:          model,
			Description: desc,
			Provider:    providerName,
		})
	}

	add("anthropic", cfg.Provider.Anthropic.Model, "anthropic_messages")
	add("openai", cfg.Provider.OpenAI.Model, "openai_chat")
	add("openai-codex", cfg.Provider.OpenAICodex.Model, "openai_codex_responses")
	add("gemini", cfg.Provider.Gemini.Model, "gemini_native")

	ids := make([]string, 0, len(cfg.Provider.Custom))
	for id := range cfg.Provider.Custom {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		raw := cfg.Provider.Custom[id]
		transport := raw.Transport
		if transport == "" {
			transport = "anthropic_messages"
		}
		add(id, raw.Model, transport)
	}
	return out
}

// configuredProviderModel resolves a provider spelling to the canonical
// configured profile and its default model. Provider IDs are matched
// case-insensitively for terminal ergonomics, while the returned name keeps
// the exact config key required by runtime.BuildProvider.
func configuredProviderModel(cfg *config.Config, name string) (providerName, model string, ok bool) {
	name = strings.TrimSpace(name)
	if strings.EqualFold(name, "google") {
		name = "gemini"
	}
	for _, choice := range configuredModelChoices(cfg) {
		if strings.EqualFold(choice.Provider, name) {
			return choice.Provider, choice.ID, true
		}
	}
	return "", "", false
}

func configuredProviderNames(cfg *config.Config) []string {
	choices := configuredModelChoices(cfg)
	out := make([]string, 0, len(choices))
	for _, choice := range choices {
		out = append(out, choice.Provider)
	}
	return out
}

func modelChoiceKey(c screen.ModelChoice) string {
	return strings.ToLower(c.Provider) + "\x00" + strings.ToLower(c.ID)
}

func modelChoiceVisionCapability(cfg *config.Config, c screen.ModelChoice) pubprovider.VisionCapability {
	if cfg == nil || c.ID == "" {
		return pubprovider.VisionUnsupported
	}
	transport := ""
	switch c.Provider {
	case "anthropic":
		transport = "anthropic_messages"
	case "openai":
		transport = "openai_chat"
	case "openai-codex":
		transport = "openai_codex_responses"
	case "gemini", "google":
		// The native Gemini adapter does not encode image content blocks yet.
		return pubprovider.VisionUnsupported
	default:
		raw, ok := cfg.Provider.Custom[c.Provider]
		if !ok {
			return pubprovider.VisionUnsupported
		}
		transport = raw.Transport
		if transport == "" {
			transport = "anthropic_messages"
		}
		if raw.SupportsVision != nil {
			// Match runtime.BuildProvider: a positive Gemini override cannot be
			// honored until its native encoder supports image content blocks.
			if *raw.SupportsVision && (transport == "gemini_native" || transport == "gemini") {
				return pubprovider.VisionUnsupported
			}
			if *raw.SupportsVision {
				return pubprovider.VisionSupported
			}
			return pubprovider.VisionUnsupported
		}
	}

	switch transport {
	case "openai_chat", "openai_responses", "openai_codex_responses", "azure_openai":
		return openai.VisionCapabilityForModel(c.ID)
	case "anthropic_messages", "bedrock_anthropic", "vertex_anthropic":
		return anthropic.VisionCapabilityForModel(c.ID)
	default:
		return pubprovider.VisionUnsupported
	}
}

func modelChoiceSupportsVision(cfg *config.Config, c screen.ModelChoice) bool {
	return modelChoiceVisionCapability(cfg, c) != pubprovider.VisionUnsupported
}

// modelChoiceHasCredentials mirrors BuildProvider's first precondition without
// constructing a client or triggering a preconnect. The merged Config always
// contains default Anthropic/OpenAI model IDs, even when the user has no key;
// treating those defaults as usable recovery targets produces a picker whose
// first choice can only fail. ResolveAPIKey covers env, auth.json and inline
// config for API-key transports. Cloud-auth transports are checked against
// their real credential shape by runtime.ProviderHasCredentials.
func modelChoiceHasCredentials(cfg *config.Config, c screen.ModelChoice) bool {
	return runtime.ProviderHasCredentials(cfg, c.Provider)
}

func (m *Model) modelPickerChoices(requireVision bool) []screen.ModelChoice {
	configured := configuredModelChoices(m.cfg)
	out := make([]screen.ModelChoice, 0, len(builtinModelChoices)+len(configured))
	seen := make(map[string]struct{}, len(builtinModelChoices)+len(configured))
	for _, group := range [][]screen.ModelChoice{builtinModelChoices, configured} {
		for _, c := range group {
			if !modelChoiceHasCredentials(m.cfg, c) {
				continue
			}
			if requireVision {
				capability := modelChoiceVisionCapability(m.cfg, c)
				if capability == pubprovider.VisionUnsupported {
					continue
				}
				if capability == pubprovider.VisionSupported {
					c.Description = "configured vision model"
				} else {
					c.Description = "configured model · image support unverified"
				}
			}
			key := modelChoiceKey(c)
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			out = append(out, c)
		}
	}
	return out
}

// providerPickerChoices is deliberately narrower than /model: a failover
// picker must not advertise a profile that cannot even construct locally.
// Credential checks are local and transport-aware; remote health remains the
// provider's responsibility when the next request is sent.
func defaultProviderConfigLoader() (*config.Config, error) {
	cfg, _, err := config.Load()
	if err != nil {
		return nil, err
	}
	if err := config.ApplyProviderPolicyForWorkspace(cfg, false); err != nil {
		return nil, err
	}
	return cfg, nil
}

func (m *Model) reloadProviderProfiles() error {
	if m == nil || m.providerConfigLoader == nil {
		return nil
	}
	fresh, err := m.providerConfigLoader()
	if err != nil {
		return err
	}
	if fresh == nil {
		return nil
	}
	if m.cfg == nil {
		m.cfg = fresh
	} else {
		// Provider failover is hot-reloadable; unrelated runtime settings keep
		// their startup semantics and are not silently reapplied mid-session.
		m.cfg.Provider = fresh.Provider
	}
	return nil
}

func (m *Model) providerPickerChoices() ([]screen.ModelChoice, error) {
	if err := m.reloadProviderProfiles(); err != nil {
		return nil, err
	}
	configured := configuredModelChoices(m.cfg)
	out := make([]screen.ModelChoice, 0, len(configured))
	for _, choice := range configured {
		if !modelChoiceHasCredentials(m.cfg, choice) {
			continue
		}
		choice.Description = "ready · " + choice.Description
		out = append(out, choice)
	}
	return out, nil
}

// openModelPicker opens either the ordinary model browser or the restricted
// recovery picker used when a text-only model receives image attachments.
// It returns false when no configured vision-capable profile exists.
func (m *Model) openModelPicker(requireVision bool, recoveryImageCount int) (bool, error) {
	if m == nil || m.turnActive || m.rewindSummaryPending {
		if m != nil && requireVision {
			m.imageRecoveryPending = false
			m.imageRecoveryImageCount = 0
		}
		return false, nil
	}
	if err := m.reloadProviderProfiles(); err != nil {
		return false, err
	}
	choices := m.modelPickerChoices(requireVision)
	if len(choices) == 0 {
		if requireVision {
			m.imageRecoveryPending = false
			m.imageRecoveryImageCount = 0
		}
		return false, nil
	}
	for i := range choices {
		choices[i].Recent = getModelState().IsRecent(choices[i].ID)
	}
	picker := screen.NewModelScreen(m.model, choices)
	picker.SetCurrentProvider(m.providerName)
	if requireVision {
		picker.SetTitle("Choose a vision model · prompt and images are kept")
	} else {
		picker.SetTitle("Pick a model · configured credentials only")
	}
	picker.Resize(m.width, m.height)
	m.activeScreen = picker
	m.imageRecoveryPending = requireVision
	if requireVision {
		m.imageRecoveryImageCount = max(0, recoveryImageCount)
	} else {
		m.imageRecoveryImageCount = 0
	}
	return true, nil
}

// openProviderPicker exposes configured, credential-ready provider profiles
// as an explicit failover surface. ModelScreen already carries provider +
// model together, so applying a row reaches the same atomic rebuild path as
// /model without duplicating widget or runtime logic.
func (m *Model) openProviderPicker() (bool, error) {
	if m == nil || m.turnActive || m.rewindSummaryPending {
		return false, nil
	}
	choices, err := m.providerPickerChoices()
	if err != nil {
		return false, err
	}
	if len(choices) == 0 {
		return false, nil
	}
	picker := screen.NewModelScreen(m.model, choices)
	picker.SetCurrentProviderOnly(m.providerName)
	picker.SetCommand("/provider")
	title := "Switch provider · configured credentials only"
	if len(choices) == 1 {
		title = "Switch provider · only 1 ready; add another with /login"
	}
	picker.SetTitle(title)
	picker.Resize(m.width, m.height)
	m.activeScreen = picker
	m.imageRecoveryPending = false
	m.imageRecoveryImageCount = 0
	return true, nil
}

func configuredProviderForModel(cfg *config.Config, model string) string {
	matches := ""
	for _, c := range configuredModelChoices(cfg) {
		if c.ID != model {
			continue
		}
		if matches != "" && matches != c.Provider {
			return "" // ambiguous: caller should stay on the current profile
		}
		matches = c.Provider
	}
	return matches
}

func splitConfiguredProviderModel(cfg *config.Config, input string) (providerName, model string) {
	providerName, model, ok := strings.Cut(input, "/")
	if !ok || providerName == "" || model == "" || cfg == nil {
		return "", input
	}
	if providerName == "anthropic" || providerName == "openai" || providerName == "openai-codex" || providerName == "gemini" || providerName == "google" {
		return providerName, model
	}
	if _, ok := cfg.Provider.Custom[providerName]; ok {
		return providerName, model
	}
	return "", input
}
