// Package hook is the public lifecycle-event surface plugins implement.
//
// A hook plugin observes (and optionally rewrites) the agent loop's flow
// without owning the loop itself: PreToolUse can short-circuit a tool
// call or rewrite its arguments; PostToolUse audits results; Session /
// Turn / Loop / Error give panel-style observability.
//
// Pairs with pkg/tool (which defines the tools hooks observe) and
// pkg/llm (which defines the requests they touch). Together they form
// Metis's stable plugin SDK.
//
// Author a hook by writing one of the typed handler funcs and passing it
// to a Registry's Register method:
//
//	r.Register(hook.PreToolUseHandler(func(ctx context.Context, tc hook.Context, in *hook.PreToolUse) *hook.ModifiedPreToolUse {
//	    if in.Tool == "Bash" && containsForbidden(in.Input) {
//	        return &hook.ModifiedPreToolUse{Output: &hook.Output{Content: "denied", IsError: true}}
//	    }
//	    return nil
//	}))
package hook

import (
	"context"
	"strings"
	"sync"
	"unicode/utf8"
)

const (
	maxPostToolUseContextPerHandler = 16 * 1024
	maxPostToolUseContextTotal      = 32 * 1024
	postToolUseContextTruncated     = "\n...[hook context truncated]"
	postToolUseContextOmitted       = "\n...[additional hook context omitted]"
)

// Event identifies a lifecycle event by name. Useful for logging and for
// hooks that want to fan out a single struct rather than register one
// handler per event.
type Event string

const (
	EventPreToolUse   Event = "PreToolUse"
	EventPostToolUse  Event = "PostToolUse"
	EventSessionStart Event = "SessionStart"
	EventSessionEnd   Event = "SessionEnd"
	EventTurnStart    Event = "TurnStart"
	EventTurnEnd      Event = "TurnEnd"
	EventLoopEnd      Event = "LoopEnd"
	EventError        Event = "Error"
	// Added 2026-05-01 to match claude-code's full lifecycle. Each one
	// has a corresponding *Handler type + Emit* method on Registry.
	EventUserPromptSubmit   Event = "UserPromptSubmit"
	EventNotification       Event = "Notification"
	EventPermissionRequest  Event = "PermissionRequest"
	EventPermissionDenied   Event = "PermissionDenied"
	EventPostToolUseFailure Event = "PostToolUseFailure"
	EventSetup              Event = "Setup"
	EventSubagentStart      Event = "SubagentStart"
	EventSubagentStop       Event = "SubagentStop"
	EventCwdChanged         Event = "CwdChanged"
	// EventPreCompact fires just before the agent loop summarizes its
	// history (auto-compaction crossing the threshold, or a manual
	// /compact). Observers use it to back up / log the transcript
	// before it's collapsed. Read-only — matches claude-code's
	// PreCompact hook (settings.json: "PreCompact").
	EventPreCompact Event = "PreCompact"
	// EventPostCompact fires just AFTER an LLM-driven compaction tier
	// (collapse or compact) successfully reduced the live history.
	// Unlike PreCompact it is feedback-capable: a handler may return
	// AdditionalContext that the loop appends as a user message right
	// after the summary boundary, so the model re-anchors (project
	// layout, active branch, key constraints) without re-reading
	// files. Mirrors claude-code's PostCompact hook (settings.json:
	// "PostCompact"); closes metis bug §28.11 (P1).
	EventPostCompact Event = "PostCompact"
)

// Context is the read-only data available to all hook handlers.
type Context struct {
	SessionID string
	Model     string
	Turn      int
}

// PreToolUse is the data passed to PreToolUse handlers — i.e. the call
// the agent loop is about to make. Handlers return *ModifiedPreToolUse
// to either short-circuit (Output non-nil) or rewrite arguments
// (ModifiedInput non-nil).
type PreToolUse struct {
	Context
	Tool  string
	Input map[string]any
}

// ModifiedPreToolUse is what a PreToolUse handler returns.
//   - Output non-nil → skip execution and use this as the tool's result.
//   - ModifiedInput non-nil → run the tool with new arguments.
//   - Halt true → stop the entire current turn after the tool batch
//     (claude-code parity: subprocess hooks signal halt via JSON
//     `{"decision":"halt"}` or process exit code 49). Output is
//     respected first (the model sees the tool_result block) and
//     then the loop short-circuits before issuing the next API call.
//   - all zero → no change, proceed normally.
type ModifiedPreToolUse struct {
	Output        *Output
	ModifiedInput map[string]any
	// PresentationInput is a redacted deep clone of ModifiedInput for approval
	// UIs and event consumers. Execution must continue to use ModifiedInput;
	// presentation surfaces must never receive hook-injected credentials.
	PresentationInput map[string]any
	Halt              bool
	// HaltReason explains why the turn is being halted, surfaced via
	// the loop's stop reason channel. Optional — defaults to
	// "halted by PreToolUse hook" when blank.
	HaltReason string
}

// Output is what a PreToolUse hook returns to short-circuit execution.
type Output struct {
	Content string
	IsError bool
}

// PostToolUse is the data passed to PostToolUse handlers — fired after a
// tool finishes execution, with its rendered output.
type PostToolUse struct {
	Context
	Tool    string
	Input   map[string]any
	Output  string
	IsError bool
}

type postToolUseIDContextKey struct{}

// WithPostToolUseID binds the provider-issued call identifier to a
// PostToolUse handler invocation without changing the public PostToolUse
// struct layout (and therefore without breaking third-party unkeyed literals).
func WithPostToolUseID(ctx context.Context, toolUseID string) context.Context {
	return context.WithValue(ctx, postToolUseIDContextKey{}, toolUseID)
}

// PostToolUseIDFromContext returns the exact call identifier associated with a
// PostToolUse handler invocation. Empty means the emitter did not provide one.
func PostToolUseIDFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	toolUseID, _ := ctx.Value(postToolUseIDContextKey{}).(string)
	return toolUseID
}

// ModifiedPostToolUse is what a PostToolUseContextHandler returns.
// AdditionalContext, when non-empty, is appended to the tool_result
// the MODEL sees (wrapped in a <system-reminder> by the dispatch
// layer) — the channel a formatter/linter hook uses to feed its
// diagnostics back into the conversation. Mirrors claude-code's
// PostToolUse hook additionalContext response field. nil / zero
// value = observe only, no injection.
type ModifiedPostToolUse struct {
	AdditionalContext string
}

// UserPromptSubmit fires when the user submits a prompt. Handlers can
// return *ModifiedUserPromptSubmit to rewrite the prompt or short-circuit
// the turn entirely (Output non-nil).
type UserPromptSubmit struct {
	Context
	Prompt string
}
type ModifiedUserPromptSubmit struct {
	ModifiedPrompt string  // empty = unchanged
	Output         *Output // non-nil = synthesize an assistant reply, skip the LLM
}

// Notification is a side-channel event the agent / runtime emits to
// inform observers without affecting flow (e.g. "context compacted",
// "tool retry"). Read-only.
type Notification struct {
	Context
	Level   string // "info" | "warn" | "error"
	Message string
}

// PermissionRequest fires before the gate prompts the user. Handlers
// may return ModifiedPermissionRequest{Decision: "allow"|"deny"} to
// short-circuit the prompt; nil means "let the gate ask normally".
type PermissionRequest struct {
	Context
	Tool   string
	Input  map[string]any
	Reason string // why the gate is asking (rule miss, mode, etc.)
}
type ModifiedPermissionRequest struct {
	Decision string // "allow" | "deny" | "" (=no override)
	Reason   string
}

// PermissionDenied fires after a tool call was denied (either by gate or
// PreToolUse). Read-only.
type PermissionDenied struct {
	Context
	Tool   string
	Input  map[string]any
	Reason string
}

// PostToolUseFailure fires when a tool returned an error result.
// Distinct from PostToolUse so observers can filter cheaply. Read-only.
type PostToolUseFailure struct {
	Context
	Tool    string
	Input   map[string]any
	Error   string
	Output  string // partial output if any
	Attempt int    // 1-indexed retry attempt
}

// Setup fires once at program startup, before any session. Useful for
// plugins that want to seed config or warm caches.
type Setup struct {
	WorkDir string
	Version string
}

// SubagentStart / SubagentStop fire around Agent-tool spawned subagents.
type SubagentStart struct {
	Context
	SubagentID string
	Prompt     string
	Profile    string // --agent name, if any
}
type SubagentStop struct {
	Context
	SubagentID string
	StopReason string
	Output     string // final assistant text (may be truncated)
}

// CwdChanged fires when the agent changes working directory mid-session
// (Bash `cd`, --add-dir, etc.).
type CwdChanged struct {
	Context
	OldCwd string
	NewCwd string
}

// PreCompact fires just before the loop summarizes its history. Trigger
// is "auto" (threshold crossed) or "manual" (/compact). MessageCount and
// EstimatedTokens describe the transcript about to be compacted, so an
// observer can decide whether to back it up. Read-only.
type PreCompact struct {
	Context
	Trigger         string // "auto" | "manual"
	MessageCount    int
	EstimatedTokens int
}

// PostCompact fires after an LLM-driven compaction tier successfully
// reduced the live history. Tier is "collapse" or "compact" (the cheap
// model-free tiers — snip / microcompact / image-prune — do NOT fire
// this event; they never destroy conversational content wholesale).
// Before/After pairs describe the reduction so observers can log or
// bill it. Handlers may contribute AdditionalContext (see
// ModifiedPostCompact); the loop appends it as a user message right
// after the summary boundary.
type PostCompact struct {
	Context
	Trigger        string // "auto" | "manual"
	Tier           string // "collapse" | "compact"
	BeforeMessages int
	AfterMessages  int
	BeforeTokens   int
	AfterTokens    int
}

// ModifiedPostCompact is the optional PostCompact return. AdditionalContext
// is injected into the conversation as a user message immediately after
// the compact boundary — the P1 use case from bug §28.11 ("用户没法在
// compact 前后 inject 上下文"): re-anchor the model with facts the
// summarizer may have folded away (current branch, build command, the
// user's active constraint). Empty string = no injection.
type ModifiedPostCompact struct {
	AdditionalContext string
}

// Handler signatures for each event type. Plugin authors write one of
// these and pass it to Registry.Register.
type (
	PreToolUseHandler  func(context.Context, Context, *PreToolUse) *ModifiedPreToolUse
	PostToolUseHandler func(context.Context, Context, *PostToolUse)
	// PostToolUseContextHandler is the feedback-capable PostToolUse
	// variant: its return value can inject AdditionalContext into the
	// tool_result. Sync only — the dispatch path consumes the return,
	// so RegisterAsync treats it as sync. Plain observers should keep
	// using PostToolUseHandler (cheaper, async-capable).
	PostToolUseContextHandler func(context.Context, Context, *PostToolUse) *ModifiedPostToolUse
	SessionStartHandler       func(context.Context, Context, string, string) // system, model
	SessionEndHandler         func(context.Context, Context, int, string)    // msgCount, stopReason
	TurnStartHandler          func(context.Context, Context, int)            // turn idx
	TurnEndHandler            func(context.Context, Context, int)            // turn idx
	LoopEndHandler            func(context.Context, Context, string)         // stopReason
	ErrorHandler              func(context.Context, Context, error)
	// 2026-05-01 additions
	UserPromptSubmitHandler   func(context.Context, Context, *UserPromptSubmit) *ModifiedUserPromptSubmit
	NotificationHandler       func(context.Context, Context, *Notification)
	PermissionRequestHandler  func(context.Context, Context, *PermissionRequest) *ModifiedPermissionRequest
	PermissionDeniedHandler   func(context.Context, Context, *PermissionDenied)
	PostToolUseFailureHandler func(context.Context, Context, *PostToolUseFailure)
	SetupHandler              func(context.Context, *Setup)
	SubagentStartHandler      func(context.Context, Context, *SubagentStart)
	SubagentStopHandler       func(context.Context, Context, *SubagentStop)
	CwdChangedHandler         func(context.Context, Context, *CwdChanged)
	PreCompactHandler         func(context.Context, Context, *PreCompact)
	// PostCompactHandler is the feedback-capable PostCompact variant:
	// its return value can inject AdditionalContext after the compact
	// boundary. Sync only — the compaction path consumes the return,
	// so RegisterAsync treats it as sync (same contract as
	// PostToolUseContextHandler).
	PostCompactHandler func(context.Context, Context, *PostCompact) *ModifiedPostCompact
)

// asyncBit is the per-handler async flag stored alongside the handler.
// When true, the registry calls the handler in a goroutine (fire-and-
// forget) and moves on without waiting. When false (default), the
// handler runs inline.
//
// Async is only meaningful for handlers whose return value the registry
// doesn't consume — PostToolUse, SessionEnd, TurnEnd, LoopEnd,
// PostToolUseFailure, Notification, PermissionDenied. PreToolUse
// modifies the call so it MUST stay sync. Same for ALL Setup-style
// handlers whose side effects must happen before the next phase
// starts.
type postToolEntry struct {
	h     PostToolUseHandler
	async bool
}
type sessionEndEntry struct {
	h     SessionEndHandler
	async bool
}
type turnEndEntry struct {
	h     TurnEndHandler
	async bool
}
type postToolFailEntry struct {
	h     PostToolUseFailureHandler
	async bool
}
type notificationEntry struct {
	h     NotificationHandler
	async bool
}
type permDeniedEntry struct {
	h     PermissionDeniedHandler
	async bool
}
type preCompactEntry struct {
	h     PreCompactHandler
	async bool
}

// Registry holds registered hooks, grouped by event type. Thread-safe.
// Pre-action hooks (PreToolUse, PermissionRequest, SessionStart) fire
// synchronously in registration order — their return values gate later
// stages. Post-action hooks (PostToolUse, etc.) accept an async flag
// at registration; async ones run in a goroutine to keep the dispatch
// hot path uncapped by slow webhook handlers.
type Registry struct {
	mu          sync.RWMutex
	preTool     []PreToolUseHandler
	postTool    []postToolEntry
	postToolCtx []PostToolUseContextHandler
	session     []SessionStartHandler
	sessionEnd  []sessionEndEntry
	turnStart   []TurnStartHandler
	turnEnd     []turnEndEntry
	loopEnd     []LoopEndHandler
	errorHook   []ErrorHandler
	// 2026-05-01 additions
	userPrompt    []UserPromptSubmitHandler
	notification  []notificationEntry
	permRequest   []PermissionRequestHandler
	permDenied    []permDeniedEntry
	postToolFail  []postToolFailEntry
	setup         []SetupHandler
	subagentStart []SubagentStartHandler
	subagentStop  []SubagentStopHandler
	cwdChanged    []CwdChangedHandler
	preCompact    []preCompactEntry
	postCompact   []PostCompactHandler
}

func NewRegistry() *Registry { return &Registry{} }

// Register adds a typed hook. Unrecognized handler types are silently
// ignored — pre-Go-1.18 generics-style discriminated dispatch via type
// switch — so that adding a future handler type doesn't break old plugins
// using `Register(any...)`.
func (r *Registry) Register(handler any) {
	r.register(handler, false)
}

// RegisterAsync is the same as Register but flags the handler so the
// registry runs it in a goroutine instead of inline. Only meaningful
// for post-action hooks (PostToolUse, SessionEnd, TurnEnd, …) whose
// return value the registry doesn't consume. Pre-action hooks
// (PreToolUse, PermissionRequest, SessionStart, TurnStart, Setup)
// silently ignore the flag — they MUST stay synchronous.
//
// Async hooks lose ordering guarantees and may continue to run after
// the session ends. They're appropriate for: telemetry to remote
// services, slow webhooks (Slack, PagerDuty), heavy logging.
//
// Mirrors claude-code's `async: true` hook setting in settings.json.
func (r *Registry) RegisterAsync(handler any) {
	r.register(handler, true)
}

// register is the shared implementation. If async is true and the
// handler is one of the post-action types, the entry is stored with
// async=true.
func (r *Registry) register(handler any, async bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	switch h := handler.(type) {
	case PreToolUseHandler:
		// Sync only — async flag is ignored (it would race with the
		// dispatch path that consumes the return value).
		r.preTool = append(r.preTool, h)
	case PostToolUseHandler:
		r.postTool = append(r.postTool, postToolEntry{h: h, async: async})
	case PostToolUseContextHandler:
		// Sync only — the dispatch path consumes the return value.
		r.postToolCtx = append(r.postToolCtx, h)
	case SessionStartHandler:
		r.session = append(r.session, h) // sync — gates startup
	case SessionEndHandler:
		r.sessionEnd = append(r.sessionEnd, sessionEndEntry{h: h, async: async})
	case TurnStartHandler:
		r.turnStart = append(r.turnStart, h) // sync — gates the turn
	case TurnEndHandler:
		r.turnEnd = append(r.turnEnd, turnEndEntry{h: h, async: async})
	case LoopEndHandler:
		r.loopEnd = append(r.loopEnd, h) // typically sync — exit observers
	case ErrorHandler:
		r.errorHook = append(r.errorHook, h) // sync — diagnostic
	case UserPromptSubmitHandler:
		r.userPrompt = append(r.userPrompt, h) // sync — gate on prompt
	case NotificationHandler:
		r.notification = append(r.notification, notificationEntry{h: h, async: async})
	case PermissionRequestHandler:
		r.permRequest = append(r.permRequest, h) // sync — must gate
	case PermissionDeniedHandler:
		r.permDenied = append(r.permDenied, permDeniedEntry{h: h, async: async})
	case PostToolUseFailureHandler:
		r.postToolFail = append(r.postToolFail, postToolFailEntry{h: h, async: async})
	case SetupHandler:
		r.setup = append(r.setup, h) // sync — must gate setup
	case SubagentStartHandler:
		r.subagentStart = append(r.subagentStart, h)
	case SubagentStopHandler:
		r.subagentStop = append(r.subagentStop, h)
	case CwdChangedHandler:
		r.cwdChanged = append(r.cwdChanged, h)
	case PreCompactHandler:
		r.preCompact = append(r.preCompact, preCompactEntry{h: h, async: async})
	case PostCompactHandler:
		// Sync only — the compaction path consumes the return value.
		r.postCompact = append(r.postCompact, h)
	}
}

// EmitPreToolUse calls PreToolUse handlers. Returns the first non-nil
// ModifiedPreToolUse — short-circuiting any later handlers in the chain.
// Callers (the agent loop) interpret nil as "proceed unchanged".
func (r *Registry) EmitPreToolUse(ctx context.Context, tc Context, in *PreToolUse) *ModifiedPreToolUse {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, h := range r.preTool {
		if mod := h(ctx, tc, in); mod != nil {
			return mod
		}
	}
	return nil
}

// EmitPostToolUse fans out to all PostToolUse handlers. Sync handlers
// run inline; async ones spawn a goroutine and detach.
func (r *Registry) EmitPostToolUse(ctx context.Context, tc Context, in *PostToolUse) {
	r.mu.RLock()
	handlers := r.postTool
	r.mu.RUnlock()
	for _, e := range handlers {
		if e.async {
			go e.h(ctx, tc, in)
		} else {
			e.h(ctx, tc, in)
		}
	}
}

// EmitPostToolUseContext fans out to the feedback-capable PostToolUse
// handlers and returns their AdditionalContext strings joined by
// newlines ("" when nothing was contributed). The dispatch layer
// appends the result to the tool_result as a <system-reminder> so the
// model sees hook diagnostics (lint output, format fixes) next turn.
func (r *Registry) EmitPostToolUseContext(ctx context.Context, tc Context, in *PostToolUse) string {
	r.mu.RLock()
	handlers := r.postToolCtx
	r.mu.RUnlock()
	var result strings.Builder
	overflow := false
	for _, h := range handlers {
		mod := h(ctx, tc, in)
		if mod == nil || mod.AdditionalContext == "" {
			continue
		}

		part := truncatePostToolUseContext(mod.AdditionalContext, maxPostToolUseContextPerHandler)
		separatorBytes := 0
		if result.Len() > 0 {
			separatorBytes = 1
		}
		if result.Len()+separatorBytes+len(part) > maxPostToolUseContextTotal {
			overflow = true
			continue
		}
		if result.Len() > 0 {
			result.WriteByte('\n')
		}
		result.WriteString(part)
	}
	if overflow {
		prefixLimit := maxPostToolUseContextTotal - len(postToolUseContextOmitted)
		prefix := utf8SafePrefix(result.String(), prefixLimit)
		result.Reset()
		result.WriteString(prefix)
		result.WriteString(postToolUseContextOmitted)
	}
	return result.String()
}

func truncatePostToolUseContext(value string, limit int) string {
	if limit <= 0 {
		return ""
	}
	if len(value) <= limit {
		return value
	}
	if limit <= len(postToolUseContextTruncated) {
		return postToolUseContextTruncated[:limit]
	}

	return utf8SafePrefix(value, limit-len(postToolUseContextTruncated)) + postToolUseContextTruncated
}

func utf8SafePrefix(value string, limit int) string {
	if limit <= 0 {
		return ""
	}
	if len(value) <= limit {
		return value
	}
	for limit > 0 && !utf8.ValidString(value[:limit]) {
		limit--
	}
	return value[:limit]
}

func (r *Registry) EmitSessionStart(ctx context.Context, tc Context, system, model string) {
	r.mu.RLock()
	handlers := r.session
	r.mu.RUnlock()
	for _, h := range handlers {
		h(ctx, tc, system, model)
	}
}

func (r *Registry) EmitSessionEnd(ctx context.Context, tc Context, msgCount int, stopReason string) {
	r.mu.RLock()
	handlers := r.sessionEnd
	r.mu.RUnlock()
	for _, e := range handlers {
		if e.async {
			go e.h(ctx, tc, msgCount, stopReason)
		} else {
			e.h(ctx, tc, msgCount, stopReason)
		}
	}
}

func (r *Registry) EmitTurnStart(ctx context.Context, tc Context, turn int) {
	r.mu.RLock()
	handlers := r.turnStart
	r.mu.RUnlock()
	for _, h := range handlers {
		h(ctx, tc, turn)
	}
}

func (r *Registry) EmitTurnEnd(ctx context.Context, tc Context, turn int) {
	r.mu.RLock()
	handlers := r.turnEnd
	r.mu.RUnlock()
	for _, e := range handlers {
		if e.async {
			go e.h(ctx, tc, turn)
		} else {
			e.h(ctx, tc, turn)
		}
	}
}

func (r *Registry) EmitLoopEnd(ctx context.Context, tc Context, stopReason string) {
	r.mu.RLock()
	handlers := r.loopEnd
	r.mu.RUnlock()
	for _, h := range handlers {
		h(ctx, tc, stopReason)
	}
}

func (r *Registry) EmitError(ctx context.Context, tc Context, err error) {
	r.mu.RLock()
	handlers := r.errorHook
	r.mu.RUnlock()
	for _, h := range handlers {
		h(ctx, tc, err)
	}
}

// EmitUserPromptSubmit returns the first non-nil mod (short-circuit
// semantics, like PreToolUse). Callers interpret nil as "send the user's
// raw prompt to the LLM".
func (r *Registry) EmitUserPromptSubmit(ctx context.Context, tc Context, in *UserPromptSubmit) *ModifiedUserPromptSubmit {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, h := range r.userPrompt {
		if mod := h(ctx, tc, in); mod != nil {
			return mod
		}
	}
	return nil
}

func (r *Registry) EmitNotification(ctx context.Context, tc Context, n *Notification) {
	r.mu.RLock()
	handlers := r.notification
	r.mu.RUnlock()
	for _, e := range handlers {
		if e.async {
			go e.h(ctx, tc, n)
		} else {
			e.h(ctx, tc, n)
		}
	}
}

// EmitPreCompact fans out to all PreCompact handlers before the loop
// summarizes its history. Read-only (observers back up / log the
// transcript); sync handlers run inline, async ones detach.
func (r *Registry) EmitPreCompact(ctx context.Context, tc Context, in *PreCompact) {
	r.mu.RLock()
	handlers := r.preCompact
	r.mu.RUnlock()
	for _, e := range handlers {
		if e.async {
			go e.h(ctx, tc, in)
		} else {
			e.h(ctx, tc, in)
		}
	}
}

// EmitPostCompact fans out to the feedback-capable PostCompact handlers
// and returns their AdditionalContext strings joined by newlines (""
// when nothing was contributed). The compaction path appends the result
// as a user message right after the summary boundary, so the model sees
// hook-contributed anchors (branch, build command, constraints) at the
// next request without re-reading files. All handlers run sync — the
// caller consumes the return inside the compaction window.
func (r *Registry) EmitPostCompact(ctx context.Context, tc Context, in *PostCompact) string {
	r.mu.RLock()
	handlers := r.postCompact
	r.mu.RUnlock()
	var parts []string
	for _, h := range handlers {
		if mod := h(ctx, tc, in); mod != nil && strings.TrimSpace(mod.AdditionalContext) != "" {
			parts = append(parts, mod.AdditionalContext)
		}
	}
	return strings.Join(parts, "\n")
}

// EmitPermissionRequest returns the first non-nil decision override; nil
// = "let the gate ask normally".
func (r *Registry) EmitPermissionRequest(ctx context.Context, tc Context, p *PermissionRequest) *ModifiedPermissionRequest {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, h := range r.permRequest {
		if mod := h(ctx, tc, p); mod != nil {
			return mod
		}
	}
	return nil
}

func (r *Registry) EmitPermissionDenied(ctx context.Context, tc Context, p *PermissionDenied) {
	r.mu.RLock()
	handlers := r.permDenied
	r.mu.RUnlock()
	for _, e := range handlers {
		if e.async {
			go e.h(ctx, tc, p)
		} else {
			e.h(ctx, tc, p)
		}
	}
}

func (r *Registry) EmitPostToolUseFailure(ctx context.Context, tc Context, p *PostToolUseFailure) {
	r.mu.RLock()
	handlers := r.postToolFail
	r.mu.RUnlock()
	for _, e := range handlers {
		if e.async {
			go e.h(ctx, tc, p)
		} else {
			e.h(ctx, tc, p)
		}
	}
}

func (r *Registry) EmitSetup(ctx context.Context, s *Setup) {
	r.mu.RLock()
	handlers := r.setup
	r.mu.RUnlock()
	for _, h := range handlers {
		h(ctx, s)
	}
}

func (r *Registry) EmitSubagentStart(ctx context.Context, tc Context, s *SubagentStart) {
	r.mu.RLock()
	handlers := r.subagentStart
	r.mu.RUnlock()
	for _, h := range handlers {
		h(ctx, tc, s)
	}
}

func (r *Registry) EmitSubagentStop(ctx context.Context, tc Context, s *SubagentStop) {
	r.mu.RLock()
	handlers := r.subagentStop
	r.mu.RUnlock()
	for _, h := range handlers {
		h(ctx, tc, s)
	}
}

func (r *Registry) EmitCwdChanged(ctx context.Context, tc Context, c *CwdChanged) {
	r.mu.RLock()
	handlers := r.cwdChanged
	r.mu.RUnlock()
	for _, h := range handlers {
		h(ctx, tc, c)
	}
}
