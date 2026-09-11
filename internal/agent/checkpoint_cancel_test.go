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
	"github.com/Ricardo-M-L/metis/internal/tools"
)

type checkpointCancelTool struct {
	tools.BaseTool
	run func()
}

func TestCheckpointStageBudgetFailureBlocksCodeRewindWithoutHash(t *testing.T) {
	l := &Loop{Checkpointer: checkpoint.NewManager("budget-pre", t.TempDir(), t.TempDir()), ckptSnappedAt: -1}
	l.Messages = []llm.Message{msg(llm.RoleUser, "command")}
	out := make(chan Event, 8)
	l.runCheckpointStage(context.Background(), out, "before tool", "Bash", func(ctx context.Context) error {
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) > checkpointStageBudget {
			t.Fatal("missing automatic checkpoint budget")
		}
		return context.DeadlineExceeded
	})
	if len(l.ckptStack) != 1 || l.ckptStack[0].hash != "" || !l.ckptStack[0].incomplete {
		t.Fatalf("missing unavailable baseline marker: %+v", l.ckptStack)
	}
	l.snapPreEdit("Bash", map[string]any{})
	if l.ckptStack[0].hash != "" {
		t.Fatal("post-mutation snapshot replaced unavailable pre-turn baseline")
	}
	if _, err := l.RewindToTurn(1, RewindCodeAndConversation); !errors.Is(err, ErrCheckpointIncomplete) {
		t.Fatalf("missing baseline restore err=%v", err)
	}
	points := l.RewindPoints()
	if len(points) != 1 || points[0].HasCodeCheckpoint {
		t.Fatalf("incomplete code checkpoint advertised as usable: %+v", points)
	}
	select {
	case event := <-out:
		if event.Kind != EventInfo {
			t.Fatalf("unexpected event: %+v", event)
		}
	default:
		t.Fatal("budget failure did not report limited rewind protection")
	}
}

func TestConversationRewindCannotEraseIncompleteCodeBarrier(t *testing.T) {
	l := &Loop{Checkpointer: checkpoint.NewManager("incomplete-history", t.TempDir(), t.TempDir()), ckptSnappedAt: -1}
	l.Messages = []llm.Message{msg(llm.RoleUser, "turn one")}
	l.snapPreEdit("Bash", map[string]any{})
	l.Messages = append(l.Messages, msg(llm.RoleAssistant, "one"), msg(llm.RoleUser, "turn two, no file edits"), msg(llm.RoleAssistant, "two"), msg(llm.RoleUser, "turn three, interrupted edit"))
	l.markCheckpointIncomplete()
	if _, err := l.RewindToTurn(3, RewindConversation); err != nil {
		t.Fatal(err)
	}
	for _, turn := range []int{1, 2} {
		if result, err := l.RewindToTurn(turn, RewindCodeAndConversation); !errors.Is(err, ErrCheckpointIncomplete) || result.CodeRestored {
			t.Fatalf("conversation-only erased barrier for turn %d: %+v %v", turn, result, err)
		}
	}
	if l.CountTurns() != 2 {
		t.Fatal("failed combined rewind changed history")
	}
}

func (checkpointCancelTool) Name() string                { return "Bash" }
func (checkpointCancelTool) Description() string         { return "checkpoint cancellation fixture" }
func (checkpointCancelTool) InputSchema() map[string]any { return map[string]any{"type": "object"} }
func (checkpointCancelTool) Concurrency(map[string]any) tools.Concurrency {
	return tools.ConcurrencyExclusive
}
func (checkpointCancelTool) CanUse(context.Context, map[string]any) (tools.Permission, string) {
	return tools.PermissionAllow, ""
}
func (checkpointCancelTool) InterruptBehavior() tools.InterruptBehavior { return tools.InterruptBlock }
func (t checkpointCancelTool) Execute(context.Context, map[string]any) (*tools.Result, error) {
	t.run()
	return &tools.Result{Output: "command completed"}, nil
}

func TestDispatchCancelledBeforeCheckpointDoesNotStartTool(t *testing.T) {
	shadow := t.TempDir()
	l := &Loop{Checkpointer: checkpoint.NewManager("cancel-pre", t.TempDir(), shadow), ckptSnappedAt: -1}
	l.Messages = []llm.Message{msg(llm.RoleUser, "command")}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	called := false
	tool := checkpointCancelTool{run: func() { called = true }}
	l.runExecute(ctx, tool, llm.ContentBlock{ToolName: "Bash", ToolUseID: "cancel-pre", ToolInput: map[string]any{}}, make(chan Event, 32), HookContext{}, 0, "", "", "")
	if called || len(l.ckptStack) != 0 {
		t.Fatalf("cancelled turn started work: tool=%v checkpoints=%d", called, len(l.ckptStack))
	}
	if _, err := os.Stat(filepath.Join(shadow, "cancel-pre")); !os.IsNotExist(err) {
		t.Fatalf("cancelled pre-checkpoint created shadow state: %v", err)
	}
}

func TestDispatchPostCheckpointUsesOriginalCancellation(t *testing.T) {
	cwd := t.TempDir()
	path := filepath.Join(cwd, "code.txt")
	if err := os.WriteFile(path, []byte("before"), 0o600); err != nil {
		t.Fatal(err)
	}
	l := &Loop{Checkpointer: checkpoint.NewManager("cancel-post", cwd, t.TempDir()), ckptSnappedAt: -1}
	l.Messages = []llm.Message{msg(llm.RoleUser, "command")}
	l.snapPreEdit("Bash", map[string]any{})
	if len(l.ckptStack) != 1 {
		t.Fatal("missing baseline checkpoint")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tool := checkpointCancelTool{run: func() {
		if err := os.WriteFile(path, []byte("after"), 0o600); err != nil {
			t.Fatal(err)
		}
		cancel()
	}}
	l.runExecute(ctx, tool, llm.ContentBlock{ToolName: "Bash", ToolUseID: "cancel-post", ToolInput: map[string]any{}}, make(chan Event, 32), HookContext{}, 0, "", "", "")
	if len(l.ckptStack[0].managedPaths) != 0 {
		t.Fatalf("cancelled post-checkpoint still attributed paths: %v", l.ckptStack[0].managedPaths)
	}
	before := len(l.History())
	if result, err := l.RewindToTurn(1, RewindCodeAndConversation); !errors.Is(err, ErrCheckpointUnavailable) || result.CodeRestored {
		t.Fatalf("incomplete attribution reported a code restore: result=%+v err=%v", result, err)
	}
	if len(l.History()) != before {
		t.Fatal("failed code rewind truncated the conversation")
	}
	if result, err := l.RewindToTurn(1, RewindConversation); err != nil || !result.ConversationRestored {
		t.Fatalf("conversation-only rewind failed: %+v %v", result, err)
	}
}
