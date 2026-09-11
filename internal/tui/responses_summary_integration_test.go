package tui

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/llm/openai"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

// Exercise the real Responses parser -> agent events -> chat rendering chain.
// All HTTP traffic stays on this test's loopback server; no user session or
// credentials are used. Encrypted replay data must never become visible text.
func TestResponsesSummaryTerminalOnlyReachesChat(t *testing.T) {
	const summary = "PUBLIC_SUMMARY_MARKER"
	const cipher = "OPAQUE_CIPHER_MUST_NOT_RENDER"
	for _, terminalOnly := range []bool{false, true} {
		t.Run(fmt.Sprintf("completed_only_%v", terminalOnly), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				send := func(value any) {
					data, err := json.Marshal(value)
					if err != nil {
						t.Errorf("encode event: %v", err)
						return
					}
					fmt.Fprintf(w, "data: %s\n\n", data)
				}
				item := map[string]any{
					"id": "rs_visible", "type": "reasoning", "encrypted_content": cipher,
					"summary": []any{map[string]any{"type": "summary_text", "text": summary}},
				}
				send(map[string]any{"type": "response.created", "response": map[string]any{"id": "resp_visible"}})
				if !terminalOnly {
					send(map[string]any{"type": "response.output_item.done", "output_index": 0, "item": item})
				}
				send(map[string]any{"type": "response.output_text.delta", "delta": "ANSWER_MARKER"})
				send(map[string]any{"type": "response.completed", "response": map[string]any{
					"id": "resp_visible", "status": "completed", "output": []any{item},
				}})
			}))
			defer server.Close()

			p := openai.NewResponses("local-test-key", server.URL, "gpt-5.5", 1024, 5*time.Second, 0)
			loop := agent.NewLoop(p, tools.NewRegistry(), nil, nil, "Local summary integration test.", 2)
			loop.AppendUser("Reply using the local fixture.")
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			events := make(chan agent.Event, 64)
			done := make(chan error, 1)
			go func() {
				defer close(events)
				done <- loop.Run(ctx, events)
			}()
			m := newE2EModel(t, 100, 35, 0)
			m.thinkingDisplay = "show"
			for event := range events {
				m.handleAgentEvent(event)
			}
			if err := <-done; err != nil {
				t.Fatalf("loop failed: %v", err)
			}
			m.finalizeTurn(nil)
			historyBefore, err := json.Marshal(loop.History())
			if err != nil {
				t.Fatal(err)
			}
			out := m.View().Content
			if got := strings.Count(out, summary); got != 1 {
				t.Fatalf("public summary rendered %d times, want once; view=%q", got, out)
			}
			if !strings.Contains(out, "ANSWER_MARKER") {
				t.Fatalf("answer missing from chat: %q", out)
			}
			if strings.Contains(out, cipher) {
				t.Fatalf("encrypted reasoning leaked into rendered chat: %q", out)
			}
			if strings.Contains(out, "🔒") || strings.Contains(out, "redacted") {
				t.Fatalf("encrypted reasoning placeholder appeared in chat: %q", out)
			}
			cipherBlocks := 0
			for _, message := range loop.History() {
				for _, block := range message.Content {
					if block.Type == "redacted_thinking" && block.Data == cipher {
						cipherBlocks++
					}
				}
			}
			if !terminalOnly && cipherBlocks != 1 {
				t.Fatalf("encrypted replay blocks retained = %d, want 1", cipherBlocks)
			}
			m.thinkingDisplay = "hide"
			m.renderCache.InvalidateAll()
			if hidden := m.View().Content; strings.Contains(hidden, summary) || strings.Contains(hidden, cipher) {
				t.Fatalf("hidden reasoning leaked into chat: %q", hidden)
			}
			historyAfter, err := json.Marshal(loop.History())
			if err != nil {
				t.Fatal(err)
			}
			if string(historyAfter) != string(historyBefore) {
				t.Fatal("presentation filtering changed provider history")
			}
		})
	}
}
