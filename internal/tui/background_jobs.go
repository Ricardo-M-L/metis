package tui

import (
	"context"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/Ricardo-M-L/metis/internal/jobs"
)

type backgroundJobReadyMsg struct{ pool *jobs.Registry }

// One host waiter observes readiness; Loop remains the only consumer of job
// envelopes. A completion never starts a second Run over an active one.
func (m *Model) waitForBackgroundJob() tea.Cmd {
	if m.loop == nil || m.loop.Jobs == nil || m.loop.JobNotify == nil {
		return nil
	}
	pool, ctx := m.loop.Jobs, m.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	return func() tea.Msg {
		select {
		case <-ctx.Done():
			return nil
		case <-pool.Wakeup():
			return backgroundJobReadyMsg{pool: pool}
		}
	}
}

// Called only on Bubble Tea's serialized update goroutine, both on readiness
// and after finalizing a turn. Pending editor text/images are left untouched;
// explicitly queued user prompts take priority over automatic continuation.
func (m *Model) resumeForBackgroundJob() tea.Cmd {
	if !m.backgroundResumeAllowed || m.turnActive || m.rewindSummaryPending || m.permissionModePending || m.queuePending || len(m.queuedPrompts) > 0 ||
		m.sessionBoundaryClosed || m.loop == nil || m.loop.Jobs == nil || m.loop.JobNotify == nil ||
		(m.ctx != nil && m.ctx.Err() != nil) || !m.loop.Jobs.HasPendingNotifications() {
		return nil
	}
	m.messages = append(m.messages, Message{
		Role: "info", Content: "[job pool] background completion received — continuing automatically", Timestamp: time.Now(),
	})
	return m.startAgentTurn(m.ctx)
}
