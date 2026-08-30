// Package mcp_tools wraps an MCP server as Metis tools.
package mcp_tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"mime"
	"strings"
	"sync"

	"github.com/Ricardo-M-L/metis/internal/mcp"
	"github.com/Ricardo-M-L/metis/internal/sandbox"
	"github.com/Ricardo-M-L/metis/internal/security"
	"github.com/Ricardo-M-L/metis/internal/tools"
	pubtool "github.com/Ricardo-M-L/metis/pkg/tool"
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
	spawn       func(context.Context) (*mcp.Client, error)
	spawnOnce   sync.Once
	spawnErr    error
	spawnCancel context.CancelFunc
	closed      bool
	mu          sync.RWMutex
}

var errMCPServerClosed = errors.New("MCP server is closed")

type MCPTool struct {
	tools.BaseTool // default IsEnabled() = true; MCP tools are always exposed once their server is registered
	name           string
	description    string
	inputSchema    map[string]any
	server         *Server
}

func (t *MCPTool) Name() string                     { return "mcp__" + t.server.name + "__" + t.name }
func (t *MCPTool) Description() string              { return "[MCP] " + t.description }
func (t *MCPTool) ToolExposure() tools.ToolExposure { return tools.ToolExposureDeferred }
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
	client, err := t.server.clientForCall(ctx)
	if err != nil {
		// First-spawn failure for a lazy server. Surface as a tool
		// error so the model sees a clean message rather than a
		// nil-pointer panic from t.server.client below.
		return redactMCPToolResult(nil, &tools.Result{
			Output:  fmt.Sprintf("MCP server %q failed to start: %v", t.server.name, err),
			IsError: true,
		}), nil
	}
	args := make(map[string]any)
	for k, v := range in {
		args[k] = v
	}
	result, err := client.CallTool(ctx, t.name, args)
	if err != nil {
		return redactMCPToolResult(client, &tools.Result{Output: friendlyMCPError(t.server.name, err), IsError: true}), nil
	}
	// MCP tool responses follow a structured envelope
	//   {"content": [{"type":"text","text":"..."},
	//                {"type":"image","data":"<base64>","mimeType":"image/png"}],
	//    "isError": false}
	// Split text vs image parts so:
	//   * image bytes land in tools.Result.Images — the dispatch layer
	//     fans them out as proper image ContentBlocks the compactor /
	//     image-pruner can actually see (pre-2026-05-27 every cu
	//     screenshot was dumped into Output as a base64 string blob,
	//     invisible to PruneOldImages and counted as plain text by the
	//     estimator, which is what made Kimi blow the 262k cap)
	//   * text parts become the human-readable Output summary
	//   * isError marks the whole result as failed without losing the
	//     text payload the model needs to recover
	parsed, ok := parseMCPResponse(result)
	if ok {
		return redactMCPToolResult(client, parsed), nil
	}
	// Fallback for non-standard / malformed responses: best-effort
	// pretty-print. Same behaviour as pre-fix — keeps weird providers
	// (lazy MCP servers that returned a raw string) working.
	var pretty json.RawMessage
	if json.Unmarshal(result, &pretty) == nil {
		b, _ := json.MarshalIndent(pretty, "", "  ")
		return redactMCPToolResult(client, &tools.Result{Output: string(b)}), nil
	}
	return redactMCPToolResult(client, &tools.Result{Output: string(result)}), nil
}

// redactMCPToolResult is the final model-facing boundary for MCP tool text.
// Only Output is rewritten; binary image attachments must remain byte-for-byte
// unchanged so screenshots and other media stay valid.
func redactMCPToolResult(client *mcp.Client, result *tools.Result) *tools.Result {
	if result == nil {
		return nil
	}
	if client == nil {
		result.Output = security.RedactSubprocessText(result.Output)
		return result
	}
	result.Output = client.RedactText(result.Output)
	return result
}

// mcpContent is the wire shape an MCP tool emits for a single content
// part. Image parts come with `data` (base64) + `mimeType`; text parts
// come with `text`. Unknown variants are forwarded as JSON to Output
// so a future content type doesn't silently disappear.
type mcpContent struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	Data     string `json:"data,omitempty"`
	MimeType string `json:"mimeType,omitempty"`
}

type mcpEnvelope struct {
	Content []mcpContent `json:"content"`
	IsError bool         `json:"isError"`
}

// parseMCPResponse extracts text + image parts from the standard
// MCP envelope. Returns (result, true) on a recognisable envelope
// (even when content is empty); (nil, false) for non-envelope
// shapes so the caller falls back to the legacy raw-dump path.
//
// Multiple text parts are joined with blank lines so the
// human-readable summary stays grep-able. Multiple image parts are
// kept distinct so the model can act on each as its own block.
func parseMCPResponse(raw []byte) (*tools.Result, bool) {
	var env mcpEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, false
	}
	if env.Content == nil {
		// Envelope key missing entirely — likely some other shape.
		// Distinguish from "valid envelope with empty content" by
		// re-parsing into a map and looking for the literal key.
		var probe map[string]json.RawMessage
		if json.Unmarshal(raw, &probe) != nil {
			return nil, false
		}
		if _, present := probe["content"]; !present {
			return nil, false
		}
	}
	var textParts []string
	var images []pubtool.ImageAttachment
	for _, c := range env.Content {
		switch c.Type {
		case "text":
			if c.Text != "" {
				textParts = append(textParts, c.Text)
			}
		case "image":
			if c.Data == "" {
				continue
			}
			mt, ok := normalizeMCPImageMIME(c.MimeType)
			if !ok {
				// MIME is model-visible metadata and later becomes part of a data:
				// URL. Never forward arbitrary or credential-bearing values. Keep
				// the base64 bytes out of text and omit only this unsafe attachment.
				textParts = append(textParts, "[MCP image omitted: unsupported image MIME type]")
				continue
			}
			images = append(images, pubtool.ImageAttachment{
				MediaType: mt, Data: c.Data,
			})
		default:
			// Forward unknown variant as JSON so we don't lose
			// future content types silently.
			b, _ := json.Marshal(c)
			textParts = append(textParts, string(b))
		}
	}
	out := strings.Join(textParts, "\n\n")
	return &tools.Result{
		Output:  out,
		IsError: env.IsError,
		Images:  images,
	}, true
}

func normalizeMCPImageMIME(raw string) (string, bool) {
	if strings.TrimSpace(raw) == "" {
		return "image/png", true // MCP-compatible default for missing mimeType
	}
	mediaType, params, err := mime.ParseMediaType(raw)
	if err != nil || len(params) != 0 {
		return "", false
	}
	mediaType = strings.ToLower(mediaType)
	switch mediaType {
	case "image/png", "image/jpeg", "image/gif", "image/webp":
		return mediaType, true
	default:
		return "", false
	}
}

// NewServer connects to a stdio MCP server (subprocess) and returns a
// Server with its tools registered as Metis tools.
func NewServer(ctx context.Context, name, command string, args ...string) (*Server, error) {
	return NewServerWithEnv(ctx, name, command, nil, args...)
}

// NewServerWithEnv is the env-aware variant. `extraEnv` (KEY=VAL strings)
// augments the MCP client's sanitized launch environment so explicitly
// configured values such as `FIRECRAWL_API_KEY = "fc-..."` from mcp.toml
// reach this server without exposing unrelated METIS process variables.
func NewServerWithEnv(ctx context.Context, name, command string, extraEnv []string, args ...string) (*Server, error) {
	return NewServerWithEnvAndDir(ctx, name, command, extraEnv, "", args...)
}

// NewServerWithEnvAndDir preserves a plugin package's working-directory
// contract while keeping the stdio process isolated from the METIS process.
func NewServerWithEnvAndDir(ctx context.Context, name, command string, extraEnv []string, workingDir string, args ...string) (*Server, error) {
	return NewServerWithEnvAndDirAndSandbox(ctx, name, command, extraEnv, workingDir, nil, args...)
}

// NewServerWithEnvAndDirAndSandbox launches a stdio MCP server through the
// runtime-owned process sandbox. The manager is shared infrastructure and is
// not closed by Server.Close.
func NewServerWithEnvAndDirAndSandbox(ctx context.Context, name, command string, extraEnv []string, workingDir string, manager *sandbox.Manager, args ...string) (*Server, error) {
	return NewServerWithEnvAndDirAndSandboxProfile(ctx, name, command, extraEnv, workingDir, manager, mcp.StdioSandboxProfileGeneric, args...)
}

// NewServerWithEnvAndDirAndSandboxProfile carries a host-selected stdio
// capability profile through the eager server wrapper. Generic callers remain
// on the least-privilege profile through NewServerWithEnvAndDirAndSandbox.
func NewServerWithEnvAndDirAndSandboxProfile(ctx context.Context, name, command string, extraEnv []string, workingDir string, manager *sandbox.Manager, profile mcp.StdioSandboxProfile, args ...string) (*Server, error) {
	client, err := mcp.NewStdioClientWithEnvAndDirAndSandboxProfile(ctx, command, extraEnv, workingDir, manager, profile, args...)
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
//	              from ~/.metis/mcp-cache/<name>.json via mcp.LoadCache).
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
	if s.closed {
		s.mu.RUnlock()
		return errMCPServerClosed
	}
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
		spawnCtx, cancel := context.WithCancel(ctx)
		s.mu.Lock()
		if s.closed {
			s.spawnErr = errMCPServerClosed
			s.mu.Unlock()
			cancel()
			return
		}
		s.spawnCancel = cancel
		s.mu.Unlock()

		client, err := s.spawn(spawnCtx)
		cancel()
		var closeClient *mcp.Client
		s.mu.Lock()
		s.spawnCancel = nil
		switch {
		case s.closed:
			s.spawnErr = errMCPServerClosed
			closeClient = client
		case err != nil:
			s.spawnErr = err
		default:
			s.client = client
		}
		s.mu.Unlock()
		if closeClient != nil {
			_ = closeClient.Close()
		}
	})
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		return errMCPServerClosed
	}
	return s.spawnErr
}

// clientForCall returns a stable, non-nil client reference for one operation.
// Close deliberately clears s.client under the same mutex, so callers must not
// split ensureClient and the client snapshot into separate unguarded steps: a
// concurrent Close could otherwise win between them and turn the subsequent
// method call into a nil-pointer panic.
//
// The returned Client remains a valid Go object even if Close starts after the
// snapshot. Client.Close is concurrency-safe and cancels in-flight transport
// work, so the operation then returns a normal transport-closed error.
func (s *Server) clientForCall(ctx context.Context) (*mcp.Client, error) {
	if err := s.ensureClient(ctx); err != nil {
		return nil, err
	}
	return s.clientSnapshot()
}

func (s *Server) clientSnapshot() (*mcp.Client, error) {
	s.mu.RLock()
	c := s.client
	closed := s.closed
	s.mu.RUnlock()
	if closed || c == nil {
		return nil, errMCPServerClosed
	}
	return c, nil
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
	if s == nil {
		return nil
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	c := s.client
	s.client = nil
	cancel := s.spawnCancel
	s.spawnCancel = nil
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
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
	c, err := s.clientForCall(ctx)
	if err != nil {
		return nil, err
	}
	return c.GetPrompt(ctx, name, args)
}

// ListResources surfaces the underlying client's resources/list. Servers
// without the capability (or a not-yet-spawned lazy server) return
// (nil, nil) so callers can treat "no resources" uniformly.
func (s *Server) ListResources(ctx context.Context) ([]mcp.Resource, error) {
	s.mu.RLock()
	c := s.client
	s.mu.RUnlock()
	if c == nil {
		return nil, nil
	}
	return c.ListResources(ctx)
}

// ReadResource fetches one resource by the opaque handle returned from
// ListResources. Forces a lazy server to spawn first, since reading a resource
// is an explicit model action.
func (s *Server) ReadResource(ctx context.Context, uri string) (*mcp.ReadResourceResult, error) {
	c, err := s.clientForCall(ctx)
	if err != nil {
		return nil, err
	}
	return c.ReadResource(ctx, uri)
}

// friendlyMCPError wraps raw transport errors with actionable guidance
// for the model. Without this wrapper a dead MCP subprocess (stdio
// child crashed mid-session) surfaces as the cryptic
//
//	mcp stdio write: write |1: broken pipe
//
// to the model, which has no idea what to do — session 41040bea
// (2026-05-26) shows the model burning 20+ turns retrying cu tool
// calls one by one as the server stayed dead.
//
// The friendly text:
//   - tells the model the subprocess is dead, not that the call is
//     malformed (so it doesn't keep retrying with permutations of args)
//   - names the server so the model can disambiguate when multiple are
//     loaded
//   - suggests `/cu disable && /cu enable` (or the generic /mcp restart
//     for non-cu servers) so the user can recover without leaving the
//     session
//   - keeps the underlying error in parentheses so devs / debug.log
//     readers still have the root cause
//
// Non-transport errors (real tool errors, schema mismatches, etc.)
// pass through unchanged — the model can act on those normally.
func friendlyMCPError(serverName string, err error) string {
	msg := err.Error()
	transportNeedles := []string{
		"broken pipe",
		"write |1:",
		"transport closed",
		"connection refused",
		"connection reset",
		"use of closed network connection",
		"EOF",
		"file already closed",
	}
	for _, needle := range transportNeedles {
		if strings.Contains(msg, needle) {
			restart := fmt.Sprintf("/mcp restart %s", serverName)
			if serverName == "computer-use" {
				restart = "/cu disable then /cu enable"
			}
			return fmt.Sprintf(
				"MCP server %q is no longer reachable — the subprocess "+
					"appears to have crashed. Ask the user to run `%s` "+
					"to relaunch it. Do not retry the same call until "+
					"the server is back; use a Bash fallback if the "+
					"task is time-sensitive. (raw: %s)",
				serverName, restart, msg,
			)
		}
	}
	return msg
}
