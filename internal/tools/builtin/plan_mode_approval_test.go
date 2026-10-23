package builtin

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/permission"
)

func TestExitPlanMode_AllOriginsRequireInteractiveApproval(t *testing.T) {
	for _, prior := range []permission.Mode{permission.ModeDefault, permission.ModeAcceptEdits, permission.ModeBypassPermissions, permission.ModeFullAccess} {
		t.Run(string(prior), func(t *testing.T) {
			g := permission.New(permission.ModePlan)
			ctrl := &stubPlanController{on: true, pre: string(prior)}
			ctx := agent.WithPlanController(context.Background(), ctrl)
			res, err := NewExitPlanModeWithGate(g).Execute(ctx, map[string]any{"plan": "Create the proposed file only after my approval."})
			if err != nil || res == nil || !res.IsError {
				t.Fatalf("headless exit must fail closed, got (%+v, %v)", res, err)
			}
			if g.Mode() != permission.ModePlan || !ctrl.on || ctrl.pre != string(prior) {
				t.Fatalf("unapproved exit changed permission state: gate=%s plan=%v pre=%q", g.Mode(), ctrl.on, ctrl.pre)
			}
		})
	}
}

func TestExitPlanMode_ExplicitApprovalNamesAndRestoresPermission(t *testing.T) {
	cases := []struct {
		prior permission.Mode
		label string
		want  permission.Mode
	}{
		{permission.ModeDefault, "Yes, auto-accept edits", permission.ModeAcceptEdits},
		{permission.ModeAcceptEdits, "Yes, auto-accept edits", permission.ModeAcceptEdits},
		{permission.ModeBypassPermissions, "Yes, bypass permissions", permission.ModeBypassPermissions},
		{permission.ModeFullAccess, "Yes, full access (no sandbox)", permission.ModeFullAccess},
	}
	for _, tc := range cases {
		t.Run(string(tc.prior), func(t *testing.T) {
			g := permission.New(permission.ModePlan)
			ctrl := &stubPlanController{on: true, pre: string(tc.prior)}
			g.SetModeChangeListener(func(mode permission.Mode) { ctrl.SetPlanMode(mode == permission.ModePlan) })
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			ctx = agent.WithPlanController(ctx, ctrl)
			events := make(chan agent.Event, 4)
			ctx = agent.WithEventOut(ctx, events)
			checked := make(chan error, 1)
			go func() {
				for {
					select {
					case ev := <-events:
						if ev.Kind != agent.EventAskUser {
							continue
						}
						if len(ev.AskUserOptions) != 3 || ev.AskUserOptions[0] != tc.label || ev.AskUserOptions[1] != "Yes, manually approve edits" || ev.AskUserOptions[2] != "No, keep planning" {
							checked <- &approvalOptionsError{ev.AskUserOptions}
							ev.AskUserReply <- "No, keep planning"
							return
						}
						if g.Mode() != permission.ModePlan {
							checked <- &approvalOptionsError{[]string{"gate changed before approval", string(g.Mode())}}
							ev.AskUserReply <- "No, keep planning"
							return
						}
						checked <- nil
						ev.AskUserReply <- tc.label
						return
					case <-ctx.Done():
						return
					}
				}
			}()
			res, err := NewExitPlanModeWithGate(g).Execute(ctx, map[string]any{"plan": "Create a file after explicit approval."})
			select {
			case promptErr := <-checked:
				if promptErr != nil {
					t.Fatal(promptErr)
				}
			default:
				t.Fatal("ExitPlanMode did not ask for explicit approval")
			}
			if err != nil || res == nil || res.IsError || g.Mode() != tc.want || ctrl.on || ctrl.pre != "" {
				t.Fatalf("approved exit mismatch: res=%+v err=%v gate=%s plan=%v pre=%q", res, err, g.Mode(), ctrl.on, ctrl.pre)
			}
		})
	}
}

type approvalOptionsError struct{ options []string }

func (e *approvalOptionsError) Error() string {
	return "unexpected approval choices or state: " + strings.Join(e.options, "; ")
}

func TestExitPlanMode_UnapprovedOutcomesKeepPlanForEveryOrigin(t *testing.T) {
	for _, prior := range []permission.Mode{permission.ModeDefault, permission.ModeAcceptEdits, permission.ModeBypassPermissions, permission.ModeFullAccess} {
		for _, outcome := range []string{"reject", "unrecognized", "closed", "cancelled", "deadline"} {
			t.Run(string(prior)+"/"+outcome, func(t *testing.T) {
				g := permission.New(permission.ModePlan)
				ctrl := &stubPlanController{on: true, pre: string(prior)}
				ctx, cancel := context.WithTimeout(context.Background(), time.Second)
				defer cancel()
				if outcome == "deadline" {
					var deadlineCancel context.CancelFunc
					ctx, deadlineCancel = context.WithTimeout(ctx, 15*time.Millisecond)
					defer deadlineCancel()
				}
				ctx = agent.WithPlanController(ctx, ctrl)
				events := make(chan agent.Event, 4)
				ctx = agent.WithEventOut(ctx, events)
				asked := make(chan struct{}, 1)
				go func() {
					for {
						select {
						case ev := <-events:
							if ev.Kind != agent.EventAskUser {
								continue
							}
							asked <- struct{}{}
							switch outcome {
							case "reject":
								ev.AskUserReply <- "No, keep planning"
							case "unrecognized":
								ev.AskUserReply <- "Switch to Rust"
							case "closed":
								close(ev.AskUserReply)
							case "cancelled":
								cancel()
							}
							return
						case <-ctx.Done():
							return
						}
					}
				}()
				res, err := NewExitPlanModeWithGate(g).Execute(ctx, map[string]any{"plan": "Propose a change without authorizing implementation."})
				select {
				case <-asked:
				default:
					t.Fatal("ExitPlanMode bypassed the approval prompt")
				}
				if err != nil || res == nil || g.Mode() != permission.ModePlan || !ctrl.on || ctrl.pre != string(prior) {
					t.Fatalf("unapproved %s changed state: res=%+v err=%v gate=%s plan=%v pre=%q", outcome, res, err, g.Mode(), ctrl.on, ctrl.pre)
				}
			})
		}
	}
}

func TestExitPlanMode_ManualApprovalAlwaysUsesDefault(t *testing.T) {
	for _, prior := range []permission.Mode{permission.ModeBypassPermissions, permission.ModeFullAccess} {
		t.Run(string(prior), func(t *testing.T) {
			g := permission.New(permission.ModePlan)
			ctrl := &stubPlanController{on: true, pre: string(prior)}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			ctx = agent.WithPlanController(ctx, ctrl)
			ctx = withPlanApproval(ctx, "Yes, manually approve edits")
			res, err := NewExitPlanModeWithGate(g).Execute(ctx, map[string]any{"plan": "Only proceed with per-change approval."})
			if err != nil || res == nil || res.IsError || g.Mode() != permission.ModeDefault || ctrl.pre != "" {
				t.Fatalf("manual approval escalated permissions: res=%+v err=%v gate=%s pre=%q", res, err, g.Mode(), ctrl.pre)
			}
		})
	}
}

func TestExitPlanMode_CancelledContextCannotUseQueuedApproval(t *testing.T) {
	g := permission.New(permission.ModePlan)
	ctrl := &stubPlanController{on: true, pre: string(permission.ModeFullAccess)}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	ctx = agent.WithPlanController(ctx, ctrl)
	ctx = withPlanApproval(ctx, "Yes, full access (no sandbox)")
	res, err := NewExitPlanModeWithGate(g).Execute(ctx, map[string]any{"plan": "Cancelled approval must not authorize any edits."})
	if err != nil || res == nil || !res.IsError || g.Mode() != permission.ModePlan || !ctrl.on || ctrl.pre != string(permission.ModeFullAccess) {
		t.Fatalf("cancelled exit changed state: res=%+v err=%v gate=%s plan=%v pre=%q", res, err, g.Mode(), ctrl.on, ctrl.pre)
	}
}

func TestExitPlanMode_CancelledApprovalDeliveryKeepsPlanning(t *testing.T) {
	for _, eventCapacity := range []int{0, 1} {
		name := "proposal"
		if eventCapacity == 1 {
			name = "approval prompt"
		}
		t.Run(name, func(t *testing.T) {
			g := permission.New(permission.ModePlan)
			ctrl := &stubPlanController{on: true, pre: string(permission.ModeFullAccess)}
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Millisecond)
			defer cancel()
			ctx = agent.WithPlanController(ctx, ctrl)
			ctx = agent.WithEventOut(ctx, make(chan agent.Event, eventCapacity))
			res, err := NewExitPlanModeWithGate(g).Execute(ctx, map[string]any{"plan": "Do not implement if the approval UI is unavailable."})
			if err != nil || res == nil || !res.IsError || g.Mode() != permission.ModePlan || !ctrl.on || ctrl.pre != string(permission.ModeFullAccess) {
				t.Fatalf("undelivered approval changed state: res=%+v err=%v gate=%s plan=%v pre=%q", res, err, g.Mode(), ctrl.on, ctrl.pre)
			}
		})
	}
}
