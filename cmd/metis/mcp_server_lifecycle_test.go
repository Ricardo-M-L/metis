package main

import (
	"context"
	"testing"
	"time"

	mcpsdk "github.com/Ricardo-M-L/metis/internal/mcp"
	"github.com/Ricardo-M-L/metis/internal/slash"
	"github.com/Ricardo-M-L/metis/internal/tools"
	mcptools "github.com/Ricardo-M-L/metis/internal/tools/mcp"
)

func lazyLifecycleServer(name string, toolNames ...string) *mcptools.Server {
	cached := make([]mcpsdk.Tool, 0, len(toolNames))
	for _, toolName := range toolNames {
		cached = append(cached, mcpsdk.Tool{Name: toolName})
	}
	return mcptools.NewLazyServer(name, cached, func(context.Context) (*mcpsdk.Client, error) {
		return nil, context.Canceled
	})
}

func TestRuntimeExplicitMCPAdoptionReplacesNamespaceAndRejectsLateStartup(t *testing.T) {
	rt := &runtime{registry: tools.NewRegistry()}
	old := lazyLifecycleServer("secure", "keep", "removed")
	if !rt.adoptMCPServer(old, old.Tools(), false) {
		t.Fatal("initial server was not adopted")
	}
	fresh := lazyLifecycleServer("secure", "keep", "added")
	if !rt.adoptMCPServer(fresh, fresh.Tools(), true) {
		t.Fatal("explicit server was not adopted")
	}
	if len(rt.mcpServers) != 1 || rt.mcpServers[0] != fresh {
		t.Fatalf("live servers after explicit adoption = %#v", rt.mcpServers)
	}
	if _, ok := rt.registry.Get("mcp__secure__removed"); ok {
		t.Fatal("stale tool from replaced server survived")
	}
	if _, ok := rt.registry.Get("mcp__secure__added"); !ok {
		t.Fatal("fresh server tool was not published")
	}

	late := lazyLifecycleServer("secure", "late")
	if rt.adoptMCPServer(late, late.Tools(), false) {
		t.Fatal("late startup server replaced explicit connection")
	}
	if _, ok := rt.registry.Get("mcp__secure__late"); ok {
		t.Fatal("late startup tools overwrote explicit namespace")
	}
}

func TestRuntimeMCPAdoptionHonorsInstalledVisibilityPolicy(t *testing.T) {
	reg := tools.NewRegistry()
	tools.ApplyToolVisibility(reg, nil, []string{"mcp__blocked"})
	rt := &runtime{registry: reg}
	server := lazyLifecycleServer("blocked", "late")
	if !rt.adoptMCPServer(server, server.Tools(), false) {
		t.Fatal("server ownership should be adopted even when all tools are filtered")
	}
	if _, ok := reg.Get("mcp__blocked__late"); ok {
		t.Fatal("late MCP adoption bypassed the installed visibility policy")
	}
	if len(rt.mcpServers) != 1 || rt.mcpServers[0] != server {
		t.Fatalf("resource/prompt server ownership was lost: %#v", rt.mcpServers)
	}
}

func TestRuntimeAdoptsResourceOnlyMCPServer(t *testing.T) {
	rt := &runtime{registry: tools.NewRegistry()}
	resourceOnly := lazyLifecycleServer("resources")
	if !rt.adoptMCPServer(resourceOnly, nil, true) {
		t.Fatal("resource-only server was not adopted")
	}
	if len(rt.mcpServers) != 1 || rt.mcpServers[0] != resourceOnly {
		t.Fatalf("resource-only live servers = %#v", rt.mcpServers)
	}
}

func TestRuntimeRejectsMCPAdoptionAfterCleanupStarts(t *testing.T) {
	rt := &runtime{registry: tools.NewRegistry(), mcpClosing: true}
	late := lazyLifecycleServer("late", "unsafe")
	if !rt.adoptMCPServer(late, late.Tools(), false) {
		t.Fatal("closing runtime did not consume the late MCP server")
	}
	if len(rt.mcpServers) != 0 {
		t.Fatalf("live servers after closing adoption = %#v", rt.mcpServers)
	}
	if _, ok := rt.registry.Get("mcp__late__unsafe"); ok {
		t.Fatal("late MCP tool was published after cleanup started")
	}
}

func TestRegisterLiveMCPPromptsDropsStalePromptFromPriorServer(t *testing.T) {
	registry := slash.NewRegistry()
	registry.Register(slash.Cmd{Name: "mcp__resources__stale", Source: "mcp:resources"})
	registry.Register(slash.Cmd{Name: "help", Source: "slash"})
	server := lazyLifecycleServer("resources")
	rt := &runtime{mcpServers: []*mcptools.Server{server}}

	rt.registerLiveMCPPrompts(context.Background(), registry, server)

	if _, ok := registry.Resolve("mcp__resources__stale"); ok {
		t.Fatal("stale prompt retained a prior server closure after reconnect")
	}
	if _, ok := registry.Resolve("help"); !ok {
		t.Fatal("unrelated slash command was removed with MCP prompt source")
	}
}

func TestRegisterLiveMCPPromptsIgnoresSupersededServer(t *testing.T) {
	registry := slash.NewRegistry()
	registry.Register(slash.Cmd{Name: "mcp__resources__current", Source: "mcp:resources"})
	old := lazyLifecycleServer("resources")
	current := lazyLifecycleServer("resources")
	rt := &runtime{mcpServers: []*mcptools.Server{current}}

	rt.registerLiveMCPPrompts(context.Background(), registry, old)

	if _, ok := registry.Resolve("mcp__resources__current"); !ok {
		t.Fatal("superseded prompt discovery removed the current server prompts")
	}
}

func TestScheduleMCPPromptsAfterLaunchIncludesResourceOnlyServer(t *testing.T) {
	registry := slash.NewRegistry()
	registry.Register(slash.Cmd{Name: "mcp__resources__stale", Source: "mcp:resources"})
	server := lazyLifecycleServer("resources")
	done := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rt := &runtime{
		mcpServers:      []*mcptools.Server{server},
		mcpLauncherDone: done,
		mcpPromptCtx:    ctx,
	}

	rt.scheduleMCPPromptsAfterLaunch(registry)
	// The startup reconciliation must wait for the launcher ownership boundary.
	if _, ok := registry.Resolve("mcp__resources__stale"); !ok {
		t.Fatal("prompt reconciliation ran before launcher completion")
	}
	close(done)
	completed := make(chan struct{})
	go func() {
		rt.mcpPromptWG.Wait()
		close(completed)
	}()
	select {
	case <-completed:
	case <-time.After(time.Second):
		t.Fatal("prompt reconciliation did not finish after launcher completion")
	}
	if _, ok := registry.Resolve("mcp__resources__stale"); ok {
		t.Fatal("resource-only server was omitted from post-launch prompt reconciliation")
	}
}
