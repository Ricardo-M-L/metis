package agent

import (
	"context"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/tasks"
	"github.com/Ricardo-M-L/metis/internal/tools"
	pubhook "github.com/Ricardo-M-L/metis/pkg/hook"
)

func TestRunToolHooksReceivePinnedSessionIdentity(t *testing.T) {
	previous := tasks.CurrentSessionID()
	tasks.SetCurrentSessionID("other-visible-session")
	t.Cleanup(func() { tasks.SetCurrentSessionID(previous) })
	hooks := NewHookRegistry()
	var before, after string
	hooks.Register(PreToolUseHandler(func(_ context.Context, tc HookContext, in *PreToolUseHook) *ModifiedPreToolUse {
		before = tc.SessionID
		if in.Context.SessionID != tc.SessionID {
			t.Error("pre-tool payload and hook identity differ")
		}
		return nil
	}))
	hooks.Register(pubhook.PostToolUseContextHandler(func(_ context.Context, tc HookContext, in *PostToolUseHook) *pubhook.ModifiedPostToolUse {
		after = tc.SessionID
		if in.Context.SessionID != tc.SessionID {
			t.Error("post-tool payload and hook identity differ")
		}
		return nil
	}))
	registry := tools.NewRegistry()
	registry.Register(haltyTool{})
	provider := &queuedStreamProvider{streams: []llm.StreamReader{toolUseStream("session-tool", "Bash", `{"command":"true"}`), textStream("done")}}
	loop := NewLoop(provider, registry, permission.New(permission.ModeAcceptEdits), hooks, "test", 4)
	loop.Model = "test-model"
	loop.AppendUser("run one tool")
	if err := loop.Run(tasks.WithSessionID(context.Background(), "owning-session"), make(chan Event, 64)); err != nil {
		t.Fatal(err)
	}
	if before != "owning-session" || after != "owning-session" {
		t.Fatalf("hooks did not receive the owning session: before=%q after=%q", before, after)
	}
}
