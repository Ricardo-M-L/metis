package agent

import (
	"context"

	"github.com/Ricardo-M-L/metis/internal/llm"
)

// emitPlan dispatches the Plan-mode short-circuit: instead of executing
// the proposed tool calls, package them as a single EventPlan and
// terminate the turn cleanly.
//
// The runtime archives the plan to ~/.metis/plans/ via the EventPlan
// handler in cmd/metis/main.go and the bubbletea handler in
// internal/tui/tui.go.
func (l *Loop) emitPlan(ctx context.Context, toolUses []llm.ContentBlock, out chan<- Event) {
	calls := make([]ToolCall, len(toolUses))
	for i, b := range toolUses {
		calls[i] = ToolCall{ID: b.ToolUseID, Name: b.ToolName, Input: redactedToolInput(b.ToolInput)}
	}
	emit(ctx, out, Event{Kind: EventPlan, ToolCalls: calls})
	emit(ctx, out, Event{Kind: EventLoopDone, StopReason: "plan_mode"})
}
