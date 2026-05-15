package builtin

// fork_placeholders_test.go pins the 2026-05-15 fix that synthesizes
// placeholder tool_results before appending the directive. The original
// fork code wrote a user TEXT message right after a snapshot whose tail
// was the parent's `assistant{tool_use: Fork(...)}` turn, leaving the
// outer tool_use without a paired tool_result. OpenAI / DeepSeek
// strictly reject this shape with `invalid_request_error: insufficient
// tool messages following tool_calls message`.

import (
	"testing"

	"github.com/Ricardo-M-L/metis/internal/llm"
)

func TestBuildPlaceholderToolResults_EmptyMessages(t *testing.T) {
	got := buildPlaceholderToolResults(nil)
	if got != nil {
		t.Errorf("empty input should yield nil; got %+v", got)
	}
}

func TestBuildPlaceholderToolResults_NoToolUseInTail(t *testing.T) {
	// Common case for Agent (cold spawn) — last message is user text.
	// Fork would never see this, but the helper must still return nil
	// without panicking.
	msgs := []llm.Message{
		{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text", Text: "hi"}}},
		{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: "text", Text: "ok"}}},
	}
	got := buildPlaceholderToolResults(msgs)
	if got != nil {
		t.Errorf("assistant text-only tail should yield no placeholders; got %+v", got)
	}
}

func TestBuildPlaceholderToolResults_SingleToolUseTail(t *testing.T) {
	// The actual Fork scenario: parent's last turn is `assistant{Fork(...)}`.
	msgs := []llm.Message{
		{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text", Text: "do it"}}},
		{Role: llm.RoleAssistant, Content: []llm.ContentBlock{
			{Type: "tool_use", ToolUseID: "tu-outer-fork", ToolName: "Fork", ToolInput: map[string]any{"directive": "x"}},
		}},
	}
	got := buildPlaceholderToolResults(msgs)
	if len(got) != 1 {
		t.Fatalf("expected 1 placeholder; got %d (%+v)", len(got), got)
	}
	if got[0].Type != "tool_result" {
		t.Errorf("Type = %q, want tool_result", got[0].Type)
	}
	if got[0].ToolUseID != "tu-outer-fork" {
		t.Errorf("ToolUseID = %q, want tu-outer-fork", got[0].ToolUseID)
	}
	if got[0].ToolResult == "" {
		t.Error("ToolResult body must be non-empty (model needs SOMETHING to read)")
	}
}

func TestBuildPlaceholderToolResults_ParallelToolUsesInTail(t *testing.T) {
	// Edge case: parent did parallel tool calls including Fork. ALL of
	// the tool_uses in that assistant message must get paired
	// placeholders so the child's API call has a valid shape — partial
	// pairing would re-introduce the same 400.
	msgs := []llm.Message{
		{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text", Text: "burst"}}},
		{Role: llm.RoleAssistant, Content: []llm.ContentBlock{
			{Type: "tool_use", ToolUseID: "tu-1", ToolName: "Grep", ToolInput: map[string]any{"pattern": "x"}},
			{Type: "tool_use", ToolUseID: "tu-2", ToolName: "Fork", ToolInput: map[string]any{"directive": "y"}},
			{Type: "tool_use", ToolUseID: "tu-3", ToolName: "LS", ToolInput: map[string]any{"path": "/"}},
		}},
	}
	got := buildPlaceholderToolResults(msgs)
	if len(got) != 3 {
		t.Fatalf("expected 3 placeholders (one per tool_use); got %d", len(got))
	}
	wantIDs := []string{"tu-1", "tu-2", "tu-3"}
	for i, w := range wantIDs {
		if got[i].ToolUseID != w {
			t.Errorf("placeholder[%d].ToolUseID = %q, want %q", i, got[i].ToolUseID, w)
		}
	}
}

func TestBuildPlaceholderToolResults_OnlyTailMatters(t *testing.T) {
	// A tool_use deeper in history must NOT generate a placeholder —
	// only the unpaired tail matters. Real conversations have lots of
	// tool_uses that ALREADY have results paired with them.
	msgs := []llm.Message{
		{Role: llm.RoleAssistant, Content: []llm.ContentBlock{
			{Type: "tool_use", ToolUseID: "tu-old", ToolName: "Grep"},
		}},
		{Role: llm.RoleUser, Content: []llm.ContentBlock{
			{Type: "tool_result", ToolUseID: "tu-old", ToolResult: "matched"},
		}},
		{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: "text", Text: "all set"}}},
	}
	got := buildPlaceholderToolResults(msgs)
	if got != nil {
		t.Errorf("trailing text-only assistant should yield nil; got %+v", got)
	}
}
