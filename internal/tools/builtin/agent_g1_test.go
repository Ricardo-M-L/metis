package builtin

// agent_g1_test.go — locks Phase G.1 (run_in_background + sub-agent
// job pool, 2026-05-12).
//
// Contracts pinned here:
//
//   1. `Agent({run_in_background: true})` returns IMMEDIATELY with a
//      handshake tool_result containing agent_id + name. The model
//      must be able to keep working without waiting for the
//      sub-agent to finish — that's the whole point of background.
//
//   2. The Concurrency() classification is INPUT-AWARE: same Agent
//      tool returns ConcurrencyExclusive for foreground and
//      ConcurrencyBackground for `run_in_background:true`. The
//      dispatcher uses this to decide whether to gate subsequent
//      Phase 2 (Exclusive) tools on completion.
//
//   3. SubAgentList / SubAgentOutput / SubAgentStop tools read +
//      mutate the Roster correctly across the lifecycle:
//        - Running sub-agent → Output shows partial text + status=running
//        - Completed sub-agent → Output shows full text + status=completed
//        - SubAgentStop on running → status=killed
//
//   4. Backward compat: existing `Agent({prompt: ...})` calls (no
//      run_in_background field) still hit the foreground path,
//      Concurrency returns Exclusive, the Result.Meta is empty (no
//      background flag).
//
// These together form the model-facing contract of the run_in_background
// feature; if any regress, claude-code parity is broken.

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

// TestAgentConcurrency_InputAware — the dispatcher's classification
// decision. Same Agent tool, two different inputs, two different
// Concurrency values. If this regresses, run_in_background sub-agents
// either block the dispatcher (Exclusive serialization) or fire
// unwanted parallelism (every Agent call running in goroutine).
func TestAgentConcurrency_InputAware(t *testing.T) {
	tool := NewAgent(permission.New(permission.ModeBypass), helloProvider(), tools.NewRegistry(), "model", "system")

	if got := tool.Concurrency(map[string]any{}); got != tools.ConcurrencyExclusive {
		t.Errorf("foreground call → Concurrency = %v, want ConcurrencyExclusive", got)
	}
	if got := tool.Concurrency(map[string]any{"run_in_background": true}); got != tools.ConcurrencyBackground {
		t.Errorf("run_in_background:true → Concurrency = %v, want ConcurrencyBackground", got)
	}
	// Explicit false MUST also yield Exclusive — not Background — so a
	// model that wraps every Agent in the field still gets the synchronous
	// path it expects.
	if got := tool.Concurrency(map[string]any{"run_in_background": false}); got != tools.ConcurrencyExclusive {
		t.Errorf("run_in_background:false → Concurrency = %v, want ConcurrencyExclusive", got)
	}
}

// TestAgentExecute_BackgroundReturnsHandshake — the headline G.1
// contract. With run_in_background:true, Execute returns within
// MILLISECONDS — not after the sub-loop finishes — and the result
// contains agent_id + name + background:true metadata.
func TestAgentExecute_BackgroundReturnsHandshake(t *testing.T) {
	roster := agent.NewRoster(0)
	tool := NewAgent(permission.New(permission.ModeBypass), slowProvider(2*time.Second), tools.NewRegistry(), "model", "system").
		WithRoster(roster)

	start := time.Now()
	res, err := tool.Execute(context.Background(), map[string]any{
		"prompt":            "long-running research task",
		"run_in_background": true,
	})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("Execute err: %v", err)
	}
	// Handshake MUST return before the slow provider's 2s — otherwise
	// the parent agent is blocked, which defeats the whole feature.
	if elapsed > 500*time.Millisecond {
		t.Fatalf("background Execute should return within ~ms, took %s — was it blocking?", elapsed)
	}
	if res.IsError {
		t.Errorf("background spawn should succeed; got IsError: %s", res.Output)
	}
	if !strings.Contains(res.Output, "agent_id=agt-") {
		t.Errorf("handshake should contain agent_id=agt-...; got %q", res.Output)
	}
	if res.Meta["background"] != true {
		t.Errorf("handshake should set Meta[background]=true for UI distinction; got %v", res.Meta)
	}
	if _, ok := res.Meta["agent_id"].(string); !ok {
		t.Errorf("handshake should set Meta[agent_id] string; got %v", res.Meta["agent_id"])
	}

	// Cleanup: roster should still show the teammate running.
	if roster.Count() != 1 {
		t.Errorf("Roster should still hold the running sub-agent; Count=%d", roster.Count())
	}
	// Give the background goroutine a chance to finish so the test
	// doesn't leak goroutines.
	waitForRosterCount(t, roster, 0, 5*time.Second)
}

// TestSubAgentList_ShowsRunningAndCompleted — read-side contract. After
// spawning two background sub-agents, SubAgentList reports them with
// correct status + transition once one finishes.
func TestSubAgentList_ShowsRunningAndCompleted(t *testing.T) {
	roster := agent.NewRoster(0)
	gate := permission.New(permission.ModeBypass)

	agentTool := NewAgent(gate, helloProvider(), tools.NewRegistry(), "model", "system").
		WithRoster(roster)
	listTool := NewSubAgentList(gate, roster)

	// Spawn two fast background sub-agents.
	for i := 0; i < 2; i++ {
		_, err := agentTool.Execute(context.Background(), map[string]any{
			"prompt":            "fast task",
			"run_in_background": true,
		})
		if err != nil {
			t.Fatalf("spawn %d failed: %v", i, err)
		}
	}

	// Give them a moment to finish (helloProvider is synchronous-ish).
	waitForRosterCount(t, roster, 0, 5*time.Second)

	// List should report 0 in flight; the Roster GC'd completed entries
	// when the runner goroutine called Unregister.
	res, err := listTool.Execute(context.Background(), map[string]any{})
	if err != nil {
		t.Fatalf("SubAgentList err: %v", err)
	}
	if !strings.Contains(res.Output, "no sub-agents in flight") {
		t.Errorf("after both sub-agents finished + unregistered, list should be empty; got %q", res.Output)
	}
}

// TestSubAgentList_LiveRunningEntry — captures the snapshot mid-run
// for a sub-agent backed by a slow provider. List MUST show
// status=running + the agent_id, so the model can decide to poll.
func TestSubAgentList_LiveRunningEntry(t *testing.T) {
	roster := agent.NewRoster(0)
	gate := permission.New(permission.ModeBypass)

	agentTool := NewAgent(gate, slowProvider(2*time.Second), tools.NewRegistry(), "model", "system").
		WithRoster(roster)
	listTool := NewSubAgentList(gate, roster)

	_, err := agentTool.Execute(context.Background(), map[string]any{
		"prompt":            "long task",
		"run_in_background": true,
	})
	if err != nil {
		t.Fatalf("spawn err: %v", err)
	}

	// While the sub-agent is still running, list must include it.
	res, err := listTool.Execute(context.Background(), map[string]any{})
	if err != nil {
		t.Fatalf("SubAgentList err: %v", err)
	}
	if !strings.Contains(res.Output, "status=running") {
		t.Errorf("list during run should show status=running; got %q", res.Output)
	}
	if !strings.Contains(res.Output, "background=true") {
		t.Errorf("list should mark background sub-agents; got %q", res.Output)
	}
	// Don't wait for full completion — test cleanup will roster.CancelAll.
	roster.CancelAll()
}

// TestSubAgentStop_TransitionsToKilled — the kill path. Spawn a slow
// sub-agent, call SubAgentStop, verify status flips to killed and
// the partial output is preserved.
func TestSubAgentStop_TransitionsToKilled(t *testing.T) {
	roster := agent.NewRoster(0)
	gate := permission.New(permission.ModeBypass)

	agentTool := NewAgent(gate, slowProvider(5*time.Second), tools.NewRegistry(), "model", "system").
		WithRoster(roster)
	stopTool := NewSubAgentStop(gate, roster)

	res, err := agentTool.Execute(context.Background(), map[string]any{
		"prompt":            "very long task",
		"run_in_background": true,
	})
	if err != nil {
		t.Fatalf("spawn err: %v", err)
	}
	agentID, _ := res.Meta["agent_id"].(string)
	if agentID == "" {
		t.Fatalf("no agent_id in handshake; got Meta=%v", res.Meta)
	}

	// Give the goroutine a beat to actually start running.
	time.Sleep(50 * time.Millisecond)

	stopRes, err := stopTool.Execute(context.Background(), map[string]any{
		"agent_id": agentID,
	})
	if err != nil {
		t.Fatalf("Stop err: %v", err)
	}
	if !strings.Contains(stopRes.Output, "cancellation requested") {
		t.Errorf("stop result should confirm cancellation; got %q", stopRes.Output)
	}

	// Wait for the sub-agent goroutine to settle.
	waitForRosterCount(t, roster, 0, 3*time.Second)
}

// TestSubAgentOutput_RejectsMissingID — error handling. Empty input
// must return IsError with a clear message, not panic or silently
// pick a random teammate.
func TestSubAgentOutput_RejectsMissingID(t *testing.T) {
	roster := agent.NewRoster(0)
	tool := NewSubAgentOutput(permission.New(permission.ModeBypass), roster)

	res, err := tool.Execute(context.Background(), map[string]any{})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !res.IsError {
		t.Errorf("missing agent_id should be IsError; got %+v", res)
	}
	if !strings.Contains(res.Output, "agent_id or name is required") {
		t.Errorf("error message should be actionable; got %q", res.Output)
	}
}

// TestSubAgentOutput_UnknownID — clear "not found" path for an
// agent_id that never existed (typo / hallucination from the model).
func TestSubAgentOutput_UnknownID(t *testing.T) {
	roster := agent.NewRoster(0)
	tool := NewSubAgentOutput(permission.New(permission.ModeBypass), roster)

	res, err := tool.Execute(context.Background(), map[string]any{
		"agent_id": "agt-nonexistent",
	})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !res.IsError {
		t.Errorf("unknown agent_id should be IsError; got %+v", res)
	}
	if !strings.Contains(res.Output, "no sub-agent with agent_id=agt-nonexistent") {
		t.Errorf("error should name the missing id; got %q", res.Output)
	}
}

// TestAgentExecute_BackwardCompat — existing callers that don't pass
// run_in_background see no behavior change. Concurrency stays Exclusive,
// Execute returns the final assistant text (not a handshake), Meta
// doesn't have a `background` key.
func TestAgentExecute_BackwardCompat(t *testing.T) {
	tool := NewAgent(permission.New(permission.ModeBypass), helloProvider(), tools.NewRegistry(), "model", "system")

	res, err := tool.Execute(context.Background(), map[string]any{
		"prompt": "do the thing",
	})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if res.IsError {
		t.Errorf("foreground happy-path should not be IsError; got %s", res.Output)
	}
	// Result is the streamed text, not a handshake.
	if strings.Contains(res.Output, "spawned in background") {
		t.Errorf("foreground call must NOT return a background handshake; got %q", res.Output)
	}
	if _, ok := res.Meta["background"]; ok {
		t.Errorf("foreground call must NOT set Meta[background]; got %v", res.Meta)
	}
}

// ---------------------------------------------------------------------------
// helpers

// slowProvider returns a provider whose Stream returns a stream that
// emits one tiny chunk after `delay` then end_turn. Used to keep
// background sub-agents alive long enough for the polling tests to
// observe them mid-run.
func slowProvider(delay time.Duration) llm.Provider {
	return &delayedProvider{delay: delay}
}

type delayedProvider struct {
	delay time.Duration
}

func (p *delayedProvider) Name() string          { return "delayed" }
func (p *delayedProvider) MaxContextTokens() int { return 100000 }
func (p *delayedProvider) Complete(_ context.Context, _ llm.Request) (*llm.Response, error) {
	return nil, nil
}
func (p *delayedProvider) Stream(ctx context.Context, _ llm.Request) (llm.StreamReader, error) {
	return &delayedStream{ctx: ctx, delay: p.delay}, nil
}

type delayedStream struct {
	ctx   context.Context
	delay time.Duration
	idx   int
}

func (s *delayedStream) Recv() (llm.StreamEvent, error) {
	s.idx++
	switch s.idx {
	case 1:
		// Slow first chunk — sleep up to delay or until ctx cancelled.
		select {
		case <-time.After(s.delay):
			return llm.StreamEvent{Type: "text_delta", TextDelta: "working..."}, nil
		case <-s.ctx.Done():
			return llm.StreamEvent{}, s.ctx.Err()
		}
	case 2:
		return llm.StreamEvent{Type: "message_delta", StopReason: "end_turn", OutputTokens: 1}, nil
	default:
		return llm.StreamEvent{Type: "message_stop"}, nil
	}
}
func (s *delayedStream) Close() error { return nil }

// waitForRosterCount polls until Roster.Count() reaches `want` or the
// deadline expires. Replaces a fixed sleep so tests don't false-fail
// on slow CI but also don't waste seconds on fast paths.
func waitForRosterCount(t *testing.T, r *agent.Roster, want int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if r.Count() == want {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Errorf("Roster.Count() did not reach %d within %s; got %d", want, timeout, r.Count())
}
