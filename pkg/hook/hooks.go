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
	"sync"
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
//   - both nil → no change, proceed normally.
type ModifiedPreToolUse struct {
	Output        *Output
	ModifiedInput map[string]any
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

// Handler signatures for each event type. Plugin authors write one of
// these and pass it to Registry.Register.
type (
	PreToolUseHandler   func(context.Context, Context, *PreToolUse) *ModifiedPreToolUse
	PostToolUseHandler  func(context.Context, Context, *PostToolUse)
	SessionStartHandler func(context.Context, Context, string, string) // system, model
	SessionEndHandler   func(context.Context, Context, int, string)    // msgCount, stopReason
	TurnStartHandler    func(context.Context, Context, int)            // turn idx
	TurnEndHandler      func(context.Context, Context, int)            // turn idx
	LoopEndHandler      func(context.Context, Context, string)         // stopReason
	ErrorHandler        func(context.Context, Context, error)
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
)

// Registry holds registered hooks, grouped by event type. Thread-safe.
// Hooks fire synchronously in registration order so a plugin can rely on
// observing the call before downstream observers do.
type Registry struct {
	mu         sync.RWMutex
	preTool    []PreToolUseHandler
	postTool   []PostToolUseHandler
	session    []SessionStartHandler
	sessionEnd []SessionEndHandler
	turnStart  []TurnStartHandler
	turnEnd    []TurnEndHandler
	loopEnd    []LoopEndHandler
	errorHook  []ErrorHandler
	// 2026-05-01 additions
	userPrompt    []UserPromptSubmitHandler
	notification  []NotificationHandler
	permRequest   []PermissionRequestHandler
	permDenied    []PermissionDeniedHandler
	postToolFail  []PostToolUseFailureHandler
	setup         []SetupHandler
	subagentStart []SubagentStartHandler
	subagentStop  []SubagentStopHandler
	cwdChanged    []CwdChangedHandler
}

func NewRegistry() *Registry { return &Registry{} }

// Register adds a typed hook. Unrecognized handler types are silently
// ignored — pre-Go-1.18 generics-style discriminated dispatch via type
// switch — so that adding a future handler type doesn't break old plugins
// using `Register(any...)`.
func (r *Registry) Register(handler any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	switch h := handler.(type) {
	case PreToolUseHandler:
		r.preTool = append(r.preTool, h)
	case PostToolUseHandler:
		r.postTool = append(r.postTool, h)
	case SessionStartHandler:
		r.session = append(r.session, h)
	case SessionEndHandler:
		r.sessionEnd = append(r.sessionEnd, h)
	case TurnStartHandler:
		r.turnStart = append(r.turnStart, h)
	case TurnEndHandler:
		r.turnEnd = append(r.turnEnd, h)
	case LoopEndHandler:
		r.loopEnd = append(r.loopEnd, h)
	case ErrorHandler:
		r.errorHook = append(r.errorHook, h)
	case UserPromptSubmitHandler:
		r.userPrompt = append(r.userPrompt, h)
	case NotificationHandler:
		r.notification = append(r.notification, h)
	case PermissionRequestHandler:
		r.permRequest = append(r.permRequest, h)
	case PermissionDeniedHandler:
		r.permDenied = append(r.permDenied, h)
	case PostToolUseFailureHandler:
		r.postToolFail = append(r.postToolFail, h)
	case SetupHandler:
		r.setup = append(r.setup, h)
	case SubagentStartHandler:
		r.subagentStart = append(r.subagentStart, h)
	case SubagentStopHandler:
		r.subagentStop = append(r.subagentStop, h)
	case CwdChangedHandler:
		r.cwdChanged = append(r.cwdChanged, h)
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

// EmitPostToolUse fans out to all PostToolUse handlers.
func (r *Registry) EmitPostToolUse(ctx context.Context, tc Context, in *PostToolUse) {
	r.mu.RLock()
	handlers := r.postTool
	r.mu.RUnlock()
	for _, h := range handlers {
		h(ctx, tc, in)
	}
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
	for _, h := range handlers {
		h(ctx, tc, msgCount, stopReason)
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
	for _, h := range handlers {
		h(ctx, tc, turn)
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
	for _, h := range handlers {
		h(ctx, tc, n)
	}
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
	for _, h := range handlers {
		h(ctx, tc, p)
	}
}

func (r *Registry) EmitPostToolUseFailure(ctx context.Context, tc Context, p *PostToolUseFailure) {
	r.mu.RLock()
	handlers := r.postToolFail
	r.mu.RUnlock()
	for _, h := range handlers {
		h(ctx, tc, p)
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
