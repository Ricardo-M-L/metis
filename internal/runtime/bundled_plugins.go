package runtime

// bundled_plugins.go — bundled plugin marketplace registry. metis ships
// a small JSON file (default_marketplaces.json) that pre-registers
// the canonical Anthropic plugin marketplaces (anthropic-agent-skills,
// claude-plugins-official, claude-plugins-community). Users get
// `metis plugin search` / `metis plugin install` working out of the
// box without manually `metis plugin marketplace add <url>` first.
//
// We deliberately do NOT bundle the plugins themselves (that path was
// abandoned 2026-05-10 after a 3.8 MB embed prototype — too heavy
// for a feature most users won't touch). The user still gets the
// "discover" affordance; download is on-demand.

import (
	"embed"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// defaultMarketplacesFS holds the pre-registered marketplace pointers.
// The single file inside is a JSON map keyed by marketplace name.
//
//go:embed default_marketplaces.json
var defaultMarketplacesFS embed.FS

// MarketplaceSource describes where a marketplace lives. Mirrors the
// shape claude-code uses in ~/.claude/plugins/known_marketplaces.json
// so existing tooling / docs concepts carry over.
type MarketplaceSource struct {
	// Source is the locator type. "github" means Repo is
	// "<owner>/<name>" and we clone via https://github.com/<repo>.git.
	// Future kinds: "gitlab", "url" (tarball), "git" (arbitrary URL).
	Source string `json:"source"`
	Repo   string `json:"repo,omitempty"`
}

// MarketplaceEntry is one row in the registry. lastUpdated is local
// (set when we last refreshed the clone), nil when never fetched.
type MarketplaceEntry struct {
	Name            string            `json:"name"`
	Source          MarketplaceSource `json:"source"`
	InstallLocation string            `json:"installLocation,omitempty"`
	LastUpdated     string            `json:"lastUpdated,omitempty"`
	Builtin         bool              `json:"builtin,omitempty"` // true for the bundled defaults
}

// MarketplaceRegistry is the on-disk + bundled merged view that the
// marketplace command operates on. Disk takes precedence — a user
// who manually `metis plugin marketplace add <name> <url>` overrides
// the bundled default, same way ~/.metis/mcp.toml overrides
// config.toml's [[mcp.servers]].
type MarketplaceRegistry struct {
	Entries map[string]MarketplaceEntry
}

// LoadMarketplaceRegistry merges bundled defaults with the user's
// overrides at <metis-home>/marketplaces.json. User overrides win.
// Never errors: a missing user file is the common path, and a
// corrupted bundled JSON would only happen during a botched build —
// in that case the user still gets an empty registry rather than a
// startup-blocking failure.
func LoadMarketplaceRegistry() *MarketplaceRegistry {
	r := &MarketplaceRegistry{Entries: map[string]MarketplaceEntry{}}
	// Bundled defaults — read from the embedded JSON.
	if data, err := defaultMarketplacesFS.ReadFile("default_marketplaces.json"); err == nil {
		var bundled map[string]MarketplaceEntry
		if err := json.Unmarshal(data, &bundled); err == nil {
			for name, e := range bundled {
				e.Name = name
				e.Builtin = true
				r.Entries[name] = e
			}
		}
	}
	// User overrides at <home>/marketplaces.json.
	if data, err := os.ReadFile(userMarketplaceFile()); err == nil {
		var user map[string]MarketplaceEntry
		if err := json.Unmarshal(data, &user); err == nil {
			for name, e := range user {
				e.Name = name
				e.Builtin = false
				r.Entries[name] = e
			}
		}
	}
	return r
}

// SaveUserMarketplaces persists every NON-bundled entry to disk.
// Builtins are omitted: we re-derive them from embed each run, and
// writing them would freeze the version on disk forever.
func (r *MarketplaceRegistry) SaveUserMarketplaces() error {
	out := map[string]MarketplaceEntry{}
	for name, e := range r.Entries {
		if e.Builtin {
			continue
		}
		out[name] = e
	}
	if err := os.MkdirAll(filepath.Dir(userMarketplaceFile()), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(userMarketplaceFile(), data, 0o644)
}

// Add registers a new marketplace (or overrides a bundled one). Caller
// is expected to call SaveUserMarketplaces afterwards.
func (r *MarketplaceRegistry) Add(name string, src MarketplaceSource) {
	r.Entries[name] = MarketplaceEntry{
		Name:    name,
		Source:  src,
		Builtin: false,
	}
}

// Remove deletes a marketplace. Bundled entries can be hidden by
// adding a user override with the same name and an empty source —
// the loader's "user wins" rule will mask the bundled entry.
func (r *MarketplaceRegistry) Remove(name string) {
	delete(r.Entries, name)
}

// CloneURL returns the git clone target for a marketplace entry.
// Empty when the source kind isn't recognized.
func (e MarketplaceEntry) CloneURL() string {
	switch strings.ToLower(e.Source.Source) {
	case "github":
		return "https://github.com/" + e.Source.Repo + ".git"
	}
	return ""
}

// MarketplaceCloneRoot is where cloned marketplace repositories live
// on disk. Each marketplace clones to <root>/<name>/. Mirrors
// claude-code's ~/.claude/plugins/marketplaces/ layout for muscle-
// memory parity.
func MarketplaceCloneRoot() string {
	return filepath.Join(home(), "plugins", "marketplaces")
}

// MarketplaceClonePath returns the on-disk path a given marketplace
// clones to.
func MarketplaceClonePath(name string) string {
	return filepath.Join(MarketplaceCloneRoot(), name)
}

// userMarketplaceFile is the path to the user-override registry.
func userMarketplaceFile() string {
	return filepath.Join(home(), "marketplaces.json")
}

// home resolves the metis data root. Mirrors internal/jobs::home() so
// METIS_HOME tests work the same way across packages.
func home() string {
	if h := os.Getenv("METIS_HOME"); h != "" {
		return h
	}
	if h, err := os.UserHomeDir(); err == nil {
		return filepath.Join(h, ".metis")
	}
	return "."
}

// EnsureBundledPlugins is kept as a no-op shim so existing call sites
// (cmd/metis/main.go's setupRuntime, cmd/metis/plugin.go's list)
// don't need conditional compilation. Returns no installed names and
// no errors — the bundled-plugin embed approach was abandoned in
// favour of bundled marketplaces.
//
// Callers that want the marketplace registry should use
// LoadMarketplaceRegistry directly.
func EnsureBundledPlugins() (installed []string, errs []error) {
	return nil, nil
}

// FormatList renders a human-readable listing for
// `metis plugin marketplace list`. Stable alphabetical order.
func (r *MarketplaceRegistry) FormatList() string {
	if len(r.Entries) == 0 {
		return "(no marketplaces registered — `metis plugin marketplace add <name> github:<owner>/<repo>`)"
	}
	names := make([]string, 0, len(r.Entries))
	for n := range r.Entries {
		names = append(names, n)
	}
	// Inline sort to avoid pulling sort just for one call.
	for i := 1; i < len(names); i++ {
		for j := i; j > 0 && names[j-1] > names[j]; j-- {
			names[j-1], names[j] = names[j], names[j-1]
		}
	}
	var b strings.Builder
	for _, n := range names {
		e := r.Entries[n]
		tag := ""
		if e.Builtin {
			tag = " (bundled)"
		}
		fmt.Fprintf(&b, "  %s%s\n      %s:%s\n", e.Name, tag, e.Source.Source, e.Source.Repo)
		if e.LastUpdated != "" {
			fmt.Fprintf(&b, "      last updated: %s\n", e.LastUpdated)
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

// ListNames returns marketplace names in alphabetical order. Used by
// `metis plugin marketplace list` for stable scripted output.
func (r *MarketplaceRegistry) ListNames() []string {
	names := make([]string, 0, len(r.Entries))
	for n := range r.Entries {
		names = append(names, n)
	}
	for i := 1; i < len(names); i++ {
		for j := i; j > 0 && names[j-1] > names[j]; j-- {
			names[j-1], names[j] = names[j], names[j-1]
		}
	}
	return names
}
