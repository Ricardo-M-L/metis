package webui

import (
	"context"
	"net/http"
	"time"

	"github.com/Ricardo-M-L/metis/internal/agent"
)

func subAgentStatusActive(status string) bool {
	return status == agent.StatusQueued.String() || status == agent.StatusRunning.String()
}

// A child's identity must belong to the requested conversation before any
// control is sent. Worker snapshots are already keyed by their owning turn;
// the shared in-process roster requires the durable transcript ownership check.
func (s *Server) handleSubAgentStop(w http.ResponseWriter, r *http.Request, sessionID, agentID string) {
	view, found := s.subAgentDetailView(sessionID, agentID)
	if !found {
		writeError(w, http.StatusNotFound, "sub-agent not found")
		return
	}
	if !subAgentStatusActive(view.Status) {
		writeJSON(w, http.StatusOK, map[string]any{"agent": view})
		return
	}
	s.workerTurnsMu.Lock()
	worker := s.workerTurns[sessionID]
	s.workerTurnsMu.Unlock()
	if worker != nil {
		request := IsolatedSteerRequest{StopSubAgentID: agentID, Reply: make(chan bool, 1)}
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		select {
		case worker.steer <- request:
		case <-worker.done:
			writeError(w, http.StatusConflict, "sub-agent worker has finished")
			return
		case <-ctx.Done():
			writeError(w, http.StatusGatewayTimeout, "sub-agent stop was not acknowledged")
			return
		}
		select {
		case accepted := <-request.Reply:
			if !accepted {
				writeError(w, http.StatusConflict, "sub-agent no longer accepts cancellation")
				return
			}
		case <-worker.done:
			writeError(w, http.StatusConflict, "sub-agent worker has finished")
			return
		case <-ctx.Done():
			writeError(w, http.StatusGatewayTimeout, "sub-agent stop was not acknowledged")
			return
		}
	} else {
		if !s.subAgentBelongsToSession(sessionID, agentID) || s.roster == nil {
			writeError(w, http.StatusConflict, "sub-agent runtime is unavailable")
			return
		}
		teammate, ok := s.roster.LookupByAgentID(agentID)
		if !ok || teammate == nil {
			writeError(w, http.StatusConflict, "sub-agent runtime is unavailable")
			return
		}
		if teammate.Snapshot().Status.IsActive() {
			teammate.RequestCancel()
		}
	}
	// Cancellation is acknowledged, not fabricated as a completed lifecycle.
	// The runner publishes killed only after it has actually unwound.
	if updated, ok := s.subAgentDetailView(sessionID, agentID); ok {
		view = updated
	}
	writeJSON(w, http.StatusOK, map[string]any{"agent": view})
}
