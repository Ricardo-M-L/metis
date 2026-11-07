package main

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/session"
	"github.com/Ricardo-M-L/metis/internal/tasks"
	"github.com/Ricardo-M-L/metis/internal/tools"
	"github.com/Ricardo-M-L/metis/internal/tools/builtin"
)

func TestHeadlessWakeupCancellationLeavesUnclaimedNoDetachedRun(t *testing.T) {
	svc, err := agent.NewCronService(filepath.Join(t.TempDir(), "cron"))
	if err != nil {
		t.Fatal(err)
	}
	gate := permission.New(permission.ModeBypassPermissions)
	rt := &runtime{cronSvc: svc, gate: gate, registry: tools.NewRegistry(), sessionID: "original", providerName: "fixture", model: "fixture"}
	rt.registry.Register(builtin.NewScheduleWakeup(gate, svc))
	rt.bindWakeupContinuation()
	tool, _ := rt.registry.Get("ScheduleWakeup")
	result, err := tool.Execute(tasks.WithSessionID(context.Background(), "original"), map[string]any{"delaySeconds": 30, "prompt": "authorized wakeup", "reason": "cancel test"})
	if err != nil || result.IsError {
		t.Fatalf("create: %+v %v", result, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	run, err := awaitHeadlessWakeup(ctx, rt)
	if run != nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("run=%+v err=%v", run, err)
	}
	jobs := svc.List()
	if len(jobs) != 1 || jobs[0].RunCount != 0 || jobs[0].ContinuationRunID != "" {
		t.Fatalf("cancel admitted background work: %+v", jobs)
	}
	runs, err := agent.ListCronRuns(svc.Root(), jobs[0].ID, 10)
	if err != nil || len(runs) != 0 {
		t.Fatalf("cancel created execution receipt: %+v %v", runs, err)
	}
}

func TestHeadlessWakeupRejectsResumedSessionInDifferentWorkspace(t *testing.T) {
	root := t.TempDir()
	svc, err := agent.NewCronService(filepath.Join(root, "cron"))
	if err != nil {
		t.Fatal(err)
	}
	store, err := session.NewStore(filepath.Join(root, "sessions"))
	if err != nil {
		t.Fatal(err)
	}
	if err = store.WriteHeaderFull(session.Header{ID: "original", WorkDir: t.TempDir(), Provider: "fixture", Model: "fixture"}); err != nil {
		t.Fatal(err)
	}
	gate := permission.New(permission.ModeBypassPermissions)
	rt := &runtime{store: store, cronSvc: svc, gate: gate, registry: tools.NewRegistry(), sessionID: "original", providerName: "fixture", model: "fixture"}
	rt.registry.Register(builtin.NewScheduleWakeup(gate, svc))
	rt.bindWakeupContinuation()
	tool, _ := rt.registry.Get("ScheduleWakeup")
	result, err := tool.Execute(tasks.WithSessionID(context.Background(), "original"), map[string]any{"delaySeconds": 30, "prompt": "forbidden crossed workspace", "reason": "test"})
	if err != nil || result == nil || !result.IsError || len(svc.List()) != 0 {
		t.Fatalf("cross-workspace scheduling accepted: result=%+v err=%v jobs=%+v", result, err, svc.List())
	}
	run, err := awaitHeadlessWakeup(context.Background(), rt)
	if err == nil || run != nil {
		t.Fatalf("workspace mismatch silently ignored: run=%+v err=%v", run, err)
	}
}

func TestRuntimeWakeupBindingDoesNotReintroduceFilteredTool(t *testing.T) {
	svc, err := agent.NewCronService(filepath.Join(t.TempDir(), "cron"))
	if err != nil {
		t.Fatal(err)
	}
	rt := &runtime{cronSvc: svc, registry: tools.NewRegistry()}
	rt.bindWakeupContinuation()
	if _, ok := rt.registry.Get("ScheduleWakeup"); ok {
		t.Fatal("binding bypassed tool visibility filters")
	}
}
