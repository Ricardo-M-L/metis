package runtime

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/Ricardo-M-L/metis/internal/agent/skills"
	"github.com/Ricardo-M-L/metis/internal/config"
	"github.com/Ricardo-M-L/metis/internal/pluginmanifest"
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

// CloseMCPServers terminates and detaches every plugin-contributed MCP
// connection while preserving plugin metadata and skill sources. A runtime
// uses this narrower boundary when leaving fullAccess: processes started while
// the sandbox was disabled must not survive, but file-backed skills remain
// safe and useful. Reconnecting plugin MCP servers requires a runtime restart.
func (r *PluginRegistry) CloseMCPServers() error {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	servers := make([]*mcptools.Server, 0)
	for _, p := range r.plugins {
		if p == nil {
			continue
		}
		if len(p.MCPServers) > 0 {
			servers = append(servers, p.MCPServers...)
		} else if p.MCP != nil {
			servers = append(servers, p.MCP)
		}
		p.MCP = nil
		p.MCPServers = nil
	}
	r.mu.Unlock()

	var errs []error
	for _, server := range servers {
		if server != nil {
			if err := server.Close(); err != nil {
				errs = append(errs, err)
			}
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("plugin close: %d error(s); first: %w", len(errs), errs[0])
	}
	return nil
}

// Close terminates spawned MCP servers and releases all plugin metadata. Safe
// to call multiple times.
func (r *PluginRegistry) Close() error {
	if r == nil {
		return nil
	}
	err := r.CloseMCPServers()
	r.mu.Lock()
	r.plugins = nil
	r.mu.Unlock()
	return err
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
func loadOne(ctx context.Context, rootDir, expectedName string, registry *tools.Registry, manager *sandbox.Manager) (_ *Plugin, retErr error) {
	manifest, err := pluginmanifest.Read(rootDir, expectedName)
	if err != nil {
		return nil, err
	}
	m := *manifest

	p := &Plugin{Manifest: m, RootDir: rootDir}
	// Until the caller receives p, loadOne owns every MCP handle and tool
	// namespace it creates. A later server/working-directory/skill failure keeps
	// the plugin out of PluginRegistry, so the normal registry shutdown path can
	// never reclaim those resources. Roll the partial load back as one ownership
	// transaction; the success path transfers ownership by clearing owned.
	owned := true
	ownedMCPNamespaces := make([]string, 0, len(m.MCPServers)+1)
	defer func() {
		if !owned {
			return
		}
		if cleanupErr := releasePartialPluginMCP(p, registry, ownedMCPNamespaces); cleanupErr != nil {
			cleanupErr = fmt.Errorf("cleanup partial MCP load: %w", cleanupErr)
			if retErr == nil {
				retErr = cleanupErr
			} else {
				retErr = errors.Join(retErr, cleanupErr)
			}
		}
	}()

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
			Name:    pluginMCPServerName(m.Name, serverName),
			Command: command, Args: append([]string(nil), spec.Args...), URL: spec.URL,
			Headers: cloneStringMap(spec.Headers), Auth: spec.Auth, Env: cloneStringMap(spec.Env),
			WorkingDir: workingDir, EnabledTools: append([]string(nil), spec.EnabledTools...),
			DisabledTools: append([]string(nil), spec.DisabledTools...),
		}
		prefix := "mcp__" + entry.Name + "__"
		for _, existing := range registry.All() {
			if strings.HasPrefix(existing.Name(), prefix) {
				return nil, errors.New("plugin MCP namespace collision with an already registered tool")
			}
		}
		// Record the namespace before launch. The production launcher registers
		// tools only after a successful handshake, but this also cleans up a future
		// launcher that publishes some tools before returning an error.
		ownedMCPNamespaces = append(ownedMCPNamespaces, entry.Name)
		// Stage the launcher's original remote tool names privately. Only the
		// validated, provider-portable aliases are published to the live model
		// registry; raw long or non-ASCII names are never visible there.
		staging := tools.NewRegistry()
		srv, err := launchPluginMCPServer(ctx, entry, staging, manager)
		if srv != nil {
			p.MCPServers = append(p.MCPServers, srv)
			if p.MCP == nil {
				p.MCP = srv
			}
		}
		if err != nil {
			return nil, fmt.Errorf("mcp server %s: %w", serverName, err)
		}
		portable, err := portablePluginMCPTools(entry.Name, staging.All(), registry)
		if err != nil {
			return nil, err
		}
		registry.ReplacePrefix(prefix, portable)
	}

	// Read checked all skills before any launch. Recheck the path immediately
	// before loading in case an external change happened during MCP startup;
	// failures roll back the plugin's partial resources.
	for _, rel := range m.Skills {
		full, err := pluginmanifest.SkillPath(rootDir, rel)
		if err != nil {
			return nil, fmt.Errorf("skill %q: %w", rel, err)
		}
		sk, err := skills.Load(full)
		if err != nil || sk == nil {
			return nil, fmt.Errorf("skill %q: unreadable or invalid skill file", rel)
		}
		// Namespace the skill name so it doesn't collide with bundled.
		if !strings.Contains(sk.Name, ":") {
			sk.Name = m.Name + ":" + sk.Name
		}
		p.Skills = append(p.Skills, *sk)
	}

	owned = false
	return p, nil
}

// MCP identifiers also become cache filenames and qualified tool names. Hash
// the unambiguous identity tuple so plugin names cannot exceed the launcher's
// portable length limit or collide through separator normalization.
func pluginMCPServerName(pluginName, serverName string) string {
	identity := sha256.Sum256([]byte(pluginName + "\x00" + serverName))
	return fmt.Sprintf("plugin-%x", identity[:12])
}

// Embedding the concrete MCP tool preserves its optional exposure and host
// behavior. Only the model-facing name changes: Execute still calls the
// server's original remote name, and filtering happened before this wrapper.
type pluginMCPTool struct {
	*mcptools.MCPTool
	name string
}

func (t *pluginMCPTool) Name() string { return t.name }

func portablePluginMCPTools(serverName string, staged []tools.Tool, live *tools.Registry) ([]tools.Tool, error) {
	prefix := "mcp__" + serverName + "__"
	seen := make(map[string]bool, len(staged))
	portable := make([]tools.Tool, 0, len(staged))
	for _, raw := range staged {
		mcpTool, ok := raw.(*mcptools.MCPTool)
		if !ok || !strings.HasPrefix(raw.Name(), prefix) {
			return nil, errors.New("plugin MCP launcher returned an unexpected tool namespace")
		}
		remoteName := strings.TrimPrefix(raw.Name(), prefix)
		if remoteName == "" {
			return nil, errors.New("plugin MCP server declared an empty tool name")
		}
		suffix := remoteName
		if len(prefix)+len(suffix) > 64 || !portableFunctionSuffix(suffix) {
			identity := sha256.Sum256([]byte(remoteName))
			suffix = fmt.Sprintf("tool-%x", identity[:10])
		}
		name := prefix + suffix
		if seen[name] {
			return nil, errors.New("plugin MCP tool alias collision; no tools from this server were published")
		}
		seen[name] = true
		if _, exists := live.Get(name); exists {
			return nil, errors.New("plugin MCP tool alias collision with an already registered tool")
		}
		portable = append(portable, &pluginMCPTool{MCPTool: mcpTool, name: name})
	}
	return portable, nil
}

func portableFunctionSuffix(name string) bool {
	for _, r := range name {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' || r == '-' {
			continue
		}
		return false
	}
	return name != ""
}

func releasePartialPluginMCP(p *Plugin, registry *tools.Registry, namespaces []string) error {
	// Remove model-visible tools even if closing a transport fails. Otherwise a
	// failed plugin leaves callable wrappers pointing at an orphaned/closed MCP
	// client for the remainder of the session.
	if registry != nil {
		seenNamespaces := make(map[string]struct{}, len(namespaces))
		for _, name := range namespaces {
			if name == "" {
				continue
			}
			if _, seen := seenNamespaces[name]; seen {
				continue
			}
			seenNamespaces[name] = struct{}{}
			registry.ReplacePrefix("mcp__"+name+"__", nil)
		}
	}

	if p == nil {
		return nil
	}
	servers := append([]*mcptools.Server(nil), p.MCPServers...)
	if len(servers) == 0 && p.MCP != nil {
		servers = append(servers, p.MCP)
	}
	p.MCP = nil
	p.MCPServers = nil

	var cleanupErr error
	seenServers := make(map[*mcptools.Server]struct{}, len(servers))
	for _, server := range servers {
		if server == nil {
			continue
		}
		if _, seen := seenServers[server]; seen {
			continue
		}
		seenServers[server] = struct{}{}
		if err := server.Close(); err != nil {
			cleanupErr = errors.Join(cleanupErr, fmt.Errorf("close MCP server %q: %w", server.Name(), err))
		}
	}
	return cleanupErr
}

// validateManifest retains the runtime's internal compatibility seam while all
// manifest consumers use the same supported-capability contract.
func validateManifest(m *pubplugin.Manifest, expectedName string) error {
	return pluginmanifest.Validate(m, expectedName)
}

func pluginWorkingDir(root, declared string) (string, error) {
	return pluginmanifest.WorkingDir(root, declared)
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
