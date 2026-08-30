// Package builtin — EnterPlanMode / ExitPlanMode tools.
//
// Mirrors claude-code's pair (tools/EnterPlanModeTool, tools/ExitPlanModeTool):
// the model declares "I'm planning, don't make changes" by calling
// EnterPlanMode, writes/edits its plan over zero or more turns, then
// calls ExitPlanMode with the final plan markdown for user approval.
//
// metis previously had a CLI-side `--mode plan` flag that hard-gated
// every tool — useful but coarse. These two tools add the per-turn
// claude-code-style entry/exit so a long conversation can drop into
// plan mode for one section then resume.
//
// Wiring: Loop satisfies agent.PlanController via SetPlanMode(bool).
// dispatch.go stashes it on tool context. These tools pull it and
// flip the flag. The loop's plan-mode gate whitelists ExitPlanMode
// (see splitExitPlanModeTools in agent/loop.go) so the exit tool can
// actually execute while plan mode is active.
package builtin

import (
	"context"
	"errors"
	"strings"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

// EnterPlanMode flips the active loop into plan mode. Read-only exploration
// continues normally; state-changing tools receive ordinary permission-denied
// results so the model can recover, finish the proposal, and call ExitPlanMode.
type EnterPlanMode struct {
	tools.BaseTool
	// gate, when non-nil, lets EnterPlanMode flip Gate.Mode in addition
	// to Loop.PlanMode. The listener wired in main.go bridges Gate→Loop
	// so the loop's PlanMode short-circuit fires correctly. Pre-fix
	// (2026-05-18 audit), EnterPlanMode only set Loop.PlanMode, leaving
	// Gate.Mode untouched — that worked for "model is in plan" but the
	// status bar showed the old mode and ExitPlanMode couldn't restore
	// the user's prior posture. Nil = test path (legacy direct
	// SetPlanMode fallback still works).
	gate *permission.Gate
}

func NewEnterPlanMode() EnterPlanMode { return EnterPlanMode{} }

// NewEnterPlanModeWithGate is the production wiring — pass the live
// Gate so EnterPlanMode flips it (and the listener propagates to Loop).
func NewEnterPlanModeWithGate(g *permission.Gate) EnterPlanMode {
	return EnterPlanMode{gate: g}
}

// isBypassUnattendedLineage recognizes both a live bypass gate and the plan
// mode descended from it. EnterPlanMode necessarily changes Gate.Mode to plan,
// so checking only the current mode would re-enable AskUser until ExitPlanMode
// restored the saved posture.
func isBypassUnattendedLineage(ctx context.Context, gate *permission.Gate) bool {
	if gate == nil {
		return false
	}
	if gate.Mode() == permission.ModeBypassPermissions {
		return true
	}
	// A pre-plan snapshot is lineage only while plan mode is still live. A
	// manual mode change must immediately restore interactive behavior even if
	// an older UI forgot to clear the snapshot.
	if gate.Mode() != permission.ModePlan {
		return false
	}
	ctrl := agent.PlanControllerFromContext(ctx)
	if ctrl == nil {
		return false
	}
	previous, ok := permission.ParseMode(ctrl.PrePlanMode())
	return ok && previous == permission.ModeBypassPermissions
}

func (EnterPlanMode) Name() string { return "EnterPlanMode" }

func (EnterPlanMode) Description() string {
	return "Switch the agent into plan mode. Read-only exploration remains " +
		"available, including the Agent tool (its child inherits the plan " +
		"gate), while edits and commands that change state are blocked. " +
		"Use this when the user asked for a proposal before action. When " +
		"the plan is complete, call ExitPlanMode with the final markdown. " +
		"Interactive modes present an approval prompt; bypassPermissions " +
		"uses its unattended preset and continues automatically. For " +
		"requirements clarification during interactive planning, use AskUser."
}

func (EnterPlanMode) InputSchema() map[string]any {
	return map[string]any{
		"type":       "object",
		"properties": map[string]any{},
	}
}

// Concurrency: plan mode toggles are fast (lock + bool write) but
// sequential against other tools by semantics — running in parallel
// with a normal tool would race the gate check at the next iteration
// boundary.
func (EnterPlanMode) Concurrency(map[string]any) tools.Concurrency {
	return tools.ConcurrencyExclusive
}

// Entering plan mode changes the agent's operating contract and therefore
// requires an explicit user decision, matching Claude Code's EnterPlanMode
// tool. A redundant call while plan mode is already active is metadata-only
// and may pass without prompting; this also prevents a model retry from
// trapping the user in duplicate approval dialogs.
func (e EnterPlanMode) CanUse(_ context.Context, _ map[string]any) (tools.Permission, string) {
	if e.gate != nil && e.gate.Mode() == permission.ModePlan {
		return tools.PermissionAllow, "already in plan mode"
	}
	if e.gate != nil && e.gate.Mode() == permission.ModeBypassPermissions {
		return tools.PermissionAllow, "bypassPermissions unattended plan policy"
	}
	return tools.PermissionAsk, "entering plan mode requires user approval"
}

func (e EnterPlanMode) Execute(ctx context.Context, _ map[string]any) (*tools.Result, error) {
	ctrl := agent.PlanControllerFromContext(ctx)
	if ctrl == nil {
		// Should not happen in normal Loop dispatch — the controller
		// is attached by dispatch.go for every tool call. nil only
		// arises in test harnesses or direct-call paths.
		return &tools.Result{
			Output:  "EnterPlanMode: no plan controller in context (tool called outside an active Loop)",
			IsError: true,
		}, nil
	}
	// Production path: flip the gate. The listener wired in main.go
	// will sync Loop.PlanMode automatically. We also snapshot the
	// pre-plan mode onto the controller so ExitPlanMode can restore
	// the user's prior posture (without this, Exit would leave Gate
	// stuck in plan and the next turn would re-trigger deny-storm
	// for every write tool).
	if e.gate != nil {
		prev := e.gate.Mode()
		if prev != permission.ModePlan {
			ctrl.SetPrePlanMode(string(prev))
		}
		e.gate.SetMode(permission.ModePlan)
		return &tools.Result{
			Output: "Plan mode active. Read-only exploration and Agent delegation remain available; state-changing tools are denied. When ready, call ExitPlanMode with a `plan` argument containing the markdown plan body for user approval.",
		}, nil
	}
	// Fallback path for test harnesses that build a Loop without a
	// Gate. Flip Loop.PlanMode directly; user sees "model thinks it's
	// in plan but gate still allows writes" which is a known test
	// limitation — production always wires the gate.
	ctrl.SetPlanMode(true)
	return &tools.Result{
		Output: "Plan mode active. Read-only exploration remains available; state-changing tools are denied. When ready, call ExitPlanMode with a `plan` argument containing the markdown plan body for user approval.",
	}, nil
}

// ExitPlanMode takes the final plan markdown, surfaces it via the
// loop's event stream (so the TUI/CLI can render it for approval), and
// flips the loop out of plan mode so subsequent tool calls execute
// normally.
//
// Interactive production paths block on user approval before changing
// permission mode. A bypass-origin plan uses its unattended preset and returns
// directly to bypass; rejection and other headless execution remain in plan.
type ExitPlanMode struct {
	tools.BaseTool
	gate *permission.Gate // optional; production wires via NewExitPlanModeWithGate
}

func NewExitPlanMode() ExitPlanMode { return ExitPlanMode{} }

// NewExitPlanModeWithGate is the production wiring — pass the live
// Gate so ExitPlanMode restores Gate.Mode (and the listener propagates
// to Loop).
func NewExitPlanModeWithGate(g *permission.Gate) ExitPlanMode {
	return ExitPlanMode{gate: g}
}

func (ExitPlanMode) Name() string { return "ExitPlanMode" }

func (ExitPlanMode) Description() string {
	return "Leave plan mode and surface the final plan to the user. " +
		"Required argument `plan` is the markdown body of your " +
		"proposed work — bullet points, file paths, exact commands, " +
		"expected outcomes. Interactive modes block until the user " +
		"approves implementation or chooses to keep planning; a " +
		"bypassPermissions-origin plan is auto-approved by its preset " +
		"and continues without an interaction.\n\n" +
		"Important: this is the right tool when you want to PROPOSE " +
		"a multi-step plan and request approval. It is NOT the right tool when you " +
		"need a structured pick-one-of-N answer — use `AskUser` for " +
		"that. The two tools cover different cases: ExitPlanMode says " +
		"'here is the plan; approve or keep planning'; AskUser " +
		"says 'pick one of these options, I'll wait.'"
}

func (ExitPlanMode) InputSchema() map[string]any {
	return map[string]any{
		"type":     "object",
		"required": []string{"plan"},
		"properties": map[string]any{
			"plan": map[string]any{
				"type":        "string",
				"description": "Final plan markdown — what you'll do, in what order, with what side effects. Shown for review in interactive modes; recorded and auto-accepted under the bypassPermissions preset.",
			},
		},
	}
}

func (ExitPlanMode) Concurrency(map[string]any) tools.Concurrency {
	return tools.ConcurrencyExclusive
}

func (ExitPlanMode) CanUse(_ context.Context, _ map[string]any) (tools.Permission, string) {
	return tools.PermissionAllow, ""
}

func (e ExitPlanMode) Execute(ctx context.Context, in map[string]any) (*tools.Result, error) {
	plan, _ := in["plan"].(string)
	plan = strings.TrimSpace(plan)
	if plan == "" {
		return nil, errors.New("plan is required")
	}
	ctrl := agent.PlanControllerFromContext(ctx)
	if ctrl == nil {
		return &tools.Result{
			Output:  "ExitPlanMode: no plan controller in context (tool called outside an active Loop)",
			IsError: true,
		}, nil
	}
	if out := agent.EventOutFromContext(ctx); out != nil {
		out <- agent.Event{
			Kind: agent.EventInfo,
			Info: "[plan proposal]\n" + plan,
		}
	}
	// Lightweight embedders/tests that construct the tool without a Gate have
	// no permission mode to restore and historically used the controller-only
	// toggle. Preserve that fallback; production always uses WithGate and takes
	// the blocking approval path below.
	if e.gate == nil {
		ctrl.SetPlanMode(false)
		ctrl.SetPrePlanMode("")
		return &tools.Result{Output: "Plan surfaced for review. Plan mode disabled by the controller-only fallback."}, nil
	}
	if e.gate.Mode() != permission.ModePlan {
		// The user already left plan mode through another surface. A stale
		// snapshot must never make ExitPlanMode silently restore bypass.
		ctrl.SetPrePlanMode("")
		return &tools.Result{
			Output:  "ExitPlanMode: plan mode is not active; stale plan state was cleared",
			IsError: true,
		}, nil
	}
	restore := permission.ModeDefault
	if prev, ok := permission.ParseMode(ctrl.PrePlanMode()); ok && prev != permission.ModePlan {
		restore = prev
	}
	// bypassPermissions is explicitly unattended. EnterPlanMode is allowed to
	// organize a complex task, but ExitPlanMode must not turn that session back
	// into an approval workflow. Use the preset policy: approve the plan and
	// restore bypass without emitting EventAskUser.
	if restore == permission.ModeBypassPermissions {
		e.gate.SetMode(permission.ModeBypassPermissions)
		ctrl.SetPrePlanMode("")
		return &tools.Result{Output: "Plan accepted by the bypassPermissions unattended policy. Continue implementation in bypassPermissions mode."}, nil
	}

	// Interactive modes keep the blocking approval boundary. Headless callers
	// cannot approve, so they remain in plan mode with a structured error.
	out := agent.EventOutFromContext(ctx)
	if out == nil {
		return &tools.Result{
			Output:  "ExitPlanMode: no interactive UI is available to approve this plan; remaining in plan mode",
			IsError: true,
		}, nil
	}
	primaryLabel := "Yes, auto-accept edits"
	primaryMode := permission.ModeAcceptEdits
	if restore == permission.ModeBypassPermissions {
		primaryLabel = "Yes, bypass permissions"
		primaryMode = permission.ModeBypassPermissions
	}
	const manualLabel = "Yes, manually approve edits"
	const rejectLabel = "No, keep planning"
	reply := make(chan string, 1)
	out <- agent.Event{
		Kind:            agent.EventAskUser,
		AskUserQuestion: "Ready to implement this plan?",
		AskUserOptions:  []string{primaryLabel, manualLabel, rejectLabel},
		AskUserReply:    reply,
	}

	select {
	case answer := <-reply:
		switch strings.TrimSpace(answer) {
		case primaryLabel:
			if e.gate != nil {
				e.gate.SetMode(primaryMode)
			} else {
				ctrl.SetPlanMode(false)
			}
			ctrl.SetPrePlanMode("")
			return &tools.Result{Output: "User approved the plan. Continue implementation in " + string(primaryMode) + " mode."}, nil
		case manualLabel:
			if e.gate != nil {
				e.gate.SetMode(permission.ModeDefault)
			} else {
				ctrl.SetPlanMode(false)
			}
			ctrl.SetPrePlanMode("")
			return &tools.Result{Output: "User approved the plan. Continue implementation in default mode; state-changing tools require approval."}, nil
		default:
			return &tools.Result{Output: "User chose to keep planning. Remain in plan mode and revise the proposal before calling ExitPlanMode again."}, nil
		}
	case <-ctx.Done():
		return &tools.Result{
			Output:  "ExitPlanMode: approval cancelled; remaining in plan mode",
			IsError: true,
		}, nil
	}
}
