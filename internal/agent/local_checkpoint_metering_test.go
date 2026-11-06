package agent

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/llm/openai"
)

func TestLocalCheckpointPreflightProjection(t *testing.T) {
	for _, typed := range []bool{false, true} {
		for _, wrapped := range []bool{false, true} {
			t.Run(map[bool]string{false: "legacy", true: "typed"}[typed]+map[bool]string{false: "/direct", true: "/wrapped"}[wrapped], func(t *testing.T) {
				p := openai.NewResponses("fixture", "http://localhost", "fixture", 64000, time.Second, 0)
				p.StateMode = openai.ResponsesStateLocal
				var provider llm.Provider = p
				if wrapped {
					provider = llm.BindExecutionProvider(p, context.Background())
				}
				state := "runtime original"
				l := &Loop{Provider: provider, Model: "fixture", System: "base", PlanSystemPrompt: strings.Repeat("plan instructions ", 700), planMode: true, turnMemoryPrepared: true, turnMemoryContext: "memory body", CurrentStateSections: func() []llm.SystemSection { return []llm.SystemSection{{Name: "custom_runtime", Body: state}} }, Messages: []llm.Message{{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text", Text: "hello"}}}}}
				if typed {
					l.SystemSections = []llm.SystemSection{{Name: "base", Body: "base"}}
				}
				req, anchor := l.buildRequestWithContext(nil)
				prompt := estimateActiveHistoryTokens(req.Messages) + estimateRequestOverhead(req)
				l.Messages = append(l.Messages, llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: "text", Text: "ok"}}})
				l.storeActiveContextSnapshotLocked(&usageTotals{in: prompt, out: 1}, anchor)
				for _, changed := range []bool{false, true} {
					if changed {
						state = "runtime changed"
					}
					history := l.History()
					active := l.activeContext
					l.BypassNextCache = true
					l.rescueNoTools = true
					before := l.EstimateContextTokens()
					got := l.EstimateRequestContextTokens(nil)
					if !changed && got != before {
						t.Errorf("unchanged before=%d preflight=%d", before, got)
					}
					if !reflect.DeepEqual(history, l.History()) || l.activeContext != active || !l.BypassNextCache || !l.rescueNoTools {
						t.Fatal("preflight mutated request state")
					}
					l.mu.Lock()
					immediateBusyUI := l.ContextUsageSnapshot()
					l.mu.Unlock()
					if immediateBusyUI.Used != got {
						t.Errorf("immediate busy UI=%d preflight=%d", immediateBusyUI.Used, got)
					}
					ui := l.ContextUsageSnapshot()
					if ui.Used != got {
						t.Errorf("UI=%d preflight=%d", ui.Used, got)
					}
					l.mu.Lock()
					busy := l.EstimateContextTokens()
					busyUI := l.ContextUsageSnapshot()
					l.mu.Unlock()
					if busy != got || busyUI.Used != got {
						t.Errorf("busy=%d UI=%d preflight=%d", busy, busyUI.Used, got)
					}
					if repeat := l.EstimateRequestContextTokens(nil); repeat != got {
						t.Errorf("repeated preflight=%d first=%d", repeat, got)
					}
					l.BypassNextCache = false
					l.rescueNoTools = false
					next, _ := l.buildRequestWithContext(nil)
					want := before + estimateActiveHistoryTokens(next.Messages) - estimateActiveHistoryTokens(history)
					if got != want {
						t.Errorf("changed=%v preflight=%d projected=%d", changed, got, want)
					}
				}
			})
		}
	}
}
