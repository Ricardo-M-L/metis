package agent

import (
	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/llm/openai"
	"testing"
	"time"
)

func TestLocalCheckpointNativeSnapshotFallback(t *testing.T) {
	p := openai.NewResponses("fixture", "http://localhost", "fixture", 64000, time.Second, 0)
	p.StateMode = openai.ResponsesStateLocal
	state := RuntimeStateSnapshot{Model: "fixture", Provider: p.Name()}
	l := &Loop{Provider: p, Model: "fixture", System: "base", CurrentStateSnapshot: func() RuntimeStateSnapshot { return state }, Messages: []llm.Message{{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text", Text: "hello"}}}}}
	for i := 0; i < 3; i++ {
		if i == 2 {
			l.SetPlanMode(true)
		}
		before := l.History()
		got := l.EstimateRequestContextTokens(nil)
		req, _ := l.buildRequestWithContext(nil)
		want := estimateActiveHistoryTokens(req.Messages) + estimateRequestOverhead(req)
		if got != want {
			t.Fatalf("round %d fallback=%d actual request=%d", i, got, want)
		}
		if i == 1 && len(before) != len(l.Messages) {
			t.Fatal("unchanged snapshot appended again")
		}
		if repeat := l.EstimateRequestContextTokens(nil); repeat != want {
			t.Fatalf("round %d repeated=%d want=%d", i, repeat, want)
		}
		ui := l.ContextUsageSnapshot()
		if ui.Breakdown == nil || ui.Breakdown.SystemTokens+ui.Breakdown.ToolTokens+ui.Breakdown.MessageTokens != want {
			t.Fatalf("breakdown=%+v want=%d", ui.Breakdown, want)
		}
	}
}
