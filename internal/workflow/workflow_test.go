package workflow

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/sandbox"
)

func TestRun_AllStepsSucceed(t *testing.T) {
	wf := Workflow{Name: "ok", Steps: []Step{
		{Name: "a", Command: "echo step-a"},
		{Name: "b", Command: "echo step-b"},
	}}
	res := Run(context.Background(), wf, RunOptions{StopOnError: true})
	if len(res) != 2 {
		t.Fatalf("got %d results, want 2", len(res))
	}
	for _, r := range res {
		if r.Status != StatusOK {
			t.Errorf("step %s status = %s, want ok", r.Name, r.Status)
		}
	}
	if !strings.Contains(res[0].Output, "step-a") {
		t.Errorf("step a output = %q", res[0].Output)
	}
	if Failed(res) {
		t.Error("Failed() = true for an all-ok run")
	}
}

func TestRun_StopsOnFailure(t *testing.T) {
	wf := Workflow{Steps: []Step{
		{Name: "a", Command: "echo fine"},
		{Name: "b", Command: "exit 3"},
		{Name: "c", Command: "echo should-not-run"},
	}}
	res := Run(context.Background(), wf, RunOptions{StopOnError: true})
	if res[0].Status != StatusOK {
		t.Errorf("step a should be ok, got %s", res[0].Status)
	}
	if res[1].Status != StatusFailed || res[1].ExitCode != 3 {
		t.Errorf("step b should fail with exit 3, got %s exit %d", res[1].Status, res[1].ExitCode)
	}
	if res[2].Status != StatusSkipped {
		t.Errorf("step c should be skipped after b failed, got %s", res[2].Status)
	}
	if strings.Contains(res[2].Output, "should-not-run") {
		t.Error("skipped step c actually ran")
	}
	if !Failed(res) {
		t.Error("Failed() should be true")
	}
}

func TestRun_ContinuesWhenStopOnErrorFalse(t *testing.T) {
	wf := Workflow{Steps: []Step{
		{Name: "a", Command: "exit 1"},
		{Name: "b", Command: "echo ran-anyway"},
	}}
	res := Run(context.Background(), wf, RunOptions{StopOnError: false})
	if res[0].Status != StatusFailed {
		t.Errorf("step a should fail, got %s", res[0].Status)
	}
	if res[1].Status != StatusOK || !strings.Contains(res[1].Output, "ran-anyway") {
		t.Errorf("step b should run despite a's failure; got %s %q", res[1].Status, res[1].Output)
	}
}

func TestRun_ProcessGuardDefaultAndExplicitOptOut(t *testing.T) {
	wf := Workflow{Steps: []Step{{Name: "probe", Command: "kill -0 $$"}}}

	t.Run("default blocks", func(t *testing.T) {
		results := Run(context.Background(), wf, RunOptions{StopOnError: true})
		if len(results) != 1 || results[0].Status != StatusFailed || !strings.Contains(results[0].Output, "BashKill(job_id)") {
			t.Fatalf("Run = %+v, want process-guard failure", results)
		}
	})

	t.Run("explicit opt-out executes", func(t *testing.T) {
		results := Run(context.Background(), wf, RunOptions{
			StopOnError:               true,
			SkipDangerousCommandCheck: true,
		})
		if len(results) != 1 || results[0].Status != StatusOK {
			t.Fatalf("Run = %+v, want harmless guarded command to execute", results)
		}
	})
}

func TestCapOutput_KeepsErrorTail(t *testing.T) {
	head := strings.Repeat("progress line\n", 200)
	full := head + "\nFATAL: the build exploded"
	got := capOutput(full, 200)
	if !strings.Contains(got, "FATAL: the build exploded") {
		t.Errorf("error tail dropped: %q", got[len(got)-60:])
	}
	if !strings.Contains(got, "error output") {
		t.Errorf("missing omission marker: %q", got)
	}
}

func TestRun_SandboxWrapFailureFailsClosedAndStops(t *testing.T) {
	manager, err := sandbox.NewManager(string(sandbox.ModeOff))
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Close(); err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(t.TempDir(), "must-not-run")
	wf := Workflow{Steps: []Step{
		{Name: "wrapped", Command: "printf should-not-run > '" + sentinel + "'"},
		{Name: "after", Command: "printf also-must-not-run"},
	}}

	results := Run(context.Background(), wf, RunOptions{StopOnError: true, Sandbox: manager})
	if len(results) != 2 {
		t.Fatalf("got %d results, want 2", len(results))
	}
	if results[0].Status != StatusFailed || results[0].ExitCode != -1 || !strings.Contains(results[0].Output, "sandbox wrap failed") {
		t.Fatalf("first result = %+v, want explicit sandbox failure", results[0])
	}
	if results[1].Status != StatusSkipped {
		t.Fatalf("second result = %+v, want skipped", results[1])
	}
	if _, err := os.Stat(sentinel); !os.IsNotExist(err) {
		t.Fatalf("workflow step ran after Wrap failure: stat err=%v", err)
	}
}

func TestRun_InjectedManagerFiltersSecretsAndSetsPrivateTemp(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "must-not-leak")
	manager, err := sandbox.NewManager(string(sandbox.ModeOff))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close() })
	wf := Workflow{Steps: []Step{{
		Name:    "env",
		Command: `if [ -n "${OPENAI_API_KEY:-}" ]; then echo leaked; exit 9; fi; printf %s "$TMPDIR"`,
	}}}

	results := Run(context.Background(), wf, RunOptions{StopOnError: true, Sandbox: manager})
	if len(results) != 1 || results[0].Status != StatusOK {
		t.Fatalf("workflow env probe failed: %+v", results)
	}
	if results[0].Output != manager.TempDir() {
		t.Fatalf("workflow TMPDIR=%q, want private temp %q", results[0].Output, manager.TempDir())
	}
}
