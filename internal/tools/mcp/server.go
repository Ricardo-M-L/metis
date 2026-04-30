// Package mcp_tools wraps an MCP server as Metis tools.
package mcp_tools

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/Ricardo-M-L/metis/internal/mcp"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

// Server wraps an MCP server as a Metis tool registry entry.
type Server struct {
	client *mcp.Client
	name   string
	tools  map[string]*MCPTool
}

type MCPTool struct {
	name        string
	description string
	inputSchema map[string]any
	server      *Server
}

func (t *MCPTool) Name() string        { return "mcp__" + t.server.name + "__" + t.name }
func (t *MCPTool) Description() string { return "[MCP] " + t.description }
func (t *MCPTool) InputSchema() map[string]any {
	if t.inputSchema == nil {
		return map[string]any{"type": "object", "properties": map[string]any{}}
	}
	return t.inputSchema
}
func (t *MCPTool) Concurrency(map[string]any) tools.Concurrency { return tools.ConcurrencySafe }

func (t *MCPTool) CanUse(_ context.Context, in map[string]any) (tools.Permission, string) {
	return tools.PermissionAllow, "mcp server tool"
}

func (t *MCPTool) Execute(ctx context.Context, in map[string]any) (*tools.Result, error) {
	args := make(map[string]any)
	for k, v := range in {
		args[k] = v
	}
	result, err := t.server.client.CallTool(ctx, t.name, args)
	if err != nil {
		return &tools.Result{Output: err.Error(), IsError: true}, nil
	}
	// Try to pretty-print JSON result
	var pretty json.RawMessage
	if json.Unmarshal(result, &pretty) == nil {
		b, _ := json.MarshalIndent(pretty, "", "  ")
		return &tools.Result{Output: string(b)}, nil
	}
	return &tools.Result{Output: string(result)}, nil
}

// NewServer connects to a stdio MCP server (subprocess) and returns a
// Server with its tools registered as Metis tools.
func NewServer(ctx context.Context, name, command string, args ...string) (*Server, error) {
	client, err := mcp.NewStdioClient(ctx, command, args...)
	if err != nil {
		return nil, fmt.Errorf("MCP server %q: %w", name, err)
	}
	return wrapClient(ctx, name, client)
}

// NewHTTPServer connects to a remote MCP server over the Streamable
// HTTP transport. `endpoint` is the single URL the spec uses for both
// POST and GET-SSE; `headers` lets the caller attach auth, e.g.
// `{"Authorization": "Bearer …"}`.
func NewHTTPServer(ctx context.Context, name, endpoint string, headers map[string]string) (*Server, error) {
	client, err := mcp.NewHTTPClient(ctx, endpoint, headers)
	if err != nil {
		return nil, fmt.Errorf("MCP server %q: %w", name, err)
	}
	return wrapClient(ctx, name, client)
}

// wrapClient is the shared post-handshake step: list the server's
// tools and build the per-tool wrappers. Used by both stdio and HTTP
// constructors so transport choice doesn't change registration logic.
func wrapClient(ctx context.Context, name string, client *mcp.Client) (*Server, error) {
	mcpTools, err := client.ListTools(ctx)
	if err != nil {
		client.Close()
		return nil, fmt.Errorf("list tools from %q: %w", name, err)
	}
	s := &Server{client: client, name: name, tools: make(map[string]*MCPTool)}
	for _, t := range mcpTools {
		tool := &MCPTool{
			name:        t.Name,
			description: t.Description,
			inputSchema: t.InputSchema,
			server:      s,
		}
		s.tools[t.Name] = tool
	}
	return s, nil
}

// Tools returns all tools from the MCP server.
func (s *Server) Tools() []tools.Tool {
	out := make([]tools.Tool, 0, len(s.tools))
	for _, t := range s.tools {
		out = append(out, t)
	}
	return out
}

// Close shuts down the MCP server connection.
func (s *Server) Close() error {
	return s.client.Close()
}
