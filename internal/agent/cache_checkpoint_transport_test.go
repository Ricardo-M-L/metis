package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/llm/anthropic"
	"github.com/Ricardo-M-L/metis/internal/llm/openai"
)

// This regression asserts the desired wire contract, not the old probe's bug.
func TestPersistedCheckpointCrossTransportWire(t *testing.T) {
	var captured map[string]json.RawMessage
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "messages") {
			if r.Header.Get("x-api-key") != "fixture" {
				t.Error("Anthropic auth identity lost")
			}
		} else if r.Header.Get("Authorization") != "Bearer fixture" {
			t.Error("OpenAI auth identity lost")
		}
		captured = nil
		if err := json.NewDecoder(r.Body).Decode(&captured); err != nil {
			t.Errorf("decode: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(r.URL.Path, "chat/completions"):
			w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"}}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
		case strings.Contains(r.URL.Path, "responses"):
			w.Write([]byte(`{"id":"response-fixture","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}],"usage":{"input_tokens":1,"output_tokens":1}}`))
		default:
			w.Write([]byte(`{"id":"fixture","type":"message","role":"assistant","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`))
		}
	}))
	defer server.Close()
	state := RuntimeStateSnapshot{WorkingDirectory: "/obsolete-directory", PermissionMode: "plan"}
	p := openai.NewResponses("fixture", server.URL, "fixture", 64, 5*time.Second, 0)
	p.StateMode = openai.ResponsesStateLocal
	l := &Loop{Provider: p, System: "stable safety policy", SystemSections: []llm.SystemSection{{Name: "base", Body: "stable safety policy"}}, CurrentStateSnapshot: func() RuntimeStateSnapshot { return state }, Messages: []llm.Message{{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text", Text: "hello"}}}}}
	l.PlanSystemPrompt = "obsolete-plan-safety-overlay"
	l.SetPlanMode(true)
	first := l.buildRequest(nil)
	if _, err := p.Complete(context.Background(), first); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(captured["input"]), "obsolete-plan-safety-overlay") {
		t.Fatal("source wire omitted privileged plan checkpoint")
	}
	if len(first.Messages) != 2 || first.Messages[1].Role != llm.RoleSystem {
		t.Fatalf("missing source checkpoint: %#v", first.Messages)
	}
	history := l.History()
	state.WorkingDirectory = "/current-directory"
	state.PermissionMode = "default"
	l.SetPlanMode(false)
	a := anthropic.New("fixture", server.URL, "fixture", 64, 5*time.Second, "")
	chat := openai.New("fixture", server.URL, "fixture", 64, 5*time.Second, 0)
	for _, tc := range []struct {
		name                   string
		provider               llm.Provider
		messagesKey, systemKey string
	}{
		{"anthropic", a, "messages", "system"},
		{"chat", chat, "messages", ""},
		{"managed", func() llm.Provider { p.StateMode = openai.ResponsesStateProvider; return p }(), "input", "instructions"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l.RebindProviderModel(tc.provider, "fixture")
			req := l.buildRequest(nil)
			if _, err := tc.provider.Complete(context.Background(), req); err != nil {
				t.Fatal(err)
			}
			wire, _ := json.Marshal(captured)
			if strings.Contains(string(wire), "/obsolete-directory") || strings.Contains(string(wire), "obsolete-plan-safety-overlay") {
				t.Fatalf("obsolete context replayed: %s", wire)
			}
			if !strings.Contains(string(wire), "/current-directory") || !strings.Contains(string(wire), "stable safety policy") {
				t.Fatalf("current privileged policy missing: %s", wire)
			}
			var messages []struct {
				Role    string          `json:"role"`
				Content json.RawMessage `json:"content"`
			}
			if err := json.Unmarshal(captured[tc.messagesKey], &messages); err != nil {
				t.Fatal(err)
			}
			for _, m := range messages {
				if tc.name == "anthropic" && m.Role != "user" && m.Role != "assistant" {
					t.Fatalf("illegal Anthropic role: %s", m.Role)
				}
				if m.Role == "user" && strings.Contains(string(m.Content), "/current-directory") {
					t.Fatal("privileged context downgraded to user")
				}
			}
			if !reflect.DeepEqual(history, l.History()) {
				t.Fatal("request projection changed persisted history")
			}
		})
	}
	// Return to local mode through the execution wrapper: capability must remain
	// live and the latest complete snapshot must again be the final system item.
	p.StateMode = openai.ResponsesStateLocal
	wrapped := llm.BindExecutionProvider(p, context.Background())
	l.RebindProviderModel(wrapped, "fixture")
	req := l.buildRequest(nil)
	last := req.Messages[len(req.Messages)-1]
	if last.Role != llm.RoleSystem || !strings.Contains(last.Content[0].Text, "/current-directory") {
		t.Fatal("wrapper lost current checkpoint")
	}
	if _, err := wrapped.Complete(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(captured["input"]), "/current-directory") {
		t.Fatal("local wrapper wire lost checkpoint")
	}

	// Establish a real managed marker, switch back to local at identical state,
	// then back to managed: the old chain must reset exactly once.
	p.StateMode = openai.ResponsesStateProvider
	managed, err := wrapped.Complete(context.Background(), l.buildRequest(nil))
	if err != nil {
		t.Fatal(err)
	}
	l.Messages = append(l.Messages, llm.Message{Role: llm.RoleAssistant, Content: managed.Content})
	l.Messages = append(l.Messages, llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text", Text: "continue"}}})
	if _, err := wrapped.Complete(context.Background(), l.buildRequest(nil)); err != nil {
		t.Fatal(err)
	}
	if string(captured["previous_response_id"]) != `"response-fixture"` {
		t.Fatal("managed continuation was not retained")
	}
	p.StateMode = openai.ResponsesStateLocal
	local := l.buildRequest(nil)
	if local.Messages[len(local.Messages)-1].Role != llm.RoleSystem {
		t.Fatal("same-state local transition did not reconstruct snapshot")
	}
	p.StateMode = openai.ResponsesStateProvider
	renewed, err := wrapped.Complete(context.Background(), l.buildRequest(nil))
	if err != nil {
		t.Fatal(err)
	}
	if len(captured["previous_response_id"]) != 0 {
		t.Fatal("managed resumed stale chain after local transition")
	}
	l.Messages = append(l.Messages, llm.Message{Role: llm.RoleAssistant, Content: renewed.Content})
	l.Messages = append(l.Messages, llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text", Text: "new chain"}}})
	if _, err := wrapped.Complete(context.Background(), l.buildRequest(nil)); err != nil {
		t.Fatal(err)
	}
	if string(captured["previous_response_id"]) != `"response-fixture"` {
		t.Fatal("rebuilt managed chain could not continue")
	}
}
