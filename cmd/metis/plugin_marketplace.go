package main

// plugin_marketplace.go — `metis plugin marketplace`, `metis plugin
// search`, and `metis plugin install`. The marketplace registry lives
// in internal/runtime/bundled_plugins.go (bundled defaults +
// ~/.metis/marketplaces.json user overrides). This file is the
// CLI-side glue that walks marketplace clones on disk and surfaces
// the plugins they expose.
//
// Pattern parity:
//   - claude-code's `/plugin marketplace add` clones the marketplace
//     repo into ~/.claude/plugins/marketplaces/<name>/ and reads
//     .claude-plugin/marketplace.json. metis mirrors that path
//     (~/.metis/plugins/marketplaces/<name>/) so users can copy
//     their existing claude-code clones over.
//   - `metis plugin install <plugin>@<marketplace>` looks the plugin
//     up in the marketplace's plugins[] list and copies the source
//     directory into ~/.metis/plugins/<plugin>/ where the existing
//     LoadPlugins picks it up.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	rtpkg "github.com/Ricardo-M-L/metis/internal/runtime"
)

// marketplacePluginEntry mirrors the upstream marketplace.json schema:
// {plugins: [{name, description, source, skills, ...}, ...]}.
// metis only consumes the fields it can act on.
//
// `source` is polymorphic across the wild marketplaces:
//   - anthropic-agent-skills uses a path string ("./") — plugin lives
//     inside the marketplace clone itself.
//   - claude-plugins-community uses an object {source:"url", url:..., sha:...}
//     where each plugin is its own external repo.
//
// We keep it as RawMessage so unmarshal never fails on either shape;
// resolveSource() teases out what we can act on.
type marketplacePluginEntry struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Source      json.RawMessage `json:"source"`
	Skills      []string        `json:"skills,omitempty"`
	Homepage    string          `json:"homepage,omitempty"`
}

// pluginSource is the resolved view of the polymorphic Source field.
// Path is non-empty when the plugin lives inside the marketplace
// clone (copy-from-disk install). URL is non-empty when the plugin is
// hosted in a separate repo (currently we tell the user we don't
// install those — claude-plugins-community has 200+ such entries).
type pluginSource struct {
	Path string // relative to marketplace clone, e.g. "./"
	URL  string // external git URL when source is {source:"url", url:...}
	Kind string // "path", "url", "git-subdir", or "" when unrecognized
}

func (e marketplacePluginEntry) resolveSource() pluginSource {
	if len(e.Source) == 0 {
		return pluginSource{Path: "./", Kind: "path"}
	}
	// Try string first.
	var s string
	if err := json.Unmarshal(e.Source, &s); err == nil {
		if s == "" {
			s = "./"
		}
		return pluginSource{Path: s, Kind: "path"}
	}
	// Then object.
	var obj struct {
		Source string `json:"source"`
		URL    string `json:"url"`
		Path   string `json:"path"`
	}
	if err := json.Unmarshal(e.Source, &obj); err == nil {
		return pluginSource{Path: obj.Path, URL: obj.URL, Kind: obj.Source}
	}
	return pluginSource{}
}

type marketplaceManifest struct {
	Name    string                   `json:"name"`
	Plugins []marketplacePluginEntry `json:"plugins"`
}

// cmdPluginMarketplace dispatches list/add/remove on the marketplace
// registry. Mirrors claude-code's `/plugin marketplace ...` shape.
func cmdPluginMarketplace(ctx context.Context, args []string) error {
	sub := "list"
	rest := []string{}
	if len(args) > 0 {
		sub = args[0]
		rest = args[1:]
	}
	switch sub {
	case "list", "ls":
		fmt.Println(rtpkg.LoadMarketplaceRegistry().FormatList())
		return nil
	case "add":
		if len(rest) < 2 {
			return errors.New("usage: metis plugin marketplace add <name> github:<owner>/<repo>")
		}
		src, err := parseMarketplaceSourceArg(rest[1])
		if err != nil {
			return err
		}
		r := rtpkg.LoadMarketplaceRegistry()
		r.Add(rest[0], src)
		if err := r.SaveUserMarketplaces(); err != nil {
			return fmt.Errorf("save: %w", err)
		}
		fmt.Printf("registered marketplace %q → %s:%s\n", rest[0], src.Source, src.Repo)
		return nil
	case "remove", "rm":
		if len(rest) == 0 {
			return errors.New("usage: metis plugin marketplace remove <name>")
		}
		r := rtpkg.LoadMarketplaceRegistry()
		r.Remove(rest[0])
		if err := r.SaveUserMarketplaces(); err != nil {
			return fmt.Errorf("save: %w", err)
		}
		fmt.Printf("unregistered marketplace %q\n", rest[0])
		return nil
	}
	return fmt.Errorf("plugin marketplace: unknown subcommand %q (use: list | add | remove)", sub)
}

// parseMarketplaceSourceArg accepts:
//
//	github:owner/repo   → MarketplaceSource{Source:"github", Repo:"owner/repo"}
//
// More kinds (gitlab:, https://… tarball) can be added without
// breaking existing CLI usage.
func parseMarketplaceSourceArg(s string) (rtpkg.MarketplaceSource, error) {
	if rest, ok := strings.CutPrefix(s, "github:"); ok {
		if !strings.Contains(rest, "/") {
			return rtpkg.MarketplaceSource{}, fmt.Errorf("github source must be owner/repo, got %q", rest)
		}
		return rtpkg.MarketplaceSource{Source: "github", Repo: rest}, nil
	}
	return rtpkg.MarketplaceSource{}, fmt.Errorf("unsupported source format %q (expected github:owner/repo)", s)
}

// ensureMarketplaceClone makes sure the marketplace's repo is on disk,
// cloning if missing. Returns the absolute path to the clone root.
// Network access is required only on the first call per marketplace;
// subsequent calls reuse the existing clone (idempotent).
func ensureMarketplaceClone(ctx context.Context, e rtpkg.MarketplaceEntry) (string, error) {
	dst := rtpkg.MarketplaceClonePath(e.Name)
	if _, err := os.Stat(filepath.Join(dst, ".git")); err == nil {
		return dst, nil
	}
	cloneURL := e.CloneURL()
	if cloneURL == "" {
		return "", fmt.Errorf("marketplace %s: unsupported source kind %q", e.Name, e.Source.Source)
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return "", err
	}
	cmd := exec.CommandContext(ctx, "git", "clone", "--depth", "1", cloneURL, dst)
	cmd.Stderr = os.Stderr
	cmd.Stdout = os.Stderr // keep stdout clean for scripted callers
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git clone %s: %w", cloneURL, err)
	}
	return dst, nil
}

// readMarketplaceManifest reads .claude-plugin/marketplace.json out
// of the cloned marketplace dir. Compat with claude-code's layout.
func readMarketplaceManifest(cloneRoot string) (*marketplaceManifest, error) {
	data, err := os.ReadFile(filepath.Join(cloneRoot, ".claude-plugin", "marketplace.json"))
	if err != nil {
		return nil, fmt.Errorf("read marketplace.json: %w", err)
	}
	var m marketplaceManifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("parse marketplace.json: %w", err)
	}
	return &m, nil
}

// cmdPluginSearch walks every registered marketplace and prints the
// plugins whose name OR description contains the query (case-
// insensitive substring). Marketplaces that haven't been cloned yet
// are skipped silently — the user has to install once before search
// can index them. Hint printed at the bottom when nothing matched.
func cmdPluginSearch(ctx context.Context, query string) error {
	q := strings.ToLower(query)
	r := rtpkg.LoadMarketplaceRegistry()
	hits := 0
	for _, name := range r.ListNames() {
		e := r.Entries[name]
		dst := rtpkg.MarketplaceClonePath(name)
		if _, err := os.Stat(dst); err != nil {
			continue // not cloned yet
		}
		m, err := readMarketplaceManifest(dst)
		if err != nil {
			fmt.Fprintf(os.Stderr, "metis: marketplace %s: %v\n", name, err)
			continue
		}
		for _, p := range m.Plugins {
			matched := strings.Contains(strings.ToLower(p.Name), q) ||
				strings.Contains(strings.ToLower(p.Description), q)
			// Also match the individual skill paths so users searching
			// for "docx" or "webapp" find the umbrella plugin (
			// document-skills, example-skills) that bundles them.
			var matchedSkills []string
			for _, sk := range p.Skills {
				if strings.Contains(strings.ToLower(sk), q) {
					matched = true
					matchedSkills = append(matchedSkills, sk)
				}
			}
			if !matched {
				continue
			}
			hits++
			fmt.Printf("  %s@%s\n      %s\n", p.Name, name, p.Description)
			if len(matchedSkills) > 0 {
				fmt.Printf("      matches skill(s): %s\n", strings.Join(matchedSkills, ", "))
			}
		}
		_ = e
	}
	if hits == 0 {
		fmt.Println("(no matches — try `metis plugin install <plugin>@anthropic-agent-skills` to clone the first marketplace then search again)")
	}
	return nil
}

// cmdPluginInstall resolves <plugin>@<marketplace>, ensures the
// marketplace is cloned, copies the plugin's source directory into
// ~/.metis/plugins/<plugin>/, and prints the resulting path. The
// existing LoadPlugins picks it up on the next chat startup.
//
// When @<marketplace> is omitted we walk all registered marketplaces
// and install the first match — claude-code's "implicit marketplace"
// shortcut. Ambiguous matches print a list and require the @ form.
func cmdPluginInstall(ctx context.Context, target string) error {
	pluginName, marketName, hasMarket := strings.Cut(target, "@")
	r := rtpkg.LoadMarketplaceRegistry()

	var candidates []struct {
		marketName string
		entry      marketplacePluginEntry
		root       string
	}
	for _, mname := range r.ListNames() {
		if hasMarket && mname != marketName {
			continue
		}
		e := r.Entries[mname]
		dst, err := ensureMarketplaceClone(ctx, e)
		if err != nil {
			fmt.Fprintf(os.Stderr, "metis: marketplace %s: %v\n", mname, err)
			continue
		}
		m, err := readMarketplaceManifest(dst)
		if err != nil {
			fmt.Fprintf(os.Stderr, "metis: marketplace %s: %v\n", mname, err)
			continue
		}
		for _, p := range m.Plugins {
			if p.Name == pluginName {
				candidates = append(candidates, struct {
					marketName string
					entry      marketplacePluginEntry
					root       string
				}{mname, p, dst})
			}
		}
	}
	if len(candidates) == 0 {
		return fmt.Errorf("plugin %q not found in any registered marketplace (use `metis plugin search %s`)",
			pluginName, pluginName)
	}
	if len(candidates) > 1 && !hasMarket {
		fmt.Fprintln(os.Stderr, "ambiguous plugin name — pick one:")
		for _, c := range candidates {
			fmt.Fprintf(os.Stderr, "  metis plugin install %s@%s\n", c.entry.Name, c.marketName)
		}
		return errors.New("multiple matches — use the @marketplace form")
	}
	c := candidates[0]
	resolved := c.entry.resolveSource()
	if resolved.Kind != "path" {
		// claude-plugins-community uses {source:"url", url:..., sha:...}
		// where each plugin is its own repo. Cloning those would add a
		// second-tier git fetch + sha pinning we haven't built yet.
		// Tell the user where the source lives so they can clone manually.
		return fmt.Errorf("plugin %s uses source kind %q (url=%s) — external-repo plugins aren't installable yet; clone manually into %s/",
			c.entry.Name, resolved.Kind, resolved.URL, rtpkg.PluginsDir())
	}
	src := filepath.Join(c.root, resolved.Path)
	dst := filepath.Join(rtpkg.PluginsDir(), c.entry.Name)
	if _, err := os.Stat(dst); err == nil {
		return fmt.Errorf("plugin %s already installed at %s — `metis plugin remove %s --yes` first",
			c.entry.Name, dst, c.entry.Name)
	}
	if err := copyDir(src, dst); err != nil {
		return fmt.Errorf("copy plugin: %w", err)
	}
	// If the upstream marketplace.json scoped which skills this plugin
	// owns (the `skills` field of marketplacePluginEntry), promote that
	// list into a synthesized plugin.toml when one isn't already
	// present. Without this, installing a marketplace plugin whose
	// source directory had no plugin.toml would silently load nothing.
	manifestPath := filepath.Join(dst, "plugin.toml")
	if _, err := os.Stat(manifestPath); err != nil && len(c.entry.Skills) > 0 {
		var b strings.Builder
		fmt.Fprintf(&b, "manifest_version = 1\n")
		fmt.Fprintf(&b, "name = %q\n", c.entry.Name)
		fmt.Fprintf(&b, "version = \"0.0.0\"\n")
		fmt.Fprintf(&b, "description = %q\n", c.entry.Description)
		fmt.Fprintf(&b, "skills = [\n")
		for _, sk := range c.entry.Skills {
			// Marketplace.json paths look like "./skills/docx" — we
			// rewrite to the SKILL.md inside that folder, which is
			// what metis's LoadPlugins expects.
			rel := strings.TrimPrefix(sk, "./")
			fmt.Fprintf(&b, "  %q,\n", filepath.Join(rel, "SKILL.md"))
		}
		fmt.Fprintf(&b, "]\n")
		if err := os.WriteFile(manifestPath, []byte(b.String()), 0o644); err != nil {
			return fmt.Errorf("synthesize plugin.toml: %w", err)
		}
	}
	fmt.Printf("installed %s@%s → %s\n", c.entry.Name, c.marketName, dst)
	fmt.Println("restart metis chat to load the new plugin (`metis plugin list` to verify)")
	return nil
}

// copyDir does a recursive directory copy preserving mode bits for
// scripts (.py, .sh get +x) the same way EnsureBundledPlugins handles
// extracted skills. Used by `metis plugin install` to materialize a
// marketplace plugin into ~/.metis/plugins/.
func copyDir(src, dst string) error {
	return filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		s, err := os.Open(path)
		if err != nil {
			return err
		}
		defer s.Close()
		mode := os.FileMode(0o644)
		switch strings.ToLower(filepath.Ext(rel)) {
		case ".py", ".sh":
			mode = 0o755
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
		if err != nil {
			return err
		}
		if _, err := io.Copy(f, s); err != nil {
			_ = f.Close()
			return err
		}
		return f.Close()
	})
}
