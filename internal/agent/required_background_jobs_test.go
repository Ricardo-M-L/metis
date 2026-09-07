package agent

import (
	"context"
	"errors"
	"os/exec"
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/internal/jobs"
	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

type requiredJobTestTool struct {
	tools.BaseTool
	pool                 *jobs.Registry
	command              string
	required             bool
	id                   string
	registerBeforeResult bool
	cancelBeforeResult   context.CancelFunc
}

func (*requiredJobTestTool) Name() string                { return "RequiredJob" }
func (*requiredJobTestTool) Description() string         { return "finite verification job fixture" }
func (*requiredJobTestTool) InputSchema() map[string]any { return map[string]any{"type": "object"} }
func (*requiredJobTestTool) Concurrency(map[string]any) tools.Concurrency {
	return tools.ConcurrencyExclusive
}
func (*requiredJobTestTool) CanUse(context.Context, map[string]any) (tools.Permission, string) {
	return tools.PermissionAllow, ""
}
func (t *requiredJobTestTool) Execute(toolCtx context.Context, _ map[string]any) (*tools.Result, error) {
	ctx, cancel := context.WithCancel(context.Background())
	spawn := func() (*jobs.Job, error) {
		return t.pool.Spawn(jobs.SpawnArgs{Command: t.command, Cmd: exec.CommandContext(ctx, "sh", "-c", t.command), Cancel: cancel})
	}
	var job *jobs.Job
	var err error
	if t.registerBeforeResult {
		job, err = StartFiniteBackgroundJob(toolCtx, t.pool, t.required, spawn)
	} else {
		job, err = spawn()
	}
	if err != nil {
		cancel()
		return nil, err
	}
	t.id = job.ID
	if t.cancelBeforeResult != nil {
		t.cancelBeforeResult()
		return nil, context.Canceled
	}
	return &tools.Result{Output: "background", Presentation: map[string]any{"kind": "background_job", "job_id": job.ID, "required_for_completion": t.required}}, nil
}

func TestRequiredJobRegisteredBeforeLostToolResult(t *testing.T) {
	pool := jobs.NewRegistry(t.TempDir())
	t.Cleanup(func() { pool.ResetAndWait(0) })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tool := &requiredJobTestTool{pool: pool, command: "sleep 60", required: true, registerBeforeResult: true, cancelBeforeResult: cancel}
	registry := tools.NewRegistry()
	registry.Register(tool)
	provider := &queuedStreamProvider{streams: []llm.StreamReader{toolUseStream("start", "RequiredJob", `{}`)}}
	loop := NewLoop(provider, registry, permission.New(permission.ModeAcceptEdits), nil, "system", 5)
	loop.Jobs = pool
	loop.JobNotify = pool.Notify()
	loop.AppendUser("start verification")
	if err := loop.Run(ctx, make(chan Event, 64)); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run err=%v", err)
	}
	job, ok := pool.Get(tool.id)
	if !ok || job.Status != jobs.StatusKilled {
		t.Fatalf("lost tool_result orphaned job: %+v", job)
	}
}

func TestFiniteJobCannotStartAfterRunOwnershipClosed(t *testing.T) {
	tracker := &finiteJobTracker{}
	ctx := context.WithValue(context.Background(), finiteJobTrackerContextKey{}, tracker)
	if err := tracker.cleanup(context.Background()); err != nil {
		t.Fatal(err)
	}
	called := false
	_, err := StartFiniteBackgroundJob(ctx, nil, true, func() (*jobs.Job, error) { called = true; return nil, nil })
	if called || !errors.Is(err, context.Canceled) {
		t.Fatalf("late spawn called=%v err=%v", called, err)
	}
}

func TestFiniteJobCleanupSignalsAllJobsEvenWhenJoinCancelled(t *testing.T) {
	pool := jobs.NewRegistry(t.TempDir())
	t.Cleanup(func() { pool.ResetAndWait(0) })
	tracker := &finiteJobTracker{pool: pool}
	ctx := context.WithValue(context.Background(), finiteJobTrackerContextKey{}, tracker)
	var ids []string
	for i := 0; i < 2; i++ {
		processCtx, cancel := context.WithCancel(context.Background())
		job, err := StartFiniteBackgroundJob(ctx, pool, true, func() (*jobs.Job, error) {
			return pool.Spawn(jobs.SpawnArgs{Command: "sleep 60", Cmd: exec.CommandContext(processCtx, "sh", "-c", "sleep 60"), Cancel: cancel})
		})
		if err != nil {
			cancel()
			t.Fatal(err)
		}
		ids = append(ids, job.ID)
	}
	cleanupCtx, cancelCleanup := context.WithCancel(context.Background())
	cancelCleanup()
	_ = tracker.cleanup(cleanupCtx)
	for _, id := range ids {
		job, _ := pool.Get(id)
		if job.Status != jobs.StatusKilled {
			t.Fatalf("cleanup left %s %s", id, job.Status)
		}
	}
}

func TestRequiredBackgroundJobMustActuallySucceed(t *testing.T) {
	for _, tc := range []struct {
		name, command, want string
		required            bool
	}{
		{"pass", "exit 0", "end_turn", true},
		{"failure", "exit 7", "acceptance_incomplete", true},
		{"server", "sleep 60", "end_turn", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := jobs.NewRegistry(t.TempDir())
			t.Cleanup(func() { pool.ResetAndWait(0) })
			tool := &requiredJobTestTool{pool: pool, command: tc.command, required: tc.required}
			registry := tools.NewRegistry()
			registry.Register(tool)
			provider := &queuedStreamProvider{streams: []llm.StreamReader{
				toolUseStream("start", "RequiredJob", `{}`), textStream("all done"), textStream("final"),
			}}
			loop := NewLoop(provider, registry, permission.New(permission.ModeAcceptEdits), nil, "system", 5)
			loop.Jobs = pool
			loop.JobNotify = pool.Notify()
			loop.AppendUser("run required verification")
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			out := make(chan Event, 64)
			if err := loop.Run(ctx, out); err != nil {
				t.Fatal(err)
			}
			close(out)
			got := ""
			for event := range out {
				if event.Kind == EventLoopDone {
					got = event.StopReason
				}
			}
			if got != tc.want {
				t.Fatalf("stop=%q, want %q", got, tc.want)
			}
			job, ok := pool.Get(tool.id)
			if !ok {
				t.Fatal("job disappeared")
			}
			if tc.required && job.Status == jobs.StatusRunning {
				t.Fatal("Run claimed completion while required job still running")
			}
			if !tc.required && job.Status != jobs.StatusRunning {
				t.Fatal("ordinary devserver was stopped by completion gate")
			}
		})
	}
}

func TestRequiredBackgroundJobCancelledRunCleansOnlyOwnedJobs(t *testing.T) {
	pool := jobs.NewRegistry(t.TempDir())
	t.Cleanup(func() { pool.ResetAndWait(0) })
	serverCtx, serverCancel := context.WithCancel(context.Background())
	defer serverCancel()
	server, err := pool.Spawn(jobs.SpawnArgs{Command: "sleep 60", Cmd: exec.CommandContext(serverCtx, "sh", "-c", "sleep 60"), Cancel: serverCancel})
	if err != nil {
		t.Fatal(err)
	}
	tool := &requiredJobTestTool{pool: pool, command: "sleep 60", required: true}
	registry := tools.NewRegistry()
	registry.Register(tool)
	provider := &queuedStreamProvider{streams: []llm.StreamReader{toolUseStream("start", "RequiredJob", `{}`), textStream("done")}}
	loop := NewLoop(provider, registry, permission.New(permission.ModeAcceptEdits), nil, "system", 5)
	loop.Jobs = pool
	loop.JobNotify = pool.Notify()
	loop.AppendUser("wait for required job")
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	err = loop.Run(ctx, make(chan Event, 64))
	if err == nil {
		t.Fatal("required active job did not wait for caller cancellation")
	}
	job, ok := pool.Get(tool.id)
	if !ok || job.Status != jobs.StatusKilled {
		t.Fatalf("owned required job was not cleaned: %+v", job)
	}
	if unrelated, _ := pool.Get(server.ID); unrelated.Status != jobs.StatusRunning {
		t.Fatal("unrelated server was killed")
	}
}
