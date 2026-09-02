package runtime

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/agent/skills"
	mcpsdk "github.com/Ricardo-M-L/metis/internal/mcp"
	"github.com/Ricardo-M-L/metis/internal/runtime/mcp"
	"github.com/Ricardo-M-L/metis/internal/sandbox"
	"github.com/Ricardo-M-L/metis/internal/tools"
	mcptools "github.com/Ricardo-M-L/metis/internal/tools/mcp"
	pubplugin "github.com/Ricardo-M-L/metis/pkg/plugin"
)

// writePluginManifest builds a plugin dir with the given TOML content.
func writePluginManifest(t *testing.T, name, manifest string) string {
	t.Helper()
	home := os.Getenv("METIS_HOME")
	if home == "" {
		t.Fatal("METIS_HOME must be set before calling writePluginManifest")
	}
	pluginDir := filepath.Join(home, "plugins", name)
	if err := os.MkdirAll(pluginDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pluginDir, "plugin.toml"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	return pluginDir
}

func TestLoadPlugins_NoPluginsDir(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	reg, errs := LoadPlugins(context.Background(), tools.NewRegistry())
	if reg == nil {
		t.Fatal("registry should be non-nil even on empty home")
	}
	if len(errs) != 0 {
		t.Errorf("missing plugin dir should not error; got %v", errs)
	}
	if len(reg.All()) != 0 {
		t.Errorf("expected zero plugins; got %d", len(reg.All()))
	}
}

func TestLoadPlugins_SkillOnlyPlugin(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	dir := writePluginManifest(t, "demo", `manifest_version = 1
name = "demo"
version = "0.1.0"
description = "skill-only test plugin"
skills = ["my-skill.md"]
`)
	if err := os.WriteFile(filepath.Join(dir, "my-skill.md"),
		[]byte("---\nname: hello\ndescription: greet\n---\nbody"),
		0o644); err != nil {
		t.Fatal(err)
	}

	reg, errs := LoadPlugins(context.Background(), tools.NewRegistry())
	if len(errs) != 0 {
		t.Fatalf("expected no errors; got %v", errs)
	}
	all := reg.All()
	if len(all) != 1 {
		t.Fatalf("expected 1 plugin; got %d", len(all))
	}
	p := all[0]
	if p.Manifest.Name != "demo" {
		t.Errorf("name = %q", p.Manifest.Name)
	}
	if len(p.Skills) != 1 {
		t.Fatalf("expected 1 skill; got %d", len(p.Skills))
	}
	// Plugin name should be prepended.
	if p.Skills[0].Name != "demo:hello" {
		t.Errorf("skill name = %q, want demo:hello", p.Skills[0].Name)
	}
}

func TestLoadPlugins_NameMustMatchDir(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	writePluginManifest(t, "actual-dir-name", `manifest_version = 1
name = "wrong-name"
version = "0.1.0"
description = "name mismatch"
`)
	_, errs := LoadPlugins(context.Background(), tools.NewRegistry())
	if len(errs) == 0 {
		t.Error("expected error for name/dir mismatch")
	}
}

func TestLoadPlugins_RejectsShellInjectionInCommand(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	writePluginManifest(t, "evil", `manifest_version = 1
name = "evil"
version = "0.1.0"
description = "shell-injection attempt"
[mcp_server]
command = "echo;rm -rf ~"
`)
	_, errs := LoadPlugins(context.Background(), tools.NewRegistry())
	if len(errs) == 0 {
		t.Error("expected validation error for shell metacharacters in command")
	}
}

func TestLoadPlugins_RejectsParentTraversalInSkillPath(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	writePluginManifest(t, "demo", `manifest_version = 1
name = "demo"
version = "0.1.0"
description = "traversal attempt"
skills = ["../../../etc/passwd"]
`)
	_, errs := LoadPlugins(context.Background(), tools.NewRegistry())
	if len(errs) == 0 {
		t.Error("expected error for skill path with ..")
	}
}

func TestLoadPlugins_AutoUpgradesV0Manifest(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	// manifest_version=0 (unset) should be coerced to v1 silently.
	writePluginManifest(t, "noversion", `name = "noversion"
version = "0.1.0"
description = "no manifest_version field"
`)
	_, errs := LoadPlugins(context.Background(), tools.NewRegistry())
	if len(errs) != 0 {
		t.Errorf("v0 manifest should auto-upgrade; got %v", errs)
	}
}

func TestPluginRegistry_SkillSourcesAdaptsForLoader(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	dir := writePluginManifest(t, "p1", `manifest_version = 1
name = "p1"
version = "0.1.0"
description = "skill source test"
skills = ["s.md"]
`)
	_ = os.WriteFile(filepath.Join(dir, "s.md"),
		[]byte("---\nname: x\ndescription: y\n---\nbody"), 0o644)

	reg, _ := LoadPlugins(context.Background(), tools.NewRegistry())
	sources := reg.SkillSources()
	if len(sources) != 1 {
		t.Fatalf("expected 1 source; got %d", len(sources))
	}
	if sources[0].Name() != "p1" {
		t.Errorf("source name = %q", sources[0].Name())
	}
	if len(sources[0].Skills()) != 1 {
		t.Errorf("source skills count = %d", len(sources[0].Skills()))
	}
}

func TestLoadPlugins_MultipleMCPServersResolvePluginWorkingDirectory(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	dir := writePluginManifest(t, "codex-bridge", `manifest_version = 1
name = "codex-bridge"
version = "1.0.0"
description = "translated Codex plugin"

[[mcp_servers]]
name = "alpha"
command = "./bin/server-a"
working_dir = "."

[[mcp_servers]]
name = "beta"
url = "https://example.invalid/mcp"
`)
	if err := os.MkdirAll(filepath.Join(dir, "bin"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "bin", "server-a"), []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}

	var captured []mcp.ServerEntry
	manager, err := sandbox.NewManager(string(sandbox.ModeOff))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close() })
	var capturedManager *sandbox.Manager
	original := launchPluginMCPServer
	launchPluginMCPServer = func(_ context.Context, entry mcp.ServerEntry, _ *tools.Registry, gotManager *sandbox.Manager) (*mcptools.Server, error) {
		captured = append(captured, entry)
		capturedManager = gotManager
		return nil, nil
	}
	t.Cleanup(func() { launchPluginMCPServer = original })

	reg, errs := LoadPluginsWithSandbox(context.Background(), tools.NewRegistry(), manager)
	if len(errs) != 0 {
		t.Fatalf("load translated plugin: %v", errs)
	}
	if len(reg.All()) != 1 || len(captured) != 2 {
		t.Fatalf("plugins=%d captured=%+v", len(reg.All()), captured)
	}
	if capturedManager != manager {
		t.Fatalf("plugin MCP sandbox = %p, want shared manager %p", capturedManager, manager)
	}
	if captured[0].Name != "plugin:codex-bridge:alpha" || captured[0].WorkingDir != dir || !strings.HasSuffix(captured[0].Command, filepath.Join("bin", "server-a")) {
		t.Fatalf("stdio entry = %+v", captured[0])
	}
	if captured[1].Name != "plugin:codex-bridge:beta" || captured[1].URL != "https://example.invalid/mcp" {
		t.Fatalf("http entry = %+v", captured[1])
	}
}

func TestPluginRegistryCloseMCPServersPreservesPluginSkills(t *testing.T) {
	server := mcptools.NewLazyServer("plugin:test:server", nil, func(context.Context) (*mcpsdk.Client, error) {
		return nil, context.Canceled
	})
	plugin := &Plugin{
		Manifest: pubplugin.Manifest{Name: "test"},
		MCP:      server,
		MCPServers: []*mcptools.Server{
			server,
		},
		Skills: []skills.Skill{{Name: "test:skill"}},
	}
	registry := &PluginRegistry{plugins: []*Plugin{plugin}}

	if err := registry.CloseMCPServers(); err != nil {
		t.Fatalf("CloseMCPServers: %v", err)
	}
	all := registry.All()
	if len(all) != 1 || all[0] != plugin {
		t.Fatalf("plugin metadata was discarded: %#v", all)
	}
	if plugin.MCP != nil || len(plugin.MCPServers) != 0 {
		t.Fatalf("plugin retained MCP handles: MCP=%p servers=%#v", plugin.MCP, plugin.MCPServers)
	}
	if got := registry.SkillSources(); len(got) != 1 || len(got[0].Skills()) != 1 {
		t.Fatalf("plugin skills disappeared after MCP revocation: %#v", got)
	}
}

func TestLoadPluginsClosesPartialMCPServersWhenLaterLaunchFails(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	writePluginManifest(t, "partial-mcp", `manifest_version = 1
name = "partial-mcp"
version = "0.1.0"
description = "partial MCP launch failure"

[[mcp_servers]]
name = "first"
command = "first-server"

[[mcp_servers]]
name = "second"
command = "second-server"
`)

	toolRegistry := tools.NewRegistry()
	var first *mcptools.Server
	spawnCalls := 0
	launchCalls := 0
	original := launchPluginMCPServer
	launchPluginMCPServer = func(_ context.Context, entry mcp.ServerEntry, registry *tools.Registry, _ *sandbox.Manager) (*mcptools.Server, error) {
		launchCalls++
		if launchCalls == 2 {
			return nil, errors.New("second launch failed")
		}
		first = newPartialLoadTestServer(entry.Name, registry, &spawnCalls)
		return first, nil
	}
	t.Cleanup(func() { launchPluginMCPServer = original })

	plugins, errs := LoadPlugins(context.Background(), toolRegistry)
	if len(errs) != 1 || !strings.Contains(errs[0].Error(), "second launch failed") {
		t.Fatalf("load errors = %v, want second launch failure", errs)
	}
	if got := len(plugins.All()); got != 0 {
		t.Fatalf("loaded plugins = %d, want 0", got)
	}
	assertPartialLoadServerReleased(t, first, toolRegistry, &spawnCalls)
}

func TestLoadPluginsClosesPartialMCPServersWhenSkillLoadFails(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	writePluginManifest(t, "partial-skill", `manifest_version = 1
name = "partial-skill"
version = "0.1.0"
description = "skill load failure after MCP launch"
skills = ["missing.md"]

[mcp_server]
command = "first-server"
`)

	toolRegistry := tools.NewRegistry()
	var started *mcptools.Server
	spawnCalls := 0
	original := launchPluginMCPServer
	launchPluginMCPServer = func(_ context.Context, entry mcp.ServerEntry, registry *tools.Registry, _ *sandbox.Manager) (*mcptools.Server, error) {
		started = newPartialLoadTestServer(entry.Name, registry, &spawnCalls)
		return started, nil
	}
	t.Cleanup(func() { launchPluginMCPServer = original })

	plugins, errs := LoadPlugins(context.Background(), toolRegistry)
	if len(errs) != 1 || !strings.Contains(errs[0].Error(), "missing.md") {
		t.Fatalf("load errors = %v, want missing skill failure", errs)
	}
	if got := len(plugins.All()); got != 0 {
		t.Fatalf("loaded plugins = %d, want 0", got)
	}
	assertPartialLoadServerReleased(t, started, toolRegistry, &spawnCalls)
}

func TestLoadPluginsClosesServerReturnedWithLaunchError(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	writePluginManifest(t, "partial-return", `manifest_version = 1
name = "partial-return"
version = "0.1.0"
description = "launcher returned both a server and an error"

[mcp_server]
command = "partial-server"
`)

	toolRegistry := tools.NewRegistry()
	var partial *mcptools.Server
	spawnCalls := 0
	original := launchPluginMCPServer
	launchPluginMCPServer = func(_ context.Context, entry mcp.ServerEntry, registry *tools.Registry, _ *sandbox.Manager) (*mcptools.Server, error) {
		partial = newPartialLoadTestServer(entry.Name, registry, &spawnCalls)
		return partial, errors.New("launch failed after allocation")
	}
	t.Cleanup(func() { launchPluginMCPServer = original })

	plugins, errs := LoadPlugins(context.Background(), toolRegistry)
	if len(errs) != 1 || !strings.Contains(errs[0].Error(), "launch failed after allocation") {
		t.Fatalf("load errors = %v, want partial launch failure", errs)
	}
	if got := len(plugins.All()); got != 0 {
		t.Fatalf("loaded plugins = %d, want 0", got)
	}
	assertPartialLoadServerReleased(t, partial, toolRegistry, &spawnCalls)
}

func newPartialLoadTestServer(name string, registry *tools.Registry, spawnCalls *int) *mcptools.Server {
	server := mcptools.NewLazyServer(name, []mcpsdk.Tool{{
		Name:        "probe",
		Description: "partial-load lifecycle probe",
		InputSchema: map[string]any{"type": "object"},
	}}, func(context.Context) (*mcpsdk.Client, error) {
		(*spawnCalls)++
		return nil, errors.New("test server unexpectedly spawned")
	})
	for _, tool := range server.Tools() {
		registry.Register(tool)
	}
	return server
}

func assertPartialLoadServerReleased(t *testing.T, server *mcptools.Server, registry *tools.Registry, spawnCalls *int) {
	t.Helper()
	if server == nil {
		t.Fatal("first MCP server was not launched")
	}
	toolName := "mcp__" + server.Name() + "__probe"
	if _, ok := registry.Get(toolName); ok {
		t.Fatalf("partial-load tool namespace still contains %q", toolName)
	}
	serverTools := server.Tools()
	if len(serverTools) != 1 {
		t.Fatalf("server tools = %d, want 1", len(serverTools))
	}
	result, err := serverTools[0].Execute(context.Background(), map[string]any{})
	if err != nil {
		t.Fatalf("execute closed server probe: %v", err)
	}
	if result == nil || !result.IsError || !strings.Contains(result.Output, "MCP server is closed") {
		t.Fatalf("server was not closed after partial load: result=%#v", result)
	}
	if *spawnCalls != 0 {
		t.Fatalf("closed server invoked lazy spawn %d time(s), want 0", *spawnCalls)
	}
}
