package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/tools"
	pubtool "github.com/Ricardo-M-L/metis/pkg/tool"
)

type recordingOperationExecutor struct {
	calls     int
	toolName  string
	input     map[string]any
	execError error
}

func (g *recordingOperationExecutor) Execute(ctx context.Context, tool pubtool.Tool, input map[string]any, invoke func(context.Context) (*pubtool.Result, error)) (*pubtool.Result, error) {
	g.calls++
	g.toolName, g.input = tool.Name(), input
	result, err := invoke(ctx)
	g.execError = err
	return result, err
}

func TestOperationExecutorObservesRecoveredToolPanic(t *testing.T) {
	guard := &recordingOperationExecutor{}
	ctx := pubtool.WithOperationExecutor(context.Background(), guard)
	result, err := safeToolExecute(ctx, panicTool{name: "remote_write"}, map[string]any{"value": 1})
	if result != nil || err == nil || guard.execError == nil || !strings.Contains(guard.execError.Error(), "panic") || guard.calls != 1 {
		t.Fatalf("panic escaped the durable boundary: result=%+v err=%v guard=%+v", result, err, guard)
	}
}

func TestOperationExecutorReachesForkExecution(t *testing.T) {
	guard := &recordingOperationExecutor{}
	ctx := pubtool.WithOperationExecutor(context.Background(), guard)
	registry := tools.NewRegistry()
	registry.Register(&fakeTool{name: "ForkLeaf", conc: tools.ConcurrencySafe})
	result := executeForkTool(ctx, ForkedAgentParams{Registry: registry, CanUseTool: func(context.Context, string, map[string]any) (bool, string) { return true, "" }}, llm.ContentBlock{Type: "tool_use", ToolUseID: "leaf-id", ToolName: "ForkLeaf", ToolInput: map[string]any{}}, nil)
	if result.IsError || guard.calls != 1 || guard.toolName != "ForkLeaf" {
		t.Fatalf("fork bypassed its inherited operation boundary: result=%+v guard=%+v", result, guard)
	}
}
