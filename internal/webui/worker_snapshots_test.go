package webui

import (
	"encoding/json"
	"fmt"
	"github.com/Ricardo-M-L/metis/internal/desktopipc"
	"net/http/httptest"
	"testing"
)

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
				ID string `json:"agentId"`
			} `json:"agents"`
		}
		if err := json.Unmarshal(rr.Body.Bytes(), &status); err != nil {
			t.Fatal(err)
		}
		if status.SubAgents != 1 || len(status.Agents) != 1 || status.Agents[0].ID != "child-"+id {
			t.Fatalf("wrong session status: %s", rr.Body.String())
		}
	}
	view, ok := s.subAgentDetailView("child-a")
	if !ok || view.Output != "output-a" {
		t.Fatalf("background session detail missing: %+v", view)
	}
	s.setWorkerSnapshot("a", desktopipc.Status{Agents: []desktopipc.Subagent{{AgentID: "child-a", Status: "completed", Output: "complete-a", Result: "done"}}})
	view, ok = s.subAgentDetailView("child-a")
	if !ok || view.Status != "completed" || view.Result != "done" {
		t.Fatalf("finished worker detail missing: %+v", view)
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
