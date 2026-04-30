// Package acp implements the Agent Client Protocol server.
// Inspired by Hermes' acp_adapter: exposes a Loop as a JSON-RPC 2.0 server
// over stdio or TCP. Clients send `prompt`, get streaming `session_update`
// notifications back, and can `abort` or reply to `permission_request`s.
package acp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"sync"

	"github.com/Ricardo-M-L/metis/internal/agent"
)

// Server runs an ACP endpoint backed by a single agent.Loop.
// Per-connection state (active prompts, pending permission asks) lives on session.
type Server struct {
	Loop *agent.Loop
	Addr string

	ln    net.Listener
	wg    sync.WaitGroup
	mu    sync.Mutex
	done  bool
	conns map[net.Conn]struct{} // active TCP connections to close on shutdown
}

func NewServer(loop *agent.Loop, addr string) *Server {
	return &Server{Loop: loop, Addr: addr, conns: make(map[net.Conn]struct{})}
}

// Listen starts serving. Stdio mode runs in a goroutine and Listen returns
// immediately; call Wait to block. TCP mode also returns immediately and
// Wait blocks until Close is called.
func (s *Server) Listen() error {
	if s.Addr == "stdio" || s.Addr == "" {
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			s.serveConn(os.Stdin, os.Stdout)
		}()
		return nil
	}
	ln, err := net.Listen("tcp", s.Addr)
	if err != nil {
		return err
	}
	s.ln = ln
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			wc := conn
			s.mu.Lock()
			s.conns[wc] = struct{}{}
			s.mu.Unlock()
			s.wg.Add(1)
			go func() {
				defer s.wg.Done()
				defer wc.Close()
				defer func() {
					s.mu.Lock()
					delete(s.conns, wc)
					s.mu.Unlock()
				}()
				s.serveConn(wc, wc)
			}()
		}
	}()
	return nil
}

// Wait blocks until all server goroutines have exited.
func (s *Server) Wait() { s.wg.Wait() }

func (s *Server) Close() error {
	s.mu.Lock()
	if s.done {
		s.mu.Unlock()
		return nil
	}
	s.done = true
	if s.ln != nil {
		s.ln.Close()
	}
	// Force-close active connections so their serveConn goroutines unblock
	// from json.Decode and the wg.Wait below actually returns.
	for c := range s.conns {
		c.Close()
	}
	s.mu.Unlock()
	s.wg.Wait()
	return nil
}

// session is per-connection state. Encoder access is serialized via encMu so
// concurrent prompt handlers don't interleave their JSON output.
type session struct {
	enc    *json.Encoder
	encMu  sync.Mutex
	server *Server

	// Active prompts: stringified request id -> cancelFunc, used by `abort`.
	prompts   map[string]context.CancelFunc
	promptsMu sync.Mutex

	// Pending permission asks: tool_use_id -> reply chan, used by `permission_reply`.
	perms   map[string]chan agent.PermissionDecision
	permsMu sync.Mutex
}

func (sn *session) write(v any) error {
	sn.encMu.Lock()
	defer sn.encMu.Unlock()
	return sn.enc.Encode(v)
}

func (s *Server) serveConn(r io.Reader, w io.Writer) {
	sn := &session{
		enc:     json.NewEncoder(w),
		server:  s,
		prompts: make(map[string]context.CancelFunc),
		perms:   make(map[string]chan agent.PermissionDecision),
	}
	dec := json.NewDecoder(r)
	for {
		var req Request
		if err := dec.Decode(&req); err != nil {
			// Any decode error — EOF, closed connection, or malformed JSON
			// — terminates this connection. The JSON decoder is positioned
			// mid-stream after a parse error and can't reliably continue,
			// and a closed-network error means the client is gone.
			if err != io.EOF {
				sn.write(Response{JSONRPC: "2.0", ID: nil,
					Error: &ResponseError{Code: -32700, Message: err.Error()}})
			}
			return
		}
		s.wg.Add(1)
		go func(req Request) {
			defer s.wg.Done()
			sn.handle(&req)
		}(req)
	}
}

// --- JSON-RPC types ----------------------------------------------------------

type Request struct {
	JSONRPC string                     `json:"jsonrpc"`
	ID      any                        `json:"id"`
	Method  string                     `json:"method"`
	Params  map[string]json.RawMessage `json:"params,omitempty"`
}

type Response struct {
	JSONRPC string         `json:"jsonrpc"`
	ID      any            `json:"id,omitempty"`
	Result  any            `json:"result,omitempty"`
	Error   *ResponseError `json:"error,omitempty"`
}

// Notification is a JSON-RPC 2.0 notification: no ID, mandatory method.
type Notification struct {
	JSONRPC string         `json:"jsonrpc"`
	Method  string         `json:"method"`
	Params  map[string]any `json:"params"`
}

type ResponseError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type PromptParams struct {
	Prompt   string `json:"prompt"`
	PlanMode bool   `json:"plan_mode,omitempty"`
	Model    string `json:"model,omitempty"`
}

type AbortParams struct {
	PromptID string `json:"prompt_id"`
}

type PermissionReplyParams struct {
	ToolUseID string `json:"tool_use_id"`
	Decision  string `json:"decision"` // "allow" | "deny" | "always"
}

// --- Method dispatch ---------------------------------------------------------

func (sn *session) handle(req *Request) {
	ctx := context.Background()
	switch req.Method {
	case "prompt":
		var p PromptParams
		decodeParams(req.Params, &p)
		sn.handlePrompt(ctx, req.ID, p)
	case "abort":
		var p AbortParams
		decodeParams(req.Params, &p)
		sn.handleAbort(req.ID, p)
	case "permission_reply":
		var p PermissionReplyParams
		decodeParams(req.Params, &p)
		sn.handlePermissionReply(req.ID, p)
	case "session.list":
		sn.write(Response{JSONRPC: "2.0", ID: req.ID, Result: map[string]any{"sessions": []any{}}})
	default:
		sn.write(Response{JSONRPC: "2.0", ID: req.ID, Error: &ResponseError{
			Code:    -32601,
			Message: fmt.Sprintf("method not found: %s", req.Method),
		}})
	}
}

func decodeParams(raw map[string]json.RawMessage, out any) {
	if raw == nil {
		return
	}
	b, err := json.Marshal(raw)
	if err != nil {
		return
	}
	_ = json.Unmarshal(b, out)
}

func (sn *session) handlePrompt(ctx context.Context, id any, p PromptParams) {
	if p.Prompt != "" {
		sn.server.Loop.AppendUser(p.Prompt)
	}

	pctx, cancel := context.WithCancel(ctx)
	defer cancel()

	pid := promptIDOf(id)
	if pid != "" {
		sn.promptsMu.Lock()
		sn.prompts[pid] = cancel
		sn.promptsMu.Unlock()
		defer func() {
			sn.promptsMu.Lock()
			delete(sn.prompts, pid)
			sn.promptsMu.Unlock()
		}()
	}

	events := make(chan agent.Event, 64)
	done := make(chan error, 1)
	go func() {
		done <- sn.server.Loop.Run(pctx, events)
		close(events)
	}()

	for ev := range events {
		// Register permission asks so the next permission_reply can route to it.
		if ev.Kind == agent.EventPermissionRequest && ev.ToolUseID != "" {
			sn.permsMu.Lock()
			sn.perms[ev.ToolUseID] = ev.PermissionReply
			sn.permsMu.Unlock()
		}
		sn.write(Notification{JSONRPC: "2.0", Method: "session_update", Params: eventToMap(ev)})
	}

	if err := <-done; err != nil {
		sn.write(Response{JSONRPC: "2.0", ID: id, Error: &ResponseError{Code: -32000, Message: err.Error()}})
		return
	}
	sn.write(Response{JSONRPC: "2.0", ID: id, Result: map[string]any{"done": true}})
}

func (sn *session) handleAbort(id any, p AbortParams) {
	sn.promptsMu.Lock()
	cancel, ok := sn.prompts[p.PromptID]
	sn.promptsMu.Unlock()
	if ok {
		cancel()
		sn.write(Response{JSONRPC: "2.0", ID: id, Result: map[string]any{"aborted": true}})
		return
	}
	sn.write(Response{JSONRPC: "2.0", ID: id, Result: map[string]any{"aborted": false, "reason": "unknown prompt_id"}})
}

func (sn *session) handlePermissionReply(id any, p PermissionReplyParams) {
	sn.permsMu.Lock()
	ch, ok := sn.perms[p.ToolUseID]
	if ok {
		delete(sn.perms, p.ToolUseID)
	}
	sn.permsMu.Unlock()
	if !ok {
		sn.write(Response{JSONRPC: "2.0", ID: id, Error: &ResponseError{Code: -32602, Message: "no pending permission for tool_use_id"}})
		return
	}
	var dec agent.PermissionDecision
	switch p.Decision {
	case "allow":
		dec = agent.PermissionDecisionAllow
	case "deny":
		dec = agent.PermissionDecisionDeny
	case "always":
		dec = agent.PermissionDecisionAlwaysAllow
	default:
		sn.write(Response{JSONRPC: "2.0", ID: id, Error: &ResponseError{Code: -32602, Message: "decision must be allow|deny|always"}})
		return
	}
	select {
	case ch <- dec:
		sn.write(Response{JSONRPC: "2.0", ID: id, Result: map[string]any{"ok": true}})
	default:
		sn.write(Response{JSONRPC: "2.0", ID: id, Error: &ResponseError{Code: -32000, Message: "reply channel full or closed"}})
	}
}

func promptIDOf(id any) string {
	switch v := id.(type) {
	case string:
		return v
	case float64:
		return fmt.Sprintf("%v", int64(v))
	case nil:
		return ""
	default:
		return fmt.Sprintf("%v", v)
	}
}

// --- Event serialization -----------------------------------------------------

// eventKind translates an internal agent.EventKind into the wire string
// the protocol carries. Keep this in sync with internal/agent/event.go —
// E2E test discovered that EventStreamStart/End and the sub-agent /
// rate-limit / channel families were silently mapped to "unknown",
// breaking client-side discriminated-union parsing for any non-text /
// non-tool / non-loop event.
func eventKind(k agent.EventKind) string {
	switch k {
	case agent.EventTextDelta:
		return "text_delta"
	case agent.EventThinkingDelta:
		return "thinking_delta"
	case agent.EventToolStart:
		return "tool_start"
	case agent.EventToolResult:
		return "tool_result"
	case agent.EventPermissionRequest:
		return "permission_request"
	case agent.EventTurnEnd:
		return "turn_end"
	case agent.EventLoopDone:
		return "loop_done"
	case agent.EventError:
		return "error"
	case agent.EventTokens:
		return "tokens"
	case agent.EventInfo:
		return "info"
	case agent.EventPlan:
		return "plan"
	case agent.EventStreamStart:
		return "stream_start"
	case agent.EventStreamEnd:
		return "stream_end"
	case agent.EventSubAgentStart:
		return "subagent_start"
	case agent.EventSubAgentProgress:
		return "subagent_progress"
	case agent.EventSubAgentEnd:
		return "subagent_end"
	case agent.EventContextWarn:
		return "context_warn"
	case agent.EventContextCompacted:
		return "context_compacted"
	case agent.EventRateLimitHit:
		return "rate_limit_hit"
	case agent.EventModelFallback:
		return "model_fallback"
	case agent.EventChannelInbound:
		return "channel_inbound"
	case agent.EventChannelSent:
		return "channel_sent"
	case agent.EventHookFired:
		return "hook_fired"
	}
	return "unknown"
}

func eventToMap(ev agent.Event) map[string]any {
	m := map[string]any{"kind": eventKind(ev.Kind)}
	switch ev.Kind {
	case agent.EventTextDelta, agent.EventThinkingDelta:
		// Both share the TextDelta payload field; clients distinguish
		// by `kind` (text_delta vs thinking_delta) so the same wire
		// shape works for both — IDE chat panels typically dim
		// thinking_delta and bold text_delta.
		m["text_delta"] = ev.TextDelta
	case agent.EventToolStart:
		m["tool_name"] = ev.ToolName
		m["tool_id"] = ev.ToolUseID
		m["tool_input"] = ev.ToolInput
	case agent.EventToolResult:
		m["tool_id"] = ev.ToolUseID
		if ev.ToolResult != nil {
			m["tool_result"] = ev.ToolResult.Output
			m["tool_error"] = ev.ToolResult.IsError
		}
	case agent.EventLoopDone:
		m["stop_reason"] = ev.StopReason
	case agent.EventTokens:
		m["input_tokens"] = ev.InputTokens
		m["output_tokens"] = ev.OutputTokens
	case agent.EventError:
		if ev.Err != nil {
			m["error"] = ev.Err.Error()
		}
	case agent.EventInfo:
		m["info"] = ev.Info
	case agent.EventPermissionRequest:
		m["tool_id"] = ev.ToolUseID
		m["tool_name"] = ev.PermissionTool
		m["tool_input"] = ev.PermissionInput
		m["reason"] = ev.PermissionReason
	}
	return m
}
