package webui

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"

	"github.com/Ricardo-M-L/metis/internal/agent"
	pubsess "github.com/Ricardo-M-L/metis/pkg/session"
)

const parentOwnedInputReason = "parent_owned"

// parentOwnedInput reports child identities before an ordinary root turn can
// acquire its workspace or launch a worker. A retained child transcript remains
// parent-managed after completion; an unreadable child record never grants a
// route around that ownership.
func (s *Server) parentOwnedInput(id string, header *pubsess.Header) (string, bool) {
	if !validSessionID(id) {
		return "", false
	}
	if header != nil && header.SubAgentOf != "" {
		return header.SubAgentOf, true
	}
	if s.store != nil {
		child, err := agent.LoadSubAgentHeader(s.store.Dir, id)
		if err == nil {
			return child.SubAgentOf, true
		}
		if !errors.Is(err, os.ErrNotExist) {
			path := filepath.Join(s.store.Dir, agent.SubAgentTranscriptDirname, id+".jsonl")
			if _, statErr := os.Lstat(path); statErr == nil || !errors.Is(statErr, os.ErrNotExist) {
				return "", true
			}
		}
	}
	s.workerTurnsMu.Lock()
	for parentID, snapshot := range s.workerSnapshots {
		for _, child := range snapshot.status.Agents {
			if child.AgentID == id {
				s.workerTurnsMu.Unlock()
				return parentID, true
			}
		}
		if _, ok := snapshot.unobserved[id]; ok {
			s.workerTurnsMu.Unlock()
			return parentID, true
		}
	}
	s.workerTurnsMu.Unlock()
	if s.roster != nil {
		if child, ok := s.roster.LookupByAgentID(id); ok && child != nil {
			return "", true
		}
	}
	return "", false
}

func (s *Server) rejectParentOwnedInput(w http.ResponseWriter, id string, header *pubsess.Header) bool {
	parentID, owned := s.parentOwnedInput(id, header)
	if !owned {
		return false
	}
	writeJSON(w, http.StatusForbidden, map[string]any{
		"error":                    "this sub-agent is managed by its parent; continue the task in the parent conversation",
		"sessionId":                id,
		"parentSessionId":          parentID,
		"ownership":                "parent",
		"canAcceptDirectInput":     false,
		"directInputBlockedReason": parentOwnedInputReason,
	})
	return true
}
