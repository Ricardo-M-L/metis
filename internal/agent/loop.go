package agent

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/Ricardo-M-L/metis/internal/agent/transcript"
	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/memory"
	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

// Loop drives the message → tools → message cycle.
//
// Design (synthesized from reference projects):
//   - Streaming-first: emit text deltas as they arrive (Claude Code pattern)
//   - Parallel-safe tool execution: ConcurrencySafe tools fan out, exclusive ones serialize
//   - Iteration budget with grace turn: Hermes pattern, allow one final call after budget
//   - Cascading permissions: Gate decides; if Ask, surface to UI via PermissionRequest
//   - Hooks: PreToolUse / PostToolUse at every tool boundary
//   - PlanMode: collect tool calls and emit as EventPlan, don't execute
//   - AutoCompaction: when token fraction exceeds threshold, compact via Compactor.
//
// Implementation is split across sibling files to keep each phase focused:
//   - streaming.go        consumeStream + filterToolUses
//   - dispatch.go         executeBatch + runOne + toolSpecs
//   - permission_ask.go   askPermission
//   - compaction_check.go maybeCompact
//   - plan_emit.go        emitPlan
type Loop struct {
	Provider   llm.Provider
	Registry   *tools.Registry
	Gate       *permission.Gate
	Hooks      *HookRegistry
	System     string
	Model      string
	MaxIters   int
	GraceCalls int

	// Memory provides persistent memory for system prompt injection.
	// When set, BuildContext() is called to inject memory into each request.
	Memory *memory.MemoryManager

	// PlanMode: collect tool calls but do NOT execute; emit as EventPlan.
	PlanMode bool

	// Compactor handles automatic context window compaction. nil = disabled.
	Compactor *Compactor

	// Detector monitors tool call patterns; aborts run when ShouldAbort returns true.
	// nil = disabled.
	Detector *LoopDetector

	// Effort sets the reasoning intensity for the next request:
	//   ""        → don't send a thinking/reasoning field (provider default)
	//   "low"     → small budget, fastest answers
	//   "medium"  → balanced
	//   "high"    → deep reasoning, slowest
	// Maps to Anthropic thinking.budget_tokens and OpenAI reasoning_effort.
	Effort llm.Effort

	// Fast collapses the next turn's resource use:
	//   - effort drops to "low" (overrides Effort for the request)
	//   - max_tokens halved via Request.MaxTokens override
	// Useful for "I just need a quick answer" rather than spinning up
	// deep deliberation on a one-line clarification.
	Fast bool

	mu       sync.RWMutex
	Messages []llm.Message
	turnIdx  int
	iterIdx  int

	// DistillEvery controls auto-distillation cadence. Every N
	// successful turns, the loop fires a background LLM call that
	// extracts durable facts from the latest user/assistant
	// exchange and writes them to archival memory. Default 5 — set
	// to 0 to disable (e.g. tests, cron-fire isolated mode where
	// per-turn distillation noise dominates the actual work).
	DistillEvery int
}

func NewLoop(p llm.Provider, r *tools.Registry, g *permission.Gate, h *HookRegistry, system string, maxIter int) *Loop {
	if h == nil {
		h = NewHookRegistry()
	}
	if maxIter <= 0 {
		maxIter = 50
	}
	return &Loop{
		Provider:   p,
		Registry:   r,
		Gate:       g,
		Hooks:      h,
		System:     system,
		MaxIters:   maxIter,
		GraceCalls: 1,
	}
}

// AppendUser adds a user message.
func (l *Loop) AppendUser(text string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.Messages = append(l.Messages, llm.Message{
		Role:    llm.RoleUser,
		Content: []llm.ContentBlock{{Type: "text", Text: text}},
	})
}

// History returns a snapshot.
func (l *Loop) History() []llm.Message {
	l.mu.Lock()
	defer l.mu.Unlock()
	return transcript.Snapshot(l.Messages)
}

// Reset clears the conversation.
func (l *Loop) Reset() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.Messages = nil
	l.turnIdx = 0
	l.iterIdx = 0
}

// Restore replaces the conversation history with the supplied messages.
// Iteration counters are reset because the next call is a new turn,
// not a resumption — caller is responsible for not mid-stream restoring.
//
// Used by the cron scheduler to switch between per-job histories under
// SessionMode "persistent" / "main" without throwing away progress made
// in earlier firings.
func (l *Loop) Restore(messages []llm.Message) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if messages == nil {
		l.Messages = nil
	} else {
		l.Messages = append([]llm.Message(nil), messages...)
	}
	l.turnIdx = 0
	l.iterIdx = 0
}

// UndoLastTurn pops the most recent user→assistant exchange (including any
// tool_use/tool_result pingpong inside that turn) off the message history.
//
// Returns true on success. Returns false if the history is empty or there's
// no plain-text user message to fall back to — caller should treat that as
// "nothing to undo" and emit a hint rather than a confirmation.
//
// Iteration counters are NOT rewound: the next turn keeps incrementing
// turnIdx so loop-detection / hooks see a coherent timeline. Compaction
// state is also preserved.
func (l *Loop) UndoLastTurn() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	out, ok := transcript.Undo(l.Messages)
	if !ok {
		return false
	}
	l.Messages = out
	return true
}

// CountTurns returns how many user prompts have been delivered so far.
// Tool-result-only user messages don't count.
func (l *Loop) CountTurns() int {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return transcript.CountTurns(l.Messages)
}

// IterIdx returns the current iteration count.
func (l *Loop) IterIdx() int {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.iterIdx
}

// Run drives one user turn end-to-end. Events are emitted on `out`; the
// caller closes `out` after Run returns.
//
// Each iteration:
//  1. maybeCompact — auto-compact if context is full
//  2. buildRequest — system + memory + messages + tools + effort/fast knobs
//  3. Provider.Stream → consumeStream → assistant blocks
//  4. If stop reason ≠ tool_use → emit LoopDone, return
//  5. Filter tool_uses; if none → return
//  6. PlanMode → emitPlan, return
//  7. executeBatch → results
//  8. Append assistant + tool_results, emit TurnEnd
//  9. Loop-detect / max-iter / grace-call checks
func (l *Loop) Run(ctx context.Context, out chan<- Event) error {
	tc := HookContext{Model: l.Model, Turn: l.turnIdx}
	l.Hooks.EmitSessionStart(ctx, tc, l.System, l.Model)

	// Deferred SessionEnd ensures hook is called on all exit paths.
	var stopReason string
	defer func() {
		l.mu.RLock()
		msgCount := len(l.Messages)
		l.mu.RUnlock()
		l.Hooks.EmitSessionEnd(ctx, tc, msgCount, stopReason)
	}()

	specs := l.toolSpecs()
	graceUsed := 0

	for {
		l.mu.Lock()
		l.iterIdx++
		curIter := l.iterIdx
		l.mu.Unlock()
		l.Hooks.EmitTurnStart(ctx, tc, curIter)

		l.maybeCompact(ctx, out)

		req := l.buildRequest(specs)

		stream, err := l.Provider.Stream(ctx, req)
		if err != nil {
			if l.tryRecoverOverflow(ctx, err, out) {
				// Compaction reduced history; rebuild request and retry once.
				req = l.buildRequest(specs)
				stream, err = l.Provider.Stream(ctx, req)
			}
			if err != nil {
				l.Hooks.EmitError(ctx, tc, err)
				emit(ctx, out, Event{Kind: EventError, Err: err})
				return err
			}
		}
		assistant, stop, usage, err := l.consumeStream(ctx, stream, out)
		stream.Close()
		if err != nil {
			l.Hooks.EmitError(ctx, tc, err)
			emit(ctx, out, Event{Kind: EventError, Err: err})
			return err
		}
		if usage != nil {
			emit(ctx, out, Event{Kind: EventTokens, InputTokens: usage.in, OutputTokens: usage.out})
		}

		l.mu.Lock()
		l.Messages = append(l.Messages, llm.Message{Role: llm.RoleAssistant, Content: assistant})
		l.mu.Unlock()

		if stop != "tool_use" {
			stopReason = stop
			// Reset per-tool counters so the next user turn starts clean —
			// otherwise `Read x5 → end_turn → Read x5` looks like 10 consecutive Reads.
			if l.Detector != nil {
				l.Detector.RecordProgress()
			}
			// Auto-distillation: every N turns, extract durable facts
			// from the most recent user/assistant exchange and write
			// to archival memory. Fire-and-forget goroutine —
			// distillation is "nice to have", a slow / failing LLM
			// call shouldn't block turn completion.
			l.maybeDistill()
			l.Hooks.EmitLoopEnd(ctx, tc, stop)
			emit(ctx, out, Event{Kind: EventLoopDone, StopReason: stop})
			return nil
		}

		toolUses := filterToolUses(assistant)
		if len(toolUses) == 0 {
			stopReason = "no_tool_calls"
			l.Hooks.EmitLoopEnd(ctx, tc, "no_tool_calls")
			emit(ctx, out, Event{Kind: EventLoopDone, StopReason: "no_tool_calls"})
			return nil
		}

		// Plan Mode: collect and emit, don't execute.
		if l.PlanMode {
			stopReason = "plan_mode"
			l.emitPlan(ctx, toolUses, out)
			return nil
		}

		// Execute tool batch.
		results, err := l.executeBatch(ctx, toolUses, out, tc)
		if err != nil {
			stopReason = "error"
			emit(ctx, out, Event{Kind: EventError, Err: err})
			return err
		}
		l.mu.Lock()
		l.Messages = append(l.Messages, llm.Message{Role: llm.RoleUser, Content: results})
		l.mu.Unlock()

		l.Hooks.EmitTurnEnd(ctx, tc, l.turnIdx)
		emit(ctx, out, Event{Kind: EventTurnEnd})
		l.turnIdx++

		// Loop detector: abort when repetitive patterns exceed thresholds.
		if l.Detector != nil && l.Detector.ShouldAbort() {
			stats := l.Detector.Stats()
			msg := fmt.Sprintf("loop detector aborted: %d total tool calls", stats.GlobalCount)
			stopReason = "loop_detected"
			l.Hooks.EmitLoopEnd(ctx, tc, "loop_detected")
			emit(ctx, out, Event{Kind: EventInfo, Info: msg})
			emit(ctx, out, Event{Kind: EventLoopDone, StopReason: "loop_detected"})
			return nil
		}

		// Budget check with grace call.
		if curIter >= l.MaxIters {
			if graceUsed < l.GraceCalls {
				graceUsed++
				continue
			}
			stopReason = "max_iterations"
			l.Hooks.EmitLoopEnd(ctx, tc, "max_iterations")
			emit(ctx, out, Event{Kind: EventInfo, Info: fmt.Sprintf("budget exhausted (%d iters, 1 grace used)", l.MaxIters)})
			emit(ctx, out, Event{Kind: EventLoopDone, StopReason: "max_iterations"})
			return nil
		}
	}
}

// buildRequest assembles the per-iteration LLM Request under l.mu so the
// snapshot of Messages and System+Memory composition is consistent.
func (l *Loop) buildRequest(specs []llm.ToolSpec) llm.Request {
	l.mu.Lock()
	defer l.mu.Unlock()
	system := l.System
	if l.Memory != nil {
		if memCtx := l.Memory.BuildContext(); memCtx != "" {
			system = system + "\n\n" + memCtx
		}
	}
	req := llm.Request{
		Model:    l.Model,
		System:   system,
		Messages: append([]llm.Message(nil), l.Messages...),
		Tools:    specs,
		Stream:   true,
		Effort:   l.Effort,
	}
	// Fast mode is a pure request-time override. We don't mutate
	// l.Effort because the user's persistent /effort preference
	// should survive a transient /fast toggle.
	if l.Fast {
		req.Effort = llm.EffortLow
		req.MaxTokens = l.Provider.MaxContextTokens() / 16
		if req.MaxTokens > 4096 {
			req.MaxTokens = 4096
		}
	}
	return req
}

// maybeDistill runs auto-distillation on the most recent user/asst
// exchange when the turn counter hits the configured cadence. Spawns
// a background goroutine so it never blocks the agent loop's return
// — distillation is a "nice to have" and a slow LLM call shouldn't
// stall the user's next prompt.
//
// Uses a fresh background context (NOT the request context that's
// about to be cancelled when Run returns). 30s timeout so a hung
// provider doesn't leak goroutines.
func (l *Loop) maybeDistill() {
	if l.Memory == nil || l.Provider == nil {
		return
	}
	cadence := l.DistillEvery
	if cadence <= 0 {
		// Default 5 turns — small enough to keep memory current,
		// large enough to amortize the LLM cost across multiple
		// turns. Caller can override via Loop.DistillEvery.
		cadence = 5
	}
	l.mu.Lock()
	turn := l.turnIdx
	l.mu.Unlock()
	if turn == 0 || turn%cadence != 0 {
		return
	}
	userMsg, asstMsg := l.lastExchange()
	if userMsg == "" || asstMsg == "" {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = l.Memory.DistillTurn(ctx, l.Provider, userMsg, asstMsg)
	}()
}

// lastExchange walks the message history backwards to find the most
// recent user→assistant pair. Returns ("", "") when the history is
// empty or doesn't contain a complete exchange yet.
//
// Pulls only `text` content blocks — tool_use / tool_result blocks
// aren't useful for distillation prompts (they're transient state,
// not durable facts).
func (l *Loop) lastExchange() (userMsg, asstMsg string) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	for i := len(l.Messages) - 1; i >= 0; i-- {
		m := l.Messages[i]
		if m.Role == llm.RoleAssistant && asstMsg == "" {
			asstMsg = textOf(m)
			continue
		}
		if m.Role == llm.RoleUser && asstMsg != "" {
			userMsg = textOf(m)
			return
		}
	}
	return
}

// textOf concatenates all text-type ContentBlocks in a message,
// ignoring tool_use / tool_result blocks. Keeps distillation focused
// on natural-language exchanges.
func textOf(m llm.Message) string {
	var parts []string
	for _, b := range m.Content {
		if b.Type == "text" && b.Text != "" {
			parts = append(parts, b.Text)
		}
	}
	if len(parts) == 0 {
		return ""
	}
	out := parts[0]
	for _, p := range parts[1:] {
		out += "\n" + p
	}
	return out
}

// emit pushes ev to ch. If ch is full, emit blocks until either there's
// room or ctx is cancelled — silently dropping events would corrupt
// EventPermissionRequest (the consumer holds the only reply channel) and
// truncate text/tool deltas under load. ctx==nil is treated as background.
func emit(ctx context.Context, ch chan<- Event, ev Event) {
	if ch == nil {
		return
	}
	if ctx == nil {
		ch <- ev
		return
	}
	select {
	case ch <- ev:
	case <-ctx.Done():
	}
}
