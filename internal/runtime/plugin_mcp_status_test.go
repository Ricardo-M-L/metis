package runtime

import (
	"context"
	"errors"
	"testing"

	protocol "github.com/Ricardo-M-L/metis/internal/mcp"
	tools "github.com/Ricardo-M-L/metis/internal/tools/mcp"
	plugin "github.com/Ricardo-M-L/metis/pkg/plugin"
)

func TestPluginMCPServerStatusesReadOnlyAndConsistentAfterClose(t *testing.T) {
	started := false
	server := tools.NewLazyServer(pluginMCPServerName("bundle", "reader"), []protocol.Tool{{Name: "read"}}, func(context.Context) (*protocol.Client, error) { started = true; return nil, nil })
	reg := &PluginRegistry{plugins: []*Plugin{{
		Manifest: plugin.Manifest{Name: "bundle", MCPServers: []plugin.MCPServerSpec{
			{Name: "reader", URL: "https://example.test/?secret=do-not-expose"},
			{Name: "off", Command: "private-command", Disabled: true},
		}}, MCPServers: []*tools.Server{server},
	}}}
	views := reg.MCPServerStatuses()
	if len(views) != 2 || views[0].Name != "bundle / reader" || views[0].Status != "lazy" || views[0].Transport != "http" || views[0].ToolCount != 1 || views[1].Status != "disabled" {
		t.Fatalf("views = %+v", views)
	}
	if started {
		t.Fatal("status snapshot started an MCP process")
	}
	if err := reg.CloseMCPServers(); err != nil {
		t.Fatal(err)
	}
	views = reg.MCPServerStatuses()
	if views[0].Status != "stopped" {
		t.Fatalf("status after close = %+v", views[0])
	}
}

func TestMCPStatusDoesNotDescribeFailedOrClosedLazyServerAsAvailable(t *testing.T) {
	server := tools.NewLazyServer("broken", []protocol.Tool{{Name: "read"}}, func(context.Context) (*protocol.Client, error) {
		return nil, errors.New("private diagnostics")
	})
	if server.SnapshotStatus() != "lazy" {
		t.Fatal("server should be lazy before starting")
	}
	_, _ = server.Tools()[0].Execute(context.Background(), nil)
	if server.SnapshotStatus() != "stopped" {
		t.Fatal("failed server must not appear ready")
	}
	if err := server.Close(); err != nil {
		t.Fatal(err)
	}
	if server.SnapshotStatus() != "stopped" {
		t.Fatal("closed server must not appear ready")
	}
}
