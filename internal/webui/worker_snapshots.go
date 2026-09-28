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

// workerViewRoster is the explicitly requested conversation's compact worker
// roster. It never consults the process-wide active session or legacy Loop:
// absence of a worker snapshot is an empty roster, not another session's
// agents. The top-level /api/status fields retain their existing semantics.
func (s *Server) workerViewRoster(sessionID string) map[string]any {
	view := map[string]any{
		"sessionId": sessionID, "subAgents": 0, "namedAgents": 0,
		"backgroundTasks": 0, "agents": make([]map[string]any, 0),
		"jobs": make([]map[string]any, 0),
	}
	status, ok := s.workerSnapshot(sessionID)
	if !ok {
		return view
	}
	agents := make([]map[string]any, 0, len(status.Agents))
	for _, item := range status.Agents {
		agents = append(agents, map[string]any{
			"sessionId": sessionID, "name": item.Name, "agentId": item.AgentID,
			"status": item.Status, "background": item.Background,
			"startedAt": item.StartedAt,
		})
	}
	jobs := make([]map[string]any, 0, len(status.Jobs))
	for _, item := range status.Jobs {
		jobs = append(jobs, map[string]any{
			"sessionId": sessionID, "id": item.ID,
			"description": item.Description, "status": item.Status,
			"startedAt": item.StartedAt,
		})
	}
	view["subAgents"] = status.SubAgents
	view["namedAgents"] = status.NamedAgents
	view["backgroundTasks"] = status.BackgroundTasks
	view["agents"] = agents
	view["jobs"] = jobs
	return view
}

func (s *Server) workerSubAgentDetail(sessionID, agentID string) (subAgentDetailView, bool) {
	if s == nil {
		return subAgentDetailView{}, false
	}
	s.workerTurnsMu.Lock()
	defer s.workerTurnsMu.Unlock()
	if sessionID == "" {
		return subAgentDetailView{}, false
	}
	snapshot, exists := s.workerSnapshots[sessionID]
	if !exists {
		return subAgentDetailView{}, false
	}
	for _, a := range snapshot.status.Agents {
		if a.AgentID != agentID {
			continue
		}
		return subAgentDetailView{
			SessionID: sessionID, Name: a.Name, AgentID: a.AgentID, Status: a.Status, Background: a.Background,
			StartedAt: a.StartedAt, EndedAt: a.EndedAt, ElapsedMS: a.ElapsedMS,
			Output: a.Output, OutputTruncated: a.OutputTruncated, Result: a.Result,
			ResultTruncated: a.ResultTruncated, StopHint: a.StopHint, ExitError: a.ExitError,
		}, true
	}
	return subAgentDetailView{}, false
}
