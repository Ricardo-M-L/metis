package agent

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/llm/openai"
	"github.com/Ricardo-M-L/metis/internal/memory"
)

func TestRequestMeteringOmitsDisplayOnlyThinkingAcrossRounds(t *testing.T) {
	for _, mode := range []openai.ResponsesStateMode{openai.ResponsesStateLocal, openai.ResponsesStateProvider} {
		t.Run(string(mode), func(t *testing.T) {
			p := openai.NewResponses("fixture", "http://127.0.0.1:1", "gpt-5", 128, time.Second, 0)
			p.StateMode = mode
			l := &Loop{Provider: p, Model: "gpt-5", System: "sys", Messages: []llm.Message{{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text", Text: "first"}}}}}
			_, first := l.buildRequestWithContext(nil)
			l.Messages = append(l.Messages, llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: "thinking", Text: strings.Repeat("display only reasoning ", 10000)}, {Type: "provider_state", Text: strings.Repeat("opaque state ", 5000)}, {Type: "text", Text: "answer"}}})
			l.storeActiveContextSnapshotLocked(&usageTotals{in: 1000, out: 50000}, first)
			if !l.activeContext.valid {
				t.Fatal("first response lost authoritative usage")
			}
			l.Messages = append(l.Messages, llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text", Text: "next"}}})
			req, second := l.buildRequestWithContext(nil)
			if second.messageCount != len(l.Messages) {
				t.Fatal("wire metering replaced durable history anchor")
			}
			if second.requestUsageFloor > 1000 {
				t.Errorf("display-only history leaked into usage floor: %d", second.requestUsageFloor)
			}
			l.Messages = append(l.Messages, llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: "text", Text: "second answer"}}})
			l.storeActiveContextSnapshotLocked(&usageTotals{in: 1100, out: 20}, second)
			if !l.activeContext.valid {
				t.Fatal("second-round valid usage rejected because display-only thinking was counted")
			}
			want := 1100 + assistantContextTokens(p, l.Messages[len(l.Messages)-1], 20)
			if got := l.ContextUsageSnapshot().Used; got != want {
				t.Fatalf("active meter=%d want=%d", got, want)
			}
			// Missing provider usage must use the same replay policy, without
			// mutating the actual request/state required by the adapter.
			_, anchor := l.buildRequestWithContext(nil)
			l.Messages = append(l.Messages, llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: "thinking", Text: strings.Repeat("not replayed ", 10000)}, {Type: "text", Text: "fallback"}}})
			l.storeActiveContextSnapshotLocked(&usageTotals{out: 1000}, anchor)
			if got := l.EstimateContextTokens(); got > 2000 {
				t.Fatalf("missing-usage fallback counted display-only reasoning: %d", got)
			}
			if got := l.EstimateRequestContextTokens(nil); got > 2000 {
				t.Fatalf("preflight counted display-only reasoning: %d", got)
			}
			if got := l.ContextUsageSnapshot(); got.Used > 2000 || got.Breakdown == nil || got.Breakdown.MessageTokens > 2000 {
				t.Fatalf("UI breakdown counted display-only reasoning: %+v", got)
			}
			hasThinking, hasProviderState := false, false
			for _, message := range req.Messages {
				for _, block := range message.Content {
					hasThinking = hasThinking || block.Type == "thinking"
					hasProviderState = hasProviderState || block.Type == "provider_state"
				}
			}
			if !hasThinking || !hasProviderState {
				t.Fatal("metering unexpectedly rewrote request/history")
			}
		})
	}
}

func TestRequestPreflightRefreshesPreparedMemory(t *testing.T) {
	for _, local := range []bool{false, true} {
		t.Run(map[bool]string{false: "header", true: "local_checkpoint"}[local], func(t *testing.T) {
			mm, err := memory.NewMemoryManager(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			if err := mm.Core().UpdateBlock("user", "original memory"); err != nil {
				t.Fatal(err)
			}
			base := &activeContextTestProvider{name: "memory-preflight", model: "fixture"}
			var p llm.Provider = base
			if local {
				p = reviewDynamicCheckpointProvider{Provider: base}
			}
			l := &Loop{Provider: p, Model: "fixture", System: "system", Memory: mm, Messages: []llm.Message{{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text", Text: "continue"}}}}}
			l.prepareTurnMemory(context.Background())
			l.buildRequest(nil)
			if err := mm.Core().UpdateBlock("user", strings.Repeat("new memory content ", 200)); err != nil {
				t.Fatal(err)
			}
			before := l.EstimateRequestContextTokens(nil)
			req := l.buildRequest(nil)
			actual := estimateActiveHistoryTokens(req.Messages) + estimateRequestOverhead(req)
			if before != actual {
				t.Fatalf("preflight=%d actual=%d after memory update", before, actual)
			}
			if !strings.Contains(l.turnMemoryContext, "new memory content") {
				t.Fatal("preflight kept stale prepared memory")
			}
			if err := mm.Core().UpdateBlock("user", ""); err != nil {
				t.Fatal(err)
			}
			afterRevoke := l.EstimateRequestContextTokens(nil)
			req = l.buildRequest(nil)
			if actual := estimateActiveHistoryTokens(req.Messages) + estimateRequestOverhead(req); afterRevoke != actual {
				t.Fatalf("revocation preflight=%d actual=%d", afterRevoke, actual)
			}
		})
	}
}

func TestRequestPreflightRefreshesMemoryChangedDuringStateCallback(t *testing.T) {
	for _, local := range []bool{false, true} {
		for _, revoke := range []bool{false, true} {
			name := map[bool]string{false: "header", true: "local"}[local] + map[bool]string{false: "/update", true: "/revoke"}[revoke]
			t.Run(name, func(t *testing.T) {
				mm, err := memory.NewMemoryManager(t.TempDir())
				if err != nil {
					t.Fatal(err)
				}
				if err := mm.Core().UpdateBlock("user", "original memory"); err != nil {
					t.Fatal(err)
				}
				base := &activeContextTestProvider{name: "memory-callback", model: "fixture"}
				var provider llm.Provider = base
				if local {
					provider = reviewDynamicCheckpointProvider{Provider: base}
				}
				l := &Loop{Provider: provider, Model: "fixture", System: "system", Memory: mm, Messages: []llm.Message{{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text", Text: "continue"}}}}}
				l.prepareTurnMemory(context.Background())
				l.buildRequest(nil)
				changed := false
				l.CurrentStateSections = func() []llm.SystemSection {
					if !changed {
						body := strings.Repeat("callback memory ", 200)
						if revoke {
							body = ""
						}
						if err := mm.Core().UpdateBlock("user", body); err != nil {
							t.Fatal(err)
						}
						changed = true
					}
					return []llm.SystemSection{{Name: "runtime", Body: "stable runtime"}}
				}
				got := l.EstimateRequestContextTokens(nil)
				req := l.buildRequest(nil)
				if want := estimateActiveHistoryTokens(req.Messages) + estimateRequestOverhead(req); got != want {
					t.Fatalf("memory changed during callback: preflight=%d actual=%d", got, want)
				}
			})
		}
	}
}
