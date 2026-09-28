package webui

import (
	"encoding/json"
	"fmt"
	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/desktopipc"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
)

func TestStatusRosterOmitsOtherSessionAndRetainsFinishedOwner(t *testing.T) {
	s, store := testServer(t)
	roster := agent.NewRoster(4)
	s.roster = roster
	for _, id := range []string{"a", "b"} {
		teammate := &agent.Teammate{Name: "child-" + id, AgentID: "agt-" + id}
		if err := roster.Register(teammate); err != nil {
			t.Fatal(err)
		}
		transcript, err := agent.NewSubAgentTranscript(store.Dir, teammate.AgentID,
			agent.NewSubAgentHeader(teammate.AgentID, "model", id, teammate.Name, t.TempDir(), "default"))
		if err != nil {
			t.Fatal(err)
		}
		if err := transcript.Close(); err != nil {
			t.Fatal(err)
		}
		if id == "a" {
			teammate.Finish(agent.StatusCompleted, "done", nil, "end_turn")
			roster.UnregisterTeammate(teammate)
		}
	}
	s.stateMu.Lock()
	s.activeSessionID = "a"
	s.stateMu.Unlock()
	rr := httptest.NewRecorder()
	s.handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/status", nil))
	var status struct {
		SubAgents int `json:"subAgents"`
		Agents    []struct {
			ID        string `json:"agentId"`
			SessionID string `json:"sessionId"`
			Status    string `json:"status"`
		} `json:"agents"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	if status.SubAgents != 0 || len(status.Agents) != 1 || status.Agents[0].ID != "agt-a" || status.Agents[0].SessionID != "a" || status.Agents[0].Status != "completed" {
		t.Fatalf("session-scoped roster status = %s", rr.Body.String())
	}
	viewed := httptest.NewRecorder()
	s.handler().ServeHTTP(viewed, httptest.NewRequest(http.MethodGet, "/api/status?sessionId=a", nil))
	var scoped struct {
		ViewRoster struct {
			SessionID string `json:"sessionId"`
			Agents    []struct {
				ID string `json:"agentId"`
			} `json:"agents"`
		} `json:"viewRoster"`
	}
	if err := json.Unmarshal(viewed.Body.Bytes(), &scoped); err != nil {
		t.Fatal(err)
	}
	if scoped.ViewRoster.SessionID != "a" || len(scoped.ViewRoster.Agents) != 1 || scoped.ViewRoster.Agents[0].ID != "agt-a" {
		t.Fatalf("legacy viewed roster lost its own child: %s", viewed.Body.String())
	}
}

func TestWorkerSnapshotStatusFollowsSelectedSession(t *testing.T) {
	s, _ := testServer(t)
	for _, id := range []string{"a", "b"} {
		s.setWorkerSnapshot(id, desktopipc.Status{SubAgents: 1, NamedAgents: 1, Agents: []desktopipc.Subagent{{Name: "child-" + id, AgentID: "child-" + id, Status: "running", Output: "output-" + id}}})
	}
	for _, id := range []string{"a", "b"} {
		s.stateMu.Lock()
		s.activeSessionID = id
		s.stateMu.Unlock()
		rr := httptest.NewRecorder()
		s.handler().ServeHTTP(rr, httptest.NewRequest("GET", "/api/status", nil))
		var status struct {
			SubAgents int `json:"subAgents"`
			Agents    []struct {
				ID        string `json:"agentId"`
				SessionID string `json:"sessionId"`
			} `json:"agents"`
		}
		if err := json.Unmarshal(rr.Body.Bytes(), &status); err != nil {
			t.Fatal(err)
		}
		if status.SubAgents != 1 || len(status.Agents) != 1 || status.Agents[0].ID != "child-"+id || status.Agents[0].SessionID != id {
			t.Fatalf("wrong session status: %s", rr.Body.String())
		}
	}
	view, ok := s.subAgentDetailView("a", "child-a")
	if !ok || view.Output != "output-a" {
		t.Fatalf("background session detail missing: %+v", view)
	}
	if view.SessionID != "a" {
		t.Fatalf("worker detail lost parent session: %+v", view)
	}
	if _, ok := s.subAgentDetailView("b", "child-a"); ok {
		t.Fatal("worker detail leaked into another session")
	}
	s.setWorkerSnapshot("a", desktopipc.Status{Agents: []desktopipc.Subagent{{AgentID: "child-a", Status: "completed", Output: "complete-a", Result: "done"}}})
	view, ok = s.subAgentDetailView("a", "child-a")
	if !ok || view.Status != "completed" || view.Result != "done" {
		t.Fatalf("finished worker detail missing: %+v", view)
	}
}

func TestStatusViewRosterReadsRequestedSessionWithoutChangingActiveStatus(t *testing.T) {
	s, _ := testServer(t)
	s.stateMu.Lock()
	s.activeSessionID = "session-a"
	s.stateMu.Unlock()
	s.setWorkerSnapshot("session-a", desktopipc.Status{
		SubAgents: 1, BackgroundTasks: 2,
		Agents: []desktopipc.Subagent{{AgentID: "agt-a", Name: "alpha", Status: "running"}},
	})
	s.setWorkerSnapshot("session-b", desktopipc.Status{
		SubAgents: 2, BackgroundTasks: 3,
		Agents: []desktopipc.Subagent{{AgentID: "agt-b", Name: "beta", Status: "running"}},
		Jobs:   []desktopipc.Job{{ID: "job-b", Description: "build", Status: "running"}},
	})
	read := func(path string) map[string]any {
		t.Helper()
		rr := httptest.NewRecorder()
		s.handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, path, nil))
		if rr.Code != http.StatusOK {
			t.Fatalf("GET %s = %d: %s", path, rr.Code, rr.Body.String())
		}
		var body map[string]any
		if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		return body
	}
	active := read("/api/status")
	viewingB := read("/api/status?sessionId=session-b")
	for _, key := range []string{"activeSessionId", "subAgents", "backgroundTasks", "agents", "jobs", "contextUsed", "contextWindow", "turnRunning"} {
		if !reflect.DeepEqual(viewingB[key], active[key]) {
			t.Fatalf("query changed active status key %s: active=%#v viewed=%#v", key, active[key], viewingB[key])
		}
	}
	view, ok := viewingB["viewRoster"].(map[string]any)
	if !ok || view["sessionId"] != "session-b" || view["subAgents"] != float64(2) || view["backgroundTasks"] != float64(3) {
		t.Fatalf("wrong viewed roster: %#v", viewingB["viewRoster"])
	}
	agents, _ := view["agents"].([]any)
	jobs, _ := view["jobs"].([]any)
	if len(agents) != 1 || agents[0].(map[string]any)["agentId"] != "agt-b" || agents[0].(map[string]any)["sessionId"] != "session-b" ||
		len(jobs) != 1 || jobs[0].(map[string]any)["id"] != "job-b" {
		t.Fatalf("view roster leaked or lost worker rows: %#v", view)
	}
	empty := read("/api/status?sessionId=session-c")["viewRoster"].(map[string]any)
	if empty["sessionId"] != "session-c" || empty["subAgents"] != float64(0) || empty["backgroundTasks"] != float64(0) ||
		len(empty["agents"].([]any)) != 0 || len(empty["jobs"].([]any)) != 0 {
		t.Fatalf("missing worker session must have empty roster: %#v", empty)
	}
}

func TestWorkerSnapshotRetentionProtectsActiveWorkers(t *testing.T) {
	s, _ := testServer(t)
	s.workerTurns["active"] = &isolatedActiveTurn{}
	s.setWorkerSnapshot("active", desktopipc.Status{})
	for i := 0; i < 32; i++ {
		s.setWorkerSnapshot(fmt.Sprint(i), desktopipc.Status{})
	}
	if len(s.workerSnapshots) != 16 {
		t.Fatalf("unbounded snapshots: %d", len(s.workerSnapshots))
	}
	if _, ok := s.workerSnapshot("active"); !ok {
		t.Fatal("active worker was evicted")
	}
	if _, ok := s.workerSnapshot("0"); ok {
		t.Fatal("oldest completed worker not evicted")
	}
}
