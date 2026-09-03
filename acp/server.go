// Package acp implements the Agent Client Protocol server.
// Inspired by Hermes' acp_adapter: exposes a Loop as a JSON-RPC 2.0 server
// over stdio or TCP. Clients send `prompt`, get streaming `session_update`
// notifications back, and can `abort` or reply to `permission_request`s.
package acp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/Ricardo-M-L/metis/internal/agent"
	rtpkg "github.com/Ricardo-M-L/metis/internal/runtime"
)

const acpPromptDistillationGrace = 35 * time.Second

// Server runs an ACP endpoint backed by a single agent.Loop.
// Per-connection state (active prompts, pending permission asks) lives on session.
type Server struct {
	Loop      *agent.Loop
	Addr      string
	SessionID string

	ctx    context.Context
	cancel context.CancelFunc

	ln        net.Listener
	wg        sync.WaitGroup
	mu        sync.Mutex
	done      bool
	closeOnce sync.Once
	conns     map[net.Conn]struct{} // active TCP connections to close on shutdown
	stdioIn   io.ReadCloser
	stdioOut  io.Writer
}

func NewServer(loop *agent.Loop, addr string) *Server {
	return NewServerForSession(loop, addr, "")
}

// NewServerForSession binds every prompt run to the runtime session that owns
// this ACP server. Keeping the session explicit avoids attributing concurrent
// or late trace events through ambient process-global selection state.
func NewServerForSession(loop *agent.Loop, addr, sessionID string) *Server {
	return NewServerForSessionContext(context.Background(), loop, addr, sessionID)
}

// NewServerForSessionContext binds the lifetime of every prompt to parent.
// Server.Close also cancels this derived context, so both a command signal and
// an explicit service shutdown terminate providers before Cleanup tears down
// their runtime dependencies.
func NewServerForSessionContext(parent context.Context, loop *agent.Loop, addr, sessionID string) *Server {
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithCancel(parent)
	return &Server{
		Loop: loop, Addr: addr, SessionID: sessionID,
		ctx: ctx, cancel: cancel, conns: make(map[net.Conn]struct{}),
	}
}

// Listen starts serving. Stdio mode runs in a goroutine and Listen returns
// immediately; call Wait to block. TCP mode also returns immediately and
// Wait blocks until Close is called.
func (s *Server) Listen() error {
	if s.Addr == "stdio" || s.Addr == "" {
		s.mu.Lock()
		if s.done {
			s.mu.Unlock()
			return errors.New("ACP server is closed")
		}
		if s.stdioIn == nil {
			s.stdioIn = os.Stdin
		}
		if s.stdioOut == nil {
			s.stdioOut = os.Stdout
		}
		in, out := s.stdioIn, s.stdioOut
		s.wg.Add(1)
		s.mu.Unlock()
		go func() {
			defer s.wg.Done()
			s.serveConn(in, out)
		}()
		return nil
	}
	ln, err := net.Listen("tcp", s.Addr)
	if err != nil {
		return err
	}
	s.mu.Lock()
	if s.done {
		s.mu.Unlock()
		_ = ln.Close()
		return errors.New("ACP server is closed")
	}
	s.ln = ln
	s.wg.Add(1)
	s.mu.Unlock()
	go func() {
		defer s.wg.Done()
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			wc := conn
			s.mu.Lock()
			if s.done {
				s.mu.Unlock()
				_ = wc.Close()
				return
			}
			s.conns[wc] = struct{}{}
			s.wg.Add(1)
			s.mu.Unlock()
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
	s.closeOnce.Do(func() {
		if s.cancel != nil {
			s.cancel()
		}
		s.mu.Lock()
		s.done = true
		if s.ln != nil {
			_ = s.ln.Close()
		}
		// Force-close active connections so their serveConn goroutines unblock
		// from json.Decode and active prompt contexts can observe cancellation.
		for c := range s.conns {
			_ = c.Close()
		}
		// Stdio has no net.Conn to close. Closing the command-owned stdin is
		// what releases a decoder blocked in Read after SIGINT/SIGTERM.
		if s.stdioIn != nil {
			_ = s.stdioIn.Close()
		}
		s.mu.Unlock()
		s.wg.Wait()
	})
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
		s.mu.Lock()
		if s.done {
			s.mu.Unlock()
			return
		}
		s.wg.Add(1)
		s.mu.Unlock()
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
	if sn.server != nil && sn.server.ctx != nil {
		ctx = sn.server.ctx
	}
	switch req.Method {
	case "initialize":
		var p InitializeParams
		decodeParams(req.Params, &p)
		sn.handleInitialize(req.ID, p)
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

// handleInitialize replies to the client's initialization request
// with our protocol version, server info, and per-external-tool
// accept/reject decisions. Mirrors kimi-agent-sdk's
// session.go::Initialize semantics: we accept tools whose names
// don't collide with registered builtins, reject the rest with a
// short reason.
func (sn *session) handleInitialize(id any, p InitializeParams) {
	// Major-version mismatch is a hard reject — clients on a future
	// major can't safely speak to us. Same-major / older-minor is
	// fine; we always advertise our own version in the reply.
	if p.ProtocolVersion != "" {
		want, got := majorOf(ProtocolVersion), majorOf(p.ProtocolVersion)
		if want != "" && got != "" && want != got {
			sn.write(Response{JSONRPC: "2.0", ID: id, Error: &ResponseError{
				Code:    -32000,
				Message: fmt.Sprintf("protocol_version mismatch: server=%s client=%s", ProtocolVersion, p.ProtocolVersion),
			}})
			return
		}
	}

	// Per-external-tool decision pass. Reject names that already
	// exist as built-in metis tools (the server's tools take
	// precedence — clients should pick a different name). Otherwise
	// accept; the actual relay-to-client wiring is a follow-up.
	var decisions []ExternalToolDecision
	if sn.server != nil && sn.server.Loop != nil && sn.server.Loop.Registry != nil {
		for _, t := range p.ExternalTools {
			d := ExternalToolDecision{Name: t.Name, Accepted: true}
			if existing, _ := sn.server.Loop.Registry.Get(t.Name); existing != nil {
				d.Accepted = false
				d.Reason = "duplicates a built-in tool of the same name"
			}
			decisions = append(decisions, d)
		}
	} else {
		for _, t := range p.ExternalTools {
			decisions = append(decisions, ExternalToolDecision{Name: t.Name, Accepted: true})
		}
	}

	res := InitializeResult{
		ProtocolVersion: ProtocolVersion,
		Server: ServerInfo{
			Name:    "metis",
			Version: serverVersion,
		},
		SlashCommands: defaultSlashCommands(),
		ExternalTools: decisions,
	}
	sn.write(Response{JSONRPC: "2.0", ID: id, Result: res})
}

// serverVersion is set by main at startup so InitializeResult
// reports the same version users see in `metis --version`. Plain
// var (not const) so cmd/metis/main.go can populate it.
var serverVersion = "dev"

// SetServerVersion lets cmd/metis/main.go push the build's version
// string into the ACP layer so InitializeResult.Server.Version
// matches what `metis --version` prints. Idempotent.
func SetServerVersion(v string) { serverVersion = v }

// defaultSlashCommands returns the canonical list of `/`-commands
// metis exposes. Hard-coded for now; future work can pull this
// from the slash registry so plugin-added commands also surface to
// ACP clients.
func defaultSlashCommands() []SlashCommand {
	return []SlashCommand{
		{Name: "/help", Description: "Show available commands"},
		{Name: "/new", Description: "Start a new session"},
		{Name: "/clear", Description: "Clear the current session"},
		{Name: "/resume", Description: "Resume a previous session"},
		{Name: "/model", Description: "Show or switch the active model"},
		{Name: "/cost", Description: "Show token + cost stats for the session"},
		{Name: "/compact", Description: "Compact the conversation"},
		{Name: "/save", Description: "Save the session"},
		{Name: "/quit", Description: "Exit metis"},
	}
}

// majorOf returns the part before the first '.' in a "MAJOR.MINOR"
// version string, or the whole string when no dot is present.
func majorOf(v string) string {
	for i := 0; i < len(v); i++ {
		if v[i] == '.' {
			return v[:i]
		}
	}
	return v
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
		done <- rtpkg.RunWithTraceTurn(pctx, sn.server.SessionID, func(turnCtx context.Context) error {
			return sn.server.Loop.Run(turnCtx, events)
		})
		close(events)
	}()

	var incompleteReason string
	for ev := range events {
		if ev.Kind == agent.EventLoopDone && agent.IsIncompleteStopReason(ev.StopReason) {
			incompleteReason = ev.StopReason
		}
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
	if incompleteReason != "" {
		sn.write(Response{JSONRPC: "2.0", ID: id, Error: &ResponseError{Code: -32000, Message: "task incomplete: " + incompleteReason}})
		return
	}
	// A successful prompt is the protocol's durability boundary for both stdio
	// and TCP. Persist here, before the success response, rather than at process
	// shutdown: TCP servers may stay alive indefinitely and each prompt must
	// flush only its explicitly bound session. The fresh grace context prevents
	// a late abort/disconnect from erasing work that Loop.Run already completed.
	if err := sn.persistPromptMemoryBoundary(); err != nil {
		sn.write(Response{JSONRPC: "2.0", ID: id, Error: &ResponseError{Code: -32000, Message: err.Error()}})
		return
	}
	sn.write(Response{JSONRPC: "2.0", ID: id, Result: map[string]any{"done": true}})
}

func (sn *session) persistPromptMemoryBoundary() error {
	if sn == nil || sn.server == nil || sn.server.Loop == nil {
		return nil
	}
	sessionID := strings.TrimSpace(sn.server.SessionID)
	if sessionID == "" {
		// An empty ID means "all sessions" to Loop. ACP must never widen one
		// request's boundary that way; embedders without an owning session keep
		// the legacy no-memory-boundary behavior.
		return nil
	}
	sn.server.Loop.FlushPendingDistillation(sessionID)
	waitCtx, cancel := context.WithTimeout(context.Background(), acpPromptDistillationGrace)
	err := sn.server.Loop.WaitForDistillation(waitCtx, sessionID)
	cancel()
	if err != nil {
		return fmt.Errorf("ACP prompt: join memory distillation for %s: %w", sessionID, err)
	}
	return nil
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
		m["incomplete"] = agent.IsIncompleteStopReason(ev.StopReason)
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
