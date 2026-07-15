package acp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

// --- fake provider/stream ----------------------------------------------------

type fakeStream struct {
	events []llm.StreamEvent
	idx    int
}

func (s *fakeStream) Recv() (llm.StreamEvent, error) {
	if s.idx >= len(s.events) {
		return llm.StreamEvent{}, io.EOF
	}
	ev := s.events[s.idx]
	s.idx++
	return ev, nil
}
func (s *fakeStream) Close() error { return nil }

type fakeProvider struct {
	events []llm.StreamEvent
}

func (p *fakeProvider) Name() string          { return "fake" }
func (p *fakeProvider) MaxContextTokens() int { return 100000 }
func (p *fakeProvider) ModelID() string       { return "" }
func (p *fakeProvider) Complete(_ context.Context, _ llm.Request) (*llm.Response, error) {
	return nil, errors.New("not implemented")
}
func (p *fakeProvider) Stream(_ context.Context, _ llm.Request) (llm.StreamReader, error) {
	return &fakeStream{events: p.events}, nil
}

// helloProvider returns "hi" and stops cleanly.
func helloProvider() llm.Provider {
	return &fakeProvider{events: []llm.StreamEvent{
		{Type: "text_delta", TextDelta: "hi"},
		{Type: "message_delta", StopReason: "end_turn", OutputTokens: 1},
		{Type: "message_stop"},
	}}
}

// blockingProvider blocks Recv forever (used for abort test).
type blockingStream struct {
	closed chan struct{}
	once   sync.Once
}

func (s *blockingStream) Recv() (llm.StreamEvent, error) {
	<-s.closed
	return llm.StreamEvent{}, io.EOF
}
func (s *blockingStream) Close() error {
	s.once.Do(func() { close(s.closed) })
	return nil
}

type blockingProvider struct{}

func (blockingProvider) Name() string          { return "blocking" }
func (blockingProvider) MaxContextTokens() int { return 100000 }
func (blockingProvider) ModelID() string       { return "" }
func (blockingProvider) Complete(_ context.Context, _ llm.Request) (*llm.Response, error) {
	return nil, errors.New("not implemented")
}
func (blockingProvider) Stream(ctx context.Context, _ llm.Request) (llm.StreamReader, error) {
	bs := &blockingStream{closed: make(chan struct{})}
	go func() {
		<-ctx.Done()
		bs.Close()
	}()
	return bs, nil
}

// --- helpers -----------------------------------------------------------------

func newTestServer(t *testing.T, prov llm.Provider) (*Server, *io.PipeWriter, *bufio.Scanner) {
	t.Helper()
	reg := tools.NewRegistry()
	gate := permission.New(permission.ModeBypass)
	loop := agent.NewLoop(prov, reg, gate, agent.NewHookRegistry(), "system", 5)
	loop.Model = "fake-model"
	srv := NewServer(loop, "stdio") // stdio addr means we drive serveConn directly

	// We don't call srv.Listen — drive serveConn over pipes manually.
	// io.Pipe returns (reader, writer): writes to writer, reads from reader.
	c2sR, c2sW := io.Pipe() // client → server
	s2cR, s2cW := io.Pipe() // server → client

	go srv.serveConn(c2sR, s2cW)

	return srv, c2sW, bufio.NewScanner(s2cR)
}

func sendRequest(t *testing.T, w io.Writer, id any, method string, params map[string]any) {
	t.Helper()
	req := map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"method":  method,
	}
	if params != nil {
		req["params"] = params
	}
	b, _ := json.Marshal(req)
	w.Write(append(b, '\n'))
}

// readJSON reads one JSON object at a time from the response stream.
func readJSON(t *testing.T, sc *bufio.Scanner, timeout time.Duration) map[string]any {
	t.Helper()
	type result struct {
		m   map[string]any
		err error
	}
	ch := make(chan result, 1)
	go func() {
		if !sc.Scan() {
			ch <- result{nil, sc.Err()}
			return
		}
		var m map[string]any
		err := json.Unmarshal(sc.Bytes(), &m)
		ch <- result{m, err}
	}()
	select {
	case r := <-ch:
		if r.err != nil {
			t.Fatalf("readJSON: %v", r.err)
		}
		return r.m
	case <-time.After(timeout):
		t.Fatal("readJSON: timeout")
		return nil
	}
}

// --- tests -------------------------------------------------------------------

func TestACP_PromptHappyPath(t *testing.T) {
	_, w, sc := newTestServer(t, helloProvider())

	sendRequest(t, w, 1, "prompt", map[string]any{"prompt": "say hi"})

	// Expect at least one notification with method == session_update,
	// and eventually a final Response with result.done==true and id==1.
	sawTextDelta := false
	sawLoopDone := false
	sawFinalResponse := false

	deadline := time.After(3 * time.Second)
	for !sawFinalResponse {
		select {
		case <-deadline:
			t.Fatalf("timed out: textDelta=%v loopDone=%v final=%v", sawTextDelta, sawLoopDone, sawFinalResponse)
		default:
		}
		m := readJSON(t, sc, 3*time.Second)
		if method, ok := m["method"].(string); ok && method == "session_update" {
			params, _ := m["params"].(map[string]any)
			kind, _ := params["kind"].(string)
			if kind == "text_delta" {
				sawTextDelta = true
			}
			if kind == "loop_done" {
				sawLoopDone = true
			}
			continue
		}
		if id, ok := m["id"].(float64); ok && id == 1 {
			result, _ := m["result"].(map[string]any)
			if done, _ := result["done"].(bool); done {
				sawFinalResponse = true
			}
		}
	}

	if !sawTextDelta {
		t.Error("expected text_delta notification")
	}
	if !sawLoopDone {
		t.Error("expected loop_done notification")
	}

	w.Close()
}

func TestACP_UnknownMethod(t *testing.T) {
	_, w, sc := newTestServer(t, helloProvider())

	sendRequest(t, w, 42, "nope.unknown", nil)

	m := readJSON(t, sc, 2*time.Second)
	errObj, ok := m["error"].(map[string]any)
	if !ok {
		t.Fatalf("expected error object, got: %v", m)
	}
	code, _ := errObj["code"].(float64)
	if int(code) != -32601 {
		t.Errorf("expected -32601 method not found, got %v", code)
	}
	w.Close()
}

func TestACP_PermissionReplyMissingPending(t *testing.T) {
	_, w, sc := newTestServer(t, helloProvider())

	sendRequest(t, w, 7, "permission_reply", map[string]any{
		"tool_use_id": "no-such-id",
		"decision":    "allow",
	})

	m := readJSON(t, sc, 2*time.Second)
	errObj, ok := m["error"].(map[string]any)
	if !ok {
		t.Fatalf("expected error, got: %v", m)
	}
	if !strings.Contains(errObj["message"].(string), "no pending permission") {
		t.Errorf("unexpected error message: %v", errObj["message"])
	}
	w.Close()
}

func TestACP_AbortReturnsAcknowledgement(t *testing.T) {
	_, w, sc := newTestServer(t, blockingProvider{})

	// Kick off a long-running prompt.
	sendRequest(t, w, "p1", "prompt", map[string]any{"prompt": "long task"})

	// Give the server a moment to register the prompt.
	time.Sleep(50 * time.Millisecond)

	// Send abort referencing prompt id "p1".
	sendRequest(t, w, 99, "abort", map[string]any{"prompt_id": "p1"})

	// Read responses until we see the abort ack with id==99.
	deadline := time.After(3 * time.Second)
	for {
		select {
		case <-deadline:
			t.Fatal("timed out waiting for abort ack")
		default:
		}
		m := readJSON(t, sc, 3*time.Second)
		if id, ok := m["id"]; ok {
			if str, isStr := id.(string); isStr && str == "99" {
				// fall through; treat numeric below
			}
			// id is unmarshaled as float64 for JSON numbers
			if num, isNum := id.(float64); isNum && int(num) == 99 {
				result, _ := m["result"].(map[string]any)
				if aborted, _ := result["aborted"].(bool); aborted {
					w.Close()
					return
				}
				t.Fatalf("expected aborted=true, got: %v", result)
			}
		}
	}
}

func TestACP_PromptIDOf(t *testing.T) {
	cases := []struct {
		in  any
		out string
	}{
		{"abc", "abc"},
		{float64(42), "42"},
		{nil, ""},
	}
	for _, c := range cases {
		if got := promptIDOf(c.in); got != c.out {
			t.Errorf("promptIDOf(%v) = %q, want %q", c.in, got, c.out)
		}
	}
}

// Regression: Close on a TCP server with active connections used to hang
// because listener.Close didn't unblock the per-conn json.Decode.
func TestACP_CloseUnblocksActiveTCPConnections(t *testing.T) {
	reg := tools.NewRegistry()
	gate := permission.New(permission.ModeBypass)
	loop := agent.NewLoop(helloProvider(), reg, gate, agent.NewHookRegistry(), "system", 5)
	srv := NewServer(loop, "127.0.0.1:0")
	if err := srv.Listen(); err != nil {
		t.Fatal(err)
	}
	addr := srv.ln.Addr().String()

	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	// Wait until the server registers the connection.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		srv.mu.Lock()
		n := len(srv.conns)
		srv.mu.Unlock()
		if n == 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	closeDone := make(chan struct{})
	go func() {
		_ = srv.Close()
		close(closeDone)
	}()
	select {
	case <-closeDone:
		conn.Close()
	case <-time.After(2 * time.Second):
		conn.Close()
		t.Fatal("Server.Close hung — active TCP conn was not force-closed")
	}
}

func TestACP_EventToMapAllKinds(t *testing.T) {
	cases := []agent.Event{
		{Kind: agent.EventTextDelta, TextDelta: "hi"},
		{Kind: agent.EventToolStart, ToolUseID: "t1", ToolName: "Read", ToolInput: map[string]any{"path": "/x"}},
		{Kind: agent.EventToolResult, ToolUseID: "t1", ToolResult: &agent.ToolResult{Output: "ok"}},
		{Kind: agent.EventLoopDone, StopReason: "end_turn"},
		{Kind: agent.EventTokens, InputTokens: 10, OutputTokens: 20},
		{Kind: agent.EventError, Err: errors.New("boom")},
		{Kind: agent.EventInfo, Info: "test"},
		{Kind: agent.EventPermissionRequest, ToolUseID: "t2", PermissionTool: "Bash", PermissionInput: map[string]any{"command": "ls"}, PermissionReason: "policy"},
	}
	for _, ev := range cases {
		m := eventToMap(ev)
		if _, ok := m["kind"].(string); !ok {
			t.Errorf("event %v: missing kind", ev.Kind)
		}
	}
}
