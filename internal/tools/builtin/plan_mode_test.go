package builtin

// plan_mode_test.go — covers the EnterPlanMode / ExitPlanMode tools
// added 2026-05-15. The pre-fix bug: internal/slash/batch_prompt.go
// instructed the model to "Call ExitPlanMode" but no such tool was
// registered, so the batch-mode plan handshake silently never worked.
// These tests pin:
//
//   1. The tools' schemas and required fields match what the prompt
//      asks the model to call.
//   2. ExitPlanMode without context controller returns IsError (it
//      shouldn't panic; the prompt promises a graceful response).
//   3. With a fake PlanController in context, EnterPlanMode flips
//      the flag on and ExitPlanMode flips it back off.
//   4. ExitPlanMode emits a [plan proposal] EventInfo through the
//      event channel from context.
//   5. ExitPlanMode rejects empty / whitespace-only plan input.

import (
	"context"
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/agent"
)

// stubPlanController is a minimal PlanController for tests — we just
// want to observe the bool state without bringing up a real Loop.
type stubPlanController struct {
	on bool
}

func (s *stubPlanController) SetPlanMode(v bool) { s.on = v }

func TestEnterPlanMode_FlipsControllerOn(t *testing.T) {
	t.Parallel()
	ctrl := &stubPlanController{}
	ctx := agent.WithPlanController(context.Background(), ctrl)
	tool := NewEnterPlanMode()
	res, err := tool.Execute(ctx, map[string]any{})
	if err != nil {
		t.Fatalf("Execute err: %v", err)
	}
	if res.IsError {
		t.Errorf("EnterPlanMode should not be IsError: %s", res.Output)
	}
	if !ctrl.on {
		t.Error("EnterPlanMode did not flip controller on")
	}
}

func TestEnterPlanMode_GracefulWithoutController(t *testing.T) {
	t.Parallel()
	// Bare context — no controller attached. Tool should return an
	// IsError result, NOT panic.
	tool := NewEnterPlanMode()
	res, err := tool.Execute(context.Background(), map[string]any{})
	if err != nil {
		t.Fatalf("Execute returned err (expected IsError result, no err): %v", err)
	}
	if !res.IsError {
		t.Errorf("missing controller should produce IsError; got: %s", res.Output)
	}
}

func TestExitPlanMode_FlipsControllerOff_AndEmitsPlan(t *testing.T) {
	t.Parallel()
	// Pre-set the controller to "on" so we can observe the flip.
	ctrl := &stubPlanController{on: true}
	events := make(chan agent.Event, 4)
	ctx := agent.WithPlanController(context.Background(), ctrl)
	ctx = agent.WithEventOut(ctx, events)
	tool := NewExitPlanMode()
	plan := "## My Plan\n1. Edit foo.go\n2. Run tests\n3. Commit"
	res, err := tool.Execute(ctx, map[string]any{"plan": plan})
	if err != nil {
		t.Fatalf("Execute err: %v", err)
	}
	if res.IsError {
		t.Errorf("ExitPlanMode should not be IsError: %s", res.Output)
	}
	if ctrl.on {
		t.Error("ExitPlanMode did not flip controller off")
	}
	// One EventInfo with the plan should have landed on the channel.
	select {
	case ev := <-events:
		if ev.Kind != agent.EventInfo {
			t.Errorf("expected EventInfo, got Kind=%d", ev.Kind)
		}
		if !strings.Contains(ev.Info, "[plan proposal]") {
			t.Errorf("missing [plan proposal] marker: %q", ev.Info)
		}
		if !strings.Contains(ev.Info, "Edit foo.go") {
			t.Errorf("plan body not echoed: %q", ev.Info)
		}
	default:
		t.Error("expected EventInfo on event channel; none emitted")
	}
}

func TestExitPlanMode_RejectsEmptyPlan(t *testing.T) {
	t.Parallel()
	ctrl := &stubPlanController{on: true}
	ctx := agent.WithPlanController(context.Background(), ctrl)
	tool := NewExitPlanMode()
	cases := []map[string]any{
		{},                    // missing
		{"plan": ""},          // empty
		{"plan": "   \n\t  "}, // whitespace only
		{"plan": nil},         // explicit nil
		{"plan": 42},          // wrong type — string assert fails, falls through to empty
	}
	for i, in := range cases {
		_, err := tool.Execute(ctx, in)
		if err == nil {
			t.Errorf("case %d: empty plan should return err; in=%v", i, in)
		}
	}
	// Controller must NOT be flipped on the error path.
	if !ctrl.on {
		t.Error("controller flipped off despite error — should only flip on success")
	}
}

func TestExitPlanMode_NoEventChannel_StillFlipsFlag(t *testing.T) {
	t.Parallel()
	// Verify the tool degrades gracefully when no event channel is
	// attached — the flag flip must still happen so the loop's plan
	// gate releases on the next iteration.
	ctrl := &stubPlanController{on: true}
	ctx := agent.WithPlanController(context.Background(), ctrl)
	tool := NewExitPlanMode()
	res, err := tool.Execute(ctx, map[string]any{"plan": "do something"})
	if err != nil {
		t.Fatalf("Execute err: %v", err)
	}
	if res.IsError {
		t.Errorf("should not be IsError: %s", res.Output)
	}
	if ctrl.on {
		t.Error("controller not flipped off when event channel absent")
	}
}

// TestPlanModeTools_SchemaAndNames pins the contract the loop and
// the batch_prompt.go template rely on.
func TestPlanModeTools_SchemaAndNames(t *testing.T) {
	t.Parallel()
	enter := NewEnterPlanMode()
	exit := NewExitPlanMode()
	if enter.Name() != "EnterPlanMode" {
		t.Errorf("EnterPlanMode.Name() = %q, want EnterPlanMode", enter.Name())
	}
	if exit.Name() != "ExitPlanMode" {
		t.Errorf("ExitPlanMode.Name() = %q, want ExitPlanMode", exit.Name())
	}
	// ExitPlanMode must require a `plan` arg — that's what the
	// batch_prompt.go template promises the model.
	schema := exit.InputSchema()
	required, _ := schema["required"].([]string)
	hasPlan := false
	for _, k := range required {
		if k == "plan" {
			hasPlan = true
		}
	}
	if !hasPlan {
		t.Errorf("ExitPlanMode schema missing required `plan`: %v", schema)
	}
}
