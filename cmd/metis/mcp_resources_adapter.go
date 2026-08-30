package main

// mcp_resources_adapter.go — bridges the live MCP server set (populated
// asynchronously into rt.mcpServers) to the ListMcpResources /
// ReadMcpResource tools. Defined here (not in internal/tools/builtin) so
// the tools stay decoupled from internal/tools/mcp, and so the server
// list is read live at Execute time rather than captured at registration.

import (
	"context"
	"fmt"
	"strings"

	"github.com/Ricardo-M-L/metis/internal/tools/builtin"
	mcptools "github.com/Ricardo-M-L/metis/internal/tools/mcp"
)

type mcpResourceAdapter struct{ rt *runtime }

// liveServers snapshots the current server list under the mutex.
func (a mcpResourceAdapter) liveServers() []*mcptools.Server {
	a.rt.mcpServersMu.Lock()
	defer a.rt.mcpServersMu.Unlock()
	out := make([]*mcptools.Server, len(a.rt.mcpServers))
	copy(out, a.rt.mcpServers)
	return out
}

func (a mcpResourceAdapter) ListResources(ctx context.Context) []builtin.MCPResourceEntry {
	var out []builtin.MCPResourceEntry
	for _, s := range a.liveServers() {
		rs, err := s.ListResources(ctx)
		if err != nil {
			continue // server without the capability / mid-handshake — skip
		}
		for _, r := range rs {
			out = append(out, builtin.MCPResourceEntry{
				Server: s.Name(), URI: r.URI, Name: r.Name,
				Description: r.Description, MimeType: r.MimeType,
			})
		}
	}
	return out
}

func (a mcpResourceAdapter) ReadResource(ctx context.Context, server, uri string) (string, error) {
	servers := a.liveServers()
	for _, s := range servers {
		if server != "" && s.Name() != server {
			continue
		}
		res, err := s.ReadResource(ctx, uri)
		if err != nil {
			// When a specific server was named, surface its error; else
			// keep trying other servers for the uri.
			if server != "" {
				return "", err
			}
			continue
		}
		var b strings.Builder
		for _, c := range res.Contents {
			if c.Text != "" {
				b.WriteString(c.Text)
			} else if c.Blob != "" {
				fmt.Fprintf(&b, "[binary resource %s, %s, %d base64 bytes omitted]", c.URI, c.MimeType, len(c.Blob))
			}
		}
		return b.String(), nil
	}
	if server != "" {
		return "", fmt.Errorf("no connected MCP server named %q", server)
	}
	return "", fmt.Errorf("no connected MCP server accepted this resource handle; list resources again")
}
