package webui

import (
	"net/http"

	"github.com/Ricardo-M-L/metis/internal/llm/catalog"
)

type routingModelView struct {
	Provider       string  `json:"provider"`
	Model          string  `json:"model"`
	Current        bool    `json:"current"`
	Reasoning      *bool   `json:"reasoning,omitempty"`
	Attachment     *bool   `json:"attachment,omitempty"`
	ToolCall       *bool   `json:"toolCall,omitempty"`
	ContextWindow  int     `json:"contextWindow,omitempty"`
	InputCostPerM  float64 `json:"inputCostPerM,omitempty"`
	CapabilityNote string  `json:"capabilityNote,omitempty"`
}

func boolPtr(value bool) *bool { return &value }

func (s *Server) handleRouting(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	cfg, err := s.loadProviderConfig()
	if err != nil || cfg == nil {
		writeError(w, http.StatusInternalServerError, "config unreadable")
		return
	}
	s.stateMu.RLock()
	currentProvider, currentModel := s.activeProviderName, s.activeModel
	s.stateMu.RUnlock()
	client := catalog.Default()
	models := make([]routingModelView, 0)
	for _, configured := range listConfiguredModels(cfg) {
		view := routingModelView{
			Provider: configured.Provider, Model: configured.Model,
			Current:        configured.Provider == currentProvider && configured.Model == currentModel,
			CapabilityNote: "No cached catalog metadata; the configured model remains available for manual selection.",
		}
		if client != nil {
			for _, hit := range client.LookupModel(configured.Model) {
				model := hit.Model
				view.Reasoning = boolPtr(model.Reasoning)
				view.Attachment = boolPtr(model.Attachment || model.SupportsImage())
				view.ToolCall = boolPtr(model.ToolCall)
				view.ContextWindow = model.Limit.Context
				view.InputCostPerM = model.Cost.Input
				view.CapabilityNote = "Capabilities come from the local models.dev cache."
				break
			}
		}
		models = append(models, view)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"mode":    "manual",
		"current": map[string]string{"provider": currentProvider, "model": currentModel},
		"rules": []map[string]string{
			{"name": "Pinned model", "description": "Desktop keeps the selected provider/model unless the user changes it."},
			{"name": "Attachments", "description": "Image turns require an attachment-capable model; unknown private gateways are allowed to adjudicate."},
			{"name": "Reasoning effort", "description": "The effort control appears only when model and transport advertise a safe mapping."},
			{"name": "Tool use", "description": "Agent turns require a tool-call-capable model; catalog facts are shown for inspection."},
		},
		"models": models,
		"note":   "Automatic cheapest-model switching is not enabled; this overview is read-only and never exposes credentials.",
	})
}
