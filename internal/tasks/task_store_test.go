package tasks

import (
	"strings"
	"testing"
)

func TestTaskStore_CreateListGetUpdate(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	s := NewTaskStore("sess1")
	tk, err := s.Create("write tests", "cover the new path", "Writing tests", nil)
	if err != nil {
		t.Fatal(err)
	}
	if tk.ID != "1" {
		t.Errorf("first id = %q", tk.ID)
	}
	if tk.Status != TaskPending {
		t.Errorf("status = %q", tk.Status)
	}

	tk2, err := s.Create("ship", "deploy", "Shipping", nil)
	if err != nil {
		t.Fatal(err)
	}
	if tk2.ID != "2" {
		t.Errorf("second id = %q", tk2.ID)
	}

	if got, ok := s.Get("1"); !ok || got.Subject != "write tests" {
		t.Errorf("Get(1) = %v ok=%v", got, ok)
	}

	all := s.List(false)
	if len(all) != 2 {
		t.Errorf("List len = %d", len(all))
	}

	// Update status + append output.
	status := TaskInProgress
	out := "compiling..."
	if _, err := s.Update("1", TaskPatch{Status: &status, AppendOutput: out}); err != nil {
		t.Fatal(err)
	}
	got, _ := s.Get("1")
	if got.Status != TaskInProgress {
		t.Errorf("status not updated: %q", got.Status)
	}
	if got.Output != "compiling..." {
		t.Errorf("output = %q", got.Output)
	}

	// Delete.
	del := TaskDeleted
	if _, err := s.Update("2", TaskPatch{Status: &del}); err != nil {
		t.Fatal(err)
	}
	if got2 := s.List(false); len(got2) != 1 || got2[0].ID != "1" {
		t.Errorf("after delete, List = %v", got2)
	}
	if got2 := s.List(true); len(got2) != 2 {
		t.Errorf("includeDeleted=true should show both: %d", len(got2))
	}
}

func TestTaskStore_PersistAcrossNewStore(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	a := NewTaskStore("sess1")
	a.Create("first", "", "Doing first", nil)
	a.Create("second", "", "Doing second", nil)

	b := NewTaskStore("sess1")
	if got := b.List(false); len(got) != 2 {
		t.Errorf("after reload List = %d", len(got))
	}
	// nextNum should continue from 3, not collide with 1/2.
	tk, _ := b.Create("third", "", "Doing third", nil)
	if tk.ID != "3" {
		t.Errorf("nextNum collision: %q", tk.ID)
	}
}

func TestTaskStore_BlocksDedup(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	s := NewTaskStore("sess1")
	s.Create("a", "", "", nil)
	s.Update("1", TaskPatch{AddBlocks: []string{"2", "3", "2"}})
	got, _ := s.Get("1")
	if len(got.Blocks) != 2 || got.Blocks[0] != "2" || got.Blocks[1] != "3" {
		t.Errorf("Blocks dedup failed: %v", got.Blocks)
	}
}

func TestTaskStore_RejectsEmptySubject(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	s := NewTaskStore("sess1")
	if _, err := s.Create("  ", "", "", nil); err == nil {
		t.Errorf("empty subject should error")
	}
}

func TestSetCurrentTaskStore(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	SetCurrentTaskStore("")
	if CurrentTaskStore() != nil {
		t.Errorf("empty session should nil out store")
	}
	SetCurrentTaskStore("session-A")
	if CurrentTaskStore() == nil {
		t.Errorf("expected non-nil store after set")
	}
	first := CurrentTaskStore()
	SetCurrentTaskStore("session-A")
	if CurrentTaskStore() != first {
		t.Errorf("re-set with same id should not rebuild")
	}
	SetCurrentTaskStore("session-B")
	if CurrentTaskStore() == first {
		t.Errorf("different id should rebuild")
	}
}

// --- bonus sanity that test names compile when used as deps ---

func TestTaskStore_OutputAppendsNewline(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	s := NewTaskStore("s")
	s.Create("x", "", "", nil)
	s.Update("1", TaskPatch{AppendOutput: "line1"})
	s.Update("1", TaskPatch{AppendOutput: "line2"})
	got, _ := s.Get("1")
	if !strings.Contains(got.Output, "line1\nline2") {
		t.Errorf("output should accumulate with newline: %q", got.Output)
	}
}

func TestPlanningItemsUnifiesTodoAndStructuredTasks(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	if _, err := Upsert("session-plan", Item{Content: "inspect existing code", Status: "completed"}); err != nil {
		t.Fatal(err)
	}
	store := TaskStoreForSession("session-plan")
	created, err := store.Create("implement independent module", "", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	owner := "alice"
	status := TaskInProgress
	if _, err := store.Update(created.ID, TaskPatch{Owner: &owner, Status: &status}); err != nil {
		t.Fatal(err)
	}
	items, err := PlanningItems("session-plan")
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 {
		t.Fatalf("PlanningItems len = %d, want 2: %+v", len(items), items)
	}
	if items[1].Content != "implement independent module" || items[1].Status != "in_progress" || items[1].Owner != "alice" {
		t.Fatalf("structured projection = %+v", items[1])
	}
}

func TestPlanningItemsDeduplicatesEquivalentTaskNames(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	if _, err := Upsert("session-dedup", Item{Content: "1. inspect cache policy", Status: "pending"}); err != nil {
		t.Fatal(err)
	}
	store := TaskStoreForSession("session-dedup")
	created, err := store.CreateOwned("inspect cache policy", "", "", "alice", nil)
	if err != nil {
		t.Fatal(err)
	}
	status := TaskInProgress
	if _, err := store.Update(created.ID, TaskPatch{Status: &status}); err != nil {
		t.Fatal(err)
	}
	items, err := PlanningItems("session-dedup")
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 {
		t.Fatalf("PlanningItems len = %d, want one merged task: %+v", len(items), items)
	}
	if items[0].Status != "in_progress" || items[0].Owner != "alice" {
		t.Fatalf("merged task = %+v", items[0])
	}
}
