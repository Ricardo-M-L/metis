// Package plugin defines the public manifest schema for Metis plugins.
//
// A plugin is a directory under ~/.metis/plugins/<name>/ containing a
// `plugin.toml` manifest. The plugin contributes to the runtime through
// either supported channel:
//
//  1. **MCP server** (most common): a stdio subprocess implementing the
//     Model Context Protocol; its tools become available to the agent
//     namespaced with a stable identifier derived from the plugin and server.
//  2. **Skill files**: Markdown files (with YAML frontmatter) that the
//     skill loader picks up at the "plugin" priority layer.
//
// Hooks retain their schema below for compatibility, but this runtime does not
// execute plugin hooks. Manifests declaring them are rejected rather than
// presented as successfully loaded. Schedules, themes, and UI extensions also
// have no native manifest contract in this version.
//
// Plugin authors write the manifest, ship the directory (git repo /
// tarball / npm package), and end users `metis plugin install <ref>`.
//
// Why MCP-bundle instead of Go's `plugin` package: the Go plugin runtime
// is unreliable across platforms (Windows: unsupported; macOS: must
// match the host's exact Go toolchain version). Spawning a stdio MCP
// server has none of these constraints — plugins can be authored in
// Node/Python/Rust and remain portable.
package plugin

// Manifest is the on-disk schema for `plugin.toml`. Validate via
// the shared internal/pluginmanifest reader before advertising or launching
// anything the manifest declares.
type Manifest struct {
	// ManifestVersion locks compatibility. v1 is the only version
	// recognized today; bumping breaks old plugins, so do it sparingly.
	ManifestVersion int `toml:"manifest_version"`

	Name        string `toml:"name"`
	Version     string `toml:"version"`
	Description string `toml:"description"`
	License     string `toml:"license,omitempty"`
	Homepage    string `toml:"homepage,omitempty"`

	// MCPServer, when non-nil, declares a stdio MCP subprocess to spawn.
	// Tools the server exposes use a stable, portable namespace derived from
	// the plugin name and server name, followed by the original tool name.
	MCPServer *MCPServerSpec `toml:"mcp_server,omitempty"`

	// MCPServers is the multi-server form used by ecosystem adapters. It maps
	// Codex's `.mcp.json` entries without collapsing several independent
	// servers into one. MCPServer remains supported for existing METIS v1
	// bundles.
	MCPServers []MCPServerSpec `toml:"mcp_servers,omitempty"`

	// Skills lists relative paths (under the plugin directory) to
	// SKILL.md files this plugin contributes. They land in the "plugin"
	// layer of the multi-source skill loader, namespaced as
	// `<plugin-name>:<skill-name>`.
	Skills []string `toml:"skills,omitempty"`

	// Hooks preserves the declaration format for future compatibility.
	// Nonempty declarations are currently unsupported and rejected.
	Hooks []HookSpec `toml:"hooks,omitempty"`
}

// MCPServerSpec is the stdio command line + env for the MCP subprocess.
// Args / Env are optional; Command is required.
type MCPServerSpec struct {
	Name          string            `toml:"name,omitempty"`
	Command       string            `toml:"command,omitempty"`
	Args          []string          `toml:"args,omitempty"`
	URL           string            `toml:"url,omitempty"`
	Headers       map[string]string `toml:"headers,omitempty"`
	Auth          string            `toml:"auth,omitempty"`
	Env           map[string]string `toml:"env,omitempty"`
	WorkingDir    string            `toml:"working_dir,omitempty"`
	EnabledTools  []string          `toml:"enabled_tools,omitempty"`
	DisabledTools []string          `toml:"disabled_tools,omitempty"`
	Disabled      bool              `toml:"disabled,omitempty"`
}

// HookSpec describes the reserved subprocess hook format. The current runtime
// does not spawn these hooks and rejects nonempty Hooks declarations.
//
//   - Events: subset of {"PreToolUse", "PostToolUse", "SessionStart",
//     "SessionEnd", "TurnStart", "TurnEnd", "LoopEnd", "Error"}.
//   - Match: optional glob filter on tool name (e.g. "browser_*").
//     Only events with a tool field matching the glob are forwarded.
type HookSpec struct {
	Events  []string `toml:"events"`
	Command string   `toml:"command"`
	Args    []string `toml:"args,omitempty"`
	Match   string   `toml:"match,omitempty"`
}

// CurrentManifestVersion is the version this build understands. Old
// plugins with manifest_version=0 (unset) are auto-upgraded to 1.
const CurrentManifestVersion = 1
