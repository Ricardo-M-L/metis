package agent

import (
	"context"

	"github.com/Ricardo-M-L/metis/internal/llm"
)

// EventKind enumerates the event types emitted by the agent loop.
//
// The set has grown over time as agent observability matured —
// new sinks (status bar, hooks, bridge SSE) need different signals
// than just "text+tool". Hooks subscribe via PreToolUse / PostToolUse
// etc and can react to any of these without the loop knowing what
// listeners exist. New event types should be appended to keep enum
// values stable across versions.
type EventKind int

const (
	EventTextDelta         EventKind = iota // streaming model text chunk
	EventToolStart                          // tool dispatcher about to invoke
	EventToolResult                         // tool returned (success or error)
	EventPermissionRequest                  // ask-mode: human in the loop
	EventTurnEnd                            // assistant's turn finished
	EventLoopDone                           // whole conversation iteration done
	EventError                              // unrecoverable error to surface
	EventTokens                             // cumulative token usage update
	EventInfo                               // generic info line (compaction, etc.)
	EventPlan                               // PlanMode: tool calls awaiting approval

	// Streaming lifecycle — pre-existing events covered text+tool
	// but not the surrounding stream open/close, which hook authors
	// often want for analytics or rate-limit accounting.
	EventStreamStart // Anthropic/OpenAI HTTP stream connected
	EventStreamEnd   // stream closed cleanly (regardless of content)

	// Sub-agent lifecycle — Agent tool spawns these.
	EventSubAgentStart    // child loop dispatched
	EventSubAgentProgress // intermediate update from child
	EventSubAgentEnd      // child returned final result

	// Context window pressure — emitted before auto-compaction kicks in
	// so external observers can snapshot history before mutation.
	EventContextWarn      // crossed soft threshold (e.g., 70%)
	EventContextCompacted // auto-compaction just ran (also emits as EventInfo today)

	// Rate limit / provider feedback.
	EventRateLimitHit  // 429 / 529 from provider, retrying
	EventModelFallback // primary model rejected, switched to fallback

	// Channel / IM events — for users routing chat through Slack/etc.
	EventChannelInbound // message received from external channel
	EventChannelSent    // metis posted to external channel

	// Hook lifecycle — hooks can themselves emit events (echo loops
	// guarded by the hook registry).
	EventHookFired // a registered hook ran for some event

	// Extended-thinking deltas — Anthropic's reasoning trace, streamed
	// as a separate content_block alongside text. Surface dim/italic so
	// the user can see the model's thinking without it competing with
	// the final answer style. Without this branch the deltas were
	// silently dropped and "thought for Xs" had no body to back it.
	EventThinkingDelta

	// Streaming tool args — emitted as the LLM types tool input JSON
	// character-by-character. Lets the UI render "Read · /tmp/foo.go..."
	// as args arrive instead of waiting for tool_use_stop. The TextDelta
	// field carries the raw partial JSON (cumulative or chunk — see
	// streaming.go's emit path; metis emits chunks). Inspired by
	// kimi-cli's streamingjson tool args parser.
	EventToolArgsDelta
)

// eventOutKey carries the parent loop's event channel down to tools
// (specifically the Agent tool) via context so sub-loops can forward
// progress events upstream for live UI rendering. Without this, the
// parent UI sees the Agent tool start, then a long silence, then the
// final result — exactly the "no real-time progress" complaint from
// the user's screenshots.
type eventOutKey struct{}

// WithEventOut tags ctx with the channel sub-loops should forward
// progress to. dispatch.go calls this around tool.Execute so any
// tool that reads it gets a live forwarder.
func WithEventOut(ctx context.Context, ch chan<- Event) context.Context {
	return context.WithValue(ctx, eventOutKey{}, ch)
}

// EventOutFromContext returns the parent forwarder channel, or nil
// when none is attached (e.g., tools called outside the loop).
func EventOutFromContext(ctx context.Context) chan<- Event {
	if v, ok := ctx.Value(eventOutKey{}).(chan<- Event); ok {
		return v
	}
	return nil
}

// cwdKey carries a per-sub-agent effective working directory through
// context (Phase G.2, 2026-05-12). The Agent tool stamps this when
// the caller passes `cwd:"<abs path>"` or `isolation:"worktree"`;
// tools that care about cwd (Bash sets cmd.Dir from it; future
// Glob/LS could resolve relative roots against it) read it via
// CwdFromContext.
//
// Tools that don't read it default to the process-wide os.Getwd(),
// which is the parent agent's cwd — matching pre-G.2 behavior so
// existing tools don't accidentally change semantics.
type cwdKey struct{}

// WithCwd tags ctx with the effective working directory for a
// sub-agent. Empty string is treated as "no override" so callers
// don't need to special-case it.
func WithCwd(ctx context.Context, dir string) context.Context {
	if dir == "" {
		return ctx
	}
	return context.WithValue(ctx, cwdKey{}, dir)
}

// CwdFromContext returns the effective cwd for the current sub-agent,
// or "" when none is attached. Tools that respect cwd (Bash) treat
// "" as "use os.Getwd()".
func CwdFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(cwdKey{}).(string); ok {
		return v
	}
	return ""
}

// parentSnapshotKey carries a copy of the parent loop's conversation
// state down to tools so a "fork" tool can spawn a child loop that
// inherits the full context. Without this snapshot, a sub-agent
// starts cold (empty history, fresh system prompt — no shared state),
// which is fine for crush-style agentic_fetch but wrong for the
// claude-code "fork to continue this work" pattern.
type parentSnapshotKey struct{}

// ParentSnapshot is what Fork sees about its parent. Cloned at
// dispatch time so a tool that takes ages to run can't observe a
// later-mutated parent message slice (the parent can keep going
// with subsequent turns while Fork is still processing).
type ParentSnapshot struct {
	Messages []llm.Message
	System   string
	Model    string
}

// WithParentSnapshot tags ctx with a defensive copy of the parent
// loop's history+system+model. Called by dispatch.go right before
// every tool.Execute so any tool that needs parent context can
// pull it from the context key.
func WithParentSnapshot(ctx context.Context, snap ParentSnapshot) context.Context {
	// Defensive copy of the messages slice — tools can't mutate the
	// parent's live history through this snapshot.
	cp := make([]llm.Message, len(snap.Messages))
	copy(cp, snap.Messages)
	snap.Messages = cp
	return context.WithValue(ctx, parentSnapshotKey{}, snap)
}

// ParentSnapshotFromContext returns the parent loop snapshot when
// dispatch attached one, or nil otherwise. Tools called outside a
// loop (e.g. from CLI testing) get nil and must handle it.
func ParentSnapshotFromContext(ctx context.Context) *ParentSnapshot {
	if v, ok := ctx.Value(parentSnapshotKey{}).(ParentSnapshot); ok {
		return &v
	}
	return nil
}

// PlanController exposes mid-conversation plan-mode toggles to tools.
// The Loop is the concrete implementation; this interface is the
// surface tools see via context so internal/tools/builtin doesn't
// need to know about the full Loop type to flip the flag.
//
// claude-code parity: model can call EnterPlanMode mid-turn to
// declare "I'm planning now, don't make changes", then ExitPlanMode
// with a markdown plan body for user approval before executing.
type PlanController interface {
	SetPlanMode(on bool)
}

// planControllerKey carries the active loop's PlanController down to
// builtin tools (EnterPlanMode / ExitPlanMode) via dispatch context.
type planControllerKey struct{}

// WithPlanController tags ctx with the loop's PlanController. Called
// by dispatch.go right before tool.Execute so the plan-mode tools can
// flip the flag without importing agent.Loop directly.
func WithPlanController(ctx context.Context, c PlanController) context.Context {
	if c == nil {
		return ctx
	}
	return context.WithValue(ctx, planControllerKey{}, c)
}

// PlanControllerFromContext returns the active controller, or nil
// when the tool is invoked outside a Loop. Tools must handle nil
// gracefully (degrade to no-op + diagnostic message).
func PlanControllerFromContext(ctx context.Context) PlanController {
	if v, ok := ctx.Value(planControllerKey{}).(PlanController); ok {
		return v
	}
	return nil
}

// Event is the discriminated union streamed from Loop.Run.
type Event struct {
	Kind EventKind

	// Text events
	TextDelta string

	// Tool events
	ToolUseID  string
	ToolName   string
	ToolInput  map[string]any
	ToolResult *ToolResult

	// ToolCall captures the tool a Plan Mode loop wants to call.
	// When Kind == EventPlan, ToolCalls is non-empty.
	ToolCalls []ToolCall

	// Permission events: consumer writes PermissionDecision to PermissionReply.
	PermissionTool   string
	PermissionInput  map[string]any
	PermissionReason string
	PermissionReply  chan PermissionDecision // buffered, size 1

	// PermissionPending is the count of additional ASK requests in the
	// SAME tool batch that are still queued behind this one. Lets the
	// TUI render "1 of 4 pending approvals" instead of one prompt at a
	// time. Mirrors claude-code's batched-ask UX.
	//
	// Zero means this is the only / last pending ASK in the batch.
	PermissionPending int

	// Token + info. CacheCreationInputTokens / CacheReadInputTokens
	// mirror Anthropic's prompt-caching usage and let the TUI compute
	// claude-code-style context-window load (input + cache, no output)
	// rather than the per-turn cost-only number.
	InputTokens              int
	OutputTokens             int
	CacheCreationInputTokens int
	CacheReadInputTokens     int
	StopReason               string
	Info                     string

	// Error
	Err error
}

// ToolCall represents a tool the model wants to invoke.
type ToolCall struct {
	ID    string         `json:"id"`
	Name  string         `json:"name"`
	Input map[string]any `json:"input"`
}

// ToolResult wraps a tool execution result for streaming.
type ToolResult struct {
	Output  string `json:"output"`
	IsError bool   `json:"is_error"`
	Display string `json:"display,omitempty"`
}

type PermissionDecision int

const (
	PermissionDecisionAllow PermissionDecision = iota
	PermissionDecisionDeny
	PermissionDecisionAlwaysAllow
)
