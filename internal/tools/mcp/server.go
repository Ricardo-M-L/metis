// Package mcp_tools wraps an MCP server as Metis tools.
package mcp_tools

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/Ricardo-M-L/metis/internal/mcp"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

// maxToolDescriptionBytes caps a single MCP tool's description length
// before it lands in the system prompt. Some servers (notably ones
// auto-generated from OpenAPI specs — see Claude Code's
// MAX_MCP_DESCRIPTION_LENGTH = 2048) dump 15-60 KB of endpoint docs
// into tool.description. Without a cap, three such servers eat the
// whole context window before the user types a single message.
//
// 2048 chars matches CC's chosen p95-tail point: enough for "what does
// this tool do + when to use" but not for full API references.
const maxToolDescriptionBytes = 2048

// Server wraps an MCP server as a Metis tool registry entry.
//
// Two lifecycle modes:
//
//	eager (NewServer / NewHTTPServer): client is connected, tools listed,
//	   and `tools` populated at construction time.
//	lazy (NewLazyServer): client is nil at construction; `tools` is
//	   pre-populated from a disk cache, and the actual subprocess (or
//	   HTTP handshake) is deferred until the first Execute. After
//	   spawn, the freshly-fetched tool list is also saved back via
//	   the spawn closure so the next run uses the latest cache.
//
// The two modes share Execute / Close / Tools / FilteredTools — only
// the construction path differs. Callers shouldn't need to know which
// mode they got back; the lazy spawn is invisible past the API.
type Server struct {
	client *mcp.Client
	name   string
	tools  map[string]*MCPTool

	// spawn is set only by NewLazyServer. It captures the deferred
	// connection logic; ensureClient() runs it through spawnOnce so
	// concurrent Execute calls don't double-spawn. spawnErr is the
	// sticky error from a failed spawn — subsequent ensureClient
	// calls return it immediately rather than retrying (a kimi-cli-
	// style flapping spawn would otherwise hammer a broken binary
	// once per tool call).
	spawn     func(context.Context) (*mcp.Client, error)
	spawnOnce sync.Once
	spawnErr  error
	mu        sync.RWMutex
}

type MCPTool struct {
	tools.BaseTool // default IsEnabled() = true; MCP tools are always exposed once their server is registered
	name           string
	description    string
	inputSchema    map[string]any
	server         *Server
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
	if err := t.server.ensureClient(ctx); err != nil {
		// First-spawn failure for a lazy server. Surface as a tool
		// error so the model sees a clean message rather than a
		// nil-pointer panic from t.server.client below.
		return &tools.Result{
			Output:  fmt.Sprintf("MCP server %q failed to start: %v", t.server.name, err),
			IsError: true,
		}, nil
	}
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
			description: truncateDescription(t.Description),
			inputSchema: t.InputSchema,
			server:      s,
		}
		s.tools[t.Name] = tool
	}
	return s, nil
}

// NewLazyServer constructs a Server in the deferred-spawn lifecycle:
// tools are pre-registered from a previously cached schema, and the
// actual subprocess (or HTTP handshake) doesn't fire until the model
// invokes a tool that calls Execute.
//
// Inputs:
//
//	name        — the server's logical name (mcp__<name>__* prefix).
//	cachedTools — tool list to register up-front (typically loaded
//	              from ~/.metis/mcp-cache/<name>.json via runtime.LoadMCPCache).
//	spawn       — closure that creates the live mcp.Client when invoked.
//	              Encapsulates "stdio vs HTTP", env-var expansion, etc.
//	              Runs exactly once across all concurrent Execute callers
//	              (sync.Once), so the underlying subprocess is single-
//	              instance even under parallel tool dispatch.
//
// Returns immediately; no I/O happens until ensureClient is called.
// If the cached schema is stale (real server's tools differ), the
// model will hit a "method not found" from the live server on the
// first wrong call — that surfaces cleanly as a tool error rather
// than an undetected mismatch. The lazy launcher refreshes the cache
// on successful spawn (via the spawn closure), so the next session
// uses the corrected list.
func NewLazyServer(name string, cachedTools []mcp.Tool, spawn func(context.Context) (*mcp.Client, error)) *Server {
	s := &Server{
		name:  name,
		tools: make(map[string]*MCPTool),
		spawn: spawn,
	}
	for _, t := range cachedTools {
		s.tools[t.Name] = &MCPTool{
			name:        t.Name,
			description: truncateDescription(t.Description),
			inputSchema: t.InputSchema,
			server:      s,
		}
	}
	return s
}

// ensureClient is the gateway every Execute crosses. Three states:
//
//	client != nil    → eager mode or already-spawned lazy mode; return.
//	spawn == nil     → eager mode with a closed client; surface error.
//	spawnErr != nil  → previous spawn already failed; return cached
//	                    error rather than re-attempting (sticky-fail).
//	else             → first call: run spawn through sync.Once,
//	                    publish the client (or spawnErr) for everyone.
//
// Why sticky-fail vs retry: a broken MCP binary that the user hasn't
// fixed (wrong path, missing dep) will fail the same way on every
// call. Retrying just spams logs and keeps the model in a loop. The
// user can edit mcp.toml and restart metis to retry.
func (s *Server) ensureClient(ctx context.Context) error {
	s.mu.RLock()
	if s.client != nil {
		s.mu.RUnlock()
		return nil
	}
	if s.spawn == nil {
		s.mu.RUnlock()
		return fmt.Errorf("server %q has no client and no lazy spawn fn", s.name)
	}
	prevErr := s.spawnErr
	s.mu.RUnlock()
	if prevErr != nil {
		return prevErr
	}
	s.spawnOnce.Do(func() {
		client, err := s.spawn(ctx)
		s.mu.Lock()
		defer s.mu.Unlock()
		if err != nil {
			s.spawnErr = err
			return
		}
		s.client = client
	})
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.spawnErr
}

// IsSpawned reports whether the lazy server has connected its client
// yet. Returns true for eager servers immediately, and for lazy servers
// only after a successful ensureClient. Used by metrics / /context /
// /mcp status surfaces to show "loaded vs deferred".
func (s *Server) IsSpawned() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.client != nil
}

// truncateDescription clamps a tool description to maxToolDescriptionBytes,
// appending an ellipsis marker so the LLM (and the human reading logs)
// can tell content was elided rather than just sentence-ending mid-word.
// Operates on bytes — UTF-8 truncation could land mid-rune, so we walk
// back to the last valid boundary before appending.
func truncateDescription(desc string) string {
	if len(desc) <= maxToolDescriptionBytes {
		return desc
	}
	// Walk back to a UTF-8 boundary. Bytes 0x80-0xBF are continuation
	// bytes; the start of a multi-byte rune is 0xC0+ or ASCII (<0x80).
	cut := maxToolDescriptionBytes
	for cut > 0 && (desc[cut]&0xC0) == 0x80 {
		cut--
	}
	return desc[:cut] + "… (truncated)"
}

// Tools returns all tools from the MCP server.
func (s *Server) Tools() []tools.Tool {
	out := make([]tools.Tool, 0, len(s.tools))
	for _, t := range s.tools {
		out = append(out, t)
	}
	return out
}

// FilteredTools returns the subset of tools after applying optional
// allow/deny lists. Empty `enabled` means "allow everything"; empty
// `disabled` means "deny nothing". Names match against the server-side
// unqualified tool name (e.g. "screenshot"), so users configure
// `enabled_tools = ["screenshot", "left_click"]` rather than the longer
// `mcp__computer-use__screenshot` form they already see in prompts.
//
// Lifted from Codex's mcp_servers.<id>.enabled_tools / disabled_tools
// semantics — Claude Code uses a hardcoded equivalent for IDE servers
// (ALLOWED_IDE_TOOLS in client.ts:567), and the per-server config form
// is what makes this useful for arbitrary servers.
func (s *Server) FilteredTools(enabled, disabled []string) []tools.Tool {
	if len(enabled) == 0 && len(disabled) == 0 {
		return s.Tools()
	}
	allow := map[string]struct{}{}
	for _, n := range enabled {
		allow[n] = struct{}{}
	}
	deny := map[string]struct{}{}
	for _, n := range disabled {
		deny[n] = struct{}{}
	}
	out := make([]tools.Tool, 0, len(s.tools))
	for name, t := range s.tools {
		if len(allow) > 0 {
			if _, ok := allow[name]; !ok {
				continue
			}
		}
		if _, denied := deny[name]; denied {
			continue
		}
		out = append(out, t)
	}
	return out
}

// Close shuts down the MCP server connection. No-op for a lazy server
// that was never spawned — there's no client to close and no
// subprocess to reap. (Returning nil here is intentional: shutdown
// fan-out in setupRuntime calls Close() on every registered server,
// including deferred ones; a "you never started this" error would be
// noise.)
func (s *Server) Close() error {
	s.mu.RLock()
	c := s.client
	s.mu.RUnlock()
	if c == nil {
		return nil
	}
	return c.Close()
}

// Name returns the server's logical name (the `mcp__<name>__*` prefix).
// Used by the prompts auto-registrar to compose slash command names.
func (s *Server) Name() string { return s.name }

// ListPrompts surfaces the underlying client's prompts/list. Servers
// without the capability return (nil, nil) so the registrar can treat
// "no prompts" the same as "this server doesn't speak prompts". For
// a lazy server that hasn't spawned yet, we return (nil, nil) too —
// prompts registration runs at startup and a deferred server simply
// gets no prompts this session. (They'll be picked up next session
// after the spawn-and-cache cycle completes.)
func (s *Server) ListPrompts(ctx context.Context) ([]mcp.Prompt, error) {
	s.mu.RLock()
	c := s.client
	s.mu.RUnlock()
	if c == nil {
		return nil, nil
	}
	return c.ListPrompts(ctx)
}

// GetPrompt resolves a prompt template against `args`. Pass-through to
// the underlying client; here so /mcp__<server>__<prompt> can render
// without callers having to reach into the unexported client field.
// Forces the lazy server to spawn first, since rendering a prompt is
// an explicit user action (a slash command invocation).
func (s *Server) GetPrompt(ctx context.Context, name string, args map[string]string) (*mcp.GetPromptResult, error) {
	if err := s.ensureClient(ctx); err != nil {
		return nil, err
	}
	return s.client.GetPrompt(ctx, name, args)
}
