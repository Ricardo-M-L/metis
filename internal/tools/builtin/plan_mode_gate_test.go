package builtin

// Plan-mode gate-bridge tests — 2026-05-18 audit follow-up. Covers
// the production wiring where EnterPlanMode/ExitPlanMode flip the
// Gate and the listener propagates to Loop.PlanMode, and ExitPlanMode
// restores the user's pre-plan posture instead of leaving Gate stuck
// in plan (which used to re-trigger the deny-storm on the next turn).

import (
	"context"
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

func withPlanApproval(ctx context.Context, answer string) context.Context {
	events := make(chan agent.Event, 4)
	go func() {
		for ev := range events {
			if ev.Kind == agent.EventAskUser && ev.AskUserReply != nil {
				ev.AskUserReply <- answer
				return
			}
		}
	}()
	return agent.WithEventOut(ctx, events)
}

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

func TestEnterPlanMode_CanUseRequiresApproval(t *testing.T) {
	g := permission.New(permission.ModeDefault)
	tool := NewEnterPlanModeWithGate(g)

	perm, reason := tool.CanUse(context.Background(), nil)
	if perm != tools.PermissionAsk {
		t.Fatalf("CanUse permission = %v, want ASK", perm)
	}
	if !strings.Contains(reason, "approval") {
		t.Fatalf("CanUse reason = %q, want approval explanation", reason)
	}
}

func TestEnterPlanMode_CanUseBypassDoesNotAsk(t *testing.T) {
	g := permission.New(permission.ModeBypassPermissions)
	tool := NewEnterPlanModeWithGate(g)

	perm, reason := tool.CanUse(context.Background(), nil)
	if perm != tools.PermissionAllow {
		t.Fatalf("CanUse permission = %v (%s), want ALLOW in bypassPermissions", perm, reason)
	}
}

func TestEnterPlanMode_CanUseAlreadyPlanningIsIdempotent(t *testing.T) {
	g := permission.New(permission.ModePlan)
	tool := NewEnterPlanModeWithGate(g)

	perm, _ := tool.CanUse(context.Background(), nil)
	if perm != tools.PermissionAllow {
		t.Fatalf("redundant EnterPlanMode permission = %v, want ALLOW", perm)
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
	ctx = withPlanApproval(ctx, "Yes, auto-accept edits")

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

func TestExitPlanMode_BypassPrePlanModeAutoApprovesWithoutUI(t *testing.T) {
	g := permission.New(permission.ModePlan)
	tool := NewExitPlanModeWithGate(g)
	ctrl := &stubPlanController{pre: string(permission.ModeBypassPermissions)}
	ctx := agent.WithPlanController(context.Background(), ctrl)

	res, err := tool.Execute(ctx, map[string]any{"plan": "step 1: do X"})
	if err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}
	if res == nil || res.IsError {
		t.Fatalf("bypass plan exit should auto-approve, got %+v", res)
	}
	if g.Mode() != permission.ModeBypassPermissions {
		t.Fatalf("Gate mode = %q, want bypassPermissions", g.Mode())
	}
	if ctrl.PrePlanMode() != "" {
		t.Fatalf("pre-plan snapshot = %q, want cleared", ctrl.PrePlanMode())
	}
}

func TestBypassPlanLineageEndsAfterManualModeChange(t *testing.T) {
	g := permission.New(permission.ModeDefault)
	ctrl := &stubPlanController{pre: string(permission.ModeBypassPermissions)}
	ctx := agent.WithPlanController(context.Background(), ctrl)

	if isBypassUnattendedLineage(ctx, g) {
		t.Fatal("stale pre-plan snapshot treated default mode as unattended")
	}
	exit := NewExitPlanModeWithGate(g)
	res, err := exit.Execute(ctx, map[string]any{"plan": "step 1"})
	if err != nil || res == nil || !res.IsError {
		t.Fatalf("stale ExitPlanMode = (%+v, %v), want structured error", res, err)
	}
	if ctrl.PrePlanMode() != "" || g.Mode() != permission.ModeDefault {
		t.Fatalf("stale plan state not cleared: pre=%q mode=%q", ctrl.PrePlanMode(), g.Mode())
	}
}

func TestExitPlanMode_WithGate_NoSnapshotFallsBackToAsk(t *testing.T) {
	// If no prePlanMode was captured (model called ExitPlanMode
	// without a prior EnterPlanMode, or loop restarted), we don't
	// want to leave Gate stuck in plan. Default to ModeAsk — the
	// safest non-plan posture after manual approval.
	g := permission.New(permission.ModePlan)
	tool := NewExitPlanModeWithGate(g)
	ctrl := &stubPlanController{pre: ""}
	ctx := agent.WithPlanController(context.Background(), ctrl)
	ctx = withPlanApproval(ctx, "Yes, manually approve edits")

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
	ctx = withPlanApproval(ctx, "Yes, manually approve edits")

	_, _ = tool.Execute(ctx, map[string]any{"plan": "step 1"})
	if g.Mode() != permission.ModeAsk {
		t.Errorf("ExitPlanMode with plan-snapshot should fall back to ModeAsk; got %q", g.Mode())
	}
}

func TestExitPlanMode_WithGate_RejectionKeepsPlanning(t *testing.T) {
	g := permission.New(permission.ModePlan)
	tool := NewExitPlanModeWithGate(g)
	ctrl := &stubPlanController{on: true, pre: string(permission.ModeAcceptEdits)}
	ctx := agent.WithPlanController(context.Background(), ctrl)
	ctx = withPlanApproval(ctx, "No, keep planning")

	res, err := tool.Execute(ctx, map[string]any{"plan": "step 1: inspect"})
	if err != nil || res.IsError {
		t.Fatalf("Execute: err=%v res=%+v", err, res)
	}
	if g.Mode() != permission.ModePlan || !ctrl.on {
		t.Fatalf("rejection must keep plan mode active: gate=%q controller=%v", g.Mode(), ctrl.on)
	}
	if ctrl.PrePlanMode() != string(permission.ModeAcceptEdits) {
		t.Fatalf("rejection must preserve pre-plan mode, got %q", ctrl.PrePlanMode())
	}
}

func TestExitPlanMode_WithGate_HeadlessCannotApprove(t *testing.T) {
	g := permission.New(permission.ModePlan)
	tool := NewExitPlanModeWithGate(g)
	ctrl := &stubPlanController{on: true, pre: string(permission.ModeDefault)}
	ctx := agent.WithPlanController(context.Background(), ctrl)

	res, err := tool.Execute(ctx, map[string]any{"plan": "step 1: inspect"})
	if err != nil {
		t.Fatalf("Execute returned Go error: %v", err)
	}
	if !res.IsError || g.Mode() != permission.ModePlan || !ctrl.on {
		t.Fatalf("headless exit must fail closed in plan mode: res=%+v gate=%q controller=%v", res, g.Mode(), ctrl.on)
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

	approvedCtx := withPlanApproval(ctx, "Yes, auto-accept edits")
	_, _ = exit.Execute(approvedCtx, map[string]any{"plan": "do it"})
	if g.Mode() != permission.ModeAcceptEdits || ctrl.on {
		t.Errorf("after exit: gate=%q controller.on=%v (want acceptEdits/false)", g.Mode(), ctrl.on)
	}
}
