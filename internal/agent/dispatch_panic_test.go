package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

// panicTool is a tools.Tool whose Execute always panics — models a buggy
// builtin, a faulting MCP wrapper, or a sub-agent loop that nil-derefs.
type panicTool struct{ name string }

func (p panicTool) Name() string                                 { return p.name }
func (p panicTool) IsEnabled() bool                              { return true }
func (p panicTool) Description() string                          { return "panics" }
func (p panicTool) InputSchema() map[string]any                  { return map[string]any{"type": "object"} }
func (p panicTool) Concurrency(map[string]any) tools.Concurrency { return tools.ConcurrencySafe }
func (p panicTool) CanUse(context.Context, map[string]any) (tools.Permission, string) {
	return tools.PermissionAllow, ""
}
func (p panicTool) Execute(context.Context, map[string]any) (*tools.Result, error) {
	panic("boom: simulated tool fault")
}

// TestDispatch_ToolPanicIsContained — a panicking tool must NOT crash the
// process (it runs in the safe-fanout goroutine, where an unrecovered
// panic would kill all of metis). The dispatcher recovers it into a
// well-formed error tool_result so the model can see and react. A sibling
// healthy tool in the same batch must still complete normally.
func TestDispatch_ToolPanicIsContained(t *testing.T) {
	reg := tools.NewRegistry()
	reg.Register(panicTool{name: "Boom"})
	reg.Register(&fakeTool{name: "Ok", conc: tools.ConcurrencySafe})

	loop := &Loop{Registry: reg}
	uses := []llm.ContentBlock{
		{Type: "tool_use", ToolUseID: "1", ToolName: "Boom"},
		{Type: "tool_use", ToolUseID: "2", ToolName: "Ok"},
	}
	out := make(chan Event, 32)

	// If the recover backstop is missing, this call crashes the whole
	// test binary instead of returning.
	results, err := loop.executeBatch(context.Background(), uses, out, HookContext{})
	if err != nil {
		t.Fatalf("executeBatch returned err (should surface panic as a tool_result, not a batch error): %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("expected 2 tool_results, got %d", len(results))
	}

	// The panicking tool's result is a well-formed error block.
	if !results[0].IsError {
		t.Errorf("panicking tool should yield IsError result; got %+v", results[0])
	}
	if !strings.Contains(results[0].ToolResult, "panic") {
		t.Errorf("panic result should mention the panic; got %q", results[0].ToolResult)
	}
	if results[0].ToolUseID != "1" {
		t.Errorf("result must keep the tool_use_id mapping; got %q", results[0].ToolUseID)
	}
	// The healthy sibling still produced a normal (non-error) result.
	if results[1].IsError {
		t.Errorf("healthy sibling tool should not be marked error; got %+v", results[1])
	}
}

// TestSafeToolExecute_RecoversPanic — unit-level: the helper converts a
// panic into an error and never re-panics.
func TestSafeToolExecute_RecoversPanic(t *testing.T) {
	res, err := safeToolExecute(context.Background(), panicTool{name: "X"}, nil)
	if err == nil {
		t.Fatal("expected an error from a panicking tool")
	}
	if res != nil {
		t.Errorf("expected nil result on panic; got %+v", res)
	}
	if !strings.Contains(err.Error(), "X") || !strings.Contains(err.Error(), "panic") {
		t.Errorf("error should name the tool and the panic; got %v", err)
	}
}
