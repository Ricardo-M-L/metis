package workflow

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunPreflightsEveryStepBeforeSpawning(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "should-not-exist")
	wf := Workflow{Steps: []Step{
		{Name: "safe-first", Command: "touch " + marker},
		{Name: "blocked-second", Command: "kill -9 123"},
	}}

	got := Run(context.Background(), wf, RunOptions{Cwd: dir, StopOnError: true})
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("safe step ran before blocked step was discovered: stat error = %v", err)
	}
	if len(got) != 2 || got[0].Status != StatusSkipped || got[1].Status != StatusFailed {
		t.Fatalf("results = %#v, want skipped then failed", got)
	}
	if !strings.Contains(got[1].Output, "BashKill(job_id)") {
		t.Fatalf("blocked output = %q, want BashKill guidance", got[1].Output)
	}
}
