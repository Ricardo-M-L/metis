package tasks

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSessionIDFromContextOverridesGlobalRouter(t *testing.T) {
	SetCurrentSessionID("global-session")
	t.Cleanup(func() { SetCurrentSessionID("") })
	ctx := WithSessionID(context.Background(), "turn-session")
	if got := SessionIDFromContext(ctx); got != "turn-session" {
		t.Fatalf("SessionIDFromContext = %q, want turn-session", got)
	}
	if got := SessionIDFromContext(context.Background()); got != "global-session" {
		t.Fatalf("fallback SessionIDFromContext = %q, want global-session", got)
	}
}

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

func TestDeleteRemovesBothTaskStoresAndPreservesOtherSessions(t *testing.T) {
	home := t.TempDir()
	t.Setenv("METIS_HOME", home)

	if _, err := Upsert("sess", Item{Content: "simple"}); err != nil {
		t.Fatal(err)
	}
	if _, err := Upsert("sess-extra", Item{Content: "keep simple"}); err != nil {
		t.Fatal(err)
	}
	structured := NewTaskStore("sess")
	if _, err := structured.Create("structured", "", "", nil); err != nil {
		t.Fatal(err)
	}
	otherStructured := NewTaskStore("sess-extra")
	if _, err := otherStructured.Create("keep structured", "", "", nil); err != nil {
		t.Fatal(err)
	}

	if err := Delete("sess"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if err := Delete("sess"); err != nil {
		t.Fatalf("second Delete should be idempotent: %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, "tasks", "sess.json")); !os.IsNotExist(err) {
		t.Fatalf("simple task file still exists: %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, "sessions", "sess")); !os.IsNotExist(err) {
		t.Fatalf("structured task directory still exists: %v", err)
	}
	for _, kept := range []string{
		filepath.Join(home, "tasks", "sess-extra.json"),
		filepath.Join(home, "sessions", "sess-extra", "tasks-structured.json"),
	} {
		if _, err := os.Stat(kept); err != nil {
			t.Errorf("unrelated task data removed: %s: %v", kept, err)
		}
	}
}

func TestDeleteRejectsUnsafeSessionIDs(t *testing.T) {
	home := t.TempDir()
	t.Setenv("METIS_HOME", home)
	outside := filepath.Join(home, "outside")
	if err := os.MkdirAll(outside, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"", ".", "..", "../outside", "a/b", `a\b`, "bad\nname"} {
		if err := Delete(id); err == nil {
			t.Errorf("Delete(%q) unexpectedly succeeded", id)
		}
	}
	if _, err := os.Stat(outside); err != nil {
		t.Fatalf("outside directory changed: %v", err)
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

// TestReplaceAll_DuplicateContentDistinctIDs — two items whose content
// normalises to the same thing must NOT both claim the same id (the
// 2026-06-14 review finding); the second gets a fresh id.
func TestReplaceAll_DuplicateContentDistinctIDs(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	sid := "dup-id-test"
	// Seed one task so both new items have something to (wrongly) match.
	if _, err := Upsert(sid, Item{Content: "1. wire protocol", Status: "pending"}); err != nil {
		t.Fatal(err)
	}
	// Two new items that normalise to the same content as the seed.
	tl, err := ReplaceAll(sid, []Item{
		{Content: "1. wire protocol", Status: "in_progress"},
		{Content: "wire protocol", Status: "pending"}, // normalises the same
	})
	if err != nil {
		t.Fatalf("ReplaceAll: %v", err)
	}
	if len(tl.Items) != 2 {
		t.Fatalf("expected 2 items, got %d", len(tl.Items))
	}
	if tl.Items[0].ID == tl.Items[1].ID {
		t.Errorf("duplicate ids %q — claimed-set guard failed", tl.Items[0].ID)
	}
}
