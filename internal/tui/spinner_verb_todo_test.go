package tui

// spinner_verb_todo_test.go — pins the 2026-05-14 fix that makes the
// metis spinner verb reflect the in-progress TodoWrite task content
// instead of a static random gerund. Mirrors claude-code Spinner.tsx:169
// fallback chain: `currentTodo?.activeForm ?? currentTodo?.subject ?? randomVerb`.
//
// Without this, the spinner shows "exploring…" for the full duration
// of a long turn even when the model has set a clear in-progress todo
// like "Implementing OAuth refresh" — user-flagged on 2026-05-14 as
// "一直在exploring 不展示过程".

import (
	"encoding/json"
	"testing"
)

func TestChooseSpinnerVerb_PrefersInProgressTodo(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("METIS_HOME", dir)

	envelope := map[string]any{
		"session": "sid-todo",
		"items": []map[string]string{
			{"id": "1", "content": "Already done bit", "status": "completed"},
			{"id": "2", "content": "Implementing OAuth refresh", "status": "in_progress"},
			{"id": "3", "content": "Will do next", "status": "pending"},
		},
	}
	raw, _ := json.Marshal(envelope)
	writeTaskFile(t, dir, "sid-todo", raw)

	got := chooseSpinnerVerb("sid-todo")
	if got != "Implementing OAuth refresh" {
		t.Errorf("chooseSpinnerVerb should return in-progress todo content; got %q", got)
	}
}

func TestChooseSpinnerVerb_FallsBackToRandomWhenNoInProgress(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("METIS_HOME", dir)

	// All todos completed → fall back to a random gerund from thinkingVerbs.
	envelope := map[string]any{
		"session": "sid-done",
		"items": []map[string]string{
			{"id": "1", "content": "Done thing", "status": "completed"},
		},
	}
	raw, _ := json.Marshal(envelope)
	writeTaskFile(t, dir, "sid-done", raw)

	got := chooseSpinnerVerb("sid-done")
	if got == "" {
		t.Errorf("fallback must yield a non-empty verb; got empty")
	}
	// Must be one of the thinkingVerbs pool — never the bare empty
	// string or a completed-todo content.
	if got == "Done thing" {
		t.Errorf("fallback should NOT leak a completed todo's content; got %q", got)
	}
	found := false
	for _, v := range thinkingVerbs {
		if v == got {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("fallback verb %q not in thinkingVerbs pool", got)
	}
}

func TestChooseSpinnerVerb_EmptySidUsesRandom(t *testing.T) {
	got := chooseSpinnerVerb("")
	if got == "" {
		t.Errorf("empty sid should still yield a random verb, not empty")
	}
}

func TestCurrentInProgressTodoContent_NoInProgress(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("METIS_HOME", dir)

	envelope := map[string]any{
		"session": "sid-pending",
		"items": []map[string]string{
			{"id": "1", "content": "Pending one", "status": "pending"},
		},
	}
	raw, _ := json.Marshal(envelope)
	writeTaskFile(t, dir, "sid-pending", raw)

	if got := currentInProgressTodoContent("sid-pending"); got != "" {
		t.Errorf("pending-only todos must NOT surface as in-progress; got %q", got)
	}
}
