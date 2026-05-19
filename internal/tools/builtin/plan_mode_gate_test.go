package builtin

// Plan-mode gate-bridge tests — 2026-05-18 audit follow-up. Covers
// the production wiring where EnterPlanMode/ExitPlanMode flip the
// Gate and the listener propagates to Loop.PlanMode, and ExitPlanMode
// restores the user's pre-plan posture instead of leaving Gate stuck
// in plan (which used to re-trigger the deny-storm on the next turn).

import (
	"context"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/permission"
)

func TestEnterPlanMode_WithGate_FlipsGateToPlan(t *testing.T) {
	g := permission.New(permission.ModeAcceptEdits)
	tool := NewEnterPlanModeWithGate(g)
	ctrl := &stubPlanController{}
	ctx := agent.WithPlanController(context.Background(), ctrl)

	_, err := tool.Execute(ctx, map[string]any{})
	if err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}
	if g.Mode() != permission.ModePlan {
		t.Errorf("Gate should be ModePlan after EnterPlanMode; got %q", g.Mode())
	}
}

func TestEnterPlanMode_WithGate_CapturesPrePlanMode(t *testing.T) {
	g := permission.New(permission.ModeAcceptEdits)
	tool := NewEnterPlanModeWithGate(g)
	ctrl := &stubPlanController{}
	ctx := agent.WithPlanController(context.Background(), ctrl)

	_, _ = tool.Execute(ctx, map[string]any{})
	if ctrl.PrePlanMode() != string(permission.ModeAcceptEdits) {
		t.Errorf("PrePlanMode should snapshot prior gate mode (%q); got %q",
			permission.ModeAcceptEdits, ctrl.PrePlanMode())
	}
}

func TestEnterPlanMode_WithGate_NoDoubleSnapshot(t *testing.T) {
	// If we're already in plan mode (e.g., user Shift+Tab'd here),
	// EnterPlanMode shouldn't overwrite a pre-existing prePlanMode
	// snapshot with "plan" — that would lose the user's true posture.
	g := permission.New(permission.ModePlan)
	tool := NewEnterPlanModeWithGate(g)
	ctrl := &stubPlanController{pre: string(permission.ModeAcceptEdits)}
	ctx := agent.WithPlanController(context.Background(), ctrl)

	_, _ = tool.Execute(ctx, map[string]any{})
	if ctrl.PrePlanMode() != string(permission.ModeAcceptEdits) {
		t.Errorf("entering plan from plan should NOT clobber prePlanMode; got %q", ctrl.PrePlanMode())
	}
}

func TestExitPlanMode_WithGate_RestoresPrePlanMode(t *testing.T) {
	g := permission.New(permission.ModePlan)
	tool := NewExitPlanModeWithGate(g)
	ctrl := &stubPlanController{pre: string(permission.ModeAcceptEdits)}
	ctx := agent.WithPlanController(context.Background(), ctrl)

	_, err := tool.Execute(ctx, map[string]any{"plan": "step 1: do X"})
	if err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}
	if g.Mode() != permission.ModeAcceptEdits {
		t.Errorf("Gate should restore to ModeAcceptEdits after ExitPlanMode; got %q", g.Mode())
	}
	if ctrl.PrePlanMode() != "" {
		t.Errorf("PrePlanMode should be cleared after restore; got %q", ctrl.PrePlanMode())
	}
}

func TestExitPlanMode_WithGate_NoSnapshotFallsBackToAsk(t *testing.T) {
	// If no prePlanMode was captured (model called ExitPlanMode
	// without a prior EnterPlanMode, or loop restarted), we don't
	// want to leave Gate stuck in plan. Default to ModeAsk — the
	// safest non-plan posture.
	g := permission.New(permission.ModePlan)
	tool := NewExitPlanModeWithGate(g)
	ctrl := &stubPlanController{pre: ""}
	ctx := agent.WithPlanController(context.Background(), ctrl)

	_, _ = tool.Execute(ctx, map[string]any{"plan": "step 1: do X"})
	if g.Mode() != permission.ModeAsk {
		t.Errorf("ExitPlanMode without snapshot should fall back to ModeAsk; got %q", g.Mode())
	}
}

func TestExitPlanMode_WithGate_PlanSnapshotFallsBackToAsk(t *testing.T) {
	// If somehow prePlanMode == "plan" (defensive — should never
	// happen given EnterPlanMode's no-double-snapshot guard, but
	// belt-and-suspenders), still fall back to ModeAsk rather than
	// re-entering plan.
	g := permission.New(permission.ModePlan)
	tool := NewExitPlanModeWithGate(g)
	ctrl := &stubPlanController{pre: string(permission.ModePlan)}
	ctx := agent.WithPlanController(context.Background(), ctrl)

	_, _ = tool.Execute(ctx, map[string]any{"plan": "step 1"})
	if g.Mode() != permission.ModeAsk {
		t.Errorf("ExitPlanMode with plan-snapshot should fall back to ModeAsk; got %q", g.Mode())
	}
}

// Verifies the Gate→listener→Loop bridge end-to-end through the tool.
func TestEnterPlanMode_WithGateAndListener_FlipsControllerOn(t *testing.T) {
	g := permission.New(permission.ModeAcceptEdits)
	ctrl := &stubPlanController{}
	g.SetModeChangeListener(func(m permission.Mode) {
		ctrl.SetPlanMode(m == permission.ModePlan)
	})

	tool := NewEnterPlanModeWithGate(g)
	ctx := agent.WithPlanController(context.Background(), ctrl)

	_, _ = tool.Execute(ctx, map[string]any{})
	if !ctrl.on {
		t.Error("listener should have flipped controller.on to true after gate.SetMode(plan)")
	}
}

// Round-trip: enter then exit, both gate and controller should be back.
func TestPlanMode_RoundTrip(t *testing.T) {
	g := permission.New(permission.ModeAcceptEdits)
	ctrl := &stubPlanController{}
	g.SetModeChangeListener(func(m permission.Mode) {
		ctrl.SetPlanMode(m == permission.ModePlan)
	})

	enter := NewEnterPlanModeWithGate(g)
	exit := NewExitPlanModeWithGate(g)
	ctx := agent.WithPlanController(context.Background(), ctrl)

	_, _ = enter.Execute(ctx, map[string]any{})
	if g.Mode() != permission.ModePlan || !ctrl.on {
		t.Fatalf("after enter: gate=%q controller.on=%v (want plan/true)", g.Mode(), ctrl.on)
	}

	_, _ = exit.Execute(ctx, map[string]any{"plan": "do it"})
	if g.Mode() != permission.ModeAcceptEdits || ctrl.on {
		t.Errorf("after exit: gate=%q controller.on=%v (want acceptEdits/false)", g.Mode(), ctrl.on)
	}
}
