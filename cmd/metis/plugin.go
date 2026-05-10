package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"

	rtpkg "github.com/Ricardo-M-L/metis/internal/runtime"
	pubplugin "github.com/Ricardo-M-L/metis/pkg/plugin"
)

// cmdPlugin handles `metis plugin <subcommand>`.
//
// Today: list, info, remove. install/reload deferred to a follow-up
// (install needs git-clone path discovery, reload needs hot-reload
// machinery; both are non-trivial). For now users install plugins by
// hand-creating ~/.metis/plugins/<name>/plugin.toml + the bundled files.
func cmdPlugin(ctx context.Context, args []string) error {
	sub := "list"
	rest := []string{}
	if len(args) > 0 {
		sub = args[0]
		rest = args[1:]
	}
	switch sub {
	case "list", "ls":
		return cmdPluginList(ctx)
	case "info":
		if len(rest) == 0 {
			return errors.New("usage: metis plugin info <name>")
		}
		return cmdPluginInfo(ctx, rest[0])
	case "marketplace", "mp":
		return cmdPluginMarketplace(ctx, rest)
	case "search":
		if len(rest) == 0 {
			return errors.New("usage: metis plugin search <query>")
		}
		return cmdPluginSearch(ctx, strings.Join(rest, " "))
	case "install":
		if len(rest) == 0 {
			return errors.New("usage: metis plugin install <plugin>[@<marketplace>]")
		}
		return cmdPluginInstall(ctx, rest[0])
	case "remove", "rm":
		if len(rest) == 0 {
			return errors.New("usage: metis plugin remove <name> [--yes]")
		}
		// Allow --yes anywhere after the name (positional flexibility for
		// shell shortcuts: `plugin rm foo --yes` or `plugin rm --yes foo`).
		var name string
		yes := false
		for _, a := range rest {
			if a == "--yes" || a == "-y" {
				yes = true
				continue
			}
			if name == "" {
				name = a
			}
		}
		if name == "" {
			return errors.New("usage: metis plugin remove <name> [--yes]")
		}
		return cmdPluginRemove(name, yes)
	case "help", "-h", "--help":
		printPluginUsage()
		return nil
	}
	return fmt.Errorf("plugin: unknown subcommand %q (use: list | info | marketplace | search | install | remove)", sub)
}

func printPluginUsage() {
	fmt.Println(`metis plugin — manage plugins + plugin marketplaces

Usage:
  metis plugin list                            List installed plugins
  metis plugin info <name>                     Show one plugin's manifest
  metis plugin search <query>                  Search across all marketplaces
  metis plugin install <plugin>[@<marketplace>]
                                               Clone marketplace if needed, copy plugin to ~/.metis/plugins/
  metis plugin remove <name> [--yes]           Delete an installed plugin
  metis plugin marketplace list                List registered marketplaces
  metis plugin marketplace add <name> github:<owner>/<repo>
                                               Register a new marketplace source
  metis plugin marketplace remove <name>       Unregister a marketplace

Bundled marketplaces (auto-registered, override by adding a user entry):
  anthropic-agent-skills      → github:anthropics/skills
  claude-plugins-official     → github:anthropics/claude-plugins-official
  claude-plugins-community    → github:anthropics/claude-plugins-community

Set METIS_NO_BUNDLED_PLUGINS=1 to ignore the bundled defaults.`)
}

// cmdPluginList prints every installed plugin's name + version + status,
// without spawning anything. Loader-spawn happens at chat startup.
func cmdPluginList(ctx context.Context) error {
	// Trigger first-run install so a fresh `metis plugin list` reflects
	// the bundled set instead of "(no plugins installed)". Idempotent
	// after the initial extract; honours METIS_NO_BUNDLED_PLUGINS=1.
	installed, _ := rtpkg.EnsureBundledPlugins()
	for _, name := range installed {
		fmt.Fprintf(os.Stderr, "\033[2m[metis] installed bundled plugin: %s\033[0m\n", name)
	}
	dir := rtpkg.PluginsDir()
	ents, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			fmt.Println("(no plugins installed)")
			return nil
		}
		return err
	}
	count := 0
	for _, e := range ents {
		if !e.IsDir() {
			continue
		}
		// `marketplaces/` is a sibling tree (cloned marketplace repos),
		// not a plugin. Skip it so `plugin list` doesn't try to read a
		// nonexistent plugin.toml at its root.
		if e.Name() == "marketplaces" {
			continue
		}
		manifestPath := filepath.Join(dir, e.Name(), "plugin.toml")
		var m pubplugin.Manifest
		if _, err := toml.DecodeFile(manifestPath, &m); err != nil {
			fmt.Printf("  %-20s  ⚠ unreadable manifest: %v\n", e.Name(), err)
			count++
			continue
		}
		mcpStatus := ""
		if m.MCPServer != nil {
			mcpStatus = " [mcp]"
		}
		fmt.Printf("  %-20s  %s  %s%s\n", m.Name, m.Version, m.Description, mcpStatus)
		count++
	}
	if count == 0 {
		fmt.Println("(no plugins installed — drop a dir under ~/.metis/plugins/)")
	}
	_ = ctx
	return nil
}

func cmdPluginInfo(ctx context.Context, name string) error {
	manifestPath := filepath.Join(rtpkg.PluginsDir(), name, "plugin.toml")
	var m pubplugin.Manifest
	if _, err := toml.DecodeFile(manifestPath, &m); err != nil {
		return fmt.Errorf("plugin %s: %w", name, err)
	}
	fmt.Printf("name:        %s\n", m.Name)
	fmt.Printf("version:     %s\n", m.Version)
	fmt.Printf("description: %s\n", m.Description)
	if m.License != "" {
		fmt.Printf("license:     %s\n", m.License)
	}
	if m.Homepage != "" {
		fmt.Printf("homepage:    %s\n", m.Homepage)
	}
	if m.MCPServer != nil {
		fmt.Println("mcp_server:")
		fmt.Printf("  command:   %s\n", m.MCPServer.Command)
		if len(m.MCPServer.Args) > 0 {
			fmt.Printf("  args:      %v\n", m.MCPServer.Args)
		}
		if len(m.MCPServer.Env) > 0 {
			fmt.Printf("  env:       %v\n", m.MCPServer.Env)
		}
	}
	if len(m.Skills) > 0 {
		fmt.Printf("skills:      %v\n", m.Skills)
	}
	if len(m.Hooks) > 0 {
		fmt.Printf("hooks:       %d declared\n", len(m.Hooks))
	}
	_ = ctx
	return nil
}

func cmdPluginRemove(name string, yes bool) error {
	dir := filepath.Join(rtpkg.PluginsDir(), name)
	if _, err := os.Stat(dir); err != nil {
		return fmt.Errorf("plugin %s: not installed", name)
	}
	// Defense-in-depth against `plugin rm ../../etc`: verify the
	// resolved path is still inside PluginsDir() before recursing rm.
	abs, err := filepath.Abs(dir)
	if err != nil {
		return fmt.Errorf("plugin %s: resolve path: %w", name, err)
	}
	rootAbs, err := filepath.Abs(rtpkg.PluginsDir())
	if err != nil {
		return fmt.Errorf("plugin %s: resolve root: %w", name, err)
	}
	if !strings.HasPrefix(abs, rootAbs+string(filepath.Separator)) {
		return fmt.Errorf("plugin %s: refusing to remove outside %s", name, rootAbs)
	}

	// Plugin removal is destructive (deletes the whole directory). Without
	// --yes we just print the path so the user can review; with --yes we
	// rm -rf the dir.
	if !yes {
		fmt.Printf("would remove: %s\n", dir)
		fmt.Println("(re-run with --yes to actually delete)")
		return nil
	}
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("plugin %s: %w", name, err)
	}
	fmt.Printf("removed: %s\n", dir)
	return nil
}
