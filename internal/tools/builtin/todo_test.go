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
