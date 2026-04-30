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
	Manifest pubplugin.Manifest
	RootDir  string
	MCP      *mcptools.Server // nil when no [mcp_server] stanza
	Skills   []skills.Skill   // pre-loaded from manifest's skills entries
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
		if p.MCP != nil {
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

		p, perr := loadOne(ctx, root, e.Name(), registry)
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
func loadOne(ctx context.Context, rootDir, expectedName string, registry *tools.Registry) (*Plugin, error) {
	manifestPath := filepath.Join(rootDir, "plugin.toml")
	var m pubplugin.Manifest
	if _, err := toml.DecodeFile(manifestPath, &m); err != nil {
		return nil, fmt.Errorf("decode manifest: %w", err)
	}
	if err := validateManifest(&m, expectedName); err != nil {
		return nil, err
	}

	p := &Plugin{Manifest: m, RootDir: rootDir}

	// Spawn MCP server if declared. Tools auto-register as
	// `plugin__<name>__<tool>` via the existing mcptools machinery.
	if m.MCPServer != nil {
		// Apply env overrides. Each plugin's env doesn't leak into the
		// parent process — applied only to the child via os/exec.
		// (mcptools.NewServer uses os/exec.CommandContext; child env
		// inheritance is handled there. For now we splice into a
		// MCPServerEntry-like spec.)
		entry := MCPServerEntry{
			Name:    "plugin:" + m.Name,
			Command: m.MCPServer.Command,
			Args:    append([]string(nil), m.MCPServer.Args...),
		}
		// Reuse single-server launch path so all our existing MCP
		// observability (errors, namespacing) applies.
		fakeReg := &MCPRegistry{Servers: []MCPServerEntry{entry}}
		srv, err := LaunchMCPServer(ctx, fakeReg, entry.Name, registry)
		if err != nil {
			return nil, fmt.Errorf("mcp_server: %w", err)
		}
		p.MCP = srv
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
		if m.MCPServer.Command == "" {
			return errors.New("[mcp_server].command required")
		}
		// Defense-in-depth against shell-tool injection: don't allow
		// shell metacharacters in command. Args is fine — that's
		// expected to carry user content.
		if strings.ContainsAny(m.MCPServer.Command, ";&|`$<>") {
			return fmt.Errorf("[mcp_server].command contains shell metacharacters: %q", m.MCPServer.Command)
		}
	}
	return nil
}
