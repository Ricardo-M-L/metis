package tui

import (
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/checkpoint"
)

type checkpointControlResultMsg struct {
	loop      *agent.Loop
	sessionID string
	row       Message
	manager   *checkpoint.Manager
	state     *checkpoint.ControlStatus
}

type checkpointPollTick struct{}
type checkpointStatusMsg struct {
	manager   *checkpoint.Manager
	sessionID string
	state     checkpoint.ControlStatus
}

func (m *Model) checkpointStatusCommand() tea.Cmd {
	manager, id := m.loop.CheckpointManager(), m.sessionID
	if manager == nil {
		return nil
	}
	return func() tea.Msg {
		return checkpointStatusMsg{manager: manager, sessionID: id, state: manager.ControlStatus()}
	}
}

func (m *Model) checkpointControlCommand(args string) tea.Cmd {
	loop, id, ctx := m.loop, m.sessionID, m.ctx
	manager := loop.CheckpointManager()
	return func() tea.Msg {
		row := Message{Role: "info", Timestamp: time.Now()}
		state, err := loop.CheckpointCommandFor(ctx, args, manager)
		if err != nil {
			row.Role = "error"
			row.Content = err.Error()
		} else {
			row.Content = state.String()
		}
		var status *checkpoint.ControlStatus
		if err == nil {
			status = &state
		}
		return checkpointControlResultMsg{loop: loop, sessionID: id, manager: manager, state: status, row: row}
	}
}
