package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/config"
	protocol "github.com/Ricardo-M-L/metis/internal/mcp"
	rtpkg "github.com/Ricardo-M-L/metis/internal/runtime"
	runtimemcp "github.com/Ricardo-M-L/metis/internal/runtime/mcp"
	mcptools "github.com/Ricardo-M-L/metis/internal/tools/mcp"
)

func TestDesktopMCPServersSnapshotMergedAndNeverLaunches(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	registry := &runtimemcp.Registry{Servers: []runtimemcp.ServerEntry{
		{Name: "remote", URL: "https://example.test/?secret=secret-value", Headers: map[string]string{"Authorization": "secret-value"}},
		{Name: "disabled", Command: "secret-value", Disabled: true},
		{Name: "lazy", Command: "secret-value", Args: []string{"secret-value"}},
	}}
	if err := runtimemcp.Save(registry); err != nil {
		t.Fatal(err)
	}
	launchCount := 0
	lazy := mcptools.NewLazyServer("lazy", []protocol.Tool{{Name: "read"}}, func(context.Context) (*protocol.Client, error) {
		launchCount++
		return nil, nil
	})
	rt := &runtime{mcpServers: []*mcptools.Server{lazy}}
	snapshot, err := desktopMCPServersSnapshot(rt, []config.MCPServer{{Name: "remote", Command: "ignored"}, {Name: "legacy", Command: "secret-value"}})()
	if err != nil {
		t.Fatal(err)
	}
	views := snapshot.Servers
	byName := make(map[string]string)
	for _, v := range views {
		byName[v.Name] = v.Source + "/" + v.Transport + "/" + v.Status
	}
	for name, want := range map[string]string{"remote": "user/http/stopped", "disabled": "user/stdio/disabled", "lazy": "user/stdio/lazy", "legacy": "config/stdio/stopped"} {
		if byName[name] != want {
			t.Errorf("%s = %s, want %s", name, byName[name], want)
		}
	}
	for _, v := range views {
		if strings.Contains(v.Name+v.Source+v.Transport+v.Status, "secret-value") {
			t.Fatal("sensitive config leaked")
		}
		if v.Name == "lazy" && v.ToolCount != 1 {
			t.Fatalf("cached tool count = %d", v.ToolCount)
		}
	}
	if launchCount != 0 {
		t.Fatal("read-only snapshot started a server")
	}
}

func TestDesktopMCPServersSnapshotSurvivesRuntimePluginCleanup(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	rt := &runtime{plugins: &rtpkg.PluginRegistry{}}
	snapshot := desktopMCPServersSnapshot(rt, nil)
	cleanupDone := make(chan struct{})
	go func() {
		rt.plugins = nil // mirrors the runtime cleanup boundary
		close(cleanupDone)
	}()
	for i := 0; i < 20; i++ {
		if _, err := snapshot(); err != nil {
			t.Fatal(err)
		}
	}
	<-cleanupDone
	if _, err := snapshot(); err != nil {
		t.Fatal(err)
	}
}

func TestDesktopPluginMCPViewsListUnloadedAndRetainLiveStatus(t *testing.T) {
	root := t.TempDir()
	bundle := filepath.Join(root, "bundle")
	if err := os.Mkdir(bundle, 0o700); err != nil {
		t.Fatal(err)
	}
	content := `name = "bundle"
[[mcp_servers]]
name = "reader"
command = "sensitive-command"
args = ["secret-value"]
disabled = true
[[mcp_servers]]
name = "new"
url = "https://example.test/?secret=private-token"
`
	if err := os.WriteFile(filepath.Join(bundle, "plugin.toml"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	live := []rtpkg.PluginMCPStatus{{Name: "bundle / reader", Status: "registered", Transport: "stdio", Enabled: true, ToolCount: 3}, {Name: "removed / old", Status: "registered", Enabled: true}}
	invalid := filepath.Join(root, "invalid")
	if err := os.Mkdir(invalid, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(invalid, "plugin.toml"), []byte("invalid private text"), 0o600); err != nil {
		t.Fatal(err)
	}
	views, incomplete, err := desktopPluginMCPViews(root, live)
	if err != nil {
		t.Fatal(err)
	}
	if !incomplete {
		t.Fatal("invalid plugin must be reported as incomplete coverage")
	}
	byName := map[string]struct {
		enabled           bool
		status, transport string
		count             int
	}{}
	for _, v := range views {
		byName[v.Name] = struct {
			enabled           bool
			status, transport string
			count             int
		}{v.Enabled, v.Status, v.Transport, v.ToolCount}
	}
	if got := byName["bundle / reader"]; got.enabled || got.status != "registered" || got.count != 3 {
		t.Fatalf("changed configuration lost live state: %+v", got)
	}
	if got := byName["bundle / new"]; !got.enabled || got.status != "stopped" || got.transport != "http" {
		t.Fatalf("unloaded plugin absent: %+v", got)
	}
	if got := byName["removed / old"]; got.status != "registered" {
		t.Fatalf("live removed plugin absent: %+v", got)
	}
	for _, v := range views {
		if strings.Contains(v.Name+v.Transport+v.Status, "secret") {
			t.Fatal("plugin secret leaked")
		}
	}
}
