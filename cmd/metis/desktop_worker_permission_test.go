package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/session"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

func permissionCheckpointRuntime(t *testing.T) *runtime {
	t.Helper()
	store, err := session.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	gate := permission.New(permission.ModePlan)
	loop := agent.NewLoop(nil, tools.NewRegistry(), gate, nil, "system", 2)
	loop.SetPrePlanMode("default")
	loop.SetPlanMode(true)
	rt := &runtime{store: store, sessionID: "worker-permission", gate: gate, loop: loop, permissionMode: permission.ModePlan}
	installRuntimePermissionListener(rt)
	return rt
}

func TestDesktopWorkerPermissionTransitionPersistsBeforeReturn(t *testing.T) {
	rt := permissionCheckpointRuntime(t)
	p := &desktopWorkerPermission{store: rt.store, id: rt.sessionID}
	if err := p.checkpoint(rt); err != nil {
		t.Fatal(err)
	}
	rt.permissionCheckpoint = p.commit
	// This reproduces ExitPlanMode's shared dispatch lease. Re-entering
	// CapturePermissionModeState here would deadlock on a lock upgrade.
	release, ok, reason := rt.gate.TryAcquireToolDispatchLease()
	if !ok {
		t.Fatal(reason)
	}
	done := make(chan struct{})
	go func() { rt.loop.SetPrePlanMode(""); rt.gate.SetModeAndWait(permission.ModeAcceptEdits); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("permission checkpoint deadlocked inside tool admission")
	}
	release()
	resumed, _ := session.NewStore(rt.store.Dir)
	h, _, err := resumed.LoadHeader(rt.sessionID)
	if err != nil || h.Mode != "acceptEdits" || h.PrePlanMode != "" {
		t.Fatalf("stale durable posture: %+v %v", h, err)
	}
	if state := p.snapshot(); state == nil || state.Mode != "acceptEdits" {
		t.Fatalf("status is not committed state: %+v", state)
	}
}

func TestDesktopWorkerPermissionFailureCancelsAndDeniesAllowedWrite(t *testing.T) {
	rt := permissionCheckpointRuntime(t)
	rt.gate.AppendRules(permission.Rule{Tool: "Write", Match: "*", Verb: permission.DecisionAllow, Source: "test"})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rt.permissionCheckpoint = func(permission.Mode, string) error { return errors.New("injected permission fsync failure") }
	rt.permissionCheckpointFailure = cancel
	rt.loop.SetPrePlanMode("")
	rt.gate.SetModeAndWait(permission.ModeAcceptEdits)
	if ctx.Err() == nil {
		t.Fatal("failed permission save did not cancel producer")
	}
	if rt.gate.Mode() != permission.ModePlan || rt.loop.PrePlanMode() != "default" {
		t.Fatal("failed approval did not restore complete plan posture")
	}
	if decision, _ := rt.gate.Check(context.Background(), "Write", "target.txt"); decision != permission.DecisionDeny {
		t.Fatal("failed approval still permits allow-rule write")
	}
}
