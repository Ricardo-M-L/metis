package agent

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/llm/openai"
)

// Capture the real Responses HTTP encoder, not a second local approximation.
// An unchanged checkpoint must preserve the entire body and the active meter.
func TestLocalCheckpointUnchangedMeterMatchesEncodedWire(t *testing.T) {
	for _, typed := range []bool{false, true} {
		for _, wrapped := range []bool{false, true} {
			name := map[bool]string{false: "legacy", true: "typed"}[typed] + map[bool]string{false: "/direct", true: "/wrapped"}[wrapped]
			t.Run(name, func(t *testing.T) {
				bodies := make(chan []byte, 4)
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					body, err := io.ReadAll(r.Body)
					if err != nil {
						t.Errorf("read body: %v", err)
					}
					bodies <- body
					w.Header().Set("Content-Type", "application/json")
					_, _ = io.WriteString(w, `{"id":"fixture-response","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}],"usage":{"input_tokens":1,"output_tokens":1}}`)
				}))
				defer server.Close()
				p := openai.NewResponses("fixture", server.URL, "fixture", 64000, 5*time.Second, 0)
				p.StateMode = openai.ResponsesStateLocal
				var provider llm.Provider = p
				if wrapped {
					provider = llm.BindExecutionProvider(p, context.Background())
				}
				state := RuntimeStateSnapshot{Model: "fixture", Provider: p.Name(), WorkingDirectory: "/unchanged-workspace", PermissionMode: "plan"}
				l := &Loop{Provider: provider, Model: "fixture", System: "stable system", PlanSystemPrompt: strings.Repeat("privileged-plan-overlay ", 700), planMode: true, CurrentStateSnapshot: func() RuntimeStateSnapshot { return state }, Messages: []llm.Message{{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text", Text: "hello"}}}}}
				if typed {
					l.SystemSections = []llm.SystemSection{{Name: "base", Body: "stable system"}}
				}
				initial, anchor := l.buildRequestWithContext(nil)
				prompt := estimateActiveHistoryTokens(initial.Messages) + estimateRequestOverhead(initial)
				l.Messages = append(l.Messages, llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: "text", Text: "previous answer"}}})
				l.storeActiveContextSnapshotLocked(&usageTotals{in: prompt, out: 4}, anchor)
				if !l.activeContext.valid {
					t.Fatal("fixture failed to establish authoritative response snapshot")
				}
				first := l.buildRequest(nil)
				send := func(req llm.Request) []byte {
					t.Helper()
					if _, err := provider.Complete(context.Background(), req); err != nil {
						t.Fatal(err)
					}
					return <-bodies
				}
				originalBody := send(first)
				for _, text := range []string{"privileged-plan-overlay", "/unchanged-workspace", "dynamic_context_snapshot"} {
					if !bytes.Contains(originalBody, []byte(text)) {
						t.Fatalf("actual wire omitted %q", text)
					}
				}
				history := l.History()
				active := l.activeContext
				baseline := l.EstimateContextTokens()
				for round := 0; round < 2; round++ {
					got := l.EstimateRequestContextTokens(nil)
					if got != baseline {
						t.Fatalf("round %d unchanged wire estimate=%d baseline=%d", round, got, baseline)
					}
					if !reflect.DeepEqual(history, l.History()) || l.activeContext != active {
						t.Fatal("preflight changed live history or response anchor")
					}
					next := l.buildRequest(nil)
					nextBody := send(next)
					if !bytes.Equal(originalBody, nextBody) {
						t.Fatalf("round %d unchanged state altered actual encoded HTTP body (original=%d next=%d bytes)", round, len(originalBody), len(nextBody))
					}
					if !reflect.DeepEqual(first, next) {
						t.Fatal("unchanged state changed assembled request")
					}
					ui := l.ContextUsageSnapshot()
					l.mu.Lock()
					busy := l.EstimateContextTokens()
					busyUI := l.ContextUsageSnapshot()
					l.mu.Unlock()
					if ui.Used != baseline || busy != baseline || busyUI.Used != baseline {
						t.Fatalf("meter/UI/busy disagree: %d/%d/%d want %d", ui.Used, busy, busyUI.Used, baseline)
					}
				}
				t.Logf("unchanged HTTP body=%d bytes, estimate=%d, checkpoints/history=%d", len(originalBody), baseline, len(history))
			})
		}
	}
}
