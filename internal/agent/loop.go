package agent

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/Ricardo-M-L/metis/internal/agent/transcript"
	"github.com/Ricardo-M-L/metis/internal/jobs"
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
	Provider llm.Provider
	Registry *tools.Registry
	Gate     *permission.Gate
	Hooks    *HookRegistry
	System   string
	// SystemSections is the typed-section form of the system prompt.
	// When non-empty, buildRequest passes it through llm.Request so the
	// Anthropic provider can emit per-section cache_control. Memory
	// context is appended as its own Volatile=true section so memory
	// updates don't invalidate the addendum cache. nil → fall back to
	// the System string + boundary-marker parsing path.
	SystemSections []llm.SystemSection
	Model          string
	MaxIters       int
	GraceCalls     int

	// Memory provides persistent memory for system prompt injection.
	// When set, BuildContext() is called to inject memory into each request.
	Memory *memory.MemoryManager

	// PlanMode: collect tool calls but do NOT execute; emit as EventPlan.
	PlanMode bool

	// ShortToolDescriptions tells the dispatch layer to consult
	// tools.ShortDescriptor for each tool's spec instead of the full
	// Description() body. Set by sub-agent setup or by METIS_SIMPLE=1
	// boot. When true, dispatch picks tool.ShortDescription() if the
	// tool implements it; otherwise falls back to the existing
	// first-paragraph truncation. Verbatim sub-agent prompts shrink
	// 8-15% per turn on a 5-core-tool registry without changing
	// behavior on the main loop.
	ShortToolDescriptions bool

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

	// haltSignal carries a "stop after this iteration" signal raised
	// by a PreToolUse hook returning Halt=true (claude-code parity:
	// subprocess hooks signal halt via JSON `{"decision":"halt"}`
	// or exit code 49). The reason is surfaced as the turn's stop
	// reason so the user / transcript reads the halt provenance,
	// not a generic "stopped".
	//
	// Cleared at the start of each Run.
	haltRequested bool
	haltReason    string

	// DistillEvery controls auto-distillation cadence. Every N
	// successful turns, the loop fires a background LLM call that
	// extracts durable facts from the latest user/assistant
	// exchange and writes them to archival memory. Default 5 — set
	// to 0 to disable (e.g. tests, cron-fire isolated mode where
	// per-turn distillation noise dominates the actual work).
	DistillEvery int

	// ContextWindow is the model's input cap, fed in by BuildAgentLoop
	// from the provider's MaxContextTokens. Used as the denominator
	// for the auto-mode lazy-tool trigger (see lazy_tools.go). 0 means
	// "unknown" — the auto path silently disables and full schemas
	// are sent. The mode itself comes from the ENABLE_TOOL_SEARCH env
	// var; this field is just the budget input.
	ContextWindow int

	// JobNotify is the receive end of the jobs.Registry notification
	// channel. The Run loop drains it at every iteration boundary and
	// appends a `<job_notification>` system-reminder user message so
	// the model knows its background commands finished. nil means the
	// job pool isn't wired (sub-agents, headless tests).
	JobNotify <-chan jobs.Notification

	// Jobs is the same registry that produced JobNotify, exposed so
	// the TUI can render a "⚙ N jobs" status bar chip without
	// reaching into the runtime layer. Optional — nil hides the chip.
	Jobs *jobs.Registry

	// PeerInbox is the receive end of this sub-agent's Mailbox in the
	// shared Roster (G.3, 2026-05-12). When set, Run drains it at
	// each iteration boundary and injects a <peer_message>
	// system-reminder so the model sees what other teammates told it
	// since the last turn. nil for the top-level user loop (which
	// has no Roster entry) and for headless tests.
	PeerInbox <-chan PeerMessage

	// DreamNotify is the receive end of the auto-memory extractor's
	// completion channel (G.5, 2026-05-12). When the extractor's
	// background fork finishes, it posts a DreamNotification here
	// and the Run loop drains it on the next iteration boundary,
	// synthesizing a <memory_consolidation_done> system-reminder so
	// the model knows recent insights have been persisted. nil means
	// auto-memory isn't wired (the common case for sub-agents and
	// `metis run` one-shots).
	DreamNotify <-chan DreamNotification

	// Monitors is the per-line pattern-match registry. The Monitor
	// tool registers a Watch on a freshly-spawned background job and
	// the loop drains MonitorEvents at every iteration boundary,
	// injecting `<monitor_event>` system-reminder user messages so
	// the model sees pattern matches in time to react. nil disables
	// the Monitor tool path entirely.
	Monitors *MonitorRegistry

	// steerBuf accumulates mid-turn user input (the "steering" feature
	// from claude-code: user can type while the agent is mid-loop, and
	// the new instruction is prepended to the next iteration's user
	// message instead of waiting for the turn to fully end). Drained
	// at iteration boundary in iterate(). Lock via mu to coordinate
	// with the chat surface goroutine that calls SteerInject.
	steerBuf []string

	// compactCircuitNoticeSent gates the "auto-compaction disabled"
	// info event in compaction_check.go to one emission per tripped
	// state. Reset to false whenever a Compact() call succeeds, so a
	// later failure run can re-emit the notice.
	compactCircuitNoticeSent bool

	// AutoMemory controls the openclaude-style "extract memorable
	// facts at turn boundaries" behaviour. Off by default — opting
	// in costs an extra LLM call every AutoMemoryEvery turns.
	AutoMemory bool
	// AutoMemoryEvery overrides the default 10-turn cadence (legacy v1
	// path; ignored by the v2 LoopEnd-driven extractor).
	AutoMemoryEvery int
	// lastAutoMemoryTurn dedupes within a single Run when compaction
	// re-enters Run for the same logical turn boundary (legacy v1).
	lastAutoMemoryTurn int

	// autoMemExtractor is the v2 extractor lazily constructed when
	// AutoMemory is enabled and the LoopEnd hook fires. Nil when
	// auto-memory is off.
	autoMemExtractor *AutoMemoryExtractor

	// CacheStats accumulates a per-turn ring of cache_create / cache_read
	// metrics + the input fingerprint, so MetisInfo can surface "your
	// last turn invalidated the cache because <field> changed".
	// Allocated lazily — nil disables tracking gracefully.
	CacheStats *CacheStatsRing

	// discoveredMCP — names of MCP tools whose schemas the model has
	// already fetched via ToolSearch in this session. While lazy mode
	// is on we usually strip mcp__* schemas, but for tools in this set
	// we leave the schema intact: the model already burned the round-
	// trip on it, and re-stripping after compaction would force a
	// second round-trip (cf. openclaude src/utils/toolSearch.ts:545,
	// which scans message history for `tool_reference` blocks for the
	// same reason). Rebuilt lazily from message history on first read
	// via ensureDiscoveredHydrated — survives compaction so long as
	// the prior ToolSearch tool_result is still in l.Messages.
	discoveredMCP         map[string]bool
	discoveredMCPHydrated bool
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

// AppendUserBlocks adds a user message composed of arbitrary content
// blocks — used by the TUI to attach pasted images alongside text.
// Empty blocks input is ignored (avoids polluting the message log
// with no-op user turns).
func (l *Loop) AppendUserBlocks(blocks []llm.ContentBlock) {
	if len(blocks) == 0 {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.Messages = append(l.Messages, llm.Message{
		Role:    llm.RoleUser,
		Content: blocks,
	})
}

// SteerInject queues user text to be folded into the next iteration's
// user message. Called from the TUI when the user types mid-turn (the
// chat input stays unlocked while the agent is running, claude-code
// parity). Multiple calls accumulate; all queued steers are joined
// with "\n" at the iteration boundary.
//
// Empty / whitespace input is dropped. Safe to call from any goroutine.
func (l *Loop) SteerInject(text string) {
	t := strings.TrimSpace(text)
	if t == "" {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.steerBuf = append(l.steerBuf, t)
}

// drainSteer returns the joined steer buffer and clears it. Called by
// the loop at iteration boundary (after the previous tool result, before
// the next provider request).
func (l *Loop) drainSteer() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.steerBuf) == 0 {
		return ""
	}
	joined := strings.Join(l.steerBuf, "\n")
	l.steerBuf = nil
	return joined
}

// SteerInjectDrainForTest is a test-only export of drainSteer so the
// TUI test (different package) can verify the steer round-trip without
// us widening the package API for production code. Behaviorally
// identical to drainSteer.
func (l *Loop) SteerInjectDrainForTest() string { return l.drainSteer() }

// History returns a snapshot.
func (l *Loop) History() []llm.Message {
	l.mu.Lock()
	defer l.mu.Unlock()
	return transcript.Snapshot(l.Messages)
}

// EstimateContextTokens returns a rough byte-derived estimate of the
// current Messages history's token cost. Used as a STABLE floor for
// the status bar so providers that under-report cache hits (some
// Anthropic-compat gateways do) don't make the displayed
// context-usage number swing down between turns. The status bar
// renders `max(provider-reported, this estimate)` — number stays
// monotone until real compaction (Snip/Microcompact/Compact) fires
// and emits a visible "[info] context snipped: …" event.
//
// Same estimator the compaction tier-1 path already uses
// (compaction_check.go::maybeCompact); exposing it on the public
// surface so the TUI doesn't have to duplicate the estimator.
func (l *Loop) EstimateContextTokens() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return estimateTokens(l.Messages)
}

// haltTurn marks the current Run for halt-after-current-iteration. The
// signal is checked after executeBatch (where PreToolUse hooks fire)
// and after the steer drain — so a hook can deny the offending tool
// (Output) AND halt the rest of the turn (Halt=true) in one decision.
//
// Concurrency: dispatch.go is single-goroutine inside its Phase 0a
// loop; the Run goroutine is the only consumer. Plain field assign
// is fine.
func (l *Loop) haltTurn(reason string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.haltRequested = true
	if reason != "" && l.haltReason == "" {
		l.haltReason = reason
	}
}

// Reset clears the conversation.
func (l *Loop) Reset() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.Messages = nil
	l.turnIdx = 0
	l.iterIdx = 0
	l.compactCircuitNoticeSent = false
	if l.Compactor != nil {
		l.Compactor.ResetCircuit()
	}
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
	_, ok := l.UndoLastTurnWithPrefill()
	return ok
}

// UndoLastTurnWithPrefill is the prefill-aware variant added 2026-05-09:
// returns the user-typed text for the turn that just got popped so the
// TUI can preload it into the input box. Mirrors kimi-cli's `/undo`
// behaviour (`Reload(prefill_text=...)`).
//
// Empty `prefill` means "nothing to prefill" — either the undo failed
// (ok=false) or the popped turn was synthetic (no user-typed text;
// happens when /retry is undone, etc.).
func (l *Loop) UndoLastTurnWithPrefill() (prefill string, ok bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	out, p, success := transcript.UndoWithPrefill(l.Messages)
	if !success {
		return "", false
	}
	l.Messages = out
	return p, true
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
	// Clear any halt signal carried over from a prior turn — Run is the
	// boundary between user prompts, and a halt request only governs
	// the turn that raised it.
	l.mu.Lock()
	l.haltRequested = false
	l.haltReason = ""
	l.mu.Unlock()

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
	emptyStopRescued := false // see empty_stop_rescue.go — at most one rescue per turn
	finalSummaryRescued := false
	nudgeFired := make([]bool, len(iterNudges)) // see iter_nudge.go
	progress := newProgressDetector()           // see progress_detector.go

	for {
		l.mu.Lock()
		l.iterIdx++
		curIter := l.iterIdx
		l.mu.Unlock()
		l.Hooks.EmitTurnStart(ctx, tc, curIter)

		l.maybeCompact(ctx, out)

		// Drain any background-bash job notifications since the last
		// iteration and synthesize <job_notification> user messages.
		// The model sees these as system-reminders telling it which
		// jobs finished (and whether to BashOutput-read the result),
		// matching claude-code's <task_notification> envelope.
		l.injectJobNotifications(out)
		l.injectPeerMessages(out)
		l.injectDreamNotifications(out)
		// Same pattern for Monitor pattern-matches — pulls every
		// MonitorEvent buffered since last iter and injects them as
		// <monitor_event> system-reminders. Cheap when no Monitor is
		// active (nil registry → instant return).
		l.injectMonitorEvents(out)

		// Soft iter-budget nudges at 50% / 75% / 90% — see
		// iter_nudge.go. Fire at most one per threshold per turn so
		// the model gets a heads-up before the hard cap kicks in.
		if idx, body := shouldFireNudge(curIter, l.MaxIters, nudgeFired); body != "" {
			nudgeFired[idx] = true
			l.mu.Lock()
			l.Messages = append(l.Messages, llm.Message{
				Role:    llm.RoleUser,
				Content: []llm.ContentBlock{{Type: "text", Text: body}},
			})
			l.mu.Unlock()
			emit(ctx, out, Event{
				Kind: EventInfo,
				Info: fmt.Sprintf("(iter nudge %d%% — model asked to pace itself)", int(iterNudges[idx].pct*100)),
			})
		}

		req := l.buildRequest(specs)

		stream, err := l.Provider.Stream(ctx, req)
		if err != nil {
			// Classify before deciding the recovery path. Mirrors
			// hermes' error_classifier — the loop now picks the
			// right strategy per error class instead of guessing
			// "always retry once" or "always bail".
			class := ClassifyError(err)
			switch class.Recovery() {
			case RecoveryCompactRetry:
				if l.tryRecoverOverflow(ctx, err, out) {
					req = l.buildRequest(specs)
					stream, err = l.Provider.Stream(ctx, req)
				}
			case RecoveryFailUser:
				// Surface a clean, actionable message for billing /
				// auth / invalid-request. Don't auto-retry these —
				// retrying just burns more requests.
				if msg := UserFacingMessage(class, err); msg != "" {
					emit(ctx, out, Event{Kind: EventInfo, Info: "[" + class.String() + "] " + msg})
				}
			case RecoveryRetry:
				// Rate / server / network — single retry with brief
				// backoff. The loop's outer iteration handles
				// repeated rate-limit cases; here we just absorb the
				// transient first 4xx/5xx if it clears immediately.
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
			emit(ctx, out, Event{
				Kind:                     EventTokens,
				InputTokens:              usage.in,
				OutputTokens:             usage.out,
				CacheCreationInputTokens: usage.cacheCreate,
				CacheReadInputTokens:     usage.cacheRead,
			})
		}

		l.mu.Lock()
		l.Messages = append(l.Messages, llm.Message{Role: llm.RoleAssistant, Content: assistant})
		l.mu.Unlock()

		if stop != "tool_use" {
			// Bug B rescue: model declared end_turn but emitted no
			// user-facing text. See empty_stop_rescue.go for why this
			// happens and the 2026-05-14 session that motivated it.
			// One nudge per turn — if the rescue iteration ALSO emits
			// empty text, accept the stop (something upstream is broken
			// and looping costs tokens for no value).
			if !emptyStopRescued && !hasUserFacingText(assistant) {
				emptyStopRescued = true
				nudge := llm.Message{
					Role:    llm.RoleUser,
					Content: []llm.ContentBlock{{Type: "text", Text: emptyStopRescueMessage}},
				}
				l.mu.Lock()
				l.Messages = append(l.Messages, nudge)
				l.mu.Unlock()
				emit(ctx, out, Event{
					Kind: EventInfo,
					Info: "(empty-final-answer rescue: nudging model to summarize)",
				})
				continue
			}

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
		// Sliding-window signature loop detection (crush parity).
		// Feed (toolUses, results) into the detector so it can pair
		// each call with its result and SHA-256 the batch. Triggers
		// ShouldAbort below (post-steer) when the model has been
		// running the identical call+result combination repeatedly —
		// the 2026-05-08 video bug, where 1h 18m of `cd path && git
		// rebase --continue` retries never got cut off.
		if l.Detector != nil {
			l.Detector.RecordStep(toolUses, results)
		}

		// Diminishing-returns detector: pair tool_uses with results
		// and count an iter as "low-progress" when the non-error
		// output bytes sum below a small threshold. After 3 such
		// iters AND we've already past the 75% nudge (i.e. budget
		// is tight AND the model isn't producing useful info), abort
		// early with an informative event. See progress_detector.go
		// for the bytes-as-token-proxy rationale.
		progress.RecordIter(toolUses, results)
		if progress.IsDiminishing() && len(nudgeFired) >= 2 && nudgeFired[1] {
			stopReason = "diminishing_returns"
			l.Hooks.EmitLoopEnd(ctx, tc, "diminishing_returns")
			emit(ctx, out, Event{
				Kind: EventInfo,
				Info: fmt.Sprintf("aborted on diminishing returns: %d consecutive iters with <%d bytes of useful tool output past 75%% budget", progress.ConsecutiveLow(), progressLowBytesThreshold),
			})
			emit(ctx, out, Event{Kind: EventLoopDone, StopReason: "diminishing_returns"})
			return nil
		}

		// Hook-driven halt: if any PreToolUse hook in this batch
		// returned Halt=true, stop the turn after delivering tool
		// results. The hook may have ALSO denied the offending tool
		// via Output (handled inline in dispatch.go); honoring both
		// in the same iteration means the model sees what was
		// rejected and the loop ends cleanly without another API
		// call. claude-code subprocess hooks signal halt via JSON
		// `{"decision":"halt"}` or exit code 49.
		l.mu.RLock()
		halt := l.haltRequested
		hreason := l.haltReason
		l.mu.RUnlock()
		if halt {
			if hreason == "" {
				hreason = "halted by hook"
			}
			l.mu.Lock()
			l.Messages = append(l.Messages, llm.Message{Role: llm.RoleUser, Content: results})
			l.mu.Unlock()
			stopReason = "halted_by_hook"
			l.Hooks.EmitLoopEnd(ctx, tc, "halted_by_hook")
			emit(ctx, out, Event{Kind: EventInfo, Info: "halt: " + hreason})
			emit(ctx, out, Event{Kind: EventLoopDone, StopReason: "halted_by_hook"})
			return nil
		}
		// Steering (Task #78): if the user typed mid-turn while tools
		// were running, fold their text into the user message that
		// carries the tool_results. The agent sees both pieces in one
		// turn and can adjust course on the next iteration.
		// claude-code surfaces this as "steering"; the user typing
		// "actually use Edit not Write" mid-loop hits the next API
		// call without waiting for a clean turn boundary.
		if steer := l.drainSteer(); steer != "" {
			results = append(results, llm.ContentBlock{
				Type: "text",
				Text: "[user steer mid-turn] " + steer,
			})
			emit(ctx, out, Event{Kind: EventInfo, Info: "steered: " + steer})
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
			reason := l.Detector.AbortReason()
			var msg string
			switch reason {
			case LoopSignatureRepeat:
				// More actionable than "X total tool calls" — tells the
				// user the agent kept running the same call+result combo.
				msg = fmt.Sprintf("loop detector aborted: same tool call+result repeated within window of %d steps",
					l.Detector.SignatureWindowSize)
			default:
				msg = fmt.Sprintf("loop detector aborted: %d total tool calls", stats.GlobalCount)
			}
			stopReason = "loop_detected"
			l.Hooks.EmitLoopEnd(ctx, tc, "loop_detected")
			emit(ctx, out, Event{Kind: EventInfo, Info: msg})
			emit(ctx, out, Event{Kind: EventLoopDone, StopReason: "loop_detected"})
			return nil
		}

		// Budget check with grace call + final-summary rescue.
		// Order matters: grace first (one extra iter for "I'm
		// almost done"), then final summary rescue (one MORE iter
		// where we explicitly tell the model to write the answer
		// now instead of starting new work), then real abort.
		if curIter >= l.MaxIters {
			if graceUsed < l.GraceCalls {
				graceUsed++
				continue
			}
			if !finalSummaryRescued {
				finalSummaryRescued = true
				l.mu.Lock()
				l.Messages = append(l.Messages, llm.Message{
					Role:    llm.RoleUser,
					Content: []llm.ContentBlock{{Type: "text", Text: finalSummaryRescueMessage}},
				})
				l.mu.Unlock()
				emit(ctx, out, Event{
					Kind: EventInfo,
					Info: fmt.Sprintf("(iter cap reached at %d — one rescue iteration for final summary)", l.MaxIters),
				})
				continue
			}
			stopReason = "max_iterations"
			l.Hooks.EmitLoopEnd(ctx, tc, "max_iterations")
			emit(ctx, out, Event{Kind: EventInfo, Info: fmt.Sprintf("budget exhausted (%d iters, %d grace + 1 final-summary rescue used)", l.MaxIters, l.GraceCalls)})
			emit(ctx, out, Event{Kind: EventLoopDone, StopReason: "max_iterations"})
			return nil
		}
	}
}

// buildRequest assembles the per-iteration LLM Request under l.mu so the
// snapshot of Messages and System+Memory composition is consistent.
//
// SystemSections wiring: when l.SystemSections is populated (the new
// path from runtime.AssembleSystemPromptSections), we emit memory
// context as its OWN section flagged Volatile=true. That lets the
// Anthropic provider keep base + addendum cached even when memory
// updates per turn — without sectioning, a single memory write would
// invalidate every cache breakpoint downstream of it.
func (l *Loop) buildRequest(specs []llm.ToolSpec) llm.Request {
	l.mu.Lock()
	defer l.mu.Unlock()
	system := l.System
	var sections []llm.SystemSection
	var memBody string
	if l.Memory != nil {
		memBody = l.Memory.BuildContext()
	}
	if len(l.SystemSections) > 0 {
		sections = make([]llm.SystemSection, 0, len(l.SystemSections)+1)
		sections = append(sections, l.SystemSections...)
		if memBody != "" {
			sections = append(sections, llm.SystemSection{
				Name:     "memory",
				Body:     memBody,
				Cache:    false,
				Volatile: true,
			})
		}
	} else if memBody != "" {
		// Legacy (string-only) path: append memory the old way so the
		// boundary-marker parser still produces [base (cached), rest].
		// Cache for `rest` is lost (memory is in there), but at least
		// the base prefix still hits.
		system = system + "\n\n" + memBody
	}
	req := llm.Request{
		Model:          l.Model,
		System:         system,
		SystemSections: sections,
		Messages:       append([]llm.Message(nil), l.Messages...),
		Tools:          specs,
		Stream:         true,
		Effort:         l.Effort,
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
