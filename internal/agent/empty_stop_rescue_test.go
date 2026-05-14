package agent

import (
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/llm"
)

func TestHasUserFacingText_TextBlock(t *testing.T) {
	yes := hasUserFacingText([]llm.ContentBlock{
		{Type: "text", Text: "found it: line 42 has the bug"},
	})
	if !yes {
		t.Error("non-empty text block should count as user-facing")
	}
}

func TestHasUserFacingText_EmptyContent(t *testing.T) {
	if hasUserFacingText(nil) {
		t.Error("nil content has no text")
	}
	if hasUserFacingText([]llm.ContentBlock{}) {
		t.Error("empty content slice has no text")
	}
}

func TestHasUserFacingText_WhitespaceOnlyIsEmpty(t *testing.T) {
	cases := []string{"", " ", "\n", "\t  \n  "}
	for _, w := range cases {
		t.Run("ws="+strings.ReplaceAll(w, "\n", "\\n"), func(t *testing.T) {
			got := hasUserFacingText([]llm.ContentBlock{{Type: "text", Text: w}})
			if got {
				t.Errorf("whitespace-only text %q should not count", w)
			}
		})
	}
}

func TestHasUserFacingText_ToolUseAlone(t *testing.T) {
	// The Bug B case: model emitted only tool_use blocks, no text.
	// Rescue must fire.
	yes := hasUserFacingText([]llm.ContentBlock{
		{Type: "tool_use", ToolName: "Read", ToolInput: map[string]any{"path": "/foo"}},
		{Type: "tool_use", ToolName: "Grep", ToolInput: map[string]any{"pattern": "bar"}},
	})
	if yes {
		t.Error("tool_use-only content has no user-facing text — rescue should fire")
	}
}

func TestHasUserFacingText_ThinkingDoesNotCount(t *testing.T) {
	// `thinking` blocks are internal — the user never sees them. A
	// stop with only thinking + tool_use is still a silent stop.
	yes := hasUserFacingText([]llm.ContentBlock{
		{Type: "thinking", Text: "let me check the file"},
		{Type: "tool_use", ToolName: "Read", ToolInput: map[string]any{"path": "/x"}},
	})
	if yes {
		t.Error("thinking blocks should not satisfy the user-facing-text check")
	}
}

func TestHasUserFacingText_MixedBlocks(t *testing.T) {
	// Real assistant turn: thinking + tool_use + final text. The
	// text block at the end is what the user actually reads.
	yes := hasUserFacingText([]llm.ContentBlock{
		{Type: "thinking", Text: "internal"},
		{Type: "tool_use", ToolName: "Read"},
		{Type: "text", Text: "done — see line 42"},
	})
	if !yes {
		t.Error("mixed content with a final text block should count as user-facing")
	}
}

func TestEmptyStopRescueMessage_ContainsKeyDirectives(t *testing.T) {
	// Pin the directive shape: must tell model to summarize and not
	// restart investigation. If a future refactor accidentally drops
	// "don't restart" the model will happily redo the whole thing.
	msg := emptyStopRescueMessage
	for _, want := range []string{
		"1-3 lines",
		"Don't restart",
		"system-reminder",
		"blank screen",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("rescue message missing %q; got:\n%s", want, msg)
		}
	}
}
