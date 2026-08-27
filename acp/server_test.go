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
	"github.com/Ricardo-M-L/metis/internal/memory"
	"github.com/Ricardo-M-L/metis/internal/permission"
	rtpkg "github.com/Ricardo-M-L/metis/internal/runtime"
	sessionpkg "github.com/Ricardo-M-L/metis/internal/session"
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

// contextBlockingProvider reports when a request reaches the provider and
// then returns the request context's cancellation error. Unlike blockingStream
// it cannot turn a cancellation into a clean EOF, which makes it suitable for
// proving that service shutdown does not persist a partial turn.
type contextBlockingProvider struct {
	started       chan struct{}
	cancelled     chan struct{}
	startedOnce   sync.Once
	cancelledOnce sync.Once
}

func newContextBlockingProvider() *contextBlockingProvider {
	return &contextBlockingProvider{started: make(chan struct{}), cancelled: make(chan struct{})}
}

func (p *contextBlockingProvider) Name() string          { return "context-blocking" }
func (p *contextBlockingProvider) MaxContextTokens() int { return 100000 }
func (p *contextBlockingProvider) ModelID() string       { return "" }
func (p *contextBlockingProvider) Complete(_ context.Context, _ llm.Request) (*llm.Response, error) {
	return nil, errors.New("not implemented")
}
func (p *contextBlockingProvider) Stream(ctx context.Context, _ llm.Request) (llm.StreamReader, error) {
	p.startedOnce.Do(func() { close(p.started) })
	return &contextBlockingStream{ctx: ctx, provider: p}, nil
}

type contextBlockingStream struct {
	ctx      context.Context
	provider *contextBlockingProvider
}

func (s *contextBlockingStream) Recv() (llm.StreamEvent, error) {
	<-s.ctx.Done()
	s.provider.cancelledOnce.Do(func() { close(s.provider.cancelled) })
	return llm.StreamEvent{}, s.ctx.Err()
}
func (*contextBlockingStream) Close() error { return nil }

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

func TestACP_PromptTraceUsesExplicitServerSessionAcrossTurns(t *testing.T) {
	traceStore, err := sessionpkg.NewTraceStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = traceStore.Close() })

	oldAdapter := rtpkg.CurrentTraceAdapter()
	adapter := rtpkg.NewTraceAdapter(traceStore)
	rtpkg.SetTraceAdapter(adapter)
	agent.SetTraceHook(adapter.OnEvent)
	t.Cleanup(func() {
		rtpkg.SetTraceAdapter(oldAdapter)
		if oldAdapter != nil {
			agent.SetTraceHook(oldAdapter.OnEvent)
		} else {
			agent.SetTraceHook(nil)
		}
	})
	adapter.SetSession("selected-other-session")

	reg := tools.NewRegistry()
	gate := permission.New(permission.ModeBypass)
	loop := agent.NewLoop(helloProvider(), reg, gate, agent.NewHookRegistry(), "system", 5)
	loop.Model = "fake-model"
	const sessionID = "acp-owned-session"
	srv := NewServerForSession(loop, "stdio", sessionID)
	sn := &session{
		enc:     json.NewEncoder(io.Discard),
		server:  srv,
		prompts: make(map[string]context.CancelFunc),
		perms:   make(map[string]chan agent.PermissionDecision),
	}

	sn.handlePrompt(context.Background(), 1, PromptParams{Prompt: "first"})
	sn.handlePrompt(context.Background(), 2, PromptParams{Prompt: "second"})

	events := traceStore.Events(sessionID)
	if len(events) == 0 {
		t.Fatal("ACP prompts produced no trace events")
	}
	sawTurn1, sawTurn2 := false, false
	for _, event := range events {
		switch event.Turn {
		case 1:
			sawTurn1 = true
		case 2:
			sawTurn2 = true
		default:
			t.Fatalf("trace event has unexpected turn: %+v", event)
		}
	}
	if !sawTurn1 || !sawTurn2 {
		t.Fatalf("ACP trace turns: saw turn1=%v turn2=%v events=%+v", sawTurn1, sawTurn2, events)
	}
	if leaked := traceStore.Events("selected-other-session"); len(leaked) != 0 {
		t.Fatalf("ACP trace leaked into selected session: %+v", leaked)
	}
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

func TestACP_CloseCancelsActiveTCPPromptWithoutPersistingPartialTurn(t *testing.T) {
	const sessionID = "acp-cancelled-session"
	manager, err := memory.NewMemoryManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	repository := &acpPromptBoundaryRepository{Repository: manager}
	provider := newContextBlockingProvider()
	loop := agent.NewLoop(
		provider,
		tools.NewRegistry(),
		permission.New(permission.ModeBypass),
		agent.NewHookRegistry(),
		"system",
		5,
	)
	loop.Memory = repository
	loop.CurrentStateSnapshot = func() agent.RuntimeStateSnapshot {
		return agent.RuntimeStateSnapshot{SessionID: sessionID}
	}

	srv := NewServerForSession(loop, "127.0.0.1:0", sessionID)
	if err := srv.Listen(); err != nil {
		t.Fatal(err)
	}
	conn, err := net.Dial("tcp", srv.ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	sendRequest(t, conn, 73, "prompt", map[string]any{"prompt": "do not persist this partial turn"})

	select {
	case <-provider.started:
	case <-time.After(2 * time.Second):
		t.Fatal("prompt never reached provider")
	}

	closeDone := make(chan error, 1)
	go func() { closeDone <- srv.Close() }()
	select {
	case <-provider.cancelled:
	case <-time.After(2 * time.Second):
		t.Fatal("Server.Close did not cancel active prompt context")
	}
	select {
	case err := <-closeDone:
		if err != nil {
			t.Fatalf("Server.Close: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Server.Close hung waiting for cancelled prompt")
	}

	recorded, distilled := repository.snapshot()
	if len(recorded) != 0 || len(distilled) != 0 {
		t.Fatalf("cancelled prompt persisted partial memory: recorded=%v distilled=%v", recorded, distilled)
	}
}

func TestACP_CloseUnblocksStdioReader(t *testing.T) {
	loop := agent.NewLoop(
		helloProvider(),
		tools.NewRegistry(),
		permission.New(permission.ModeBypass),
		agent.NewHookRegistry(),
		"system",
		5,
	)
	srv := NewServer(loop, "stdio")
	inputReader, inputWriter := io.Pipe()
	defer inputWriter.Close()
	srv.stdioIn = inputReader
	srv.stdioOut = io.Discard
	if err := srv.Listen(); err != nil {
		t.Fatal(err)
	}

	closeDone := make(chan error, 1)
	go func() { closeDone <- srv.Close() }()
	select {
	case err := <-closeDone:
		if err != nil {
			t.Fatalf("Server.Close: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Server.Close hung on stdio decoder")
	}
}

type acpPromptBoundaryRepository struct {
	memory.Repository

	mu        sync.Mutex
	recorded  []string
	distilled []string
}

func (r *acpPromptBoundaryRepository) RecordTurn(
	_ context.Context,
	sessionID, _, _, _ string,
) error {
	r.mu.Lock()
	r.recorded = append(r.recorded, sessionID)
	r.mu.Unlock()
	return nil
}

func (r *acpPromptBoundaryRepository) DistillTurnWithMetadata(
	_ context.Context,
	_ llm.Provider,
	sessionID, _, _, _ string,
) error {
	r.mu.Lock()
	r.distilled = append(r.distilled, sessionID)
	r.mu.Unlock()
	return nil
}

func (r *acpPromptBoundaryRepository) snapshot() (recorded, distilled []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.recorded...), append([]string(nil), r.distilled...)
}

func TestACP_TCPPromptPersistsResidualBeforeSuccessResponse(t *testing.T) {
	const sessionID = "acp-tcp-session"
	manager, err := memory.NewMemoryManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	repository := &acpPromptBoundaryRepository{Repository: manager}
	loop := agent.NewLoop(
		helloProvider(),
		tools.NewRegistry(),
		permission.New(permission.ModeBypass),
		agent.NewHookRegistry(),
		"system",
		5,
	)
	loop.Memory = repository
	loop.DistillEvery = 5
	loop.CurrentStateSnapshot = func() agent.RuntimeStateSnapshot {
		return agent.RuntimeStateSnapshot{SessionID: sessionID}
	}

	srv := NewServerForSession(loop, "127.0.0.1:0", sessionID)
	if err := srv.Listen(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	conn, err := net.Dial("tcp", srv.ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}

	request := map[string]any{
		"jsonrpc": "2.0",
		"id":      17,
		"method":  "prompt",
		"params":  map[string]any{"prompt": "Remember the TCP release codename White Finch."},
	}
	if err := json.NewEncoder(conn).Encode(request); err != nil {
		t.Fatal(err)
	}

	decoder := json.NewDecoder(conn)
	for {
		var response map[string]any
		if err := decoder.Decode(&response); err != nil {
			t.Fatalf("decode ACP response: %v", err)
		}
		id, hasID := response["id"].(float64)
		if !hasID || int(id) != 17 {
			continue
		}
		if response["error"] != nil {
			t.Fatalf("ACP prompt error: %v", response["error"])
		}
		break
	}

	recorded, distilled := repository.snapshot()
	if len(recorded) != 1 || recorded[0] != sessionID {
		t.Fatalf("recorded sessions = %v, want [%s]", recorded, sessionID)
	}
	if len(distilled) != 1 || distilled[0] != sessionID {
		t.Fatalf("distilled sessions at success response = %v, want exactly [%s]", distilled, sessionID)
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
