package mcp

// mcp_cache.go — persisted MCP tool-schema cache under ~/.metis/mcp-cache/.
//
// Purpose: skip the "spawn server + handshake + ListTools" round-trip on
// metis startup. The schema rarely changes between sessions, so storing
// it on disk lets startup register stub tools without spawning the
// subprocess (kimi-cli's `defer_mcp_tool_loading` pattern). The actual
// server is spawned on first tool invocation; until then the process,
// fds, and RAM stay un-allocated.
//
// File layout:
//
//	~/.metis/mcp-cache/<server-name>.json
//
//	{
//	  "fingerprint": "sha256:abc…",
//	  "cached_at":   "2026-05-11T06:30:00Z",
//	  "tools":       [{name, description, input_schema}, ...]
//	}
//
// Fingerprint covers (command, args, url, headers) — anything that
// changes the server identity invalidates the cache automatically. If
// the user edits mcp.toml to point a server at a new binary, the next
// startup spawns it fresh and rewrites the cache.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/Ricardo-M-L/metis/internal/config"
	mcpsdk "github.com/Ricardo-M-L/metis/internal/mcp"
)

// CachedTool mirrors mcp.Tool with JSON tags chosen to match the
// upstream MCP protocol shape — keeps the file readable + reusable if
// another tool wants to consume metis's cache.
type CachedTool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

// Cache is the on-disk envelope. Loose fields (cached_at, version)
// are informational — the load path only uses Fingerprint + Tools.
type Cache struct {
	Fingerprint string       `json:"fingerprint"`
	CachedAt    string       `json:"cached_at"`
	Tools       []CachedTool `json:"tools"`
}

// CacheDir returns the canonical cache directory under metis home.
// Sibling of mcp.toml so a single `rm -rf ~/.metis/mcp-cache` flushes
// every server's stale schemas without touching the registry.
func CacheDir() string {
	return filepath.Join(config.Home(), "mcp-cache")
}

// CachePath returns the full path for one server's cache file.
// File name is the literal server name; mcp.toml constrains the legal
// charset (no `/` etc.) since the same string lands in `mcp__<name>__*`
// tool prefixes the LLM sees.
func CachePath(serverName string) string {
	return filepath.Join(CacheDir(), serverName+".json")
}

// FingerprintEntry computes a SHA-256 over the entry's launch identity.
// Covers (command, args, url, headers) so any edit to mcp.toml that
// would actually change which process gets spawned invalidates the
// cache. `name` is NOT part of the fingerprint — file name covers that
// dimension, and rename-without-change should still hit the cache.
//
// Header values are sorted by key before hashing so map-iteration
// nondeterminism doesn't produce different fingerprints for identical
// configs (this would flap the cache on every restart).
func FingerprintEntry(e ServerEntry) string {
	h := sha256.New()
	fmt.Fprintf(h, "cmd:%s\n", e.Command)
	fmt.Fprintf(h, "cwd:%s\n", e.WorkingDir)
	for _, a := range e.Args {
		fmt.Fprintf(h, "arg:%s\n", a)
	}
	fmt.Fprintf(h, "url:%s\n", e.URL)
	keys := make([]string, 0, len(e.Headers))
	for k := range e.Headers {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(h, "hdr:%s=%s\n", k, e.Headers[k])
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil))
}

// LoadCache reads the cache for one server. Returns (nil, nil) on
// missing-file — callers should treat that as "no cache, spawn fresh"
// rather than a hard error. Malformed JSON is a hard error so we
// don't silently downgrade to no-cache when something has gone wrong.
func LoadCache(serverName string) (*Cache, error) {
	p := CachePath(serverName)
	raw, err := os.ReadFile(p)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read %s: %w", p, err)
	}
	var c Cache
	if err := json.Unmarshal(raw, &c); err != nil {
		return nil, fmt.Errorf("decode %s: %w", p, err)
	}
	return &c, nil
}

// SaveCache writes (or overwrites) the cache for one server.
// Atomic via tempfile + rename so a partial write can't strand
// callers with a half-parsed cache (json.Unmarshal would reject and
// we'd respawn — annoying but recoverable). Same security stance as
// Save: 0o600 because cached schemas may reflect tool params that
// reference paths or argv shapes worth keeping out of broad reads.
func SaveCache(serverName string, c *Cache) error {
	if c == nil {
		return fmt.Errorf("mcp_cache: nil cache for %q", serverName)
	}
	dir := CacheDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("mkdir %s: %w", dir, err)
	}
	if c.CachedAt == "" {
		c.CachedAt = time.Now().UTC().Format(time.RFC3339)
	}
	raw, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return fmt.Errorf("encode cache for %q: %w", serverName, err)
	}
	final := CachePath(serverName)
	tmp, err := os.CreateTemp(dir, "."+serverName+".*.json")
	if err != nil {
		return fmt.Errorf("tempfile: %w", err)
	}
	tmpPath := tmp.Name()
	defer func() { _ = os.Remove(tmpPath) }()
	if _, err := tmp.Write(raw); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write tempfile: %w", err)
	}
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

// CachedToolsToMCPTools converts the cache shape back into mcp.Tool —
// what the existing wrapClient code already knows how to register.
// Lets the lazy launch path reuse the same per-tool registration
// rather than duplicating the wrapper logic.
func CachedToolsToMCPTools(in []CachedTool) []mcpsdk.Tool {
	out := make([]mcpsdk.Tool, len(in))
	for i, t := range in {
		out[i] = mcpsdk.Tool{
			Name:        t.Name,
			Description: t.Description,
			InputSchema: t.InputSchema,
		}
	}
	return out
}

// MCPToolsToCached is the inverse: snapshot a freshly-fetched tool
// list into the cache shape so SaveCache can persist it.
func MCPToolsToCached(in []mcpsdk.Tool) []CachedTool {
	out := make([]CachedTool, len(in))
	for i, t := range in {
		out[i] = CachedTool{
			Name:        t.Name,
			Description: t.Description,
			InputSchema: t.InputSchema,
		}
	}
	return out
}

// LazyMode is the user-facing knob for METIS_LAZY_MCP.
// Mirrors the LazyMode tri-state from lazy_tools.go for consistency:
//
//	(unset) / "auto" → use cache when valid, spawn-and-cache on miss
//	"always"         → never spawn at startup; require a cache hit
//	                    (servers without one are deferred until first
//	                    tool call — same as auto but more aggressive)
//	"never"          → eager spawn at startup (legacy behavior)
type LazyMode int

const (
	LazyMCPModeAuto LazyMode = iota
	LazyMCPModeAlways
	LazyMCPModeNever
)

// ParseLazyMode resolves the METIS_LAZY_MCP env value. Trimmed +
// lowercased; unknown values fall back to Auto rather than erroring,
// so a typo doesn't break startup.
func ParseLazyMode(value string) LazyMode {
	v := strings.ToLower(strings.TrimSpace(value))
	switch v {
	case "always", "true", "1", "yes":
		return LazyMCPModeAlways
	case "never", "false", "0", "no", "off":
		return LazyMCPModeNever
	}
	return LazyMCPModeAuto
}
