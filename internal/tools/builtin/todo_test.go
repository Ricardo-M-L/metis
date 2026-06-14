package builtin

import (
	"context"
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/tasks"
)

// setupTodoTestEnv sandboxes ~/.metis and gives the test its own
// session id so the TodoWrite tool reads/writes a clean tasks file.
// Matches setupTaskTestEnv's style in task_test.go but uses the
// lower-level tasks.SetCurrentSessionID hook that TodoWrite reads
// from.
func setupTodoTestEnv(t *testing.T) {
	t.Helper()
	t.Setenv("METIS_HOME", t.TempDir())
	sid := "todo-test-" + t.Name()
	tasks.SetCurrentSessionID(sid)
	t.Cleanup(func() { tasks.SetCurrentSessionID("") })
}

// TestTodoWrite_VerifierNudge_Fires — closing out 3 TodoWrite items
// with no verify-related content in any of them must append the
// NUDGE block to the tool result, mirroring the TaskUpdate path.
// The first claude-code-go port run (2026-05-17) showed the model
// preferring TodoWrite over TaskCreate; without this nudge the
// model finishes Phase 1 build + vet and silently stops.
func TestTodoWrite_VerifierNudge_Fires(t *testing.T) {
	setupTodoTestEnv(t)
	tool := Todo{gate: permission.New(permission.ModeBypass)}
	contents := []string{"Phase 1 build parser", "Phase 1 build emitter", "Phase 1 build CLI"}
	var last string
	for i, c := range contents {
		res, err := tool.Execute(context.Background(), map[string]any{
			"content": c, "status": "completed",
		})
		if err != nil {
			t.Fatalf("Execute %d: %v", i, err)
		}
		last = res.Output
	}
	if !strings.Contains(last, "NUDGE:") {
		t.Errorf("3rd TodoWrite completion should fire nudge; got: %q", last)
	}
	if !strings.Contains(last, "subagent_type: \"verify\"") {
		t.Errorf("nudge should name the verify subagent; got: %q", last)
	}
	if !strings.Contains(last, "VERDICT") {
		t.Errorf("nudge should reference VERDICT contract; got: %q", last)
	}
}

// TestTodoWrite_VerifierNudge_SuppressedByVerifyContent — when one of
// the TodoWrite items already contains a verify keyword in its
// content, the nudge stays silent. The model has already planned
// for verification; pointless nagging.
func TestTodoWrite_VerifierNudge_SuppressedByVerifyContent(t *testing.T) {
	setupTodoTestEnv(t)
	tool := Todo{gate: permission.New(permission.ModeBypass)}
	contents := []string{"build parser", "build emitter", "run test suite + verify VERDICT"}
	var last string
	for _, c := range contents {
		res, err := tool.Execute(context.Background(), map[string]any{
			"content": c, "status": "completed",
		})
		if err != nil {
			t.Fatalf("Execute: %v", err)
		}
		last = res.Output
	}
	if strings.Contains(last, "NUDGE:") {
		t.Errorf("nudge should stay silent when a verify item is tracked; got: %q", last)
	}
}

// TestTodoWrite_VerifierNudge_QuietUnderThreshold — fewer than 3
// completed items must never fire the nudge.
func TestTodoWrite_VerifierNudge_QuietUnderThreshold(t *testing.T) {
	setupTodoTestEnv(t)
	tool := Todo{gate: permission.New(permission.ModeBypass)}
	for _, c := range []string{"step a", "step b"} {
		res, err := tool.Execute(context.Background(), map[string]any{
			"content": c, "status": "completed",
		})
		if err != nil {
			t.Fatalf("Execute: %v", err)
		}
		if strings.Contains(res.Output, "NUDGE:") {
			t.Errorf("under-threshold completion should not nudge; got: %q", res.Output)
		}
	}
}

// TestTodoWrite_VerifierNudge_OnlyFiresOnCompletion — patches that
// flip status to in_progress (not completed) must not trip the
// heuristic even when 3+ prior items were completed. Status flow
// is normally pending → in_progress → completed, so we test the
// in_progress transition explicitly.
func TestTodoWrite_VerifierNudge_OnlyFiresOnCompletion(t *testing.T) {
	setupTodoTestEnv(t)
	tool := Todo{gate: permission.New(permission.ModeBypass)}
	for _, c := range []string{"a", "b", "c"} {
		if _, err := tool.Execute(context.Background(), map[string]any{
			"content": c, "status": "completed",
		}); err != nil {
			t.Fatalf("Execute completed: %v", err)
		}
	}
	res, err := tool.Execute(context.Background(), map[string]any{
		"content": "d-in-progress", "status": "in_progress",
	})
	if err != nil {
		t.Fatalf("Execute in_progress: %v", err)
	}
	if strings.Contains(res.Output, "NUDGE:") {
		t.Errorf("in_progress update should not nudge; got: %q", res.Output)
	}
}

// TestTodoWrite_FullListReplace — the Claude Code form. Passing the
// complete `todos` array replaces the whole list; restating all tasks
// with their status is what fixes the 2026-06-14 bug (earlier tasks
// silently left mid-status). The final call below marks the three
// verify tasks completed and they MUST all show completed.
func TestTodoWrite_FullListReplace(t *testing.T) {
	setupTodoTestEnv(t)
	tool := Todo{gate: permission.New(permission.ModeBypass)}

	mk := func(statuses ...string) map[string]any {
		names := []string{"验证 MiMo-Code", "验证 Metis", "验证 Codex", "汇总报告"}
		todos := make([]any, len(names))
		for i, n := range names {
			todos[i] = map[string]any{"content": n, "status": statuses[i], "priority": "high"}
		}
		return map[string]any{"todos": todos}
	}

	// Initial: first in progress, rest pending.
	if _, err := tool.Execute(context.Background(), mk("in_progress", "pending", "pending", "pending")); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	// Final: all three verify tasks completed + summary completed.
	res, err := tool.Execute(context.Background(), mk("completed", "completed", "completed", "completed"))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	tl, _ := tasks.Load(tasks.CurrentSessionID())
	if len(tl.Items) != 4 {
		t.Fatalf("expected 4 tasks, got %d (list replace should not duplicate)", len(tl.Items))
	}
	for _, it := range tl.Items {
		if it.Status != "completed" {
			t.Errorf("task %q left at status %q — full-list replace must mark all as sent", it.Content, it.Status)
		}
	}
	// Output should show every task, all completed.
	if strings.Count(res.Output, "[completed]") < 4 {
		t.Errorf("result should list all 4 completed tasks; got:\n%s", res.Output)
	}
}

// TestTodoWrite_FullListReplace_PreservesIdentity — re-sending the same
// task by content keeps its id + CreatedAt (no churn / no duplicate).
func TestTodoWrite_FullListReplace_PreservesIdentity(t *testing.T) {
	setupTodoTestEnv(t)
	tool := Todo{gate: permission.New(permission.ModeBypass)}

	one := func(status string) map[string]any {
		return map[string]any{"todos": []any{map[string]any{"content": "task A", "status": status}}}
	}
	if _, err := tool.Execute(context.Background(), one("in_progress")); err != nil {
		t.Fatal(err)
	}
	before, _ := tasks.Load(tasks.CurrentSessionID())
	id0, created0 := before.Items[0].ID, before.Items[0].CreatedAt

	if _, err := tool.Execute(context.Background(), one("completed")); err != nil {
		t.Fatal(err)
	}
	after, _ := tasks.Load(tasks.CurrentSessionID())
	if len(after.Items) != 1 {
		t.Fatalf("expected 1 task, got %d (identity not preserved → duplicate)", len(after.Items))
	}
	if after.Items[0].ID != id0 || !after.Items[0].CreatedAt.Equal(created0) {
		t.Error("re-sent task should keep its id + CreatedAt")
	}
	if after.Items[0].Status != "completed" {
		t.Errorf("status should update to completed, got %q", after.Items[0].Status)
	}
}

// TestTodoWrite_SingleTaskBackCompat — the legacy single-task form must
// still work (one-off updates).
func TestTodoWrite_SingleTaskBackCompat(t *testing.T) {
	setupTodoTestEnv(t)
	tool := Todo{gate: permission.New(permission.ModeBypass)}
	if _, err := tool.Execute(context.Background(), map[string]any{"content": "lone task", "status": "in_progress"}); err != nil {
		t.Fatalf("single-task form should still work: %v", err)
	}
	tl, _ := tasks.Load(tasks.CurrentSessionID())
	if len(tl.Items) != 1 || tl.Items[0].Status != "in_progress" {
		t.Errorf("single-task upsert broken: %+v", tl.Items)
	}
}
