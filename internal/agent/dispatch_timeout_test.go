package agent

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

// timeoutTool declares a cooperative per-call deadline and respects ctx:
// it blocks until ctx.Done() fires, then returns a partial result. This
// mirrors the reference signal-forwarding tools (web_fetch/web_search).
type timeoutTool struct {
	timeoutMs int
}

func (t *timeoutTool) Name() string                                 { return "TimeoutTool" }
func (t *timeoutTool) Description() string                          { return "test" }
func (t *timeoutTool) InputSchema() map[string]any                  { return map[string]any{"type": "object"} }
func (t *timeoutTool) Concurrency(map[string]any) tools.Concurrency { return tools.ConcurrencySafe }
func (t *timeoutTool) CanUse(context.Context, map[string]any) (tools.Permission, string) {
	return tools.PermissionAllow, ""
}
func (t *timeoutTool) TimeoutMs() int  { return t.timeoutMs }
func (t *timeoutTool) IsEnabled() bool { return true }
func (t *timeoutTool) Execute(ctx context.Context, _ map[string]any) (*tools.Result, error) {
	select {
	case <-ctx.Done():
		return &tools.Result{Output: "partial"}, nil
	case <-time.After(10 * time.Second):
		return &tools.Result{Output: "finished"}, nil
	}
}

// noTimeoutTool declares no budget: the dispatcher must leave it untouched.
type noTimeoutTool struct{}

func (*noTimeoutTool) Name() string                { return "NoTimeoutTool" }
func (*noTimeoutTool) Description() string         { return "test" }
func (*noTimeoutTool) InputSchema() map[string]any { return map[string]any{"type": "object"} }
func (*noTimeoutTool) Concurrency(map[string]any) tools.Concurrency {
	return tools.ConcurrencySafe
}
func (*noTimeoutTool) CanUse(context.Context, map[string]any) (tools.Permission, string) {
	return tools.PermissionAllow, ""
}
func (*noTimeoutTool) Execute(context.Context, map[string]any) (*tools.Result, error) {
	return &tools.Result{Output: "instant"}, nil
}
func (*noTimeoutTool) IsEnabled() bool { return true }

func TestDispatch_TimeoutPolicyProducesToolTimeout(t *testing.T) {
	reg := tools.NewRegistry()
	reg.Register(&timeoutTool{timeoutMs: 30})

	loop := &Loop{Registry: reg}
	uses := []llm.ContentBlock{{Type: "tool_use", ToolUseID: "1", ToolName: "TimeoutTool"}}
	out := make(chan Event, 32)

	results, err := loop.executeBatch(context.Background(), uses, out, HookContext{})
	if err != nil {
		t.Fatalf("executeBatch err: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	r := results[0]
	if !r.IsError {
		t.Fatalf("expected an error result, got success %q", r.ToolResult)
	}
	if !strings.Contains(r.ToolResult, "timed out after 30ms") {
		t.Fatalf("expected structured TOOL_TIMEOUT message, got %q", r.ToolResult)
	}
}

func TestDispatch_NoTimeoutBudgetPassesThrough(t *testing.T) {
	reg := tools.NewRegistry()
	reg.Register(&noTimeoutTool{})

	loop := &Loop{Registry: reg}
	uses := []llm.ContentBlock{{Type: "tool_use", ToolUseID: "1", ToolName: "NoTimeoutTool"}}
	out := make(chan Event, 32)

	results, err := loop.executeBatch(context.Background(), uses, out, HookContext{})
	if err != nil {
		t.Fatalf("executeBatch err: %v", err)
	}
	if len(results) != 1 || results[0].IsError || results[0].ToolResult != "instant" {
		t.Fatalf("tool without a budget must pass through untouched: %+v", results)
	}
}
