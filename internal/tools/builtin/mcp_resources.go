package builtin

// mcp_resources.go — ListMcpResources + ReadMcpResource tools, letting
// the model discover and read resources (files, DB rows, API payloads)
// that connected MCP servers expose. Mirrors claude-code's
// ListMcpResourcesTool / ReadMcpResourceTool.
//
// Decoupled from the live MCP server set via MCPResourceProvider so this
// package doesn't depend on internal/tools/mcp (and to read the
// async-populated server list at Execute time, not registration time).
// cmd/metis supplies the adapter over the runtime's server list.

import (
	"context"
	"fmt"
	"strings"

	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

// MCPResourceEntry is one resource advertised by some connected server.
type MCPResourceEntry struct {
	Server      string
	URI         string
	Name        string
	Description string
	MimeType    string
}

// MCPResourceProvider exposes the live MCP servers' resources. Read at
// Execute time so it reflects servers that finished their async handshake
// after the tool was registered.
type MCPResourceProvider interface {
	ListResources(ctx context.Context) []MCPResourceEntry
	// ReadResource reads `uri` from `server` (server may be empty to mean
	// "any server that has this uri"). Returns the concatenated text.
	ReadResource(ctx context.Context, server, uri string) (string, error)
}

// ---- ListMcpResources ----

type ListMcpResources struct {
	tools.BaseTool
	gate     *permission.Gate
	provider MCPResourceProvider
}

func NewListMcpResources(gate *permission.Gate, p MCPResourceProvider) ListMcpResources {
	return ListMcpResources{gate: gate, provider: p}
}

func (ListMcpResources) Name() string { return "ListMcpResources" }
func (ListMcpResources) Description() string {
	return "List the resources (files, records, API payloads) exposed by connected MCP servers. Returns server/uri/name/description lines. Use ReadMcpResource to fetch one by uri. Empty when no server exposes resources."
}
func (ListMcpResources) InputSchema() map[string]any {
	return map[string]any{
		"type":       "object",
		"properties": map[string]any{},
	}
}
func (ListMcpResources) Concurrency(map[string]any) tools.Concurrency { return tools.ConcurrencySafe }
func (l ListMcpResources) CanUse(_ context.Context, _ map[string]any) (tools.Permission, string) {
	d, src := l.gate.Check(context.Background(), "ListMcpResources", "")
	return mapDecision(d), src
}
func (l ListMcpResources) Execute(ctx context.Context, _ map[string]any) (*tools.Result, error) {
	if l.provider == nil {
		return &tools.Result{Output: "ListMcpResources unavailable: no MCP server registry wired", IsError: true}, nil
	}
	entries := l.provider.ListResources(ctx)
	if len(entries) == 0 {
		return &tools.Result{Output: "(no MCP server exposes resources)"}, nil
	}
	var b strings.Builder
	for _, e := range entries {
		fmt.Fprintf(&b, "%s\t%s", e.Server, e.URI)
		if e.Name != "" {
			fmt.Fprintf(&b, "\t%s", e.Name)
		}
		if e.Description != "" {
			fmt.Fprintf(&b, "\t— %s", e.Description)
		}
		b.WriteByte('\n')
	}
	return &tools.Result{Output: strings.TrimRight(b.String(), "\n")}, nil
}

// ---- ReadMcpResource ----

type ReadMcpResource struct {
	tools.BaseTool
	gate     *permission.Gate
	provider MCPResourceProvider
}

func NewReadMcpResource(gate *permission.Gate, p MCPResourceProvider) ReadMcpResource {
	return ReadMcpResource{gate: gate, provider: p}
}

func (ReadMcpResource) Name() string { return "ReadMcpResource" }
func (ReadMcpResource) Description() string {
	return "Read the contents of an MCP resource by uri (from ListMcpResources). Optionally scope to a `server` when the same uri exists on multiple servers. Returns the resource's text."
}
func (ReadMcpResource) InputSchema() map[string]any {
	return map[string]any{
		"type":     "object",
		"required": []string{"uri"},
		"properties": map[string]any{
			"uri":    map[string]any{"type": "string", "description": "the resource URI to read (see ListMcpResources)"},
			"server": map[string]any{"type": "string", "description": "optional MCP server name to disambiguate"},
		},
	}
}
func (ReadMcpResource) Concurrency(map[string]any) tools.Concurrency { return tools.ConcurrencySafe }
func (r ReadMcpResource) CanUse(_ context.Context, in map[string]any) (tools.Permission, string) {
	d, src := r.gate.Check(context.Background(), "ReadMcpResource", strFromAny(in["uri"]))
	return mapDecision(d), src
}
func (r ReadMcpResource) Execute(ctx context.Context, in map[string]any) (*tools.Result, error) {
	if r.provider == nil {
		return &tools.Result{Output: "ReadMcpResource unavailable: no MCP server registry wired", IsError: true}, nil
	}
	uri, _ := in["uri"].(string)
	uri = strings.TrimSpace(uri)
	if uri == "" {
		return &tools.Result{Output: "`uri` is required (see ListMcpResources)", IsError: true}, nil
	}
	server, _ := in["server"].(string)
	out, err := r.provider.ReadResource(ctx, strings.TrimSpace(server), uri)
	if err != nil {
		return &tools.Result{Output: err.Error(), IsError: true}, nil
	}
	if strings.TrimSpace(out) == "" {
		return &tools.Result{Output: "(resource is empty)"}, nil
	}
	return &tools.Result{Output: out}, nil
}
