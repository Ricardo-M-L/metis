package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

type visibilityReadTool struct {
	tools.BaseTool
	visible []bool
}

func (*visibilityReadTool) Name() string                { return "Read" }
func (*visibilityReadTool) Description() string         { return "test" }
func (*visibilityReadTool) InputSchema() map[string]any { return map[string]any{"type": "object"} }
func (*visibilityReadTool) Concurrency(map[string]any) tools.Concurrency {
	return tools.ConcurrencySafe
}
func (*visibilityReadTool) CanUse(context.Context, map[string]any) (tools.Permission, string) {
	return tools.PermissionAllow, "test"
}
func (*visibilityReadTool) Execute(context.Context, map[string]any) (*tools.Result, error) {
	return &tools.Result{}, nil
}
func (v *visibilityReadTool) ObserveResultProjection(_ context.Context, _ map[string]any, visible bool) {
	v.visible = append(v.visible, visible)
}

func TestReadProjectionObserverSeesActualCommittedBody(t *testing.T) {
	for _, large := range []bool{false, true} {
		t.Run(map[bool]string{false: "full", true: "spilled"}[large], func(t *testing.T) {
			read := &visibilityReadTool{}
			reg := tools.NewRegistry()
			reg.Register(read)
			loop := &Loop{Registry: reg, SpillDir: t.TempDir()}
			loop.Messages = []llm.Message{{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: "tool_use", ToolName: "Read", ToolUseID: "same-id", TraceCallID: "occurrence", ToolInput: map[string]any{"path": "/fixture"}}}}}
			body := "short source"
			if large {
				body = strings.Repeat("large-source ", 10000)
			}
			blocks := []llm.ContentBlock{{Type: "tool_result", ToolUseID: "same-id", TraceCallID: "occurrence", ToolResult: body, Presentation: map[string]any{"read_view": "token"}}}
			if err := loop.commitToolBatchWithContext(context.Background(), blocks); err != nil {
				t.Fatal(err)
			}
			if len(read.visible) != 1 || read.visible[0] == large {
				t.Fatalf("observer visibility=%v want %v", read.visible, !large)
			}
		})
	}
}

func TestReadProjectionLowBudgetRecoversOriginalSourcePage(t *testing.T) {
	loop := &Loop{SpillDir: t.TempDir(), ToolResultTokenLimit: 800, ToolBatchTokenLimit: 1000}
	block := llm.ContentBlock{Type: "tool_result", ToolUseID: "read-long", ToolResult: strings.Repeat("source ", 2000), Presentation: map[string]any{"read_view": map[string]any{"path": "/fixture/original.txt"}, "read_page": map[string]any{"start": 128, "end": 8192, "total": 60000}}}
	out, err := loop.projectToolBatch([]llm.ContentBlock{block})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out[0].ToolResult, `path="/fixture/original.txt", byte_offset=128, byte_limit=400`) {
		t.Fatalf("Read budget recovery lost original byte position: %s", out[0].ToolResult)
	}
}
