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
	"github.com/Ricardo-M-L/metis/internal/tools"
)

// EnterPlanMode flips the active loop into plan mode. After the call
// succeeds, the next iteration's tool batch is intercepted (collected
// and emitted as EventPlan instead of executed) so the model has to
// surface its proposed actions before they happen.
type EnterPlanMode struct{ tools.BaseTool }

func NewEnterPlanMode() EnterPlanMode { return EnterPlanMode{} }

func (EnterPlanMode) Name() string { return "EnterPlanMode" }

func (EnterPlanMode) Description() string {
	return "Switch the agent into plan mode. After this call, the next " +
		"batch of tool calls will be SHOWN to the user as a plan " +
		"instead of executed — use this when the user asked for a " +
		"proposal before action, or when the task is risky enough that " +
		"a dry-run review is warranted. Call ExitPlanMode with the " +
		"final plan markdown to surface it for approval and resume " +
		"normal execution.\n\n" +
		"Caveat: in metis, ExitPlanMode surfaces the plan for review and " +
		"then resumes execution on the next turn — the user must " +
		"actively interrupt (Ctrl+C) to halt. It is NOT a blocking " +
		"approval gate. Headless / scheduled runs see the plan in the " +
		"event stream but cannot pause for human input. Do NOT rely on " +
		"this tool for true 'wait for user to click approve' semantics. " +
		"For multi-choice clarification (pick one of N options), use " +
		"`AskUser` instead — that's the right tool when you actually " +
		"need a response back."
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

// CanUse: plan mode entry has no permission impact (no FS / network
// effects), so always allow regardless of gate mode.
func (EnterPlanMode) CanUse(_ context.Context, _ map[string]any) (tools.Permission, string) {
	return tools.PermissionAllow, ""
}

func (EnterPlanMode) Execute(ctx context.Context, _ map[string]any) (*tools.Result, error) {
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
	ctrl.SetPlanMode(true)
	return &tools.Result{
		Output: "Plan mode active. Tool calls from your next turn will be collected as a plan for user review instead of executed. When ready, call ExitPlanMode with a `plan` argument containing the markdown plan body.",
	}, nil
}

// ExitPlanMode takes the final plan markdown, surfaces it via the
// loop's event stream (so the TUI/CLI can render it for approval), and
// flips the loop out of plan mode so subsequent tool calls execute
// normally.
//
// claude-code's variant blocks on user approval before continuing;
// metis surfaces the plan and lets normal execution resume on the
// next turn. The user can Ctrl+C to halt if the plan looks wrong —
// matches the existing metis interrupt UX.
type ExitPlanMode struct{ tools.BaseTool }

func NewExitPlanMode() ExitPlanMode { return ExitPlanMode{} }

func (ExitPlanMode) Name() string { return "ExitPlanMode" }

func (ExitPlanMode) Description() string {
	return "Leave plan mode and surface the final plan to the user. " +
		"Required argument `plan` is the markdown body of your " +
		"proposed work — bullet points, file paths, exact commands, " +
		"expected outcomes. After this call, normal tool execution " +
		"resumes; the user can interrupt (Ctrl+C) if the plan needs " +
		"revision.\n\n" +
		"Important: this is the right tool when you want to PROPOSE " +
		"a multi-step plan and continue (the user reads it, the next " +
		"turn starts executing). It is NOT the right tool when you " +
		"need a structured pick-one-of-N answer — use `AskUser` for " +
		"that. The two tools cover different cases: ExitPlanMode says " +
		"'here is what I'm about to do, speak up if wrong'; AskUser " +
		"says 'pick one of these options, I'll wait.'"
}

func (ExitPlanMode) InputSchema() map[string]any {
	return map[string]any{
		"type":     "object",
		"required": []string{"plan"},
		"properties": map[string]any{
			"plan": map[string]any{
				"type":        "string",
				"description": "Final plan markdown — what you'll do, in what order, with what side effects. Will be shown to the user for review.",
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

func (ExitPlanMode) Execute(ctx context.Context, in map[string]any) (*tools.Result, error) {
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
	// Emit the plan body as an info event so the TUI / CLI can render
	// it for user review. Using EventInfo (rather than extending the
	// existing EventPlan schema which carries ToolCalls) keeps the
	// change small and leaves the EventPlan handler untouched for the
	// `--mode plan` path that uses EventPlan with tool_use blocks.
	if out := agent.EventOutFromContext(ctx); out != nil {
		out <- agent.Event{
			Kind: agent.EventInfo,
			Info: "[plan proposal]\n" + plan,
		}
	}
	ctrl.SetPlanMode(false)
	return &tools.Result{
		Output: "Plan surfaced for user review. Plan mode disabled — subsequent tool calls will execute normally. If the user objects, they will interrupt; otherwise continue with the plan.",
	}, nil
}
