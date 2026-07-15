package main

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/internal/agent"
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

func TestRuntimeReleaseSessionWorkCancelsRosterAndIsNilSafe(t *testing.T) {
	cancelled := 0
	roster := agent.NewRoster(1)
	if err := roster.Register(&agent.Teammate{Name: "worker", Cancel: func() { cancelled++ }}); err != nil {
		t.Fatal(err)
	}
	gate := permission.New(permission.ModeAsk)
	loop := agent.NewLoop(nil, tools.NewRegistry(), gate, nil, "system", 3)
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
	(&runtime{}).releaseSessionWork()
	var nilRuntime *runtime
	nilRuntime.releaseSessionWork()
}
