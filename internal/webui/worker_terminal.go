package webui

import (
	"log"
	"sort"
	"time"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/desktopipc"
)

// finishWorkerSnapshot runs only after Run has joined its worker. Graceful
// workers already published and persisted their terminals; forced exits need
// an explicit failure/cancellation record so neither a live drawer nor a
// reopened history can retain impossible queued/running children.
func (s *Server) finishWorkerSnapshot(sessionID string, failed, stopped bool) {
	s.workerTurnsMu.Lock()
	snapshot := s.workerSnapshots[sessionID]
	status := snapshot.status
	status.Agents = append([]desktopipc.Subagent(nil), status.Agents...)
	status.Jobs = append([]desktopipc.Job(nil), status.Jobs...)
	known := make(map[string]int, len(status.Agents))
	for i, item := range status.Agents {
		known[item.AgentID] = i
	}
	var pendingIDs []string
	for id := range snapshot.unobserved {
		pendingIDs = append(pendingIDs, id)
	}
	sort.Strings(pendingIDs)
	for _, id := range pendingIDs {
		if _, exists := known[id]; exists {
			continue
		}
		known[id] = len(status.Agents)
		status.Agents = append(status.Agents, snapshot.unobserved[id])
	}
	s.workerTurnsMu.Unlock()

	// A worker's compact status frame is bounded. Recover unsampled identities
	// from this parent's headers too, without loading another session's data.
	if s.store != nil {
		ids, _ := agent.ListSubAgentTranscripts(s.store.Dir)
		for _, id := range ids {
			if _, exists := known[id]; exists {
				continue
			}
			header, err := agent.LoadSubAgentHeader(s.store.Dir, id)
			if err != nil || header.SubAgentOf != sessionID {
				continue
			}
			child, err := agent.LoadSubAgentSnapshot(s.store.Dir, id)
			if err != nil || child.Terminal != nil {
				continue
			}
			known[id] = len(status.Agents)
			status.Agents = append(status.Agents, desktopipc.Subagent{AgentID: id, Name: header.TeammateName, Status: "unknown", StartedAt: header.CreatedAt})
		}
	}
	end := time.Now()
	for i, item := range status.Agents {
		if !subAgentStatusActive(item.Status) && item.Status != "unknown" {
			continue
		}
		item.Status, item.StopHint = "killed", "parent worker exited"
		if failed && !stopped {
			item.Status, item.StopHint = "failed", "parent worker failed"
		}
		if stopped {
			item.StopHint = "parent turn stopped"
		}
		item.EndedAt = end
		if !item.StartedAt.IsZero() {
			item.ElapsedMS = max(0, end.Sub(item.StartedAt).Milliseconds())
		}
		if s.store != nil && s.subAgentBelongsToSession(sessionID, item.AgentID) {
			_, err := agent.RecoverSubAgentTerminal(s.store.Dir, sessionID, item.AgentID, agent.SubAgentTerminal{
				Status: item.Status, Background: item.Background, EndedAt: item.EndedAt,
				Output: item.Output, Result: item.Result, StopHint: item.StopHint, ExitError: item.ExitError,
			})
			if err != nil {
				log.Printf("recover stopped sub-agent %s: %v", item.AgentID, err)
			}
			if durable, ok := s.subAgentDetailFromTranscript(sessionID, item.AgentID); ok && durable.Status != "unknown" {
				item.Status, item.EndedAt, item.ElapsedMS = durable.Status, durable.EndedAt, durable.ElapsedMS
				item.Output, item.OutputTruncated, item.Result, item.ResultTruncated = durable.Output, durable.OutputTruncated, durable.Result, durable.ResultTruncated
				item.StopHint, item.ExitError, item.Background = durable.StopHint, durable.ExitError, durable.Background
			}
		}
		status.Agents[i] = item
	}
	for i := range status.Jobs {
		if status.Jobs[i].Status == "running" {
			status.Jobs[i].Status = "killed"
		}
	}
	status.SubAgents, status.NamedAgents, status.BackgroundTasks = 0, 0, 0
	s.workerTurnsMu.Lock()
	clear(s.workerSnapshots[sessionID].unobserved)
	s.workerTurnsMu.Unlock()
	s.setWorkerSnapshot(sessionID, status)
}
