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
	return fmt.Errorf("plugin: unknown subcommand %q (use: list | info <name> | remove <name>)", sub)
}

func printPluginUsage() {
	fmt.Println(`metis plugin — manage MCP-bundle plugins under ~/.metis/plugins/

Usage:
  metis plugin list             List installed plugins (manifest-only scan)
  metis plugin info <name>      Show one plugin's manifest details
  metis plugin remove <name> [--yes]
                                Delete a plugin directory (--yes / -y to actually delete)

To install: drop a plugin directory under ~/.metis/plugins/<name>/ with a
plugin.toml manifest. See pkg/plugin/manifest.go for the schema. The
runtime auto-loads plugins on next startup; tools register as
plugin__<name>__<tool>.`)
}

// cmdPluginList prints every installed plugin's name + version + status,
// without spawning anything. Loader-spawn happens at chat startup.
func cmdPluginList(ctx context.Context) error {
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
