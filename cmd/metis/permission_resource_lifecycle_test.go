package main

import (
	"testing"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/permission"
	rtpkg "github.com/Ricardo-M-L/metis/internal/runtime"
	"github.com/Ricardo-M-L/metis/internal/sandbox"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

func TestRuntimePermissionListenerRevokesOnDirectFullAccessExit(t *testing.T) {
	manager, err := sandbox.NewManager(string(sandbox.ModeOff))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close() })
	gate := permission.New(permission.ModeDefault)
	loop := &agent.Loop{}
	rt := &runtime{
		gate:           gate,
		loop:           loop,
		sandbox:        manager,
		registry:       tools.NewRegistry(),
		permissionMode: permission.ModeDefault,
	}
	installRuntimePermissionListener(rt)

	// An already-running MCP was launched under the prior safe posture. Entering
	// fullAccess must not weaken/restart it; only the exit boundary drains it,
	// because a lazy client could have first spawned while fullAccess was live.
	server := lazyLifecycleServer("direct", "unsafe")
	if !rt.adoptMCPServer(server, server.Tools(), true) {
		t.Fatal("adopt pre-fullAccess server")
	}
	if err := rtpkg.ApplyPermissionMode(gate, loop, manager, permission.ModeFullAccess); err != nil {
		t.Fatalf("enter fullAccess: %v", err)
	}
	if len(rt.mcpServers) != 1 || rt.mcpServers[0] != server {
		t.Fatalf("entering fullAccess restarted or removed prior sandboxed MCP: %#v", rt.mcpServers)
	}
	if err := rtpkg.ApplyPermissionMode(gate, loop, manager, permission.ModeDefault); err != nil {
		t.Fatalf("leave fullAccess: %v", err)
	}
	if len(rt.mcpServers) != 0 {
		t.Fatalf("direct permission transition retained MCP: %#v", rt.mcpServers)
	}
	if _, ok := rt.registry.Get("mcp__direct__unsafe"); ok {
		t.Fatal("direct permission transition retained MCP namespace")
	}
}

func TestRuntimePermissionListenerRevokesWhenPlanOverlaysFullAccess(t *testing.T) {
	manager, err := sandbox.NewManager(string(sandbox.ModeOff))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close() })
	gate := permission.New(permission.ModeFullAccess)
	loop := &agent.Loop{}
	loop.SetPrePlanMode(string(permission.ModeFullAccess))
	rt := &runtime{
		gate:           gate,
		loop:           loop,
		sandbox:        manager,
		registry:       tools.NewRegistry(),
		permissionMode: permission.ModeFullAccess,
	}
	installRuntimePermissionListener(rt)
	server := lazyLifecycleServer("plan", "unsafe")
	if !rt.adoptMCPServer(server, server.Tools(), true) {
		t.Fatal("adopt fullAccess server")
	}

	// EnterPlanMode and restored sessions use Gate.SetMode internally rather
	// than the public ApplyPermissionMode path. The listener is the lifecycle
	// boundary for those transitions.
	gate.SetMode(permission.ModePlan)
	if len(rt.mcpServers) != 0 {
		t.Fatalf("plan overlay retained fullAccess MCP: %#v", rt.mcpServers)
	}
	if _, ok := rt.registry.Get("mcp__plan__unsafe"); ok {
		t.Fatal("plan overlay retained fullAccess MCP namespace")
	}
}

func TestRuntimePermissionListenerRevokesOnRestoredSessionReset(t *testing.T) {
	manager, err := sandbox.NewManager(string(sandbox.ModeOff))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close() })
	gate := permission.New(permission.ModeFullAccess)
	loop := &agent.Loop{}
	rt := &runtime{
		gate:           gate,
		loop:           loop,
		sandbox:        manager,
		registry:       tools.NewRegistry(),
		permissionMode: permission.ModeFullAccess,
	}
	installRuntimePermissionListener(rt)
	server := lazyLifecycleServer("restore", "unsafe")
	if !rt.adoptMCPServer(server, server.Tools(), true) {
		t.Fatal("adopt fullAccess server")
	}

	// ApplyPreparedResume restores a session through ResetSessionState rather
	// than the direct ApplyPermissionMode entry point.
	gate.ResetSessionState(permission.ModeDefault, nil)
	if len(rt.mcpServers) != 0 {
		t.Fatalf("session restore retained fullAccess MCP: %#v", rt.mcpServers)
	}
	if _, ok := rt.registry.Get("mcp__restore__unsafe"); ok {
		t.Fatal("session restore retained fullAccess MCP namespace")
	}
}
