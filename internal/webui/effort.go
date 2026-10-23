package webui

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/Ricardo-M-L/metis/internal/auth"
	"github.com/Ricardo-M-L/metis/internal/config"
	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/llm/catalog"
	"github.com/Ricardo-M-L/metis/internal/session"
)

type effortCapability struct {
	Supported bool     `json:"supported"`
	Reason    string   `json:"reason,omitempty"`
	Options   []string `json:"options,omitempty"`
}

func effortHeaderValue(e llm.Effort) string {
	if e == llm.EffortDefault {
		return "default"
	}
	return string(e)
}

func effortFromHeader(value string) llm.Effort {
	if value == "" || value == "default" {
		return llm.EffortDefault
	}
	e, ok := llm.ParseEffort(strings.ToLower(strings.TrimSpace(value)))
	if !ok {
		return llm.EffortDefault
	}
	return e
}

func configuredTransport(cfg *config.Config, providerName string) string {
	switch providerName {
	case "anthropic":
		return "anthropic_messages"
	case "openai":
		if cfg != nil && strings.EqualFold(strings.TrimSpace(cfg.Provider.OpenAI.WireProtocol), "responses") {
			return "openai_responses"
		}
		return "openai_chat"
	case "openai-codex":
		return "openai_codex_responses"
	case "gemini", "google":
		return "gemini_native"
	}
	if cfg != nil {
		if raw, ok := cfg.Provider.Custom[providerName]; ok {
			transport := strings.ToLower(strings.TrimSpace(raw.Transport))
			if transport == "" {
				return "anthropic_messages"
			}
			return transport
		}
	}
	return ""
}

// reasoningEffortCapability is intentionally conservative. An explicit
// catalog "reasoning=false" or Gemini native's disabled thought-signature
// path suppresses the control; private/unknown model ids do not get a fake
// dial merely because their gateway speaks an OpenAI-compatible protocol.
func reasoningEffortCapability(cfg *config.Config, providerName, model string) effortCapability {
	providerName = auth.CanonicalProviderID(providerName)
	transport := configuredTransport(cfg, providerName)
	if transport == "gemini" || transport == "gemini_native" {
		return effortCapability{Reason: "Gemini thinking is disabled until thought-signature replay is available"}
	}
	if declaration, found, err := savedReasoningDeclaration(cfg, providerName, model); err != nil {
		return effortCapability{Reason: "Saved model reasoning settings are unavailable"}
	} else if found && (declaration.SupportsReasoning != nil || len(declaration.ReasoningEfforts) > 0) {
		if declaration.SupportsReasoning == nil {
			return effortCapability{Reason: "Reasoning mappings require an explicit model capability"}
		}
		if !*declaration.SupportsReasoning {
			return effortCapability{Reason: "Reasoning effort is disabled for this model", Options: []string{"default"}}
		}
		openAI := transport == "openai" || transport == "openai_chat" || transport == "openai_responses" ||
			transport == "openai_codex_responses" || transport == "azure" || transport == "azure_openai"
		anthropic := transport == "anthropic" || transport == "anthropic_messages" || transport == "vertex" ||
			transport == "vertex_anthropic" || transport == "bedrock" || transport == "bedrock_anthropic"
		if !openAI && !anthropic || len(declaration.ReasoningEfforts) > 0 && !openAI {
			return effortCapability{Reason: "The configured transport has no model-specific reasoning-effort mapping"}
		}
		options := []string{"default"}
		for _, level := range []string{"low", "medium", "high"} {
			if len(declaration.ReasoningEfforts) == 0 || declaration.ReasoningEfforts[level] != "" {
				options = append(options, level)
			}
		}
		return effortCapability{Supported: true, Options: options}
	}
	if cli := catalog.Default(); cli != nil {
		if supported, found := cli.LookupReasoningByModelID(model); found {
			if supported {
				return effortCapability{Supported: true}
			}
			return effortCapability{Reason: "The model catalog marks this model as non-reasoning"}
		}
	}
	m := strings.ToLower(strings.TrimSpace(model))
	supportedByModel := strings.HasPrefix(m, "o1") || strings.HasPrefix(m, "o3") || strings.HasPrefix(m, "o4") ||
		strings.HasPrefix(m, "gpt-5") || strings.Contains(m, "codex") || strings.Contains(m, "reasoning") ||
		strings.Contains(m, "deepseek-r1") || strings.Contains(m, "claude-3-7") || strings.Contains(m, "claude-3.7") ||
		strings.Contains(m, "claude-sonnet-4") || strings.Contains(m, "claude-opus-4") || strings.Contains(m, "claude-haiku-4")
	if !supportedByModel {
		return effortCapability{Reason: "Reasoning effort is not advertised for this model"}
	}
	switch transport {
	case "openai", "openai_chat", "openai_responses", "openai_codex_responses", "anthropic", "anthropic_messages", "bedrock", "bedrock_anthropic", "vertex", "vertex_anthropic":
		return effortCapability{Supported: true}
	default:
		return effortCapability{Reason: "The configured transport has no reasoning-effort mapping"}
	}
}

// Settings declarations are bound to the exact saved endpoint and wire model.
// No credential resolution, endpoint discovery or provider build is needed.
func savedReasoningDeclaration(cfg *config.Config, providerName, model string) (config.ProviderModelSettings, bool, error) {
	if cfg == nil {
		return config.ProviderModelSettings{}, false, nil
	}
	transport := configuredTransport(cfg, providerName)
	// Cloud-auth profiles are read-only in the Desktop model library. Their
	// resource/region syntax is not an API-key endpoint binding; retain the
	// existing catalog-based capability path instead of rejecting that syntax.
	switch transport {
	case "azure", "azure_openai", "vertex", "vertex_anthropic", "bedrock", "bedrock_anthropic":
		return config.ProviderModelSettings{}, false, nil
	}
	baseURL := ""
	switch providerName {
	case "openai":
		baseURL = cfg.Provider.OpenAI.BaseURL
	case "anthropic":
		baseURL = cfg.Provider.Anthropic.BaseURL
	case "gemini":
		baseURL = cfg.Provider.Gemini.BaseURL
	case "openai-codex":
		transport, baseURL = "openai_responses", openAICodexBaseURL
	default:
		raw, exists := cfg.Provider.Custom[providerName]
		if !exists {
			return config.ProviderModelSettings{}, false, nil
		}
		baseURL = raw.BaseURL
	}
	if baseURL == "" {
		switch transport {
		case "openai", "openai_chat", "openai_responses":
			baseURL = "https://api.openai.com/v1"
		case "anthropic", "anthropic_messages":
			baseURL = "https://api.anthropic.com"
		case "gemini", "gemini_native":
			baseURL = "https://generativelanguage.googleapis.com"
		default:
			return config.ProviderModelSettings{}, false, nil
		}
	}
	return config.LookupProviderModelSettings(providerName, transport, baseURL, model)
}

func effortCapabilityOptions(capability effortCapability) []string {
	if len(capability.Options) > 0 {
		return capability.Options
	}
	return []string{"default", "low", "medium", "high"}
}

func effortOptionAllowed(options []string, effort string) bool {
	for _, option := range options {
		if option == effort {
			return true
		}
	}
	return false
}

func (s *Server) handleEffort(w http.ResponseWriter, r *http.Request) {
	if s.loop == nil {
		writeError(w, http.StatusServiceUnavailable, "agent runtime unavailable")
		return
	}
	s.effortMu.Lock()
	defer s.effortMu.Unlock()
	// Effort is a live preference, so a POST is allowed while Loop.Run owns
	// runMu. buildRequest snapshots it behind Loop.effortMu and the following
	// model request observes the new value. Model and session transitions take
	// this same mutex, which keeps capability validation, persistence and the
	// live update on one coherent provider runtime without blocking on a turn.
	cfg, err := s.loadProviderConfig()
	if err != nil || cfg == nil {
		writeError(w, http.StatusInternalServerError, "config unreadable")
		return
	}
	s.stateMu.RLock()
	providerName, model, sessionID := s.activeProviderName, s.activeModel, s.activeSessionID
	s.stateMu.RUnlock()
	capability := reasoningEffortCapability(cfg, providerName, model)
	options := effortCapabilityOptions(capability)
	applies := "next model request"
	if s.isolatedRunner != nil {
		applies = "next turn"
	}
	switch r.Method {
	case http.MethodGet:
		effectiveEffort := effortHeaderValue(s.loop.EffortValue())
		if !capability.Supported || !effortOptionAllowed(options, effectiveEffort) {
			effectiveEffort = "default"
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"effort": effectiveEffort, "options": options, "applies": applies,
			"supported": capability.Supported, "reason": capability.Reason, "provider": providerName, "model": model,
		})
	case http.MethodPost:
		if !capability.Supported {
			writeError(w, http.StatusConflict, capability.Reason)
			return
		}
		var body struct {
			Effort string `json:"effort"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&body); err != nil {
			writeError(w, http.StatusBadRequest, "invalid body")
			return
		}
		body.Effort = strings.ToLower(strings.TrimSpace(body.Effort))
		if body.Effort == "default" {
			body.Effort = ""
		}
		effort, ok := llm.ParseEffort(body.Effort)
		if !ok {
			writeError(w, http.StatusBadRequest, "effort must be default, low, medium, or high")
			return
		}
		if !effortOptionAllowed(options, effortHeaderValue(effort)) {
			writeError(w, http.StatusBadRequest, "reasoning effort is not configured for this model")
			return
		}
		if sessionID != "" && s.store != nil {
			if err := s.store.WriteHeaderFull(session.Header{ID: sessionID, Effort: effortHeaderValue(effort)}); err != nil {
				writeError(w, http.StatusInternalServerError, "effort persistence failed; live effort was not changed")
				return
			}
		}
		s.loop.SetEffort(effort)
		writeJSON(w, http.StatusOK, map[string]any{"effort": effortHeaderValue(effort), "supported": true, "applies": applies})
	default:
		w.Header().Set("Allow", "GET, POST")
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}
