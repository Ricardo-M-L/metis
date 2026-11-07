package tui

import (
	"encoding/json"
	"strings"
	"testing"
)

// A historical tool result must remain tied to its own call when expansion,
// resizing, or cache invalidation causes it to render again.
func TestReviewTodoWriteHistoryMustUseItsOwnSnapshot(t *testing.T) {
	home := t.TempDir()
	t.Setenv("METIS_HOME", home)
	sid := "review-history-snapshot"
	previous := tasksCurrentSessionIDFn
	tasksCurrentSessionIDFn = func() string { return sid }
	t.Cleanup(func() { tasksCurrentSessionIDFn = previous })
	seed := func(content string) {
		raw, _ := json.Marshal(map[string]any{"session": sid, "items": []TaskItem{{ID: "one", Content: content, Status: "completed"}}})
		writeTaskFile(t, home, sid, raw)
	}
	seed("HISTORICAL_TOOL_RESULT_TASK")
	event := ToolEvent{
		Kind: "result", ToolName: "TodoWrite",
		Input:  map[string]any{"todos": []any{map[string]any{"content": "HISTORICAL_TOOL_RESULT_TASK", "status": "completed"}}},
		Output: "HISTORICAL_TOOL_RESULT_TASK completed",
	}
	first := stripAnsiForTest(renderToolEvent(event, true))
	if !strings.Contains(first, "HISTORICAL_TOOL_RESULT_TASK") {
		t.Fatalf("fixture not rendered: %q", first)
	}
	seed("UNRELATED_LATER_PLAN_TASK")
	for _, width := range []int{0, 80, 36} {
		for _, expanded := range []bool{false, true} {
			second := stripAnsiForTest(renderToolEventAtWidth(event, expanded, width))
			if strings.Contains(second, "UNRELATED_LATER_PLAN_TASK") || !strings.Contains(second, "HISTORICAL_TOOL_RESULT_TASK\n") {
				t.Fatalf("historical TodoWrite changed at width=%d expanded=%v: %q", width, expanded, second)
			}
		}
	}
}

func TestTodoWriteSnapshotDoesNotNeedCurrentSession(t *testing.T) {
	previous := tasksCurrentSessionIDFn
	tasksCurrentSessionIDFn = func() string { return "" }
	t.Cleanup(func() { tasksCurrentSessionIDFn = previous })
	event := ToolEvent{
		Kind: "result", ToolName: "TodoWrite",
		Input: map[string]any{"todos": []any{
			map[string]any{"content": "finished stage", "status": "completed"},
			map[string]any{"content": "active stage", "status": "in_progress", "owner": "worker"},
		}},
		Output: "● [completed] finished stage\n◐ [in_progress] active stage\n\nNUDGE: INTERNAL_REMINDER_NOT_A_TASK",
	}
	got := stripAnsiForTest(renderToolEvent(event, true))
	for _, want := range []string{"finished stage", "active stage", "@worker"} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing snapshot item %q: %q", want, got)
		}
	}
	// The normal result summary is only the first result line. The internal
	// reminder must never be rendered as part of the task-list body.
	if strings.Contains(got, "INTERNAL_REMINDER_NOT_A_TASK") {
		t.Fatalf("internal reminder leaked into snapshot: %q", got)
	}
}

func TestTodoWriteSnapshotEmptyAndLegacyResult(t *testing.T) {
	t.Run("explicit empty list does not read later tasks", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("METIS_HOME", home)
		previous := tasksCurrentSessionIDFn
		tasksCurrentSessionIDFn = func() string { return "empty-history" }
		t.Cleanup(func() { tasksCurrentSessionIDFn = previous })
		raw, _ := json.Marshal(map[string]any{"session": "empty-history", "items": []TaskItem{{Content: "LATER_TASK", Status: "pending"}}})
		writeTaskFile(t, home, "empty-history", raw)
		event := ToolEvent{Kind: "result", ToolName: "TodoWrite", Input: map[string]any{"todos": []any{}}, Output: "Task list cleared."}
		if got := stripAnsiForTest(renderToolEvent(event, true)); strings.Contains(got, "LATER_TASK") {
			t.Fatalf("empty snapshot reused later task list: %q", got)
		}
	})
	t.Run("single task gets saved result status", func(t *testing.T) {
		event := ToolEvent{Kind: "result", ToolName: "TodoWrite", Input: map[string]any{"content": "single legacy task"}, Output: "● 🟡 [saved-id] single legacy task"}
		got := stripAnsiForTest(renderToolEvent(event, true))
		if !strings.Contains(got, glyphTaskCompleted+" single legacy task") {
			t.Fatalf("saved result status not reflected: %q", got)
		}
	})
	t.Run("result-only restored event", func(t *testing.T) {
		event := ToolEvent{Kind: "result", ToolName: "TodoWrite", Output: "● [completed] historical done\n○ [pending] historical pending\n\nNUDGE: IGNORE_ME"}
		got := stripAnsiForTest(renderToolEvent(event, true))
		if !strings.Contains(got, glyphTaskCompleted+" historical done") || !strings.Contains(got, glyphTaskPending+" historical pending") || strings.Contains(got, "IGNORE_ME") {
			t.Fatalf("restored event lost its result snapshot: %q", got)
		}
	})
	t.Run("running event does not show persisted tasks", func(t *testing.T) {
		event := ToolEvent{Kind: "start", ToolName: "TodoWrite", Input: map[string]any{"todos": []any{map[string]any{"content": "not committed yet", "status": "pending"}}}}
		if got := stripAnsiForTest(renderToolEvent(event, true)); strings.Contains(got, "not committed yet") {
			t.Fatalf("running call showed an uncommitted task result: %q", got)
		}
	})
}

func TestTodoWriteSnapshotDuplicateTitlesKeepTheirOwnStatuses(t *testing.T) {
	event := ToolEvent{
		Kind: "result", ToolName: "TodoWrite",
		Input: map[string]any{"todos": []any{
			map[string]any{"id": "first", "content": "same task title", "status": "pending", "owner": "first-worker"},
			map[string]any{"id": "second", "content": "same task title", "status": "completed", "owner": "second-worker"},
		}},
		Output: "○ [pending] same task title\n● [completed] same task title",
	}
	items := todoSnapshotItems(event)
	if len(items) != 2 || items[0].ID != "first" || items[0].Status != "pending" || items[1].ID != "second" || items[1].Status != "completed" {
		t.Fatalf("same-title tasks lost their distinct accepted states: %+v", items)
	}
	got := stripAnsiForTest(renderTodoSnapshot(event))
	if strings.Count(got, glyphTaskPending) != 1 || strings.Count(got, glyphTaskCompleted) != 1 || !strings.Contains(got, "@first-worker") || !strings.Contains(got, "@second-worker") {
		t.Fatalf("same-title task rows were merged: %q", got)
	}
}
