package webui

import (
	"time"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/desktopipc"
)

type isolatedWorkerSnapshot struct {
	status  desktopipc.Status
	updated time.Time
	// Creation events precede the UI's first detail request. Retain identities
	// not yet observed by the periodic sampler, including a sampler frame that
	// was captured just before creation but written to the pipe afterward.
	unobserved map[string]desktopipc.Subagent
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
	unobserved := s.workerSnapshots[sessionID].unobserved
	for _, item := range status.Agents {
		if pending, ok := unobserved[item.AgentID]; ok &&
			(item.Status == pending.Status || !subAgentStatusActive(item.Status)) {
			delete(unobserved, item.AgentID)
		}
	}
	s.workerSnapshots[sessionID] = isolatedWorkerSnapshot{status: status, updated: time.Now(), unobserved: unobserved}
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

func (s *Server) recordWorkerSubAgentLifecycle(sessionID string, event agent.Event) {
	if s == nil || sessionID == "" || event.SubAgentID == "" ||
		(event.Kind != agent.EventSubAgentStart && event.Kind != agent.EventSubAgentEnd) {
		return
	}
	s.workerTurnsMu.Lock()
	defer s.workerTurnsMu.Unlock()
	if s.workerSnapshots == nil {
		s.workerSnapshots = make(map[string]isolatedWorkerSnapshot)
	}
	snapshot := s.workerSnapshots[sessionID]
	snapshot.updated = time.Now()
	if event.Kind == agent.EventSubAgentEnd {
		delete(snapshot.unobserved, event.SubAgentID)
		s.workerSnapshots[sessionID] = snapshot
		return
	}
	if !subAgentStatusActive(event.SubAgentStatus) {
		return
	}
	item := desktopipc.Subagent{AgentID: event.SubAgentID, Name: event.SubAgentName, Status: event.SubAgentStatus, Background: event.SubAgentBackground, StartedAt: time.Now()}
	if snapshot.unobserved == nil {
		snapshot.unobserved = make(map[string]desktopipc.Subagent)
	}
	if previous, ok := snapshot.unobserved[item.AgentID]; ok {
		item.StartedAt = previous.StartedAt
	}
	snapshot.unobserved[item.AgentID] = item
	s.workerSnapshots[sessionID] = snapshot
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
		if pending, ok := snapshot.unobserved[agentID]; ok && subAgentStatusActive(a.Status) {
			a.Status = pending.Status
		}
		return subAgentDetailFromWorker(sessionID, a), true
	}
	if pending, ok := snapshot.unobserved[agentID]; ok {
		return subAgentDetailFromWorker(sessionID, pending), true
	}
	return subAgentDetailView{}, false
}

func subAgentDetailFromWorker(sessionID string, a desktopipc.Subagent) subAgentDetailView {
	return subAgentDetailView{
		SessionID: sessionID, Name: a.Name, AgentID: a.AgentID, Status: a.Status, Background: a.Background,
		StartedAt: a.StartedAt, EndedAt: a.EndedAt, ElapsedMS: a.ElapsedMS,
		Output: a.Output, OutputTruncated: a.OutputTruncated, Result: a.Result,
		ResultTruncated: a.ResultTruncated, StopHint: a.StopHint, ExitError: a.ExitError,
	}
}
