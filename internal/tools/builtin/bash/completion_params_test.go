package bash

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/internal/jobs"
	"github.com/Ricardo-M-L/metis/internal/permission"
)

func TestBashCompletionParamsSchema(t *testing.T) {
	props := (Bash{}).InputSchema()["properties"].(map[string]any)
	for _, name := range []string{"await_completion", "required_for_completion"} {
		prop, ok := props[name].(map[string]any)
		if !ok || prop["type"] != "boolean" {
			t.Errorf("schema %q = %#v, want optional boolean", name, props[name])
		}
	}
	for _, name := range (Bash{}).InputSchema()["required"].([]string) {
		if name == "await_completion" || name == "required_for_completion" {
			t.Errorf("completion parameter %q must be opt-in", name)
		}
	}
}

func TestBashCompletionParamsPresentation(t *testing.T) {
	for _, tc := range []struct {
		name       string
		flags      map[string]any
		background bool
		await      bool
		required   bool
	}{
		{name: "foreground_default"},
		{name: "foreground_explicit_false", flags: map[string]any{"await_completion": false, "required_for_completion": false}},
		{name: "server_default", flags: map[string]any{"run_in_background": true}, background: true},
		{name: "server_explicit_false", flags: map[string]any{"run_in_background": true, "await_completion": false, "required_for_completion": false}, background: true},
		{name: "await_implies_background", flags: map[string]any{"await_completion": true}, background: true, await: true},
		{name: "await_with_background", flags: map[string]any{"run_in_background": true, "await_completion": true}, background: true, await: true},
		{name: "required_implies_both", flags: map[string]any{"required_for_completion": true}, background: true, await: true, required: true},
		{name: "required_overrides_false", flags: map[string]any{"run_in_background": false, "await_completion": false, "required_for_completion": true}, background: true, await: true, required: true},
		{name: "required_with_background", flags: map[string]any{"run_in_background": true, "required_for_completion": true}, background: true, await: true, required: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := jobs.NewRegistry(t.TempDir())
			t.Cleanup(func() { pool.ResetAndWait(0) })
			b := Bash{gate: permission.New(permission.ModeBypass), Jobs: pool}
			in := map[string]any{"command": "printf completion-marker", "description": "verify completion parameter transport"}
			for k, v := range tc.flags {
				in[k] = v
			}
			res, err := b.Execute(context.Background(), in)
			if err != nil || res == nil || res.IsError {
				t.Fatalf("Execute: result=%+v err=%v", res, err)
			}
			if got := res.Presentation["kind"] == "background_job"; got != tc.background {
				t.Fatalf("background = %v, want %v; presentation=%#v", got, tc.background, res.Presentation)
			}
			if got, _ := res.Presentation["await_completion"].(bool); got != tc.await {
				t.Errorf("await_completion = %v, want %v", got, tc.await)
			}
			if got, _ := res.Presentation["required_for_completion"].(bool); got != tc.required {
				t.Errorf("required_for_completion = %v, want %v", got, tc.required)
			}
			if !tc.background {
				if len(pool.List()) != 0 || res.Output != "completion-marker" {
					t.Errorf("foreground behavior changed: jobs=%v output=%q", pool.List(), res.Output)
				}
				return
			}
			id, ok := res.Presentation["job_id"].(string)
			if !ok || id == "" {
				t.Fatalf("missing job_id: %#v", res.Presentation)
			}
			if _, ok := pool.Get(id); !ok {
				t.Fatalf("published job %q is absent from registry", id)
			}
			if tc.required && !strings.Contains(res.Output, "required for task completion") {
				t.Errorf("required job lacks model-facing completion instruction: %q", res.Output)
			}
		})
	}
}

func TestBashCompletionParamsRequireRegistry(t *testing.T) {
	for _, name := range []string{"await_completion", "required_for_completion"} {
		t.Run(name, func(t *testing.T) {
			b := Bash{gate: permission.New(permission.ModeBypass)}
			res, err := b.Execute(context.Background(), map[string]any{
				"command": "printf must-not-execute", "description": "require background completion support", name: true,
			})
			if err != nil || res == nil || !res.IsError || !strings.Contains(res.Output, "jobs registry") {
				t.Fatalf("missing registry must return explicit tool error: result=%+v err=%v", res, err)
			}
		})
	}
}

// Execute receives a short-lived tool-call context. Finite jobs are owned and
// stopped by the agent Run, not by this child context which is cancelled as
// soon as the tool returns a job ID. Binding Spawn directly to it would make
// every required background verifier fail before it can provide evidence.
func TestBashCompletionParamsSurviveToolContextCancellation(t *testing.T) {
	pool := jobs.NewRegistry(t.TempDir())
	t.Cleanup(func() { pool.ResetAndWait(0) })
	b := Bash{gate: permission.New(permission.ModeBypass), Jobs: pool}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	res, err := b.Execute(ctx, map[string]any{
		"command":     "printf verification-start; sleep 0.10; printf verification-finish",
		"description": "verify finite job context ownership", "required_for_completion": true,
	})
	if err != nil || res == nil || res.IsError || res.Presentation["required_for_completion"] != true {
		t.Fatalf("required background job: result=%+v err=%v", res, err)
	}
	id := res.Presentation["job_id"].(string)
	cancel()
	select {
	case note := <-pool.Notify():
		if note.JobID != id || note.Status != jobs.StatusCompleted || note.ExitCode != 0 {
			t.Fatalf("tool context cancellation killed required job: %+v", note)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("required job did not complete")
	}
}
