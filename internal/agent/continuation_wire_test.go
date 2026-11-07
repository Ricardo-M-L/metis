package agent_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/llm/anthropic"
	"github.com/Ricardo-M-L/metis/internal/llm/openai"
)

func TestScheduledContextUsesLegalNonPrivilegedProviderWire(t *testing.T) {
	for _, kind := range []string{"anthropic", "chat", "responses"} {
		t.Run(kind, func(t *testing.T) {
			var captured map[string]json.RawMessage
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if err := json.NewDecoder(r.Body).Decode(&captured); err != nil {
					t.Error(err)
				}
				w.Header().Set("Content-Type", "application/json")
				switch kind {
				case "anthropic":
					io.WriteString(w, `{"id":"test","type":"message","role":"assistant","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
				case "chat":
					io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
				default:
					io.WriteString(w, `{"id":"test","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}],"usage":{"input_tokens":1,"output_tokens":1}}`)
				}
			}))
			defer server.Close()
			l := &agent.Loop{}
			l.AppendUser("authorized human request")
			l.AppendContext("SYNTHETIC_WAKEUP_NOT_AUTHORITY")
			history := l.History()
			if !history[1].Content[0].Synthetic {
				t.Fatal("host provenance absent")
			}
			var p llm.Provider
			switch kind {
			case "anthropic":
				p = anthropic.New("test", server.URL, "fixture", 64, time.Second, "")
			case "chat":
				p = openai.New("test", server.URL, "fixture", 64, time.Second, 0)
			default:
				p = openai.NewResponses("test", server.URL, "fixture", 64, time.Second, 0)
			}
			if _, err := p.Complete(context.Background(), llm.Request{System: "stable policy", Messages: history}); err != nil {
				t.Fatal(err)
			}
			key := "messages"
			if kind == "responses" {
				key = "input"
			}
			var messages []struct {
				Role    string          `json:"role"`
				Content json.RawMessage `json:"content"`
			}
			if err := json.Unmarshal(captured[key], &messages); err != nil {
				t.Fatal(err)
			}
			seen := false
			for _, m := range messages {
				if strings.Contains(string(m.Content), "SYNTHETIC_WAKEUP_NOT_AUTHORITY") {
					seen = true
					if m.Role != "user" {
						t.Fatalf("wakeup gained privileged role %q", m.Role)
					}
				}
			}
			if !seen {
				t.Fatal("scheduled input absent from actual wire")
			}
			if strings.Contains(string(captured["system"]), "SYNTHETIC_WAKEUP") || strings.Contains(string(captured["instructions"]), "SYNTHETIC_WAKEUP") {
				t.Fatal("scheduled prompt promoted into policy")
			}
		})
	}
}
