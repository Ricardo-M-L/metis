package main

import (
	"context"
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/sandbox"
	"github.com/Ricardo-M-L/metis/internal/tools"
	"github.com/Ricardo-M-L/metis/internal/tools/builtin"
)

func TestRuntimePlanSandboxFailureNeverReleasesReadOnlyBoundary(t *testing.T) {
	for _, from := range []permission.Mode{permission.ModeDefault, permission.ModeAcceptEdits, permission.ModeBypassPermissions, permission.ModeFullAccess} {
		t.Run(string(from), func(t *testing.T) {
			manager, err := sandbox.NewManager("off")
			if err != nil {
				t.Fatal(err)
			}
			if err = manager.Close(); err != nil {
				t.Fatal(err)
			}
			gate := permission.New(from)
			gate.AppendRules(permission.Rule{Tool: "Write", Verb: permission.DecisionAllow, Source: "test"})
			loop := &agent.Loop{Gate: gate}
			rt := &runtime{gate: gate, loop: loop, sandbox: manager, permissionMode: from}
			installRuntimePermissionListener(rt)
			release, ok, reason := gate.TryAcquireToolDispatchLease()
			if !ok {
				t.Fatal(reason)
			}
			defer release()
			result, err := builtin.NewEnterPlanModeWithGate(gate).Execute(agent.WithPlanController(context.Background(), loop), nil)
			if err != nil || result == nil {
				t.Fatalf("enter plan: %+v %v", result, err)
			}
			if gate.Mode() != permission.ModePlan || !loop.IsPlanMode() {
				t.Fatalf("sandbox failure released Plan: gate=%s plan=%v result=%+v", gate.Mode(), loop.IsPlanMode(), result)
			}
			if decision, reason := gate.Check(context.Background(), "Write", "target.txt"); decision != permission.DecisionDeny {
				t.Fatalf("sandbox failure permits pre-approved Write: %v %s", decision, reason)
			}
			if decision, reason := gate.Check(context.Background(), "Read", "target.txt"); decision != permission.DecisionAllow {
				t.Fatalf("read-only work unavailable: %v %s", decision, reason)
			}
		})
	}
}

func TestRuntimeFailedPlanExitReturnsToReadOnlyPlan(t *testing.T) {
	manager, err := sandbox.NewManager("off")
	if err != nil {
		t.Fatal(err)
	}
	if err = manager.Close(); err != nil {
		t.Fatal(err)
	}
	gate := permission.New(permission.ModePlan)
	gate.AppendRules(permission.Rule{Tool: "Write", Verb: permission.DecisionAllow, Source: "test"})
	loop := &agent.Loop{Gate: gate}
	loop.SetPrePlanMode("default")
	loop.SetPlanMode(true)
	rt := &runtime{gate: gate, loop: loop, sandbox: manager, permissionMode: permission.ModePlan}
	installRuntimePermissionListener(rt)
	// ExitPlanMode clears lineage before committing the approved target. A
	// sandbox failure still must not leave allow rules executable in dontAsk.
	loop.SetPrePlanMode("")
	gate.SetModeAndWait(permission.ModeDefault)
	if gate.Mode() != permission.ModePlan || !loop.IsPlanMode() || loop.PrePlanMode() != "default" {
		t.Fatalf("failed exit did not restore complete Plan: %s %v %s", gate.Mode(), loop.IsPlanMode(), loop.PrePlanMode())
	}
	if d, _ := gate.Check(context.Background(), "Write", "target.txt"); d != permission.DecisionDeny {
		t.Fatal("failed exit permits Write")
	}
}

func TestRuntimePlanEntryRevokesMCPFromEveryPriorMode(t *testing.T) {
	for _, from := range []permission.Mode{permission.ModeDefault, permission.ModeAcceptEdits, permission.ModeDontAsk, permission.ModeBypassPermissions, permission.ModeFullAccess} {
		t.Run(string(from), func(t *testing.T) {
			registry := tools.NewRegistry()
			gate := permission.New(from)
			loop := &agent.Loop{Gate: gate}
			loop.SetPrePlanMode(string(from))
			rt := &runtime{gate: gate, loop: loop, registry: registry, permissionMode: from}
			installRuntimePermissionListener(rt)
			server := lazyLifecycleServer("plan-edge", "unsafe")
			t.Cleanup(func() { _ = server.Close() })
			if !rt.adoptMCPServer(server, server.Tools(), true) {
				t.Fatal("adopt old MCP")
			}
			gate.SetModeAndWait(permission.ModePlan)
			if len(rt.mcpServers) != 0 {
				t.Fatal("Plan retained old MCP process generation")
			}
			if _, ok := registry.Get("mcp__plan-edge__unsafe"); ok {
				t.Fatal("Plan retained old MCP tool")
			}
			// Once Plan is active, redundant mode notifications must not revoke
			// a new explicitly reconnected read-only resource generation.
			fresh := lazyLifecycleServer("plan-fresh", "safe")
			t.Cleanup(func() { _ = fresh.Close() })
			if !rt.adoptMCPServer(fresh, fresh.Tools(), true) {
				t.Fatal("adopt fresh MCP")
			}
			gate.SetModeAndWait(permission.ModePlan)
			if len(rt.mcpServers) != 1 {
				t.Fatal("redundant Plan notification drained fresh resources")
			}
		})
	}
}

func TestRuntimePlanEntryWaitsForLaunchAndSettlesConcurrentMode(t *testing.T) {
	gate := permission.New(permission.ModeAcceptEdits)
	loop := &agent.Loop{Gate: gate}
	loop.SetPrePlanMode("acceptEdits")
	rt := &runtime{gate: gate, loop: loop, permissionMode: permission.ModeAcceptEdits}
	installRuntimePermissionListener(rt)
	ticket := rt.beginExplicitMCPLaunch(context.Background())
	defer ticket.Finish()
	done := make(chan struct{})
	go func() { gate.SetModeAndWait(permission.ModePlan); close(done) }()
	select {
	case <-ticket.Context().Done():
	case <-time.After(time.Second):
		t.Fatal("Plan did not cancel in-flight MCP launch")
	}
	select {
	case <-done:
		t.Fatal("Plan returned before launch joined")
	default:
	}
	// A newer manual decision can supersede a draining transition. The Gate
	// keeps dispatch closed until both the revoke and latest listener settle.
	gate.SetMode(permission.ModeDefault)
	ticket.Finish()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Plan drain deadlocked with concurrent mode")
	}
	if gate.Mode() != permission.ModeDefault || loop.IsPlanMode() {
		t.Fatalf("concurrent mode lost: %s plan=%v", gate.Mode(), loop.IsPlanMode())
	}
}
