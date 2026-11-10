package tasks

import (
	"bytes"
	"os"
	"reflect"
	"testing"
)

func TestPlanningPresentationSeparatesExecutionWithoutChangingCanonical(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	const sid = "presentation-mixed"
	plan, err := BeginUserWork(sid)
	if err != nil {
		t.Fatal(err)
	}
	if err := Save(&List{SessionID: sid, PlanID: plan, Items: []Item{
		{ID: "first", Content: "Inspect current schema", Status: "completed"},
		{ID: "second", Content: "Compare identity protocols", Status: "in_progress"},
		{ID: "third", Content: "Review migration proposal", Status: "pending"},
	}}); err != nil {
		t.Fatal(err)
	}
	store := TaskStoreForSession(sid)
	if _, err := store.CreateOwnedInPlan(plan, "Inspect protocol storage", "", "", "reviewer", nil); err != nil {
		t.Fatal(err)
	}
	before := map[string][]byte{}
	for _, file := range []string{path(sid), taskFilePath(sid)} {
		before[file], err = os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
	}
	for name, read := range map[string]func(string) (PlanPresentation, error){"runtime": PlanningPresentation, "snapshot": PlanningPresentationSnapshot} {
		t.Run(name, func(t *testing.T) {
			view, err := read(sid)
			if err != nil {
				t.Fatal(err)
			}
			if len(view.Steps) != 3 || len(view.ExecutionItems) != 1 || view.ExecutionItems[0].Content != "Inspect protocol storage" || view.ExecutionItems[0].Owner != "reviewer" {
				t.Fatalf("execution item became a fourth primary step: %+v", view)
			}
		})
	}
	canonical, err := PlanningItems(sid)
	if err != nil || len(canonical) != 4 {
		t.Fatalf("runtime completeness lost execution record: %+v, %v", canonical, err)
	}
	for file, want := range before {
		got, err := os.ReadFile(file)
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("presentation mutated %s: %v", file, err)
		}
	}
}

func TestPlanningPresentationOnlyTodoAndOnlyTask(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	if err := Save(&List{SessionID: "only-todo", Items: []Item{{ID: "todo", Content: "Primary step", Status: "pending"}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := TaskStoreForSession("only-task").Create("Standalone execution", "", "", nil); err != nil {
		t.Fatal(err)
	}
	for _, sid := range []string{"only-todo", "only-task"} {
		view, err := PlanningPresentationSnapshot(sid)
		if err != nil || len(view.Steps) != 1 || len(view.ExecutionItems) != 0 {
			t.Fatalf("standalone plan %s: %+v, %v", sid, view, err)
		}
	}
}

func TestPlanningPresentationKeepsNormalizedDuplicateLifecycleAndArchivedFilter(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	const sid = "presentation-duplicate"
	oldPlan, err := BeginUserWork(sid)
	if err != nil {
		t.Fatal(err)
	}
	store := TaskStoreForSession(sid)
	if _, err := store.CreateOwnedInPlan(oldPlan, "Old hidden execution", "", "", "old-worker", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := ReplaceAll(sid, []Item{{Content: "1. Verify new service", Status: "pending"}}); err != nil {
		t.Fatal(err)
	}
	plan, err := CurrentPlanID(sid)
	if err != nil {
		t.Fatal(err)
	}
	// ReplaceAll started a disjoint plan; only a task bound to that plan can join it.
	duplicate, err := store.CreateOwnedInPlan(plan, "Verify new service", "", "", "verifier", nil)
	if err != nil {
		t.Fatal(err)
	}
	status := TaskInProgress
	if _, err := store.Update(duplicate.ID, TaskPatch{Status: &status}); err != nil {
		t.Fatal(err)
	}
	view, err := PlanningPresentationSnapshot(sid)
	if err != nil || len(view.Steps) != 1 || len(view.ExecutionItems) != 0 || view.Steps[0].Status != "in_progress" || view.Steps[0].Owner != "verifier" {
		t.Fatalf("duplicate lifecycle/filter changed: %+v, %v", view, err)
	}
	canonical, _ := PlanningItemsSnapshot(sid)
	if !reflect.DeepEqual(view.Steps, canonical) {
		t.Fatalf("same-content presentation changed canonical: %+v vs %+v", view, canonical)
	}
}
