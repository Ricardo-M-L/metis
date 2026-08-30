package webui

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/session"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

func TestSessionHistoryResponsesUseRedactedPresentationCopy(t *testing.T) {
	store, err := session.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	provider := &activationTestProvider{name: "test-wire", model: "test-model"}
	loop := agent.NewLoop(provider, tools.NewRegistry(), permission.New(permission.ModeDefault), agent.NewHookRegistry(), "system", 2)
	loop.Model = "test-model"
	s := NewServer("127.0.0.1:0", loop, store, RuntimeBindings{
		ProviderName: "test-wire", FreshPermissionMode: permission.ModeDefault,
	})
	const sessionID = "history-presentation"
	secret := "ghp_" + strings.Repeat("w", 36)
	if err := store.WriteHeaderFull(session.Header{
		ID: sessionID, Provider: "test-wire", Model: "test-model", System: "system",
		Mode: string(permission.ModeDefault), Status: "idle",
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendMessage(sessionID, llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{
		{
			Type: "tool_use", ToolUseID: "tool-1", ToolName: "Bash",
			ToolInput: map[string]any{
				"api_key": "hunter2",
				"command": "curl https://example.test/?token=" + secret,
				"safe":    "keep",
			},
		},
	}}); err != nil {
		t.Fatal(err)
	}

	assertResponse := func(method, path string, body []byte) {
		t.Helper()
		rr := httptest.NewRecorder()
		s.handler().ServeHTTP(rr, httptest.NewRequest(method, path, bytes.NewReader(body)))
		if rr.Code != http.StatusOK {
			t.Fatalf("%s %s = %d: %s", method, path, rr.Code, rr.Body.String())
		}
		response := rr.Body.String()
		if strings.Contains(response, "hunter2") || strings.Contains(response, secret) {
			t.Fatalf("%s %s leaked raw tool input: %s", method, path, response)
		}
		if !strings.Contains(response, "[REDACTED]") || !strings.Contains(response, "keep") {
			t.Fatalf("%s %s lost redacted/safe values: %s", method, path, response)
		}
	}
	assertResponse(http.MethodGet, "/api/sessions/"+sessionID, nil)
	assertResponse(http.MethodPost, "/api/sessions/activate", []byte(`{"id":"`+sessionID+`"}`))

	// API presentation must not rewrite the canonical transcript needed by a
	// provider on the next resumed request.
	_, canonical, err := store.Load(sessionID)
	if err != nil {
		t.Fatal(err)
	}
	input := canonical[0].Content[0].ToolInput
	if input["api_key"] != "hunter2" || !strings.Contains(input["command"].(string), secret) {
		raw, _ := json.Marshal(canonical)
		t.Fatalf("canonical history changed: %s", raw)
	}
}

func TestTraceFromHistoryUsesRedactedToolArguments(t *testing.T) {
	s, store := testServer(t)
	const sessionID = "trace-history-presentation"
	secret := "ghp_" + strings.Repeat("z", 36)
	if err := store.WriteHeader(sessionID, "test-model", ""); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendMessage(sessionID, llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{
		{
			Type: "tool_use", ToolUseID: "tool-1", ToolName: "WebFetch",
			ToolInput: map[string]any{"password": "hunter2", "url": "https://example.test/?token=" + secret, "safe": "keep"},
		},
	}}); err != nil {
		t.Fatal(err)
	}
	nodes := traceFromHistory(s, sessionID)
	if len(nodes) != 1 {
		t.Fatalf("trace nodes = %#v", nodes)
	}
	text := nodes[0].Event.Text
	if strings.Contains(text, "hunter2") || strings.Contains(text, secret) || !strings.Contains(text, "[REDACTED]") || !strings.Contains(text, "keep") {
		t.Fatalf("trace presentation text = %q", text)
	}
}
