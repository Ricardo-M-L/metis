package agent

import (
	"context"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/Ricardo-M-L/metis/internal/budget"
	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/google/uuid"
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
	EventContextCompacted // context history was successfully reduced and applied

	// EventCompactionStart marks the start of the unified LLM-driven
	// compaction pipeline. The TUI swaps the spinner label to "Compacting
	// conversation..." so the user knows the apparent freeze is a
	// summarization call and not a hang. Info carries the trigger name
	// (auto, manual, overflow, or second-wind). Mirrors claude-code's
	// onCompactProgress event
	// (REPL.tsx:2497, case 'compact_start').
	EventCompactionStart

	// EventCompactionProgress carries the cumulative output-bytes count
	// from the summarize stream so the TUI can show "Compacting
	// conversation (12K tokens streamed)" — the token counter that lets
	// the user see the summarize call is still alive. InputTokens field
	// piggy-backs the cumulative *byte* count (we don't have a real
	// tokenizer in the loop; bytes/4 is the standard estimate). Mirrors
	// CC's responseLengthRef-driven spinner token counter.
	EventCompactionProgress

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

	// Redacted thinking — Anthropic's safety classifier replaced a
	// chunk of the model's reasoning with opaque cipher text. There's
	// no plaintext to display; the TUI shows a "🔒 redacted" placeholder
	// and the persisted ContentBlock carries the encrypted Data field
	// so subsequent turns can echo it back unchanged for decryption.
	// TextDelta carries the base64 cipher text (used by the TUI for
	// downstream Message{Role:"redacted_thinking"} accumulation, not
	// for rendering — the cipher text is never user-visible).
	EventRedactedThinking

	// Streaming tool args — emitted as the LLM types tool input JSON
	// character-by-character. Lets the UI render "Read · /tmp/foo.go..."
	// as args arrive instead of waiting for tool_use_stop. The TextDelta
	// field carries the raw partial JSON (cumulative or chunk — see
	// streaming.go's emit path; metis emits chunks). Inspired by
	// kimi-cli's streamingjson tool args parser.
	EventToolArgsDelta

	// Dreaming lifecycle (Phase C, 2026-05-16) — main-channel events
	// for the auto-memory consolidation workflow so the TUI can swap
	// the spinner verb to "Dreaming...", render a progress pill, and
	// post a `✻ context dreamed` inline summary at completion.
	//
	// Distinct from the older DreamNotification channel: that channel
	// feeds a system-reminder back to the LLM (so the model knows
	// memory was just refreshed); these events feed the TUI.
	//
	//   EventDreamingStart    — fork is about to run; spinner override
	//                           "Dreaming..." pins. Info = trigger
	//                           reason ("scheduled"/"manual").
	//   EventDreamingProgress — incremental phase update. Info encodes
	//                           "<phase> <key>=<value>" tags the TUI
	//                           parses for the status pill (sessions
	//                           reviewed, files written, etc).
	//   EventDreamingEnd      — fork completed. Info carries the inline
	//                           summary tail (e.g. "+2 memories, +1 skill").
	//                           Cleared spinner; appended one-line message.
	EventDreamingStart
	EventDreamingProgress
	EventDreamingEnd

	// EventAskUser — the model dispatched the AskUser tool and is
	// blocking on a user response. The TUI renders a question prompt
	// with the model's labeled options (and optional freeform input)
	// then writes the chosen answer back through AskUserReply so the
	// tool can return a structured result. Mirrors claude-code's
	// AskUserQuestion tool flow: tool blocks → UI surfaces options →
	// user picks → tool unblocks with the choice as its tool result.
	//
	// Headless callers (metis run / sub-agents / scheduled jobs) see
	// EventAskUser and route it to a fallback handler that returns
	// "non-interactive" so the model gets a structured error instead
	// of an indefinite hang.
	EventAskUser

	// EventCompactionEnd is the lifecycle counterpart to
	// EventCompactionStart. It is emitted after every LLM-driven
	// compaction attempt, including failures, and exists only to release
	// progress UI. A successful history mutation is reported separately by
	// EventContextCompacted. This mirrors Claude Code's compact_end event:
	// ending the animation must not claim that compaction succeeded.
	// Appended here (rather than beside EventCompactionStart) to preserve
	// the numeric values of existing EventKind constants.
	EventCompactionEnd

	// Internal trace-invocation lifecycle. These events are delivered only to
	// the process trace hook (never to the user-facing event channel) and let
	// the trace adapter distinguish a child that really started from an Agent /
	// Fork / Ralph call that permission or a hook short-circuited. Keep them at
	// the end so every externally consumed EventKind value remains stable.
	EventTraceInvocationStart
	EventTraceInvocationEnd
)

// agentNameKey carries the current agent's team identity (the `name` param
// passed to Agent({name: "alice"})) through the tool-execute context so
// MessageTeammate can show the real sender instead of the hardcoded "main".
// Fallback is "main" for the top-level loop which has no roster name.
type agentNameKey struct{}

// WithAgentName stamps ctx with the sub-agent's roster name. Called by
// the Agent tool after the teammate entry is resolved, before building
// baseCtx for the sub-loop — so all tools that sub-agent calls
// (including MessageTeammate) read the right identity.
func WithAgentName(ctx context.Context, name string) context.Context {
	if name == "" {
		return ctx
	}
	return context.WithValue(ctx, agentNameKey{}, name)
}

// AgentNameFromContext returns the current agent's roster name, or "main"
// when none is stamped (top-level loop, headless tests, or any tool path
// that doesn't go through a named Agent spawn).
func AgentNameFromContext(ctx context.Context) string {
	if ctx == nil {
		return "main"
	}
	if v, ok := ctx.Value(agentNameKey{}).(string); ok && v != "" {
		return v
	}
	return "main"
}

// toolLifecycleContextKey carries a sub-agent's own lifecycle cancellation
// through InterruptBlock dispatch. InterruptBlock normally detaches the
// current turn's cancellation so a first Ctrl+C cannot interrupt an atomic
// file write or foreground command. A detached background sub-agent still
// needs SubAgentStop, timeout, and roster shutdown to be authoritative,
// however, so Agent stamps its private lifecycle context under this key.
//
// Top-level loops deliberately do not stamp this value. Their historical
// first-Ctrl+C InterruptBlock semantics therefore remain unchanged.
type toolLifecycleContextKey struct{}

// WithToolLifecycleContext stamps lifecycle onto ctx. lifecycle must be the
// cancelable context owned by the sub-agent runner, not a parent turn context.
func WithToolLifecycleContext(ctx, lifecycle context.Context) context.Context {
	if ctx == nil || lifecycle == nil {
		return ctx
	}
	return context.WithValue(ctx, toolLifecycleContextKey{}, lifecycle)
}

// ToolLifecycleContextFromContext returns a sub-agent lifecycle context when
// one was explicitly stamped, or nil for ordinary top-level dispatch.
func ToolLifecycleContextFromContext(ctx context.Context) context.Context {
	if ctx == nil {
		return nil
	}
	lifecycle, _ := ctx.Value(toolLifecycleContextKey{}).(context.Context)
	return lifecycle
}

// subAgentNotifyKey carries the parent loop's sub-agent notification
// channel send end down to the Agent tool so background sub-agents can
// signal completion without the parent polling SubAgentList. Mirrors
// claude-code's idle_notification flow: worker finishes → sends to
// parent's notify channel → parent injects <sub_agent_idle> at the
// next iter boundary.
//
// Deliberately NOT inherited by child sub-loops: Agent.Execute extracts
// the value from the tool's ctx, shadows the key with nil in baseCtx,
// and passes the extracted channel directly to executeBackground. This
// prevents sub-sub-agents (depth >= 2) from accidentally notifying the
// grandparent's loop.
type subAgentNotifyKey struct{}

// WithSubAgentNotify stamps ctx with the parent loop's sub-agent notify
// send channel. Called by Loop.Run at the top so every tool dispatched
// in that turn can reach the channel via SubAgentNotifyFromContext.
// Storing nil is intentional: Agent.Execute uses it to shadow the key
// in baseCtx so the child loop doesn't inherit a stale pointer.
func WithSubAgentNotify(ctx context.Context, ch chan<- SubAgentNotification) context.Context {
	return context.WithValue(ctx, subAgentNotifyKey{}, ch)
}

// SubAgentNotifyFromContext retrieves the parent loop's sub-agent notify
// send channel, or nil when not stamped (top-level loop, tests, or a
// context where the key was explicitly cleared by the Agent tool).
func SubAgentNotifyFromContext(ctx context.Context) chan<- SubAgentNotification {
	if ctx == nil {
		return nil
	}
	if v, ok := ctx.Value(subAgentNotifyKey{}).(chan<- SubAgentNotification); ok {
		return v
	}
	return nil
}

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
// when none is attached (e.g., tools called outside the loop). Nil
// ctx is treated the same as "no channel attached" — callers that
// pass context.Background() or genuinely nil ctx get nil back rather
// than a panic on the Value dereference.
func EventOutFromContext(ctx context.Context) chan<- Event {
	if ctx == nil {
		return nil
	}
	if v, ok := ctx.Value(eventOutKey{}).(chan<- Event); ok {
		return v
	}
	return nil
}

// cwdKey carries a per-sub-agent effective working directory through
// context (Phase G.2, 2026-05-12). The Agent tool stamps this when
// the caller passes `cwd:"<abs path>"` or `isolation:"worktree"`;
// tools that care about cwd (including Bash, Git, Glob, Grep, LS, and
// ViewImage) read it via CwdFromContext.
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

// parentToolUseIDKey carries the parent's Agent tool_use_id down to
// the Agent tool itself, so its sub-event forwarder can stamp each
// forwarded child event with the parent's id. The TUI uses this to
// attribute "sub: Read", "sub: Bash" progress events to the right
// SubAgentInfo entry when multiple sub-agents are in flight.
//
// Without this, sub-event progress would have to be assigned by
// process of elimination — wrong as soon as two sub-agents run in
// parallel.
type parentToolUseIDKey struct{}

// traceInvocationIDKey is the process-unique execution identity of the child
// invocation whose events a context emits. It is deliberately independent of
// the provider's tool_use_id: providers may reuse IDs across sessions, turns,
// retries, Ralph rounds, or even parallel responses in one process.
type traceInvocationIDKey struct{}

var traceInvocationSequence atomic.Uint64
var traceInvocationProcessPrefix = uuid.NewString()

// NewTraceInvocationID returns an identifier unique for this process. The
// trace adapter is process-local too, so an atomic sequence is sufficient and
// avoids randomness/error handling in the tool-dispatch hot path.
func NewTraceInvocationID() string {
	return "trace-inv-" + traceInvocationProcessPrefix + "-" + strconv.FormatUint(traceInvocationSequence.Add(1), 10)
}

// WithTraceInvocationID pins child events emitted through ctx to one internal
// trace invocation. Public UI attribution continues to use
// SubAgentParentID/ParentToolUseID; this value is observability-only.
func WithTraceInvocationID(ctx context.Context, id string) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if id == "" {
		return ctx
	}
	return context.WithValue(ctx, traceInvocationIDKey{}, id)
}

// TraceInvocationIDFromContext returns the internal trace identity, if any.
func TraceInvocationIDFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	if v, ok := ctx.Value(traceInvocationIDKey{}).(string); ok {
		return v
	}
	return ""
}

// WithParentToolUseID stamps ctx with the parent tool_use_id. dispatch.go
// calls this in runExecute right before tool.Execute. Tools that don't
// read it (Bash, Read, Edit, ...) are unaffected.
func WithParentToolUseID(ctx context.Context, id string) context.Context {
	if id == "" {
		return ctx
	}
	return context.WithValue(ctx, parentToolUseIDKey{}, id)
}

// ParentToolUseIDFromContext returns the parent's tool_use_id when set,
// or "" otherwise. Used by the Agent tool's sub-event forwarder.
func ParentToolUseIDFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(parentToolUseIDKey{}).(string); ok {
		return v
	}
	return ""
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

// budgetKey carries the parent loop's *budget.Tracker to sub-agent
// builders (Agent / Fork tools). Children add their usage to the SAME
// tracker, so a session-level USD cap covers the whole agent tree —
// a parent can't dodge its budget by fanning out sub-agents.
type budgetKey struct{}

// WithBudget tags ctx with the parent loop's budget tracker. Called
// by dispatch.go alongside WithParentSnapshot. nil trackers are not
// stored.
func WithBudget(ctx context.Context, t *budget.Tracker) context.Context {
	if t == nil {
		return ctx
	}
	return context.WithValue(ctx, budgetKey{}, t)
}

// BudgetFromContext returns the shared budget tracker, or nil when
// the parent loop has no USD cap.
func BudgetFromContext(ctx context.Context) *budget.Tracker {
	t, _ := ctx.Value(budgetKey{}).(*budget.Tracker)
	return t
}

// spillDirKey carries the parent loop's SpillDir to sub-agent builders
// so a sub-agent's oversized tool results spill to disk too, instead
// of entering the child context wholesale. Sub-agents share the
// parent's session store; tool_use_ids are globally unique so files
// never collide across the tree.
type spillDirKey struct{}

// WithSpillDir tags ctx with the parent loop's spill directory. Called
// by dispatch.go alongside WithBudget. Empty dir is not stored.
func WithSpillDir(ctx context.Context, dir string) context.Context {
	if dir == "" {
		return ctx
	}
	return context.WithValue(ctx, spillDirKey{}, dir)
}

// SpillDirFromContext returns the inherited spill directory, or "" when
// spilling is disabled in the parent.
func SpillDirFromContext(ctx context.Context) string {
	d, _ := ctx.Value(spillDirKey{}).(string)
	return d
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
	// PrePlanMode / SetPrePlanMode let the plan-mode meta-tools
	// remember the user's previous gate mode across the plan window
	// so ExitPlanMode can restore it. Empty string means "no snapshot
	// captured" — ExitPlanMode then picks a safe default (ModeAsk).
	// Pre-fix (2026-05-18), exiting plan left Gate.Mode stuck at
	// "plan" because nothing restored it, which re-triggered the
	// deny-storm on the next turn.
	PrePlanMode() string
	SetPrePlanMode(mode string)
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
	// Elapsed is the authoritative wall-clock duration of the tool
	// call. Set by dispatch.go on EventToolResult only — populated
	// from `time.Since(jobStartedAt)` inside the loop that ACTUALLY
	// ran the tool, so forwarded sub-agent results carry the real
	// child-loop duration instead of the gap between parent-side
	// event arrivals.
	//
	// 2026-05-23: added after image #54 repro — sub-agent Read
	// calls showed 0ms because parent's `time.Since(startTime)`
	// only measured the inter-event delay (~µs) once the forwarded
	// start + result both reached the parent's channel. With this
	// field the TUI reads the loop's own measurement instead.
	Elapsed time.Duration

	// ToolCall captures the tool a Plan Mode loop wants to call.
	// When Kind == EventPlan, ToolCalls is non-empty.
	ToolCalls []ToolCall

	// Permission events: consumer writes PermissionDecision to PermissionReply.
	PermissionTool string
	// PermissionInput is the redacted presentation copy. It is the ONLY
	// permission argument map that may be rendered, logged, traced, sent over
	// ACP/WebUI, audited, or persisted.
	PermissionInput map[string]any
	// permissionPolicyInput is an exact deep-cloned snapshot of the arguments
	// that will be executed. It exists solely so an unattended in-process
	// policy consumer (currently cron) authorizes the same bytes the tool sees.
	//
	// Keep this field private: it must never be rendered, logged, traced, sent
	// over ACP/WebUI, audited, or persisted. Call
	// PermissionPolicyInputForAuthorization only at the policy decision point.
	// Store the snapshot behind an accessor closure instead of as a map field.
	// Besides keeping it package-private, this prevents an accidental generic
	// fmt dump of Event from printing the raw values.
	permissionPolicyInput func() map[string]any
	PermissionReason      string
	PermissionReply       chan PermissionDecision // buffered, size 1

	// PermissionPending is the count of additional ASK requests in the
	// SAME tool batch that are still queued behind this one. Lets the
	// TUI render "1 of 4 pending approvals" instead of one prompt at a
	// time. Mirrors claude-code's batched-ask UX.
	//
	// Zero means this is the only / last pending ASK in the batch.
	PermissionPending int

	// AskUser events: model called the AskUser tool. Consumer (TUI)
	// renders the prompt + options and writes the chosen answer back
	// to AskUserReply. Empty AllowFreeform means the user is restricted
	// to the supplied options; true means they may type a custom
	// answer instead.
	AskUserQuestion      string
	AskUserOptions       []string
	AskUserAllowFreeform bool
	AskUserReply         chan string // buffered, size 1

	// Token + info. CacheCreationInputTokens / CacheReadInputTokens
	// mirror Anthropic's prompt-caching usage and let the TUI compute
	// claude-code-style context-window load (input + cache, no output)
	// rather than the per-turn cost-only number.
	InputTokens              int
	OutputTokens             int
	CacheCreationInputTokens int
	CacheReadInputTokens     int
	// PreviousContextTokens and ContextTokens are byte-derived context
	// estimates attached to EventContextCompacted. They describe the
	// history that was actually replaced, unlike the compaction API call's
	// own input/output usage (which is roughly the pre-compact context and
	// must never be shown as the resulting context size).
	PreviousContextTokens int
	ContextTokens         int
	StopReason            string
	Info                  string

	// Error
	Err error

	// SubAgentParentID is set on EventToolStart / EventToolResult
	// events that were FORWARDED from a sub-agent's loop. The value is
	// the parent's Agent tool_use_id (the one the TUI registered when
	// SubAgentInfo first appeared), so consumers can attribute child
	// tool activity to the right pill when multiple sub-agents run in
	// parallel.
	//
	// Empty means "this event came from the main loop, not a sub-agent."
	SubAgentParentID string

	// TraceInvocationID is a process-unique internal execution identity. It
	// MUST NOT be rendered as the public sub-agent parent: SubAgentParentID
	// remains the provider tool_use_id used by the UI. TraceParentInvocationID
	// links a nested Agent/Fork/Ralph ToolStart to the invocation of the loop
	// that emitted it, allowing the trace adapter to inherit an immutable
	// session+turn without consulting whichever Desktop session is active now.
	TraceInvocationID       string
	TraceParentInvocationID string
	// TraceCallID uniquely identifies this specific tool-call occurrence.
	// Provider tool_use_id values can repeat across responses and process
	// restarts; persistence uses this durable-in-session key to pair one
	// tool_start with its own result without merging later calls.
	TraceCallID string
}

// PermissionPolicyInputForAuthorization returns a fresh defensive copy of
// the exact tool arguments captured for an EventPermissionRequest. This is a
// deliberately narrow escape hatch for in-process authorization policy. The
// returned value must never be rendered, logged, traced, audited, persisted,
// or sent over ACP/WebUI; all presentation code must use PermissionInput.
func (e Event) PermissionPolicyInputForAuthorization() (map[string]any, bool) {
	if e.permissionPolicyInput == nil {
		return nil, false
	}
	return e.permissionPolicyInput(), true
}

// capturePermissionPolicyInput freezes one JSON-shaped argument graph and
// returns defensive copies to the sole policy accessor. The closure also keeps
// raw values out of generic Event formatting and serialization surfaces.
func capturePermissionPolicyInput(input map[string]any) func() map[string]any {
	snapshot := clonePresentation(input)
	return func() map[string]any { return clonePresentation(snapshot) }
}

// PresentationCopy returns a detached event suitable for UI replay, wire
// adapters, and other presentation-only consumers. Authorization-only state
// is deliberately removed so a replay buffer cannot extend the lifetime of
// raw credential-bearing tool arguments after the decision point.
//
// Presentation maps are redacted again and deep-cloned here. Producers are
// expected to emit presentation-safe values already, but enforcing that
// boundary at the storage handoff keeps a future producer mistake from
// turning into a long-lived UI or replay leak.
func (e Event) PresentationCopy() Event {
	e.permissionPolicyInput = nil
	e.PermissionReply = nil
	e.AskUserReply = nil

	e.ToolInput = redactedToolInput(e.ToolInput)
	e.PermissionInput = redactedToolInput(e.PermissionInput)
	if e.ToolResult != nil {
		result := *e.ToolResult
		result.Presentation = redactedToolInput(result.Presentation)
		e.ToolResult = &result
	}
	if len(e.ToolCalls) > 0 {
		e.ToolCalls = append([]ToolCall(nil), e.ToolCalls...)
		for i := range e.ToolCalls {
			e.ToolCalls[i].Input = redactedToolInput(e.ToolCalls[i].Input)
		}
	}
	e.AskUserOptions = append([]string(nil), e.AskUserOptions...)
	return e
}

// ToolCall represents a tool the model wants to invoke.
type ToolCall struct {
	ID    string         `json:"id"`
	Name  string         `json:"name"`
	Input map[string]any `json:"input"`
}

// ToolResult wraps a tool execution result for streaming.
type ToolResult struct {
	Output       string         `json:"output"`
	IsError      bool           `json:"is_error"`
	Display      string         `json:"display,omitempty"`
	Presentation map[string]any `json:"presentation,omitempty"`
}

type PermissionDecision int

const (
	PermissionDecisionAllow PermissionDecision = iota
	PermissionDecisionDeny
	PermissionDecisionAlwaysAllow
)
