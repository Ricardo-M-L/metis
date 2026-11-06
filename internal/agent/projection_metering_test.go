package agent

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/llm/anthropic"
	"github.com/Ricardo-M-L/metis/internal/llm/openai"
)

// Acceptance 6: persisted local checkpoints must not become token pressure
// when routing switches to a transport that does not replay them.
func TestProjectedMeteringAfterLocalTransportSwitch(t *testing.T) {
	for _, name := range []string{"anthropic", "defaultChat", "managed", "wrappedManaged"} {
		t.Run(name, func(t *testing.T) {
			local := openai.NewResponses("fixture", "http://localhost", "fixture", 64, time.Second, 0)
			local.StateMode = openai.ResponsesStateLocal
			l := &Loop{Provider: local, Model: "fixture", System: "sys", Messages: []llm.Message{{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text", Text: "hello"}}}}}
			l.checkpointDynamicContextLocked([]llm.SystemSection{{Name: "base", Body: "sys"}, {Name: "runtime_state", Body: strings.Repeat("old state ", 2000)}})
			localReq, oldAnchor := l.buildRequestWithContext(nil)
			if oldAnchor.messageCount != len(l.Messages) || len(localReq.Messages) != len(l.Messages) {
				t.Fatal("local checkpoint or live history anchor lost")
			}
			localUsage := estimateUsageFloor(localReq.Messages) + estimateRequestOverhead(localReq)
			localHistory := l.History()
			l.Messages = append(l.Messages, llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: "text", Text: "ok"}}})
			l.storeActiveContextSnapshotLocked(&usageTotals{in: localUsage, out: 1}, oldAnchor)
			if !l.activeContext.valid || l.activeContext.tokens < localUsage {
				t.Fatal("local authoritative usage was not retained")
			}
			l.Messages = localHistory
			var target llm.Provider
			switch name {
			case "anthropic":
				target = anthropic.New("fixture", "http://localhost", "fixture", 64, time.Second, "")
			case "defaultChat":
				target = openai.New("fixture", "http://localhost", "fixture", 64, time.Second, 0)
			default:
				managed := openai.NewResponses("fixture", "http://localhost", "fixture", 64, time.Second, 0)
				managed.StateMode = openai.ResponsesStateProvider
				target = managed
				if name == "wrappedManaged" {
					target = llm.BindExecutionProvider(managed, context.Background())
				}
			}
			l.RebindProviderModel(target, "fixture")
			projectedFallback := estimateActiveHistoryTokens(l.projectDynamicCheckpoints(l.Messages)) + int(l.requestOverheadTokens.Load())
			l.mu.Lock()
			busyFallback := l.EstimateContextTokens()
			l.mu.Unlock()
			if busyFallback != projectedFallback {
				t.Errorf("busy routing fallback=%d projected=%d", busyFallback, projectedFallback)
			}
			history := l.History()
			req, anchor := l.buildRequestWithContext(nil)
			if len(req.Messages) != 1 || req.Messages[0].Role != llm.RoleUser || req.Messages[0].Content[0].Text != "hello" || anchor.messageCount != len(history) {
				t.Fatal("switch must project only the old checkpoint, preserving the live anchor")
			}
			want := estimateActiveHistoryTokens([]llm.Message{{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text", Text: "hello"}}}}) + estimateRequestOverhead(req)
			l.rescueNoTools, l.BypassNextCache = true, true
			if got := l.EstimateRequestContextTokens(nil); got != want {
				t.Errorf("preflight=%d projected=%d", got, want)
			}
			if got := l.EstimateContextTokens(); got != want {
				t.Errorf("unknown snapshot fallback=%d projected=%d", got, want)
			}
			if got := l.ContextUsageSnapshot(); got.Used != want || got.Breakdown == nil || got.Breakdown.MessageTokens != estimateActiveHistoryTokens(req.Messages) {
				t.Errorf("UI fallback=%+v want=%d", got, want)
			}
			if !reflect.DeepEqual(history, l.History()) || !l.rescueNoTools || !l.BypassNextCache {
				t.Fatal("estimation mutated history or one-shot flags")
			}
			l.Messages = append(l.Messages, llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: "text", Text: "ok"}}})
			l.storeActiveContextSnapshotLocked(&usageTotals{in: 100, out: 1}, oldAnchor)
			if l.activeContext.valid {
				t.Fatal("old routing response accepted")
			}
			promptUsage := 100
			if name == "managed" || name == "wrappedManaged" {
				// Managed usage covers the server-owned chain, not only the
				// locally projected request/delta. Never cap it to our estimate.
				promptUsage = 3000
			}
			l.storeActiveContextSnapshotLocked(&usageTotals{in: promptUsage, out: 1}, anchor)
			if !l.activeContext.valid || l.activeContext.tokens < promptUsage {
				t.Fatal("projected request usage rejected or capped")
			}
			base := l.EstimateContextTokens()
			l.Messages = append(l.Messages, llm.Message{Role: llm.RoleSystem, Content: []llm.ContentBlock{{Type: "text", Text: strings.Repeat("omitted ", 2000), ProviderHint: map[string]string{dynamicCheckpointHint: "checkpoint"}}}})
			added := llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text", Text: "next question"}}}
			l.Messages = append(l.Messages, added)
			if got, want := l.EstimateContextTokens(), base+estimateActiveHistoryTokens([]llm.Message{added}); got != want {
				t.Errorf("projected increment=%d want=%d", got, want)
			}
			if got, want := l.EstimateRequestContextTokens(nil), l.EstimateContextTokens(); got != want {
				t.Errorf("increment preflight=%d UI=%d", got, want)
			}
			l.rescueNoTools, l.BypassNextCache = false, false
			// Missing prompt usage takes the projected fallback, not durable history.
			req, anchor = l.buildRequestWithContext(nil)
			l.Messages = append(l.Messages, llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: "text", Text: "unknown"}}})
			l.storeActiveContextSnapshotLocked(&usageTotals{out: 1}, anchor)
			want = estimateActiveHistoryTokens(l.projectDynamicCheckpoints(l.Messages)) + estimateRequestOverhead(req)
			if got := l.ContextUsageSnapshot().Used; got != want {
				t.Errorf("missing usage fallback=%d want=%d", got, want)
			}
		})
	}
}
