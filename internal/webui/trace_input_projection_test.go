package webui

import (
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/internal/llm"
	rtpkg "github.com/Ricardo-M-L/metis/internal/runtime"
	"github.com/Ricardo-M-L/metis/internal/session"
)

func TestTraceHistoryProjectionDoesNotTurnToolsOrContextIntoUsers(t *testing.T) {
	s, store := testServer(t)
	id := "history-context-projection"
	if err := store.WriteHeader(id, "fixture", ""); err != nil {
		t.Fatal(err)
	}
	for _, msg := range []llm.Message{
		{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text", Text: "question"}}},
		{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: "tool_use", ToolName: "Read", ToolUseID: "call"}}},
		{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "tool_result", ToolUseID: "call", ToolResult: "output"}}},
		{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text", Text: "<sub_agent_idle>worker completed</sub_agent_idle>"}}},
		{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: "text", Text: "done"}}},
	} {
		if err := store.AppendMessage(id, msg); err != nil {
			t.Fatal(err)
		}
	}
	users, contexts := 0, 0
	for _, node := range traceFromHistory(s, id) {
		if node.Event.Turn != 1 || !node.Event.TS.IsZero() {
			t.Fatalf("fake turn/timestamp: %#v", node.Event)
		}
		if node.Event.Kind == "user" {
			users++
		}
		if node.Event.Kind == "context" {
			contexts++
		}
	}
	if users != 1 || contexts != 1 {
		t.Fatalf("users=%d contexts=%d", users, contexts)
	}
}

func TestTraceAPIBackfillsInputWithoutFabricatingTiming(t *testing.T) {
	old := rtpkg.CurrentTraceAdapter()
	rtpkg.InstallTrace(t.TempDir())
	t.Cleanup(func() { _ = rtpkg.CurrentTraceStore().Close(); rtpkg.SetTraceAdapter(old) })
	s, store := testServer(t)
	id := "partial-input-api"
	if err := store.WriteHeader(id, "fixture", ""); err != nil {
		t.Fatal(err)
	}
	for _, msg := range []llm.Message{
		{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text", Text: "original question"}}},
		{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: "text", Text: "distinct original answer"}}},
	} {
		if err := store.AppendMessage(id, msg); err != nil {
			t.Fatal(err)
		}
	}
	ts := time.Date(2026, 9, 14, 1, 0, 0, 0, time.UTC)
	for i, kind := range []string{"text", "loop_done"} {
		err := rtpkg.CurrentTraceStore().Append(&session.TraceEvent{ID: kind, SessionID: id, Turn: 1, Kind: kind, Text: "distinct original answer", TS: ts.Add(time.Duration(i) * time.Second)})
		if err != nil {
			t.Fatal(err)
		}
	}
	rr := httptest.NewRecorder()
	s.handler().ServeHTTP(rr, httptest.NewRequest("GET", "/api/trace?sessionId="+id, nil))
	var result struct {
		Events []traceEventView `json:"events"`
		Stats  traceStats       `json:"stats"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Events) != 3 || result.Events[0].Kind != "user" || result.Events[0].TS != "" || result.Events[0].Source != "history-reconstructed" {
		t.Fatalf("missing/incorrect recovered input: %s", rr.Body.String())
	}
	if result.Events[1].ID != "text" || result.Stats.DurationMs != 1000 || result.Stats.TtftAverageMs != 0 {
		t.Fatalf("changed real events or timing: %s", rr.Body.String())
	}
}
