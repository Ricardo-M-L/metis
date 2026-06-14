package workflow

import (
	"context"
	"strings"
	"testing"
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
