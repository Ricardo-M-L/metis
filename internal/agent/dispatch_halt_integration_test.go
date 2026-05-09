package agent

// dispatch_halt_integration_test.go covers the integration path
// between a PreToolUse hook returning Halt=true and the loop's halt
// state machine. We don't run the full Loop.Run here (it needs a
// streaming provider) — instead we drive executeBatch directly with
// a registered hook that says "halt". After the call, the loop
// fields (haltRequested / haltReason) must reflect the signal so the
// real Run loop's post-batch check would fire.

import (
	"context"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/tools"
	pubhook "github.com/Ricardo-M-L/metis/pkg/hook"
	pubtool "github.com/Ricardo-M-L/metis/pkg/tool"
)

// haltyTool is a minimal Tool that just echoes back. We never expect
// Execute to run in the halt-path test because the PreToolUse hook
// short-circuits via Output. The struct exists only so the registry
// has a Tool to look up by name.
type haltyTool struct{}

func (haltyTool) Name() string                                   { return "Bash" }
func (haltyTool) Description() string                            { return "test bash" }
func (haltyTool) InputSchema() map[string]any                    { return map[string]any{"type": "object"} }
func (haltyTool) Concurrency(map[string]any) pubtool.Concurrency { return pubtool.ConcurrencySafe }
func (haltyTool) CanUse(context.Context, map[string]any) (pubtool.Permission, string) {
	return pubtool.PermissionAllow, "ok"
}
func (haltyTool) Execute(context.Context, map[string]any) (*pubtool.Result, error) {
	return &pubtool.Result{Output: "should not have run"}, nil
}

// TestDispatch_HaltFromHookFlipsLoopFlag — the canonical integration
// path: a PreToolUse hook returns Halt=true; executeBatch sees it on
// the EmitPreToolUse return value; the dispatch.go branch we added
// calls l.haltTurn; the Loop's haltRequested flag is now true and
// the next Run iteration check would terminate the turn.
func TestDispatch_HaltFromHookFlipsLoopFlag(t *testing.T) {
	reg := tools.NewRegistry()
	reg.Register(haltyTool{})
	hooks := pubhook.NewRegistry()
	hooks.Register(pubhook.PreToolUseHandler(
		func(_ context.Context, _ pubhook.Context, _ *pubhook.PreToolUse) *pubhook.ModifiedPreToolUse {
			return &pubhook.ModifiedPreToolUse{
				Output:     &pubhook.Output{Content: "blocked", IsError: true},
				Halt:       true,
				HaltReason: "test halt from hook",
			}
		},
	))

	l := &Loop{
		Provider: &captureProvider{},
		Registry: reg,
		Gate:     permission.New(permission.ModeAuto),
		Hooks:    hooks,
		Model:    "test-model",
		System:   "test",
	}

	if l.haltRequested {
		t.Fatal("precondition: fresh loop must not have halt set")
	}

	out := make(chan Event, 16)
	defer close(out)

	tc := HookContext{Model: l.Model}
	uses := []llm.ContentBlock{
		{Type: "tool_use", ToolUseID: "tu-1", ToolName: "Bash",
			ToolInput: map[string]any{"command": "ls /"}},
	}
	results, err := l.executeBatch(context.Background(), uses, out, tc)
	if err != nil {
		t.Fatalf("executeBatch returned error: %v", err)
	}
	if len(results) != 1 || !results[0].IsError {
		t.Errorf("expected one error tool_result; got %+v", results)
	}

	// Critical assertion: the loop now knows it must halt.
	if !l.haltRequested {
		t.Errorf("haltRequested should be true after hook returned Halt; got false")
	}
	if l.haltReason != "test halt from hook" {
		t.Errorf("haltReason = %q, want %q", l.haltReason, "test halt from hook")
	}
}

// TestDispatch_NoHaltWhenHookOnlyDenies — a hook that returns Output
// (deny) but Halt=false must NOT raise the halt signal. Pin the
// boundary so a future "always halt on deny" overreach would be
// caught.
func TestDispatch_NoHaltWhenHookOnlyDenies(t *testing.T) {
	reg := tools.NewRegistry()
	reg.Register(haltyTool{})
	hooks := pubhook.NewRegistry()
	hooks.Register(pubhook.PreToolUseHandler(
		func(_ context.Context, _ pubhook.Context, _ *pubhook.PreToolUse) *pubhook.ModifiedPreToolUse {
			return &pubhook.ModifiedPreToolUse{
				Output: &pubhook.Output{Content: "denied", IsError: true},
				// Halt intentionally omitted
			}
		},
	))

	l := &Loop{
		Provider: &captureProvider{},
		Registry: reg,
		Gate:     permission.New(permission.ModeAuto),
		Hooks:    hooks,
		Model:    "test-model",
		System:   "test",
	}
	out := make(chan Event, 16)
	defer close(out)
	uses := []llm.ContentBlock{
		{Type: "tool_use", ToolUseID: "tu-1", ToolName: "Bash",
			ToolInput: map[string]any{"command": "ls /"}},
	}
	if _, err := l.executeBatch(context.Background(), uses, out, HookContext{}); err != nil {
		t.Fatalf("executeBatch err: %v", err)
	}
	if l.haltRequested {
		t.Errorf("plain deny without Halt flag must NOT set halt; got haltRequested=true")
	}
}

// TestRun_ResetsHaltOnEntry — Run must clear any halt signal carried
// over from a previous turn, otherwise a single bad hook could leave
// the entire session permanently halted.
func TestRun_ResetsHaltOnEntry(t *testing.T) {
	l := NewLoop(&captureProvider{}, tools.NewRegistry(),
		permission.New(permission.ModeAuto), nil, "sys", 1)
	l.haltTurn("from previous turn")
	if !l.haltRequested {
		t.Fatal("setup: haltRequested should be true before Run")
	}
	// Note: a real Run drives a streaming provider; captureProvider
	// here returns immediately, which is enough to exercise the
	// entry-point clear without an end-to-end LLM round-trip.
	out := make(chan Event, 64)
	go func() {
		for range out {
		}
	}()
	_ = l.Run(context.Background(), out)
	close(out)
	// After Run, haltRequested must have been cleared at entry. (We
	// don't care whether the Run later set it again — for this pin
	// the entry-clear is the contract.)
	//
	// captureProvider's `Stream` returns a stream that immediately
	// reports stop_reason="end_turn" with no tool calls, so Run
	// returns cleanly without re-raising halt.
	if l.haltRequested {
		t.Errorf("Run should have cleared haltRequested at entry; still true")
	}
	if l.haltReason != "" {
		t.Errorf("haltReason should also be cleared; got %q", l.haltReason)
	}
}
