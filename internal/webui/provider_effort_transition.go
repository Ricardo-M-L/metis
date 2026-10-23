package webui

import (
	"fmt"

	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/session"
)

func effectiveEffortForCapabilities(capability effortCapability, requested llm.Effort) llm.Effort {
	if !capability.Supported || !effortOptionAllowed(effortCapabilityOptions(capability), effortHeaderValue(requested)) {
		return llm.EffortDefault
	}
	return requested
}

// Caller holds effortMu. A previously saved effort may no longer be accepted
// after the user edits this exact model's declared slots. Repair the durable
// value before activating a Loop or launching a worker that reads its header.
func (s *Server) restoreSessionEffort(id string, header *session.Header, provider, model string) (llm.Effort, error) {
	requested := effortFromHeader(header.Effort)
	if requested == llm.EffortDefault && (header.Effort == "" || header.Effort == "default") {
		return requested, nil
	}
	cfg, err := s.loadProviderConfig()
	if err != nil || cfg == nil {
		return llm.EffortDefault, fmt.Errorf("reasoning-effort preflight: provider configuration unreadable")
	}
	selected := effectiveEffortForCapabilities(reasoningEffortCapability(cfg, provider, model), requested)
	if header.Effort != effortHeaderValue(selected) {
		if err := s.store.WriteHeaderFull(session.Header{ID: id, Effort: effortHeaderValue(selected)}); err != nil {
			return llm.EffortDefault, fmt.Errorf("persist restored reasoning effort: %w", err)
		}
		header.Effort = effortHeaderValue(selected)
	}
	return selected, nil
}
