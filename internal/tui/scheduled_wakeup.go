package tui

import (
	"errors"
	"fmt"
	"path/filepath"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/notify"
	"github.com/Ricardo-M-L/metis/internal/runtime"
	"github.com/Ricardo-M-L/metis/internal/tools/builtin"
	"os"
)

func (m *Model) wakeupContinuationIdentity(now time.Time) (agent.ContinuationIdentity, bool) {
	if m.session == nil || m.sessionID == "" {
		return agent.ContinuationIdentity{}, false
	}
	h, _, err := m.session.LoadHeader(m.sessionID)
	if err != nil || h == nil {
		return agent.ContinuationIdentity{}, false
	}
	cwd, cwdErr := os.Getwd()
	expected, expectedErr := filepath.EvalSymlinks(h.WorkDir)
	actual, actualErr := filepath.EvalSymlinks(cwd)
	if cwdErr != nil || expectedErr != nil || actualErr != nil || expected != actual {
		if m.wakeupWarnings == nil {
			m.wakeupWarnings = map[string]bool{}
		}
		key := "workspace-mismatch:" + m.sessionID
		if !m.wakeupWarnings[key] {
			m.wakeupWarnings[key] = true
			m.messages = append(m.messages, Message{Role: "info", Content: "scheduled wakeup blocked: original workspace differs from current runtime; restore original workspace before continuing", Timestamp: now})
		}
		return agent.ContinuationIdentity{}, false
	}
	return agent.ContinuationIdentity{SessionID: m.sessionID, WorkDir: expected, Provider: m.providerName, Model: m.model}, true
}

func (m *Model) bindWakeupContinuation() {
	if m.loop == nil || m.loop.Registry == nil || m.cronSvc == nil {
		return
	}
	if _, visible := m.loop.Registry.Get("ScheduleWakeup"); !visible {
		return
	}
	cwd, cwdErr := os.Getwd()
	identity := agent.ContinuationIdentity{SessionID: m.sessionID, WorkDir: cwd, Provider: m.providerName, Model: m.model}
	// A restored conversation does not itself change the process tool cwd.
	// Refuse creation rather than reporting an unconsumable cross-workspace job.
	valid := cwdErr == nil && m.session != nil
	if valid {
		header, _, err := m.session.LoadHeader(m.sessionID)
		if err != nil || header == nil || header.WorkDir == "" {
			valid = false
		} else {
			expected, expectedErr := filepath.EvalSymlinks(header.WorkDir)
			actual, actualErr := filepath.EvalSymlinks(cwd)
			valid = expectedErr == nil && actualErr == nil && expected == actual
		}
	}
	if !valid {
		identity = agent.ContinuationIdentity{}
	}
	m.loop.Registry.Replace(builtin.NewScheduleWakeup(m.gate, m.cronSvc).WithContinuation(identity))
}

func (m *Model) consumeScheduledWakeup(now time.Time) tea.Cmd {
	if m.session == nil || m.cronSvc == nil || m.sessionID == "" {
		return nil
	}
	h, _, err := m.session.LoadHeader(m.sessionID)
	if err != nil || h == nil {
		return nil
	}
	identity := agent.ContinuationIdentity{SessionID: m.sessionID, WorkDir: h.WorkDir, Provider: m.providerName, Model: m.model}
	cwd, cwdErr := os.Getwd()
	expected, expectedErr := filepath.EvalSymlinks(h.WorkDir)
	actual, actualErr := filepath.EvalSymlinks(cwd)
	if cwdErr != nil || expectedErr != nil || actualErr != nil || expected != actual {
		if m.wakeupWarnings == nil {
			m.wakeupWarnings = map[string]bool{}
		}
		key := "workspace-mismatch:" + m.sessionID
		if !m.wakeupWarnings[key] {
			m.wakeupWarnings[key] = true
			m.messages = append(m.messages, Message{Role: "info", Content: "scheduled wakeup blocked: original workspace differs from current runtime; restore original workspace before continuing", Timestamp: now})
		}
		return nil
	}
	pending, err := m.cronSvc.PendingContinuations(identity, now)
	if err != nil {
		return nil
	}
	for _, j := range pending {
		if j.NextRun.After(now) {
			continue
		}
		if m.wakeupWarnings == nil {
			m.wakeupWarnings = map[string]bool{}
		}
		if !m.wakeupWarnings[j.ID] {
			m.wakeupWarnings[j.ID] = true
			m.messages = append(m.messages, Message{Role: "info", Content: "⏰ scheduled wakeup overdue; queued for original session (while closed, waits for restoration)", Timestamp: now})
		}
	}
	// Leave work durable while busy. In particular never put a continuation into
	// the unscoped composer queue, which could be drained after a session switch.
	if m.turnActive || m.wakeupRun != nil {
		return nil
	}
	run, err := m.cronSvc.AcquireDueContinuation(identity, now)
	if err != nil || run == nil {
		return nil
	}
	m.loop.AppendContext(run.Job.Prompt)
	runtime.RecordContextInput(m.sessionID, "scheduled-wakeup", run.Job.Prompt)
	m.messages = append(m.messages, Message{Role: "info", Content: run.Job.Prompt, Timestamp: now})
	if err := m.persistTail(); err != nil {
		_ = run.Finish("failed", "scheduled wakeup input persistence failed", "", err)
		m.warnSessionSave(err)
		return nil
	}
	if err := run.Start(); err != nil {
		_ = run.Finish("failed", "scheduled wakeup start failed", "", err)
		return nil
	}
	m.wakeupRun = run
	return m.startAgentTurn(m.ctx)
}
func (m *Model) finishScheduledWakeup(runErr, persistErr error) {
	if m.wakeupRun == nil {
		return
	}
	run := m.wakeupRun
	m.wakeupRun = nil
	err := errors.Join(runErr, persistErr)
	status := "succeeded"
	if err != nil || !m.backgroundResumeAllowed {
		status = "failed"
		if err == nil {
			err = fmt.Errorf("scheduled turn did not reach end_turn")
		}
	}
	if m.turnCancelledByUser {
		status = "cancelled"
	}
	summary := "scheduled wakeup " + status
	output := ""
	if m.loop != nil && m.sessionID == run.Job.OriginSessionID {
		for _, message := range m.loop.History() {
			if message.Role == "assistant" {
				output = ""
				for _, block := range message.Content {
					if block.Type == "text" {
						output += block.Text
					}
				}
			}
		}
	}
	if receiptErr := run.Finish(status, summary, output, err); receiptErr != nil {
		summary += fmt.Sprintf("; receipt save failed (job=%s session=%s): %v", run.Job.ID, run.Job.OriginSessionID, receiptErr)
	}
	if m.sessionID == run.Job.OriginSessionID {
		m.messages = append(m.messages, Message{Role: "info", Content: summary, Timestamp: time.Now()})
	}
	notify.SendNotification("metis", summary+" ["+run.Job.OriginSessionID+"]")
}
