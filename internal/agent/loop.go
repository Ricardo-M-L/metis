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

	// AutoRetrieveK > 0 enables per-turn BM25 retrieval from archival
	// memory: buildRequest looks up the most recent user message, ranks
	// archival passages by relevance, and appends the top-K as their
	// own <auto-retrieve> system section. Mirrors claude-code's
	// findRelevantMemories pattern, minus the LLM-ranking sub-call
	// (we use local BM25 — cheap, deterministic, no extra spend).
	//
	// 0 = disabled (default). Typical opt-in is METIS_AUTO_RETRIEVE=5
	// via runtime.BuildAgentLoop's env override.
	AutoRetrieveK int

	// AutoRetrieveRerank, when true, fetches BM25 top K*3 candidates
	// then asks the active provider to LLM-rerank them down to top-K.
	// More accurate than raw BM25 (the ranker is an actual language
	// model that understands "the user asked about X, this passage
	// is about a different X") at the cost of one extra Complete()
	// call per turn (~500ms-2s). Falls back to BM25 ordering on
	// timeout / parse error / empty provider — never blocks the loop.
	//
	// false = disabled (default). Opt-in via METIS_AUTO_RETRIEVE_RERANK=1.
	// Has no effect when AutoRetrieveK == 0.
	AutoRetrieveRerank bool

	// PlanMode: collect tool calls but do NOT execute; emit as EventPlan.
	PlanMode bool

	// prePlanMode snapshots the gate mode that was active right before
	// EnterPlanMode flipped the gate to "plan". ExitPlanMode reads this
	// to restore the user's prior posture rather than leaving Gate
	// stuck in plan (which would re-trigger the deny-storm on the next
	// turn — the bug the 2026-05-18 audit caught). Empty string = no
	// snapshot captured.
	prePlanMode string

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

	// BypassNextCache, when true, makes buildRequest append a unique
	// nonce to the system prompt for the next request, guaranteeing a
	// cache miss + fresh breakpoint write. Auto-clears after read so
	// subsequent turns resume normal cache reuse. Set via the TUI
	// /break-cache slash command.
	BypassNextCache bool

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

	// lastTimeBasedMicrocompactAt is the wall-clock of the most recent
	// Microcompact pass (whether triggered by SnipThreshold or by the
	// IdleMaxSeconds time-based path). maybeCompact reads it to decide
	// whether to force a fresh Microcompact when the conversation has
	// been "idle" longer than the cache TTL — mirrors CC's
	// timeBasedMC. Initialised to Loop creation time in NewLoop so the
	// first IdleMaxSeconds-second window starts from there, not from
	// time.Time{} (which would trip on every call).
	lastTimeBasedMicrocompactAt time.Time

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

	// contract enforces the dispatch contract at the loop level —
	// see contract.go. Per-loop state (counters reset on Loop.Reset);
	// gates mid-turn reminder + end-of-turn block when the model has
	// done substantial work without spawning a verify subagent.
	contract contractTracker
}

func NewLoop(p llm.Provider, r *tools.Registry, g *permission.Gate, h *HookRegistry, system string, maxIter int) *Loop {
	if h == nil {
		h = NewHookRegistry()
	}
	if maxIter <= 0 {
		maxIter = 50
	}
	return &Loop{
		Provider:                    p,
		Registry:                    r,
		Gate:                        g,
		Hooks:                       h,
		System:                      system,
		MaxIters:                    maxIter,
		GraceCalls:                  1,
		lastTimeBasedMicrocompactAt: time.Now(),
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

// SetPlanMode satisfies the PlanController interface so builtin tools
// (EnterPlanMode / ExitPlanMode) can flip plan mode mid-conversation
// without importing the full Loop type. Mutex-protected because the
// loop reads PlanMode at iteration boundaries while tools execute on
// the same goroutine as the loop but write to shared state.
func (l *Loop) SetPlanMode(on bool) {
	l.mu.Lock()
	l.PlanMode = on
	l.mu.Unlock()
}

// PrePlanMode / SetPrePlanMode implement the PlanController surface
// for EnterPlanMode/ExitPlanMode to round-trip the user's prior gate
// posture across the plan window. See the prePlanMode field doc for
// the design rationale.
func (l *Loop) PrePlanMode() string {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.prePlanMode
}

func (l *Loop) SetPrePlanMode(mode string) {
	l.mu.Lock()
	l.prePlanMode = mode
	l.mu.Unlock()
}

// Reset clears the conversation.
func (l *Loop) Reset() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.Messages = nil
	l.turnIdx = 0
	l.iterIdx = 0
	l.compactCircuitNoticeSent = false
	l.contract.reset()
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
//
// Orphan-repair runs on the incoming slice so old sessions written
// before the Run-defer guard don't immediately 400 on the first
// resumed turn. RepairOrphanedToolUses is idempotent — a clean
// history passes through unchanged.
func (l *Loop) Restore(messages []llm.Message) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if messages == nil {
		l.Messages = nil
	} else {
		clone := append([]llm.Message(nil), messages...)
		l.Messages = RepairOrphanedToolUses(clone)
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
	// Orphan-repair runs FIRST so persistTail / sessions resume see a
	// fully-paired history regardless of how Run exited (ctx cancel
	// mid-tool-batch, panic in a tool above runExecute's recovery
	// layer, hook halt before tool_results landed). Session 8cfc076b
	// (2026-05-17) caught the bug in the wild: a foreground Agent
	// tool_use was persisted with no matching tool_result, which would
	// then 400 on resume.
	var stopReason string
	defer func() {
		l.repairOrphansInPlace()
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
	stuckDet := &stuckDetector{}                // see stuck_detector.go — Phase C-mini

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

		// Provider stop-reason defense (2026-05-18, session 8cfc076b).
		// MiniMax / some OpenAI-compatible gateways report
		// `finish_reason: "stop"` (mapped to "end_turn") on a chunk
		// that ALSO carried tool_calls, instead of the correct
		// "tool_calls" → "tool_use" mapping. Without this heal, the
		// loop hits the `stop != "tool_use"` branch below, emits
		// LoopDone, and the assistant's tool_use blocks become
		// orphans — orphan_repair.go then has to synthesize stub
		// tool_results, the model never sees the real output.
		//
		// Rule: if assistant content contains tool_use blocks, the
		// only correct stop reason is "tool_use" — anything else is
		// a provider misreport. Override and continue into
		// executeBatch so the tools actually run.
		if stop != "tool_use" && containsToolUseBlock(assistant) {
			emit(ctx, out, Event{
				Kind: EventInfo,
				Info: fmt.Sprintf("(provider reported stop_reason=%q while emitting tool_use blocks — treating as tool_use)", stop),
			})
			stop = "tool_use"
		}

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

			// Dispatch-contract end-of-turn gate. See contract.go for
			// the trigger logic; runs after the empty-stop rescue so
			// the two don't race on the same turn. When the gate fires,
			// inject the reminder as a user message and continue the
			// loop instead of ending — the model gets another turn to
			// either spawn the verifier or write OVERRIDE CONTRACT:.
			if body := l.contract.shouldGateEnd(assistantText(assistant)); body != "" {
				l.mu.Lock()
				l.Messages = append(l.Messages, llm.Message{
					Role:    llm.RoleUser,
					Content: []llm.ContentBlock{{Type: "text", Text: body}},
				})
				l.mu.Unlock()
				emit(ctx, out, Event{
					Kind: EventInfo,
					Info: "(contract gate: forced re-entry to spawn verify; model can override by writing OVERRIDE CONTRACT: <reason>)",
				})
				continue
			}
			// Override was used — log it once before releasing so the
			// user sees the audit trail in the event stream rather
			// than a silent release.
			if l.contract.wasOverridden(assistantText(assistant)) && l.contract.thresholdMet() && !l.contract.verifyDispatched {
				emit(ctx, out, Event{
					Kind: EventInfo,
					Info: "(contract override: model explicitly bypassed the verify gate)",
				})
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
		// Dispatch-contract observation + mid-turn reminder. Count
		// the batch the model just emitted (Write/Edit/MultiEdit/
		// Agent), and if the threshold just crossed without a verify
		// dispatch, queue a one-time heads-up reminder so the model
		// can plan the verify step before it tries to end. The
		// reminder appears as an extra text block alongside the
		// tool_results in the SAME user message the executeBatch
		// loop will produce — see the `pendingContractReminder`
		// drain below.
		//
		// 2026-05-18 — pre-fix this appended a separate user-text
		// message BEFORE executeBatch ran, which sat between the
		// assistant message (containing tool_use blocks) and the
		// follow-up user message (carrying tool_results). Anthropic
		// tolerates the gap; OpenAI / DeepSeek / Kimi reject with
		// "An assistant message with 'tool_calls' must be followed
		// by tool messages" (caught in the wild on a fan-out test
		// that emitted 3+ Agent calls). Folding the reminder into
		// the same user message via append-text-block keeps the
		// tool_calls→tool_results adjacency the OpenAI dialect
		// requires.
		l.contract.observeToolUses(toolUses)
		pendingContractReminder := ""
		if body := l.contract.shouldFireMidTurnReminder(); body != "" {
			pendingContractReminder = body
			emit(ctx, out, Event{
				Kind: EventInfo,
				Info: "(contract reminder: substantial work in flight — plan for a verify subagent before claiming done)",
			})
		}
		if len(toolUses) == 0 {
			stopReason = "no_tool_calls"
			l.Hooks.EmitLoopEnd(ctx, tc, "no_tool_calls")
			emit(ctx, out, Event{Kind: EventLoopDone, StopReason: "no_tool_calls"})
			return nil
		}

		// "Mid-turn enter" — model decided to enter plan mode AND
		// batched other tools in the same turn. Without this guard,
		// EnterPlanMode would flip Loop.PlanMode mid-dispatch but
		// the sibling tools would still hit executeBatch first. Treat
		// the whole batch as plan content: run EnterPlanMode (it
		// flips PlanMode synchronously), then fall through to the
		// l.PlanMode branch below which will emitPlan the rest.
		if !l.PlanMode && containsEnterPlanMode(toolUses) {
			enterTools, otherTools := splitEnterPlanModeTools(toolUses)
			results, err := l.executeBatch(ctx, enterTools, out, tc)
			if err != nil {
				stopReason = "error"
				emit(ctx, out, Event{Kind: EventError, Err: err})
				return err
			}
			l.mu.Lock()
			l.Messages = append(l.Messages, llm.Message{Role: llm.RoleUser, Content: results})
			l.mu.Unlock()
			// PlanMode should now be true (EnterPlanMode just flipped
			// it). If for any reason it isn't (defensive — controller
			// detached?), fall through and let normal dispatch run.
			if l.PlanMode && len(otherTools) > 0 {
				stopReason = "plan_mode"
				l.emitPlan(ctx, otherTools, out)
				return nil
			}
			// EnterPlanMode + nothing else → just continue to next
			// turn so the model can plan with no real tools to undo.
			if len(otherTools) == 0 {
				l.Hooks.EmitTurnEnd(ctx, tc, l.turnIdx)
				emit(ctx, out, Event{Kind: EventTurnEnd})
				l.turnIdx++
				continue
			}
			// Defensive fall-through: PlanMode didn't stick, dispatch
			// the rest normally (better to run than to drop silently).
			toolUses = otherTools
		}

		// Plan Mode: split the batch three ways:
		//   - ExitPlanMode → execute (the model's only way to leave plan)
		//   - read-only tools → execute (the model needs to read code
		//     while planning; collecting them defeats the point of plan
		//     mode for any non-trivial task)
		//   - side-effect tools → collect as plan for user review
		//
		// Pre-fix (2026-05-18), plan mode collected ALL non-Exit tools,
		// including Read / LS / Glob / Grep / MetisInfo, which made it
		// impossible for the model to actually plan against current state.
		// User would say "look at /tmp/foo and propose changes" and the
		// "plan" would just be `Read(/tmp/foo)` — useless. The gate's
		// readOnlyHook already knows which tools are side-effect-free
		// (it's the same hook ModeAcceptEdits uses); plan mode now
		// consults it to partition correctly.
		if l.PlanMode {
			exitTools, nonExit := splitExitPlanModeTools(toolUses)
			readTools, writeTools := splitReadOnlyTools(nonExit, l.Gate)

			// 1. ExitPlanMode + read-only tools both execute. ExitPlanMode
			//    going first means a subsequent Read in the same batch
			//    runs in post-plan mode (correct behavior — Exit just
			//    restored the user's prior posture).
			runTools := append([]llm.ContentBlock(nil), exitTools...)
			runTools = append(runTools, readTools...)
			if len(runTools) > 0 {
				results, err := l.executeBatch(ctx, runTools, out, tc)
				if err != nil {
					stopReason = "error"
					emit(ctx, out, Event{Kind: EventError, Err: err})
					return err
				}
				l.mu.Lock()
				l.Messages = append(l.Messages, llm.Message{Role: llm.RoleUser, Content: results})
				l.mu.Unlock()
			}

			// 2. Side-effect tools get collected as the plan. If the
			//    batch is read-only with no writes AND ExitPlanMode ran
			//    (so we're no longer in plan), continue the next turn
			//    normally so the model can use those reads to plan.
			if len(writeTools) > 0 && l.PlanMode {
				stopReason = "plan_mode"
				l.emitPlan(ctx, writeTools, out)
				return nil
			}

			// 3. No writes (or ExitPlanMode flipped us out of plan) →
			//    let the turn continue so the model can proceed.
			if len(runTools) > 0 {
				l.Hooks.EmitTurnEnd(ctx, tc, l.turnIdx)
				emit(ctx, out, Event{Kind: EventTurnEnd})
				l.turnIdx++
				continue
			}

			// 4. All tools were write-class (nothing executed) — emit
			//    the plan and end the turn.
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

		// Phase B verdict tracking: scan results for verify-subagent
		// VERDICT lines so the end-of-turn gate can refuse release on
		// non-PASS verdicts. Must run AFTER executeBatch (need result
		// bodies) and BEFORE the next iteration's shouldGateEnd check.
		// See contract.go::observeToolResults for the extraction logic.
		l.contract.observeToolResults(toolUses, results)

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

		// Stuck-edit detector (Phase C-mini): catches the bench6 mini-
		// interpreter failure mode — same file edited N turns in a row
		// with the same test failing. LoopDetector's signature-based
		// check misses this because each Edit has different content
		// (different SHA). Diminishing-returns DOES catch it but only
		// past 75% budget; by then 3-4M tokens are gone. This detector
		// fires on a tighter signal so we can either nudge the model
		// out (first trip → reset reminder) or abort honestly (second
		// trip → stuck_after_reset, mapped to Incomplete exit code
		// by cmdRun's Phase A switch).
		switch stuckDet.AfterTurn(toolUses, results) {
		case stuckResetNeeded:
			pendingContractReminder = stuckResetReminderText
			emit(ctx, out, Event{
				Kind: EventInfo,
				Info: fmt.Sprintf("stuck-loop detected (reset #%d of %d): test failures not converging; injecting reset reminder", stuckDet.resetsFired, stuckMaxResets),
			})
		case stuckAbort:
			stopReason = "stuck_after_reset"
			l.Hooks.EmitLoopEnd(ctx, tc, "stuck_after_reset")
			emit(ctx, out, Event{
				Kind: EventInfo,
				Info: fmt.Sprintf("stuck-loop detected after %d reset reminders; aborting to surface the loop instead of burning more tokens", stuckMaxResets),
			})
			emit(ctx, out, Event{Kind: EventLoopDone, StopReason: "stuck_after_reset"})
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
			// Fold contract reminder if pending — keeps adjacency for
			// OpenAI-dialect providers even on the halt exit path.
			if pendingContractReminder != "" {
				results = append(results, llm.ContentBlock{
					Type: "text",
					Text: pendingContractReminder,
				})
				pendingContractReminder = ""
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
		// Contract reminder fold-in (2026-05-18) — see the queue site
		// above for full rationale. Pre-fix this lived as a separate
		// user message before executeBatch; that broke OpenAI's
		// strict tool_calls→tool_results adjacency.
		if pendingContractReminder != "" {
			results = append(results, llm.ContentBlock{
				Type: "text",
				Text: pendingContractReminder,
			})
			pendingContractReminder = ""
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
				// The genuine loop signal: same tool call+result combo
				// fired N+ times in the recent window. Far more
				// actionable than a raw call count.
				msg = fmt.Sprintf("loop detector aborted: same tool call+result repeated within window of %d steps",
					l.Detector.SignatureWindowSize)
			case LoopGlobalCircuitBreaker:
				// Only reachable when user opted in to the runaway
				// backstop via [loop_detection].global > 0. Default
				// config disables this branch entirely.
				msg = fmt.Sprintf("loop detector aborted: opt-in runaway cap of %d total tool calls reached",
					l.Detector.GlobalThreshold)
			default:
				msg = fmt.Sprintf("loop detector aborted (reason=%s, count=%d)", reason, stats.GlobalCount)
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
			// User-facing exhaustion note. The "next time" suggestion
			// points at the dispatch contract — without it users keep
			// running the same single-threaded prompt and hitting the
			// same cap. Fan-out is the only real escape on this size
			// of task.
			emit(ctx, out, Event{
				Kind: EventInfo,
				Info: fmt.Sprintf(
					"budget exhausted (%d iters, %d grace + 1 final-summary rescue used). "+
						"Next time for tasks this size: open with `Agent({subagent_type: \"plan\", "+
						"prompt: \"...\"})` to get a stepped breakdown, then fan out implementer "+
						"agents and a `verify` agent — each subagent gets its own iter budget so "+
						"the main thread doesn't run out mid-work.",
					l.MaxIters, l.GraceCalls,
				),
			})
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
	// Per-turn auto retrieval: BM25 the latest user message against
	// archival and append the top-K passages as their own section.
	// Off by default (AutoRetrieveK == 0); typically toggled via
	// METIS_AUTO_RETRIEVE=K at runtime.BuildAgentLoop wiring time.
	//
	// AutoRetrieveRerank, when on, fetches BM25 top K*3 candidates and
	// asks the provider to LLM-rerank them down to the final K — more
	// accurate than raw BM25 score, costs one Complete() call per turn.
	// Falls back to BM25 ordering on any failure.
	var retrieveBody string
	if l.Memory != nil && l.AutoRetrieveK > 0 {
		query := lastUserTextLocked(l.Messages)
		if l.AutoRetrieveRerank && l.Provider != nil {
			candidates := l.Memory.AutoRetrieveCandidates(query, l.AutoRetrieveK*3)
			picked := rerankAutoRetrieve(context.Background(), l.Provider, query, candidates, l.AutoRetrieveK)
			retrieveBody = memory.FormatRetrieveSection(picked)
		} else {
			retrieveBody = l.Memory.AutoRetrieve(query, l.AutoRetrieveK)
		}
	}
	if len(l.SystemSections) > 0 {
		sections = make([]llm.SystemSection, 0, len(l.SystemSections)+2)
		sections = append(sections, l.SystemSections...)
		if memBody != "" {
			sections = append(sections, llm.SystemSection{
				Name:     "memory",
				Body:     memBody,
				Cache:    false,
				Volatile: true,
			})
		}
		if retrieveBody != "" {
			sections = append(sections, llm.SystemSection{
				Name:     "auto-retrieve",
				Body:     retrieveBody,
				Cache:    false,
				Volatile: true,
			})
		}
	} else if memBody != "" || retrieveBody != "" {
		// Legacy (string-only) path: append both bodies the old way so
		// the boundary-marker parser still produces [base (cached),
		// rest]. Cache for `rest` is lost but the base prefix still hits.
		if memBody != "" {
			system = system + "\n\n" + memBody
		}
		if retrieveBody != "" {
			system = system + "\n\n" + retrieveBody
		}
	}
	// BypassNextCache: when /break-cache is invoked, the next request
	// gets a fresh nonce appended to the system prompt so the prefix
	// differs from the previous run, guaranteeing a cache miss + fresh
	// breakpoint write. Cleared after we read it so subsequent
	// requests resume normal cache reuse.
	if l.BypassNextCache {
		nonce := fmt.Sprintf("\n\n<cache-refresh nonce=%d>", time.Now().UnixNano())
		if len(sections) > 0 {
			// Tag the last section so the nonce sits AFTER any cached
			// boundary marker; cached sections stay cached, the volatile
			// tail invalidates as intended.
			sections = append(sections, llm.SystemSection{
				Name: "cache-refresh", Body: nonce, Cache: false, Volatile: true,
			})
		} else {
			system = system + nonce
		}
		l.BypassNextCache = false
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

// splitExitPlanModeTools partitions a tool_use batch into the
// ExitPlanMode entries vs everything else. Used by the plan-mode gate
// to whitelist the exit tool while still collecting (not executing)
// any other tools the model batched alongside it.
func splitExitPlanModeTools(blocks []llm.ContentBlock) (exit, other []llm.ContentBlock) {
	for _, b := range blocks {
		if b.ToolName == "ExitPlanMode" {
			exit = append(exit, b)
		} else {
			other = append(other, b)
		}
	}
	return
}

// containsToolUseBlock reports whether the assistant content includes
// at least one tool_use block. Used as the stop-reason heal trigger:
// providers occasionally report "stop" / "end_turn" on a chunk that
// also carried tool_calls (session 8cfc076b, MiniMax m2.7 caught in
// the wild), which would orphan the tool_uses if accepted at face
// value. The presence of any tool_use forces the loop into
// executeBatch regardless of the reported stop reason.
func containsToolUseBlock(blocks []llm.ContentBlock) bool {
	for _, b := range blocks {
		if b.Type == "tool_use" {
			return true
		}
	}
	return false
}

// containsEnterPlanMode reports whether the batch the model just emitted
// includes EnterPlanMode. Used by the loop to upgrade a "not yet in plan
// mode" turn into plan-collection mid-flight: if the model decided to
// enter plan AND batched real tool calls in the same turn, we want
// those calls collected as the plan, NOT executed first.
//
// Without this check, the model's [EnterPlanMode, Write] batch would
// have EnterPlanMode flip Loop.PlanMode → true mid-dispatch, but the
// Write that ran before it (or alongside it in the same executeBatch)
// would already have hit the filesystem. The fix is to short-circuit
// at the dispatch boundary: if the batch contains EnterPlanMode, treat
// the whole batch as plan-mode (run the EnterPlanMode tool, then collect
// the rest as plan content, then return).
func containsEnterPlanMode(blocks []llm.ContentBlock) bool {
	for _, b := range blocks {
		if b.ToolName == "EnterPlanMode" {
			return true
		}
	}
	return false
}

// splitEnterPlanModeTools partitions a batch into EnterPlanMode entries
// vs everything else — symmetric to splitExitPlanModeTools.
func splitEnterPlanModeTools(blocks []llm.ContentBlock) (enter, other []llm.ContentBlock) {
	for _, b := range blocks {
		if b.ToolName == "EnterPlanMode" {
			enter = append(enter, b)
		} else {
			other = append(other, b)
		}
	}
	return
}

// splitReadOnlyTools partitions a batch into read-only tools (safe to
// execute even in plan mode) vs side-effect tools (collected as plan
// for user review). Uses the gate's readOnlyHook for the decision —
// same source of truth ModeAcceptEdits uses for auto-allow. A nil
// gate treats nothing as read-only (caller falls back to old
// "collect everything" behavior).
//
// 2026-05-18: introduced so plan mode no longer collects Read / LS /
// Grep — those have no side effect and the model needs them to plan
// against current code. See the plan-mode block in Loop.Run for the
// full rationale.
func splitReadOnlyTools(blocks []llm.ContentBlock, gate readOnlyChecker) (readOnly, sideEffect []llm.ContentBlock) {
	for _, b := range blocks {
		if gate != nil && gate.IsReadOnly(b.ToolName, stringifyToolInput(b.ToolInput)) {
			readOnly = append(readOnly, b)
		} else {
			sideEffect = append(sideEffect, b)
		}
	}
	return
}

// readOnlyChecker is the narrow interface splitReadOnlyTools needs
// from a Gate. Defining it locally avoids a hard dep on permission.Gate
// at the test boundary and lets us swap a stub in unit tests.
type readOnlyChecker interface {
	IsReadOnly(tool, stringInput string) bool
}

// stringifyToolInput renders the model's tool input into the same
// string form the gate's readOnlyHook receives elsewhere (Bash's cmd,
// Edit's path, etc.). Falls back to JSON when no canonical key is
// known so the hook can still inspect structure if it wants to.
func stringifyToolInput(in map[string]any) string {
	if in == nil {
		return ""
	}
	for _, key := range []string{"command", "path", "file_path", "query", "url"} {
		if v, ok := in[key].(string); ok && v != "" {
			return v
		}
	}
	return ""
}

// lastUserTextLocked returns the most recent user-role message text.
// Caller must already hold l.mu — used by buildRequest which holds
// the lock for the whole request-assembly window. Tool_result content
// blocks are skipped: a turn that injected tool results as the user
// "message" doesn't represent a fresh user query, and querying
// archival with raw tool output would dilute relevance.
func lastUserTextLocked(messages []llm.Message) string {
	for i := len(messages) - 1; i >= 0; i-- {
		m := messages[i]
		if m.Role != llm.RoleUser {
			continue
		}
		t := textOf(m)
		if t == "" {
			continue
		}
		return t
	}
	return ""
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
