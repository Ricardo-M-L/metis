package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Ricardo-M-L/metis/internal/agent/transcript"
	"github.com/Ricardo-M-L/metis/internal/budget"
	"github.com/Ricardo-M-L/metis/internal/checkpoint"
	"github.com/Ricardo-M-L/metis/internal/jobs"
	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/llm/transport"
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

	// TimingSink, when set, is called with each tool's measured execution
	// time (name, elapsed, isError). The runtime wires it to the session's
	// timing sidecar so `metis sessions timing <id>` can show per-step
	// durations after the fact. nil = not recorded.
	TimingSink func(tool string, elapsed time.Duration, isError bool)

	System string
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

	// rescueNoTools forces the NEXT buildRequest to omit the tool list,
	// guaranteeing a text-only iteration. Set by the final-summary rescue
	// when the iter cap exhausts: the model is told to "write the answer
	// now", but previously tools stayed available and a tool_use on that
	// rescue iteration left its results dangling — the turn then ended
	// with no conclusion (the "ran and then just stopped" symptom). With
	// no tools in the schema the provider can only emit text, so the turn
	// closes with a real summary. Consumed (cleared) by buildRequest so it
	// affects exactly one request. Guarded by mu.
	rescueNoTools bool

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

	// PlanMode keeps read-only exploration available while plan-control tools
	// execute normally and potentially mutating tools receive denied results.
	// The model sees those results and can recover or request ExitPlanMode.
	// planMode is private so every live read/write goes through the mutex-backed
	// accessors below. Permission mode can change from the Bubble Tea goroutine
	// while Run is consuming a provider response, so a public bool made the
	// plan boundary both racy and vulnerable to stale reads.
	planMode bool
	// PlanSystemPrompt is the plan-mode overlay body. NewLoop installs a
	// compact agent-local fallback; the runtime replaces it with its richer
	// PlanOverlay(true) body.
	// buildRequest adds it only while PlanMode is live and removes any
	// boot-time plan_mode section when the mode is off. The field lives in
	// agent to avoid an agent -> runtime import cycle; runtime wires the body
	// from PlanOverlay(true).
	PlanSystemPrompt string

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

	// Budget enforces the session-level USD cap (claude-code's
	// maxBudgetUsd). nil = no cap. Sub-agent loops receive the SAME
	// tracker pointer as their parent, so child spend draws down one
	// shared pool. Checked at the top of each iteration (same clean
	// boundary as the wall-clock cap — no orphaned tool_use blocks);
	// usage lands after each consumeStream.
	Budget *budget.Tracker

	// SpillDir is where ingestion-time result spilling persists
	// oversized tool outputs (claude-code's maxResultSizeChars; see
	// internal/spill). Wired by runtime independently from the
	// Compactor's MicrocompactDir so the two kill switches
	// (METIS_SPILL / METIS_MICROCOMPACT) stay independent. Empty =
	// spilling disabled.
	SpillDir string

	// Detector monitors tool call patterns; aborts run when ShouldAbort returns true.
	// nil = disabled.
	Detector *LoopDetector

	// Effort sets the reasoning intensity for the next request:
	//   ""        → don't send a thinking/reasoning field (provider default)
	//   "low"     → small budget, fastest answers
	//   "medium"  → balanced
	//   "high"    → deep reasoning, slowest
	// Maps to Anthropic thinking.budget_tokens and OpenAI reasoning_effort.
	//
	// Live callers use EffortValue / SetEffort. Keep this on a small dedicated
	// lock: buildRequest's main mu can cover memory assembly and must not stall
	// the Bubble Tea renderer just because it reads the status-bar glyph.
	effortMu sync.RWMutex
	effort   llm.Effort

	// fast collapses the next turn's resource use:
	//   - effort drops to "low" (overrides Effort for the request)
	//   - max_tokens halved via Request.MaxTokens override
	// Useful for "I just need a quick answer" rather than spinning up
	// deep deliberation on a one-line clarification. It is atomic because a
	// local /quick command can toggle it while Run snapshots the next provider
	// request and the TUI renders the status bar.
	fast atomic.Bool

	// BypassNextCache, when true, makes buildRequest append a unique
	// nonce to the system prompt for the next request, guaranteeing a
	// cache miss + fresh breakpoint write. Auto-clears after read so
	// subsequent turns resume normal cache reuse. Set via the TUI
	// /break-cache slash command.
	BypassNextCache bool

	mu       sync.RWMutex
	Messages []llm.Message
	// estTokens caches the last context-token estimate so the TUI can read
	// it WITHOUT taking l.mu (see EstimateContextTokens) — otherwise a
	// render-frame RLock during compaction (which holds l.mu through a
	// 5-30s summarization) freezes the UI and deadlocks against the loop's
	// emit() under the same lock.
	estTokens atomic.Int64
	turnIdx   int
	iterIdx   int

	// todoWriteIter / todoReminderIter drive the periodic todo
	// re-surfacing (todo_reminder.go): the iteration of the last
	// TodoWrite call and the last reminder injection, so we can fire a
	// <system-reminder> when the list has gone untouched for a while
	// with incomplete items.
	todoWriteIter    int
	todoReminderIter int
	// todoReconciledThisTurn guards the end-of-turn todo reconciliation
	// (the no_tool_calls branch in Run) to at most once per turn — without
	// it a model that keeps stopping without updating the list would loop.
	// Reset at the top of Run.
	todoReconciledThisTurn bool

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

	// subAgentNotify is the internal channel used to receive completion
	// signals from background sub-agents. Both ends are created in
	// NewLoop; the send end is stamped into ctx at the top of Run()
	// so the Agent tool can post a SubAgentNotification after each
	// background sub-agent finishes. Run drains it at every iter
	// boundary (injectSubAgentNotifications) and synthesizes a
	// <sub_agent_idle> system-reminder so the model learns about
	// the completion without polling SubAgentList each turn.
	//
	// Mirrors claude-code's idle_notification flow. Buffer 64 is
	// generous — the practical concurrent background cap is far lower
	// (capNamed default 20).
	subAgentNotify chan SubAgentNotification

	// Checkpointer, when set, snapshots the working tree before the first
	// file-mutating tool of each user turn (shadow-git copy). It powers
	// the unified /rewind — restoring BOTH file state and conversation to
	// a chosen turn. nil disables checkpointing (best-effort feature;
	// Snap/Restore errors are swallowed). See checkpoint_hook.go.
	Checkpointer  *checkpoint.Manager
	ckptMu        sync.Mutex
	ckptStack     []ckptEntry
	ckptSnappedAt int // CountTurns() value last snapped; -1 = none yet

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
	steerBuf    []string
	steerClosed bool

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

const agentPlanSystemPromptFallback = `# Plan mode

Use read-only tools to inspect the current state, but do not edit files, run
state-changing commands, or otherwise mutate the system. Prepare a concrete
plan, then call ExitPlanMode with the complete plan for user approval. Do not
claim implementation is complete while plan mode remains active.`

func NewLoop(p llm.Provider, r *tools.Registry, g *permission.Gate, h *HookRegistry, system string, maxIter int) *Loop {
	if h == nil {
		h = NewHookRegistry()
	}
	if maxIter <= 0 {
		// 2026-05-21: bumped 50 → 150. Top-level turns for large
		// tasks (project rewrite, multi-file refactor, "explore the
		// whole codebase") routinely exceeded 50 iter and got
		// truncated. claude-code's forkSubagent runs at 200; we set
		// the parent at 150 so it caps below maxBudget while still
		// giving non-trivial tasks room. Override via CLI
		// `--max-iter` or per-call max_iter on Agent/Fork.
		maxIter = 150
	}
	return &Loop{
		Provider:                    p,
		Registry:                    r,
		Gate:                        g,
		Hooks:                       h,
		System:                      system,
		PlanSystemPrompt:            agentPlanSystemPromptFallback,
		MaxIters:                    maxIter,
		GraceCalls:                  1,
		lastTimeBasedMicrocompactAt: time.Now(),
		subAgentNotify:              make(chan SubAgentNotification, 64),
		ckptSnappedAt:               -1,
	}
}

// AppendUser adds a user message. When the text contains a
// "rewrite/port/migrate" intent and the loop hasn't yet entered
// plan mode, a `<system-reminder>` block is PREPENDED to the user's
// content listing the required Plan→Ask sequence (see plan_trigger.go
// for the rationale + trigger list). This backs up the prompt-level
// guidance in 08_interaction_modes.md with a model-independent
// runtime intercept.
func (l *Loop) AppendUser(text string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	content := []llm.ContentBlock{{Type: "text", Text: text}}
	if shouldInjectPlanTrigger(text, detectPlanModeEntered(l.Messages)) {
		content = []llm.ContentBlock{
			{Type: "text", Text: planTriggerReminder},
			{Type: "text", Text: text},
		}
	}
	l.Messages = append(l.Messages, llm.Message{
		Role:    llm.RoleUser,
		Content: content,
	})
}

// AppendUserBlocks adds a user message composed of arbitrary content
// blocks — used by the TUI to attach pasted images alongside text.
// Empty blocks input is ignored (avoids polluting the message log
// with no-op user turns).
//
// Same plan-trigger detection as AppendUser. We scan the text blocks
// (skipping image blocks) for a trigger phrase. When matched, the
// system-reminder block is PREPENDED to whatever the caller passed.
func (l *Loop) AppendUserBlocks(blocks []llm.ContentBlock) {
	if len(blocks) == 0 {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	// Concatenate text-only blocks for trigger detection.
	var textBuf strings.Builder
	for _, b := range blocks {
		if b.Type == "text" {
			if textBuf.Len() > 0 {
				textBuf.WriteByte(' ')
			}
			textBuf.WriteString(b.Text)
		}
	}
	if shouldInjectPlanTrigger(textBuf.String(), detectPlanModeEntered(l.Messages)) {
		blocks = append([]llm.ContentBlock{{Type: "text", Text: planTriggerReminder}}, blocks...)
	}
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
func (l *Loop) SteerInject(text string) bool {
	t := strings.TrimSpace(text)
	if t == "" {
		return false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.steerClosed {
		return false
	}
	l.steerBuf = append(l.steerBuf, t)
	return true
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

// drainSteerOrClose atomically handles the final-response race. If input
// arrived while the model was streaming its final text, return it and keep
// this Run open for another iteration. If nothing is pending, close the steer
// gate before LoopDone is emitted; a concurrent TUI submit then receives false
// from SteerInject and can safely fall back to the next-turn queue.
func (l *Loop) drainSteerOrClose() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.steerBuf) == 0 {
		l.steerClosed = true
		return ""
	}
	joined := strings.Join(l.steerBuf, "\n")
	l.steerBuf = nil
	return joined
}

// stopAcceptingSteer closes this Run's steering gate and returns anything that
// arrived after the last iteration-boundary drain. The caller surfaces a
// resend notice before discarding it: carrying accepted input into a later Run
// would misapply it, while clearing it silently would lose user intent.
func (l *Loop) stopAcceptingSteer() []string {
	l.mu.Lock()
	discarded := append([]string(nil), l.steerBuf...)
	l.steerClosed = true
	l.steerBuf = nil
	l.mu.Unlock()
	return discarded
}

// emitAssistantReentryBoundary flushes a completed user-facing assistant
// response before an internal gate or a mid-turn steer starts another model
// request in the same Run. Without this boundary, TUI consumers concatenate
// the next response's text deltas onto the previous response.
func (l *Loop) emitAssistantReentryBoundary(ctx context.Context, out chan<- Event, tc HookContext, assistant []llm.ContentBlock) {
	if !hasUserFacingText(assistant) {
		return
	}
	l.Hooks.EmitTurnEnd(ctx, tc, l.turnIdx)
	emit(ctx, out, Event{Kind: EventTurnEnd})
	l.turnIdx++
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
	// TryRLock, NOT a blocking lock: the TUI calls this every render frame.
	// maybeCompact holds l.mu (the write lock) across an entire compaction
	// pass — including a 5-30s summarization LLM call — and emits progress
	// events under it. A blocking lock here would freeze the bubbletea
	// Update/View goroutine, which then stops draining the agent event
	// channel, so that under-lock emit() blocks forever: a lock+channel
	// deadlock that spins the spinner for hours (observed: the 6h "stuck"
	// session). Falling back to the cached estimate keeps the UI draining.
	if l.mu.TryRLock() {
		v := estimateTokens(l.Messages)
		l.mu.RUnlock()
		l.estTokens.Store(int64(v))
		return v
	}
	return int(l.estTokens.Load())
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
	l.planMode = on
	l.mu.Unlock()
}

// IsPlanMode returns the live planning posture. Use this outside code that
// already holds l.mu; buildRequest reads planMode directly under its request
// snapshot lock.
func (l *Loop) IsPlanMode() bool {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.planMode
}

// EffortValue returns the live reasoning-effort preference. The TUI may read
// this while Run is assembling a provider request, so direct field access from
// presentation or command code would otherwise race with /effort updates.
func (l *Loop) EffortValue() llm.Effort {
	if l == nil {
		return llm.EffortDefault
	}
	l.effortMu.RLock()
	defer l.effortMu.RUnlock()
	return l.effort
}

// SetEffort changes the reasoning-effort preference for the next request
// snapshot. A running iteration may already have captured the previous value;
// the following iteration observes this update without racing buildRequest or
// SnapshotForFork.
func (l *Loop) SetEffort(e llm.Effort) {
	if l == nil {
		return
	}
	l.effortMu.Lock()
	l.effort = e
	l.effortMu.Unlock()
}

// FastEnabled returns the live quick-output preference. The setting can be
// read concurrently by provider request assembly and TUI rendering.
func (l *Loop) FastEnabled() bool {
	return l != nil && l.fast.Load()
}

// SetFast changes the quick-output preference for subsequent request
// snapshots. A request already assembled may retain the previous value.
func (l *Loop) SetFast(on bool) {
	if l != nil {
		l.fast.Store(on)
	}
}

// ToggleFast atomically flips quick-output mode and returns its new value.
func (l *Loop) ToggleFast() bool {
	if l == nil {
		return false
	}
	for {
		old := l.fast.Load()
		if l.fast.CompareAndSwap(old, !old) {
			return !old
		}
	}
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

// SetCheckpointer rebinds working-tree checkpoints at a top-level session
// boundary. The stack is session-scoped: retaining hashes from the previous
// manager would let /rewind in the destination session restore unrelated file
// state and conversation turns.
func (l *Loop) SetCheckpointer(manager *checkpoint.Manager) {
	l.ckptMu.Lock()
	defer l.ckptMu.Unlock()
	l.Checkpointer = manager
	l.ckptStack = nil
	l.ckptSnappedAt = -1
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
	l.restoreMessagesLocked(messages)
	// Restore is also used by manual /compact. Refresh the non-blocking
	// estimate immediately so the next render cannot reuse the pre-compact
	// cache while the history replacement is already visible.
	l.estTokens.Store(int64(estimateTokens(l.Messages)))
}

func (l *Loop) restoreMessagesLocked(messages []llm.Message) {
	if messages == nil {
		l.Messages = nil
	} else {
		clone := append([]llm.Message(nil), messages...)
		l.Messages = RepairOrphanedToolUses(clone)
	}
	l.turnIdx = 0
	l.iterIdx = 0
}

// ResetSession crosses a top-level chat-session boundary. Unlike Restore,
// which is also used by lightweight history swaps, this clears every mutable
// guard/cache whose lifetime must not span independent conversations.
// Callers invoke it only while no foreground turn is running.
func (l *Loop) ResetSession(messages []llm.Message) {
	if l == nil {
		return
	}
	l.mu.Lock()
	l.restoreMessagesLocked(messages)
	l.estTokens.Store(0)
	l.todoWriteIter = 0
	l.todoReminderIter = 0
	l.todoReconciledThisTurn = false
	l.haltRequested = false
	l.haltReason = ""
	l.steerBuf = nil
	l.compactCircuitNoticeSent = false
	l.lastTimeBasedMicrocompactAt = time.Now()
	l.lastAutoMemoryTurn = 0
	l.BypassNextCache = false
	l.discoveredMCP = nil
	l.discoveredMCPHydrated = false
	l.contract.reset()
	l.mu.Unlock()

	if l.Compactor != nil {
		l.Compactor.ResetCircuit()
	}
	if l.Budget != nil {
		l.Budget.Reset()
	}
	if l.Detector != nil {
		l.Detector.Reset()
	}
	if l.CacheStats != nil {
		l.CacheStats.Reset()
	}
	if l.Monitors != nil {
		l.Monitors.StopAll()
	}
	// Rotate the completion channel at a top-level session boundary. A
	// cancelled background sub-agent may finish slightly later; it retains the
	// old send endpoint and therefore cannot inject its completion into the new
	// conversation. The old channel is buffered, so a late non-blocking notify
	// remains safe until it is garbage-collected.
	drainSubAgentNotifications(l.subAgentNotify)
	l.subAgentNotify = make(chan SubAgentNotification, 64)
	drainJobNotifications(l.JobNotify)
}

func drainSubAgentNotifications(ch <-chan SubAgentNotification) {
	for ch != nil {
		select {
		case <-ch:
		default:
			return
		}
	}
}

func drainJobNotifications(ch <-chan jobs.Notification) {
	for ch != nil {
		select {
		case <-ch:
		default:
			return
		}
	}
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
	// Custom-command `allowed-tools` are one-turn pre-approvals. Install them
	// under a unique source and remove that exact source on every exit path;
	// later interactive approvals must survive this cleanup.
	if rules := turnPermissionRulesFromContext(ctx); len(rules) > 0 && l.Gate != nil {
		cleanup := l.Gate.PushScopedRules(rules...)
		defer cleanup()
	}

	// Clear any halt signal carried over from a prior turn — Run is the
	// boundary between user prompts, and a halt request only governs
	// the turn that raised it.
	l.mu.Lock()
	// Defensive cleanup for sessions created by an older loop that could leave
	// accepted steering behind after an early exit. Preserve a buffer accepted
	// before a fresh Loop's first Run (steerClosed=false): the TUI marks the
	// turn active immediately before starting this goroutine, so that is valid
	// current-Run input rather than stale state.
	discardedStaleSteers := 0
	if l.steerClosed && len(l.steerBuf) > 0 {
		discardedStaleSteers = len(l.steerBuf)
		l.steerBuf = nil
	}
	l.haltRequested = false
	l.haltReason = ""
	l.todoReconciledThisTurn = false
	l.steerClosed = false
	l.mu.Unlock()
	if discardedStaleSteers > 0 {
		emit(ctx, out, Event{
			Kind: EventInfo,
			Info: fmt.Sprintf("discarded %d stale steering message(s) from a previous interrupted run", discardedStaleSteers),
		})
	}
	defer func() {
		if discarded := l.stopAcceptingSteer(); len(discarded) > 0 {
			// The Run commonly exits because ctx was cancelled; detach only the
			// cancellation signal so the loss notice itself is not dropped.
			emit(context.WithoutCancel(ctx), out, Event{
				Kind: EventInfo,
				Info: fmt.Sprintf(
					"current turn ended before %d accepted steering message(s) could be applied; please resend",
					len(discarded),
				),
			})
		}
	}()

	// Stamp the sub-agent notification send end into ctx so the Agent
	// tool can post SubAgentNotification when a background sub-agent
	// finishes. The Agent tool extracts it before building baseCtx and
	// shadows the key with nil in the child's ctx to prevent
	// grandchild agents from writing to the wrong channel.
	if l.subAgentNotify != nil {
		ctx = WithSubAgentNotify(ctx, l.subAgentNotify)
	}

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
	diminishingRescued := false
	nudgeFired := make([]bool, len(iterNudges)) // see iter_nudge.go
	progress := newProgressDetector()           // see progress_detector.go
	stuckDet := &stuckDetector{}                // see stuck_detector.go — Phase C-mini
	runIter := 0                                // MaxIters is per Run, not cumulative session history

	// 2026-05-23: cumulative output tokens this Run() so the iter
	// nudge formatter can include "you've also used N output tokens"
	// — claude-code's tokenBudget continuation message gives the
	// model the same kind of cost-context. metis budgets by iter not
	// tokens, but the cost context still helps the model decide
	// whether to keep exploring or wrap up.
	var runOutputTokens int

	// 2026-05-23: per-Run wall-clock cap. iter count is the primary
	// budget but doesn't bound real time — a Run that hangs in 30
	// iter could nominally take 60+ minutes if every iter triggers a
	// long Bash + reads. Independent cap so a runaway turn surfaces
	// instead of silently burning hours. Override via
	// METIS_TURN_MAX_SECONDS for workflows that legitimately need
	// longer turns (test runs, slow Docker builds, etc).
	turnDeadline := time.Now().Add(45 * time.Minute)
	if env := os.Getenv("METIS_TURN_MAX_SECONDS"); env != "" {
		if n, err := strconv.Atoi(env); err == nil && n > 0 {
			turnDeadline = time.Now().Add(time.Duration(n) * time.Second)
		}
	}

	for {
		// Per-turn wall-clock cap (see deadline computation above).
		// Checked at the top of each iter so an in-flight Bash /
		// LLM call still finishes before we abort — same shape as
		// the iter-cap branch farther down.
		if time.Now().After(turnDeadline) {
			stopReason = "turn_wall_clock"
			l.Hooks.EmitLoopEnd(ctx, tc, "turn_wall_clock")
			emit(ctx, out, Event{
				Kind: EventInfo,
				Info: "turn wall-clock cap reached (METIS_TURN_MAX_SECONDS) — aborting before next iteration",
			})
			emit(ctx, out, Event{Kind: EventLoopDone, StopReason: "turn_wall_clock"})
			return nil
		}

		// USD budget cap (claude-code's maxBudgetUsd) — same clean
		// pre-request boundary as the wall-clock check above, so no
		// tool_use is left orphaned by a mid-batch stop.
		if l.Budget.Exceeded() {
			stopReason = "budget_usd"
			l.Hooks.EmitLoopEnd(ctx, tc, "budget_usd")
			emit(ctx, out, Event{Kind: EventInfo, Info: l.Budget.ExceededMessage()})
			emit(ctx, out, Event{Kind: EventLoopDone, StopReason: "budget_usd"})
			return nil
		}
		// One-shot 90%-budget warning. Injected HERE — the same
		// pre-request boundary the iter nudge uses — and not in the
		// post-stream usage block, because there the assistant message
		// hadn't been appended yet: the warning would land BEFORE the
		// reply the model produced without seeing it, corrupting
		// transcript order (caught by 2026-06-11 review).
		if warn := l.Budget.TakeWarning(); warn != "" {
			l.mu.Lock()
			l.Messages = append(l.Messages, llm.Message{
				Role:    llm.RoleUser,
				Content: []llm.ContentBlock{{Type: "text", Text: warn}},
			})
			l.mu.Unlock()
			emit(ctx, out, Event{
				Kind: EventInfo,
				Info: fmt.Sprintf("(budget nudge: $%.4f spent — model asked to wrap up)", l.Budget.SpentUSD()),
			})
		}

		l.mu.Lock()
		l.iterIdx++
		globalIter := l.iterIdx
		l.mu.Unlock()
		runIter++
		l.Hooks.EmitTurnStart(ctx, tc, globalIter)

		l.maybeCompact(ctx, out)

		// Drain any background-bash job notifications since the last
		// iteration and synthesize <job_notification> user messages.
		// The model sees these as system-reminders telling it which
		// jobs finished (and whether to BashOutput-read the result),
		// matching claude-code's <task_notification> envelope.
		l.injectJobNotifications(out)
		l.injectPeerMessages(out)
		l.injectSubAgentNotifications(out)
		l.injectDreamNotifications(out)
		// Same pattern for Monitor pattern-matches — pulls every
		// MonitorEvent buffered since last iter and injects them as
		// <monitor_event> system-reminders. Cheap when no Monitor is
		// active (nil registry → instant return).
		l.injectMonitorEvents(out)
		// Re-surface the todo list when it's gone untouched for a while
		// with incomplete items — claude-code's todo_reminder mechanism,
		// the thing that actually keeps tasks from being left stuck
		// mid-status (see todo_reminder.go).
		l.injectTodoReminder(ctx, out)

		// Soft iter-budget nudges at 50% / 75% / 90% — see
		// iter_nudge.go. Fire at most one per threshold per turn so
		// the model gets a heads-up before the hard cap kicks in.
		// runOutputTokens is included so the nudge body can give the
		// model real cost context ("you've also used 12K output
		// tokens"), matching claude-code's tokenBudget continuation
		// message style.
		if idx, body := shouldFireNudgeWithTokens(runIter, l.MaxIters, runOutputTokens, nudgeFired); body != "" {
			nudgeFired[idx] = true
			// Diminishing-return streaks only count after the 75% warning.
			// Previously early cache hits accumulated silently and caused an
			// immediate abort as soon as this threshold was crossed.
			if idx == 1 {
				progress.Reset()
			}
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
				// Providers using transport.RetryWithBackoff already spent
				// their full retry budget. Do not turn three bounded attempts
				// into a second immediate three-attempt round. Custom providers
				// that return a plain transient error still retain this one
				// loop-level recovery attempt.
				if !transport.IsRetryExhausted(err) {
					stream, err = l.Provider.Stream(ctx, req)
				}
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
			runOutputTokens += usage.out
			emit(ctx, out, Event{
				Kind:                     EventTokens,
				InputTokens:              usage.in,
				OutputTokens:             usage.out,
				CacheCreationInputTokens: usage.cacheCreate,
				CacheReadInputTokens:     usage.cacheRead,
			})
			l.Budget.AddUsage(usage.in, usage.out, usage.cacheRead, usage.cacheCreate)
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
			// A user may submit while final text is still streaming. Consume
			// that steer before any end-of-turn gates so it remains part of
			// the current query chain rather than waiting for another prompt.
			if steer := l.drainSteer(); steer != "" {
				l.mu.Lock()
				l.Messages = append(l.Messages, llm.Message{
					Role: llm.RoleUser,
					Content: []llm.ContentBlock{{
						Type: "text",
						Text: "[user steer mid-turn] " + steer,
					}},
				})
				l.mu.Unlock()
				l.emitAssistantReentryBoundary(ctx, out, tc, assistant)
				emit(ctx, out, Event{Kind: EventInfo, Info: "steered: " + steer})
				continue
			}
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
				l.emitAssistantReentryBoundary(ctx, out, tc, assistant)
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

			// End-of-turn todo reconciliation. The model is ending the turn
			// (stop != "tool_use"). If the task list still has open items and
			// we haven't nudged this turn, re-surface it and run one more
			// iteration so the model marks finished items completed (or says
			// what's blocked) before stopping. Without this, a turn that
			// finishes within todoReminderTurns of the last TodoWrite leaves a
			// stale in_progress row in the bottom task strip — the work is
			// delivered but the tracker still shows ◐ (observed 2026-06-17:
			// "总结变更并给出 PR 建议" left in_progress after the PR suggestion was
			// given). Mirrors the contract-gate re-entry above; once-per-turn
			// so a stubborn model can't loop here.
			if !l.todoReconciledThisTurn {
				if items := l.incompleteTodos(); items != nil {
					l.emitAssistantReentryBoundary(ctx, out, tc, assistant)
					l.mu.Lock()
					l.todoReconciledThisTurn = true
					l.mu.Unlock()
					l.appendInjectedMessage(endOfTurnTodoReminder(items))
					emit(ctx, out, Event{
						Kind: EventInfo,
						Info: "[todo] open items at turn end — asking the model to reconcile before stopping",
					})
					continue
				}
			}

			// Close acceptance atomically with the last pending check. If a
			// submit won the race, process it now; otherwise subsequent TUI
			// input falls back to the explicit next-turn queue.
			if steer := l.drainSteerOrClose(); steer != "" {
				l.mu.Lock()
				l.Messages = append(l.Messages, llm.Message{
					Role: llm.RoleUser,
					Content: []llm.ContentBlock{{
						Type: "text",
						Text: "[user steer mid-turn] " + steer,
					}},
				})
				l.mu.Unlock()
				l.emitAssistantReentryBoundary(ctx, out, tc, assistant)
				emit(ctx, out, Event{Kind: EventInfo, Info: "steered: " + steer})
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
		// Keep the provider-visible tool call order stable even when plan-mode
		// control requires phased execution. A single assistant message owns one
		// batch of tool_use blocks, so every matching tool_result must be written
		// back in one RoleUser message. Splitting EnterPlanMode, reads, and denied
		// writes across separate messages breaks strict OpenAI-compatible
		// tool_calls -> tool_results adjacency.
		originalToolUses := append([]llm.ContentBlock(nil), toolUses...)
		resultSlots := make([]llm.ContentBlock, len(originalToolUses))
		resultFilled := make([]bool, len(originalToolUses))
		batchWasSplit := false
		// Reset the todo-reminder countdown whenever the model touches
		// the tracker, so we only re-surface the list after a genuine
		// lull (todo_reminder.go).
		for _, tu := range toolUses {
			if tu.ToolName == "TodoWrite" {
				l.noteTodoWriteActivity(globalIter)
				break
			}
		}
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
		// the sibling tools would still hit executeBatch first. Run
		// EnterPlanMode first (it flips PlanMode synchronously), then
		// route the remaining calls through the plan-mode partition.
		if !l.IsPlanMode() && containsEnterPlanMode(toolUses) {
			enterTools, otherTools := splitEnterPlanModeTools(toolUses)
			results, err := l.executeBatch(ctx, enterTools, out, tc)
			if err != nil {
				stopReason = "error"
				emit(ctx, out, Event{Kind: EventError, Err: err})
				return err
			}
			mergeBatchResults(originalToolUses, enterTools, results, resultSlots, resultFilled)
			batchWasSplit = true
			// Dispatch the rest through the plan-mode partition below. Any
			// mutating sibling is denied by the now-active gate and fed back
			// to the model instead of ending the turn as an archived pseudo-plan.
			toolUses = otherTools
			if !l.IsPlanMode() {
				// EnterPlanMode may be denied by a PreToolUse hook or fail inside
				// an embedder. Never let its siblings fall through to the normal
				// dispatcher: under acceptEdits that would execute the very Write
				// the model asked to protect by entering plan mode.
				const reason = "skipped: EnterPlanMode was denied or did not activate plan mode; sibling execution was refused"
				skipped := make([]llm.ContentBlock, len(otherTools))
				for i, toolUse := range otherTools {
					emit(ctx, out, Event{
						Kind: EventToolStart, ToolUseID: toolUse.ToolUseID,
						ToolName: toolUse.ToolName, ToolInput: toolUse.ToolInput,
					})
					skipped[i] = llm.ContentBlock{
						Type: "tool_result", ToolUseID: toolUse.ToolUseID,
						ToolResult: reason, IsError: true,
					}
					emit(ctx, out, Event{
						Kind: EventToolResult, ToolUseID: toolUse.ToolUseID, ToolName: toolUse.ToolName,
						ToolResult: &ToolResult{Output: reason, IsError: true},
					})
				}
				mergeBatchResults(originalToolUses, otherTools, skipped, resultSlots, resultFilled)
				results = orderedBatchResults(originalToolUses, resultSlots, resultFilled)
				l.contract.observeToolResults(originalToolUses, results)
				if steer := l.drainSteer(); steer != "" {
					results = append(results, llm.ContentBlock{
						Type: "text", Text: "[user steer mid-turn] " + steer,
					})
					emit(ctx, out, Event{Kind: EventInfo, Info: "steered: " + steer})
				}
				if pendingContractReminder != "" {
					results = append(results, llm.ContentBlock{Type: "text", Text: pendingContractReminder})
					pendingContractReminder = ""
				}
				l.mu.Lock()
				l.Messages = append(l.Messages, llm.Message{Role: llm.RoleUser, Content: results})
				l.mu.Unlock()
				l.Hooks.EmitTurnEnd(ctx, tc, l.turnIdx)
				emit(ctx, out, Event{Kind: EventTurnEnd})
				l.turnIdx++
				continue
			}
		}

		// Plan Mode: split the batch three ways:
		//   - EnterPlanMode / ExitPlanMode → execute (plan-control metadata)
		//   - read-only tools → execute (the model needs to read code
		//     while planning; collecting them defeats the point of plan
		//     mode for any non-trivial task)
		//   - side-effect tools → dispatch through Gate, which denies them
		//     while planning and returns the result to the model
		//
		// Pre-fix (2026-05-18), plan mode collected ALL non-Exit tools,
		// including Read / LS / Glob / Grep / MetisInfo, which made it
		// impossible for the model to actually plan against current state.
		// User would say "look at /tmp/foo and propose changes" and the
		// "plan" would just be `Read(/tmp/foo)` — useless. The gate's
		// readOnlyHook already knows which tools are side-effect-free
		// (it's the same hook ModeAcceptEdits uses); plan mode now
		// consults it to partition correctly.
		if l.IsPlanMode() {
			metaTools, nonMeta := splitExitPlanModeTools(toolUses)
			// ExitPlanMode is an interactive approval boundary. Only the first
			// Exit call from this assistant message may run; every sibling was
			// generated before the user saw/approved the plan and must not execute
			// under the newly-relaxed mode. The next provider request receives the
			// Exit result and can generate implementation calls afresh.
			var approvalBoundarySiblings []llm.ContentBlock
			if exitIdx := firstExitPlanModeIndex(toolUses); exitIdx >= 0 {
				metaTools = []llm.ContentBlock{toolUses[exitIdx]}
				approvalBoundarySiblings = make([]llm.ContentBlock, 0, len(toolUses)-1)
				approvalBoundarySiblings = append(approvalBoundarySiblings, toolUses[:exitIdx]...)
				approvalBoundarySiblings = append(approvalBoundarySiblings, toolUses[exitIdx+1:]...)
				nonMeta = nil
			}
			var checker readOnlyChecker
			if l.Gate != nil {
				checker = l.Gate
			}
			readTools, writeTools := splitReadOnlyTools(nonMeta, checker)

			// 1. Execute plan-control tools first so Enter/Exit changes the live
			//    gate before sibling calls are checked. Results are staged rather
			//    than appended; the whole assistant batch is committed below.
			if len(metaTools) > 0 {
				results, err := l.executeBatch(ctx, metaTools, out, tc)
				if err != nil {
					stopReason = "error"
					emit(ctx, out, Event{Kind: EventError, Err: err})
					return err
				}
				mergeBatchResults(originalToolUses, metaTools, results, resultSlots, resultFilled)
				batchWasSplit = true
			}

			if len(approvalBoundarySiblings) > 0 {
				const reason = "skipped: ExitPlanMode is an approval boundary; sibling tool calls from the pre-approval batch were not executed and must be reissued after the approval result"
				results := make([]llm.ContentBlock, len(approvalBoundarySiblings))
				for i, toolUse := range approvalBoundarySiblings {
					emit(ctx, out, Event{
						Kind: EventToolStart, ToolUseID: toolUse.ToolUseID,
						ToolName: toolUse.ToolName, ToolInput: toolUse.ToolInput,
					})
					results[i] = llm.ContentBlock{
						Type: "tool_result", ToolUseID: toolUse.ToolUseID,
						ToolResult: reason, IsError: true,
					}
					emit(ctx, out, Event{
						Kind: EventToolResult, ToolUseID: toolUse.ToolUseID, ToolName: toolUse.ToolName,
						ToolResult: &ToolResult{Output: reason, IsError: true},
					})
				}
				mergeBatchResults(originalToolUses, approvalBoundarySiblings, results, resultSlots, resultFilled)
				batchWasSplit = true
			}

			// 2. Read-only exploration can execute while plan mode is active.
			//    executeBatch still fans independent reads out concurrently.
			if len(readTools) > 0 {
				results, err := l.executeBatch(ctx, readTools, out, tc)
				if err != nil {
					stopReason = "error"
					emit(ctx, out, Event{Kind: EventError, Err: err})
					return err
				}
				mergeBatchResults(originalToolUses, readTools, results, resultSlots, resultFilled)
				batchWasSplit = true
			}

			// 3. Mutating tools are still dispatched through the plan gate when
			//    this batch has no ExitPlanMode approval boundary. While plan
			//    remains active they receive a normal DENY result, which lets the
			//    model recover and call ExitPlanMode in a later request.
			if len(writeTools) > 0 {
				if l.Gate == nil {
					// Defensive embedder fallback: no gate means we cannot prove any
					// remaining call is safe. Return one explicit denial per tool so
					// the provider never receives orphan tool_use IDs.
					results := make([]llm.ContentBlock, len(writeTools))
					for i, toolUse := range writeTools {
						const reason = "denied: plan mode has no permission gate; refusing to execute a potentially mutating tool"
						emit(ctx, out, Event{
							Kind: EventToolStart, ToolUseID: toolUse.ToolUseID,
							ToolName: toolUse.ToolName, ToolInput: toolUse.ToolInput,
						})
						results[i] = llm.ContentBlock{
							Type: "tool_result", ToolUseID: toolUse.ToolUseID,
							ToolResult: reason, IsError: true,
						}
						emit(ctx, out, Event{
							Kind: EventToolResult, ToolUseID: toolUse.ToolUseID, ToolName: toolUse.ToolName,
							ToolResult: &ToolResult{Output: reason, IsError: true},
						})
					}
					mergeBatchResults(originalToolUses, writeTools, results, resultSlots, resultFilled)
				} else {
					results, err := l.executeBatch(ctx, writeTools, out, tc)
					if err != nil {
						stopReason = "error"
						emit(ctx, out, Event{Kind: EventError, Err: err})
						return err
					}
					mergeBatchResults(originalToolUses, writeTools, results, resultSlots, resultFilled)
				}
				batchWasSplit = true
			}

			// 4. Commit exactly one result message, ordered exactly like the
			//    assistant's original tool_use blocks. Steering and contract
			//    reminders are folded in once after every tool result.
			results := orderedBatchResults(originalToolUses, resultSlots, resultFilled)
			l.contract.observeToolResults(originalToolUses, results)
			if steer := l.drainSteer(); steer != "" {
				results = append(results, llm.ContentBlock{
					Type: "text", Text: "[user steer mid-turn] " + steer,
				})
				emit(ctx, out, Event{Kind: EventInfo, Info: "steered: " + steer})
			}
			if pendingContractReminder != "" {
				results = append(results, llm.ContentBlock{Type: "text", Text: pendingContractReminder})
				pendingContractReminder = ""
			}
			l.mu.Lock()
			l.Messages = append(l.Messages, llm.Message{Role: llm.RoleUser, Content: results})
			l.mu.Unlock()
			l.Hooks.EmitTurnEnd(ctx, tc, l.turnIdx)
			emit(ctx, out, Event{Kind: EventTurnEnd})
			l.turnIdx++
			continue
		}

		// Execute tool batch.
		results, err := l.executeBatch(ctx, toolUses, out, tc)
		if err != nil {
			stopReason = "error"
			emit(ctx, out, Event{Kind: EventError, Err: err})
			return err
		}
		if batchWasSplit {
			mergeBatchResults(originalToolUses, toolUses, results, resultSlots, resultFilled)
			results = orderedBatchResults(originalToolUses, resultSlots, resultFilled)
			toolUses = originalToolUses
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

		// Diminishing-returns is advisory, never a hard stop inside the
		// tool loop. The old branch returned before appending `results`,
		// so orphan repair replaced a successfully completed Read with
		// "tool_use never completed" and the user got no final answer.
		// After three low-output iterations *past* 75%, preserve the real
		// results and inject one bounded recovery reminder instead.
		progress.RecordIter(toolUses, results)
		if !diminishingRescued && progress.IsDiminishing() && len(nudgeFired) >= 2 && nudgeFired[1] {
			diminishingRescued = true
			results = append(results, llm.ContentBlock{Type: "text", Text: diminishingReturnsRescueMessage})
			progress.Reset()
			emit(ctx, out, Event{
				Kind: EventInfo,
				Info: fmt.Sprintf("diminishing returns detected after %d low-output iters past 75%% — preserving results and focusing the current task", progressMaxLowIters),
			})
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
			// The tools have already completed. Persist their real results before
			// terminating so Run's orphan-repair defer does not replace them with
			// synthetic "tool_use never completed" failures.
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
		if runIter >= l.MaxIters {
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
				// Strip tools from the next request so the model MUST
				// emit text and the turn ends with a conclusion, not a
				// dangling tool_use whose results nobody summarizes.
				l.rescueNoTools = true
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
	// Resolve the plan overlay body from the explicit runtime injection, or
	// from a legacy boot-time plan_mode section. We remove static copies from
	// both request representations below and re-add exactly one copy according
	// to the live Loop.PlanMode flag, so EnterPlanMode/ExitPlanMode affect the
	// very next provider request.
	planPrompt := l.PlanSystemPrompt
	planBodiesToRemove := make([]string, 0, 2)
	if planPrompt != "" {
		planBodiesToRemove = append(planBodiesToRemove, planPrompt)
	}
	for _, section := range l.SystemSections {
		if section.Name != "plan_mode" || section.Body == "" {
			continue
		}
		planBodiesToRemove = append(planBodiesToRemove, section.Body)
		// A legacy static section is richer than NewLoop's local fallback.
		// Explicit runtime injection still wins when it is non-fallback.
		if planPrompt == "" || planPrompt == agentPlanSystemPromptFallback {
			planPrompt = section.Body
		}
	}
	for _, body := range planBodiesToRemove {
		system = strings.TrimSpace(strings.ReplaceAll(system, body, ""))
	}
	if planPrompt != "" {
		if l.planMode {
			if system != "" {
				system += "\n\n"
			}
			system += planPrompt
		}
	}

	if len(l.SystemSections) > 0 {
		sections = make([]llm.SystemSection, 0, len(l.SystemSections)+3)
		for _, section := range l.SystemSections {
			if section.Name != "plan_mode" {
				sections = append(sections, section)
			}
		}
		if l.planMode && planPrompt != "" {
			sections = append(sections, llm.SystemSection{
				Name: "plan_mode", Body: planPrompt, Cache: false, Volatile: true,
			})
		}
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
		// Effort has a dedicated lock so the TUI can update it while the main
		// request snapshot lock is busy assembling memory and messages.
		Effort: l.EffortValue(),
	}
	// Final-summary rescue: the iter cap exhausted and the model was told to
	// write the answer now. Strip the tool list so the provider can only
	// emit text — otherwise a tool_use here produces results that never get
	// a follow-up summary (the turn ends right after), the exact "ran and
	// stopped with no conclusion" failure mode.
	if l.rescueNoTools {
		req.Tools = nil
		l.rescueNoTools = false
	}
	// Fast mode is a pure request-time override. We don't mutate
	// l.effort because the user's persistent /effort preference
	// should survive a transient /fast toggle.
	if l.FastEnabled() {
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

// splitExitPlanModeTools partitions plan-control meta tools vs everything
// else. The historical name is retained for test/source compatibility.
// EnterPlanMode must be executable too: models commonly call it even when the
// CLI already started with --mode plan, and archiving that no-op as a proposed
// write was the root cause of the one-tool "plan archived" dead-end.
func splitExitPlanModeTools(blocks []llm.ContentBlock) (exit, other []llm.ContentBlock) {
	for _, b := range blocks {
		if b.ToolName == "EnterPlanMode" || b.ToolName == "ExitPlanMode" {
			exit = append(exit, b)
		} else {
			other = append(other, b)
		}
	}
	return
}

// firstExitPlanModeIndex locates the interactive approval boundary in a tool
// batch. The caller executes only this call and refuses every sibling, so an
// already-generated Write cannot ride the permission relaxation granted by the
// user's plan approval.
func firstExitPlanModeIndex(blocks []llm.ContentBlock) int {
	for i, b := range blocks {
		if b.ToolName == "ExitPlanMode" {
			return i
		}
	}
	return -1
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

// mergeBatchResults puts the results of one execution phase back into slots
// belonging to the assistant's original tool_use batch. Non-empty IDs are the
// primary key. Some OpenAI-compatible providers omit IDs while streaming, so
// the fallback is the first still-unfilled call with the same tool name. A
// final first-unfilled fallback keeps malformed provider output API-valid
// instead of manufacturing an orphan.
func mergeBatchResults(original, batch, results, slots []llm.ContentBlock, filled []bool) {
	for batchIdx, toolUse := range batch {
		slot := -1
		if toolUse.ToolUseID != "" {
			for i, candidate := range original {
				if !filled[i] && candidate.ToolUseID == toolUse.ToolUseID {
					slot = i
					break
				}
			}
		}
		if slot < 0 {
			for i, candidate := range original {
				if !filled[i] && candidate.ToolName == toolUse.ToolName {
					slot = i
					break
				}
			}
		}
		if slot < 0 {
			for i := range original {
				if !filled[i] {
					slot = i
					break
				}
			}
		}
		if slot < 0 || slot >= len(slots) || slot >= len(filled) {
			continue
		}

		var result llm.ContentBlock
		if batchIdx < len(results) {
			result = results[batchIdx]
		}
		if result.Type == "" {
			result = llm.ContentBlock{
				Type:       "tool_result",
				ToolUseID:  original[slot].ToolUseID,
				ToolResult: "tool execution completed without a result block",
				IsError:    true,
			}
		}
		if result.ToolUseID == "" {
			result.ToolUseID = original[slot].ToolUseID
		}
		slots[slot] = result
		filled[slot] = true
	}
}

// orderedBatchResults returns exactly one result block per original tool_use,
// preserving order. The synthetic fallback should only be reachable for a
// malformed provider/dispatcher response, but keeping it here guarantees the
// next request never relies on the much later orphan-repair pass.
func orderedBatchResults(original, slots []llm.ContentBlock, filled []bool) []llm.ContentBlock {
	ordered := make([]llm.ContentBlock, len(original))
	for i, toolUse := range original {
		if i < len(slots) && i < len(filled) && filled[i] && slots[i].Type != "" {
			ordered[i] = slots[i]
			continue
		}
		ordered[i] = llm.ContentBlock{
			Type:       "tool_result",
			ToolUseID:  toolUse.ToolUseID,
			ToolResult: "tool was not dispatched while processing its assistant batch",
			IsError:    true,
		}
	}
	return ordered
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
	// Dispatcher-style tools (notably Skill and Agent) need the whole input
	// to decide whether this particular call is read-only. Preserve it as
	// JSON for the gate hook instead of collapsing every action to "".
	if b, err := json.Marshal(in); err == nil {
		return string(b)
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
