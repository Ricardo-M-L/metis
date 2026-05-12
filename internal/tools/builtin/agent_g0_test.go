package builtin

// agent_g0_test.go — locks Phase G.0 (cross-cutting safety net).
//
// Two contracts pinned here:
//
//   1. **Concurrency cap**: a Roster with capacity N refuses the
//      (N+1)th Register, and the Agent tool surfaces that refusal as
//      an IsError result (not a panic, not a silent skip) before
//      booting the sub-loop. The error message must name the cap so
//      the parent model can decide whether to back off vs. ask for
//      a larger budget.
//
//   2. **Wall-clock timeout**: when `timeout_seconds` is passed (or
//      the default from config is set), a sub-agent that exceeds
//      that budget terminates with a user-recognizable timeout
//      message — claude-code parity (their AgentTool returns a
//      similar "timed out after Xs" string).
//
// If a refactor breaks either contract, the fork-bomb / runaway-task
// protection silently regresses — which is the exact failure mode
// the user complained about on 2026-05-12 when reviewing claude-code's
// run_in_background semantics.

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

// TestAgentTool_CapacityCapEnforced — Roster with capacity 1, pre-fill
// it manually with one teammate, then try to launch a real sub-agent
// via the tool. Must return IsError with "capacity exceeded".
func TestAgentTool_CapacityCapEnforced(t *testing.T) {
	roster := agent.NewRoster(1)
	// Occupy the only slot — register a synthetic teammate so the
	// Agent tool's slot is already taken when it tries to register.
	if err := roster.Register(&agent.Teammate{Name: "_held"}); err != nil {
		t.Fatalf("preload Register failed: %v", err)
	}

	gate := permission.New(permission.ModeBypass)
	tool := NewAgent(gate, helloProvider(), tools.NewRegistry(), "model", "system").
		WithRoster(roster)

	res, err := tool.Execute(context.Background(), map[string]any{"prompt": "should be rejected"})
	if err != nil {
		t.Fatalf("Execute should return tool result (not error) for cap rejection; got err=%v", err)
	}
	if !res.IsError {
		t.Fatalf("cap rejection must set IsError; got Output=%q", res.Output)
	}
	if !strings.Contains(res.Output, "capacity exceeded") {
		t.Errorf("cap message should mention 'capacity exceeded' for the parent model to recognize; got %q", res.Output)
	}
	if !strings.Contains(res.Output, "1/1") {
		t.Errorf("cap message should show the in-flight/total counts so the operator sees the actual budget; got %q", res.Output)
	}
	// Roster count must still be 1 — the rejected attempt didn't
	// leave a dangling entry.
	if roster.Count() != 1 {
		t.Errorf("rejected sub-agent should not pollute the Roster; Count=%d, want 1", roster.Count())
	}
}

// TestAgentTool_CapacityCapAllowsWhenSlotsFree — counter-case: an
// empty Roster lets the sub-agent run normally + unregisters on exit
// so the next call gets the slot back. Without this we'd never be
// able to chain sub-agents at all.
func TestAgentTool_CapacityCapAllowsWhenSlotsFree(t *testing.T) {
	roster := agent.NewRoster(2)
	gate := permission.New(permission.ModeBypass)
	tool := NewAgent(gate, helloProvider(), tools.NewRegistry(), "model", "system").
		WithRoster(roster)

	res, err := tool.Execute(context.Background(), map[string]any{"prompt": "go"})
	if err != nil {
		t.Fatalf("Execute err: %v", err)
	}
	if res.IsError {
		t.Errorf("with slots free, sub-agent should succeed; got IsError: %s", res.Output)
	}
	// After the sub-agent finishes, the slot must be released.
	if roster.Count() != 0 {
		t.Errorf("sub-agent should unregister on exit; Count=%d, want 0", roster.Count())
	}
}

// TestAgentTool_TimeoutFromInputArg — sub-agent that hangs on a
// never-returning provider must trip `timeout_seconds=1` and surface
// the timeout text. Uses a provider that blocks on Recv() until ctx
// is cancelled (which is exactly what a real network read would do).
func TestAgentTool_TimeoutFromInputArg(t *testing.T) {
	tool := NewAgent(permission.New(permission.ModeBypass), &hangingProvider{}, tools.NewRegistry(), "model", "system")

	start := time.Now()
	res, err := tool.Execute(context.Background(), map[string]any{
		"prompt":          "hang forever",
		"timeout_seconds": 1,
	})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("Execute err: %v", err)
	}
	if !res.IsError {
		t.Fatalf("timeout must set IsError; got Output=%q", res.Output)
	}
	if !strings.Contains(res.Output, "timed out") {
		t.Errorf("timeout result must say 'timed out' so the model knows to retry with a larger budget; got %q", res.Output)
	}
	// The wall-clock must be near 1s — if it's > 5s, the cancel
	// pipeline failed and Execute leaked into a long wait.
	if elapsed > 5*time.Second {
		t.Errorf("timeout=1s should land near 1s, not %s — cancel may have leaked", elapsed)
	}
}

// TestAgentTool_TimeoutFromDefaultConfig — when the input arg is
// missing, the Agent tool's defaultTimeout (set from
// config.Agents.DefaultTimeoutSeconds at wiring time) applies.
// Verifies the path runtime/tools.go uses, so a misconfigured default
// would surface here before users hit it in production.
func TestAgentTool_TimeoutFromDefaultConfig(t *testing.T) {
	tool := NewAgent(permission.New(permission.ModeBypass), &hangingProvider{}, tools.NewRegistry(), "model", "system").
		WithDefaultTimeout(1 * time.Second)

	res, err := tool.Execute(context.Background(), map[string]any{
		"prompt": "hang forever",
		// no timeout_seconds — should fall back to defaultTimeout
	})
	if err != nil {
		t.Fatalf("Execute err: %v", err)
	}
	if !res.IsError || !strings.Contains(res.Output, "timed out") {
		t.Errorf("default-timeout path should land same as input-arg path; got %+v", res)
	}
}

// TestAgentTool_TimeoutZeroDisabled — passing `timeout_seconds: 0`
// explicitly should NOT cap the sub-agent. Otherwise users couldn't
// opt out for legitimately long tasks. Verify by using a fast
// helloProvider — if 0 wrongly triggered cancellation we'd see an
// IsError.
func TestAgentTool_TimeoutZeroDisabled(t *testing.T) {
	tool := NewAgent(permission.New(permission.ModeBypass), helloProvider(), tools.NewRegistry(), "model", "system").
		WithDefaultTimeout(1 * time.Second)

	res, err := tool.Execute(context.Background(), map[string]any{
		"prompt":          "fast task",
		"timeout_seconds": 0,
	})
	if err != nil {
		t.Fatalf("Execute err: %v", err)
	}
	if res.IsError {
		t.Errorf("timeout_seconds=0 must disable the cap (override default); got IsError: %s", res.Output)
	}
}

// hangingProvider is a Provider that simulates a hung network read —
// Recv() blocks until the context is cancelled. Used to exercise the
// timeout path without flaky real-network tests.
type hangingProvider struct{}

func (p *hangingProvider) Name() string          { return "hanging" }
func (p *hangingProvider) MaxContextTokens() int { return 100000 }
func (p *hangingProvider) Complete(ctx context.Context, _ llm.Request) (*llm.Response, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}
func (p *hangingProvider) Stream(ctx context.Context, _ llm.Request) (llm.StreamReader, error) {
	return &hangingStream{ctx: ctx}, nil
}

type hangingStream struct {
	ctx context.Context
}

func (s *hangingStream) Recv() (llm.StreamEvent, error) {
	<-s.ctx.Done()
	return llm.StreamEvent{}, s.ctx.Err()
}
func (s *hangingStream) Close() error { return nil }
