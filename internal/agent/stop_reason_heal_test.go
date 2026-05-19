package agent

// stop_reason_heal_test.go — covers the 2026-05-18 root-cause fix for
// session 8cfc076b. MiniMax / some OpenAI-compatible gateways report
// finish_reason="stop" (→ "end_turn") on a chunk that ALSO carried
// tool_calls. Pre-fix the loop took the reported stop at face value,
// skipped executeBatch, and orphaned the tool_use blocks.
//
// The heal: if assistant content contains ANY tool_use block, force
// stop to "tool_use" so executeBatch runs. containsToolUseBlock is
// the predicate the loop now checks.

import (
	"testing"

	"github.com/Ricardo-M-L/metis/internal/llm"
)

func TestContainsToolUseBlock_True(t *testing.T) {
	t.Parallel()
	blocks := []llm.ContentBlock{
		{Type: "thinking", Text: "..."},
		{Type: "text", Text: "calling agent..."},
		{Type: "tool_use", ToolUseID: "id-A", ToolName: "Agent"},
	}
	if !containsToolUseBlock(blocks) {
		t.Error("expected true for content containing tool_use")
	}
}

func TestContainsToolUseBlock_FalseOnPureText(t *testing.T) {
	t.Parallel()
	blocks := []llm.ContentBlock{
		{Type: "text", Text: "final answer"},
	}
	if containsToolUseBlock(blocks) {
		t.Error("expected false for content with no tool_use")
	}
}

func TestContainsToolUseBlock_FalseOnEmpty(t *testing.T) {
	t.Parallel()
	if containsToolUseBlock(nil) {
		t.Error("expected false for nil")
	}
	if containsToolUseBlock([]llm.ContentBlock{}) {
		t.Error("expected false for empty slice")
	}
}

func TestContainsToolUseBlock_IgnoresToolResult(t *testing.T) {
	t.Parallel()
	// tool_result is a USER-role block and shouldn't trigger heal;
	// the predicate is only meaningful on assistant content.
	blocks := []llm.ContentBlock{
		{Type: "tool_result", ToolUseID: "id-A", ToolResult: "ok"},
	}
	if containsToolUseBlock(blocks) {
		t.Error("tool_result should not trigger tool_use detection")
	}
}
