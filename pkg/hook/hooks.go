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
