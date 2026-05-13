package agent

// loop_estimate_test.go — pins the EstimateContextTokens accessor that
// the TUI status bar uses as a monotone floor under provider-reported
// cache-aware usage figures. Per the 2026-05-13 bug ("token went from
// 50k to 30k between turns 3 and 4"): the underlying gateway
// under-reported `cache_read_input_tokens` on some turns and the
// status bar took the bad number at face value. The new floor reads
// directly from on-disk Loop.Messages so the displayed denominator
// only shrinks when compaction emits a visible event.

import (
	"testing"

	"github.com/Ricardo-M-L/metis/internal/llm"
)

func TestEstimateContextTokens_GrowsWithHistory(t *testing.T) {
	l := &Loop{}
	if n := l.EstimateContextTokens(); n != 0 {
		t.Errorf("empty loop should estimate ~0; got %d", n)
	}
	l.Messages = []llm.Message{
		{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text", Text: "hi there"}}},
	}
	after1 := l.EstimateContextTokens()
	if after1 <= 0 {
		t.Errorf("non-empty history should estimate > 0; got %d", after1)
	}
	l.Messages = append(l.Messages, llm.Message{
		Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: "text", Text: "reply with some longer body to make sure the byte count increases monotonically"}},
	})
	after2 := l.EstimateContextTokens()
	if after2 <= after1 {
		t.Errorf("appending a message must grow the estimate; before=%d after=%d", after1, after2)
	}
}

func TestEstimateContextTokens_IncludesToolResults(t *testing.T) {
	// Tool results in metis are sometimes the bulk of history; the
	// status bar's floor MUST count them, otherwise a Bash-heavy turn
	// hides its real context cost behind a tiny "free text" estimate.
	l := &Loop{
		Messages: []llm.Message{
			{
				Role: llm.RoleUser,
				Content: []llm.ContentBlock{
					{Type: "text", Text: "run ls"},
				},
			},
			{
				Role: llm.RoleAssistant,
				Content: []llm.ContentBlock{
					{Type: "tool_use", ToolUseID: "u1", ToolName: "Bash"},
				},
			},
			{
				Role: llm.RoleUser,
				Content: []llm.ContentBlock{
					{Type: "tool_result", ToolUseID: "u1",
						ToolResult: "drwxr-xr-x  10  user  staff  320 May 13 00:00 .\ndrwxr-xr-x   5  user  staff  160 May 13 00:00 ..\n-rw-r--r--   1  user  staff   42 May 13 00:00 a.go\n"},
				},
			},
		},
	}
	got := l.EstimateContextTokens()
	if got < 30 { // ~120 chars of tool_result + envelope; ~30 tokens floor
		t.Errorf("tool_result content not counted; got %d (expected ≥30)", got)
	}
}
