package webui

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	rtpkg "github.com/Ricardo-M-L/metis/internal/runtime"
	"github.com/Ricardo-M-L/metis/internal/session"
	"github.com/Ricardo-M-L/metis/internal/tasks"
)

func TestPlanningStatusHidesPriorCompletedTasksWithoutChangingStores(t *testing.T) {
	home := t.TempDir()
	t.Setenv("METIS_HOME", home)
	previous := rtpkg.CurrentTraceAdapter()
	defer rtpkg.SetTraceAdapter(previous)
	trace, err := session.NewTraceStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer trace.Close()
	rtpkg.SetTraceAdapter(rtpkg.NewTraceAdapter(trace))
	s, _ := testServer(t)
	const sid = "planning-current"
	s.activeSessionID = sid
	start := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	before, after := start.Add(-time.Minute), start.Add(time.Minute)
	todos := []tasks.Item{
		{ID: "old", Content: "Previous completed task", Status: "completed", CreatedAt: before, UpdatedAt: before},
		{ID: "active", Content: "Unfinished task", Status: "in_progress", CreatedAt: before, UpdatedAt: before},
		{ID: "new", Content: "Current completed task", Status: "completed", CreatedAt: start, UpdatedAt: after},
		{ID: "legacy", Content: "Legacy completed task", Status: "completed"},
		{ID: "created", Content: "Created this turn", Status: "completed", CreatedAt: start},
	}
	if err := tasks.Save(&tasks.List{SessionID: sid, Items: todos}); err != nil {
		t.Fatal(err)
	}
	structured := []*tasks.Task{
		{ID: "1", Subject: "Previous structured task", Status: tasks.TaskCompleted, CreatedAt: before, UpdatedAt: before},
		{ID: "2", Subject: "Current structured task", Status: tasks.TaskCompleted, CreatedAt: start, UpdatedAt: after},
	}
	raw, err := json.Marshal(structured)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(home, "sessions", sid, "tasks-structured.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	for _, ev := range []session.TraceEvent{
		{SessionID: sid, Turn: 1, Kind: "user", TS: before.Add(-time.Minute)},
		{SessionID: sid, Turn: 2, Kind: "user", TS: start},
		// Internal context arriving later in the same task must not move its boundary.
		{SessionID: sid, Turn: 2, Kind: "context", Source: "peer", TS: after.Add(time.Minute)},
	} {
		if err := trace.Append(&ev); err != nil {
			t.Fatal(err)
		}
	}
	read := func() struct {
		ActiveSessionID string       `json:"activeSessionId"`
		PlanItems       []tasks.Item `json:"planItems"`
		PlanTurn        int          `json:"planTurn"`
		PlanStartedAt   string       `json:"planTurnStartedAt"`
	} {
		t.Helper()
		rr := httptest.NewRecorder()
		s.handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/status?sessionId="+sid, nil))
		if rr.Code != http.StatusOK {
			t.Fatalf("status=%d: %s", rr.Code, rr.Body.String())
		}
		var payload struct {
			ActiveSessionID string       `json:"activeSessionId"`
			PlanItems       []tasks.Item `json:"planItems"`
			PlanTurn        int          `json:"planTurn"`
			PlanStartedAt   string       `json:"planTurnStartedAt"`
		}
		if err := json.Unmarshal(rr.Body.Bytes(), &payload); err != nil {
			t.Fatal(err)
		}
		return payload
	}
	payload := read()
	var got []string
	for _, item := range payload.PlanItems {
		got = append(got, item.Content)
	}
	want := []string{"Unfinished task", "Current completed task", "Created this turn", "Current structured task"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("current plan=%v; want %v (prior completed tasks must stay out of the new turn)", got, want)
	}
	if payload.ActiveSessionID != sid || payload.PlanTurn != 2 || payload.PlanStartedAt != start.Format(time.RFC3339Nano) {
		t.Fatalf("plan owner/boundary = %+v", payload)
	}
	saved, err := tasks.Load(sid)
	if err != nil || !reflect.DeepEqual(saved.Items, todos) {
		t.Fatalf("display projection mutated saved todo list: %+v, %v", saved, err)
	}
	if got := tasks.TaskStoreForSession(sid).List(false); !reflect.DeepEqual(got, structured) {
		t.Fatalf("display projection mutated structured tasks: %+v", got)
	}
}

func TestPlanningStatusRefreshesWorkerInputBoundary(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	previous := rtpkg.CurrentTraceAdapter()
	defer rtpkg.SetTraceAdapter(previous)
	dir := t.TempDir()
	writer, err := session.NewTraceStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	reader, err := session.NewTraceStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	rtpkg.SetTraceAdapter(rtpkg.NewTraceAdapter(reader))
	const sid = "worker-planning"
	start := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	if err := tasks.Save(&tasks.List{SessionID: sid, Items: []tasks.Item{{
		ID: "done", Content: "Finished first task", Status: "completed", UpdatedAt: start.Add(time.Second),
	}}}); err != nil {
		t.Fatal(err)
	}
	appendInput := func(id string, turn int, at time.Time) {
		t.Helper()
		if err := writer.Append(&session.TraceEvent{SessionID: id, Turn: turn, Kind: "user", TS: at}); err != nil {
			t.Fatal(err)
		}
		if err := writer.SyncSession(id); err != nil {
			t.Fatal(err)
		}
	}
	appendInput(sid, 1, start)
	if items, turn, _ := planningStatus(sid); len(items) != 1 || turn != 1 {
		t.Fatalf("first task plan=%+v turn=%d", items, turn)
	}
	appendInput(sid, 2, start.Add(time.Minute))
	appendInput("unrelated-worker", 99, start.Add(time.Hour))
	if items, turn, at := planningStatus(sid); len(items) != 0 || turn != 2 || at != start.Add(time.Minute).Format(time.RFC3339Nano) {
		t.Fatalf("worker's next input was not imported or owner was mixed: items=%+v turn=%d at=%q", items, turn, at)
	}
}

func TestPlanningStatusWithoutInputBoundaryPreservesLegacyProjection(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	previous := rtpkg.CurrentTraceAdapter()
	defer rtpkg.SetTraceAdapter(previous)
	const sid = "legacy-planning"
	items := []tasks.Item{{ID: "legacy", Content: "Legacy completed task", Status: "completed"}}
	if err := tasks.Save(&tasks.List{SessionID: sid, Items: items}); err != nil {
		t.Fatal(err)
	}
	trace, err := session.NewTraceStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer trace.Close()
	for _, adapter := range []*rtpkg.TraceAdapter{nil, rtpkg.NewTraceAdapter(trace)} {
		rtpkg.SetTraceAdapter(adapter)
		got, turn, at := planningStatus(sid)
		if !reflect.DeepEqual(got, items) || turn != 0 || at != "" {
			t.Fatalf("no reliable input boundary changed legacy plan: %+v turn=%d at=%q", got, turn, at)
		}
	}
}

func TestPlanningStatusReadsStructuredWorkerUpdatesWithoutReplacingCachedStore(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	previous := rtpkg.CurrentTraceAdapter()
	defer rtpkg.SetTraceAdapter(previous)
	trace, err := session.NewTraceStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer trace.Close()
	rtpkg.SetTraceAdapter(rtpkg.NewTraceAdapter(trace))
	const sid = "planning-worker-structured"
	oldWriter := tasks.NewTaskStore(sid)
	completed := tasks.TaskCompleted
	for _, subject := range []string{"Previous inspect", "Previous implement", "Previous verify"} {
		item, err := oldWriter.Create(subject, "", "", nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := oldWriter.Update(item.ID, tasks.TaskPatch{Status: &completed}); err != nil {
			t.Fatal(err)
		}
	}
	parentCache := tasks.TaskStoreForSession(sid)
	if got := parentCache.List(false); len(got) != 3 {
		t.Fatalf("parent cache did not contain previous task list: %+v", got)
	}
	inputAt := time.Now()
	if err := trace.Append(&session.TraceEvent{SessionID: sid, Turn: 2, Kind: "user", TS: inputAt}); err != nil {
		t.Fatal(err)
	}
	newWriter := tasks.NewTaskStore(sid)
	current, err := newWriter.Create("New task from worker", "", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	assertCurrent := func(status string) {
		t.Helper()
		items, turn, _ := planningStatus(sid)
		if turn != 2 || len(items) != 1 || items[0].Content != current.Subject || items[0].Status != status {
			t.Fatalf("parent status missed worker update: items=%+v turn=%d want status=%s", items, turn, status)
		}
		if got := parentCache.List(false); len(got) != 3 {
			t.Fatalf("read-only display replaced the runtime writer cache: %+v", got)
		}
	}
	assertCurrent("pending")
	if _, err := newWriter.Update(current.ID, tasks.TaskPatch{Status: &completed}); err != nil {
		t.Fatal(err)
	}
	assertCurrent("completed")
}
