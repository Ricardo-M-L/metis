// Package client is a thin Go SDK for driving a metis agent out-of-process
// over the ACP (Agent Client Protocol) control plane. It spawns
// `metis acp` and speaks the JSON-RPC-over-stdio protocol, mirroring how
// @anthropic-ai/claude-agent-sdk drives the `claude` binary.
//
// Why out-of-process (vs importing the agent loop in-package): the agent's
// permission gate, sandbox, and session lifecycle stay on the far side of a
// process boundary, so the SDK can't accidentally bypass them; the wire
// contract (this protocol + `metis schema`) is the only coupling, so metis'
// internals refactor freely without breaking SDK consumers; and the same
// protocol backs SDKs in any language. Same trade-off Claude Code makes.
//
// Usage:
//
//	c, err := client.Spawn(ctx, client.WithArgs("--mode", "bypass"))
//	if err != nil { ... }
//	defer c.Close()
//	err = c.Prompt(ctx, "what is 2+2?", func(ev client.Event) {
//		if ev.Kind == "text_delta" { fmt.Print(ev.TextDelta) }
//	})
package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"

)

// Option configures Spawn.
type Option func(*options)

type options struct {
	binary     string
	args       []string
	env        []string
	stderr     io.Writer
	clientName string
}

// WithBinary overrides the metis executable (default: "metis" on PATH).
func WithBinary(path string) Option { return func(o *options) { o.binary = path } }

// WithArgs appends flags passed to `metis acp` (e.g. "--mode", "bypass",
// "--model", "kimi-k2"). Same grammar as `metis chat`.
func WithArgs(args ...string) Option { return func(o *options) { o.args = append(o.args, args...) } }

// WithEnv sets the subprocess environment (default: inherit the parent's).
func WithEnv(env []string) Option { return func(o *options) { o.env = env } }

// WithStderr redirects the agent's stderr (logs/diagnostics). Default os.Stderr.
func WithStderr(w io.Writer) Option { return func(o *options) { o.stderr = w } }

// WithClientName sets the name advertised in the initialize handshake.
func WithClientName(name string) Option { return func(o *options) { o.clientName = name } }

// Client is a live connection to a spawned `metis acp` process.
type Client struct {
	cmd   *exec.Cmd
	stdin io.WriteCloser

	writeMu sync.Mutex // serializes JSON-RPC writes to stdin
	enc     *json.Encoder

	idMu   sync.Mutex
	nextID int

	pendingMu sync.Mutex
	pending   map[int]chan rpcResult

	sinkMu sync.Mutex
	sink   func(Event) // active Prompt's event handler, nil when idle

	closeOnce sync.Once
	closed    chan struct{}

	// Server is the agent's identity from the initialize handshake.
	Server ServerInfo
	// ProtocolVersion is the version the server agreed to speak.
	ProtocolVersion string
}

// Event is one streamed update during a Prompt (an ACP session_update).
// Kind mirrors the wire kinds: "text_delta", "thinking_delta",
// "tool_start", "tool_result", "permission_request", "loop_done",
// "tokens", "error", "info".
type Event struct {
	Kind         string
	TextDelta    string
	ToolName     string
	ToolID       string
	ToolInput    map[string]any
	ToolResult   string
	ToolError    bool
	Reason       string // permission_request: why the gate stopped
	StopReason   string // loop_done
	InputTokens  int
	OutputTokens int
	Info         string
	Err          string
}

type rpcResult struct {
	result json.RawMessage
	err    *rpcError
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// Spawn launches `metis acp`, performs the initialize handshake, and
// returns a ready Client. Close it when done.
func Spawn(ctx context.Context, opts ...Option) (*Client, error) {
	o := options{binary: "metis", stderr: os.Stderr, clientName: "metis-go-sdk"}
	for _, fn := range opts {
		fn(&o)
	}
	cmd := exec.CommandContext(ctx, o.binary, append([]string{"acp"}, o.args...)...)
	if o.env != nil {
		cmd.Env = o.env
	}
	cmd.Stderr = o.stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("spawn %s acp: %w", o.binary, err)
	}
	c := &Client{
		cmd:     cmd,
		stdin:   stdin,
		enc:     json.NewEncoder(stdin),
		pending: map[int]chan rpcResult{},
		closed:  make(chan struct{}),
	}
	go c.readLoop(json.NewDecoder(stdout))

	var res initializeResult
	err = c.call(ctx, "initialize", initializeParams{
		ProtocolVersion: protocolVersion,
		Client:          &clientInfo{Name: o.clientName},
	}, &res)
	if err != nil {
		_ = c.Close()
		return nil, fmt.Errorf("initialize: %w", err)
	}
	c.Server = res.Server
	c.ProtocolVersion = res.ProtocolVersion
	return c, nil
}

// Prompt runs one turn. onEvent (may be nil) is called for each streamed
// event in order; Prompt returns when the turn completes or ctx is
// cancelled. To answer a permission_request mid-turn, call PermissionReply
// from within onEvent (it's fire-and-forget, so it won't deadlock).
func (c *Client) Prompt(ctx context.Context, text string, onEvent func(Event)) error {
	c.sinkMu.Lock()
	c.sink = onEvent
	c.sinkMu.Unlock()
	defer func() {
		c.sinkMu.Lock()
		c.sink = nil
		c.sinkMu.Unlock()
	}()
	return c.call(ctx, "prompt", promptParams{Prompt: text}, nil)
}

// PermissionReply answers a pending permission_request. decision is
// "allow", "deny", or "always". Fire-and-forget: it returns once the reply
// is written, so it's safe to call from inside a Prompt onEvent callback
// (which runs on the read goroutine).
func (c *Client) PermissionReply(toolID, decision string) error {
	return c.send("permission_reply", permissionReplyParams{ToolUseID: toolID, Decision: decision})
}

// Abort cancels the in-flight prompt. promptID is the id Prompt used; pass
// "" to abort the current one when the server tracks a single prompt.
func (c *Client) Abort(promptID string) error {
	return c.send("abort", abortParams{PromptID: promptID})
}

// Close shuts the connection and terminates the subprocess.
func (c *Client) Close() error {
	c.closeOnce.Do(func() {
		_ = c.stdin.Close()
		if c.cmd.Process != nil {
			_ = c.cmd.Process.Kill()
		}
	})
	return c.cmd.Wait()
}

// --- internals ---------------------------------------------------------------

func (c *Client) nextRequestID() int {
	c.idMu.Lock()
	defer c.idMu.Unlock()
	c.nextID++
	return c.nextID
}

type request struct {
	JSONRPC string `json:"jsonrpc"`
	ID      int    `json:"id"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

func (c *Client) writeRequest(req request) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	return c.enc.Encode(req)
}

// call sends a request and blocks for its matching response.
func (c *Client) call(ctx context.Context, method string, params any, out any) error {
	id := c.nextRequestID()
	ch := make(chan rpcResult, 1)
	c.pendingMu.Lock()
	c.pending[id] = ch
	c.pendingMu.Unlock()
	if err := c.writeRequest(request{JSONRPC: "2.0", ID: id, Method: method, Params: params}); err != nil {
		c.pendingMu.Lock()
		delete(c.pending, id)
		c.pendingMu.Unlock()
		return err
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-c.closed:
		return errors.New("metis acp connection closed")
	case r := <-ch:
		if r.err != nil {
			return fmt.Errorf("acp error %d: %s", r.err.Code, r.err.Message)
		}
		if out != nil && len(r.result) > 0 {
			return json.Unmarshal(r.result, out)
		}
		return nil
	}
}

// send writes a request without waiting for its response (fire-and-forget).
// Used for control messages whose effect is server-side (permission_reply,
// abort) so they can be issued from the read goroutine without deadlock.
func (c *Client) send(method string, params any) error {
	return c.writeRequest(request{JSONRPC: "2.0", ID: c.nextRequestID(), Method: method, Params: params})
}

func (c *Client) readLoop(dec *json.Decoder) {
	for {
		var raw struct {
			ID     *int            `json:"id"`
			Method string          `json:"method"`
			Result json.RawMessage `json:"result"`
			Error  *rpcError       `json:"error"`
			Params json.RawMessage `json:"params"`
		}
		if err := dec.Decode(&raw); err != nil {
			c.failAll(err)
			return
		}
		switch {
		case raw.Method == "session_update":
			var m map[string]any
			_ = json.Unmarshal(raw.Params, &m)
			c.sinkMu.Lock()
			sink := c.sink
			c.sinkMu.Unlock()
			if sink != nil {
				sink(eventFromMap(m))
			}
		case raw.ID != nil:
			c.pendingMu.Lock()
			ch := c.pending[*raw.ID]
			delete(c.pending, *raw.ID)
			c.pendingMu.Unlock()
			if ch != nil {
				ch <- rpcResult{result: raw.Result, err: raw.Error}
			}
			// No pending match (e.g. a fire-and-forget reply's response): drop.
		}
	}
}

// failAll closes the connection and unblocks every pending call.
func (c *Client) failAll(err error) {
	c.closeOnce.Do(func() { _ = c.stdin.Close() })
	close(c.closed)
	c.pendingMu.Lock()
	for id, ch := range c.pending {
		ch <- rpcResult{err: &rpcError{Message: err.Error()}}
		delete(c.pending, id)
	}
	c.pendingMu.Unlock()
}

// eventFromMap decodes an ACP session_update payload into an Event.
func eventFromMap(m map[string]any) Event {
	ev := Event{Kind: str(m, "kind")}
	ev.TextDelta = str(m, "text_delta")
	ev.ToolName = str(m, "tool_name")
	ev.ToolID = str(m, "tool_id")
	if ti, ok := m["tool_input"].(map[string]any); ok {
		ev.ToolInput = ti
	}
	ev.ToolResult = str(m, "tool_result")
	ev.ToolError, _ = m["tool_error"].(bool)
	ev.Reason = str(m, "reason")
	ev.StopReason = str(m, "stop_reason")
	ev.Info = str(m, "info")
	ev.Err = str(m, "error")
	ev.InputTokens = num(m, "input_tokens")
	ev.OutputTokens = num(m, "output_tokens")
	return ev
}

func str(m map[string]any, k string) string {
	s, _ := m[k].(string)
	return s
}

func num(m map[string]any, k string) int {
	if f, ok := m[k].(float64); ok {
		return int(f)
	}
	return 0
}
