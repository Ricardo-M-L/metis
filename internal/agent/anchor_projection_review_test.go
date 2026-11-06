package agent

import (
	"github.com/Ricardo-M-L/metis/internal/llm"
	"strings"
	"testing"
)

type reviewDynamicCheckpointProvider struct{ llm.Provider }

func (reviewDynamicCheckpointProvider) DynamicContextCheckpoints() bool { return true }
func TestReviewFilteredCheckpointMustNotInvalidateWireUsage(t *testing.T) {
	for _, checkpointBytes := range []int{0, 8000} {
		t.Run(map[bool]string{true: "filtered_checkpoint", false: "baseline"}[checkpointBytes > 0], func(t *testing.T) {
			l, originalProvider, _ := newContinuationTestLoop(3, 0)
			if checkpointBytes > 0 {
				l.Provider = reviewDynamicCheckpointProvider{Provider: originalProvider}
				l.checkpointDynamicContextLocked([]llm.SystemSection{
					{Name: "base", Body: "sys", Cache: true},
					{Name: "runtime_state", Body: strings.Repeat("old runtime state ", checkpointBytes/18), Cache: true},
				})
				l.Provider = originalProvider
			}
			req, anchor := l.buildRequestWithContext(nil)
			preflight := l.EstimateRequestContextTokens(nil)
			l.Messages = append(l.Messages, llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: "text", Text: "ok"}}})
			l.storeActiveContextSnapshotLocked(&usageTotals{in: 100, out: 1}, anchor)
			t.Logf("projected=%d liveBeforeAssistant=%d projectedFloor=%d liveFloor=%d snapshotValid=%t estimatedAfter=%d preflight=%d", len(req.Messages), anchor.messageCount, estimateUsageFloor(req.Messages), estimateUsageFloor(l.Messages[:anchor.messageCount]), l.activeContext.valid, l.estTokens.Load(), preflight)
			if !l.activeContext.valid {
				t.Fatal("valid usage for projected wire request rejected due to omitted checkpoint")
			}
		})
	}
}
