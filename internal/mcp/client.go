// Package mcp implements a client for the Model Context Protocol (MCP).
// Supports both stdio (local subprocess) and HTTP+SSE (remote server) transports.
package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// ErrTransportClosed is returned to pending senders when the read loop
// exits unexpectedly (subprocess crash, EOF, etc).
var ErrTransportClosed = errors.New("mcp: transport closed")

// Three-layer timeout model — same shape as Claude Code's
// services/mcp/client.ts uses internally:
//
//   - Connect (default 30 s, env MCP_CONNECT_TIMEOUT) — handshake +
//     initial tools/list. Without this a hung server keeps the user
//     staring at "starting…" until they Ctrl+C.
//   - Request (default 60 s, env MCP_REQUEST_TIMEOUT) — bookkeeping
//     RPCs after handshake (re-listing tools / etc).
//   - Tool (default 27.8 h ~ effectively infinite, env MCP_TOOL_TIMEOUT)
//     — `tools/call`. Long because some tools (browser sessions,
//     long-running data jobs) legitimately run for hours; Claude Code
//     uses 100,000,000 ms for the same reason.
//
// All env vars accept Go duration syntax (e.g. "45s", "5m", "1h"); a
// bad/empty value falls back to the default rather than panicking.
const (
	// 30s budget. Tried 10s on 2026-05-18 morning to fail-fast on
	// slow `npx` cold starts; reverted same day because real-world
	// stdio servers (firecrawl-mcp + playwright-mcp under user's
	// config) consistently took >10s for npm boot + browser-binary
	// check even on warm cache. The 10s value made every prompt
	// re-spawn → re-timeout, spamming "MCP handshake: context
	// deadline exceeded" into the chat after each user turn (image
	// #7 / user report 2026-05-18 noon). 30s is the safe default;
	// raise via MCP_CONNECT_TIMEOUT=Xs for truly cold caches.
	defaultConnectTimeout = 30 * time.Second
	defaultRequestTimeout = 60 * time.Second
	defaultToolTimeout    = 100_000 * time.Second // ~27.8 h
)

// ConnectTimeout returns the per-server connect+handshake budget.
func ConnectTimeout() time.Duration {
	return durationFromEnv("MCP_CONNECT_TIMEOUT", defaultConnectTimeout)
}

// RequestTimeout returns the per-RPC budget for non-tool calls
// (tools/list, ping, etc).
func RequestTimeout() time.Duration {
	return durationFromEnv("MCP_REQUEST_TIMEOUT", defaultRequestTimeout)
}

// ToolTimeout returns the per-tools/call budget.
func ToolTimeout() time.Duration {
	return durationFromEnv("MCP_TOOL_TIMEOUT", defaultToolTimeout)
}

func durationFromEnv(key string, fallback time.Duration) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		return fallback
	}
	return d
}

// Transport is the underlying communication mechanism.
type Transport interface {
	Close() error
}

// MaxStderrBytes caps how much stderr we keep accumulating from an MCP
// subprocess. Without a cap, a server stuck in a logspam loop (or a
// poorly-configured tool dumping a download progress bar) would grow
// the buffer unbounded — Claude Code hit this in production and
// settled on 64 MiB as the trade-off (client.ts:982). Anything past
// the cap is silently discarded; the transport keeps draining the
// pipe so the kernel's pipe buffer doesn't deadlock the subprocess.
const MaxStderrBytes = 64 * 1024 * 1024

// StdioTransport launches a local MCP server and communicates over stdin/stdout.
type StdioTransport struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout io.ReadCloser
	// stderrBuf is the bounded ring of stderr bytes the subprocess
	// emitted. We keep at most MaxStderrBytes — used to surface
	// real diagnostic output when a handshake / tool call fails
	// (without it, the user just sees "EOF" with no context).
	stderrBuf *boundedBuffer
	stderrWG  sync.WaitGroup
}

// NewStdioTransport starts an MCP server process and returns a transport over its stdio.
// Pipes are released on every error path so a failed Start doesn't leak fds.
func NewStdioTransport(ctx context.Context, command string, args ...string) (*StdioTransport, error) {
	return NewStdioTransportWithEnv(ctx, command, nil, args...)
}

// NewStdioTransportWithEnv is the env-aware variant. `extraEnv` entries
// are appended onto os.Environ() so the spawned subprocess inherits the
// metis process env plus the caller-supplied KEY=VAL overrides. nil/empty
// `extraEnv` matches NewStdioTransport's behavior exactly.
func NewStdioTransportWithEnv(ctx context.Context, command string, extraEnv []string, args ...string) (*StdioTransport, error) {
	cmd := exec.CommandContext(ctx, command, args...)
	if len(extraEnv) > 0 {
		cmd.Env = append(os.Environ(), extraEnv...)
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		stdin.Close()
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		stdin.Close()
		stdout.Close()
		return nil, fmt.Errorf("stderr pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		stdin.Close()
		stdout.Close()
		stderr.Close()
		return nil, fmt.Errorf("start %s: %w", command, err)
	}
	t := &StdioTransport{
		cmd: cmd, stdin: stdin, stdout: stdout,
		stderrBuf: newBoundedBuffer(MaxStderrBytes),
	}
	// Drain stderr in a goroutine so the kernel pipe buffer never
	// fills (a full pipe would block the subprocess's next write,
	// freezing the whole server). The bounded writer silently
	// drops bytes past the cap.
	t.stderrWG.Add(1)
	go func() {
		defer t.stderrWG.Done()
		_, _ = io.Copy(t.stderrBuf, stderr)
	}()
	return t, nil
}

func (t *StdioTransport) Close() error {
	t.stdin.Close()
	t.stdout.Close()
	t.cmd.Wait()
	t.stderrWG.Wait()
	return nil
}

// Stderr returns the captured (truncated) stderr output. Callers use
// this to attach real diagnostic context to handshake / RPC errors —
// a failed `npx some-mcp` is much easier to debug when "command not
// found: some-mcp" surfaces alongside the EOF error than when it
// disappeared into a black hole.
func (t *StdioTransport) Stderr() string {
	if t.stderrBuf == nil {
		return ""
	}
	return t.stderrBuf.String()
}

// boundedBuffer is a ring-style cap on accumulated bytes. Writes past
// `cap` are dropped silently and `truncated` records how many bytes
// were lost so the rendered output can show a "…(N bytes elided)"
// suffix.
type boundedBuffer struct {
	mu        sync.Mutex
	buf       []byte
	cap       int
	truncated int
}

func newBoundedBuffer(cap int) *boundedBuffer {
	return &boundedBuffer{cap: cap}
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	room := b.cap - len(b.buf)
	if room <= 0 {
		b.truncated += len(p)
		return len(p), nil // pretend success so io.Copy keeps draining
	}
	if len(p) > room {
		b.buf = append(b.buf, p[:room]...)
		b.truncated += len(p) - room
		return len(p), nil
	}
	b.buf = append(b.buf, p...)
	return len(p), nil
}

func (b *boundedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.truncated == 0 {
		return string(b.buf)
	}
	return fmt.Sprintf("%s\n…(%d bytes elided after stderr cap)\n", string(b.buf), b.truncated)
}

// JSONRPCRequest is a JSON-RPC 2.0 request.
type JSONRPCRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
	ID      interface{}     `json:"id,omitempty"`
}

// JSONRPCResponse is a JSON-RPC 2.0 response.
type JSONRPCResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *JSONRPCError   `json:"error,omitempty"`
	ID      interface{}     `json:"id,omitempty"`
}

// JSONRPCError is a JSON-RPC 2.0 error.
type JSONRPCError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

// Notification is an incoming JSON-RPC notification (no id).
type Notification struct {
	Method string          `json:"method"`
	Params json.RawMessage `json:"params,omitempty"`
}

// Tool describes an MCP tool available on the server.
type Tool struct {
	Name        string                 `json:"name"`
	Description string                 `json:"description"`
	InputSchema map[string]interface{} `json:"inputSchema"`
}

// ListToolsResult is the result of tools/list.
type ListToolsResult struct {
	Tools []Tool `json:"tools"`
}

// Client is an MCP client. It maintains a connection to one MCP server.
type Client struct {
	transport     Transport
	mu            sync.RWMutex
	pending       map[string]chan *JSONRPCResponse // request id → response chan
	notifications chan Notification
	ctx           context.Context
	cancel        context.CancelFunc

	// idSeq generates monotonically increasing request ids. Using a counter
	// instead of time.UnixNano() avoids collisions when two send() calls
	// land in the same nanosecond.
	idSeq atomic.Uint64
}

// NewClient creates an MCP client using the given transport.
func NewClient(ctx context.Context, transport Transport) *Client {
	ctx, cancel := context.WithCancel(ctx)
	return &Client{
		transport:     transport,
		pending:       make(map[string]chan *JSONRPCResponse),
		notifications: make(chan Notification, 50),
		ctx:           ctx,
		cancel:        cancel,
	}
}

// NewStdioClient starts an MCP server subprocess and connects to it.
//
// Handshake (the initial tools/list) gets ConnectTimeout regardless of
// the caller's ctx deadline — this is a tighter budget than the post-
// connect RequestTimeout. Without an explicit short clock here, a
// hanging server keeps `/mcp start` blocked the full RequestTimeout
// or longer (cmd_cu's caller-set 30 s used to be the only stop-gap).
func NewStdioClient(ctx context.Context, command string, args ...string) (*Client, error) {
	return NewStdioClientWithEnv(ctx, command, nil, args...)
}

// NewStdioClientWithEnv is the env-aware variant of NewStdioClient. The
// `extraEnv` entries (KEY=VAL strings) are appended onto os.Environ()
// for the spawned subprocess.
func NewStdioClientWithEnv(ctx context.Context, command string, extraEnv []string, args ...string) (*Client, error) {
	transport, err := NewStdioTransportWithEnv(ctx, command, extraEnv, args...)
	if err != nil {
		return nil, err
	}
	c := NewClient(ctx, transport)
	go c.readLoop()
	handshakeCtx, cancel := context.WithTimeout(ctx, ConnectTimeout())
	defer cancel()
	if err := c.initialize(handshakeCtx); err != nil {
		c.Close()
		if stderr := transport.Stderr(); stderr != "" {
			return nil, fmt.Errorf("MCP initialize: %w\nserver stderr:\n%s", err, stderr)
		}
		return nil, fmt.Errorf("MCP initialize: %w", err)
	}
	if _, err := c.ListTools(handshakeCtx); err != nil {
		c.Close()
		// Surface stderr from the subprocess so the user sees the
		// real reason (not just "EOF" / "timeout"). Bounded by
		// MaxStderrBytes — see boundedBuffer in NewStdioTransport.
		if stderr := transport.Stderr(); stderr != "" {
			return nil, fmt.Errorf("MCP handshake: %w\nserver stderr:\n%s", err, stderr)
		}
		return nil, fmt.Errorf("MCP handshake: %w", err)
	}
	return c, nil
}

// NewHTTPClient connects to a remote MCP server using the modern
// "Streamable HTTP" transport. The client POSTs JSON-RPC requests to
// `endpoint`; the server responds either with a single JSON body
// (`Content-Type: application/json`) or with an SSE stream
// (`Content-Type: text/event-stream`) carrying one or more responses.
// In addition we open a long-lived GET on the same endpoint with
// `Accept: text/event-stream` so server-initiated notifications
// (e.g. tools/list_changed) reach `c.notifications`.
//
// optHeaders lets callers attach auth (`Authorization: Bearer …`) or
// API-key headers — passed straight through on every request.
func NewHTTPClient(ctx context.Context, endpoint string, optHeaders ...map[string]string) (*Client, error) {
	var headers map[string]string
	if len(optHeaders) > 0 {
		headers = optHeaders[0]
	}
	transport := &HTTPTransport{
		endpoint: endpoint,
		client:   &http.Client{},
		headers:  headers,
	}
	c := NewClient(ctx, transport)
	go c.httpNotificationLoop()
	handshakeCtx, cancel := context.WithTimeout(ctx, ConnectTimeout())
	defer cancel()
	if err := c.initialize(handshakeCtx); err != nil {
		c.Close()
		return nil, fmt.Errorf("MCP HTTP initialize: %w", err)
	}
	if _, err := c.ListTools(handshakeCtx); err != nil {
		c.Close()
		return nil, fmt.Errorf("MCP HTTP handshake: %w", err)
	}
	return c, nil
}

// Close terminates the MCP server connection.
func (c *Client) Close() error {
	c.cancel()
	return c.transport.Close()
}

// send issues a JSON-RPC request and waits for its response.
//
// Three things this guards against (each one was a real bug):
//  1. id collision when two callers hit the same nanosecond — we use a
//     monotonic counter, not time.UnixNano().
//  2. stdin write failure being swallowed — if the subprocess died, we
//     surface the write error immediately instead of waiting for ctx.
//  3. pending[id] leaking when ctx is cancelled or HTTP returns directly
//     — every exit path through this function deletes the entry.
func (c *Client) send(ctx context.Context, method string, params interface{}) (*JSONRPCResponse, error) {
	// Omit params entirely when nil rather than sending `"params":null`.
	// Spec-strict servers (the official @modelcontextprotocol/sdk) validate
	// the JSON-RPC envelope with a schema where params is optional-not-
	// nullable, and reject a literal null with -32700. Leaving raw empty
	// lets the omitempty tag drop the field.
	var raw json.RawMessage
	if params != nil {
		var err error
		raw, err = json.Marshal(params)
		if err != nil {
			return nil, err
		}
	}
	id := fmt.Sprintf("%d", c.idSeq.Add(1))
	ch := make(chan *JSONRPCResponse, 1)
	c.mu.Lock()
	c.pending[id] = ch
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
	}()

	req := JSONRPCRequest{JSONRPC: "2.0", Method: method, Params: raw, ID: id}
	data, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}

	c.mu.RLock()
	transport := c.transport
	c.mu.RUnlock()

	switch t := transport.(type) {
	case *StdioTransport:
		if _, err := t.stdin.Write(append(data, '\n')); err != nil {
			return nil, fmt.Errorf("mcp stdio write: %w", err)
		}
	case *HTTPTransport:
		// Streamable HTTP: POST the request, then look at
		// Content-Type to choose the response shape.
		//   - application/json : single response, decode + return
		//   - text/event-stream: SSE frames; the response we want
		//     comes through as a `data:` line carrying the JSON-RPC
		//     reply matching our id. The server may send notifications
		//     in the same stream — those get routed via dispatchJSONRPC
		//     into c.notifications.
		req, rerr := http.NewRequestWithContext(ctx, "POST", t.endpoint, strings.NewReader(string(data)))
		if rerr != nil {
			return nil, rerr
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		for k, v := range t.headers {
			req.Header.Set(k, v)
		}
		resp, err := t.client.Do(req)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		ct := resp.Header.Get("Content-Type")
		if strings.HasPrefix(ct, "text/event-stream") {
			// SSE response — feed lines through the same dispatcher
			// path; the matching response will arrive on `ch` because
			// we registered into c.pending above.
			go c.parseSSE(resp.Body)
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-c.ctx.Done():
				return nil, ErrTransportClosed
			case rpcResp := <-ch:
				return rpcResp, nil
			}
		}
		var rpcResp JSONRPCResponse
		if err := json.NewDecoder(resp.Body).Decode(&rpcResp); err != nil {
			return nil, err
		}
		return &rpcResp, nil
	}

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-c.ctx.Done():
		return nil, ErrTransportClosed
	case resp := <-ch:
		return resp, nil
	}
}

// MCPProtocolVersion is the MCP version metis advertises in the
// initialize handshake. 2024-11-05 is the Streamable-HTTP baseline the
// official SDK servers accept.
const MCPProtocolVersion = "2024-11-05"

// initialize performs the MCP lifecycle handshake the spec requires
// before any other request: an `initialize` request followed by an
// `notifications/initialized` notification. metis historically skipped
// this and used tools/list as a de-facto handshake — lenient servers
// tolerated it, but spec-strict ones (the official @modelcontextprotocol
// SDK) reject the out-of-order tools/list. Called once by the client
// constructors before the initial ListTools.
func (c *Client) initialize(ctx context.Context) error {
	params := map[string]any{
		"protocolVersion": MCPProtocolVersion,
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "metis", "version": "0.1"},
	}
	resp, err := c.send(ctx, "initialize", params)
	if err != nil {
		return err
	}
	if resp.Error != nil {
		return fmt.Errorf("MCP error %d: %s", resp.Error.Code, resp.Error.Message)
	}
	// Best-effort initialized notification; servers don't reply to it.
	return c.sendNotification(ctx, "notifications/initialized", nil)
}

// sendNotification writes a fire-and-forget JSON-RPC notification (no id,
// no response awaited). Used for notifications/initialized.
func (c *Client) sendNotification(ctx context.Context, method string, params interface{}) error {
	var raw json.RawMessage
	if params != nil {
		var err error
		raw, err = json.Marshal(params)
		if err != nil {
			return err
		}
	}
	msg := Notification{Method: method, Params: raw}
	envelope := struct {
		JSONRPC string `json:"jsonrpc"`
		Notification
	}{JSONRPC: "2.0", Notification: msg}
	data, err := json.Marshal(envelope)
	if err != nil {
		return err
	}

	c.mu.RLock()
	transport := c.transport
	c.mu.RUnlock()

	switch t := transport.(type) {
	case *StdioTransport:
		_, err := t.stdin.Write(append(data, '\n'))
		return err
	case *HTTPTransport:
		req, rerr := http.NewRequestWithContext(ctx, "POST", t.endpoint, strings.NewReader(string(data)))
		if rerr != nil {
			return rerr
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		for k, v := range t.headers {
			req.Header.Set(k, v)
		}
		resp, err := t.client.Do(req)
		if err != nil {
			return err
		}
		// Notifications get 202 Accepted with no body; drain + close.
		_ = resp.Body.Close()
		return nil
	}
	return nil
}

// readLoop pumps JSON-RPC messages from the stdio transport. On EOF or any
// decode error it cancels c.ctx so blocked send() callers wake up with
// ErrTransportClosed instead of waiting on their per-call ctx to expire.
func (c *Client) readLoop() {
	t, ok := c.transport.(*StdioTransport)
	if !ok {
		return
	}
	defer c.cancel() // wake any sender blocked on <-c.ctx.Done()

	r := bufio.NewReader(t.stdout)
	dec := json.NewDecoder(r)
	for {
		select {
		case <-c.ctx.Done():
			return
		default:
		}
		var msg json.RawMessage
		if err := dec.Decode(&msg); err != nil {
			return
		}
		// Try response (has id)
		var resp JSONRPCResponse
		if err := json.Unmarshal(msg, &resp); err == nil && resp.ID != nil {
			id := fmt.Sprintf("%v", resp.ID)
			c.mu.Lock()
			ch, ok := c.pending[id]
			c.mu.Unlock()
			if ok {
				select {
				case ch <- &resp:
				default:
				}
			}
			continue
		}
		// Try notification
		var notif Notification
		if err := json.Unmarshal(msg, &notif); err == nil && notif.Method != "" {
			select {
			case c.notifications <- notif:
			default:
			}
		}
	}
}

// ListTools returns all tools available on the MCP server.
//
// As with CallTool, a deadline-less ctx gets RequestTimeout() applied
// so a wedged server doesn't hang `/mcp start` indefinitely.
func (c *Client) ListTools(ctx context.Context) ([]Tool, error) {
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, RequestTimeout())
		defer cancel()
	}
	resp, err := c.send(ctx, "tools/list", nil)
	if err != nil {
		return nil, err
	}
	if resp.Error != nil {
		return nil, fmt.Errorf("MCP error %d: %s", resp.Error.Code, resp.Error.Message)
	}
	var result ListToolsResult
	if err := json.Unmarshal(resp.Result, &result); err != nil {
		return nil, err
	}
	return result.Tools, nil
}

// Prompt is an MCP-server-advertised prompt template the user can
// invoke through the chat surface (`/mcp__<server>__<prompt>`).
// Mirrors Anthropic's prompts/list response shape.
type Prompt struct {
	Name        string           `json:"name"`
	Description string           `json:"description"`
	Arguments   []PromptArgument `json:"arguments,omitempty"`
}

// PromptArgument describes one templated input the server expects when
// `prompts/get` is called for this prompt. Required arguments without
// a value cause a slash command to refuse with a usage hint instead of
// firing an empty server call.
type PromptArgument struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Required    bool   `json:"required,omitempty"`
}

// ListPromptsResult mirrors `prompts/list` JSON response.
type ListPromptsResult struct {
	Prompts []Prompt `json:"prompts"`
}

// PromptMessage is one element of a server-rendered prompt body.
// `Role` is "user" or "assistant"; `Content` is a list of typed parts
// (currently always {type:"text", text:"..."}). Mirrors MCP spec.
type PromptMessage struct {
	Role    string             `json:"role"`
	Content []PromptContentRaw `json:"content"`
}

// PromptContentRaw stays raw so clients that only need the text body
// don't pay JSON-decode cost on schema fields they ignore. Helpers
// (PromptText) extract what we need.
type PromptContentRaw struct {
	Type string          `json:"type"`
	Text string          `json:"text,omitempty"`
	Data json.RawMessage `json:"-"`
}

// GetPromptResult is the `prompts/get` response — typically one user
// message whose Content[0].Text is what the chat surface should send.
type GetPromptResult struct {
	Description string          `json:"description,omitempty"`
	Messages    []PromptMessage `json:"messages"`
}

// ListPrompts returns the prompts a server advertises. Servers without
// the prompts capability return method-not-found; we surface that as
// (nil, nil) so callers (the runtime registrar at startup) can treat
// "no prompts" identically to "this server doesn't support them".
func (c *Client) ListPrompts(ctx context.Context) ([]Prompt, error) {
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, RequestTimeout())
		defer cancel()
	}
	resp, err := c.send(ctx, "prompts/list", nil)
	if err != nil {
		return nil, err
	}
	if resp.Error != nil {
		// -32601 == method not found. Normalize to "no prompts".
		if resp.Error.Code == -32601 {
			return nil, nil
		}
		return nil, fmt.Errorf("MCP error %d: %s", resp.Error.Code, resp.Error.Message)
	}
	var result ListPromptsResult
	if err := json.Unmarshal(resp.Result, &result); err != nil {
		return nil, err
	}
	return result.Prompts, nil
}

// GetPrompt resolves a prompt template against `args`. Returns the
// server-rendered messages — caller picks the body to send.
func (c *Client) GetPrompt(ctx context.Context, name string, args map[string]string) (*GetPromptResult, error) {
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, RequestTimeout())
		defer cancel()
	}
	params := map[string]any{"name": name}
	if len(args) > 0 {
		params["arguments"] = args
	}
	resp, err := c.send(ctx, "prompts/get", params)
	if err != nil {
		return nil, err
	}
	if resp.Error != nil {
		return nil, fmt.Errorf("MCP error %d: %s", resp.Error.Code, resp.Error.Message)
	}
	var result GetPromptResult
	if err := json.Unmarshal(resp.Result, &result); err != nil {
		return nil, err
	}
	return &result, nil
}

// CallTool invokes an MCP tool with the given arguments.
//
// If the caller's ctx has no deadline set, ToolTimeout() is applied so
// a misbehaving tool can't hang the agent loop forever. A caller that
// wants the long path explicitly (interactive flows, debug sessions)
// should set its own deadline before calling.
func (c *Client) CallTool(ctx context.Context, name string, args map[string]interface{}) (json.RawMessage, error) {
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, ToolTimeout())
		defer cancel()
	}
	params := map[string]interface{}{
		"name":      name,
		"arguments": args,
	}
	resp, err := c.send(ctx, "tools/call", params)
	if err != nil {
		return nil, err
	}
	if resp.Error != nil {
		return nil, fmt.Errorf("MCP error %d: %s", resp.Error.Code, resp.Error.Message)
	}
	return resp.Result, nil
}

// HTTPTransport implements the MCP Streamable HTTP transport. Single
// endpoint for both request-response (POST) and server-initiated
// notifications (long-poll GET with Accept: text/event-stream).
type HTTPTransport struct {
	endpoint string
	client   *http.Client
	headers  map[string]string

	// cancelGET is set by httpNotificationLoop when it opens the GET
	// SSE channel. Calling it (via Close → cancel ctx) closes the
	// notification stream cleanly.
	cancelGET context.CancelFunc
}

func (t *HTTPTransport) Close() error {
	if t.cancelGET != nil {
		t.cancelGET()
	}
	return nil
}

// httpNotificationLoop opens a GET on the endpoint and reads SSE
// frames so the server can push notifications. It only runs for
// HTTPTransport-backed clients; stdio clients use readLoop instead.
//
// Failures here are non-fatal — many MCP servers don't support the GET
// notification channel, in which case we log once and move on. The
// request-response path (POST) keeps working either way.
func (c *Client) httpNotificationLoop() {
	ht, ok := c.transport.(*HTTPTransport)
	if !ok {
		return
	}
	getCtx, cancel := context.WithCancel(c.ctx)
	ht.cancelGET = cancel

	req, err := http.NewRequestWithContext(getCtx, "GET", ht.endpoint, nil)
	if err != nil {
		return
	}
	req.Header.Set("Accept", "text/event-stream")
	for k, v := range ht.headers {
		req.Header.Set(k, v)
	}
	resp, err := ht.client.Do(req)
	if err != nil {
		return // server doesn't support GET-SSE — silent skip
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return
	}
	if !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		return
	}
	c.parseSSE(resp.Body)
}

// parseSSE reads `data: <json>` lines and dispatches each into the
// existing pending/notifications channels, same as readLoop for stdio.
func (c *Client) parseSSE(r io.Reader) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" || payload == "[DONE]" {
			continue
		}
		c.dispatchJSONRPC([]byte(payload))
	}
}

// dispatchJSONRPC routes one raw JSON-RPC message into either the
// pending response map (when it has an id) or the notifications
// channel (no id). Shared between stdio and HTTP+SSE paths so the
// per-transport code only needs to forward bytes.
func (c *Client) dispatchJSONRPC(data []byte) {
	var probe struct {
		ID interface{} `json:"id"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		return
	}
	if probe.ID != nil {
		var resp JSONRPCResponse
		if err := json.Unmarshal(data, &resp); err != nil {
			return
		}
		id := fmt.Sprintf("%v", resp.ID)
		c.mu.RLock()
		ch, ok := c.pending[id]
		c.mu.RUnlock()
		if ok {
			ch <- &resp
		}
		return
	}
	var notif Notification
	if err := json.Unmarshal(data, &notif); err == nil && notif.Method != "" {
		select {
		case c.notifications <- notif:
		default:
		}
	}
}
