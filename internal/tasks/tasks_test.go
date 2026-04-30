package tasks

import (
	"os"
	"strings"
	"testing"
)

func TestUpsert_NewTaskGetsID(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	got, err := Upsert("s1", Item{Content: "first task"})
	if err != nil {
		t.Fatal(err)
	}
	if got.ID == "" {
		t.Error("new task should get a generated id")
	}
	if got.Status != "pending" {
		t.Errorf("default status = %q, want pending", got.Status)
	}
	if got.Priority != "medium" {
		t.Errorf("default priority = %q, want medium", got.Priority)
	}
}

func TestUpsert_UpdateByID(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	created, _ := Upsert("s", Item{Content: "do the thing"})
	updated, err := Upsert("s", Item{ID: created.ID, Content: "do the thing", Status: "in_progress"})
	if err != nil {
		t.Fatal(err)
	}
	if updated.Status != "in_progress" {
		t.Errorf("status not updated: %q", updated.Status)
	}
	tl, _ := Load("s")
	if len(tl.Items) != 1 {
		t.Errorf("update should not append; got %d items", len(tl.Items))
	}
}

func TestUpsert_UpdateByContent(t *testing.T) {
	// LLM commonly re-emits same content with new status to indicate
	// progress. Match by content as a fallback.
	t.Setenv("METIS_HOME", t.TempDir())
	_, _ = Upsert("s", Item{Content: "task A"})
	_, err := Upsert("s", Item{Content: "task A", Status: "completed"})
	if err != nil {
		t.Fatal(err)
	}
	tl, _ := Load("s")
	if len(tl.Items) != 1 {
		t.Errorf("re-emit by content should update, not append; got %d", len(tl.Items))
	}
	if tl.Items[0].Status != "completed" {
		t.Errorf("status not updated: %q", tl.Items[0].Status)
	}
}

func TestLoad_MissingSessionReturnsEmpty(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	tl, err := Load("brand-new")
	if err != nil {
		t.Fatal(err)
	}
	if len(tl.Items) != 0 {
		t.Errorf("missing session should yield empty list; got %d items", len(tl.Items))
	}
}

func TestUpsert_PerSessionIsolation(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	_, _ = Upsert("s1", Item{Content: "s1 task"})
	_, _ = Upsert("s2", Item{Content: "s2 task"})
	tl1, _ := Load("s1")
	tl2, _ := Load("s2")
	if len(tl1.Items) != 1 || tl1.Items[0].Content != "s1 task" {
		t.Errorf("s1 contaminated: %+v", tl1.Items)
	}
	if len(tl2.Items) != 1 || tl2.Items[0].Content != "s2 task" {
		t.Errorf("s2 contaminated: %+v", tl2.Items)
	}
}

func TestSetCurrentSessionID_RoundTrip(t *testing.T) {
	SetCurrentSessionID("set-then-get")
	defer SetCurrentSessionID("")
	if got := CurrentSessionID(); got != "set-then-get" {
		t.Errorf("current = %q, want set-then-get", got)
	}
}

func TestUpsert_EmptySessionFallsBackToDefault(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	_, err := Upsert("", Item{Content: "stray"})
	if err != nil {
		t.Fatal(err)
	}
	// File should land at "default.json"
	entries, _ := os.ReadDir(Dir())
	found := false
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "default") {
			found = true
		}
	}
	if !found {
		t.Errorf("empty session should write to default file; entries: %+v", entries)
	}
}
