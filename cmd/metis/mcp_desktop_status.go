package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/Ricardo-M-L/metis/internal/config"
	"github.com/Ricardo-M-L/metis/internal/pluginmanifest"
	rtpkg "github.com/Ricardo-M-L/metis/internal/runtime"
	runtimemcp "github.com/Ricardo-M-L/metis/internal/runtime/mcp"
	"github.com/Ricardo-M-L/metis/internal/webui"
	pubplugin "github.com/Ricardo-M-L/metis/pkg/plugin"
)

// desktopMCPServersSnapshot lists locally configured servers and handles
// owned by this Desktop backend; independent turn workers have their own
// runtimes. This function never launches or probes an MCP process.
func desktopMCPServersSnapshot(rt *runtime, configured []config.MCPServer) func() (webui.MCPServersSnapshot, error) {
	// Runtime config is a startup snapshot; the user registry may change later
	// through /mcp, so read it afresh for each explicit UI refresh.
	legacy := append([]config.MCPServer(nil), configured...)
	// Cleanup can clear rt.plugins after shutdown begins. Capture the startup
	// pointer before serving requests; the registry itself serializes reads and
	// shutdown, including a slow request that outlives the HTTP drain timeout.
	var pluginRegistry *rtpkg.PluginRegistry
	if rt != nil {
		pluginRegistry = rt.plugins
	}
	return func() (webui.MCPServersSnapshot, error) {
		registry, err := runtimemcp.Load()
		if err != nil {
			return webui.MCPServersSnapshot{}, err
		}
		entries := make(map[string]webui.MCPServerView)
		for _, server := range registry.Servers {
			transport := "unknown"
			if server.URL != "" {
				transport = "http"
			} else if server.Command != "" {
				transport = "stdio"
			}
			status := "stopped"
			if server.Disabled {
				status = "disabled"
			}
			entries[server.Name] = webui.MCPServerView{Name: server.Name, Source: "user", Transport: transport, Enabled: !server.Disabled, Status: status}
		}
		for _, server := range legacy {
			if server.Name == "" {
				continue
			}
			if _, exists := entries[server.Name]; exists {
				continue
			}
			transport := "unknown"
			if server.Command != "" {
				transport = "stdio"
			}
			status := "stopped"
			if server.Disabled {
				status = "disabled"
			}
			entries[server.Name] = webui.MCPServerView{Name: server.Name, Source: "config", Transport: transport, Enabled: !server.Disabled, Status: status}
		}
		if rt != nil {
			rt.mcpServersMu.Lock()
			for _, server := range rt.mcpServers {
				if server == nil {
					continue
				}
				name := server.Name()
				if strings.TrimSpace(name) == "" {
					continue
				}
				view, exists := entries[name]
				if !exists {
					view = webui.MCPServerView{Name: name, Source: "runtime", Transport: "unknown", Enabled: true}
				}
				view.Status = server.SnapshotStatus()
				view.ToolCount = len(server.Tools())
				entries[name] = view
			}
			rt.mcpServersMu.Unlock()
		}
		result := make([]webui.MCPServerView, 0, len(entries))
		for _, view := range entries {
			result = append(result, view)
		}
		var loaded []rtpkg.PluginMCPStatus
		if pluginRegistry != nil {
			loaded = pluginRegistry.MCPServerStatuses()
		}
		pluginViews, incomplete, err := desktopPluginMCPViews(rtpkg.PluginsDir(), loaded)
		if err != nil {
			return webui.MCPServersSnapshot{}, err
		}
		result = append(result, pluginViews...)
		return webui.MCPServersSnapshot{Servers: result, PluginScanIncomplete: incomplete}, nil
	}
}

// desktopPluginMCPViews enumerates validated installed plugin declarations,
// including plugins that failed to load or were installed after startup.
// Manifest errors are intentionally not returned because they can contain
// private paths; the UI directs users to the Plugins page for invalid entries.
func desktopPluginMCPViews(root string, loaded []rtpkg.PluginMCPStatus) ([]webui.MCPServerView, bool, error) {
	dirs, err := os.ReadDir(root)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, false, err
	}
	views := make(map[string]webui.MCPServerView)
	incomplete := false
	for _, entry := range dirs {
		if entry.Name() == "marketplaces" {
			continue
		}
		// Do not follow a link outside the managed plugin directory. Surface
		// incomplete coverage rather than silently asserting no servers exist.
		if entry.Type()&os.ModeSymlink != 0 {
			incomplete = true
			continue
		}
		if !entry.IsDir() {
			continue
		}
		manifest, err := pluginmanifest.Read(filepath.Join(root, entry.Name()), entry.Name())
		if err != nil {
			incomplete = true
			continue
		}
		specs := append([]pubplugin.MCPServerSpec(nil), manifest.MCPServers...)
		if manifest.MCPServer != nil {
			specs = append([]pubplugin.MCPServerSpec{*manifest.MCPServer}, specs...)
		}
		for _, spec := range specs {
			serverName := spec.Name
			if serverName == "" {
				serverName = "default"
			}
			name := manifest.Name + " / " + serverName
			transport := "unknown"
			if spec.URL != "" {
				transport = "http"
			} else if spec.Command != "" {
				transport = "stdio"
			}
			status := "stopped"
			if spec.Disabled {
				status = "disabled"
			}
			views[name] = webui.MCPServerView{Name: name, Source: "plugin", Transport: transport, Enabled: !spec.Disabled, Status: status}
		}
	}
	for _, live := range loaded {
		view, exists := views[live.Name]
		if !exists {
			view = webui.MCPServerView{Name: live.Name, Source: "plugin", Transport: live.Transport, Enabled: live.Enabled}
		}
		view.Status, view.ToolCount = live.Status, live.ToolCount
		views[live.Name] = view
	}
	result := make([]webui.MCPServerView, 0, len(views))
	for _, view := range views {
		result = append(result, view)
	}
	return result, incomplete, nil
}
