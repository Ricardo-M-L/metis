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
	"github.com/Ricardo-M-L/metis/internal/tools"
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

func TestBashOutput_RedactsCredentialBearingJobLog(t *testing.T) {
	pool, gate := jobPoolFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, "sh", "-c", "printf 'CUSTOM_API_KEY=background-secret-value'")
	j, err := pool.Spawn(jobs.SpawnArgs{Command: "credential output test", Cmd: cmd, Cancel: cancel})
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if snap, ok := pool.Snapshot(j.ID); ok && snap.Status == jobs.StatusCompleted {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	res, err := (Output{gate: gate, pool: pool}).Execute(context.Background(), map[string]any{"job_id": j.ID})
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError || strings.Contains(res.Output, "background-secret-value") || !strings.Contains(res.Output, "[REDACTED]") {
		t.Fatalf("credential-bearing job output = %#v, want redacted successful output", res)
	}
}

func TestBashListAndOutputReadSnapshotsDuringCompletion(t *testing.T) {
	pool, gate := jobPoolFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, "sh", "-c", "printf 'reader-start\\n'; sleep 0.15; printf 'reader-done\\n'")
	j, err := pool.Spawn(jobs.SpawnArgs{Command: "reader transition", Cmd: cmd, Cancel: cancel})
	if err != nil {
		t.Fatal(err)
	}
	listTool := List{gate: gate, pool: pool}
	outputTool := Output{gate: gate, pool: pool}

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := listTool.Execute(context.Background(), nil); err != nil {
			t.Fatalf("List while completing: %v", err)
		}
		if _, err := outputTool.Execute(context.Background(), map[string]any{"job_id": j.ID}); err != nil {
			t.Fatalf("Output while completing: %v", err)
		}
		if snap, ok := pool.Get(j.ID); ok && snap.Status != jobs.StatusRunning {
			if snap.Status != jobs.StatusCompleted || snap.ExitCode != 0 {
				t.Fatalf("terminal snapshot = %s exit=%d", snap.Status, snap.ExitCode)
			}
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("job did not complete while readers were polling")
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

func TestBashKillCanUseChecksRealToolAndJobID(t *testing.T) {
	t.Parallel()
	gate := permission.New(permission.ModeDefault)
	gate.AppendRules(permission.Rule{Tool: "BashKill", Match: "bg_allowed", Verb: permission.DecisionAllow, Source: "test:job"})
	tool := Kill{gate: gate}

	if got, source := tool.CanUse(context.Background(), map[string]any{"job_id": "bg_allowed"}); got != tools.PermissionAllow || source != "test:job" {
		t.Fatalf("matching BashKill rule = %v (%q), want allow from test:job", got, source)
	}
	if got, _ := tool.CanUse(context.Background(), map[string]any{"job_id": "bg_other"}); got == tools.PermissionAllow {
		t.Fatalf("different job_id unexpectedly reused bg_allowed permission")
	}
}

// TestDetectBlockingWaitPattern pins the foreground-wait classifier. Delays
// of two seconds or more must leave the foreground turn, including the
// interpreter form models commonly use after a shell sleep is redirected.
func TestDetectBlockingWaitPattern(t *testing.T) {
	cases := []struct {
		in   string
		want bool // true → expected blocked
	}{
		{"sleep 1", false},
		{"sleep 1.5", false},
		{"sleep 2", true},
		{"sleep 3", true},
		{"sleep 5", true},
		{"sleep 9", true},
		{"sleep 10", true},
		{"sleep 30", true},
		{"sleep 10 && echo done", true},
		{"sleep 10; echo done", true},
		{"sleep 0.5", false}, // sub-two-second pacing is allowed
		{"echo a | sleep 5", false},
		{"(sleep 5; echo b)", false},
		{"{ sleep 5; echo done; }", false},
		{"for i in 1 2; do sleep 5; done", false},
		{"while true; do sleep 5; done", false},
		{`python3 -c "import time; time.sleep(45); print('done')"`, true},
		{`python -c "from time import sleep; sleep(9)"`, true},
		{`ruby -e 'sleep 12; puts :done'`, true},
		{`perl -e 'sleep 12; print qq(done)'`, true},
		{"echo no-sleep-here", false},
		// 2026-05-21 image #50 / session 5d9a38e5 regression:
		// `sleep N && find ... 2>/dev/null | wc -l` got PAST the
		// old blocker because cmd contained `|` `>` and the
		// pre-fix check `ContainsAny(cmd, "|()<>")` over-matched
		// the trailing pipe as "in pipeline". The model used this
		// to poll sub-agent progress 3 times in a row.
		{`sleep 30 && find /Users/x -name "*.go" 2>/dev/null | wc -l`, true},
		{`sleep 60 && find /Users/x -name "*.go" 2>/dev/null | wc -l`, true},
		{`sleep 120 && find /Users/x -name "*.go" 2>/dev/null | wc -l`, true},
		// `||` chain (rare but possible)
		{"sleep 30 || echo fallback", true},
		// `sleep N | rest` (pipe right after sleep, no &&) — also
		// polling-then-process, reject.
		{"sleep 30 | xargs echo", true},
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			got := detectBlockingWaitPattern(c.in)
			if c.want && got == "" {
				t.Errorf("detectBlockingWaitPattern(%q) should be redirected; got empty", c.in)
			}
			if !c.want && got != "" {
				t.Errorf("detectBlockingWaitPattern(%q) should stay foreground; got %s", c.in, got)
			}
		})
	}
}

func TestBlockingWaitIsAutomaticallyBackgrounded(t *testing.T) {
	pool, gate := jobPoolFixture(t)
	t.Cleanup(func() { pool.Shutdown(0) })
	tool := Bash{gate: gate, Jobs: pool}

	started := time.Now()
	res, err := tool.Execute(context.Background(), map[string]any{
		"command":     "sleep 2; printf done",
		"description": "verify wait redirection",
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if elapsed := time.Since(started); elapsed > 500*time.Millisecond {
		t.Fatalf("blocking wait held foreground for %s", elapsed)
	}
	if res == nil || res.IsError {
		t.Fatalf("wait redirection result = %+v", res)
	}
	if !strings.Contains(res.Output, "job_id=") || strings.Contains(res.Output, "Bare `sleep") {
		t.Fatalf("wait redirection output = %q", res.Output)
	}
	if res.Presentation["kind"] != "background_job" ||
		res.Presentation["await_completion"] != true ||
		res.Presentation["job_id"] == "" {
		t.Fatalf("wait redirection presentation = %#v", res.Presentation)
	}
	listed := pool.List()
	if len(listed) != 1 {
		t.Fatalf("background jobs = %+v, want one", listed)
	}
	_ = pool.Stop(listed[0].ID, 0)
}

func TestExplicitBackgroundBlockingWaitDoesNotAwaitCompletion(t *testing.T) {
	pool, gate := jobPoolFixture(t)
	t.Cleanup(func() { pool.Shutdown(0) })
	tool := Bash{gate: gate, Jobs: pool}

	started := time.Now()
	res, err := tool.Execute(context.Background(), map[string]any{
		"command":           "sleep 2; printf done",
		"description":       "verify explicit background semantics",
		"run_in_background": true,
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if elapsed := time.Since(started); elapsed > 500*time.Millisecond {
		t.Fatalf("explicit background command held foreground for %s", elapsed)
	}
	if res == nil || res.IsError {
		t.Fatalf("explicit background result = %+v", res)
	}
	if res.Presentation["kind"] != "background_job" || res.Presentation["job_id"] == "" {
		t.Fatalf("explicit background presentation = %#v", res.Presentation)
	}
	if _, ok := res.Presentation["await_completion"]; ok {
		t.Fatalf("explicit background command was rewritten as awaited: %#v", res.Presentation)
	}
	if _, ok := res.Presentation["wait_pattern"]; ok {
		t.Fatalf("explicit background command was tagged as an automatic wait: %#v", res.Presentation)
	}

	listed := pool.List()
	if len(listed) != 1 {
		t.Fatalf("background jobs = %+v, want one", listed)
	}
	_ = pool.Stop(listed[0].ID, 0)
}

func TestRebindJobsRegistryUsesPrivatePoolWithoutAddingFilteredTools(t *testing.T) {
	parentPool, gate := jobPoolFixture(t)
	childPool := jobs.NewRegistry(t.TempDir())
	t.Cleanup(func() {
		parentPool.Shutdown(0)
		childPool.Shutdown(0)
	})

	registry := tools.NewRegistry()
	registry.Register(Bash{gate: gate, Jobs: parentPool})
	registry.Register(Output{gate: gate, pool: parentPool})
	RebindJobsRegistry(registry, childPool, gate)

	boundBash, ok := registry.Get("Bash")
	if !ok || boundBash.(Bash).Jobs != childPool {
		t.Fatal("Bash was not rebound to the child pool")
	}
	boundOutput, ok := registry.Get("BashOutput")
	if !ok || boundOutput.(Output).pool != childPool {
		t.Fatal("BashOutput was not rebound to the child pool")
	}
	for _, absent := range []string{"BashList", "BashKill"} {
		if _, ok := registry.Get(absent); ok {
			t.Fatalf("RebindJobsRegistry re-added filtered tool %s", absent)
		}
	}

	res, err := boundBash.Execute(context.Background(), map[string]any{
		"command": "sleep 2; printf CHILD_POOL_MARKER",
	})
	if err != nil || res == nil || res.IsError {
		t.Fatalf("child Bash Execute = (%+v, %v)", res, err)
	}
	select {
	case notification := <-childPool.Notify():
		if notification.Status != jobs.StatusCompleted {
			t.Fatalf("child notification = %#v", notification)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("private child pool did not receive completion")
	}
	select {
	case notification := <-parentPool.Notify():
		t.Fatalf("parent pool stole child notification: %#v", notification)
	default:
	}
}
