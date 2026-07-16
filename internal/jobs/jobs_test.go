package jobs

import (
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// quickRegistry returns a Registry rooted at the test's t.TempDir so
// the on-disk job files don't pollute ~/.metis/jobs and don't survive
// the test run.
func quickRegistry(t *testing.T) *Registry {
	t.Helper()
	return NewRegistry(t.TempDir())
}

// spawnEcho is the fast happy-path spawn: a one-shot `echo` that
// completes within milliseconds. Used as the building block for
// state-machine tests.
func spawnEcho(t *testing.T, r *Registry, msg string) *Job {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, "sh", "-c", "echo "+msg)
	j, err := r.Spawn(SpawnArgs{
		Command: "echo " + msg,
		Cmd:     cmd,
		Cancel:  cancel,
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	return j
}

// waitForStatus polls the registry until the job leaves StatusRunning
// or the test deadline trips. Eliminates flaky time.Sleep waits.
//
// Get returns a detached value snapshot taken under r.mu, so the spawn
// goroutine can concurrently update Status / EndTime / ExitCode safely.
func waitForStatus(t *testing.T, r *Registry, id string, want Status, timeout time.Duration) *Job {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		snap, ok := r.Get(id)
		if ok && snap.Status == want {
			return &snap
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("job %s never reached %s within %s", id, want, timeout)
	return nil
}

// TestSpawn_HappyPath — simplest case: echo runs, transitions to
// completed, ExitCode=0, output file contains the echoed text.
func TestSpawn_HappyPath(t *testing.T) {
	r := quickRegistry(t)
	j := spawnEcho(t, r, "hello-jobs")
	if j.Status != StatusRunning {
		t.Errorf("fresh job should be StatusRunning; got %s", j.Status)
	}
	if !strings.HasPrefix(j.ID, "bg_") {
		t.Errorf("ID should be bg_<hex>; got %q", j.ID)
	}
	if j.OutputPath == "" {
		t.Errorf("OutputPath must be set on Spawn return")
	}

	final := waitForStatus(t, r, j.ID, StatusCompleted, 3*time.Second)
	if final.ExitCode != 0 {
		t.Errorf("echo should exit 0; got %d", final.ExitCode)
	}
	if final.EndTime.IsZero() {
		t.Errorf("EndTime must be set when status is Completed")
	}

	out, err := ReadJobOutput(final.OutputPath, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "hello-jobs") {
		t.Errorf("output file should contain echoed text; got %q", out)
	}
}

// TestSpawn_NotificationFires — a completed job publishes one
// Notification on the registry's chan. Pin the envelope so the agent-
// loop drainer doesn't drift from the test contract.
func TestSpawn_NotificationFires(t *testing.T) {
	r := quickRegistry(t)
	j := spawnEcho(t, r, "notif")
	select {
	case n := <-r.Notify():
		if n.JobID != j.ID {
			t.Errorf("notification JobID = %q, want %q", n.JobID, j.ID)
		}
		if n.Status != StatusCompleted {
			t.Errorf("notification Status = %s, want completed", n.Status)
		}
		if n.ExitCode != 0 {
			t.Errorf("notification ExitCode = %d, want 0", n.ExitCode)
		}
		if n.Elapsed <= 0 {
			t.Errorf("notification Elapsed must be > 0")
		}
		if n.Command != "echo notif" {
			t.Errorf("notification Command = %q, want %q", n.Command, "echo notif")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("notification never fired")
	}
}

// TestSpawn_FailedExit — non-zero exit lands StatusFailed with the
// real exit code. Distinct from StatusKilled (which the model needs
// to differentiate so it doesn't pretend a kill was a real error).
func TestSpawn_FailedExit(t *testing.T) {
	r := quickRegistry(t)
	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, "sh", "-c", "exit 42")
	j, err := r.Spawn(SpawnArgs{
		Command: "exit 42",
		Cmd:     cmd,
		Cancel:  cancel,
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	final := waitForStatus(t, r, j.ID, StatusFailed, 3*time.Second)
	if final.ExitCode != 42 {
		t.Errorf("exit code = %d, want 42", final.ExitCode)
	}
}

// TestStop_TerminalStateIsKilled — Stop on a running job transitions
// it to StatusKilled (NOT StatusFailed) and publishes a Notification.
// The wait goroutine must see Status=Killed and skip its own state
// mutation so the kill envelope wins.
func TestStop_TerminalStateIsKilled(t *testing.T) {
	r := quickRegistry(t)
	ctx, cancel := context.WithCancel(context.Background())
	// sleep long enough that we'll Stop while it's still running.
	cmd := exec.CommandContext(ctx, "sh", "-c", "sleep 30")
	j, err := r.Spawn(SpawnArgs{
		Command: "sleep 30",
		Cmd:     cmd,
		Cancel:  cancel,
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if err := r.Stop(j.ID, 100*time.Millisecond); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	// Drain the notification (registry buffers it but a slow test
	// shouldn't depend on order).
	select {
	case n := <-r.Notify():
		if n.Status != StatusKilled {
			t.Errorf("kill notification Status = %s, want killed", n.Status)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("kill notification never fired")
	}
	// Wait for the process to fully exit so our cleanup is real.
	// Use Snapshot + CleanedUp (both take r.mu) instead of naked
	// r.Get(id).cmd reads — the spawn goroutine concurrently nils
	// cmd/cancel/output in waitAndComplete and the race detector
	// (correctly) caught the unlocked field access.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		snap, ok := r.Snapshot(j.ID)
		if ok && snap.Status == StatusKilled && r.CleanedUp(j.ID) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("kill cleanup never landed")
}

// TestStop_UnknownIDError — Stop on a non-existent ID returns a clear
// error. The JobStop tool surfaces this verbatim to the model.
func TestStop_UnknownIDError(t *testing.T) {
	r := quickRegistry(t)
	if err := r.Stop("bg_doesnotexist", 0); err == nil {
		t.Error("expected error for unknown job ID")
	}
}

// TestStop_AlreadyTerminalIsNoOp — Stop on a job that already
// completed is a silent success. This is the common race when both
// the model and the user (Ctrl+B equivalent) try to act on the same
// job; the second hit shouldn't error.
func TestStop_AlreadyTerminalIsNoOp(t *testing.T) {
	r := quickRegistry(t)
	j := spawnEcho(t, r, "already-done")
	waitForStatus(t, r, j.ID, StatusCompleted, 3*time.Second)
	if err := r.Stop(j.ID, 0); err != nil {
		t.Errorf("Stop on terminal job should be no-op; got %v", err)
	}
}

// TestRegistryResetStartsFreshSessionState verifies the in-place reset used by
// /new, /branch and /resume. Tool instances keep the Registry pointer, so a
// reset must hide old completed/running jobs without making that pointer
// unusable for the destination session.
func TestRegistryResetStartsFreshSessionState(t *testing.T) {
	r := quickRegistry(t)
	completed := spawnEcho(t, r, "old-completed")
	waitForStatus(t, r, completed.ID, StatusCompleted, 3*time.Second)

	ctx, cancel := context.WithCancel(context.Background())
	running, err := r.Spawn(SpawnArgs{
		Command: "sleep 30",
		Cmd:     exec.CommandContext(ctx, "sh", "-c", "sleep 30"),
		Cancel:  cancel,
	})
	if err != nil {
		t.Fatalf("spawn old running job: %v", err)
	}

	r.Reset(0)
	if got := r.List(); len(got) != 0 {
		t.Fatalf("old jobs visible after reset: %+v", got)
	}
	for _, id := range []string{completed.ID, running.ID} {
		if _, ok := r.Get(id); ok {
			t.Errorf("old job %s remains addressable after reset", id)
		}
	}

	fresh := spawnEcho(t, r, "new-session")
	waitForStatus(t, r, fresh.ID, StatusCompleted, 3*time.Second)
	select {
	case n := <-r.Notify():
		if n.JobID != fresh.ID {
			t.Fatalf("stale notification crossed reset: got %s, want %s", n.JobID, fresh.ID)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("fresh job did not publish through reset registry")
	}
}

// TestList_StableOrder — multiple jobs return in start-time order,
// regardless of map iteration order.
func TestList_StableOrder(t *testing.T) {
	r := quickRegistry(t)
	a := spawnEcho(t, r, "a")
	time.Sleep(20 * time.Millisecond) // distinct timestamps
	b := spawnEcho(t, r, "b")
	time.Sleep(20 * time.Millisecond)
	c := spawnEcho(t, r, "c")
	got := r.List()
	if len(got) != 3 {
		t.Fatalf("List len = %d, want 3", len(got))
	}
	if got[0].ID != a.ID || got[1].ID != b.ID || got[2].ID != c.ID {
		t.Errorf("List order wrong: got %s, %s, %s", got[0].ID, got[1].ID, got[2].ID)
	}
}

func TestListAndGetReturnDetachedSnapshots(t *testing.T) {
	r := quickRegistry(t)
	j := spawnEcho(t, r, "snapshot")

	got, ok := r.Get(j.ID)
	if !ok {
		t.Fatal("Get did not find spawned job")
	}
	got.Status = StatusKilled
	got.Command = "mutated by caller"

	list := r.List()
	if len(list) != 1 {
		t.Fatalf("List len = %d, want 1", len(list))
	}
	list[0].Description = "also mutated"

	fresh, ok := r.Get(j.ID)
	if !ok {
		t.Fatal("second Get did not find spawned job")
	}
	if fresh.Command == got.Command || fresh.Description == list[0].Description {
		t.Fatalf("caller mutation leaked into registry: %+v", fresh)
	}
}

// TestReadJobOutput_HandlesTruncation — when the file is bigger than
// tailMax, return the tail with a "[truncated head ...]" prefix.
func TestReadJobOutput_HandlesTruncation(t *testing.T) {
	r := quickRegistry(t)
	// `yes` produces unbounded "y\n" lines; cap the run with `head`.
	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, "sh", "-c", "yes | head -c 5000")
	j, err := r.Spawn(SpawnArgs{
		Command: "yes | head -c 5000",
		Cmd:     cmd,
		Cancel:  cancel,
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	waitForStatus(t, r, j.ID, StatusCompleted, 3*time.Second)
	out, err := ReadJobOutput(j.OutputPath, 200)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out, "[truncated head") {
		t.Errorf("expected truncation prefix on big file; got %.80q", out)
	}
}

// TestShortDesc_MultiLineFolded — multi-line commands collapse to a
// single line so JobList rows render predictably.
func TestShortDesc_MultiLineFolded(t *testing.T) {
	in := "line1\nline2\nline3"
	got := shortDesc(in)
	if strings.Contains(got, "\n") {
		t.Errorf("shortDesc must drop newlines; got %q", got)
	}
}

// TestNewJobID_FormatStable — IDs always look like bg_<8hex>.
func TestNewJobID_FormatStable(t *testing.T) {
	for i := 0; i < 100; i++ {
		id := newJobID()
		if !strings.HasPrefix(id, "bg_") {
			t.Errorf("id %q missing bg_ prefix", id)
		}
		if len(id) != 11 {
			t.Errorf("id %q length = %d, want 11", id, len(id))
		}
	}
}
