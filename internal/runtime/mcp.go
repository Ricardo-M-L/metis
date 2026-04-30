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

// AddMCPServer inserts (or replaces) a server entry. Empty name / command
// errors so accidental `/mcp add` typos can't write garbage entries.
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
func LaunchMCPServer(ctx context.Context, reg *MCPRegistry, name string, registry *tools.Registry) (*mcptools.Server, error) {
	entry := FindMCPServer(reg, name)
	if entry == nil {
		return nil, fmt.Errorf("mcp: no server named %q", name)
	}
	if entry.Disabled {
		return nil, fmt.Errorf("mcp: server %q is disabled", name)
	}
	// Pick transport from whichever field the entry populated. URL
	// wins when both are set so a user converting an entry from stdio
	// to HTTP doesn't have to clear Command first.
	var srv *mcptools.Server
	var err error
	switch {
	case entry.URL != "":
		srv, err = mcptools.NewHTTPServer(ctx, entry.Name, entry.URL, entry.Headers)
	case entry.Command != "":
		srv, err = mcptools.NewServer(ctx, entry.Name, entry.Command, entry.Args...)
	default:
		return nil, fmt.Errorf("mcp: server %q has neither command nor url", name)
	}
	if err != nil {
		return nil, err
	}
	for _, t := range srv.Tools() {
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
