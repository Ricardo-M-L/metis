package agent

import (
	"context"
	"errors"

	"github.com/Ricardo-M-L/metis/internal/checkpoint"
)

// CheckpointTurnDiff is a read-only patch reconstructed from two adjacent
// shadow-git snapshots (or the newest snapshot and the live working tree).
type CheckpointTurnDiff struct {
	Turn  int
	Label string
	Patch string
}

// CheckpointTurnDiffs returns newest-first per-edit-turn patches without
// restoring files or changing the checkpoint stack. Empty/missing snapshots
// are skipped so callers can fall back to structured Edit/Write history.
func (l *Loop) CheckpointTurnDiffs() []CheckpointTurnDiff {
	return l.CheckpointTurnDiffsContext(context.Background(), 0)
}

// CheckpointTurnDiffsContext is the cancellable, budgeted collector used by
// the interactive /diff command. The budget is shared across all snapshots;
// partial results are returned when it is exhausted.
func (l *Loop) CheckpointTurnDiffsContext(ctx context.Context, maxBytes int) []CheckpointTurnDiff {
	if l == nil || l.Checkpointer == nil {
		return nil
	}
	l.ckptMu.Lock()
	stack := append([]ckptEntry(nil), l.ckptStack...)
	manager := l.Checkpointer
	l.ckptMu.Unlock()

	out := make([]CheckpointTurnDiff, 0, len(stack))
	remaining := maxBytes
	for index := len(stack) - 1; index >= 0; index-- {
		if ctx.Err() != nil || (maxBytes > 0 && remaining <= 0) {
			break
		}
		entry := stack[index]
		toHash := ""
		if index+1 < len(stack) {
			toHash = stack[index+1].hash
		}
		patch, err := manager.DiffContext(ctx, entry.hash, toHash, remaining)
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			break
		}
		if errors.Is(err, checkpoint.ErrDiffOutputLimit) {
			break
		}
		if err != nil || patch == "" {
			continue
		}
		if maxBytes > 0 {
			remaining -= len(patch)
		}
		out = append(out, CheckpointTurnDiff{
			Turn:  entry.restoreToTurns + 1,
			Label: entry.label,
			Patch: patch,
		})
	}
	return out
}
