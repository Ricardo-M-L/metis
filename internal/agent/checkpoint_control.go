package agent

import (
	"context"
	"errors"

	"github.com/Ricardo-M-L/metis/internal/checkpoint"
)

func (l *Loop) CheckpointManager() *checkpoint.Manager {
	if l == nil {
		return nil
	}
	l.ckptMu.Lock()
	defer l.ckptMu.Unlock()
	return l.Checkpointer
}

func (l *Loop) CheckpointCommand(ctx context.Context, args string) (checkpoint.ControlStatus, error) {
	return l.CheckpointCommandFor(ctx, args, l.CheckpointManager())
}

func (l *Loop) CheckpointCommandFor(ctx context.Context, args string, manager *checkpoint.Manager) (checkpoint.ControlStatus, error) {
	if l == nil {
		return checkpoint.ControlStatus{}, errors.New("checkpoint: 当前没有活动会话")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	action, _, parseErr := checkpoint.ParseControlArgs(args)
	if parseErr != nil {
		return checkpoint.ControlStatus{}, parseErr
	}
	if manager == nil {
		return checkpoint.ControlStatus{}, errors.New("checkpoint: 当前会话未绑定工作区")
	}
	state, err := manager.Command(ctx, args)
	if err == nil && action != "status" {
		l.ckptMu.Lock()
		if l.Checkpointer == manager {
			l.ckptSnappedAt = -1
		}
		l.ckptMu.Unlock()
	}
	return state, err
}

func checkpointScopeMatches(stack []ckptEntry, turn int, scope string) bool {
	for _, entry := range stack {
		if entry.restoreToTurns >= turn-1 && entry.scope != scope {
			return false
		}
	}
	return true
}
