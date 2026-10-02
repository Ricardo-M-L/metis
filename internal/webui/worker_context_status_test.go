package webui

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/desktopipc"
	"github.com/Ricardo-M-L/metis/internal/llm"
)

func TestStatusDropsOldWorkerContextAfterSameSessionActivation(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	s, loop, _, _, sid := modelSwitchFixture(t, t.TempDir())
	s.setWorkerSnapshot(sid, desktopipc.Status{Context: &desktopipc.ContextPressure{Used: 48_000, Window: 128_000}})
	if err := s.store.AppendMessage(sid, llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text", Text: "new image turn"}}}); err != nil {
		t.Fatal(err)
	}
	header, history, err := s.store.Load(sid)
	if err != nil {
		t.Fatal(err)
	}
	// Image turns run on the parent Loop after activating their transcript.
	if err := s.activateSession(sid, header, history); err != nil {
		t.Fatal(err)
	}
	loop.AppendUser("new in-process prompt")
	rr := httptest.NewRecorder()
	s.handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/status?sessionId="+sid, nil))
	var payload map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload["contextSessionId"] != sid || payload["contextUsed"] != float64(loop.EstimateContextTokens()) {
		t.Fatalf("old worker reading obscured the current parent Loop: %#v", payload)
	}
}

func TestModelSwitchInvalidatesOnlySelectedWorkerContext(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	s, _, _, _, sid := modelSwitchFixture(t, t.TempDir())
	old := desktopipc.Status{Context: &desktopipc.ContextPressure{Used: 48_000, Window: 128_000}, Agents: []desktopipc.Subagent{{AgentID: "keep-agent", Status: "completed"}}}
	s.setWorkerSnapshot(sid, old)
	s.setWorkerSnapshot("other-session", old)
	// A completed isolated turn leaves the parent's history unowned/stale.
	s.contextMu.Lock()
	s.contextSessionID = ""
	s.contextMu.Unlock()
	rr := httptest.NewRecorder()
	s.handler().ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/models", bytes.NewBufferString(`{"provider":"anthropic","model":"claude-new"}`)))
	if rr.Code != http.StatusOK {
		t.Fatalf("model switch %d: %s", rr.Code, rr.Body.String())
	}
	selected, _ := s.workerSnapshot(sid)
	other, _ := s.workerSnapshot("other-session")
	if selected.Context != nil || len(selected.Agents) != 1 || other.Context == nil {
		t.Fatalf("incorrect context invalidation: selected=%+v other=%+v", selected, other)
	}
	status := httptest.NewRecorder()
	s.handler().ServeHTTP(status, httptest.NewRequest(http.MethodGet, "/api/status?sessionId="+sid, nil))
	var payload map[string]any
	if err := json.Unmarshal(status.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload["contextSessionId"] != nil {
		t.Fatalf("model switch kept obsolete worker context: %#v", payload)
	}
}

func TestStatusUsesViewedWorkerContextAndRetainsFinalSnapshot(t *testing.T) {
	s, _ := testServer(t)
	s.stateMu.Lock()
	s.activeSessionID = "session-a"
	s.stateMu.Unlock()
	s.setWorkerSnapshot("session-b", desktopipc.Status{Context: &desktopipc.ContextPressure{
		Used: 48_000, Window: 128_000, CompactThreshold: 0.8, CompactAtTokens: 100_000,
	}})
	read := func(path string) map[string]any {
		t.Helper()
		rr := httptest.NewRecorder()
		s.handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, path, nil))
		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d: %s", rr.Code, rr.Body.String())
		}
		var payload map[string]any
		if err := json.Unmarshal(rr.Body.Bytes(), &payload); err != nil {
			t.Fatal(err)
		}
		return payload
	}
	viewB := read("/api/status?sessionId=session-b")
	if viewB["contextSessionId"] != "session-b" || viewB["contextUsed"] != float64(48_000) ||
		viewB["contextWindow"] != float64(128_000) || viewB["compactAtTokens"] != float64(100_000) {
		t.Fatalf("viewed worker pressure missing: %#v", viewB)
	}
	viewA := read("/api/status?sessionId=session-a")
	if viewA["contextSessionId"] == "session-b" {
		t.Fatalf("worker B pressure leaked into A: %#v", viewA)
	}
	s.stateMu.Lock()
	s.activeSessionID = "session-b"
	s.stateMu.Unlock()
	activeB := read("/api/status")
	if activeB["contextSessionId"] != "session-b" || activeB["contextUsed"] != float64(48_000) {
		t.Fatalf("active worker pressure missing without a view query: %#v", activeB)
	}
	s.finishWorkerSnapshot("session-b", false, false)
	finalB := read("/api/status?sessionId=session-b")
	if finalB["contextSessionId"] != "session-b" || finalB["contextUsed"] != float64(48_000) {
		t.Fatalf("completed worker pressure was lost: %#v", finalB)
	}
}
