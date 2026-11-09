package agent

import (
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/llm"
)

func TestHistoryReplacementPressureUsesNativeImageContext(t *testing.T) {
	loop := &Loop{Messages: []llm.Message{{Role: llm.RoleUser, Content: []llm.ContentBlock{
		{Type: "tool_result", ToolUseID: "view-image", ToolResultBlocks: []llm.ContentBlock{
			{Type: "image", MediaType: "image/png", Data: strings.Repeat("A", 1_300_000)},
		}},
	}}}}
	raw := estimateTokens(loop.Messages)
	want := estimateActiveHistoryTokens(loop.projectDynamicCheckpoints(loop.Messages))
	if raw <= want {
		t.Fatalf("fixture did not separate transport and image context: raw=%d active=%d", raw, want)
	}
	loop.mu.Lock()
	loop.storeContextEstimateFromHistory(raw)
	got := loop.historyReplacementTokens
	loop.mu.Unlock()
	if got != want {
		t.Fatalf("replacement watermark used transport tokens: got=%d want=%d raw=%d", got, want, raw)
	}
	if gotContext := loop.EstimateContextTokens(); gotContext != want {
		t.Fatalf("display and replacement pressure disagree: display=%d watermark=%d", gotContext, got)
	}
}
