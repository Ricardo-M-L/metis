package agent

import "context"

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

	// Token + info
	InputTokens  int
	OutputTokens int
	StopReason   string
	Info         string

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
