package agent

import (
	"testing"

	"github.com/Ricardo-M-L/metis/internal/llm"
)

func TestProjectedRequestUsageAnchorsLiveHistory(t *testing.T) {
	l, p, _ := newContinuationTestLoop(3, 0)
	l.Messages = append(l.Messages, llm.Message{Role: llm.RoleSystem, Content: []llm.ContentBlock{{Type: "text", Text: "host checkpoint", ProviderHint: map[string]string{dynamicCheckpointHint: "checkpoint"}}}})
	req, anchor := l.buildRequestWithContext(nil)
	if len(req.Messages) >= len(l.Messages) {
		t.Fatal("fixture did not project checkpoint out of request")
	}
	if anchor.messageCount != len(l.Messages) {
		t.Fatal("usage anchor uses projected request length")
	}
	l.Messages = append(l.Messages, llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: "text", Text: "ok"}}})
	l.storeActiveContextSnapshotLocked(&usageTotals{in: 100, out: 1}, anchor)
	if !l.activeContext.valid {
		t.Fatalf("actual provider usage rejected after projected request: %s", p.Name())
	}
}
