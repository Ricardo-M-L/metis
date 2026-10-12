package webui

import (
	"net/http"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/session"
)

func applyDesktopResumeReminder(loop *agent.Loop) func() {
	const reminder = "继续被用户停止的同一轮任务。历史中已完成的工具调用及结果是已执行事实，不要重放。未确认完成的工具可能已产生外部副作用；先检查当前结果，再决定是否重试。"
	snapshot := loop.ProviderRuntimeState()
	sections := snapshot.SystemSections
	if len(sections) > 0 {
		sections = append(sections, llm.SystemSection{Name: "desktop_resume_turn", Body: reminder, Volatile: true})
	}
	loop.RebindProviderRuntime(snapshot.Provider, snapshot.Model, snapshot.MaxOutputTokens, snapshot.System+"\n\n"+reminder, sections)
	return func() {
		loop.RebindProviderRuntime(snapshot.Provider, snapshot.Model, snapshot.MaxOutputTokens, snapshot.System, snapshot.SystemSections)
	}
}

// Snapshot ownership is explicit even when unavailable, so a stopped session A
// can never show a Play action using session B's token after navigation.
func (s *Server) addDesktopResumeSnapshot(payload map[string]any, id string) {
	payload["resumeSessionId"] = id
	payload["resumeAvailable"] = false
	payload["resumeToken"] = ""
	if s == nil || s.store == nil || id == "" {
		return
	}
	s.cancelMu.Lock()
	running := s.cancelTurn != nil && s.runningSession == id
	s.cancelMu.Unlock()
	s.workerTurnsMu.Lock()
	running = running || s.workerTurns[id] != nil
	s.workerTurnsMu.Unlock()
	if running {
		return
	}
	hdr, _, err := s.store.LoadHeader(id)
	if err == nil && session.DesktopResumeAvailable(hdr) {
		payload["resumeAvailable"] = true
		payload["resumeToken"] = hdr.ResumeToken
	}
}

func (s *Server) writeDesktopResumeJSON(w http.ResponseWriter, status int, id string, payload map[string]any) {
	s.addDesktopResumeSnapshot(payload, id)
	writeJSON(w, status, payload)
}

// A stopped token describes one exact transcript checkpoint. Desktop undo,
// retry, clear and manual compaction start a different history lifetime.
func (s *Server) replaceDesktopHistory(id string, history []llm.Message, cursor *session.HistoryCursor) error {
	if err := s.store.InvalidateDesktopResume(id); err != nil {
		return err
	}
	if err := s.store.ReplaceHistoryAndMark(id, history, cursor); err != nil {
		return err
	}
	return s.store.Sync(id)
}
