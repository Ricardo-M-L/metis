package webui

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/desktopipc"
)

func TestSubAgentWorkerContinuationMetadata(t *testing.T) {
	s, _ := testServer(t)
	s.setWorkerSnapshot("parent-worker", desktopipc.Status{SubAgents: 1, Agents: []desktopipc.Subagent{{
		Name: "战略路线", AgentID: "agt-continuing", Status: "running", ExecutionPhase: "continuing", ContinuationAttempts: 2,
	}}})
	get := func(path string) map[string]any {
		t.Helper()
		rr := httptest.NewRecorder()
		s.handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, path, nil))
		var body map[string]any
		if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if rr.Code != http.StatusOK {
			t.Fatalf("GET %s: %d %#v", path, rr.Code, body)
		}
		return body
	}
	status := get("/api/status?sessionId=parent-worker")
	roster := status["viewRoster"].(map[string]any)["agents"].([]any)[0].(map[string]any)
	detail := get("/api/subagents/agt-continuing?sessionId=parent-worker")["agent"].(map[string]any)
	for _, item := range []map[string]any{roster, detail} {
		if item["executionPhase"] != "continuing" || item["continuationAttempts"] != float64(2) || item["name"] != "战略路线" {
			t.Fatalf("worker continuation state lost: %#v", item)
		}
	}
	view, ok := s.subAgentDetailView("parent-worker", "agt-continuing")
	if !ok {
		t.Fatal("worker detail not available")
	}
	delta := subAgentDeltaPayload(view, "continued text")
	if delta["continuationAttempts"] != 2 || delta["executionPhase"] != "continuing" {
		t.Fatalf("SSE delta discarded continuation state: %#v", delta)
	}
}
