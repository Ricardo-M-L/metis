package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/internal/checkpoint"
	"github.com/Ricardo-M-L/metis/internal/llm"
)

func TestCheckpointUnchangedBaselineProtectsFollowingTurn(t *testing.T) {
	cwd := t.TempDir()
	path := filepath.Join(cwd, "code.txt")
	if err := os.WriteFile(path, []byte("before"), 0o600); err != nil {
		t.Fatal(err)
	}
	l := &Loop{Checkpointer: checkpoint.NewManager("unchanged-baseline", cwd, t.TempDir()), ckptSnappedAt: -1}
	l.Messages = []llm.Message{msg(llm.RoleUser, "read-only command")}
	l.snapPreEdit("Bash", nil)
	l.recordCheckpointMutation("Bash", nil, nil, nil)
	l.Messages = append(l.Messages, msg(llm.RoleAssistant, "read"), msg(llm.RoleUser, "edit"))
	l.snapPreEdit("Edit", map[string]any{"path": path})
	if err := os.WriteFile(path, []byte("after"), 0o600); err != nil {
		t.Fatal(err)
	}
	l.recordCheckpointMutation("Edit", map[string]any{"path": path}, nil, nil)
	result, err := l.RewindToTurn(2, RewindCode)
	if err != nil || !result.CodeRestored {
		t.Fatalf("rewind: %+v %v", result, err)
	}
	if body, err := os.ReadFile(path); err != nil || string(body) != "before" {
		t.Fatalf("rewind claimed success without restoring the unchanged baseline: %q %v", body, err)
	}
}

func TestCheckpointStageReportsEachIncompleteTurnOnce(t *testing.T) {
	l := &Loop{Checkpointer: checkpoint.NewManager("warnings", t.TempDir(), t.TempDir()), ckptSnappedAt: -1}
	l.Messages = []llm.Message{msg(llm.RoleUser, "one")}
	out := make(chan Event, 16)
	l.runCheckpointStage(context.Background(), out, "before tool", "Bash", func(context.Context) error { return context.DeadlineExceeded })
	for range 3 {
		l.runCheckpointStage(context.Background(), out, "after tool", "Bash", func(context.Context) error { return ErrCheckpointIncomplete })
	}
	if len(out) != 1 {
		t.Fatalf("same protection gap produced %d warnings; want one", len(out))
	}
	if _, err := l.RewindToTurn(1, RewindCode); !errors.Is(err, ErrCheckpointIncomplete) {
		t.Fatalf("warning suppression removed incomplete barrier: %v", err)
	}
	l.Messages = append(l.Messages, msg(llm.RoleAssistant, "one"), msg(llm.RoleUser, "two"))
	l.runCheckpointStage(context.Background(), out, "before tool", "Bash", func(context.Context) error { return context.DeadlineExceeded })
	if len(out) != 2 {
		t.Fatalf("new turn did not report its own protection gap: %d", len(out))
	}
}

func TestCheckpointMutationCannotUsePreviousTurnBaseline(t *testing.T) {
	cwd := t.TempDir()
	path := filepath.Join(cwd, "code.txt")
	if err := os.WriteFile(path, []byte("before"), 0o600); err != nil {
		t.Fatal(err)
	}
	l := &Loop{Checkpointer: checkpoint.NewManager("old-turn", cwd, t.TempDir()), ckptSnappedAt: -1}
	l.Messages = []llm.Message{msg(llm.RoleUser, "first")}
	l.snapPreEdit("Bash", nil)
	l.Messages = append(l.Messages, msg(llm.RoleAssistant, "first"), msg(llm.RoleUser, "second"))
	if err := os.WriteFile(path, []byte("after"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := l.recordCheckpointMutationContext(context.Background(), "Edit", map[string]any{"path": path}, nil, nil)
	if !errors.Is(err, ErrCheckpointIncomplete) || len(l.ckptStack[0].managedPaths) != 0 {
		t.Fatalf("attributed current mutation to an old turn: err=%v entries=%+v", err, l.ckptStack)
	}
}

func TestCheckpointNewBaselineDoesNotErasePreviousGap(t *testing.T) {
	cwd := t.TempDir()
	path := filepath.Join(cwd, "code.txt")
	if err := os.WriteFile(path, []byte("before"), 0o600); err != nil {
		t.Fatal(err)
	}
	l := &Loop{Checkpointer: checkpoint.NewManager("gap-baseline", cwd, t.TempDir()), ckptSnappedAt: -1}
	l.Messages = []llm.Message{msg(llm.RoleUser, "first")}
	l.snapPreEdit("Bash", nil)
	l.markCheckpointIncomplete()
	l.Messages = append(l.Messages, msg(llm.RoleAssistant, "first"), msg(llm.RoleUser, "second"))
	l.snapPreEdit("Edit", map[string]any{"path": path})
	if err := os.WriteFile(path, []byte("after"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := l.recordCheckpointMutationContext(context.Background(), "Edit", map[string]any{"path": path}, nil, nil); err != nil {
		t.Fatalf("new complete baseline unavailable: %v", err)
	}
	if _, err := l.RewindToTurn(2, RewindCode); err != nil {
		t.Fatalf("new complete baseline could not restore: %v", err)
	}
	if body, err := os.ReadFile(path); err != nil || string(body) != "before" {
		t.Fatalf("new baseline did not restore: %q %v", body, err)
	}
	if _, err := l.RewindToTurn(1, RewindCode); !errors.Is(err, ErrCheckpointIncomplete) {
		t.Fatalf("new complete baseline erased previous protection gap: %v", err)
	}
}

func TestCheckpointLargerWorkspaceBudgetStillHonorsParentDeadline(t *testing.T) {
	l := &Loop{Checkpointer: checkpoint.NewManager("parent-budget", t.TempDir(), t.TempDir()), ckptSnappedAt: -1}
	l.Messages = []llm.Message{msg(llm.RoleUser, "command")}
	if got := l.checkpointBudget("before tool"); got != checkpointInitialBudget {
		t.Fatalf("cold baseline budget: %v", got)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	started := time.Now()
	l.runCheckpointStage(ctx, make(chan Event, 8), "before tool", "Bash", func(stage context.Context) error {
		<-stage.Done()
		return context.Cause(stage)
	})
	if time.Since(started) > time.Second || len(l.ckptStack) != 0 {
		t.Fatalf("parent cancellation ignored or pre-execution cancellation invented a code gap: %v %+v", time.Since(started), l.ckptStack)
	}
	if err := l.snapPreEditContext(context.Background(), "Bash", nil); err != nil {
		t.Fatal(err)
	}
	if got := l.checkpointBudget("before tool"); got != checkpointStageBudget {
		t.Fatalf("warm baseline budget: %v", got)
	}
}

func TestCheckpointUncapturedMutationBlocksCompleteRewind(t *testing.T) {
	for _, partial := range []bool{false, true} {
		t.Run(map[bool]string{false: "all", true: "partial"}[partial], func(t *testing.T) {
			cwd := t.TempDir()
			path := filepath.Join(cwd, "changed.txt")
			other := filepath.Join(cwd, "other.txt")
			for _, file := range []string{path, other} {
				if err := os.WriteFile(file, []byte("before"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			l := &Loop{Checkpointer: checkpoint.NewManager("uncaptured", cwd, t.TempDir()), ckptSnappedAt: -1}
			l.Messages = []llm.Message{msg(llm.RoleUser, "change file type")}
			if err := l.snapPreEditContext(context.Background(), "Bash", nil); err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(other, path); err != nil {
				t.Skipf("symlink unavailable: %v", err)
			}
			if partial {
				if err := os.WriteFile(other, []byte("after"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			l.runCheckpointStage(context.Background(), make(chan Event, 8), "after tool", "Bash", func(ctx context.Context) error {
				return l.recordCheckpointMutationContext(ctx, "Bash", nil, nil, nil)
			})
			result, err := l.RewindToTurn(1, RewindCode)
			if !errors.Is(err, ErrCheckpointIncomplete) || result.CodeRestored {
				t.Fatalf("uncaptured mutation reported complete restore: %+v %v", result, err)
			}
		})
	}
}
