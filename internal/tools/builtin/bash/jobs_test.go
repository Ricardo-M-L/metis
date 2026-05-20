package bash

// bash_jobs_test.go — pin the three job-pool tools (List /
// Output / Kill) against a real jobs.Registry. Each test
// runs against a TempDir-rooted registry so on-disk job logs don't
// leak across tests.

import (
	"context"
	"encoding/json"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/internal/jobs"
	"github.com/Ricardo-M-L/metis/internal/permission"
)

// jobPoolFixture spins up a fresh registry + bypass gate so each test
// starts from a known-empty state.
func jobPoolFixture(t *testing.T) (*jobs.Registry, *permission.Gate) {
	t.Helper()
	gate := permission.New(permission.ModeBypass)
	pool := jobs.NewRegistry(t.TempDir())
	return pool, gate
}

// spawnSleepyJob registers a fake bash job that sleeps long enough to
// outlive the assertions. Used by tests that need a still-running
// job to query against.
func spawnSleepyJob(t *testing.T, r *jobs.Registry, label string) *jobs.Job {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, "sh", "-c", "sleep 30")
	j, err := r.Spawn(jobs.SpawnArgs{
		Command:     "sleep 30 # " + label,
		Description: label,
		Cmd:         cmd,
		Cancel:      cancel,
	})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	return j
}

func TestBashList_Empty(t *testing.T) {
	pool, gate := jobPoolFixture(t)
	tool := List{gate: gate, pool: pool}
	res, err := tool.Execute(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	// Empty pool returns "[]" (still-valid JSON, parseable).
	var arr []map[string]any
	if err := json.Unmarshal([]byte(res.Output), &arr); err != nil {
		t.Fatalf("output not valid JSON: %v\n%s", err, res.Output)
	}
	if len(arr) != 0 {
		t.Errorf("empty pool should return zero rows; got %d", len(arr))
	}
}

func TestBashList_RunningJobAppears(t *testing.T) {
	pool, gate := jobPoolFixture(t)
	j := spawnSleepyJob(t, pool, "test-running")
	defer pool.Stop(j.ID, 0)

	tool := List{gate: gate, pool: pool}
	res, err := tool.Execute(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Output, j.ID) {
		t.Errorf("List output missing job ID %s; got %q", j.ID, res.Output)
	}
	if !strings.Contains(res.Output, `"running"`) {
		t.Errorf("running job's status should be 'running'; got %q", res.Output)
	}
	// Running job should NOT have an exit_code yet (omitempty).
	if strings.Contains(res.Output, `"exit_code"`) {
		t.Errorf("running job should not have exit_code; got %q", res.Output)
	}
}

func TestBashList_TerminalJobIncludesExitCode(t *testing.T) {
	pool, gate := jobPoolFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, "sh", "-c", "exit 7")
	j, err := pool.Spawn(jobs.SpawnArgs{
		Command: "exit 7",
		Cmd:     cmd,
		Cancel:  cancel,
	})
	if err != nil {
		t.Fatal(err)
	}
	// Wait for the wait-goroutine to record the terminal state.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if snap, ok := pool.Snapshot(j.ID); ok && snap.Status == jobs.StatusFailed {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	tool := List{gate: gate, pool: pool}
	res, err := tool.Execute(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Output, `"exit_code": 7`) {
		t.Errorf("failed job should report exit_code=7; got %q", res.Output)
	}
}

func TestBashOutput_RequiresJobID(t *testing.T) {
	pool, gate := jobPoolFixture(t)
	tool := Output{gate: gate, pool: pool}
	if _, err := tool.Execute(context.Background(), map[string]any{}); err == nil {
		t.Error("expected error on missing job_id")
	}
}

func TestBashOutput_UnknownIDReturnsError(t *testing.T) {
	pool, gate := jobPoolFixture(t)
	tool := Output{gate: gate, pool: pool}
	res, err := tool.Execute(context.Background(), map[string]any{"job_id": "bg_doesnotexist"})
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError {
		t.Errorf("unknown job_id should set IsError")
	}
}

func TestBashOutput_ReadsRunningJob(t *testing.T) {
	pool, gate := jobPoolFixture(t)
	// A short-lived job that completes quickly so the disk file has
	// content by the time we read.
	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, "sh", "-c", "echo job-output-marker")
	j, err := pool.Spawn(jobs.SpawnArgs{Command: "echo job-output-marker", Cmd: cmd, Cancel: cancel})
	if err != nil {
		t.Fatal(err)
	}
	// Wait for completion so the on-disk file is fully flushed.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if snap, ok := pool.Snapshot(j.ID); ok && snap.Status == jobs.StatusCompleted {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	tool := Output{gate: gate, pool: pool}
	res, err := tool.Execute(context.Background(), map[string]any{"job_id": j.ID})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Output, "job-output-marker") {
		t.Errorf("output should include the echoed marker; got %q", res.Output)
	}
	if !strings.Contains(res.Output, j.ID) {
		t.Errorf("output header should include job ID; got %q", res.Output)
	}
	if !strings.Contains(res.Output, "exit 0") {
		t.Errorf("completed job should report exit 0; got %q", res.Output)
	}
}

func TestBashKill_StopsRunningJob(t *testing.T) {
	pool, gate := jobPoolFixture(t)
	j := spawnSleepyJob(t, pool, "to-be-killed")

	tool := Kill{gate: gate, pool: pool}
	res, err := tool.Execute(context.Background(), map[string]any{"job_id": j.ID})
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Errorf("Kill on a live job shouldn't IsError; got %q", res.Output)
	}
	// Verify the job actually transitions to Killed.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if snap, ok := pool.Snapshot(j.ID); ok && snap.Status == jobs.StatusKilled {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("Kill didn't transition job to StatusKilled within 3s")
}

func TestBashKill_UnknownIDIsError(t *testing.T) {
	pool, gate := jobPoolFixture(t)
	tool := Kill{gate: gate, pool: pool}
	res, err := tool.Execute(context.Background(), map[string]any{"job_id": "bg_nope"})
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError {
		t.Error("unknown job ID should be IsError")
	}
}

// TestDetectBlockedSleepPattern — pin the sleep blacklist matrix:
// bare integer sleeps ≥ 2s and `sleep N && rest` are blocked,
// sub-2s and pipeline / subshell forms are allowed.
func TestDetectBlockedSleepPattern(t *testing.T) {
	cases := []struct {
		in   string
		want bool // true → expected blocked
	}{
		{"sleep 5", true},
		{"sleep 30", true},
		{"sleep 5 && echo done", true},
		{"sleep 5; echo done", true},
		{"sleep 1", false},                        // sub-2s allowed
		{"sleep 0.5", false},                      // float allowed (regex matches integer only)
		{"echo a | sleep 5", false},               // in pipeline → allowed
		{"(sleep 5; echo b)", false},              // in subshell → allowed
		{"for i in 1 2; do sleep 5; done", false}, // in for-loop → allowed
		{"echo no-sleep-here", false},
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			got := detectBlockedSleepPattern(c.in)
			if c.want && got == "" {
				t.Errorf("detectBlockedSleepPattern(%q) should be blocked; got empty", c.in)
			}
			if !c.want && got != "" {
				t.Errorf("detectBlockedSleepPattern(%q) should be allowed; got blocked: %s", c.in, got)
			}
		})
	}
}
