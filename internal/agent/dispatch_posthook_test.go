package agent

// dispatch_posthook_test.go — locks the PostToolUseContext feedback
// channel added 2026-06-11 (claude-code's PostToolUse hook
// additionalContext): a context-capable handler's return value lands
// in the model-facing tool_result as a <system-reminder>; plain
// observers keep their no-return contract.

import (
	"context"
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/tools"
	pubhook "github.com/Ricardo-M-L/metis/pkg/hook"
)

func TestDispatch_PostToolUseContextInjectsReminder(t *testing.T) {
	reg := tools.NewRegistry()
	reg.Register(textTool{}) // returns Output "ok"

	hooks := NewHookRegistry()
	hooks.Register(pubhook.PostToolUseContextHandler(
		func(_ context.Context, _ pubhook.Context, in *pubhook.PostToolUse) *pubhook.ModifiedPostToolUse {
			if in.Tool != "TextStub" {
				return nil
			}
			return &pubhook.ModifiedPostToolUse{AdditionalContext: "lint: 2 issues found in x.go"}
		}))

	loop := &Loop{Registry: reg, Hooks: hooks}
	uses := []llm.ContentBlock{
		{Type: "tool_use", ToolUseID: "tu_hookctx", ToolName: "TextStub"},
	}
	out := make(chan Event, 16)
	results, err := loop.executeBatch(context.Background(), uses, out, HookContext{})
	if err != nil {
		t.Fatalf("executeBatch: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("result count = %d, want 1", len(results))
	}
	body := results[0].ToolResult
	if !strings.HasPrefix(body, "ok") {
		t.Errorf("original output lost; got %q", body)
	}
	if !strings.Contains(body, "system-reminder") || !strings.Contains(body, "lint: 2 issues") {
		t.Errorf("hook context not injected as system-reminder; got %q", body)
	}
}

// A handler returning nil must leave the result untouched.
func TestDispatch_PostToolUseContextNilIsNoop(t *testing.T) {
	reg := tools.NewRegistry()
	reg.Register(textTool{})

	hooks := NewHookRegistry()
	hooks.Register(pubhook.PostToolUseContextHandler(
		func(context.Context, pubhook.Context, *pubhook.PostToolUse) *pubhook.ModifiedPostToolUse {
			return nil
		}))

	loop := &Loop{Registry: reg, Hooks: hooks}
	uses := []llm.ContentBlock{
		{Type: "tool_use", ToolUseID: "tu_noop", ToolName: "TextStub"},
	}
	out := make(chan Event, 16)
	results, _ := loop.executeBatch(context.Background(), uses, out, HookContext{})
	if results[0].ToolResult != "ok" {
		t.Errorf("nil handler must not modify result; got %q", results[0].ToolResult)
	}
}
