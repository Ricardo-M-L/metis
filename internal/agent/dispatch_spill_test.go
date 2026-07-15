package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/tools"
	pubtool "github.com/Ricardo-M-L/metis/pkg/tool"
)

// bigOutputTool returns a payload past the default spill threshold.
type bigOutputTool struct{ output string }

func (bigOutputTool) Name() string                                 { return "BigStub" }
func (bigOutputTool) Description() string                          { return "test big-output stub" }
func (bigOutputTool) InputSchema() map[string]any                  { return map[string]any{"type": "object"} }
func (bigOutputTool) Concurrency(map[string]any) tools.Concurrency { return tools.ConcurrencySafe }
func (bigOutputTool) CanUse(context.Context, map[string]any) (tools.Permission, string) {
	return tools.PermissionAllow, ""
}
func (bigOutputTool) IsEnabled() bool { return true }
func (b bigOutputTool) Execute(context.Context, map[string]any) (*tools.Result, error) {
	return &tools.Result{Output: b.output}, nil
}

// unlimitedTool opts out via MaxResultSizer, like Read does.
type unlimitedTool struct{ bigOutputTool }

func (unlimitedTool) Name() string            { return "UnlimitedStub" }
func (unlimitedTool) MaxResultSizeChars() int { return tools.ResultSizeUnlimited }

// TestDispatch_SpillsOversizedToolResult — a tool_result past the
// default 50k threshold must be persisted to the spill dir and the
// model-facing block replaced with a preview + Read-recoverable path.
func TestDispatch_SpillsOversizedToolResult(t *testing.T) {
	dir := t.TempDir()
	payload := strings.Repeat("x", pubtool.DefaultMaxResultSizeChars+100)

	reg := tools.NewRegistry()
	reg.Register(bigOutputTool{output: payload})
	loop := &Loop{
		Registry: reg,
		SpillDir: dir,
	}
	uses := []llm.ContentBlock{
		{Type: "tool_use", ToolUseID: "tu_big", ToolName: "BigStub"},
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
	if len(body) >= len(payload) {
		t.Fatalf("tool_result not spilled; len = %d", len(body))
	}
	if !strings.Contains(body, "use Read tool") {
		t.Errorf("stub missing recovery idiom; got %q", body[:120])
	}
	// ".spill.txt" suffix keeps spill disjoint from Microcompact's
	// bare "<id>.txt" namespace (2026-06-11 review).
	path := filepath.Join(dir, "tu_big.spill.txt")
	got, rerr := os.ReadFile(path)
	if rerr != nil {
		t.Fatalf("spill file not written: %v", rerr)
	}
	if string(got) != payload {
		t.Error("spilled content differs from original output")
	}
	if !strings.Contains(body, path) {
		t.Errorf("stub does not reference spill path %s", path)
	}
}

// TestDispatch_SpillRespectsUnlimitedOptOut — tools returning
// ResultSizeUnlimited (Read) must pass their output through verbatim.
func TestDispatch_SpillRespectsUnlimitedOptOut(t *testing.T) {
	dir := t.TempDir()
	payload := strings.Repeat("y", pubtool.DefaultMaxResultSizeChars+100)

	reg := tools.NewRegistry()
	reg.Register(unlimitedTool{bigOutputTool{output: payload}})
	loop := &Loop{
		Registry: reg,
		SpillDir: dir,
	}
	uses := []llm.ContentBlock{
		{Type: "tool_use", ToolUseID: "tu_unlimited", ToolName: "UnlimitedStub"},
	}
	out := make(chan Event, 16)
	results, _ := loop.executeBatch(context.Background(), uses, out, HookContext{})
	if len(results) != 1 {
		t.Fatalf("result count = %d, want 1", len(results))
	}
	if results[0].ToolResult != payload {
		t.Errorf("opt-out tool result was modified; len = %d, want %d", len(results[0].ToolResult), len(payload))
	}
}

// TestDispatch_SpillSkippedWithoutDir — no Compactor / no cache dir
// means no spilling (e.g. METIS_MICROCOMPACT=0 sessions); the result
// passes through whole rather than being lost.
func TestDispatch_SpillSkippedWithoutDir(t *testing.T) {
	payload := strings.Repeat("z", pubtool.DefaultMaxResultSizeChars+100)

	reg := tools.NewRegistry()
	reg.Register(bigOutputTool{output: payload})
	loop := &Loop{Registry: reg} // SpillDir unset → spilling disabled
	uses := []llm.ContentBlock{
		{Type: "tool_use", ToolUseID: "tu_nodir", ToolName: "BigStub"},
	}
	out := make(chan Event, 16)
	results, _ := loop.executeBatch(context.Background(), uses, out, HookContext{})
	if len(results) != 1 {
		t.Fatalf("result count = %d, want 1", len(results))
	}
	if results[0].ToolResult != payload {
		t.Error("result should pass through untouched when spill dir is unset")
	}
}
