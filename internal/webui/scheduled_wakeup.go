package webui

// Scheduled continuations use exactly the ordinary turn admission, workspace,
// worker and permission routes. They never construct an unattended provider.
import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/tools/builtin"
)

type scheduledWakeupKey struct{}

// Called only under runMu after activation; immutable binding lasts for the
// whole shared-Loop turn and never reintroduces a profile-filtered tool.
func (s *Server) bindWakeupContinuation(sessionID string) {
	if s.loop == nil || s.loop.Registry == nil || s.cronSvc == nil {
		return
	}
	if _, visible := s.loop.Registry.Get("ScheduleWakeup"); !visible {
		return
	}
	s.stateMu.RLock()
	identity := agent.ContinuationIdentity{SessionID: sessionID, WorkDir: s.activeWorkDir, Provider: s.activeProviderName, Model: s.activeModel}
	s.stateMu.RUnlock()
	s.loop.Registry.Replace(builtin.NewScheduleWakeup(s.loop.Gate, s.cronSvc).WithContinuation(identity))
}

func (s *Server) startScheduledWakeup(ctx context.Context, sessionID string) error {
	if run, ok := ctx.Value(scheduledWakeupKey{}).(*agent.ContinuationRun); ok {
		identity, available := s.wakeupIdentity(sessionID)
		j := run.Job
		if !available || identity.SessionID != j.OriginSessionID || identity.WorkDir != j.WorkDir || identity.Provider != j.Provider || identity.Model != j.Model {
			return fmt.Errorf("scheduled wakeup identity changed while queued; restore its original runtime")
		}
		return run.Start()
	}
	return nil
}
func (s *Server) rememberWakeupSession(id string) {
	if id == "" {
		return
	}
	s.wakeupMu.Lock()
	defer s.wakeupMu.Unlock()
	if s.wakeupSessions == nil {
		s.wakeupSessions = map[string]bool{}
	}
	s.wakeupSessions[id] = true
}
func (s *Server) wakeupIdentity(id string) (agent.ContinuationIdentity, bool) {
	if s.store == nil {
		return agent.ContinuationIdentity{}, false
	}
	h, _, err := s.store.LoadHeader(id)
	if err != nil || h == nil {
		return agent.ContinuationIdentity{}, false
	}
	return agent.ContinuationIdentity{SessionID: id, WorkDir: h.WorkDir, Provider: h.Provider, Model: h.Model}, true
}
func (s *Server) scheduledWakeupStatus(id string, now time.Time) []map[string]any {
	out := []map[string]any{}
	if s.cronSvc == nil {
		return out
	}
	identity, ok := s.wakeupIdentity(id)
	if !ok {
		return out
	}
	_, err := s.cronSvc.PendingContinuations(identity, now) // refresh/recover receipts
	if err != nil {
		s.hub.publish(id, agent.Event{Kind: agent.EventInfo, Info: "scheduled wakeup status unavailable: " + err.Error(), Source: "scheduled-wakeup"})
		return out
	}
	for _, j := range s.cronSvc.List() {
		if j.Kind != agent.CronKindContinuation || j.OriginSessionID != id {
			continue
		}
		status := agent.CronJobStatus(j, now)
		if len(status) >= 7 && status[:7] == "overdue" {
			status = "overdue"
		}
		out = append(out, map[string]any{"id": j.ID, "status": status, "dueAt": j.NextRun, "sessionId": id, "runId": j.ContinuationRunID, "executionScope": "live-session; while closed, waits for session restoration"})
	}
	return out
}
func (s *Server) watchScheduledWakeups(ctx context.Context) {
	if s.cronSvc == nil {
		return
	}
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			s.consumeScheduledWakeups(ctx, now)
		}
	}
}
func (s *Server) consumeScheduledWakeups(ctx context.Context, now time.Time) {
	s.wakeupMu.Lock()
	ids := []string{}
	for id := range s.wakeupSessions {
		if id != "" {
			ids = append(ids, id)
		}
	}
	s.wakeupMu.Unlock()
	for _, id := range ids {
		if ctx.Err() != nil {
			return
		}
		identity, ok := s.wakeupIdentity(id)
		if !ok {
			continue
		}
		for _, status := range s.scheduledWakeupStatus(id, now) {
			if status["status"] != "overdue" {
				continue
			}
			key := status["id"].(string)
			s.wakeupMu.Lock()
			seen := s.wakeupWarnings[key]
			if s.wakeupWarnings == nil {
				s.wakeupWarnings = map[string]bool{}
			}
			s.wakeupWarnings[key] = true
			s.wakeupMu.Unlock()
			if !seen {
				s.hub.publish(id, agent.Event{Kind: agent.EventInfo, Info: "⏰ scheduled wakeup overdue; queued for its original session", Source: "scheduled-wakeup"}, map[string]any{"scheduledWakeup": status})
			}
		}
		run, err := s.cronSvc.AcquireDueContinuation(identity, now)
		if err != nil {
			s.hub.publish(id, agent.Event{Kind: agent.EventInfo, Info: "scheduled wakeup claim failed: " + err.Error(), Source: "scheduled-wakeup"})
			continue
		}
		if run == nil {
			continue
		}
		s.executeScheduledWakeup(ctx, id, run)
	}
}

// wakeupResponse captures only the ordinary JSON result, not browser state.
type wakeupResponse struct {
	header http.Header
	status int
	bytes.Buffer
}

func (w *wakeupResponse) Header() http.Header {
	if w.header == nil {
		w.header = make(http.Header)
	}
	return w.header
}
func (w *wakeupResponse) WriteHeader(status int) { w.status = status }
func (s *Server) executeScheduledWakeup(ctx context.Context, id string, run *agent.ContinuationRun) {
	s.hub.publish(id, agent.Event{Kind: agent.EventInfo, Info: "⏰ scheduled wakeup queued", Source: "scheduled-wakeup"})
	raw, _ := json.Marshal(map[string]any{"sessionId": id, "input": run.Job.Prompt})
	req, _ := http.NewRequestWithContext(context.WithValue(ctx, scheduledWakeupKey{}, run), http.MethodPost, "/api/turn", bytes.NewReader(raw))
	response := &wakeupResponse{}
	s.handleTurn(response, req)
	var result struct {
		SessionID string `json:"sessionId"`
		Text      string `json:"text"`
		Error     string `json:"error"`
		Stopped   bool   `json:"stopped"`
	}
	decodeErr := json.Unmarshal(response.Bytes(), &result)
	status := "succeeded"
	var runErr error
	if response.status != http.StatusOK || decodeErr != nil || result.SessionID != id || result.Error != "" {
		status = "failed"
		runErr = fmt.Errorf("scheduled turn: HTTP %d %s", response.status, result.Error)
	}
	if result.Stopped || ctx.Err() != nil {
		status = "cancelled"
		runErr = context.Canceled
	}
	summary := "scheduled wakeup " + status
	if err := run.Finish(status, summary, result.Text, runErr); err != nil {
		status = "failed"
		summary = "scheduled wakeup receipt failed: " + err.Error()
	}
	s.hub.publish(id, agent.Event{Kind: agent.EventInfo, Info: summary, Source: "scheduled-wakeup"}, map[string]any{"scheduledWakeup": map[string]any{"id": run.Job.ID, "status": status, "sessionId": id}})
}
