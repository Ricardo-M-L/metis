package builtin

import (
	"context"
	"strings"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

// AskUser is metis's structured user-interaction tool — the model
// dispatches it when it needs the human (not another LLM) to weigh
// in on a decision. Mirrors claude-code's AskUserQuestion: a labeled
// 3-5 option menu rendered in the TUI; the user picks; the choice
// flows back as a structured tool_result.
//
// The Execute path is bridged through the agent loop's event channel:
// it emits an EventAskUser carrying a reply channel, then blocks
// until the TUI handler writes the user's chosen answer or the ctx
// is cancelled. Headless contexts (metis run, sub-agents, scheduled
// jobs) have no TUI consumer — the event channel comes back nil and
// Execute returns a "non-interactive" structured error so the model
// sees a clean failure instead of hanging on a reply that will
// never arrive.
type AskUser struct {
	tools.BaseTool
	gate *permission.Gate
}

func (AskUser) Name() string { return "AskUser" }
func (AskUser) Description() string {
	return "Ask the user a question and wait for a structured answer. " +
		"Use for: (a) genuinely-stuck-after-investigation moments, (b) " +
		"presenting 3-5 concrete options for the user to pick from, (c) " +
		"clarifying ambiguous requirements before locking a plan. " +
		"DO NOT use for: requesting approval of a finished plan (use " +
		"EnterPlanMode/ExitPlanMode instead — that's what they exist " +
		"for); confirming permission-gated destructive actions (the " +
		"permission gate already prompts at tool dispatch); first-" +
		"response-to-friction (investigate first, ask only after honest " +
		"investigation). Pass `options` as a 3-5 item array of concrete " +
		"choices; set `allow_freeform: true` to additionally allow the " +
		"user to type their own answer. Blocked in non-interactive mode."
}
func (AskUser) InputSchema() map[string]any {
	return map[string]any{
		"type":     "object",
		"required": []string{"question"},
		"properties": map[string]any{
			"question": map[string]any{
				"type":        "string",
				"description": "The question to surface to the user. Keep it specific and self-contained — the user shouldn't need to re-read prior turns to understand what's being asked.",
			},
			"options": map[string]any{
				"type":        "array",
				"items":       map[string]any{"type": "string"},
				"description": "Optional list of 3-5 concrete choices the user can pick from. The user sees them as numbered options; the chosen one comes back as the tool_result.",
			},
			"allow_freeform": map[string]any{
				"type":        "boolean",
				"description": "When true, the user may type a custom answer in addition to picking from `options`. Default false — pure multiple-choice.",
			},
		},
	}
}
func (AskUser) Concurrency(map[string]any) tools.Concurrency { return tools.ConcurrencyExclusive }
func (a AskUser) CanUse(ctx context.Context, in map[string]any) (tools.Permission, string) {
	if isBypassUnattendedLineage(ctx, a.gate) {
		return tools.PermissionDeny, "AskUser unavailable in bypassPermissions unattended mode"
	}
	if a.gate == nil {
		return tools.PermissionAsk, "interactive"
	}
	d, _ := a.gate.Check(ctx, "AskUser", strFromAny(in["question"]))
	return mapDecision(d), "interactive"
}

func (a AskUser) Execute(ctx context.Context, in map[string]any) (*tools.Result, error) {
	// CanUse normally rejects this before dispatch. Keep the same invariant in
	// Execute for direct callers and for a bypass session that temporarily
	// switched its live Gate to plan mode: the pre-plan lineage is still
	// unattended and must never emit EventAskUser.
	if isBypassUnattendedLineage(ctx, a.gate) {
		return &tools.Result{
			Output:  "AskUser: unavailable in bypassPermissions unattended mode; apply the configured default policy and continue",
			IsError: true,
		}, nil
	}
	question, _ := in["question"].(string)
	question = strings.TrimSpace(question)
	if question == "" {
		return &tools.Result{
			Output:  "AskUser: question is required",
			IsError: true,
		}, nil
	}

	// Normalize options. Trim each, drop empties so a stray "" doesn't
	// render as a blank row in the TUI menu. Cap at 9 because the
	// keyboard handler binds 1-9 for selection — more than 9 options
	// would force a fallback we don't want to design for this round.
	var options []string
	if raw, ok := in["options"].([]any); ok {
		for _, v := range raw {
			if s, ok := v.(string); ok {
				s = strings.TrimSpace(s)
				if s != "" {
					options = append(options, s)
				}
			}
		}
	}
	if len(options) > 9 {
		options = options[:9]
	}
	allowFreeform, _ := in["allow_freeform"].(bool)
	// If no options AND no freeform fallback, the model has effectively
	// asked an open-ended question with no answer surface. Force
	// freeform on so the user can at least type back — rather than
	// silently strand the request.
	if len(options) == 0 {
		allowFreeform = true
	}

	out := agent.EventOutFromContext(ctx)
	if out == nil {
		// Headless / non-interactive path. The model sees a clean
		// structured error and can either OVERRIDE CONTRACT, fall
		// back to its best guess, or surface the question as text
		// for the user to manually answer in a future session.
		return &tools.Result{
			Output: "AskUser: no interactive UI available (running headless / metis run / sub-agent). " +
				"Either pick a reasonable default and continue, or surface the question + options in your " +
				"text reply so the user can answer in their next turn.",
			IsError: true,
		}, nil
	}

	reply := make(chan string, 1)
	out <- agent.Event{
		Kind:                 agent.EventAskUser,
		AskUserQuestion:      question,
		AskUserOptions:       options,
		AskUserAllowFreeform: allowFreeform,
		AskUserReply:         reply,
	}

	select {
	case answer := <-reply:
		// Empty answer = user dismissed the prompt (Esc / Cancel).
		// Treat as a soft failure so the model can choose to retry
		// with different framing or abandon the ask.
		if strings.TrimSpace(answer) == "" {
			return &tools.Result{
				Output:  "AskUser: user dismissed the prompt without answering",
				IsError: true,
			}, nil
		}
		return &tools.Result{Output: answer}, nil
	case <-ctx.Done():
		// Loop / session shutting down. Same structured-error shape
		// as the headless path so the model can't tell the
		// difference and handle it identically.
		return &tools.Result{
			Output:  "AskUser: cancelled (context done)",
			IsError: true,
		}, nil
	}
}
