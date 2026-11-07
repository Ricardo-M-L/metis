package tasks

import (
	"context"
	"os"
	"reflect"
	"testing"
)

func TestNewTodoPlanDoesNotResurrectOldStructuredTasks(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	const sid = "new-plan-isolation"
	store := TaskStoreForSession(sid)
	old, err := store.CreateOwned("Audit the old cache task", "", "", "old-worker", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ReplaceAll(sid, []Item{{Content: old.Subject, Status: "completed"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := ReplaceAll(sid, []Item{{Content: "Fix the new wakeup task", Status: "pending"}}); err != nil {
		t.Fatal(err)
	}
	items, err := PlanningItems(sid)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].Content != "Fix the new wakeup task" {
		t.Fatalf("a replaced plan resurrected old structured tasks: %+v", items)
	}
	prior, ok := store.Get(old.ID)
	if !ok || prior.Status != TaskPending {
		t.Fatalf("old task must remain inspectable without fabricated completion: %+v", prior)
	}
	visible, err := PlanningItemsSnapshot(sid)
	if err != nil || len(visible) != 1 || visible[0].Content != items[0].Content {
		t.Fatalf("cross-process projection differs: %+v %v", visible, err)
	}
}

func TestFailedStructuredUpdateMustNotArchivePendingDiskTask(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	const sid = "failed-completion-plan"
	plan, err := BeginUserWork(sid)
	if err != nil {
		t.Fatal(err)
	}
	store := TaskStoreForSession(sid)
	task, err := store.CreateOwnedInPlan(plan, "Still pending on disk", "", "", "", map[string]any{"nested": map[string]any{"keep": true}})
	if err != nil {
		t.Fatal(err)
	}
	before, _ := store.Get(task.ID)
	if err := os.Mkdir(taskFilePath(sid)+".tmp", 0o700); err != nil {
		t.Fatal(err)
	}
	status := TaskCompleted
	if _, err := store.Update(task.ID, TaskPatch{Status: &status, Metadata: map[string]any{"nested": map[string]any{"changed": true}, "new": true}, AddBlocks: []string{"other"}}); err == nil {
		t.Fatal("expected failed persistence")
	}
	got, _ := store.Get(task.ID)
	if !reflect.DeepEqual(got, before) {
		t.Fatalf("failed update changed cached task: before=%+v after=%+v", before, got)
	}
	disk, ok := NewTaskStore(sid).Get(task.ID)
	if !ok || disk.Status != TaskPending {
		t.Fatalf("disk task changed: %+v", disk)
	}
	if next, err := BeginUserWork(sid); err != nil || next != plan {
		t.Fatalf("failed completion archived unfinished work: next=%s err=%v", next, err)
	}
}

func TestPlanBoundaryArchivesCompletedLegacyWithoutClosingUnrelatedTask(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	const sid = "legacy-completion"
	store := TaskStoreForSession(sid)
	old, err := store.Create("Unfinished older review", "", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := Save(&List{SessionID: sid, Items: []Item{{Content: "Finished current work", Status: "completed"}}}); err != nil {
		t.Fatal(err)
	}
	id, err := BeginUserWork(sid)
	if err != nil {
		t.Fatal(err)
	}
	items, err := PlanningItems(sid)
	if err != nil || len(items) != 0 {
		t.Fatalf("new work inherited legacy rows: %+v %v", items, err)
	}
	if id == "" {
		t.Fatal("no plan id")
	}
	prior, ok := store.Get(old.ID)
	if !ok || prior.Status != TaskPending {
		t.Fatal("archiving fabricated completion")
	}
	history, err := PlanHistory(sid)
	if err != nil || len(history) != 1 || len(history[0].Items) != 2 {
		t.Fatalf("legacy history lost: %+v %v", history, err)
	}
}

func TestFollowupKeepsPlanAndFrozenChildCannotPolluteNewPlan(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	const sid = "child-plan"
	plan, err := BeginUserWork(sid)
	if err != nil {
		t.Fatal(err)
	}
	ctx := WithPlanID(WithSessionID(context.Background(), sid), plan)
	if _, err := ReplaceAllForContext(ctx, []Item{{Content: "Parent current task", Status: "in_progress"}}, false); err != nil {
		t.Fatal(err)
	}
	if next, err := BeginUserWork(sid); err != nil || next != plan {
		t.Fatalf("follow-up reset unfinished work: %s %v", next, err)
	}
	child := FreezePlanContext(ctx)
	store := TaskStoreForSession(sid)
	task, err := store.CreateOwnedInPlan(plan, "Child investigation", "", "", "child", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ReplaceAllForContext(ctx, []Item{{Content: "New unrelated task", Status: "pending"}}, true); err != nil {
		t.Fatal(err)
	}
	newPlan := PlanIDFromContext(ctx)
	if newPlan == plan {
		t.Fatal("root binding did not switch")
	}
	if PlanIDFromContext(child) != plan {
		t.Fatal("child binding followed parent")
	}
	if _, err := UpsertForContext(child, Item{Content: "Late child result", Status: "completed"}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Update(task.ID, TaskPatch{}); err != nil {
		t.Fatal(err)
	}
	items, err := PlanningItems(sid)
	if err != nil || len(items) != 1 || items[0].Content != "New unrelated task" {
		t.Fatalf("old child polluted current plan: %+v %v", items, err)
	}
	old, err := PlanningItemsForContext(child)
	if err != nil || len(old) < 2 {
		t.Fatalf("child lost historical plan: %+v %v", old, err)
	}
	if _, err := ReplaceAllForContext(child, []Item{{Content: "Force another root plan"}}, true); err == nil {
		t.Fatal("child started a new root plan")
	}
	if current, _ := CurrentPlanID(sid); current != newPlan {
		t.Fatal("child reactivated old plan")
	}
	snapshot, err := PlanningItemsSnapshot(sid)
	if err != nil || !reflect.DeepEqual(snapshot, items) {
		t.Fatalf("disk projection differs: %+v %v", snapshot, err)
	}
}

func TestEmptyTodoPlanArchivesStructuredWorkAndSameNamesCanStartNewScope(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	const sid = "empty-plan"
	if _, err := ReplaceAll(sid, []Item{{Content: "Run tests", Status: "completed"}}); err != nil {
		t.Fatal(err)
	}
	old, _ := CurrentPlanID(sid)
	ctx := WithPlanID(WithSessionID(context.Background(), sid), old)
	store := TaskStoreForSession(sid)
	prior, err := store.Create("Old extra step", "", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ReplaceAllForContext(ctx, []Item{{Content: "Run tests", Status: "pending"}}, true); err != nil {
		t.Fatal(err)
	}
	if PlanIDFromContext(ctx) == old {
		t.Fatal("explicit same-name new plan retained old scope")
	}
	if _, err := ReplaceAllForContext(ctx, nil, false); err != nil {
		t.Fatal(err)
	}
	items, err := PlanningItems(sid)
	if err != nil || len(items) != 0 {
		t.Fatalf("clear resurrected structured work: %+v %v", items, err)
	}
	if _, ok := store.Get(prior.ID); !ok {
		t.Fatal("clear deleted old task")
	}
}
