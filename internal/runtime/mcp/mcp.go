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
package mcp

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	goruntime "runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/Ricardo-M-L/metis/internal/config"
	mcpsdk "github.com/Ricardo-M-L/metis/internal/mcp"
	"github.com/Ricardo-M-L/metis/internal/mcpoauth"
	"github.com/Ricardo-M-L/metis/internal/tools"
	mcptools "github.com/Ricardo-M-L/metis/internal/tools/mcp"
)

// Registry is the on-disk schema for ~/.metis/mcp.toml.
//
// Why a separate file from config.toml: MCP servers churn — users `/mcp add`
// and `/mcp remove` from the chat surface — and we'd rather not rewrite the
// whole config.toml every time. Splitting into its own file also keeps
// secrets/paths/argv out of the otherwise-shareable config.toml.
type Registry struct {
	Servers []ServerEntry `toml:"servers"`
}

// resolveAuthHeaders returns the HTTP headers to use for a server,
// injecting a freshly-ensured OAuth Bearer token when the entry declares
// auth="oauth". On any OAuth failure it logs to stderr and falls back to
// the static Headers, so a broken auth setup degrades to "connect without
// the token" (the server then returns its own 401) rather than hard-
// failing the whole launch. No-op for stdio entries.
func resolveAuthHeaders(ctx context.Context, e ServerEntry) map[string]string {
	if e.URL == "" || !strings.EqualFold(e.Auth, "oauth") {
		return e.Headers
	}
	// interactive=false: an autonomous connect (startup / lazy tool call)
	// must not block on a browser flow. Missing token → connect without
	// (server returns 401); the user runs an explicit login to authorize.
	tok, err := mcpoauth.NewTokenStore().EnsureToken(ctx, e.Name, e.URL, false)
	if err != nil {
		fmt.Fprintf(os.Stderr, "mcp: oauth for %q: %v\n", e.Name, err)
		return e.Headers
	}
	out := make(map[string]string, len(e.Headers)+1)
	for k, v := range e.Headers {
		out[k] = v
	}
	out["Authorization"] = "Bearer " + tok
	return out
}

// ServerEntry mirrors config.MCPServer but lives in this package so the
// runtime layer doesn't reach into config to mutate slices. A cross-walk
// helper (FromConfigServers / ToConfigServers) covers the conversion.
//
// Two transport shapes are recognized:
//
//   - stdio (default): set Command + Args, leave URL empty
//   - http  (Streamable HTTP / SSE): set URL, optional Headers, leave
//     Command empty
//
// LaunchServer auto-detects from whichever pair is populated, so
// existing stdio-only registries continue to load unchanged.
type ServerEntry struct {
	Name    string            `toml:"name"`
	Command string            `toml:"command,omitempty"`
	Args    []string          `toml:"args,omitempty"`
	URL     string            `toml:"url,omitempty"`     // HTTP endpoint
	Headers map[string]string `toml:"headers,omitempty"` // optional HTTP auth
	// Auth selects an authentication strategy for an HTTP server. "oauth"
	// runs the OAuth 2.0 (PKCE) flow against the server's discovered
	// endpoints and attaches the resulting Bearer token; the token is
	// cached + refreshed in ~/.metis/mcp-oauth.json. Empty = use Headers
	// verbatim (static API key or none).
	Auth string `toml:"auth,omitempty"`
	// Env injects extra environment variables into a stdio subprocess
	// (no effect for url-transport servers). Values are env-expanded the
	// same way command/args/url are — so `FIRECRAWL_API_KEY = "${FC_KEY}"`
	// works. Wired via `/mcp add --env KEY=VAL` and the user can also
	// hand-edit `[servers.env]` inline tables in mcp.toml.
	Env      map[string]string `toml:"env,omitempty"`
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

// Path returns the canonical path of mcp.toml under the metis home.
func Path() string {
	return filepath.Join(config.Home(), "mcp.toml")
}

// Load reads the registry. A missing file is NOT an error — returns an
// empty registry so callers can append-and-save without first checking.
func Load() (*Registry, error) {
	p := Path()
	if _, err := os.Stat(p); err != nil {
		if os.IsNotExist(err) {
			return &Registry{}, nil
		}
		return nil, err
	}
	var r Registry
	if _, err := toml.DecodeFile(p, &r); err != nil {
		return nil, fmt.Errorf("decode %s: %w", p, err)
	}
	return &r, nil
}

// Save writes the registry atomically (tempfile + rename) at 0o600.
// MCP entries can carry sensitive arg payloads (API keys passed via env or
// argv to the spawned subprocess), so the file inherits auth.json-style
// perms even though it's mostly plaintext config.
func Save(reg *Registry) error {
	if reg == nil {
		reg = &Registry{}
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
	final := Path()
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
// through SetReservedComputerUseServer below — never through AddServer.
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

// AddServer inserts (or replaces) a server entry. Empty name / command
// errors so accidental `/mcp add` typos can't write garbage entries.
//
// Refuses ReservedComputerUseName outright — the slot is owned by metis's
// built-in computer-use server (see /cu enable). Same behavior as
// Claude Code's `addMcpConfig` for `computer-use` and `claude-in-chrome`.
func AddServer(reg *Registry, name, command string, args []string) error {
	return AddServerWithEnv(reg, name, command, args, nil)
}

// AddServerWithEnv is the env-aware variant of AddServer. Pass nil
// `env` for parity with the simpler form. Env values are written verbatim
// (NOT expanded) so users can keep `${SOMETHING}` placeholders in mcp.toml
// for shell-resolved secrets — the expansion happens at LaunchServer
// time via expandEnvVarsInEntry.
func AddServerWithEnv(reg *Registry, name, command string, args []string, env map[string]string) error {
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
	var envCopy map[string]string
	if len(env) > 0 {
		envCopy = make(map[string]string, len(env))
		for k, v := range env {
			envCopy[k] = v
		}
	}
	for i, s := range reg.Servers {
		if s.Name == name {
			reg.Servers[i] = ServerEntry{
				Name: name, Command: command,
				Args: append([]string(nil), args...),
				Env:  envCopy,
			}
			return nil
		}
	}
	reg.Servers = append(reg.Servers, ServerEntry{
		Name: name, Command: command,
		Args: append([]string(nil), args...),
		Env:  envCopy,
	})
	return nil
}

// SetReservedComputerUseServer is the internal-only path for /cu enable
// to write the reserved `computer-use` entry. AddServer refuses this
// name, so /cu uses this dedicated API. Behaviorally identical to
// AddServer except (a) skips the reserved-name check, (b) hardcodes
// the command to ReservedComputerUseBinary so callers can't override —
// it's "enable the built-in", not "add an arbitrary entry".
//
// Returns true if a prior entry was replaced (re-enable case) so the
// REPL can word the success message accordingly.
func SetReservedComputerUseServer(reg *Registry) (replaced bool) {
	if reg == nil {
		return false
	}
	for i, s := range reg.Servers {
		if s.Name == ReservedComputerUseName {
			reg.Servers[i] = ServerEntry{
				Name:    ReservedComputerUseName,
				Command: ReservedComputerUseBinary,
			}
			return true
		}
	}
	reg.Servers = append(reg.Servers, ServerEntry{
		Name:    ReservedComputerUseName,
		Command: ReservedComputerUseBinary,
	})
	return false
}

// RemoveServer drops a server by name. Returns true if removed, false if
// no entry by that name existed.
func RemoveServer(reg *Registry, name string) bool {
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

// SetDisabled flips the Disabled flag for the named server. Returns
// (true, prior) when the entry was found, (false, false) when no such
// server exists. Caller is responsible for Save. Pulled out of the
// /mcp enable/disable handlers so the slash code stays declarative —
// runtime owns the actual mutation rule (idempotent, no-op when already
// in the requested state).
func SetDisabled(reg *Registry, name string, disabled bool) (found bool, prior bool) {
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

// FindServer returns the entry by name (or nil if missing). Useful when a
// caller only needs read access without iterating all entries.
func FindServer(reg *Registry, name string) *ServerEntry {
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

// LaunchServer spawns a single MCP server subprocess and registers its
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
func LaunchServer(ctx context.Context, reg *Registry, name string, registry *tools.Registry) (*mcptools.Server, error) {
	entry := FindServer(reg, name)
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
		srv, err = mcptools.NewHTTPServer(ctx, expanded.Name, expanded.URL, resolveAuthHeaders(ctx, expanded))
	case expanded.Command != "":
		srv, err = mcptools.NewServerWithEnv(
			ctx, expanded.Name, expanded.Command,
			envSliceFromMap(maybeInjectCUEnv(expanded.Command, expanded.Env)),
			expanded.Args...)
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

// LaunchAll starts every enabled server in the registry and returns the
// Servers that came up. Errors on individual servers are appended to the
// returned []error so callers can warn but keep going (mirrors the existing
// behavior in cmd/metis/main.go's setupRuntime).
//
// Launches run CONCURRENTLY. Before 2026-05-18 this was a sequential for
// loop — wall-clock for N servers was sum(handshake_i) which made cold-
// start brutal when even one stdio server was slow (an `npx -y …` cache
// miss + a real-API-key firecrawl-mcp could each pin the full 30s
// ConnectTimeout). claude-code uses `pMap` + `Promise.allSettled` for
// the same reason; goroutines + WaitGroup is the Go-native equivalent.
// Wall-clock is now max(handshake_i), capped by ConnectTimeout.
//
// tools.Registry.Register is mutex-protected (see tool.go), so we can
// safely Register from each launching goroutine without coordination.
// Result-collection still happens under a mutex so the returned slices
// have stable ordering matching reg.Servers.
func LaunchAll(ctx context.Context, reg *Registry, registry *tools.Registry) ([]*mcptools.Server, []error) {
	if reg == nil {
		return nil, nil
	}
	type launchResult struct {
		idx int
		srv *mcptools.Server
		err error
	}
	results := make([]launchResult, 0, len(reg.Servers))
	var mu sync.Mutex
	var wg sync.WaitGroup
	for i, s := range reg.Servers {
		if s.Disabled || (s.Command == "" && s.URL == "") {
			continue
		}
		wg.Add(1)
		go func(idx int, name string) {
			defer wg.Done()
			srv, err := LaunchServer(ctx, reg, name, registry)
			mu.Lock()
			results = append(results, launchResult{idx: idx, srv: srv, err: err})
			mu.Unlock()
		}(i, s.Name)
	}
	wg.Wait()
	// Sort by original registry index so a later run of the same registry
	// produces the same ordering — important for the diag/log output the
	// user reads when something's wrong.
	sort.Slice(results, func(i, j int) bool { return results[i].idx < results[j].idx })
	var ok []*mcptools.Server
	var errs []error
	for _, r := range results {
		if r.err != nil {
			errs = append(errs, r.err)
			continue
		}
		ok = append(ok, r.srv)
	}
	return ok, errs
}

// LaunchAllLazy is the kimi-cli-style lazy variant of LaunchAll.
// Mode selection (default auto):
//
//	auto   — for each enabled entry: if ~/.metis/mcp-cache/<name>.json
//	         exists AND its fingerprint matches the current entry,
//	         register stub tools from cache and DO NOT spawn. Otherwise
//	         spawn eagerly (so subsequent runs benefit from the cache).
//	always — register from cache when available; if no cache, still
//	         defer (no spawn) — first tool call triggers spawn-and-cache.
//	         Most aggressive; new servers stay unspawned until used.
//	never  — eager spawn for every entry, ignore the cache. Equivalent
//	         to legacy LaunchAll. Use to debug cache-driven issues.
//
// Result shape matches LaunchAll: ok []*Server is everything
// registered (whether spawned or deferred); errs []error collects the
// per-server failures so setupRuntime can warn-but-keep-going.
func LaunchAllLazy(ctx context.Context, reg *Registry, registry *tools.Registry, mode LazyMode) ([]*mcptools.Server, []error) {
	if reg == nil {
		return nil, nil
	}
	if mode == LazyMCPModeNever {
		return LaunchAll(ctx, reg, registry)
	}
	// Parallel launch — same rationale as LaunchAll above. Cache-hit
	// entries are near-instant (no subprocess), but cache-miss entries
	// still pay the eager spawn cost and serializing those is exactly
	// the user-visible regression we just fixed.
	type launchResult struct {
		idx int
		srv *mcptools.Server
		err error
	}
	results := make([]launchResult, 0, len(reg.Servers))
	var mu sync.Mutex
	var wg sync.WaitGroup
	for i, entry := range reg.Servers {
		if entry.Disabled || (entry.Command == "" && entry.URL == "") {
			continue
		}
		wg.Add(1)
		go func(idx int, e ServerEntry) {
			defer wg.Done()
			srv, err := launchOneMCPLazy(ctx, e, registry, mode)
			mu.Lock()
			results = append(results, launchResult{idx: idx, srv: srv, err: err})
			mu.Unlock()
		}(i, entry)
	}
	wg.Wait()
	sort.Slice(results, func(i, j int) bool { return results[i].idx < results[j].idx })
	var ok []*mcptools.Server
	var errs []error
	for _, r := range results {
		if r.err != nil {
			errs = append(errs, r.err)
		}
		if r.srv != nil {
			ok = append(ok, r.srv)
		}
	}
	return ok, errs
}

// launchOneMCPLazy is the per-entry dispatch path used by
// LaunchAllLazy. Loads the cache, decides between deferred /
// eager spawn, registers tools, returns the resulting Server.
func launchOneMCPLazy(ctx context.Context, entry ServerEntry, registry *tools.Registry, mode LazyMode) (*mcptools.Server, error) {
	// Step 1: env-var expansion happens up-front so the fingerprint
	// reflects the actual launch identity (the same way an eager
	// LaunchServer would see it).
	expanded, missing := expandEnvVarsInEntry(entry)
	if len(missing) > 0 {
		return nil, fmt.Errorf("mcp: server %q references unset env vars: %s "+
			"(use ${VAR:-default} for an inline fallback)",
			entry.Name, strings.Join(missing, ", "))
	}

	wantFP := FingerprintEntry(expanded)
	cache, _ := LoadCache(entry.Name) // LoadCache returns (nil, nil) on missing — fine

	// Cache hit? Register stub tools + defer spawn.
	if cache != nil && cache.Fingerprint == wantFP && len(cache.Tools) > 0 {
		srv := buildLazyServer(expanded, entry, cache.Tools)
		for _, t := range srv.FilteredTools(entry.EnabledTools, entry.DisabledTools) {
			registry.Register(t)
		}
		return srv, nil
	}

	// Cache miss / fingerprint stale / empty cache.
	if mode == LazyMCPModeAlways {
		// Caller explicitly opted into "never spawn at startup, even
		// without cache". Register a server with NO tools — the model
		// can't invoke anything until the cache exists, but the entry
		// is recognized so /mcp surface lists it. The first manual
		// `/mcp reload` (or the next session's startup) will repair.
		srv := buildLazyServer(expanded, entry, nil)
		// No tools to register here — registry stays empty for this
		// server. Surface a warning to the caller.
		return srv, fmt.Errorf("mcp: server %q has no cache and METIS_LAZY_MCP=always — "+
			"run `/mcp reload %s` after first use to populate the cache",
			entry.Name, entry.Name)
	}

	// Auto mode + cache miss → spawn eagerly so we have something to
	// register THIS session, AND seed the cache for next time. This is
	// the "first-run" cost: identical to legacy LaunchAll for new
	// servers, but cheaper for every subsequent run.
	srv, err := LaunchServer(ctx, &Registry{Servers: []ServerEntry{entry}}, entry.Name, registry)
	if err != nil {
		return nil, err
	}
	// Persist the freshly-handshaked tool list for next time.
	saveCacheFromServer(srv, expanded)
	return srv, nil
}

// buildLazyServer composes the deferred-spawn closure that
// mcptools.NewLazyServer needs. The closure captures the entry's
// already-expanded fields plus the transport pick (HTTP vs stdio) so
// the first Execute fires the same code path an eager launch would
// have used at startup.
func buildLazyServer(expanded, original ServerEntry, cachedTools []CachedTool) *mcptools.Server {
	spawn := func(ctx context.Context) (*mcpsdk.Client, error) {
		switch {
		case expanded.URL != "":
			return mcpsdk.NewHTTPClient(ctx, expanded.URL, resolveAuthHeaders(ctx, expanded))
		case expanded.Command != "":
			return mcpsdk.NewStdioClientWithEnv(ctx, expanded.Command,
				envSliceFromMap(maybeInjectCUEnv(expanded.Command, expanded.Env)),
				expanded.Args...)
		default:
			return nil, fmt.Errorf("mcp: server %q has neither command nor url", expanded.Name)
		}
	}
	tools := CachedToolsToMCPTools(cachedTools)
	srv := mcptools.NewLazyServer(expanded.Name, tools, func(ctx context.Context) (*mcpsdk.Client, error) {
		client, err := spawn(ctx)
		if err != nil {
			return nil, err
		}
		// Refresh the cache opportunistically — the model just paid
		// the spawn round-trip; we may as well capture any tool-list
		// drift so the next run's cache is current.
		go func() {
			refreshCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			freshTools, err := client.ListTools(refreshCtx)
			if err != nil {
				return // best-effort; old cache is still better than no cache
			}
			cache := &Cache{
				Fingerprint: FingerprintEntry(expanded),
				Tools:       MCPToolsToCached(freshTools),
			}
			_ = SaveCache(original.Name, cache)
		}()
		return client, nil
	})
	return srv
}

// saveCacheFromServer reads the just-spawned server's already-listed
// tools and writes them to disk. Called by the cache-miss path in
// launchOneMCPLazy so the next session can skip the spawn round-trip.
//
// Tools come from Server.Tools() which returns the per-tool wrappers;
// we have to walk back to (name, description, schema) for the cache
// shape. We use the truncated description that's already been applied
// during wrapClient — that's intentional, because the eager-launch
// path uses truncated descriptions in its prompts too, so the cache
// matches what the eager path would produce.
func saveCacheFromServer(srv *mcptools.Server, expanded ServerEntry) {
	if srv == nil {
		return
	}
	var cached []CachedTool
	for _, t := range srv.Tools() {
		name, desc, schema := splitToolMeta(t)
		// Walk back the "mcp__<server>__" prefix that MCPTool.Name()
		// adds, so the cache stores the raw upstream tool name. The
		// "[MCP] " prefix is added by Description() at serve time,
		// not stored — drop it back out for consistency with how the
		// eager-spawn path lists tools.
		raw := stripMCPNamePrefix(name, expanded.Name)
		rawDesc := strings.TrimPrefix(desc, "[MCP] ")
		cached = append(cached, CachedTool{
			Name:        raw,
			Description: rawDesc,
			InputSchema: schema,
		})
	}
	c := &Cache{
		Fingerprint: FingerprintEntry(expanded),
		Tools:       cached,
	}
	_ = SaveCache(expanded.Name, c)
}

// splitToolMeta extracts a registered Tool's (name, description,
// schema) without reaching into the unexported mcptools.MCPTool
// fields. The Tool interface already exposes these three accessors;
// we just call them.
func splitToolMeta(t toolsTool) (string, string, map[string]any) {
	return t.Name(), t.Description(), t.InputSchema()
}

// toolsTool is the local alias for tools.Tool's read-only subset used
// by the cache-snapshot path. Kept un-imported so this file doesn't
// rely on the tools package's full Tool surface (Concurrency, CanUse,
// Execute) when it only needs the trio of metadata accessors.
type toolsTool interface {
	Name() string
	Description() string
	InputSchema() map[string]any
}

// stripMCPNamePrefix returns the raw tool name without the
// "mcp__<server>__" wrapper that MCPTool.Name() adds. Pulled out so
// the cache-snapshot path doesn't need to know the exact concatenation
// rule.
func stripMCPNamePrefix(full, serverName string) string {
	prefix := "mcp__" + serverName + "__"
	return strings.TrimPrefix(full, prefix)
}

// MergeWithConfig folds entries declared in config.toml's [[mcp.servers]]
// into the runtime registry. Config-declared entries don't overwrite ones
// already in mcp.toml — the user's runtime additions always win.
func (r *Registry) MergeWithConfig(cfgServers []config.MCPServer) {
	for _, s := range cfgServers {
		if s.Name == "" {
			continue
		}
		if FindServer(r, s.Name) != nil {
			continue
		}
		r.Servers = append(r.Servers, ServerEntry{
			Name: s.Name, Command: s.Command,
			Args: append([]string(nil), s.Args...), Disabled: s.Disabled,
		})
	}
}
