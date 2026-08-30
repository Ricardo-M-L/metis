package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/jobs"
	"github.com/Ricardo-M-L/metis/internal/llm/transport"
	"github.com/Ricardo-M-L/metis/internal/permission"
	rtpkg "github.com/Ricardo-M-L/metis/internal/runtime"
	"github.com/Ricardo-M-L/metis/internal/session"
	"github.com/Ricardo-M-L/metis/internal/tasks"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

func TestRuntimeRebindSessionMovesGlobalRouters(t *testing.T) {
	oldTodo := rtpkg.CurrentSessionID()
	oldDump := transport.GlobalSessionID()
	t.Cleanup(func() {
		rtpkg.SetCurrentSessionID(oldTodo)
		transport.SetSessionID(oldDump)
		tasks.SetCurrentTaskStore("")
	})

	gate := permission.New(permission.ModeAsk)
	loop := agent.NewLoop(nil, tools.NewRegistry(), gate, nil, "system", 3)
	rt := &runtime{loop: loop}
	rt.rebindSession("session-a")
	firstTaskStore := tasks.CurrentTaskStore()
	firstCheckpointer := loop.Checkpointer

	if rt.sessionID != "session-a" || rtpkg.CurrentSessionID() != "session-a" || transport.GlobalSessionID() != "session-a" {
		t.Fatalf("first bind incomplete: runtime=%q todo=%q dump=%q", rt.sessionID, rtpkg.CurrentSessionID(), transport.GlobalSessionID())
	}
	if firstTaskStore == nil || firstCheckpointer == nil {
		t.Fatalf("first bind missing task/checkpoint store: task=%v checkpoint=%v", firstTaskStore, firstCheckpointer)
	}

	rt.rebindSession("session-b")
	if rt.sessionID != "session-b" || rtpkg.CurrentSessionID() != "session-b" || transport.GlobalSessionID() != "session-b" {
		t.Fatalf("second bind incomplete: runtime=%q todo=%q dump=%q", rt.sessionID, rtpkg.CurrentSessionID(), transport.GlobalSessionID())
	}
	if tasks.CurrentTaskStore() == firstTaskStore {
		t.Fatal("structured Task store was not replaced at session boundary")
	}
	if loop.Checkpointer == firstCheckpointer {
		t.Fatal("working-tree checkpointer was not replaced at session boundary")
	}
}

func TestSetupRuntimeBindsInitialTraceSession(t *testing.T) {
	isolateResumeRuntimeTest(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	rt, err := setupRuntime(ctx, &cliFlags{
		newSessionID: "initial-trace-session",
		bare:         true,
		noAuthWizard: true,
	})
	if err != nil {
		t.Fatalf("setupRuntime: %v", err)
	}
	adapter := rtpkg.CurrentTraceAdapter()
	store := rtpkg.CurrentTraceStore()
	t.Cleanup(func() {
		rt.Cleanup()
		agent.SetTraceHook(nil)
		rtpkg.SetTraceAdapter(nil)
		if store != nil {
			_ = store.Close()
		}
	})
	if adapter == nil || store == nil {
		t.Fatal("setupRuntime did not install the process trace")
	}

	adapter.OnEvent(agent.Event{Kind: agent.EventLoopDone, StopReason: "test"})
	events := store.Events(rt.sessionID)
	if len(events) != 1 || events[0].Kind != "loop_done" {
		t.Fatalf("initial session trace was not bound: session=%q events=%+v", rt.sessionID, events)
	}
}

func TestRuntimeCleanupFlushesTraceBeforeWaitingForDependencies(t *testing.T) {
	oldAdapter := rtpkg.CurrentTraceAdapter()
	traceDir := t.TempDir()
	store, err := session.NewTraceStore(traceDir)
	if err != nil {
		t.Fatal(err)
	}
	adapter := rtpkg.NewTraceAdapter(store)
	adapter.SetSession("shutdown-trace")
	rtpkg.SetTraceAdapter(adapter)
	t.Cleanup(func() {
		rtpkg.SetTraceAdapter(oldAdapter)
		_ = store.Close()
	})

	adapter.OnEvent(agent.Event{Kind: agent.EventTextDelta, TextDelta: "last partial response"})
	launcherDone := make(chan struct{})
	rt := &runtime{mcpLauncherDone: launcherDone}
	cleanupDone := make(chan struct{})
	go func() {
		rt.Cleanup()
		close(cleanupDone)
	}()

	deadline := time.Now().Add(250 * time.Millisecond)
	tracePath := filepath.Join(traceDir, "shutdown-trace.jsonl")
	for {
		raw, readErr := os.ReadFile(tracePath)
		if readErr == nil && strings.Contains(string(raw), "last partial response") {
			break
		}
		if time.Now().After(deadline) {
			close(launcherDone)
			<-cleanupDone
			t.Fatalf("trace was not flushed before dependency wait; path=%s err=%v contents=%q", tracePath, readErr, raw)
		}
		time.Sleep(10 * time.Millisecond)
	}

	select {
	case <-cleanupDone:
		t.Fatal("Cleanup returned before the simulated dependency launcher finished")
	default:
	}
	close(launcherDone)
	<-cleanupDone
}

func TestRuntimeRebindSessionUpdatesCrashRecoveryPointer(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	oldTodo := rtpkg.CurrentSessionID()
	oldDump := transport.GlobalSessionID()
	t.Cleanup(func() {
		rtpkg.SetCurrentSessionID(oldTodo)
		transport.SetSessionID(oldDump)
		tasks.SetCurrentTaskStore("")
	})
	cwd := t.TempDir()
	rt := &runtime{sessionPointerCwd: cwd}
	rt.rebindSession("session-after-new")

	pointer, err := session.ReadPointer(cwd)
	if err != nil {
		t.Fatal(err)
	}
	if pointer == nil || pointer.SessionID != "session-after-new" || pointer.CWD != cwd {
		t.Fatalf("pointer after rebind = %+v", pointer)
	}
}

func TestRuntimeRebindSessionAtUsesDesktopTargetWorkspace(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	workspaceA := t.TempDir()
	workspaceB := t.TempDir()
	canonicalWorkspaceB, err := filepath.EvalSymlinks(workspaceB)
	if err != nil {
		t.Fatal(err)
	}
	targetFile := filepath.Join(canonicalWorkspaceB, "target-only.txt")
	if err := os.WriteFile(targetFile, []byte("target workspace"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := session.WritePointer("session-a", workspaceA); err != nil {
		t.Fatal(err)
	}
	loop := agent.NewLoop(nil, tools.NewRegistry(), permission.New(permission.ModeAsk), nil, "system", 3)
	rt := &runtime{loop: loop, sessionPointerCwd: workspaceA}
	rt.rebindSessionAt("session-b", workspaceB)

	if loop.Checkpointer == nil {
		t.Fatal("Desktop target workspace did not install a checkpointer")
	}
	states, err := loop.Checkpointer.CapturePathStates([]string{targetFile})
	if err != nil {
		t.Fatalf("capture target workspace path: %v", err)
	}
	if _, ok := states["target-only.txt"]; !ok {
		t.Fatalf("checkpointer is not rooted at target workspace: %+v", states)
	}
	pointerB, err := session.ReadPointer(canonicalWorkspaceB)
	if err != nil {
		t.Fatal(err)
	}
	if pointerB == nil || pointerB.SessionID != "session-b" || pointerB.CWD != canonicalWorkspaceB {
		t.Fatalf("target workspace pointer = %+v", pointerB)
	}
	pointerA, err := session.ReadPointer(workspaceA)
	if err != nil {
		t.Fatal(err)
	}
	if pointerA != nil {
		t.Fatalf("source workspace pointer remained after Desktop switch: %+v", pointerA)
	}
}

func TestRuntimeReleaseSessionWorkCancelsRosterAndIsNilSafe(t *testing.T) {
	cancelled := 0
	roster := agent.NewRoster(1)
	if err := roster.Register(&agent.Teammate{Name: "worker", Cancel: func() { cancelled++ }}); err != nil {
		t.Fatal(err)
	}
	finished := &agent.Teammate{Name: "finished", AgentID: "agt-old-finished"}
	// The configured cap is split by kind, so use the anonymous slot for a
	// separate completed entry while the named worker remains live.
	finished.Anonymous = true
	if err := roster.Register(finished); err != nil {
		t.Fatal(err)
	}
	roster.Unregister(finished.Name)
	gate := permission.New(permission.ModeAsk)
	loop := agent.NewLoop(nil, tools.NewRegistry(), gate, nil, "system", 3)
	jobsPool := jobs.NewRegistry(t.TempDir())
	ctx, cancelJob := context.WithCancel(context.Background())
	oldJob, err := jobsPool.Spawn(jobs.SpawnArgs{
		Command: "sleep 30",
		Cmd:     exec.CommandContext(ctx, "sh", "-c", "sleep 30"),
		Cancel:  cancelJob,
	})
	if err != nil {
		t.Fatal(err)
	}
	loop.Jobs = jobsPool
	loop.JobNotify = jobsPool.Notify()
	loop.Monitors = agent.NewMonitorRegistry(1)
	cronSvc, err := agent.NewCronService(filepath.Join(t.TempDir(), "cron"))
	if err != nil {
		t.Fatal(err)
	}
	ephemeral := &agent.CronJob{
		ID: "session-only", Prompt: "old session", Enabled: true, Ephemeral: true,
		Schedule: agent.CronSchedule{Kind: "every", EveryMs: int64(time.Hour / time.Millisecond)},
	}
	durable := &agent.CronJob{
		ID: "durable", Prompt: "global", Enabled: true,
		Schedule: agent.CronSchedule{Kind: "every", EveryMs: int64(time.Hour / time.Millisecond)},
	}
	if err := cronSvc.Create(ephemeral); err != nil {
		t.Fatal(err)
	}
	if err := cronSvc.Create(durable); err != nil {
		t.Fatal(err)
	}
	rt := &runtime{loop: loop, cronSvc: cronSvc, subAgentRoster: roster}

	// Cleanup owns final process shutdown and must invoke the same boundary
	// release used by /new, /branch and /resume.
	rt.Cleanup()
	if cancelled != 1 || roster.Count() != 0 {
		t.Fatalf("session work not cancelled: cancelled=%v roster=%d", cancelled, roster.Count())
	}
	if got := roster.List(); len(got) != 0 {
		t.Fatalf("session boundary retained roster state: %+v", got)
	}
	if _, ok := roster.LookupByAgentID(finished.AgentID); ok {
		t.Fatal("session boundary retained recently-finished sub-agent output")
	}
	if got := jobsPool.List(); len(got) != 0 {
		t.Fatalf("session boundary retained Bash jobs: %+v", got)
	}
	if _, ok := jobsPool.Get(oldJob.ID); ok {
		t.Fatal("session boundary retained addressable Bash output")
	}
	if _, ok := cronSvc.Get(ephemeral.ID); ok {
		t.Fatal("session boundary retained an ephemeral cron job")
	}
	if _, ok := cronSvc.Get(durable.ID); !ok {
		t.Fatal("session boundary removed a durable cron job")
	}
	rt.Cleanup()
	if cancelled != 1 {
		t.Fatalf("idempotent cleanup cancelled an already-cleared roster %d times", cancelled)
	}
	if err := roster.Register(&agent.Teammate{Name: "new-worker", AgentID: "agt-new"}); err != nil {
		t.Fatalf("reset roster rejected destination-session teammate: %v", err)
	}
	if _, ok := roster.LookupByAgentID("agt-new"); !ok {
		t.Fatal("destination-session teammate unavailable through retained roster pointer")
	}
	newJobCtx, cancelNewJob := context.WithCancel(context.Background())
	newJob, err := jobsPool.Spawn(jobs.SpawnArgs{
		Command: "echo new-session",
		Cmd:     exec.CommandContext(newJobCtx, "sh", "-c", "echo new-session"),
		Cancel:  cancelNewJob,
	})
	if err != nil {
		t.Fatalf("reset jobs registry rejected destination-session job: %v", err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if snap, ok := jobsPool.Get(newJob.ID); ok && snap.Status == jobs.StatusCompleted {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if snap, ok := jobsPool.Get(newJob.ID); !ok || snap.Status != jobs.StatusCompleted {
		t.Fatalf("destination-session job unavailable through retained registry pointer: ok=%v job=%+v", ok, snap)
	}
	jobsPool.Reset(0)
	roster.Reset()
	(&runtime{}).releaseSessionWork()
	var nilRuntime *runtime
	nilRuntime.releaseSessionWork()
}
