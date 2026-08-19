package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/tasks"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

type loopRegressionStream struct {
	events []llm.StreamEvent
	idx    int
}

func (s *loopRegressionStream) Recv() (llm.StreamEvent, error) {
	if s.idx >= len(s.events) {
		return llm.StreamEvent{}, io.EOF
	}
	ev := s.events[s.idx]
	s.idx++
	return ev, nil
}

func (s *loopRegressionStream) Close() error { return nil }

func textStream(text string) llm.StreamReader {
	return &loopRegressionStream{events: []llm.StreamEvent{
		{Type: "text_delta", TextDelta: text},
		{Type: "message_delta", StopReason: "end_turn"},
		{Type: "message_stop"},
	}}
}

// gatedRegressionStream lets a test inject steering only after Run is
// genuinely blocked inside StreamReader.Recv, reproducing the final-token
// race instead of preloading steer before Run starts.
type gatedRegressionStream struct {
	started chan struct{}
	release chan struct{}
	events  []llm.StreamEvent
	err     error
	once    sync.Once
	idx     int
}

func (s *gatedRegressionStream) Recv() (llm.StreamEvent, error) {
	s.once.Do(func() {
		close(s.started)
		<-s.release
	})
	if s.err != nil {
		return llm.StreamEvent{}, s.err
	}
	if s.idx >= len(s.events) {
		return llm.StreamEvent{}, io.EOF
	}
	ev := s.events[s.idx]
	s.idx++
	return ev, nil
}

func (s *gatedRegressionStream) Close() error { return nil }

type queuedStreamProvider struct {
	mu       sync.Mutex
	streams  []llm.StreamReader
	requests []llm.Request
}

func (p *queuedStreamProvider) Name() string          { return "queued-stream" }
func (p *queuedStreamProvider) ModelID() string       { return "test-model" }
func (p *queuedStreamProvider) MaxContextTokens() int { return 200_000 }
func (p *queuedStreamProvider) Complete(context.Context, llm.Request) (*llm.Response, error) {
	return nil, errors.New("queued stream provider only supports Stream")
}
func (p *queuedStreamProvider) Stream(_ context.Context, req llm.Request) (llm.StreamReader, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.requests = append(p.requests, req)
	if len(p.streams) == 0 {
		return textStream("done"), nil
	}
	stream := p.streams[0]
	p.streams = p.streams[1:]
	return stream, nil
}

func (p *queuedStreamProvider) capturedRequests() []llm.Request {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]llm.Request(nil), p.requests...)
}

// loopRegressionProvider emits toolCalls short successful tool rounds, then
// a real final answer. It captures every request so tests can verify exactly
// when run-local nudges and the diminishing rescue entered history.
type loopRegressionProvider struct {
	mu        sync.Mutex
	toolCalls int
	calls     []llm.Request
}

func (p *loopRegressionProvider) Name() string          { return "loop-regression" }
func (p *loopRegressionProvider) ModelID() string       { return "" }
func (p *loopRegressionProvider) MaxContextTokens() int { return 200_000 }
func (p *loopRegressionProvider) Complete(context.Context, llm.Request) (*llm.Response, error) {
	return nil, errors.New("loop regression provider only supports Stream")
}
func (p *loopRegressionProvider) Stream(_ context.Context, req llm.Request) (llm.StreamReader, error) {
	p.mu.Lock()
	p.calls = append(p.calls, req)
	call := len(p.calls)
	p.mu.Unlock()
	if call <= p.toolCalls {
		id := "low-" + string(rune('a'+call-1))
		return &loopRegressionStream{events: []llm.StreamEvent{
			{Type: "tool_use_start", ToolUseID: id, ToolName: "LowOutput"},
			{Type: "tool_input_delta", ToolUseID: id, InputDelta: `{"payload":"tiny"}`},
			{Type: "tool_use_stop", ToolUseID: id, InputDelta: `{"payload":"tiny"}`},
			{Type: "message_delta", StopReason: "tool_use"},
			{Type: "message_stop"},
		}}, nil
	}
	return &loopRegressionStream{events: []llm.StreamEvent{
		{Type: "text_delta", TextDelta: "finished and verified"},
		{Type: "message_delta", StopReason: "end_turn"},
		{Type: "message_stop"},
	}}, nil
}

func (p *loopRegressionProvider) requests() []llm.Request {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]llm.Request(nil), p.calls...)
}

type lowOutputTool struct{ tools.BaseTool }

func (lowOutputTool) Name() string                { return "LowOutput" }
func (lowOutputTool) Description() string         { return "returns a deliberately short successful result" }
func (lowOutputTool) InputSchema() map[string]any { return map[string]any{"type": "object"} }
func (lowOutputTool) Concurrency(map[string]any) tools.Concurrency {
	return tools.ConcurrencySafe
}
func (lowOutputTool) CanUse(context.Context, map[string]any) (tools.Permission, string) {
	return tools.PermissionAllow, ""
}
func (lowOutputTool) Execute(context.Context, map[string]any) (*tools.Result, error) {
	return &tools.Result{Output: "tiny"}, nil
}

func requestContains(req llm.Request, needle string) bool {
	for _, msg := range req.Messages {
		for _, block := range msg.Content {
			if strings.Contains(block.Text, needle) || strings.Contains(block.ToolResult, needle) {
				return true
			}
		}
	}
	return false
}

func TestLoopRun_IterationBudgetIsPerRun(t *testing.T) {
	provider := &loopRegressionProvider{}
	loop := NewLoop(provider, tools.NewRegistry(), permission.New(permission.ModeAcceptEdits), nil, "sys", 10)
	loop.iterIdx = 140 // historical session count must not consume this Run's budget
	loop.AppendUser("new user turn")
	out := make(chan Event, 64)
	if err := loop.Run(context.Background(), out); err != nil {
		t.Fatal(err)
	}
	close(out)

	reqs := provider.requests()
	if len(reqs) != 1 {
		t.Fatalf("new Run should finish in one provider call, got %d", len(reqs))
	}
	for _, stale := range []string{"50%", "75%", "90%", "iteration cap"} {
		if requestContains(reqs[0], stale) {
			t.Errorf("first request inherited cumulative iteration warning %q", stale)
		}
	}
	for ev := range out {
		if ev.StopReason == "max_iterations" {
			t.Fatal("cumulative iterIdx incorrectly exhausted a fresh Run")
		}
	}
}

func TestLoopRun_FinalResponseSteerReentersCurrentRun(t *testing.T) {
	provider := &loopRegressionProvider{}
	loop := NewLoop(provider, tools.NewRegistry(), permission.New(permission.ModeAcceptEdits), nil, "sys", 10)
	loop.AppendUser("initial request")
	if ok := loop.SteerInject("also include the verification result"); !ok {
		t.Fatal("active loop should accept steer")
	}
	out := make(chan Event, 64)
	if err := loop.Run(context.Background(), out); err != nil {
		t.Fatal(err)
	}
	close(out)
	turnEnds := 0
	for ev := range out {
		if ev.Kind == EventTurnEnd {
			turnEnds++
		}
	}
	if turnEnds != 1 {
		t.Fatalf("steer re-entry should expose one assistant boundary; EventTurnEnd=%d", turnEnds)
	}

	reqs := provider.requests()
	if len(reqs) != 2 {
		t.Fatalf("pending steer at end_turn should re-enter the same Run; calls=%d", len(reqs))
	}
	if requestContains(reqs[0], "also include the verification result") {
		t.Fatal("steer unexpectedly leaked into the request that was already in flight")
	}
	if !requestContains(reqs[1], "[user steer mid-turn] also include the verification result") {
		t.Fatal("second request did not include the mid-turn steer")
	}
	if ok := loop.SteerInject("too late"); ok {
		t.Fatal("steering gate should be closed atomically after normal LoopDone")
	}
}

func TestLoopRun_FinalStreamSteerInjectedConcurrently(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	first := &gatedRegressionStream{
		started: started,
		release: release,
		events: []llm.StreamEvent{
			{Type: "text_delta", TextDelta: "first answer"},
			{Type: "message_delta", StopReason: "end_turn"},
			{Type: "message_stop"},
		},
	}
	provider := &queuedStreamProvider{streams: []llm.StreamReader{
		first,
		textStream("revised answer"),
	}}
	loop := NewLoop(provider, tools.NewRegistry(), permission.New(permission.ModeAcceptEdits), nil, "sys", 10)
	loop.AppendUser("initial request")
	out := make(chan Event, 64)
	done := make(chan error, 1)
	go func() { done <- loop.Run(context.Background(), out) }()

	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not block in the first final-response stream")
	}
	if ok := loop.SteerInject("include the live correction"); !ok {
		t.Fatal("in-flight final response should accept steer")
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	close(out)

	turnEnds := 0
	for ev := range out {
		if ev.Kind == EventTurnEnd {
			turnEnds++
		}
	}
	if turnEnds != 1 {
		t.Fatalf("concurrent final-stream steer should create exactly one assistant boundary; got %d", turnEnds)
	}
	reqs := provider.capturedRequests()
	if len(reqs) != 2 {
		t.Fatalf("provider requests = %d, want 2", len(reqs))
	}
	if requestContains(reqs[0], "include the live correction") {
		t.Fatal("steer appeared in the request that was already streaming")
	}
	if !requestContains(reqs[1], "[user steer mid-turn] include the live correction") {
		t.Fatal("follow-up request did not contain the concurrently injected steer")
	}
}

func TestLoopRun_StreamErrorDiscardsAcceptedSteer(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	provider := &queuedStreamProvider{streams: []llm.StreamReader{
		&gatedRegressionStream{
			started: started,
			release: release,
			err:     errors.New("stream exploded"),
		},
		textStream("fresh run complete"),
	}}
	loop := NewLoop(provider, tools.NewRegistry(), permission.New(permission.ModeAcceptEdits), nil, "sys", 10)
	loop.AppendUser("first run")
	out1 := make(chan Event, 64)
	done := make(chan error, 1)
	go func() { done <- loop.Run(context.Background(), out1) }()

	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not enter the failing stream")
	}
	if ok := loop.SteerInject("must not leak"); !ok {
		t.Fatal("in-flight run should accept steer before the provider fails")
	}
	close(release)
	if err := <-done; err == nil || !strings.Contains(err.Error(), "stream exploded") {
		t.Fatalf("Run error = %v, want stream exploded", err)
	}
	close(out1)
	var sawResendNotice bool
	for ev := range out1 {
		if ev.Kind == EventInfo && strings.Contains(ev.Info, "accepted steering") && strings.Contains(ev.Info, "resend") {
			sawResendNotice = true
		}
	}
	if !sawResendNotice {
		t.Fatal("provider failure discarded steer without an explicit resend notice")
	}

	loop.mu.RLock()
	buffered := len(loop.steerBuf)
	closed := loop.steerClosed
	loop.mu.RUnlock()
	if buffered != 0 || !closed {
		t.Fatalf("failed Run left steering state buffered=%d closed=%v", buffered, closed)
	}

	loop.AppendUser("second run")
	out2 := make(chan Event, 64)
	if err := loop.Run(context.Background(), out2); err != nil {
		t.Fatalf("second Run: %v", err)
	}
	reqs := provider.capturedRequests()
	if len(reqs) != 2 {
		t.Fatalf("provider requests = %d, want 2", len(reqs))
	}
	if requestContains(reqs[1], "must not leak") {
		t.Fatal("steer accepted by failed Run leaked into the next Run")
	}
}

func TestLoopRun_CancelDiscardsAcceptedSteerWithNotice(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	provider := &queuedStreamProvider{streams: []llm.StreamReader{
		&gatedRegressionStream{
			started: started,
			release: release,
			err:     context.Canceled,
		},
	}}
	loop := NewLoop(provider, tools.NewRegistry(), permission.New(permission.ModeAcceptEdits), nil, "sys", 10)
	loop.AppendUser("cancelled run")
	ctx, cancel := context.WithCancel(context.Background())
	out := make(chan Event, 64)
	done := make(chan error, 1)
	go func() { done <- loop.Run(ctx, out) }()

	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not enter the cancellable stream")
	}
	if ok := loop.SteerInject("cancelled steer"); !ok {
		t.Fatal("in-flight run should accept steer before cancellation")
	}
	cancel()
	close(release)
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("Run error = %v, want context.Canceled", err)
	}
	close(out)

	var sawResendNotice bool
	for ev := range out {
		if ev.Kind == EventInfo && strings.Contains(ev.Info, "accepted steering") && strings.Contains(ev.Info, "resend") {
			sawResendNotice = true
		}
	}
	if !sawResendNotice {
		t.Fatal("cancelled Run discarded steer without an explicit resend notice")
	}
	loop.mu.RLock()
	buffered := len(loop.steerBuf)
	closed := loop.steerClosed
	loop.mu.RUnlock()
	if buffered != 0 || !closed {
		t.Fatalf("cancelled Run left steering state buffered=%d closed=%v", buffered, closed)
	}
}

func TestLoopRun_DiscardsArtificialStaleSteerAtRunStart(t *testing.T) {
	provider := &queuedStreamProvider{streams: []llm.StreamReader{textStream("done")}}
	loop := NewLoop(provider, tools.NewRegistry(), permission.New(permission.ModeAcceptEdits), nil, "sys", 10)
	loop.AppendUser("fresh turn")
	loop.mu.Lock()
	loop.steerClosed = true
	loop.steerBuf = []string{"stale from interrupted run"}
	loop.mu.Unlock()

	out := make(chan Event, 64)
	if err := loop.Run(context.Background(), out); err != nil {
		t.Fatal(err)
	}
	close(out)
	var sawDiscardInfo bool
	for ev := range out {
		if ev.Kind == EventInfo && strings.Contains(ev.Info, "discarded 1 stale steering") {
			sawDiscardInfo = true
		}
	}
	if !sawDiscardInfo {
		t.Fatal("stale-buffer cleanup was not surfaced as an info event")
	}
	reqs := provider.capturedRequests()
	if len(reqs) != 1 || requestContains(reqs[0], "stale from interrupted run") {
		t.Fatal("stale steer reached the provider request")
	}
}

func TestLoopRun_ContractTextReentryEmitsAssistantBoundary(t *testing.T) {
	t.Setenv(contractDisableEnvVar, "0")
	provider := &queuedStreamProvider{streams: []llm.StreamReader{
		textStream("implementation complete"),
		textStream("OVERRIDE CONTRACT: verification does not apply to this fixture"),
	}}
	loop := NewLoop(provider, tools.NewRegistry(), permission.New(permission.ModeAcceptEdits), nil, "sys", 5)
	loop.contract.mainWrites = contractWriteThreshold
	loop.AppendUser("finish")
	out := make(chan Event, 64)
	if err := loop.Run(context.Background(), out); err != nil {
		t.Fatal(err)
	}
	close(out)

	turnEnds := 0
	for ev := range out {
		if ev.Kind == EventTurnEnd {
			turnEnds++
		}
	}
	if turnEnds != 1 {
		t.Fatalf("contract text re-entry EventTurnEnd=%d, want 1", turnEnds)
	}
	if len(provider.capturedRequests()) != 2 {
		t.Fatal("contract gate did not re-enter the provider exactly once")
	}
}

func TestLoopRun_TodoTextReentryEmitsAssistantBoundary(t *testing.T) {
	t.Setenv("METIS_HOME", t.TempDir())
	sessionID := "boundary-reconcile"
	tasks.SetCurrentSessionID(sessionID)
	t.Cleanup(func() { tasks.SetCurrentSessionID("") })
	if _, err := tasks.ReplaceAll(sessionID, []tasks.Item{{
		Status: "in_progress", Content: "finish verification",
	}}); err != nil {
		t.Fatal(err)
	}

	provider := &queuedStreamProvider{streams: []llm.StreamReader{
		textStream("work is done"),
		textStream("open item acknowledged"),
	}}
	loop := NewLoop(provider, tools.NewRegistry(), permission.New(permission.ModeAcceptEdits), nil, "sys", 5)
	loop.AppendUser("finish")
	out := make(chan Event, 64)
	if err := loop.Run(context.Background(), out); err != nil {
		t.Fatal(err)
	}
	close(out)

	turnEnds := 0
	for ev := range out {
		if ev.Kind == EventTurnEnd {
			turnEnds++
		}
	}
	if turnEnds != 1 {
		t.Fatalf("todo text re-entry EventTurnEnd=%d, want 1", turnEnds)
	}
	if len(provider.capturedRequests()) != 2 {
		t.Fatal("todo reconciliation did not re-enter the provider exactly once")
	}
}

type stuckBashProvider struct {
	mu       sync.Mutex
	requests []llm.Request
}

func (p *stuckBashProvider) Name() string          { return "stuck-bash" }
func (p *stuckBashProvider) ModelID() string       { return "test-model" }
func (p *stuckBashProvider) MaxContextTokens() int { return 200_000 }
func (p *stuckBashProvider) Complete(context.Context, llm.Request) (*llm.Response, error) {
	return nil, errors.New("stuck bash provider only supports Stream")
}
func (p *stuckBashProvider) Stream(_ context.Context, req llm.Request) (llm.StreamReader, error) {
	p.mu.Lock()
	p.requests = append(p.requests, req)
	call := len(p.requests)
	p.mu.Unlock()
	id := fmt.Sprintf("bash-%02d", call)
	return &loopRegressionStream{events: []llm.StreamEvent{
		{Type: "tool_use_start", ToolUseID: id, ToolName: "Bash"},
		{Type: "tool_input_delta", ToolUseID: id, InputDelta: `{"command":"go test ./..."}`},
		{Type: "tool_use_stop", ToolUseID: id},
		{Type: "message_delta", StopReason: "tool_use"},
		{Type: "message_stop"},
	}}, nil
}

type alwaysFailingBashTool struct{ tools.BaseTool }

func (alwaysFailingBashTool) Name() string        { return "Bash" }
func (alwaysFailingBashTool) Description() string { return "returns one stable test failure" }
func (alwaysFailingBashTool) InputSchema() map[string]any {
	return map[string]any{"type": "object"}
}
func (alwaysFailingBashTool) Concurrency(map[string]any) tools.Concurrency {
	return tools.ConcurrencySafe
}
func (alwaysFailingBashTool) CanUse(context.Context, map[string]any) (tools.Permission, string) {
	return tools.PermissionAllow, ""
}
func (alwaysFailingBashTool) Execute(context.Context, map[string]any) (*tools.Result, error) {
	return &tools.Result{Output: "--- FAIL: TestStuck (0.00s)\nFAIL", IsError: true}, nil
}

func TestLoopRun_StuckAbortPreservesLatestRealToolResult(t *testing.T) {
	provider := &stuckBashProvider{}
	registry := tools.NewRegistry()
	registry.Register(alwaysFailingBashTool{})
	loop := NewLoop(provider, registry, permission.New(permission.ModeAcceptEdits), nil, "sys", 20)
	loop.AppendUser("fix the tests")
	out := make(chan Event, 256)
	if err := loop.Run(context.Background(), out); err != nil {
		t.Fatal(err)
	}
	close(out)

	var stopReason string
	for ev := range out {
		if ev.Kind == EventLoopDone {
			stopReason = ev.StopReason
		}
	}
	if stopReason != "stuck_after_reset" {
		t.Fatalf("stop reason = %q, want stuck_after_reset", stopReason)
	}

	var sawLatestReal, sawOrphan bool
	for _, msg := range loop.History() {
		for _, block := range msg.Content {
			if block.ToolUseID == "bash-12" && strings.Contains(block.ToolResult, "--- FAIL: TestStuck") {
				sawLatestReal = true
			}
			if strings.Contains(block.ToolResult, orphanRepairMessage) {
				sawOrphan = true
			}
		}
	}
	if !sawLatestReal {
		t.Fatal("stuck abort did not persist the twelfth tool's real failure result")
	}
	if sawOrphan {
		t.Fatal("stuck abort history contains orphan-repair text for a completed tool")
	}
	provider.mu.Lock()
	requestCount := len(provider.requests)
	provider.mu.Unlock()
	if requestCount != 12 {
		t.Fatalf("provider requests = %d, want 12 (reset at 4/8, abort at 12)", requestCount)
	}
}

func TestLoopRun_DiminishingRescuePreservesResultsAndFinalAnswer(t *testing.T) {
	provider := &loopRegressionProvider{toolCalls: 11}
	registry := tools.NewRegistry()
	registry.Register(lowOutputTool{})
	loop := NewLoop(provider, registry, permission.New(permission.ModeAcceptEdits), nil, "sys", 12)
	loop.AppendUser("finish the active task")
	out := make(chan Event, 512)
	if err := loop.Run(context.Background(), out); err != nil {
		t.Fatal(err)
	}
	close(out)

	var stop string
	var sawRecovery bool
	for ev := range out {
		if ev.Kind == EventLoopDone {
			stop = ev.StopReason
		}
		if ev.Kind == EventInfo && strings.Contains(ev.Info, "preserving results") {
			sawRecovery = true
		}
	}
	if stop != "end_turn" {
		t.Fatalf("stop = %q, want end_turn (never diminishing_returns)", stop)
	}
	if !sawRecovery {
		t.Fatal("expected a visible diminishing recovery event")
	}

	history := loop.History()
	var sawLastRealResult, sawFinal, sawOrphan bool
	for _, msg := range history {
		for _, block := range msg.Content {
			if block.ToolUseID == "low-k" && block.ToolResult == "tiny" {
				sawLastRealResult = true
			}
			if block.Text == "finished and verified" {
				sawFinal = true
			}
			if strings.Contains(block.ToolResult, orphanRepairMessage) {
				sawOrphan = true
			}
		}
	}
	if !sawLastRealResult {
		t.Fatal("last completed tool result was not preserved in history")
	}
	if sawOrphan {
		t.Fatal("completed tool result was replaced by orphan repair")
	}
	if !sawFinal {
		t.Fatal("loop ended without user-facing final assistant text")
	}

	reqs := provider.requests()
	if len(reqs) != 12 {
		t.Fatalf("provider calls = %d, want 11 tools + final response", len(reqs))
	}
	if requestContains(reqs[9], "Recent tool calls produced little new output") ||
		requestContains(reqs[10], "Recent tool calls produced little new output") {
		t.Fatal("pre-75% low-output history tripped recovery before three new post-75% iterations")
	}
	if !requestContains(reqs[11], "Recent tool calls produced little new output") {
		t.Fatal("final request did not receive the bounded recovery reminder")
	}
}

// rescueStrippingProvider keeps emitting a tool call whenever the request
// carries tools, and emits a final text answer when tools are absent — so a
// rescue request with tools stripped is the only thing that terminates the
// loop. It records every request's tool set for assertions.
type rescueStrippingProvider struct {
	mu       sync.Mutex
	requests []llm.Request
}

func (p *rescueStrippingProvider) Name() string          { return "rescue-stripping" }
func (p *rescueStrippingProvider) ModelID() string       { return "test-model" }
func (p *rescueStrippingProvider) MaxContextTokens() int { return 200_000 }
func (p *rescueStrippingProvider) Complete(context.Context, llm.Request) (*llm.Response, error) {
	return nil, errors.New("rescue stripping provider only supports Stream")
}
func (p *rescueStrippingProvider) Stream(_ context.Context, req llm.Request) (llm.StreamReader, error) {
	p.mu.Lock()
	p.requests = append(p.requests, req)
	call := len(p.requests)
	p.mu.Unlock()
	if len(req.Tools) > 0 {
		id := fmt.Sprintf("low-%02d", call)
		return &loopRegressionStream{events: []llm.StreamEvent{
			{Type: "tool_use_start", ToolUseID: id, ToolName: "LowOutput"},
			{Type: "tool_input_delta", ToolUseID: id, InputDelta: `{"payload":"tiny"}`},
			{Type: "tool_use_stop", ToolUseID: id},
			{Type: "message_delta", StopReason: "tool_use"},
			{Type: "message_stop"},
		}}, nil
	}
	return textStream("final summary written under the rescue cap"), nil
}

func (p *rescueStrippingProvider) captured() []llm.Request {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]llm.Request(nil), p.requests...)
}

func TestLoopRun_FinalSummaryRescueStripsTools(t *testing.T) {
	provider := &rescueStrippingProvider{}
	registry := tools.NewRegistry()
	registry.Register(lowOutputTool{})
	loop := NewLoop(provider, registry, permission.New(permission.ModeAcceptEdits), nil, "sys", 6)
	loop.GraceCalls = 1
	loop.AppendUser("do a large task")
	out := make(chan Event, 256)
	if err := loop.Run(context.Background(), out); err != nil {
		t.Fatal(err)
	}
	close(out)

	reqs := provider.captured()
	var sawRescueRequest, rescueHadTools bool
	var sawFinalText bool
	for _, req := range reqs {
		hasRescue := requestContains(req, "hit the iteration cap")
		if hasRescue {
			sawRescueRequest = true
			rescueHadTools = len(req.Tools) > 0
		}
	}
	for _, msg := range loop.History() {
		for _, block := range msg.Content {
			if block.Text == "final summary written under the rescue cap" {
				sawFinalText = true
			}
		}
	}
	if !sawRescueRequest {
		t.Fatal("rescue message never reached a provider request")
	}
	if rescueHadTools {
		t.Fatalf("rescue request still carried tools (model could dodge the summary): tools=%d", len(reqs[len(reqs)-1].Tools))
	}
	if !sawFinalText {
		t.Fatal("turn ended without a final text answer after the rescue")
	}

	// The loop must have terminated on the rescue iteration's end_turn, not
	// by silently hitting the stop branch again.
	var stop string
	for ev := range out {
		if ev.Kind == EventLoopDone {
			stop = ev.StopReason
		}
	}
	if stop != "end_turn" {
		t.Fatalf("stop reason = %q, want end_turn (rescue text should close the turn cleanly)", stop)
	}
}
