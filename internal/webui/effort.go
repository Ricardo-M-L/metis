package webui

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/Ricardo-M-L/metis/internal/config"
	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/llm/catalog"
	"github.com/Ricardo-M-L/metis/internal/session"
)

type effortCapability struct {
	Supported bool   `json:"supported"`
	Reason    string `json:"reason,omitempty"`
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
		return "openai_chat"
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
	transport := configuredTransport(cfg, providerName)
	if transport == "gemini" || transport == "gemini_native" {
		return effortCapability{Reason: "Gemini thinking is disabled until thought-signature replay is available"}
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
	case "openai", "openai_chat", "openai_responses", "anthropic", "anthropic_messages", "bedrock", "bedrock_anthropic", "vertex", "vertex_anthropic":
		return effortCapability{Supported: true}
	default:
		return effortCapability{Reason: "The configured transport has no reasoning-effort mapping"}
	}
}

func (s *Server) handleEffort(w http.ResponseWriter, r *http.Request) {
	if s.loop == nil {
		writeError(w, http.StatusServiceUnavailable, "agent runtime unavailable")
		return
	}
	s.effortMu.Lock()
	defer s.effortMu.Unlock()
	cfg, _, err := config.Load()
	if err != nil || cfg == nil {
		writeError(w, http.StatusInternalServerError, "config unreadable")
		return
	}
	s.stateMu.RLock()
	providerName, model, sessionID := s.activeProviderName, s.activeModel, s.activeSessionID
	s.stateMu.RUnlock()
	capability := reasoningEffortCapability(cfg, providerName, model)
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, map[string]any{
			"effort": effortHeaderValue(s.loop.EffortValue()), "options": []string{"default", "low", "medium", "high"},
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
		s.loop.SetEffort(effort)
		if sessionID != "" && s.store != nil {
			if err := s.store.WriteHeaderFull(session.Header{ID: sessionID, Effort: effortHeaderValue(effort)}); err != nil {
				writeError(w, http.StatusInternalServerError, "effort changed in memory but session persistence failed")
				return
			}
		}
		writeJSON(w, http.StatusOK, map[string]any{"effort": effortHeaderValue(effort), "supported": true, "applies": "next model request"})
	default:
		w.Header().Set("Allow", "GET, POST")
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}
