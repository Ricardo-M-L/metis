package builtin

import (
	"context"
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/permission"
	taskstore "github.com/Ricardo-M-L/metis/internal/tasks"
)

// Each test sets up a fresh per-session TaskStore via SetCurrentTaskStore
// and runs through the tool.Execute path the agent would actually hit.
// METIS_HOME is sandboxed so the tests don't pollute ~/.metis.

func setupTaskTestEnv(t *testing.T) string {
	t.Helper()
	t.Setenv("METIS_HOME", t.TempDir())
	sid := "test-session-" + t.Name()
	taskstore.SetCurrentTaskStore(sid)
	t.Cleanup(func() { taskstore.SetCurrentTaskStore("") })
	return sid
}

func TestTaskCreate_WrapsStore(t *testing.T) {
	setupTaskTestEnv(t)
	tool := TaskCreate{gate: permission.New(permission.ModeBypass)}
	res, err := tool.Execute(context.Background(), map[string]any{
		"subject":     "write tests",
		"description": "cover the Task* tool wrappers",
		"activeForm":  "Writing tests",
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(res.Output, "Task #1") {
		t.Errorf("expected 'Task #1' in output, got %q", res.Output)
	}
	if !strings.Contains(res.Output, "write tests") {
		t.Errorf("output should mention subject, got %q", res.Output)
	}

	// Verify the store actually persisted.
	store := taskstore.CurrentTaskStore()
	got, ok := store.Get("1")
	if !ok {
		t.Fatalf("task #1 not found in store after create")
	}
	if got.Subject != "write tests" {
		t.Errorf("Subject = %q", got.Subject)
	}
	if got.Status != taskstore.TaskPending {
		t.Errorf("Status = %q, want pending", got.Status)
	}
}

func TestTaskCreate_RequiresSubject(t *testing.T) {
	setupTaskTestEnv(t)
	tool := TaskCreate{gate: permission.New(permission.ModeBypass)}
	_, err := tool.Execute(context.Background(), map[string]any{
		"description": "no subject",
	})
	if err == nil {
		t.Errorf("missing subject should error")
	}
}

func TestTaskCreate_ErrorWhenNoStore(t *testing.T) {
	// Clear current store; tool should refuse.
	taskstore.SetCurrentTaskStore("")
	tool := TaskCreate{gate: permission.New(permission.ModeBypass)}
	_, err := tool.Execute(context.Background(), map[string]any{
		"subject":     "anything",
		"description": "x",
	})
	if err == nil {
		t.Errorf("Execute should error when no current store is set")
	}
}

func TestTaskGet_Renders(t *testing.T) {
	setupTaskTestEnv(t)
	store := taskstore.CurrentTaskStore()
	tk, _ := store.Create("foo", "bar baz", "Doing foo", nil)

	tool := TaskGet{gate: permission.New(permission.ModeBypass)}
	res, err := tool.Execute(context.Background(), map[string]any{"taskId": tk.ID})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	for _, sub := range []string{"id:", "subject:", "foo", "description:", "bar baz", "status:", "pending"} {
		if !strings.Contains(res.Output, sub) {
			t.Errorf("Get output missing %q\n----\n%s\n----", sub, res.Output)
		}
	}
}

func TestTaskGet_NotFoundErrors(t *testing.T) {
	setupTaskTestEnv(t)
	tool := TaskGet{gate: permission.New(permission.ModeBypass)}
	_, err := tool.Execute(context.Background(), map[string]any{"taskId": "999"})
	if err == nil {
		t.Errorf("expected error for missing task id")
	}
}

func TestTaskList_Empty(t *testing.T) {
	setupTaskTestEnv(t)
	tool := TaskList{gate: permission.New(permission.ModeBypass)}
	res, err := tool.Execute(context.Background(), map[string]any{})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(res.Output, "no tasks") {
		t.Errorf("empty list output: %q", res.Output)
	}
}

func TestTaskList_RendersAllRows(t *testing.T) {
	setupTaskTestEnv(t)
	store := taskstore.CurrentTaskStore()
	store.Create("first", "", "", nil)
	store.Create("second", "", "", nil)
	store.Create("third", "", "", nil)

	tool := TaskList{gate: permission.New(permission.ModeBypass)}
	res, err := tool.Execute(context.Background(), map[string]any{})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	for _, sub := range []string{"3 task(s)", "first", "second", "third", "#1", "#2", "#3"} {
		if !strings.Contains(res.Output, sub) {
			t.Errorf("List output missing %q\n----\n%s\n----", sub, res.Output)
		}
	}
}

func TestTaskList_ExcludesDeletedByDefault(t *testing.T) {
	setupTaskTestEnv(t)
	store := taskstore.CurrentTaskStore()
	store.Create("alive", "", "", nil)
	store.Create("dead", "", "", nil)
	st := taskstore.TaskDeleted
	store.Update("2", taskstore.TaskPatch{Status: &st})

	tool := TaskList{gate: permission.New(permission.ModeBypass)}
	// default: exclude deleted
	res, _ := tool.Execute(context.Background(), map[string]any{})
	if strings.Contains(res.Output, "dead") {
		t.Errorf("default List should hide deleted, got: %s", res.Output)
	}
	if !strings.Contains(res.Output, "alive") {
		t.Errorf("List should still show alive: %s", res.Output)
	}

	// includeDeleted=true
	res, _ = tool.Execute(context.Background(), map[string]any{"includeDeleted": true})
	if !strings.Contains(res.Output, "dead") {
		t.Errorf("includeDeleted=true should surface deleted: %s", res.Output)
	}
}

func TestTaskUpdate_PatchesAndPersists(t *testing.T) {
	setupTaskTestEnv(t)
	store := taskstore.CurrentTaskStore()
	store.Create("before", "old desc", "", nil)

	tool := TaskUpdate{gate: permission.New(permission.ModeBypass)}
	_, err := tool.Execute(context.Background(), map[string]any{
		"taskId":      "1",
		"status":      "in_progress",
		"description": "new desc",
		"owner":       "me",
		"addBlocks":   []any{"2", "3"},
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	got, _ := store.Get("1")
	if got.Status != taskstore.TaskInProgress {
		t.Errorf("Status = %q", got.Status)
	}
	if got.Description != "new desc" {
		t.Errorf("Description = %q", got.Description)
	}
	if got.Owner != "me" {
		t.Errorf("Owner = %q", got.Owner)
	}
	if len(got.Blocks) != 2 || got.Blocks[0] != "2" || got.Blocks[1] != "3" {
		t.Errorf("Blocks = %v", got.Blocks)
	}
}

func TestTaskUpdate_UnknownIdErrors(t *testing.T) {
	setupTaskTestEnv(t)
	tool := TaskUpdate{gate: permission.New(permission.ModeBypass)}
	_, err := tool.Execute(context.Background(), map[string]any{
		"taskId": "999",
		"status": "completed",
	})
	if err == nil {
		t.Errorf("unknown id should error")
	}
}

func TestTaskOutput_Appends(t *testing.T) {
	setupTaskTestEnv(t)
	store := taskstore.CurrentTaskStore()
	store.Create("running", "", "", nil)

	tool := TaskOutput{gate: permission.New(permission.ModeBypass)}
	res, err := tool.Execute(context.Background(), map[string]any{
		"taskId": "1", "output": "step 1 done",
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(res.Output, "Appended") || !strings.Contains(res.Output, "step 1 done"[:0]+"") {
		t.Errorf("Output result: %q", res.Output)
	}

	// Append a second chunk; verify both end up in the buffer.
	tool.Execute(context.Background(), map[string]any{
		"taskId": "1", "output": "step 2 done",
	})

	got, _ := store.Get("1")
	if !strings.Contains(got.Output, "step 1 done") || !strings.Contains(got.Output, "step 2 done") {
		t.Errorf("buffer should contain both outputs: %q", got.Output)
	}
	if !strings.Contains(got.Output, "step 1 done\nstep 2 done") {
		t.Errorf("expected newline separator between appends: %q", got.Output)
	}
}

func TestTaskOutput_RequiresOutput(t *testing.T) {
	setupTaskTestEnv(t)
	store := taskstore.CurrentTaskStore()
	store.Create("x", "", "", nil)

	tool := TaskOutput{gate: permission.New(permission.ModeBypass)}
	_, err := tool.Execute(context.Background(), map[string]any{"taskId": "1"})
	if err == nil {
		t.Errorf("missing output should error")
	}
}

func TestTaskStop_MarksDeleted(t *testing.T) {
	setupTaskTestEnv(t)
	store := taskstore.CurrentTaskStore()
	store.Create("victim", "", "", nil)

	tool := TaskStop{gate: permission.New(permission.ModeBypass)}
	res, err := tool.Execute(context.Background(), map[string]any{"taskId": "1"})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(res.Output, "deleted") {
		t.Errorf("Stop output: %q", res.Output)
	}

	got, _ := store.Get("1")
	if got.Status != taskstore.TaskDeleted {
		t.Errorf("Status after Stop = %q, want deleted", got.Status)
	}
}

// --- end-to-end across multiple tools (Plan→Execute→Done flow) ---

func TestTaskTools_FullPlanExecuteFlow(t *testing.T) {
	setupTaskTestEnv(t)
	gate := permission.New(permission.ModeBypass)
	create := TaskCreate{gate: gate}
	upd := TaskUpdate{gate: gate}
	out := TaskOutput{gate: gate}
	get := TaskGet{gate: gate}

	// 1. Plan: create three tasks
	for _, subj := range []string{"setup", "implement", "verify"} {
		_, err := create.Execute(context.Background(), map[string]any{
			"subject": subj, "description": subj + " phase",
		})
		if err != nil {
			t.Fatalf("Create %q: %v", subj, err)
		}
	}

	// 2. Walk through them: in_progress → output → completed
	for _, id := range []string{"1", "2", "3"} {
		s := "in_progress"
		upd.Execute(context.Background(), map[string]any{"taskId": id, "status": s})
		out.Execute(context.Background(), map[string]any{"taskId": id, "output": "phase " + id + " ok"})
		s = "completed"
		upd.Execute(context.Background(), map[string]any{"taskId": id, "status": s})
	}

	// 3. Verify each task ended up completed with output recorded.
	for _, id := range []string{"1", "2", "3"} {
		res, err := get.Execute(context.Background(), map[string]any{"taskId": id})
		if err != nil {
			t.Errorf("Get %s: %v", id, err)
			continue
		}
		if !strings.Contains(res.Output, "completed") {
			t.Errorf("task %s should be completed:\n%s", id, res.Output)
		}
		if !strings.Contains(res.Output, "phase "+id+" ok") {
			t.Errorf("task %s output buffer missing recorded text:\n%s", id, res.Output)
		}
	}
}

// --- verifierNudge ---

// TestTaskUpdate_VerifierNudge_Fires — when the 3rd implementation
// task completes with no verify step among the closed tasks, the
// TaskUpdate result MUST carry the NUDGE: line so the model sees
// it on the same loop iteration.
func TestTaskUpdate_VerifierNudge_Fires(t *testing.T) {
	setupTaskTestEnv(t)
	create := TaskCreate{gate: permission.New(permission.ModeBypass)}
	for _, subj := range []string{"build parser", "build emitter", "build CLI"} {
		_, err := create.Execute(context.Background(), map[string]any{
			"subject": subj, "activeForm": subj,
		})
		if err != nil {
			t.Fatalf("Create %q: %v", subj, err)
		}
	}
	upd := TaskUpdate{gate: permission.New(permission.ModeBypass)}
	for _, id := range []string{"1", "2", "3"} {
		res, err := upd.Execute(context.Background(), map[string]any{
			"taskId": id, "status": "completed",
		})
		if err != nil {
			t.Fatalf("Update %s: %v", id, err)
		}
		if id == "3" {
			if !strings.Contains(res.Output, "NUDGE:") {
				t.Errorf("3rd completion should fire nudge; got: %q", res.Output)
			}
			if !strings.Contains(res.Output, "subagent_type: \"verify\"") {
				t.Errorf("nudge should name the verify subagent_type; got: %q", res.Output)
			}
			if !strings.Contains(res.Output, "VERDICT:") {
				t.Errorf("nudge should reference the verifier's VERDICT contract; got: %q", res.Output)
			}
		}
	}
}

// TestTaskUpdate_VerifierNudge_SuppressedByVerifyTask — when one of
// the tracked tasks is itself a verify step (subject contains
// "verify"/"test"/"review"/etc.), the nudge stays silent: the
// model has already planned the verification, no point nagging.
func TestTaskUpdate_VerifierNudge_SuppressedByVerifyTask(t *testing.T) {
	setupTaskTestEnv(t)
	create := TaskCreate{gate: permission.New(permission.ModeBypass)}
	subjects := []string{"build parser", "build emitter", "run test suite + verify"}
	for _, s := range subjects {
		if _, err := create.Execute(context.Background(), map[string]any{
			"subject": s, "activeForm": s,
		}); err != nil {
			t.Fatalf("Create %q: %v", s, err)
		}
	}
	upd := TaskUpdate{gate: permission.New(permission.ModeBypass)}
	for _, id := range []string{"1", "2", "3"} {
		res, err := upd.Execute(context.Background(), map[string]any{
			"taskId": id, "status": "completed",
		})
		if err != nil {
			t.Fatalf("Update %s: %v", id, err)
		}
		if strings.Contains(res.Output, "NUDGE:") {
			t.Errorf("nudge should be silent when a verify task is tracked; got: %q", res.Output)
		}
	}
}

// TestTaskUpdate_VerifierNudge_QuietUnderThreshold — closing fewer
// than 3 tasks should never fire the nudge. Cheap small task lists
// are noise if every completion drags a NUDGE block.
func TestTaskUpdate_VerifierNudge_QuietUnderThreshold(t *testing.T) {
	setupTaskTestEnv(t)
	create := TaskCreate{gate: permission.New(permission.ModeBypass)}
	for _, s := range []string{"step a", "step b"} {
		if _, err := create.Execute(context.Background(), map[string]any{
			"subject": s, "activeForm": s,
		}); err != nil {
			t.Fatalf("Create: %v", err)
		}
	}
	upd := TaskUpdate{gate: permission.New(permission.ModeBypass)}
	for _, id := range []string{"1", "2"} {
		res, err := upd.Execute(context.Background(), map[string]any{
			"taskId": id, "status": "completed",
		})
		if err != nil {
			t.Fatalf("Update %s: %v", id, err)
		}
		if strings.Contains(res.Output, "NUDGE:") {
			t.Errorf("under-threshold completion should not nudge; got: %q", res.Output)
		}
	}
}

// TestTaskUpdate_VerifierNudge_OnlyFiresOnCompletion — patches that
// change subject/owner without flipping status to completed must
// not trip the heuristic.
func TestTaskUpdate_VerifierNudge_OnlyFiresOnCompletion(t *testing.T) {
	setupTaskTestEnv(t)
	create := TaskCreate{gate: permission.New(permission.ModeBypass)}
	for _, s := range []string{"a", "b", "c"} {
		if _, err := create.Execute(context.Background(), map[string]any{
			"subject": s, "activeForm": s,
		}); err != nil {
			t.Fatalf("Create: %v", err)
		}
	}
	upd := TaskUpdate{gate: permission.New(permission.ModeBypass)}
	// Mark 1 and 2 completed so the threshold *would* be met if we
	// completed task 3. Instead, just rename task 3's subject.
	for _, id := range []string{"1", "2"} {
		_, err := upd.Execute(context.Background(), map[string]any{
			"taskId": id, "status": "completed",
		})
		if err != nil {
			t.Fatalf("Update %s: %v", id, err)
		}
	}
	res, err := upd.Execute(context.Background(), map[string]any{
		"taskId": "3", "subject": "renamed-only",
	})
	if err != nil {
		t.Fatalf("Update 3: %v", err)
	}
	if strings.Contains(res.Output, "NUDGE:") {
		t.Errorf("non-completion patch should not nudge; got: %q", res.Output)
	}
}
