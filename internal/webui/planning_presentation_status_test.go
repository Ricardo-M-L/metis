package webui

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	rtpkg "github.com/Ricardo-M-L/metis/internal/runtime"
	"github.com/Ricardo-M-L/metis/internal/tasks"
)

func TestPlanningStatusSeparatesMainStepsAndExecution(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	previous := rtpkg.CurrentTraceAdapter()
	defer rtpkg.SetTraceAdapter(previous)
	rtpkg.SetTraceAdapter(nil)
	s, _ := testServer(t)
	const sid = "status-grouped-execution"
	s.activeSessionID = sid
	plan, err := tasks.BeginUserWork(sid)
	if err != nil {
		t.Fatal(err)
	}
	if err := tasks.Save(&tasks.List{SessionID: sid, PlanID: plan, Items: []tasks.Item{
		{ID: "first", Content: "Inspect primary schema", Status: "completed"},
		{ID: "second", Content: "Compare primary design", Status: "in_progress"},
		{ID: "third", Content: "Review primary proposal", Status: "pending"},
	}}); err != nil {
		t.Fatal(err)
	}
	if _, err := tasks.TaskStoreForSession(sid).CreateOwnedInPlan(plan, "Protocol inspection", "", "", "verifier", nil); err != nil {
		t.Fatal(err)
	}
	rr := httptest.NewRecorder()
	s.handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/status?sessionId="+sid, nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d: %s", rr.Code, rr.Body.String())
	}
	var payload struct {
		PlanItems      []tasks.Item `json:"planItems"`
		PlanSteps      []tasks.Item `json:"planSteps"`
		ExecutionItems []tasks.Item `json:"executionItems"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.PlanItems) != 4 || len(payload.PlanSteps) != 3 || len(payload.ExecutionItems) != 1 || payload.ExecutionItems[0].Owner != "verifier" {
		t.Fatalf("execution was flattened or lost: %+v", payload)
	}
	for _, name := range []string{"", "empty-status-plan"} {
		view, _, _ := planningStatusPresentation(name)
		if view.Steps == nil || view.ExecutionItems == nil || len(view.Steps)+len(view.ExecutionItems) != 0 {
			t.Fatalf("empty projection must expose explicit empty groups: %+v", view)
		}
	}
}
