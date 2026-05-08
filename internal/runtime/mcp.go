// Package runtime holds composition helpers for Metis's mutable runtime
// state: MCP server registry, agent loop bootstrap, etc. Code here is the
// glue between the on-disk configuration files and the live in-memory
// objects that main.go and the slash commands manipulate.
//
// This is the destination openclaude / openclaw / hermes-agent's `bootstrap`
// directories point us at: business logic that doesn't belong in main.go
// (too low-level for command dispatch) but also doesn't belong in any one
// domain package (it spans several). Splitting it out keeps cmd/metis/
// minimal and gives slash commands a clean call site.
package runtime

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	goruntime "runtime"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"

	"github.com/Ricardo-M-L/metis/internal/config"
	"github.com/Ricardo-M-L/metis/internal/tools"
	mcptools "github.com/Ricardo-M-L/metis/internal/tools/mcp"
)

// MCPRegistry is the on-disk schema for ~/.metis/mcp.toml.
//
// Why a separate file from config.toml: MCP servers churn — users `/mcp add`
// and `/mcp remove` from the chat surface — and we'd rather not rewrite the
// whole config.toml every time. Splitting into its own file also keeps
// secrets/paths/argv out of the otherwise-shareable config.toml.
type MCPRegistry struct {
	Servers []MCPServerEntry `toml:"servers"`
}

// MCPServerEntry mirrors config.MCPServer but lives in this package so the
// runtime layer doesn't reach into config to mutate slices. A cross-walk
// helper (FromConfigServers / ToConfigServers) covers the conversion.
//
// Two transport shapes are recognized:
//
//   - stdio (default): set Command + Args, leave URL empty
//   - http  (Streamable HTTP / SSE): set URL, optional Headers, leave
//     Command empty
//
// LaunchMCPServer auto-detects from whichever pair is populated, so
// existing stdio-only registries continue to load unchanged.
type MCPServerEntry struct {
	Name     string            `toml:"name"`
	Command  string            `toml:"command,omitempty"`
	Args     []string          `toml:"args,omitempty"`
	URL      string            `toml:"url,omitempty"`     // HTTP endpoint
	Headers  map[string]string `toml:"headers,omitempty"` // optional HTTP auth
	Disabled bool              `toml:"disabled"`
	// EnabledTools and DisabledTools filter the tool list a server
	// exposes after handshake. Modeled on Codex's mcp_servers.<id>
	// fields with the same names; the motivation is the same — many
	// MCP servers (notably metis-cu's 24-tool set) flood prompts with
	// tools the user never wants in their flow, and disabling individual
	// tools without a config entry meant editing the server's source.
	//
	// Semantics: if EnabledTools is non-empty, ONLY tools whose unqualified
	// name appears in the list survive. Then DisabledTools is applied as
	// a deny pass. Names are matched against the server-reported `name`
	// (e.g. "screenshot"), NOT the qualified `mcp__computer-use__screenshot`
	// — users shouldn't have to repeat the namespace they already typed
	// as the entry's `name = ...`.
	EnabledTools  []string `toml:"enabled_tools,omitempty"`
	DisabledTools []string `toml:"disabled_tools,omitempty"`
}

// MCPPath returns the canonical path of mcp.toml under the metis home.
func MCPPath() string {
	return filepath.Join(config.Home(), "mcp.toml")
}

// LoadMCP reads the registry. A missing file is NOT an error — returns an
// empty registry so callers can append-and-save without first checking.
func LoadMCP() (*MCPRegistry, error) {
	p := MCPPath()
	if _, err := os.Stat(p); err != nil {
		if os.IsNotExist(err) {
			return &MCPRegistry{}, nil
		}
		return nil, err
	}
	var r MCPRegistry
	if _, err := toml.DecodeFile(p, &r); err != nil {
		return nil, fmt.Errorf("decode %s: %w", p, err)
	}
	return &r, nil
}

// SaveMCP writes the registry atomically (tempfile + rename) at 0o600.
// MCP entries can carry sensitive arg payloads (API keys passed via env or
// argv to the spawned subprocess), so the file inherits auth.json-style
// perms even though it's mostly plaintext config.
func SaveMCP(reg *MCPRegistry) error {
	if reg == nil {
		reg = &MCPRegistry{}
	}
	dir := config.Home()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("mkdir %s: %w", dir, err)
	}
	// Stable order so the file diffs cleanly.
	sort.Slice(reg.Servers, func(i, j int) bool {
		return reg.Servers[i].Name < reg.Servers[j].Name
	})
	var b strings.Builder
	b.WriteString("# metis MCP server registry — managed by `/mcp add` / `/mcp remove`\n")
	b.WriteString("# Edit by hand at your own risk; comments are not preserved by writes.\n\n")
	enc := toml.NewEncoder(&b)
	if err := enc.Encode(reg); err != nil {
		return fmt.Errorf("encode mcp.toml: %w", err)
	}
	final := MCPPath()
	tmp, err := os.CreateTemp(dir, ".mcp.toml.*")
	if err != nil {
		return fmt.Errorf("tempfile: %w", err)
	}
	tmpPath := tmp.Name()
	defer func() { _ = os.Remove(tmpPath) }()
	if _, err := tmp.WriteString(b.String()); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write tempfile: %w", err)
	}
	// fsync the data before rename. Without this, a power loss between
	// rename and the kernel's eventual writeback leaves a zero-length
	// mcp.toml — every registered MCP server vanishes from the user's
	// next session. Claude Code's writeMcpjsonFile (config.ts:88-131)
	// does the same datasync()-then-rename dance for the same reason.
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("fsync tempfile: %w", err)
	}
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("chmod tempfile: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close tempfile: %w", err)
	}
	if err := os.Rename(tmpPath, final); err != nil {
		return fmt.Errorf("rename: %w", err)
	}
	return nil
}

// ReservedComputerUseName is the MCP server name that mirrors Anthropic's
// `mcp__computer-use__*` tool namespace. It is treated as a metis built-in
// — registration of arbitrary commands under this name is REFUSED, matching
// Claude Code's `isComputerUseMCPServer(name)` reservation in addMcpConfig
// (config.ts:642-648).
//
// The user-facing way to enable the built-in is `/cu enable`, which goes
// through SetReservedComputerUseServer below — never through AddMCPServer.
// That asymmetry is deliberate:
//
//   - `/mcp add` is the user-typed surface; pretending the slot is free
//     lets a user accidentally point it at a malicious / unrelated binary,
//     and the next `/cu enable` would silently replace their config.
//   - `/cu enable` is metis's own management of the reserved slot, the
//     same way Claude Code's built-in `mcp__computer-use__*` server is
//     in-process and not user-replaceable.
//
// Codex doesn't reserve any names because it has no built-in computer-use
// server to protect. metis follows CC because metis-cu *is* metis's
// built-in (even though it currently lives as an external binary — that's
// an in-process optimization for later).
const (
	ReservedComputerUseName   = "computer-use"
	ReservedComputerUseBinary = "metis-cu"
)

// AddMCPServer inserts (or replaces) a server entry. Empty name / command
// errors so accidental `/mcp add` typos can't write garbage entries.
//
// Refuses ReservedComputerUseName outright — the slot is owned by metis's
// built-in computer-use server (see /cu enable). Same behavior as
// Claude Code's `addMcpConfig` for `computer-use` and `claude-in-chrome`.
func AddMCPServer(reg *MCPRegistry, name, command string, args []string) error {
	if name == "" {
		return errors.New("mcp: name required")
	}
	if command == "" {
		return errors.New("mcp: command required")
	}
	if reg == nil {
		return errors.New("mcp: nil registry")
	}
	if name == ReservedComputerUseName {
		return fmt.Errorf("mcp: name %q is reserved for the metis built-in — "+
			"use `/cu enable` to enable computer-use, or pick a different name",
			ReservedComputerUseName)
	}
	for i, s := range reg.Servers {
		if s.Name == name {
			reg.Servers[i] = MCPServerEntry{
				Name: name, Command: command, Args: append([]string(nil), args...),
			}
			return nil
		}
	}
	reg.Servers = append(reg.Servers, MCPServerEntry{
		Name: name, Command: command, Args: append([]string(nil), args...),
	})
	return nil
}

// SetReservedComputerUseServer is the internal-only path for /cu enable
// to write the reserved `computer-use` entry. AddMCPServer refuses this
// name, so /cu uses this dedicated API. Behaviorally identical to
// AddMCPServer except (a) skips the reserved-name check, (b) hardcodes
// the command to ReservedComputerUseBinary so callers can't override —
// it's "enable the built-in", not "add an arbitrary entry".
//
// Returns true if a prior entry was replaced (re-enable case) so the
// REPL can word the success message accordingly.
func SetReservedComputerUseServer(reg *MCPRegistry) (replaced bool) {
	if reg == nil {
		return false
	}
	for i, s := range reg.Servers {
		if s.Name == ReservedComputerUseName {
			reg.Servers[i] = MCPServerEntry{
				Name:    ReservedComputerUseName,
				Command: ReservedComputerUseBinary,
			}
			return true
		}
	}
	reg.Servers = append(reg.Servers, MCPServerEntry{
		Name:    ReservedComputerUseName,
		Command: ReservedComputerUseBinary,
	})
	return false
}

// RemoveMCPServer drops a server by name. Returns true if removed, false if
// no entry by that name existed.
func RemoveMCPServer(reg *MCPRegistry, name string) bool {
	if reg == nil {
		return false
	}
	for i, s := range reg.Servers {
		if s.Name == name {
			reg.Servers = append(reg.Servers[:i], reg.Servers[i+1:]...)
			return true
		}
	}
	return false
}

// SetMCPDisabled flips the Disabled flag for the named server. Returns
// (true, prior) when the entry was found, (false, false) when no such
// server exists. Caller is responsible for SaveMCP. Pulled out of the
// /mcp enable/disable handlers so the slash code stays declarative —
// runtime owns the actual mutation rule (idempotent, no-op when already
// in the requested state).
func SetMCPDisabled(reg *MCPRegistry, name string, disabled bool) (found bool, prior bool) {
	if reg == nil {
		return false, false
	}
	for i := range reg.Servers {
		if reg.Servers[i].Name == name {
			prior = reg.Servers[i].Disabled
			reg.Servers[i].Disabled = disabled
			return true, prior
		}
	}
	return false, false
}

// FindMCPServer returns the entry by name (or nil if missing). Useful when a
// caller only needs read access without iterating all entries.
func FindMCPServer(reg *MCPRegistry, name string) *MCPServerEntry {
	if reg == nil {
		return nil
	}
	for i := range reg.Servers {
		if reg.Servers[i].Name == name {
			return &reg.Servers[i]
		}
	}
	return nil
}

// LaunchMCPServer spawns a single MCP server subprocess and registers its
// exposed tools onto the supplied tools.Registry. Returns the live Server so
// the caller can Close() it later (e.g. on shutdown / `/mcp stop`).
//
// Returns an error if the entry is missing, disabled, or the subprocess
// can't be launched. Tool registration is non-fatal per tool — partial
// success leaves whichever tools loaded.
//
// Environment variable expansion: ${VAR} and ${VAR:-default} in command,
// args, url, and headers are resolved against os.Environ() AT LAUNCH
// TIME — same as Claude Code's expandEnvVars(). Missing-without-default
// variables fail the launch with a clear error so a misspelled
// $GITHUB_TOKEN doesn't silently land an empty string into the
// Authorization header (which the server then rejects with an opaque
// 401, hiding the actual root cause).
func LaunchMCPServer(ctx context.Context, reg *MCPRegistry, name string, registry *tools.Registry) (*mcptools.Server, error) {
	entry := FindMCPServer(reg, name)
	if entry == nil {
		return nil, fmt.Errorf("mcp: no server named %q", name)
	}
	if entry.Disabled {
		return nil, fmt.Errorf("mcp: server %q is disabled", name)
	}
	expanded, missing := expandEnvVarsInEntry(*entry)
	if len(missing) > 0 {
		return nil, fmt.Errorf("mcp: server %q references unset env vars: %s "+
			"(use ${VAR:-default} for an inline fallback)",
			name, strings.Join(missing, ", "))
	}
	// Windows-on-npx footgun: spawning `npx some-mcp` directly fails
	// on Windows with "exec: \"npx\": file does not exist" because npx
	// is a .cmd batch wrapper, not an executable. Surfacing this here
	// gives a far more useful error than the cryptic ENOENT the OS
	// would otherwise report. Mirrors Claude Code's parseMcpConfig
	// warning at config.ts:1351-1369.
	if goruntime.GOOS == "windows" && expanded.Command != "" {
		base := expanded.Command
		// Compare basename so absolute paths like C:\node\npx are caught.
		if i := strings.LastIndexAny(base, `\/`); i >= 0 {
			base = base[i+1:]
		}
		base = strings.TrimSuffix(strings.ToLower(base), ".exe")
		if base == "npx" {
			return nil, fmt.Errorf("mcp: server %q uses `npx` directly on Windows; "+
				"npx is a .cmd batch wrapper that exec.Command can't launch. "+
				"Set command = \"cmd\" with args = [\"/c\", \"npx\", ...]",
				name)
		}
	}
	// Pick transport from whichever field the entry populated. URL
	// wins when both are set so a user converting an entry from stdio
	// to HTTP doesn't have to clear Command first.
	var srv *mcptools.Server
	var err error
	switch {
	case expanded.URL != "":
		srv, err = mcptools.NewHTTPServer(ctx, expanded.Name, expanded.URL, expanded.Headers)
	case expanded.Command != "":
		srv, err = mcptools.NewServer(ctx, expanded.Name, expanded.Command, expanded.Args...)
	default:
		return nil, fmt.Errorf("mcp: server %q has neither command nor url", name)
	}
	if err != nil {
		return nil, err
	}
	// Per-server tool allow/deny lists (Codex parity). Without this,
	// metis-cu's 24-tool surface lands in EVERY prompt even when the
	// user only wanted screenshot+click — wasting ~3 KB of tokens
	// turn after turn.
	for _, t := range srv.FilteredTools(entry.EnabledTools, entry.DisabledTools) {
		registry.Register(t)
	}
	return srv, nil
}

// LaunchAllMCP starts every enabled server in the registry and returns the
// Servers that came up. Errors on individual servers are appended to the
// returned []error so callers can warn but keep going (mirrors the existing
// behavior in cmd/metis/main.go's setupRuntime).
func LaunchAllMCP(ctx context.Context, reg *MCPRegistry, registry *tools.Registry) ([]*mcptools.Server, []error) {
	if reg == nil {
		return nil, nil
	}
	var ok []*mcptools.Server
	var errs []error
	for _, s := range reg.Servers {
		// Skip disabled entries or entries with neither transport
		// set. The launch helper itself handles "command vs url"
		// branching; here we only filter unusable rows.
		if s.Disabled {
			continue
		}
		if s.Command == "" && s.URL == "" {
			continue
		}
		srv, err := LaunchMCPServer(ctx, reg, s.Name, registry)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		ok = append(ok, srv)
	}
	return ok, errs
}

// MergeWithConfig folds entries declared in config.toml's [[mcp.servers]]
// into the runtime registry. Config-declared entries don't overwrite ones
// already in mcp.toml — the user's runtime additions always win.
func (r *MCPRegistry) MergeWithConfig(cfgServers []config.MCPServer) {
	for _, s := range cfgServers {
		if s.Name == "" {
			continue
		}
		if FindMCPServer(r, s.Name) != nil {
			continue
		}
		r.Servers = append(r.Servers, MCPServerEntry{
			Name: s.Name, Command: s.Command,
			Args: append([]string(nil), s.Args...), Disabled: s.Disabled,
		})
	}
}
