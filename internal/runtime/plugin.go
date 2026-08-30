package runtime

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/BurntSushi/toml"

	"github.com/Ricardo-M-L/metis/internal/agent/skills"
	"github.com/Ricardo-M-L/metis/internal/config"
	"github.com/Ricardo-M-L/metis/internal/runtime/mcp"
	"github.com/Ricardo-M-L/metis/internal/sandbox"
	"github.com/Ricardo-M-L/metis/internal/tools"
	mcptools "github.com/Ricardo-M-L/metis/internal/tools/mcp"
	pubplugin "github.com/Ricardo-M-L/metis/pkg/plugin"
)

// PluginsDir returns the directory where plugin bundles live.
// Each subdirectory is one plugin; its manifest is `plugin.toml` at the
// plugin's root.
func PluginsDir() string {
	return filepath.Join(config.Home(), "plugins")
}

// Plugin represents one loaded plugin: parsed manifest + live MCP server
// reference (when the manifest declared one). Held by the PluginRegistry
// so we can call Close() on shutdown.
type Plugin struct {
	Manifest   pubplugin.Manifest
	RootDir    string
	MCP        *mcptools.Server // legacy first server; nil when none loaded
	MCPServers []*mcptools.Server
	Skills     []skills.Skill // pre-loaded from manifest's skills entries
}

// launchPluginMCPServer is a narrow seam for proving ecosystem manifest
// translation without spawning test subprocesses.
var launchPluginMCPServer = func(ctx context.Context, entry mcp.ServerEntry, registry *tools.Registry, manager *sandbox.Manager) (*mcptools.Server, error) {
	return mcp.LaunchServerWithSandbox(ctx, &mcp.Registry{Servers: []mcp.ServerEntry{entry}}, entry.Name, registry, manager)
}

// Name implements the loader's PluginSkillSource contract.
func (p *Plugin) Name() string { return p.Manifest.Name }

// SkillsList returns the plugin's skill set.
//
// Defined separately from the loader's interface method "Skills()" to
// avoid name collision when callers iterate. Use the wrapper below for
// loader registration.
func (p *Plugin) SkillsList() []skills.Skill { return p.Skills }

// pluginSkillAdapter satisfies skills.PluginSkillSource by delegating
// to a *Plugin. We need this trampoline because skills.Skill is an
// alias for pkg/skill.Skill — and the loader wants the alias.
type pluginSkillAdapter struct{ p *Plugin }

func (a pluginSkillAdapter) Name() string           { return a.p.Manifest.Name }
func (a pluginSkillAdapter) Skills() []skills.Skill { return a.p.Skills }

// PluginRegistry holds the set of active plugins. Built once at startup
// (LoadPlugins) and closed on shutdown.
type PluginRegistry struct {
	mu      sync.Mutex
	plugins []*Plugin
}

// All returns a snapshot of the loaded plugins. Safe for concurrent reads
// after LoadPlugins finished.
func (r *PluginRegistry) All() []*Plugin {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]*Plugin, len(r.plugins))
	copy(out, r.plugins)
	return out
}

// SkillSources returns each plugin wrapped as a skills.PluginSkillSource
// so the multi-source skill loader can pick them up at the "plugin" layer.
func (r *PluginRegistry) SkillSources() []skills.PluginSkillSource {
	all := r.All()
	out := make([]skills.PluginSkillSource, 0, len(all))
	for _, p := range all {
		out = append(out, pluginSkillAdapter{p: p})
	}
	return out
}

// Close terminates every spawned MCP server. Safe to call multiple times.
func (r *PluginRegistry) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	var errs []error
	for _, p := range r.plugins {
		for _, server := range p.MCPServers {
			if server != nil {
				if err := server.Close(); err != nil {
					errs = append(errs, err)
				}
			}
		}
		if len(p.MCPServers) == 0 && p.MCP != nil {
			if err := p.MCP.Close(); err != nil {
				errs = append(errs, err)
			}
		}
	}
	r.plugins = nil
	if len(errs) > 0 {
		return fmt.Errorf("plugin close: %d error(s); first: %w", len(errs), errs[0])
	}
	return nil
}

// LoadPlugins scans ~/.metis/plugins/, validates each manifest, spawns any
// declared MCP servers, and reads the contributed skills. Errors per
// plugin are collected and returned alongside the registry — bad plugins
// don't block startup; the user sees them on stderr or via `metis plugin list`.
func LoadPlugins(ctx context.Context, registry *tools.Registry) (*PluginRegistry, []error) {
	return LoadPluginsWithSandbox(ctx, registry, nil)
}

// LoadPluginsWithSandbox starts plugin-contributed stdio MCP servers through
// the shared runtime sandbox. The caller owns manager and closes it only after
// the returned PluginRegistry has been closed.
func LoadPluginsWithSandbox(ctx context.Context, registry *tools.Registry, manager *sandbox.Manager) (*PluginRegistry, []error) {
	dir := PluginsDir()
	reg := &PluginRegistry{}

	ents, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return reg, nil
		}
		return reg, []error{fmt.Errorf("plugins: read %s: %w", dir, err)}
	}

	var errs []error
	for _, e := range ents {
		if !e.IsDir() {
			continue
		}
		root := filepath.Join(dir, e.Name())
		manifestPath := filepath.Join(root, "plugin.toml")
		if _, err := os.Stat(manifestPath); err != nil {
			continue // not a plugin dir
		}

		p, perr := loadOne(ctx, root, e.Name(), registry, manager)
		if perr != nil {
			errs = append(errs, fmt.Errorf("plugin %s: %w", e.Name(), perr))
			continue
		}
		reg.mu.Lock()
		reg.plugins = append(reg.plugins, p)
		reg.mu.Unlock()
	}
	return reg, errs
}

// loadOne reads + validates one plugin's manifest, spawns its MCP server
// (if any), and prefetches its skill files.
func loadOne(ctx context.Context, rootDir, expectedName string, registry *tools.Registry, manager *sandbox.Manager) (*Plugin, error) {
	manifestPath := filepath.Join(rootDir, "plugin.toml")
	var m pubplugin.Manifest
	if _, err := toml.DecodeFile(manifestPath, &m); err != nil {
		return nil, fmt.Errorf("decode manifest: %w", err)
	}
	if err := validateManifest(&m, expectedName); err != nil {
		return nil, err
	}

	p := &Plugin{Manifest: m, RootDir: rootDir}

	// Spawn every declared MCP server. The singular form remains the legacy
	// default; ecosystem adapters use the named list so a Codex package keeps
	// all of its independent servers.
	specs := append([]pubplugin.MCPServerSpec(nil), m.MCPServers...)
	if m.MCPServer != nil {
		legacy := *m.MCPServer
		if legacy.Name == "" {
			legacy.Name = "default"
		}
		specs = append([]pubplugin.MCPServerSpec{legacy}, specs...)
	}
	for _, spec := range specs {
		if spec.Disabled {
			continue
		}
		serverName := spec.Name
		if serverName == "" {
			serverName = "default"
		}
		workingDir, err := pluginWorkingDir(rootDir, spec.WorkingDir)
		if err != nil {
			return nil, fmt.Errorf("mcp server %s: %w", serverName, err)
		}
		command := spec.Command
		if command != "" && (strings.HasPrefix(command, "./") || strings.HasPrefix(command, `.\`)) {
			command = filepath.Join(workingDir, filepath.FromSlash(strings.TrimPrefix(strings.TrimPrefix(command, "./"), `.\`)))
		}
		entry := mcp.ServerEntry{
			Name:    "plugin:" + m.Name + ":" + serverName,
			Command: command, Args: append([]string(nil), spec.Args...), URL: spec.URL,
			Headers: cloneStringMap(spec.Headers), Auth: spec.Auth, Env: cloneStringMap(spec.Env),
			WorkingDir: workingDir, EnabledTools: append([]string(nil), spec.EnabledTools...),
			DisabledTools: append([]string(nil), spec.DisabledTools...),
		}
		srv, err := launchPluginMCPServer(ctx, entry, registry, manager)
		if err != nil {
			return nil, fmt.Errorf("mcp server %s: %w", serverName, err)
		}
		p.MCPServers = append(p.MCPServers, srv)
		if p.MCP == nil {
			p.MCP = srv
		}
	}

	// Pre-load contributed skill files so the skill loader's plugin layer
	// has them ready. Failures here are per-skill; we log via err return
	// rather than blocking the whole plugin.
	for _, rel := range m.Skills {
		// Reject ".." path traversal — skills must live inside the plugin dir.
		if strings.Contains(rel, "..") || strings.HasPrefix(rel, "/") {
			return nil, fmt.Errorf("skill path %q escapes plugin dir", rel)
		}
		full := filepath.Join(rootDir, rel)
		sk, err := skills.Load(full)
		if err != nil || sk == nil {
			return nil, fmt.Errorf("skill %s: %w", rel, err)
		}
		// Namespace the skill name so it doesn't collide with bundled.
		if !strings.Contains(sk.Name, ":") {
			sk.Name = m.Name + ":" + sk.Name
		}
		p.Skills = append(p.Skills, *sk)
	}

	return p, nil
}

// validateManifest enforces the must-haves: manifest_version supported,
// name matches dir, command not shell-injected.
func validateManifest(m *pubplugin.Manifest, expectedName string) error {
	if m.ManifestVersion == 0 {
		// v0 = unset; auto-upgrade to v1 (the only version known).
		m.ManifestVersion = pubplugin.CurrentManifestVersion
	}
	if m.ManifestVersion != pubplugin.CurrentManifestVersion {
		return fmt.Errorf("manifest_version=%d not supported (this build understands v%d)",
			m.ManifestVersion, pubplugin.CurrentManifestVersion)
	}
	if m.Name == "" {
		return errors.New("name required")
	}
	if m.Name != expectedName {
		return fmt.Errorf("manifest name %q must match plugin dir %q", m.Name, expectedName)
	}
	if m.MCPServer != nil {
		if err := validateMCPServer(*m.MCPServer, "[mcp_server]"); err != nil {
			return err
		}
	}
	seenServers := map[string]bool{}
	for i, server := range m.MCPServers {
		label := fmt.Sprintf("[[mcp_servers]][%d]", i)
		if server.Name == "" {
			return fmt.Errorf("%s.name required", label)
		}
		if seenServers[server.Name] {
			return fmt.Errorf("duplicate mcp server name %q", server.Name)
		}
		seenServers[server.Name] = true
		if err := validateMCPServer(server, label); err != nil {
			return err
		}
	}
	return nil
}

func validateMCPServer(server pubplugin.MCPServerSpec, label string) error {
	if server.Command == "" && server.URL == "" {
		return fmt.Errorf("%s requires command or url", label)
	}
	if strings.ContainsAny(server.Command, ";&|`$<>") {
		return fmt.Errorf("%s.command contains shell metacharacters: %q", label, server.Command)
	}
	return nil
}

func pluginWorkingDir(root, declared string) (string, error) {
	if strings.TrimSpace(declared) == "" {
		return root, nil
	}
	if filepath.IsAbs(declared) {
		return "", errors.New("working_dir must be relative to the plugin")
	}
	clean := filepath.Clean(filepath.FromSlash(declared))
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", errors.New("working_dir escapes plugin root")
	}
	resolved := filepath.Join(root, clean)
	info, err := os.Stat(resolved)
	if err != nil || !info.IsDir() {
		return "", fmt.Errorf("working_dir is unavailable")
	}
	return resolved, nil
}

func cloneStringMap(input map[string]string) map[string]string {
	if len(input) == 0 {
		return nil
	}
	output := make(map[string]string, len(input))
	for key, value := range input {
		output[key] = value
	}
	return output
}
