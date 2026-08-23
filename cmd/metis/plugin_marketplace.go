package main

// plugin_marketplace.go — `metis plugin marketplace`, `metis plugin
// search`, and `metis plugin install`. The marketplace registry lives
// in internal/runtime/bundled_plugins.go (bundled defaults +
// ~/.metis/marketplaces.json user overrides). This file is the
// CLI-side glue for the shared marketplace manager used by Desktop.
// Keeping search and install on the same implementation means Codex,
// Claude, local-cache, and remote Git compatibility rules cannot drift.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/Ricardo-M-L/metis/internal/pluginmarket"
	rtpkg "github.com/Ricardo-M-L/metis/internal/runtime"
)

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

// cmdPluginSearch walks every registered marketplace and prints the
// plugins whose name OR description contains the query (case-
// insensitive substring). Marketplaces that haven't been cloned yet
// are skipped silently — the user has to install once before search
// can index them. Hint printed at the bottom when nothing matched.
func cmdPluginSearch(ctx context.Context, query string) error {
	q := strings.ToLower(query)
	catalog := pluginmarket.NewManager().Catalog()
	hits := 0
	for _, p := range catalog.Plugins {
		haystack := strings.ToLower(strings.Join([]string{
			p.Name, p.DisplayName, p.Description, p.Developer, p.Category,
			strings.Join(p.Keywords, " "), strings.Join(p.Capabilities, " "), strings.Join(p.Skills, " "),
		}, " "))
		if strings.Contains(haystack, "ppt") || strings.Contains(haystack, "powerpoint") ||
			strings.Contains(haystack, "presentation") || strings.Contains(haystack, "slide") || strings.Contains(haystack, "deck") {
			haystack += " ppt pptx powerpoint presentation slides deck 幻灯片 演示文稿"
		}
		if !strings.Contains(haystack, q) {
			continue
		}
		hits++
		status := p.Compatibility
		if !p.Installable {
			status = "unavailable"
		}
		fmt.Printf("  %s@%s  [%s]\n      %s\n", p.Name, p.Marketplace, status, p.Description)
	}
	if hits == 0 {
		fmt.Println("(no matches in synced or local marketplaces — run Desktop marketplace Sync to refresh remote catalogs)")
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
	manager := pluginmarket.NewManager()
	if hasMarket {
		result, err := manager.Install(ctx, pluginName, marketName)
		if err != nil {
			return err
		}
		fmt.Printf("installed %s@%s → %s\n", result.Name, result.Source, result.Path)
		fmt.Println("restart metis chat to load the new plugin (`metis plugin list` to verify)")
		return nil
	}
	catalog := manager.Catalog()
	if catalog.NeedsSync {
		_, catalog = manager.Sync(ctx, nil)
	}
	var candidates []pluginmarket.Plugin
	for _, plugin := range catalog.Plugins {
		if plugin.Name == pluginName && plugin.Installable {
			candidates = append(candidates, plugin)
		}
	}
	if len(candidates) == 0 {
		return fmt.Errorf("plugin %q not found in any registered marketplace (use `metis plugin search %s`)",
			pluginName, pluginName)
	}
	if len(candidates) > 1 && !hasMarket {
		fmt.Fprintln(os.Stderr, "ambiguous plugin name — pick one:")
		for _, c := range candidates {
			fmt.Fprintf(os.Stderr, "  metis plugin install %s@%s\n", c.Name, c.Marketplace)
		}
		return errors.New("multiple matches — use the @marketplace form")
	}
	result, err := manager.Install(ctx, candidates[0].Name, candidates[0].Marketplace)
	if err != nil {
		return err
	}
	fmt.Printf("installed %s@%s → %s\n", result.Name, result.Source, result.Path)
	fmt.Println("restart metis chat to load the new plugin (`metis plugin list` to verify)")
	return nil
}
