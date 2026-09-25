package webui

import (
	"github.com/Ricardo-M-L/metis/internal/desktopipc"
	"time"
)

type isolatedWorkerSnapshot struct {
	status  desktopipc.Status
	updated time.Time
}

// Keep active workers and a bounded set of recent sessions for the detail
// drawer. A session's runtime belongs to its worker, never the selected Loop.
func (s *Server) setWorkerSnapshot(sessionID string, status desktopipc.Status) {
	if s == nil {
		return
	}
	s.workerTurnsMu.Lock()
	defer s.workerTurnsMu.Unlock()
	if s.workerSnapshots == nil {
		s.workerSnapshots = make(map[string]isolatedWorkerSnapshot)
	}
	s.workerSnapshots[sessionID] = isolatedWorkerSnapshot{status: status, updated: time.Now()}
	for len(s.workerSnapshots) > 16 {
		oldest := ""
		var oldestTime time.Time
		for id, snapshot := range s.workerSnapshots {
			if s.workerTurns[id] != nil || id == sessionID {
				continue
			}
			if oldest == "" || snapshot.updated.Before(oldestTime) {
				oldest, oldestTime = id, snapshot.updated
			}
		}
		if oldest == "" {
			break
		}
		delete(s.workerSnapshots, oldest)
	}
}

func (s *Server) workerSnapshot(sessionID string) (desktopipc.Status, bool) {
	if s == nil {
		return desktopipc.Status{}, false
	}
	s.workerTurnsMu.Lock()
	defer s.workerTurnsMu.Unlock()
	snapshot, ok := s.workerSnapshots[sessionID]
	return snapshot.status, ok
}

func (s *Server) workerSubAgentDetail(agentID string) (subAgentDetailView, bool) {
	if s == nil {
		return subAgentDetailView{}, false
	}
	s.workerTurnsMu.Lock()
	defer s.workerTurnsMu.Unlock()
	for _, snapshot := range s.workerSnapshots {
		for _, a := range snapshot.status.Agents {
			if a.AgentID != agentID {
				continue
			}
			return subAgentDetailView{
				Name: a.Name, AgentID: a.AgentID, Status: a.Status, Background: a.Background,
				StartedAt: a.StartedAt, EndedAt: a.EndedAt, ElapsedMS: a.ElapsedMS,
				Output: a.Output, OutputTruncated: a.OutputTruncated, Result: a.Result,
				ResultTruncated: a.ResultTruncated, StopHint: a.StopHint, ExitError: a.ExitError,
			}, true
		}
	}
	return subAgentDetailView{}, false
}
