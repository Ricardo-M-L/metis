package builtin

import (
	"context"
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/tasks"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

func TestPlanSwitchesAreExclusiveWithinToolBatches(t *testing.T) {
	for _, in := range []map[string]any{{"todos": []any{}}, {"new_plan": true}, {"plan_id": "existing-plan"}} {
		if (Todo{}).Concurrency(in) != tools.ConcurrencyExclusive {
			t.Fatalf("scope-changing TodoWrite must serialize: %+v", in)
		}
	}
	if (TaskCreate{}).Concurrency(map[string]any{"new_plan": true}) != tools.ConcurrencyExclusive {
		t.Fatal("new-plan TaskCreate must serialize")
	}
	if (TaskCreate{}).Concurrency(nil) != tools.ConcurrencySafe || (Todo{}).Concurrency(map[string]any{"content": "one row", "status": "completed"}) != tools.ConcurrencySafe {
		t.Fatal("ordinary same-plan mutations lost safe batching")
	}
}

func TestTaskToolsCurrentPlanAndExplicitArchiveAccess(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	const sid = "tool-plan-scope"
	id, err := tasks.BeginUserWork(sid)
	if err != nil {
		t.Fatal(err)
	}
	ctx := tasks.WithPlanID(tasks.WithSessionID(context.Background(), sid), id)
	create := TaskCreate{}
	if _, err := create.Execute(ctx, map[string]any{"subject": "Old unfinished audit", "description": "old"}); err != nil {
		t.Fatal(err)
	}
	store := tasks.TaskStoreForSession(sid)
	old := store.List(false)[0]
	if _, err := create.Execute(ctx, map[string]any{"subject": "New wakeup work", "description": "new", "new_plan": true}); err != nil {
		t.Fatal(err)
	}
	if tasks.PlanIDFromContext(ctx) == id {
		t.Fatal("new TaskCreate plan did not update the parent scope")
	}
	list, err := (TaskList{}).Execute(ctx, map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(list.Output, old.Subject) || !strings.Contains(list.Output, "New wakeup work") {
		t.Fatalf("TaskList leaked history: %s", list.Output)
	}
	history, err := (TaskList{}).Execute(ctx, map[string]any{"includeArchived": true})
	if err != nil || !strings.Contains(history.Output, old.Subject) {
		t.Fatalf("history unavailable: %+v %v", history, err)
	}
	if _, err := (TaskUpdate{}).Execute(ctx, map[string]any{"taskId": old.ID, "status": "completed"}); err != nil {
		t.Fatal(err)
	}
	list, err = (TaskList{}).Execute(ctx, map[string]any{})
	if err != nil || strings.Contains(list.Output, old.Subject) {
		t.Fatalf("historical update reactivated task: %+v %v", list, err)
	}
	read, err := (TodoRead{}).Execute(ctx, map[string]any{"include_archived": true})
	if err != nil || !strings.Contains(read.Output, "Archived plan") || !strings.Contains(read.Output, old.Subject) {
		t.Fatalf("TodoRead lost archive: %+v %v", read, err)
	}
}

func TestTodoPlanExplicitResumeAndInvalidResumeDoesNotRotate(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	const sid = "todo-resume-plan"
	id, err := tasks.BeginUserWork(sid)
	if err != nil {
		t.Fatal(err)
	}
	ctx := tasks.WithPlanID(tasks.WithSessionID(context.Background(), sid), id)
	tool := Todo{}
	call := func(content string, newPlan bool) {
		t.Helper()
		res, err := tool.Execute(ctx, map[string]any{"todos": []any{map[string]any{"content": content, "status": "pending"}}, "new_plan": newPlan})
		if err != nil || res.IsError {
			t.Fatalf("write: %+v %v", res, err)
		}
	}
	call("Original unfinished step", false)
	call("Different new work", true)
	current := tasks.PlanIDFromContext(ctx)
	res, err := tool.Execute(ctx, map[string]any{"plan_id": id, "todos": []any{map[string]any{"status": "completed"}}})
	if err != nil || !res.IsError {
		t.Fatalf("invalid payload unexpectedly accepted: %+v %v", res, err)
	}
	if got, _ := tasks.CurrentPlanID(sid); got != current {
		t.Fatal("invalid resume changed active plan")
	}
	res, err = tool.Execute(ctx, map[string]any{"plan_id": id, "todos": []any{map[string]any{"content": "Original unfinished step", "status": "in_progress"}}})
	if err != nil || res.IsError {
		t.Fatalf("resume: %+v %v", res, err)
	}
	if got, _ := tasks.CurrentPlanID(sid); got != id || tasks.PlanIDFromContext(ctx) != id {
		t.Fatal("explicit resume did not activate the selected scope")
	}
	items, err := tasks.PlanningItems(sid)
	if err != nil || len(items) != 1 || items[0].Content != "Original unfinished step" {
		t.Fatalf("resume merged unrelated work: %+v %v", items, err)
	}
}
