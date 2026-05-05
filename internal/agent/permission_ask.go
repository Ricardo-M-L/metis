package agent

import (
	"context"

	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/permission"
)

// askResult is what the permission-ask helper returns to runOne.
//
//   - proceed=true, earlyReturn=nil → caller continues to Execute the tool.
//   - proceed=false, earlyReturn≠nil → return that block immediately
//     (denied by user, ctx cancelled, or other terminal cases).
type askResult struct {
	proceed     bool
	earlyReturn *llm.ContentBlock
}

// askPermission surfaces an interactive permission request to the UI and
// blocks until the user replies (or ctx is cancelled). On AlwaysAllow it
// also writes a permanent rule into the gate so the same tool/match
// doesn't prompt again this session.
//
// Extracted from runOne so the perm-Ask path is self-contained and
// independently testable. The runtime caller uses an EventPermissionRequest
// reply channel to deliver the user's decision.
func (l *Loop) askPermission(ctx context.Context, blk llm.ContentBlock, out chan<- Event) askResult {
	return l.askPermissionPending(ctx, blk, out, 0)
}

// askPermissionPending is the same as askPermission but sets
// PermissionPending on the outgoing event so the TUI can render
// "1 of N pending approvals". Used by executeBatch's batched ASK
// phase. pending is the number of additional ASKs still queued
// behind this one in the same batch.
func (l *Loop) askPermissionPending(ctx context.Context, blk llm.ContentBlock, out chan<- Event, pending int) askResult {
	reply := make(chan PermissionDecision, 1)
	emit(ctx, out, Event{
		Kind:              EventPermissionRequest,
		ToolUseID:         blk.ToolUseID,
		ToolName:          blk.ToolName,
		ToolInput:         blk.ToolInput,
		PermissionTool:    blk.ToolName,
		PermissionInput:   blk.ToolInput,
		PermissionReply:   reply,
		PermissionPending: pending,
	})

	var decision PermissionDecision
	select {
	case decision = <-reply:
	case <-ctx.Done():
		// Treat as a soft failure — return a tool_result block so the
		// LLM sees the cancellation as a structured error rather than
		// the loop exploding.
		blkOut := llm.ContentBlock{
			Type:       "tool_result",
			ToolUseID:  blk.ToolUseID,
			ToolResult: "cancelled",
			IsError:    true,
		}
		return askResult{proceed: false, earlyReturn: &blkOut}
	}

	switch decision {
	case PermissionDecisionDeny:
		blkOut := llm.ContentBlock{
			Type:       "tool_result",
			ToolUseID:  blk.ToolUseID,
			ToolResult: "denied by user",
			IsError:    true,
		}
		return askResult{proceed: false, earlyReturn: &blkOut}
	case PermissionDecisionAlwaysAllow:
		// Persist the decision so subsequent calls of the same tool
		// auto-pass. Source="interactive" so audits can tell this came
		// from a live user rather than config.
		l.Gate.AppendRules(permission.Rule{
			Tool:   blk.ToolName,
			Verb:   permission.DecisionAllow,
			Source: "interactive",
		})
	}
	return askResult{proceed: true}
}
