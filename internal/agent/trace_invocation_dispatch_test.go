package agent

import (
	"context"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

type traceInvocationAgentStub struct {
	tools.BaseTool
	seen chan string
}

func (traceInvocationAgentStub) Name() string                { return "Agent" }
func (traceInvocationAgentStub) Description() string         { return "test child" }
func (traceInvocationAgentStub) InputSchema() map[string]any { return map[string]any{"type": "object"} }
func (traceInvocationAgentStub) Concurrency(map[string]any) tools.Concurrency {
	return tools.ConcurrencySafe
}
func (traceInvocationAgentStub) CanUse(context.Context, map[string]any) (tools.Permission, string) {
	return tools.PermissionAllow, ""
}
func (s traceInvocationAgentStub) Execute(ctx context.Context, _ map[string]any) (*tools.Result, error) {
	s.seen <- TraceInvocationIDFromContext(ctx)
	return &tools.Result{Output: "ok"}, nil
}

func TestDispatchPropagatesUniqueTraceInvocationAndParent(t *testing.T) {
	registry := tools.NewRegistry()
	seen := make(chan string, 1)
	registry.Register(traceInvocationAgentStub{seen: seen})
	loop := NewLoop(nil, registry, permission.New(permission.ModeBypass), NewHookRegistry(), "", 1)
	out := make(chan Event, 4)
	parentCtx := WithTraceInvocationID(context.Background(), "parent-internal")

	results, err := loop.executeBatch(parentCtx, []llm.ContentBlock{{
		Type: "tool_use", ToolName: "Agent", ToolUseID: "provider-duplicate",
	}}, out, HookContext{})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].ToolResult != "ok" {
		t.Fatalf("results = %+v", results)
	}
	start := <-out
	result := <-out
	if start.Kind != EventToolStart || result.Kind != EventToolResult {
		t.Fatalf("events = start:%+v result:%+v", start, result)
	}
	if start.TraceInvocationID == "" || start.TraceInvocationID == "parent-internal" {
		t.Fatalf("child invocation id = %q", start.TraceInvocationID)
	}
	if result.TraceInvocationID != start.TraceInvocationID {
		t.Fatalf("result invocation %q != start %q", result.TraceInvocationID, start.TraceInvocationID)
	}
	if start.TraceCallID == "" || result.TraceCallID != start.TraceCallID {
		t.Fatalf("trace call pairing missing: start=%+v result=%+v", start, result)
	}
	if start.TraceParentInvocationID != "parent-internal" || result.TraceParentInvocationID != "parent-internal" {
		t.Fatalf("trace parent missing: start=%+v result=%+v", start, result)
	}
	if got := <-seen; got != start.TraceInvocationID {
		t.Fatalf("Execute context id = %q, want %q", got, start.TraceInvocationID)
	}
}
