package agent

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/llm/openai"
	"github.com/Ricardo-M-L/metis/internal/memory"
)

// Exercise the actual Loop -> Responses HTTP boundary: Cache=true describes
// an explicit-cache block, but does not make mutable state safe to put ahead
// of the entire conversation in Responses' implicit-prefix cache.
func TestResponsesWireMutableContextPreservesHistoryPrefix(t *testing.T) {
	for _, codex := range []bool{false, true} {
		name := "responses"
		if codex {
			name = "openai-codex"
		}
		t.Run(name, func(t *testing.T) {
			type wireRequest struct {
				Instructions string            `json:"instructions"`
				CacheKey     string            `json:"prompt_cache_key"`
				Input        []json.RawMessage `json:"input"`
				Tools        json.RawMessage   `json:"tools"`
				Store        bool              `json:"store"`
			}
			captured := make(chan wireRequest, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				var body wireRequest
				if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
					t.Errorf("decode wire request: %v", err)
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				captured <- body
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"fixture\",\"status\":\"completed\",\"output\":[]}}\n\n")
			}))
			t.Cleanup(server.Close)
			p := openai.NewResponses("fixture-key", server.URL, "gpt-5.5", 256, 5*time.Second, 0)
			if codex {
				p = openai.NewCodexResponses("gpt-5.5", 256, 5*time.Second, 0, func(context.Context) (openai.ResponsesOAuthCredential, error) {
					return openai.ResponsesOAuthCredential{AccessToken: "fixture-token", AccountID: "fixture-account"}, nil
				})
				p.BaseURL = server.URL
			}
			if err := p.ConfigureCapabilityProfile("openai"); err != nil {
				t.Fatal(err)
			}
			mm, err := memory.NewMemoryManager(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			if err := mm.Core().UpdateBlock("user", "memory-before"); err != nil {
				t.Fatal(err)
			}
			state := RuntimeStateSnapshot{SessionID: "cache-fixture-session", PermissionMode: "default", WorkingDirectory: "/fixture", CurrentPlan: "plan-before"}
			l := &Loop{
				Provider: p, Model: "gpt-5.5", Memory: mm,
				System: "stable base",
				SystemSections: []llm.SystemSection{
					{Name: "base", Body: "stable base", Cache: true},
					{Name: "env", Body: "<env>fixture</env>", Volatile: true},
					{Name: "addendum", Body: "stable instructions", Cache: true},
				},
				CurrentStateSnapshot: func() RuntimeStateSnapshot { return state },
				Messages: []llm.Message{
					{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text", Text: "inspect fixture"}}},
					{Role: llm.RoleAssistant, Content: []llm.ContentBlock{
						{Type: "redacted_thinking", Data: "opaque-fixture", ProviderHint: map[string]string{"openai.responses.item_id": "rs_fixture"}},
						{Type: "tool_use", ToolUseID: "call-fixture", ToolName: "Fixture", ToolInput: map[string]any{}},
					}},
					{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "tool_result", ToolUseID: "call-fixture", ToolResult: "fixture result"}}},
				},
			}
			send := func() wireRequest {
				t.Helper()
				req := l.buildRequest([]llm.ToolSpec{{Name: "Fixture", Description: "fixture tool", InputSchema: map[string]any{"type": "object"}}})
				stream, err := p.Stream(context.Background(), req)
				if err != nil {
					t.Fatal(err)
				}
				defer stream.Close()
				for {
					_, err := stream.Recv()
					if err == io.EOF {
						break
					}
					if err != nil {
						t.Fatal(err)
					}
				}
				return <-captured
			}
			before := send()
			if unchanged := send(); !reflect.DeepEqual(before, unchanged) {
				t.Fatal("unchanged context altered actual HTTP payload")
			}
			if err := mm.Core().UpdateBlock("user", "memory-after"); err != nil {
				t.Fatal(err)
			}
			afterMemory := send()
			state.PermissionMode, state.CurrentPlan = "plan", "plan-after"
			afterState := send()
			for _, current := range []wireRequest{before, afterMemory, afterState} {
				if current.Instructions != "stable base\n\nstable instructions" {
					t.Fatal("mutable memory/runtime context still precedes Responses conversation history")
				}
				if current.Store || current.CacheKey == "" || current.CacheKey != before.CacheKey {
					t.Fatal("local cache fix changed storage policy or session affinity")
				}
				if len(current.Input) != 5 || !reflect.DeepEqual(current.Input[:4], before.Input[:4]) || !reflect.DeepEqual(current.Tools, before.Tools) {
					t.Fatal("mutable context changed history, encrypted reasoning, tool pairs, or tool schemas")
				}
				var last struct{ Role string }
				if err := json.Unmarshal(current.Input[4], &last); err != nil || last.Role != "developer" {
					t.Fatal("fresh context must retain developer authority at the input tail")
				}
			}
			if codex && before.CacheKey != state.SessionID {
				t.Fatal("Codex session cache key changed")
			}
			if tail := string(afterState.Input[4]); !strings.Contains(tail, "memory-after") || !strings.Contains(tail, "plan-after") || !strings.Contains(tail, "permission_mode: plan") || strings.Contains(tail, "memory-before") || strings.Contains(tail, "plan-before") {
				t.Fatal("fresh developer context omitted updates or retained stale state")
			}
			l.AppendUser("next submitted turn")
			nextTurn := send()
			if len(nextTurn.Input) != 6 || !reflect.DeepEqual(nextTurn.Input[:4], before.Input[:4]) || nextTurn.Instructions != before.Instructions {
				t.Fatal("new user turn changed the previously sent conversation prefix")
			}
			if history := l.History(); len(history) != 4 {
				t.Fatal("request-only context was persisted into conversation history")
			}
		})
	}
}
