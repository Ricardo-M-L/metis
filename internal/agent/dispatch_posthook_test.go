package agent

// dispatch_posthook_test.go — locks the PostToolUseContext feedback
// channel added 2026-06-11 (claude-code's PostToolUse hook
// additionalContext): a context-capable handler's return value lands
// in the model-facing tool_result as a <system-reminder>; plain
// observers keep their no-return contract.

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/tools"
	pubhook "github.com/Ricardo-M-L/metis/pkg/hook"
)

type postHookErrorTool struct{}

func (postHookErrorTool) Name() string                { return "PostHookError" }
func (postHookErrorTool) Description() string         { return "returns a Go error" }
func (postHookErrorTool) InputSchema() map[string]any { return map[string]any{"type": "object"} }
func (postHookErrorTool) Concurrency(map[string]any) tools.Concurrency {
	return tools.ConcurrencySafe
}
func (postHookErrorTool) CanUse(context.Context, map[string]any) (tools.Permission, string) {
	return tools.PermissionAllow, ""
}
func (postHookErrorTool) Execute(context.Context, map[string]any) (*tools.Result, error) {
	return nil, errors.New("boom")
}
func (postHookErrorTool) IsEnabled() bool { return true }

type postHookNilResultTool struct{ postHookErrorTool }

func (postHookNilResultTool) Name() string { return "PostHookNilResult" }
func (postHookNilResultTool) Execute(context.Context, map[string]any) (*tools.Result, error) {
	return nil, nil
}

type postHookResultErrorTool struct{ postHookErrorTool }

func (postHookResultErrorTool) Name() string { return "PostHookResultError" }
func (postHookResultErrorTool) Execute(context.Context, map[string]any) (*tools.Result, error) {
	return &tools.Result{Output: "reported failure", IsError: true}, nil
}

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

func TestDispatch_PostToolUseContextPreservedOnExecuteError(t *testing.T) {
	reg := tools.NewRegistry()
	reg.Register(postHookErrorTool{})

	hooks := NewHookRegistry()
	hooks.Register(pubhook.PostToolUseContextHandler(
		func(ctx context.Context, _ pubhook.Context, in *pubhook.PostToolUse) *pubhook.ModifiedPostToolUse {
			if got := pubhook.PostToolUseIDFromContext(ctx); got != "tu_error" {
				t.Errorf("ToolUseID = %q, want tu_error", got)
			}
			if !in.IsError || !strings.Contains(in.Output, "boom") {
				t.Errorf("error hook input = %+v", in)
			}
			return &pubhook.ModifiedPostToolUse{AdditionalContext: "RECOVERY_SENTINEL"}
		}))

	loop := &Loop{Registry: reg, Hooks: hooks}
	out := make(chan Event, 16)
	results, err := loop.executeBatch(context.Background(), []llm.ContentBlock{{
		Type: "tool_use", ToolUseID: "tu_error", ToolName: "PostHookError",
	}}, out, HookContext{})
	if err != nil {
		t.Fatalf("executeBatch: %v", err)
	}
	if len(results) != 1 || !results[0].IsError {
		t.Fatalf("error result = %+v", results)
	}
	body := results[0].ToolResult
	if !strings.Contains(body, "boom") || strings.Count(body, "RECOVERY_SENTINEL") != 1 || strings.Count(body, "system-reminder") != 2 {
		t.Fatalf("feedback was not preserved exactly once on error: %q", body)
	}
	close(out)
	for event := range out {
		if event.Kind == EventToolResult && event.ToolResult != nil && strings.Contains(event.ToolResult.Output, "RECOVERY_SENTINEL") {
			t.Fatalf("model-only hook feedback leaked into display event: %q", event.ToolResult.Output)
		}
	}
}

func TestDispatch_PostToolUseSeesNilResultAsFailure(t *testing.T) {
	reg := tools.NewRegistry()
	reg.Register(postHookNilResultTool{})

	hooks := NewHookRegistry()
	hooks.Register(pubhook.PostToolUseContextHandler(
		func(_ context.Context, _ pubhook.Context, in *pubhook.PostToolUse) *pubhook.ModifiedPostToolUse {
			if !in.IsError || !strings.Contains(in.Output, "no result") {
				t.Errorf("nil-result hook input = %+v, want normalized failure", in)
			}
			return nil
		}))

	loop := &Loop{Registry: reg, Hooks: hooks}
	results, err := loop.executeBatch(context.Background(), []llm.ContentBlock{{
		Type: "tool_use", ToolUseID: "tu_nil", ToolName: "PostHookNilResult",
	}}, make(chan Event, 16), HookContext{})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || !results[0].IsError || !strings.Contains(results[0].ToolResult, "no result") {
		t.Fatalf("nil result = %+v, want normalized failure", results)
	}
}

func TestDispatch_PostToolUseSeesResultError(t *testing.T) {
	reg := tools.NewRegistry()
	reg.Register(postHookResultErrorTool{})

	hooks := NewHookRegistry()
	hooks.Register(pubhook.PostToolUseContextHandler(
		func(_ context.Context, _ pubhook.Context, in *pubhook.PostToolUse) *pubhook.ModifiedPostToolUse {
			if !in.IsError || in.Output != "reported failure" {
				t.Errorf("result-error hook input = %+v", in)
			}
			return nil
		}))

	loop := &Loop{Registry: reg, Hooks: hooks}
	results, err := loop.executeBatch(context.Background(), []llm.ContentBlock{{
		Type: "tool_use", ToolUseID: "tu_result_error", ToolName: "PostHookResultError",
	}}, make(chan Event, 16), HookContext{})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || !results[0].IsError {
		t.Fatalf("result error = %+v", results)
	}
}
