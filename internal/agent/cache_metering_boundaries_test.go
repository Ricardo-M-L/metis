package agent

import (
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/llm"
)

func TestIndependentProjectedAnchorRejectsExtraLiveAppend(t *testing.T) {
	l, p, _ := newContinuationTestLoop(3, 0)
	l.Provider = reviewDynamicCheckpointProvider{Provider: p}
	l.checkpointDynamicContextLocked([]llm.SystemSection{{Name: "runtime_state", Body: strings.Repeat("old runtime state ", 500)}})
	l.Provider = p
	req, anchor := l.buildRequestWithContext(nil)
	if len(req.Messages) != 1 || anchor.messageCount != 2 {
		t.Fatal("projected/live fixture invalid")
	}
	l.Messages = append(l.Messages, llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text", Text: "steered while request in flight"}}})
	l.Messages = append(l.Messages, llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: "text", Text: "stale answer"}}})
	l.storeActiveContextSnapshotLocked(&usageTotals{in: 100, out: 1}, anchor)
	if l.activeContext.valid {
		t.Fatal("old request accepted after unmatched live append")
	}
}

func TestIndependentProjectedDeltaPreservesMixedSystemBody(t *testing.T) {
	l, _, _ := newContinuationTestLoop(3, 0)
	_, anchor := l.buildRequestWithContext(nil)
	l.Messages = append(l.Messages, llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: "text", Text: "accepted"}}})
	l.storeActiveContextSnapshotLocked(&usageTotals{in: 100, out: 1}, anchor)
	if !l.activeContext.valid {
		t.Fatal("initial usage invalid")
	}
	base := l.EstimateContextTokens()
	kept := llm.Message{Role: llm.RoleSystem, Content: []llm.ContentBlock{{Type: "text", Text: "unrelated trusted instruction"}}}
	mixed := kept
	mixed.Content = append(mixed.Content, llm.ContentBlock{Type: "text", Text: strings.Repeat("omitted checkpoint ", 500), ProviderHint: map[string]string{dynamicCheckpointHint: "checkpoint"}})
	added := llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text", Text: "new question"}}}
	l.Messages = append(l.Messages, mixed, added)
	want := base + estimateActiveHistoryTokens([]llm.Message{kept, added})
	if got := l.EstimateContextTokens(); got != want {
		t.Fatalf("delta=%d want=%d", got, want)
	}
	t.Logf("base=%d mixed-suffix-total=%d", base, want)
}
