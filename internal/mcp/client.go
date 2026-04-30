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
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
)

// ErrTransportClosed is returned to pending senders when the read loop
// exits unexpectedly (subprocess crash, EOF, etc).
var ErrTransportClosed = errors.New("mcp: transport closed")

// Transport is the underlying communication mechanism.
type Transport interface {
	Close() error
}

// StdioTransport launches a local MCP server and communicates over stdin/stdout.
type StdioTransport struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout io.ReadCloser
}

// NewStdioTransport starts an MCP server process and returns a transport over its stdio.
// Pipes are released on every error path so a failed Start doesn't leak fds.
func NewStdioTransport(ctx context.Context, command string, args ...string) (*StdioTransport, error) {
	cmd := exec.CommandContext(ctx, command, args...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		stdin.Close()
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		stdin.Close()
		stdout.Close()
		return nil, fmt.Errorf("start %s: %w", command, err)
	}
	return &StdioTransport{cmd: cmd, stdin: stdin, stdout: stdout}, nil
}

func (t *StdioTransport) Close() error {
	t.stdin.Close()
	t.stdout.Close()
	t.cmd.Wait()
	return nil
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
	Name        string                  `json:"name"`
	Description string                  `json:"description"`
	InputSchema map[string]interface{}   `json:"inputSchema"`
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
func NewStdioClient(ctx context.Context, command string, args ...string) (*Client, error) {
	transport, err := NewStdioTransport(ctx, command, args...)
	if err != nil {
		return nil, err
	}
	c := NewClient(ctx, transport)
	go c.readLoop()
	_, err = c.ListTools(ctx)
	if err != nil {
		c.Close()
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
	if _, err := c.ListTools(ctx); err != nil {
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
//   1. id collision when two callers hit the same nanosecond — we use a
//      monotonic counter, not time.UnixNano().
//   2. stdin write failure being swallowed — if the subprocess died, we
//      surface the write error immediately instead of waiting for ctx.
//   3. pending[id] leaking when ctx is cancelled or HTTP returns directly
//      — every exit path through this function deletes the entry.
func (c *Client) send(ctx context.Context, method string, params interface{}) (*JSONRPCResponse, error) {
	raw, err := json.Marshal(params)
	if err != nil {
		return nil, err
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
func (c *Client) ListTools(ctx context.Context) ([]Tool, error) {
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

// CallTool invokes an MCP tool with the given arguments.
func (c *Client) CallTool(ctx context.Context, name string, args map[string]interface{}) (json.RawMessage, error) {
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
