package runtime

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/runtime/mcp"
	"github.com/Ricardo-M-L/metis/internal/sandbox"
	"github.com/Ricardo-M-L/metis/internal/tools"
	mcptools "github.com/Ricardo-M-L/metis/internal/tools/mcp"
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
