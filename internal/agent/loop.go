package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

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
	// CurrentStateSnapshot reads authoritative mutable state. buildRequest
	// compares the structured value with the last session baseline and only
	// re-renders the runtime section when a field actually changes. The exact
	// same bytes are reused on unchanged iterations, preserving provider prompt
	// cache prefixes without letting permission/cwd/plan metadata go stale.
	CurrentStateSnapshot func() RuntimeStateSnapshot
	// CurrentStateSections is the legacy section callback retained for
	// embedders. Runtime wiring uses CurrentStateSnapshot; legacy sections stay
	// volatile because their semantic stability cannot be proven.
	CurrentStateSections func() []llm.SystemSection
	runtimeStateSnapshot RuntimeStateSnapshot
	runtimeStateBody     string
	runtimeStateReady    bool
	runtimeStateRevision uint64
	Model                string
	MaxIters             int
	GraceCalls           int

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
	Memory memory.Repository

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

	// turnMemory* freezes the repository snapshot and query-specific recall
	// once at the start of a user turn. Tool iterations then reuse the exact
	// same bytes instead of re-reading Daily/topic files or rebuilding BM25
	// while l.mu is held. The recall block is attached to the originating user
	// message (Synthetic=true), not the system prompt, so the next request can
	// reuse the complete prior conversation prefix.
	turnMemoryContext        string
	turnMemoryRecall         string
	turnMemoryPrepared       bool
	turnMemoryRecallAttached bool

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
	// CompactionCheckpoint, when non-nil, durably records a context
	// replacement before CompactNow installs it as the loop's final live
	// history. Both arguments are exact snapshots: before is the raw history
	// that was summarized (including any not-yet-persisted tool tail), and
	// after is the final replacement including PostCompact hook context.
	//
	// Session-backed surfaces should wire this to
	// session.Store.CheckpointCompaction. It takes precedence over the legacy
	// per-call CompactOptions.Persist callback so automatic, overflow and
	// second-wind compaction cannot bypass the raw-ledger checkpoint. The
	// callback is synchronous, but CompactNow invokes it without holding
	// Loop.mu, so implementations may safely inspect the loop (for example via
	// History). Recursive compaction is still unsupported.
	CompactionCheckpoint func(before, after []llm.Message) error

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

	// RepeatGuard watches consecutive identical tool calls and injects an
	// escalating advisory reminder (DSH repeat-tool-reminder parity). nil =
	// disabled; NewRepeatGuard applies the [3,5,8] defaults when nil.
	RepeatGuard *RepeatGuard

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

	// compactMu serializes complete compaction transactions. Loop.mu is only
	// held for short history snapshots/CAS installs; provider calls, hooks,
	// persistence and event callbacks must remain outside it.
	compactMu sync.Mutex
	mu        sync.RWMutex
	Messages  []llm.Message
	// compactorResetPending is set when a reset/session replacement happens
	// inside a compaction callback and therefore cannot acquire compactMu. The
	// current compactMu owner clears mutable summary/circuit state before
	// releasing the transaction. Protected by mu.
	compactorResetPending bool
	// estTokens caches the last context-token estimate so the TUI can read
	// it WITHOUT taking l.mu (see EstimateContextTokens). Keeping this cache
	// avoids making render frames contend with request/history assembly.
	estTokens atomic.Int64
	// requestOverheadTokens caches the most recent non-history request cost
	// (system/state/memory/tool schemas). Render paths add this without doing
	// filesystem reads, memory retrieval or registry hydration every frame.
	requestOverheadTokens atomic.Int64
	// activeContext is the provider-authoritative active-window snapshot from
	// the most recently completed response. Unlike EventTokens/Budget usage it
	// is replaced, never accumulated. Protected by mu; estTokens remains the
	// non-blocking render fallback while mu is busy.
	activeContext activeContextSnapshot
	// autoCompactPressurePinned remembers the history size immediately after an
	// automatic compaction that could not bring the full request below the
	// trigger because non-history overhead was already too large. Until the
	// transcript grows materially, another summary would only rewrite the same
	// checkpoint, spend tokens and eventually trip the failure circuit.
	// Protected by mu.
	autoCompactPressurePinned bool
	autoCompactHistoryTokens  int
	autoCompactOverheadTokens int
	// autoCompactHistoryRevision distinguishes a normal append (which should
	// count toward re-arming) from a manual/overflow/second-wind replacement
	// (which must establish a new baseline). autoCompactSessionGeneration keeps
	// a CompactNow result from re-arming state after Reset/Restore won the race.
	autoCompactHistoryRevision uint64
	historyRevision            uint64
	// routingRevision advances whenever provider/model runtime binding changes.
	// Long-running compaction transactions capture it before calling the
	// summarizer and refuse to install a checkpoint produced by an obsolete
	// provider after a concurrent model switch.
	routingRevision              uint64
	historyReplacementTokens     int
	autoCompactSessionGeneration uint64
	turnIdx                      int
	iterIdx                      int

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

	// distillJobs owns the background auto-distillation lifecycle. A session
	// delete or process shutdown can cancel and join these jobs before removing
	// session-owned memory, so a late provider response cannot recreate data
	// after deletion. The provider, repository, provenance and exchange are
	// captured before a job is registered; no goroutine reads mutable Loop
	// routing/history state after a session switch.
	distillMu     sync.Mutex
	distillNextID uint64
	distillJobs   map[uint64]*distillJob
	// distillPending retains immutable successful exchanges until either the
	// normal cadence or a session durability boundary launches them. The
	// source-message key is the idempotency watermark: repeated boundaries can
	// neither launch an in-flight exchange nor re-distill one that completed.
	distillPending   map[string]distillSnapshot
	distillInFlight  map[string]struct{}
	distillWatermark map[string]struct{}
	distillFailures  map[string]error
	distillSlots     chan struct{}

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

type distillJob struct {
	sessionID string
	cancel    context.CancelFunc
	done      chan struct{}
}

type distillSnapshot struct {
	repository      memory.Repository
	provider        llm.Provider
	sessionID       string
	sourceMessageID string
	userMsg         string
	assistantMsg    string
	turn            int
}

// DefaultDistillEvery is deliberately installed by NewLoop. Zero has one
// unambiguous contract everywhere else: disable both cadence and residual
// boundary distillation.
const DefaultDistillEvery = 5

// maxConcurrentDistillations prevents a long or retried session boundary from
// turning into an unbounded provider-call burst. At the default cadence this
// still lets the complete five-exchange batch make progress together.
const maxConcurrentDistillations = DefaultDistillEvery

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
		DistillEvery:                DefaultDistillEvery,
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
	// The previous turn's frozen index/recall must never be reused by context
	// estimation or direct request inspection after a new prompt is appended.
	l.clearTurnMemoryLocked()
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
	l.clearTurnMemoryLocked()
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

func (l *Loop) clearTurnMemoryLocked() {
	l.turnMemoryContext = ""
	l.turnMemoryRecall = ""
	l.turnMemoryPrepared = false
	l.turnMemoryRecallAttached = false
}

// prepareTurnMemory snapshots stable memory and performs query recall before
// the provider iteration loop starts. Filesystem reads, BM25 construction and
// optional LLM reranking therefore happen without l.mu, remain cancellable,
// and run at most once for the person's submitted turn.
func (l *Loop) prepareTurnMemory(ctx context.Context) {
	if l == nil {
		return
	}
	l.mu.RLock()
	manager := l.Memory
	k := l.AutoRetrieveK
	rerank := l.AutoRetrieveRerank
	provider := l.Provider
	query := lastUserTextLocked(l.Messages)
	revision := l.historyRevision
	l.mu.RUnlock()

	memBody, retrieveBody := "", ""
	var picked []memory.Passage
	if manager != nil {
		memBody = manager.BuildContext()
		if k > 0 && query != "" {
			candidates := manager.AutoRetrieveCandidates(query, k)
			if rerank && provider != nil {
				candidates = manager.AutoRetrieveCandidates(query, k*3)
				picked = rerankAutoRetrieve(ctx, provider, query, candidates, k)
			} else {
				picked = candidates
			}
			retrieveBody = memory.FormatRetrieveSection(picked)
		}
	}

	l.mu.Lock()
	// A top-level history replacement won the race while recall was being
	// built. Do not attach data selected for the previous session/query.
	if l.historyRevision != revision {
		l.clearTurnMemoryLocked()
		l.mu.Unlock()
		return
	}
	l.turnMemoryContext = memBody
	l.turnMemoryRecall = retrieveBody
	l.turnMemoryPrepared = true
	if retrieveBody == "" {
		l.mu.Unlock()
		return
	}
	idx := transcript.LastPlainUserIndex(l.Messages)
	if idx < 0 || messageContainsAutoRetrieve(l.Messages[idx]) {
		l.mu.Unlock()
		return
	}
	l.Messages[idx].Content = append(l.Messages[idx].Content, llm.ContentBlock{
		Type: "text", Text: retrieveBody, Synthetic: true,
	})
	l.turnMemoryRecallAttached = true
	l.mu.Unlock()

	// Retrieval candidates and token estimates are previews, not memory use.
	// Persist usage only after the exact Top-K has survived the session/history
	// revision check and was attached to the request-bound user message.
	_ = manager.MarkRetrieved(picked)
}

func messageContainsAutoRetrieve(message llm.Message) bool {
	for _, block := range message.Content {
		if block.Type == "text" && strings.Contains(block.Text, "<auto-retrieve") {
			return true
		}
	}
	return false
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

// EstimateContextTokens returns the active provider context plus any local
// messages appended since the last response and the latest request-overhead
// delta. Providers that do not report usable prompt usage fall back to the
// full-history byte estimate. Exposing the same estimator on the public
// surface keeps TUI/Desktop status and compaction pressure aligned.
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
		overhead := int(l.requestOverheadTokens.Load())
		v := 0
		if base, ok := l.activeContextBaseLocked(); ok {
			v = max(base+overhead, 0)
		} else {
			v = estimateActiveHistoryTokens(l.Messages) + overhead
		}
		l.mu.RUnlock()
		l.estTokens.Store(int64(v))
		return v
	}
	return int(l.estTokens.Load())
}

func (l *Loop) storeContextEstimateFromHistory(historyTokens int) {
	if l == nil {
		return
	}
	// Every caller holds l.mu and invokes this after replacing the logical
	// history (including CompactNow's temporary/final/rollback installations).
	// Ordinary appends intentionally do not advance the revision: their token
	// growth is what re-arms a pressure-pinned automatic checkpoint.
	l.invalidateActiveContextLocked()
	l.historyRevision++
	l.historyReplacementTokens = historyTokens
	l.estTokens.Store(int64(estimateActiveHistoryTokens(l.Messages)) + l.requestOverheadTokens.Load())
}

// EstimateRequestContextTokens preflights the input that the next provider
// request will carry without consuming one-shot cache/rescue flags or calling
// an LLM reranker. Callers must obtain specs outside Loop.mu: lazy MCP tool
// hydration may itself acquire that mutex.
func (l *Loop) EstimateRequestContextTokens(specs []llm.ToolSpec) int {
	if l == nil {
		return 0
	}

	l.mu.RLock()
	historyTokens := estimateTokens(l.Messages)
	activeBase, hasActiveSnapshot := l.activeContextBaseLocked()
	system := l.System
	sections := append([]llm.SystemSection(nil), l.SystemSections...)
	planMode := l.planMode
	planPrompt := l.PlanSystemPrompt
	planBodiesToRemove := make([]string, 0, 2)
	if planPrompt != "" {
		planBodiesToRemove = append(planBodiesToRemove, planPrompt)
	}
	for _, section := range sections {
		if section.Name != "plan_mode" || section.Body == "" {
			continue
		}
		planBodiesToRemove = append(planBodiesToRemove, section.Body)
		if planPrompt == "" || planPrompt == agentPlanSystemPromptFallback {
			planPrompt = section.Body
		}
	}
	for _, body := range planBodiesToRemove {
		system = strings.TrimSpace(strings.ReplaceAll(system, body, ""))
	}
	if planMode && planPrompt != "" {
		if system != "" {
			system += "\n\n"
		}
		system += planPrompt
	}
	memoryManager := l.Memory
	autoRetrieveK := l.AutoRetrieveK
	query := lastUserTextLocked(l.Messages)
	turnMemoryPrepared := l.turnMemoryPrepared
	turnMemoryContext := l.turnMemoryContext
	turnMemoryRecall := l.turnMemoryRecall
	turnMemoryRecallAttached := l.turnMemoryRecallAttached
	currentStateSnapshot := l.CurrentStateSnapshot
	currentStateSections := l.CurrentStateSections
	runtimeModel := l.Model
	runtimeProviderName, runtimeProviderModel := "", ""
	if l.Provider != nil {
		runtimeProviderName = l.Provider.Name()
		runtimeProviderModel = l.Provider.ModelID()
	}
	rescueWithoutTools := l.rescueNoTools
	l.mu.RUnlock()

	memBody, retrieveBody := turnMemoryContext, turnMemoryRecall
	if turnMemoryRecallAttached {
		retrieveBody = ""
	}
	if !turnMemoryPrepared && memoryManager != nil {
		memBody = memoryManager.BuildContext()
		if autoRetrieveK > 0 {
			// Preflight deliberately uses the local BM25 path even when live
			// requests enable reranking. Estimation must never spend tokens.
			retrieveBody = memoryManager.PreviewAutoRetrieve(query, autoRetrieveK)
		}
	}
	dynamic := []llm.SystemSection(nil)
	if currentStateSnapshot != nil {
		state := currentStateSnapshot()
		state.PlanMode = planMode
		if state.Model == "" {
			state.Model = runtimeModel
		}
		if state.Provider == "" {
			state.Provider = runtimeProviderName
		}
		if state.Model == "" {
			state.Model = runtimeProviderModel
		}
		dynamic = []llm.SystemSection{{Name: "runtime_state", Body: state.Render(), Cache: true}}
	} else if currentStateSections != nil {
		dynamic = currentStateSections()
	}

	requestSections := []llm.SystemSection(nil)
	if len(sections) > 0 {
		requestSections = make([]llm.SystemSection, 0, len(sections)+len(dynamic)+3)
		for _, section := range sections {
			if section.Name == "plan_mode" {
				continue
			}
			requestSections = append(requestSections, section)
		}
		if memBody != "" {
			requestSections = append(requestSections, llm.SystemSection{
				Name: "memory_index", Body: memBody, Cache: true,
			})
		}
		if planMode && planPrompt != "" {
			requestSections = append(requestSections, llm.SystemSection{
				Name: "plan_mode", Body: planPrompt, Cache: false, Volatile: true,
			})
		}
		requestSections = append(requestSections, dynamic...)
		if retrieveBody != "" {
			requestSections = append(requestSections, llm.SystemSection{
				Name: "auto-retrieve", Body: retrieveBody, Cache: false, Volatile: true,
			})
		}
	} else {
		for _, section := range dynamic {
			if strings.TrimSpace(section.Body) != "" {
				system += "\n\n" + section.Body
			}
		}
		if memBody != "" {
			system += "\n\n" + memBody
		}
		if retrieveBody != "" {
			system += "\n\n" + retrieveBody
		}
	}
	requestTools := specs
	if rescueWithoutTools {
		requestTools = nil
	}
	overhead := estimateRequestOverhead(llm.Request{
		System: system, SystemSections: requestSections, Tools: requestTools,
	})

	l.requestOverheadTokens.Store(int64(overhead))
	total := historyTokens + overhead
	if hasActiveSnapshot {
		total = max(activeBase+overhead, 0)
	}
	displayTotal := estimateActiveHistoryTokens(l.Messages) + overhead
	if hasActiveSnapshot {
		displayTotal = max(activeBase+overhead, 0)
	}
	l.estTokens.Store(int64(displayTotal))
	return total
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

// ProviderRuntimeSnapshot is a coherent copy of the provider-controlled
// request state. It is used by transactional switch surfaces to roll back a
// failed metadata commit without reconstructing the old binding piecemeal.
type ProviderRuntimeSnapshot struct {
	Provider        llm.Provider
	Model           string
	ContextWindow   int
	MaxOutputTokens int
	System          string
	SystemSections  []llm.SystemSection
}

// RebindProviderModel atomically swaps the provider/model/window tuple while
// preserving the current output budget and system prompt. String-only/test
// switches use this compatibility surface; production provider rebuilds use
// RebindProviderRuntime so target-provider limits and hints change together.
func (l *Loop) RebindProviderModel(provider llm.Provider, model string) {
	l.rebindProviderRuntime(provider, model, 0, false, "", nil, false)
}

// RebindProviderRuntime atomically installs a complete provider runtime:
// routing identity, model window, target output budget, and provider-managed
// prompt sections. This prevents a request from observing a new transport with
// an old provider hint (or old compaction reservation).
func (l *Loop) RebindProviderRuntime(provider llm.Provider, model string, maxOutputTokens int, system string, sections []llm.SystemSection) {
	l.rebindProviderRuntime(provider, model, maxOutputTokens, true, system, sections, true)
}

func (l *Loop) rebindProviderRuntime(provider llm.Provider, model string, maxOutputTokens int, replaceOutput bool, system string, sections []llm.SystemSection, replacePrompt bool) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()

	window := 0
	if provider != nil {
		window = provider.MaxContextTokens()
	}
	if old := l.Compactor; old != nil && provider != nil {
		outputBudget := old.MaxOutputTokens
		if replaceOutput {
			outputBudget = max(maxOutputTokens, 0)
		}
		next := NewCompactor(old.Config, model, window, provider)
		next.MaxOutputTokens = outputBudget
		next.ApplyWindowTier(window - outputBudget)
		l.Compactor = next
	} else if provider == nil {
		l.Compactor = nil
	}
	l.Provider = provider
	l.Model = model
	l.ContextWindow = window
	if replacePrompt {
		l.System = system
		l.SystemSections = append([]llm.SystemSection(nil), sections...)
	}
	l.routingRevision++
	l.invalidateActiveContextLocked()
	l.estTokens.Store(int64(estimateActiveHistoryTokens(l.Messages)) + l.requestOverheadTokens.Load())
}

// ProviderRuntimeState returns an immutable provider/runtime binding snapshot.
func (l *Loop) ProviderRuntimeState() ProviderRuntimeSnapshot {
	if l == nil {
		return ProviderRuntimeSnapshot{}
	}
	l.mu.RLock()
	defer l.mu.RUnlock()
	snapshot := ProviderRuntimeSnapshot{
		Provider:       l.Provider,
		Model:          l.Model,
		ContextWindow:  l.ContextWindow,
		System:         l.System,
		SystemSections: append([]llm.SystemSection(nil), l.SystemSections...),
	}
	if l.Compactor != nil {
		snapshot.MaxOutputTokens = l.Compactor.MaxOutputTokens
	}
	return snapshot
}

// ProviderModelSnapshot returns a coherent routing tuple for callers that
// need to rebuild prompts or persist selector state without racing a switch.
func (l *Loop) ProviderModelSnapshot() (llm.Provider, string, int) {
	if l == nil {
		return nil, "", 0
	}
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.Provider, l.Model, l.ContextWindow
}

// ContextStatusSnapshot exposes immutable status values under the loop lock.
// The active token estimate is computed separately through
// EstimateContextTokens so render paths retain its non-blocking TryRLock
// behavior while a long compaction owns the history lock.
func (l *Loop) ContextStatusSnapshot() (window int, threshold float64, trigger int) {
	if l == nil {
		return 0, 0, 0
	}
	l.mu.RLock()
	defer l.mu.RUnlock()
	window = l.ContextWindow
	if l.Compactor != nil {
		threshold = l.Compactor.Config.Threshold
		trigger = l.Compactor.TriggerTokens()
	}
	return window, threshold, trigger
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
	if l == nil {
		return
	}
	// Reset is allowed from CompactNow callbacks. TryLock keeps that re-entry
	// non-blocking; when a transaction is active, history changes immediately
	// and its owner performs the deferred circuit/summary reset on release.
	ownsCompact := l.compactMu.TryLock()
	l.mu.Lock()
	l.Messages = nil
	l.invalidateActiveContextLocked()
	l.turnIdx = 0
	l.iterIdx = 0
	l.historyRevision++
	l.autoCompactSessionGeneration++
	l.clearAutoCompactPressureLocked()
	l.invalidateRuntimeStateLocked()
	l.clearTurnMemoryLocked()
	l.compactCircuitNoticeSent = false
	l.contract.reset()
	if ownsCompact {
		if l.Compactor != nil {
			l.Compactor.ResetCircuit()
		}
		l.compactorResetPending = false
	} else {
		l.compactorResetPending = true
	}
	l.requestOverheadTokens.Store(0)
	l.estTokens.Store(0)
	// Releasing compactMu while mu is still held closes the hand-off gap: a
	// reset that just failed TryLock cannot publish pending state after an owner
	// has already performed its final pending-state check.
	if ownsCompact {
		l.compactMu.Unlock()
	}
	l.mu.Unlock()
	if !ownsCompact {
		l.flushPendingCompactorResetIfIdle()
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
	l.storeContextEstimateFromHistory(estimateTokens(l.Messages))
}

// FirePostCompactHook remains for compatibility with external/manual history
// replacement callers. CompactNow owns hooks for every built-in automatic,
// manual, overflow and second-wind path. Any AdditionalContext returned by
// handlers is appended as a user message so the next request re-anchors.
func (l *Loop) FirePostCompactHook(ctx context.Context, trigger, tier string,
	beforeMsgs, afterMsgs, beforeToks, afterToks int) {
	if l == nil || l.Hooks == nil {
		return
	}
	l.mu.Lock()
	model, turn := l.Model, l.turnIdx
	l.mu.Unlock()
	pc := &PostCompact{
		Trigger:        trigger,
		Tier:           tier,
		BeforeMessages: beforeMsgs,
		AfterMessages:  afterMsgs,
		BeforeTokens:   beforeToks,
		AfterTokens:    afterToks,
	}
	extra := l.Hooks.EmitPostCompact(ctx, HookContext{Model: model, Turn: turn}, pc)
	if strings.TrimSpace(extra) == "" {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.Messages = append(l.Messages, llm.Message{
		Role: llm.RoleUser,
		Content: []llm.ContentBlock{{
			Type: "text",
			Text: "[post-compact hook context] " + extra,
		}},
	})
	l.storeContextEstimateFromHistory(estimateTokens(l.Messages))
}

func (l *Loop) restoreMessagesLocked(messages []llm.Message) {
	l.invalidateActiveContextLocked()
	if messages == nil {
		l.Messages = nil
	} else {
		clone := append([]llm.Message(nil), messages...)
		l.Messages = RepairOrphanedToolUses(clone)
	}
	l.turnIdx = 0
	l.iterIdx = 0
	l.historyRevision++
	l.autoCompactSessionGeneration++
	l.clearAutoCompactPressureLocked()
	l.invalidateRuntimeStateLocked()
	l.clearTurnMemoryLocked()
}

// ResetSession crosses a top-level chat-session boundary. Unlike Restore,
// which is also used by lightweight history swaps, this clears every mutable
// guard/cache whose lifetime must not span independent conversations.
// Callers invoke it only while no foreground turn is running.
func (l *Loop) ResetSession(messages []llm.Message) {
	if l == nil {
		return
	}
	// Session replacement is also legal from a lifecycle/persistence callback.
	// Publish it immediately and defer only mutable Compactor state when the
	// active transaction owns compactMu.
	ownsCompact := l.compactMu.TryLock()
	l.mu.Lock()
	l.restoreMessagesLocked(messages)
	l.requestOverheadTokens.Store(0)
	// ResetSession can be followed immediately by a non-blocking status poll.
	// Prime the fallback from the restored history while mu is held so a
	// concurrent compaction/lifecycle write lock cannot make a real session
	// appear to have an empty context window.
	l.estTokens.Store(int64(estimateActiveHistoryTokens(l.Messages)))
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
	if ownsCompact {
		if l.Compactor != nil {
			l.Compactor.ResetCircuit()
		}
		l.compactorResetPending = false
	} else {
		l.compactorResetPending = true
	}
	if ownsCompact {
		l.compactMu.Unlock()
	}
	l.mu.Unlock()
	if !ownsCompact {
		l.flushPendingCompactorResetIfIdle()
	}
	if l.Budget != nil {
		l.Budget.Reset()
	}
	if l.Detector != nil {
		l.Detector.Reset()
	}
	if l.RepeatGuard != nil {
		l.RepeatGuard.Reset()
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

// finishCompactorCriticalSection is the only release path for a compactMu
// owner that can overlap Reset/ResetSession. It applies any deferred reset to
// the currently installed Compactor, then releases compactMu while still
// holding mu so a failed TryLock cannot miss the final pending-state check.
func (l *Loop) finishCompactorCriticalSection() {
	l.mu.Lock()
	if l.compactorResetPending {
		if l.Compactor != nil {
			l.Compactor.ResetCircuit()
		}
		l.compactorResetPending = false
	}
	l.compactMu.Unlock()
	l.mu.Unlock()
}

// flushPendingCompactorResetIfIdle handles the narrow case where Reset lost
// TryLock to an owner that released before Reset could publish the pending
// bit. If another owner still exists, its finish path is responsible.
func (l *Loop) flushPendingCompactorResetIfIdle() {
	if l.compactMu.TryLock() {
		l.finishCompactorCriticalSection()
	}
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
	l.storeContextEstimateFromHistory(estimateTokens(l.Messages))
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

	// Freeze stable memory + Top-K recall before any request is assembled.
	// This deliberately runs outside l.mu; a slow disk or optional reranker
	// must not freeze Desktop status, stop, or session-navigation paths.
	l.prepareTurnMemory(ctx)

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
	compactedAtCap := false // at most one forced-compaction "second wind" per turn
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

		// Drain any background-bash job notifications since the last
		// iteration and synthesize <job_notification> user messages.
		// The model sees these as system-reminders telling it which
		// jobs finished (and whether to BashOutput-read the result),
		// matching claude-code's <task_notification> envelope.
		l.injectJobNotifications(ctx, out)
		l.injectPeerMessages(ctx, out)
		l.injectSubAgentNotifications(ctx, out)
		l.injectDreamNotifications(ctx, out)
		// Same pattern for Monitor pattern-matches — pulls every
		// MonitorEvent buffered since last iter and injects them as
		// <monitor_event> system-reminders. Cheap when no Monitor is
		// active (nil registry → instant return).
		l.injectMonitorEvents(ctx, out)
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

		// Tool/skill/plugin availability is live runtime state. Refresh the
		// schema every iteration so post-compaction requests cannot inherit a
		// stale Run-start catalog after lazy MCP discovery or plugin changes.
		specs = l.toolSpecs()
		pressureTokens := l.EstimateRequestContextTokens(specs)
		l.maybeCompactWithPressure(ctx, out, pressureTokens)
		requestWithoutTools := l.rescueNoToolsSnapshot()
		req, contextAnchor := l.buildRequestWithContext(specs)
		provider := contextAnchor.provider
		if provider == nil {
			provider = l.Provider
		}

		stream, err := provider.Stream(ctx, req)
		if err != nil {
			// Classify before deciding the recovery path. Mirrors
			// hermes' error_classifier — the loop now picks the
			// right strategy per error class instead of guessing
			// "always retry once" or "always bail".
			class := ClassifyError(err)
			switch class.Recovery() {
			case RecoveryCompactRetry:
				if l.tryRecoverOverflow(ctx, err, out) {
					req, contextAnchor = l.buildRequestForRetryWithContext(specs, requestWithoutTools)
					provider = contextAnchor.provider
					if provider == nil {
						provider = l.Provider
					}
					stream, err = provider.Stream(ctx, req)
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
					stream, err = provider.Stream(ctx, req)
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
		l.storeActiveContextSnapshotLocked(usage, contextAnchor)
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
			// Auto-distillation: every N turns, extract durable facts from
			// the most recent user/assistant exchange and write to archival
			// memory. The registered background job does not block turn
			// completion, but session deletion/shutdown can cancel and join it.
			completed, err := l.recordCompletedTurn(ctx)
			if err != nil {
				emit(ctx, out, Event{Kind: EventInfo, Info: "memory recall persistence failed: " + err.Error()})
			}
			l.maybeDistillSnapshot(completed)
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
					traceCallID := NewTraceInvocationID()
					emit(ctx, out, Event{
						Kind: EventToolStart, ToolUseID: toolUse.ToolUseID,
						ToolName: toolUse.ToolName, ToolInput: toolUse.ToolInput,
						TraceCallID: traceCallID,
					})
					skipped[i] = llm.ContentBlock{
						Type: "tool_result", ToolUseID: toolUse.ToolUseID,
						ToolResult: reason, IsError: true,
					}
					emit(ctx, out, Event{
						Kind: EventToolResult, ToolUseID: toolUse.ToolUseID, ToolName: toolUse.ToolName,
						ToolResult:  &ToolResult{Output: reason, IsError: true},
						TraceCallID: traceCallID,
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
					traceCallID := NewTraceInvocationID()
					emit(ctx, out, Event{
						Kind: EventToolStart, ToolUseID: toolUse.ToolUseID,
						ToolName: toolUse.ToolName, ToolInput: toolUse.ToolInput,
						TraceCallID: traceCallID,
					})
					results[i] = llm.ContentBlock{
						Type: "tool_result", ToolUseID: toolUse.ToolUseID,
						ToolResult: reason, IsError: true,
					}
					emit(ctx, out, Event{
						Kind: EventToolResult, ToolUseID: toolUse.ToolUseID, ToolName: toolUse.ToolName,
						ToolResult:  &ToolResult{Output: reason, IsError: true},
						TraceCallID: traceCallID,
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
						traceCallID := NewTraceInvocationID()
						emit(ctx, out, Event{
							Kind: EventToolStart, ToolUseID: toolUse.ToolUseID,
							ToolName: toolUse.ToolName, ToolInput: toolUse.ToolInput,
							TraceCallID: traceCallID,
						})
						results[i] = llm.ContentBlock{
							Type: "tool_result", ToolUseID: toolUse.ToolUseID,
							ToolResult: reason, IsError: true,
						}
						emit(ctx, out, Event{
							Kind: EventToolResult, ToolUseID: toolUse.ToolUseID, ToolName: toolUse.ToolName,
							ToolResult:  &ToolResult{Output: reason, IsError: true},
							TraceCallID: traceCallID,
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

		// Repeat-tool reminder (DSH repeat-tool-reminder parity): count
		// runs of consecutive identical calls and inject an advisory
		// reminder next to this batch's tool_results when a threshold is
		// crossed. Advisory only — never vetoes or rewrites a call.
		if l.RepeatGuard == nil {
			l.RepeatGuard = NewRepeatGuard(RepeatGuardConfig{})
		}
		if reminder := l.RepeatGuard.RecordStep(toolUses); reminder != "" {
			results = append(results, llm.ContentBlock{Type: "text", Text: reminder, Synthetic: true})
			emit(ctx, out, Event{
				Kind: EventInfo,
				Info: "repeat-tool-reminder: model is repeating an identical tool call",
			})
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

		// Budget check with second wind + grace call + final-summary rescue.
		// Order matters:
		//   1. Second wind — force one compaction and reset the budget so a
		//      long-but-progressing task keeps going instead of hard-stopping
		//      mid-work (DSH loops while(true); codex/claude-code compact and
		//      continue rather than stop on an iteration count).
		//   2. Grace call (one extra iter for "I'm almost done").
		//   3. Final-summary rescue (one tool-less iter that forces text).
		//   4. Real abort.
		if runIter >= l.MaxIters {
			if !compactedAtCap && l.compactorAvailable(false) {
				compactedAtCap = true
				if l.compactForSecondWind(ctx, out) {
					runIter = 0
					emit(ctx, out, Event{
						Kind: EventInfo,
						Info: fmt.Sprintf("(iteration budget reached at %d — context compacted, continuing this turn with a fresh budget)", l.MaxIters),
					})
					continue
				}
				// Compaction unavailable/failed — fall through to the
				// bounded grace → rescue → stop path.
			}
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

// compactForSecondWind force-compacts the conversation history once when
// the iteration budget is exhausted, so a long-but-progressing turn gets a
// fresh budget instead of hard-stopping mid-work (claude-code's token-budget
// compaction / DSH's unbounded while(true) both prefer continue-over-stop).
// Returns true when the history actually shrank and the caller should reset
// its run counter and keep looping. False = compactor unavailable, the
// circuit breaker tripped, or the history was already too small to shrink —
// in which case the caller falls through to the bounded grace→rescue→stop
// path. CompactNow keeps provider work, hooks and persistence outside l.mu.
func (l *Loop) compactForSecondWind(ctx context.Context, out chan<- Event) bool {
	if !l.compactorAvailable(true) {
		return false
	}
	sessionGeneration := l.autoCompactGenerationSnapshot()
	result, err := l.CompactNow(ctx, CompactOptions{
		Trigger: "second-wind",
		Force:   true,
		Emit: func(ev Event) {
			emit(ctx, out, ev)
		},
	})
	if err == nil && result.Applied {
		pressureTokens := result.BeforeTokens + int(l.requestOverheadTokens.Load())
		l.noteAutoCompactPressure(result, pressureTokens, sessionGeneration)
	}
	return err == nil && result.Applied
}

// buildRequest assembles the per-iteration LLM Request under l.mu so the
// snapshot of Messages and System+Memory composition is consistent.
//
// SystemSections wiring: when l.SystemSections is populated (the new path
// from runtime.AssembleSystemPromptSections), the compact Core/topic index is
// a stable cacheable section. Query-specific Top-K recall is attached to the
// latest user message before the provider loop, preserving the system-prefix
// cache while keeping recalled bodies close to the query that selected them.
func (l *Loop) buildRequest(specs []llm.ToolSpec) llm.Request {
	req, _ := l.buildRequestWithContext(specs)
	return req
}

// buildRequestWithContext couples the request with the exact history,
// provider and header identity that produced it. Run retains the anchor until
// the response is appended so a concurrent session/model replacement cannot
// publish stale provider usage as the new active-context value.
func (l *Loop) buildRequestWithContext(specs []llm.ToolSpec) (llm.Request, contextRequestAnchor) {
	l.mu.Lock()
	defer l.mu.Unlock()
	system := l.System
	var sections []llm.SystemSection
	memBody := l.turnMemoryContext
	retrieveBody := l.turnMemoryRecall
	if l.turnMemoryRecallAttached {
		retrieveBody = ""
	}
	if !l.turnMemoryPrepared && l.Memory != nil {
		memBody = l.Memory.BuildContext()
	}
	// Per-turn auto retrieval: BM25 the latest user message against archival
	// and topic memory. Runtime defaults to Top-5; AutoRetrieveK == 0 disables
	// it. This branch is only a side-effect-free compatibility fallback for
	// direct buildRequest callers because normal Run calls prepareTurnMemory.
	//
	// AutoRetrieveRerank, when on, fetches BM25 top K*3 candidates and
	// asks the provider to LLM-rerank them down to the final K — more
	// accurate than raw BM25 score, costs one Complete() call per turn.
	// Falls back to BM25 ordering on any failure.
	if !l.turnMemoryPrepared && l.Memory != nil && l.AutoRetrieveK > 0 {
		query := lastUserTextLocked(l.Messages)
		if l.AutoRetrieveRerank && l.Provider != nil {
			candidates := l.Memory.AutoRetrieveCandidates(query, l.AutoRetrieveK*3)
			// Compatibility path for embedders that call buildRequest directly
			// without Run/prepareTurnMemory. Live turns always use the cancellable
			// precomputed path above and never rerank while l.mu is held.
			picked := capPassages(candidates, l.AutoRetrieveK)
			retrieveBody = memory.FormatRetrieveSection(picked)
		} else {
			retrieveBody = l.Memory.PreviewAutoRetrieve(query, l.AutoRetrieveK)
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
		sections = make([]llm.SystemSection, 0, len(l.SystemSections)+4)
		for _, section := range l.SystemSections {
			if section.Name != "plan_mode" {
				sections = append(sections, section)
			}
		}
		if memBody != "" {
			sections = append(sections, llm.SystemSection{
				Name: "memory_index", Body: memBody, Cache: true,
			})
		}
		if l.planMode && planPrompt != "" {
			sections = append(sections, llm.SystemSection{
				Name: "plan_mode", Body: planPrompt, Cache: false, Volatile: true,
			})
		}
		// Runtime state changes far less often than memory/retrieval output.
		// Place its byte-stable snapshot before those volatile tails so an
		// unchanged permission/cwd/plan prefix remains reusable.
		if state, ok := l.currentRuntimeStateSectionLocked(); ok {
			sections = append(sections, llm.SystemSection{
				Name: "runtime_state", Body: state.body, Cache: true,
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
	} else {
		// Legacy (string-only) path keeps the same semantic snapshot, but
		// cannot express per-section cache policy on the wire.
		if state, ok := l.currentRuntimeStateSectionLocked(); ok {
			if system != "" {
				system += "\n\n"
			}
			system += state.body
		}
		if memBody != "" {
			system = system + "\n\n" + memBody
		}
		if retrieveBody != "" {
			system = system + "\n\n" + retrieveBody
		}
	}
	if l.CurrentStateSnapshot == nil && l.CurrentStateSections != nil {
		dynamic := l.CurrentStateSections()
		if len(dynamic) > 0 {
			if len(sections) > 0 {
				for _, section := range dynamic {
					section.Cache = false
					section.Volatile = true
					sections = append(sections, section)
				}
			} else {
				for _, section := range dynamic {
					if strings.TrimSpace(section.Body) == "" {
						continue
					}
					if system != "" {
						system += "\n\n"
					}
					system += section.Body
				}
			}
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
	return req, l.contextRequestAnchorLocked(l.Provider, req)
}

func (l *Loop) rescueNoToolsSnapshot() bool {
	if l == nil {
		return false
	}
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.rescueNoTools
}

// buildRequestForRetry preserves the posture of the request that overflowed.
// In particular, final-summary rescue is intentionally text-only; buildRequest
// consumes its one-shot flag on the first request, so a compact-and-retry must
// keep using an empty schema list explicitly.
func (l *Loop) buildRequestForRetry(specs []llm.ToolSpec, withoutTools bool) llm.Request {
	req, _ := l.buildRequestForRetryWithContext(specs, withoutTools)
	return req
}

func (l *Loop) buildRequestForRetryWithContext(specs []llm.ToolSpec, withoutTools bool) (llm.Request, contextRequestAnchor) {
	if withoutTools {
		specs = nil
	}
	return l.buildRequestWithContext(specs)
}

// maybeDistill runs auto-distillation on the most recent user/asst exchange
// when the turn counter hits the configured cadence. Distillation remains
// asynchronous, but it is not fire-and-forget: launchDistillation registers a
// cancellable job before this method returns, allowing session deletion and
// process shutdown to join the writer before removing session-owned memory.
//
// Every mutable input is frozen before the goroutine starts. In particular,
// switching Desktop sessions or rebinding a provider cannot make a completed
// exchange from session A use session B's repository, provider or provenance.
func (l *Loop) maybeDistill() {
	if l == nil {
		return
	}
	l.mu.RLock()
	snapshot := distillSnapshot{
		repository: l.Memory,
		provider:   l.Provider,
		turn:       transcript.CountTurns(l.Messages),
	}
	snapshot.userMsg, snapshot.assistantMsg = lastExchangeLocked(l.Messages)
	sessionGeneration := l.autoCompactSessionGeneration
	l.mu.RUnlock()
	if snapshot.repository == nil || snapshot.provider == nil || snapshot.turn == 0 ||
		strings.TrimSpace(snapshot.userMsg) == "" || strings.TrimSpace(snapshot.assistantMsg) == "" {
		return
	}
	if l.CurrentStateSnapshot != nil {
		snapshot.sessionID = strings.TrimSpace(l.CurrentStateSnapshot().SessionID)
	}
	l.mu.RLock()
	stillCurrentSession := l.autoCompactSessionGeneration == sessionGeneration
	l.mu.RUnlock()
	if !stillCurrentSession {
		// ResetSession won while the runtime-state callback was resolving. The
		// exchange belongs to the prior generation; do not attribute it to the
		// newly active session.
		return
	}
	if snapshot.sessionID != "" {
		snapshot.sourceMessageID = snapshot.sessionID + "/message/" + uuid.NewString()
	}
	l.maybeDistillSnapshot(&snapshot)
}

// maybeDistillSnapshot launches one completed exchange when it lands on the
// configured cadence. A nil snapshot is a no-op. Zero is an explicit disable;
// NewLoop installs DefaultDistillEvery for normal runtime construction.
func (l *Loop) maybeDistillSnapshot(snapshot *distillSnapshot) {
	if l == nil || snapshot == nil {
		return
	}
	l.mu.RLock()
	cadence := l.DistillEvery
	l.mu.RUnlock()
	if cadence <= 0 || snapshot.turn == 0 || snapshot.turn%cadence != 0 {
		return
	}
	if snapshot.sourceMessageID != "" {
		l.queuePendingDistillation(*snapshot)
		// Cadence is a checkpoint for every successful exchange since the prior
		// checkpoint, not merely the Nth exchange. This keeps a long-running
		// session's residual boundary bounded to at most cadence-1 snapshots.
		l.FlushPendingDistillation(snapshot.sessionID)
		return
	}
	l.launchDistillation(*snapshot)
}

func distillationSourceKey(sessionID, sourceMessageID string) string {
	sessionID = strings.TrimSpace(sessionID)
	sourceMessageID = strings.TrimSpace(sourceMessageID)
	if sessionID == "" || sourceMessageID == "" {
		return ""
	}
	return sessionID + "\x00" + sourceMessageID
}

func (l *Loop) queuePendingDistillation(snapshot distillSnapshot) bool {
	if l == nil || snapshot.repository == nil || snapshot.provider == nil ||
		strings.TrimSpace(snapshot.userMsg) == "" || strings.TrimSpace(snapshot.assistantMsg) == "" {
		return false
	}
	key := distillationSourceKey(snapshot.sessionID, snapshot.sourceMessageID)
	if key == "" {
		return false
	}
	l.distillMu.Lock()
	defer l.distillMu.Unlock()
	if _, ok := l.distillWatermark[key]; ok {
		return false
	}
	if _, ok := l.distillInFlight[key]; ok {
		return false
	}
	if l.distillPending == nil {
		l.distillPending = make(map[string]distillSnapshot)
	}
	if _, ok := l.distillPending[key]; ok {
		return false
	}
	l.distillPending[key] = snapshot
	return true
}

// FlushPendingDistillation registers every successful exchange for sessionID
// that has not yet reached the normal cadence. Registration is synchronous;
// provider calls remain cancellable background jobs. A durability boundary
// invokes this method and then WaitForDistillation before replacing history or
// closing provider/repository dependencies. Repeated calls are idempotent by
// the session/source-message watermark. It returns the number of new jobs.
func (l *Loop) FlushPendingDistillation(sessionID string) int {
	if l == nil {
		return 0
	}
	l.mu.RLock()
	disabled := l.DistillEvery <= 0
	l.mu.RUnlock()
	if disabled {
		return 0
	}
	sessionID = strings.TrimSpace(sessionID)
	l.distillMu.Lock()
	pending := make([]distillSnapshot, 0, len(l.distillPending))
	for key, snapshot := range l.distillPending {
		if sessionID != "" && strings.TrimSpace(snapshot.sessionID) != sessionID {
			continue
		}
		if _, done := l.distillWatermark[key]; done {
			continue
		}
		if _, running := l.distillInFlight[key]; running {
			continue
		}
		pending = append(pending, snapshot)
	}
	l.distillMu.Unlock()
	sort.SliceStable(pending, func(i, j int) bool {
		if pending[i].turn != pending[j].turn {
			return pending[i].turn < pending[j].turn
		}
		return pending[i].sourceMessageID < pending[j].sourceMessageID
	})

	launched := 0
	for _, snapshot := range pending {
		if l.launchDistillation(snapshot) {
			launched++
		}
	}
	return launched
}

// launchDistillation registers the job synchronously, then performs the
// provider call in the background. Register-before-launch closes the boundary
// race where a DELETE arriving immediately after Run returned could observe no
// writer and remove the session while a goroutine was about to start.
func (l *Loop) launchDistillation(snapshot distillSnapshot) bool {
	if l == nil || snapshot.repository == nil || snapshot.provider == nil ||
		strings.TrimSpace(snapshot.userMsg) == "" || strings.TrimSpace(snapshot.assistantMsg) == "" {
		return false
	}
	sourceKey := distillationSourceKey(snapshot.sessionID, snapshot.sourceMessageID)
	ctx, cancel := context.WithCancel(context.Background())
	job := &distillJob{
		sessionID: strings.TrimSpace(snapshot.sessionID),
		cancel:    cancel,
		done:      make(chan struct{}),
	}

	l.distillMu.Lock()
	if sourceKey != "" {
		if _, done := l.distillWatermark[sourceKey]; done {
			l.distillMu.Unlock()
			cancel()
			return false
		}
		if _, running := l.distillInFlight[sourceKey]; running {
			l.distillMu.Unlock()
			cancel()
			return false
		}
		if l.distillInFlight == nil {
			l.distillInFlight = make(map[string]struct{})
		}
		// A retry supersedes the prior attempt's error. If it fails again the
		// goroutine stores the fresh error for the boundary waiter.
		delete(l.distillFailures, sourceKey)
		l.distillInFlight[sourceKey] = struct{}{}
	}
	if l.distillJobs == nil {
		l.distillJobs = make(map[uint64]*distillJob)
	}
	if l.distillSlots == nil {
		l.distillSlots = make(chan struct{}, maxConcurrentDistillations)
	}
	slots := l.distillSlots
	l.distillNextID++
	jobID := l.distillNextID
	l.distillJobs[jobID] = job
	l.distillMu.Unlock()

	go func() {
		var err error
		defer func() {
			cancel()
			l.distillMu.Lock()
			if sourceKey != "" {
				delete(l.distillInFlight, sourceKey)
				if err == nil {
					if l.distillWatermark == nil {
						l.distillWatermark = make(map[string]struct{})
					}
					l.distillWatermark[sourceKey] = struct{}{}
					delete(l.distillPending, sourceKey)
					delete(l.distillFailures, sourceKey)
				} else if !errors.Is(err, context.Canceled) {
					if l.distillFailures == nil {
						l.distillFailures = make(map[string]error)
					}
					l.distillFailures[sourceKey] = err
				}
			}
			delete(l.distillJobs, jobID)
			close(job.done)
			l.distillMu.Unlock()
		}()
		select {
		case slots <- struct{}{}:
			defer func() { <-slots }()
		case <-ctx.Done():
			err = ctx.Err()
			return
		}
		providerCtx, timeout := context.WithTimeout(ctx, 30*time.Second)
		defer timeout()
		err = snapshot.repository.DistillTurnWithMetadata(
			providerCtx,
			snapshot.provider,
			snapshot.sessionID,
			snapshot.sourceMessageID,
			snapshot.userMsg,
			snapshot.assistantMsg,
		)
	}()
	return true
}

// CancelDistillation requests cancellation for all in-flight distillation jobs
// attributed to sessionID. An empty sessionID means every job (shutdown). This
// method is non-blocking; destructive callers must follow it with
// WaitForDistillation or use CancelAndWaitForDistillation so providers that
// ignore context cannot write after cleanup.
func (l *Loop) CancelDistillation(sessionID string) {
	if l == nil {
		return
	}
	sessionID = strings.TrimSpace(sessionID)
	l.distillMu.Lock()
	cancels := make([]context.CancelFunc, 0, len(l.distillJobs))
	for _, job := range l.distillJobs {
		if sessionID == "" || job.sessionID == sessionID {
			cancels = append(cancels, job.cancel)
		}
	}
	l.distillMu.Unlock()
	for _, cancel := range cancels {
		cancel()
	}
}

// WaitForDistillation waits until every currently registered job for
// sessionID exits. An empty sessionID waits for all jobs. It loops after each
// snapshot so a job registered concurrently at the boundary is not missed;
// callers are still expected to stop foreground turns before invoking it.
func (l *Loop) WaitForDistillation(ctx context.Context, sessionID string) error {
	if l == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	sessionID = strings.TrimSpace(sessionID)
	for {
		l.distillMu.Lock()
		done := make([]<-chan struct{}, 0, len(l.distillJobs))
		for _, job := range l.distillJobs {
			if sessionID == "" || job.sessionID == sessionID {
				done = append(done, job.done)
			}
		}
		if len(done) == 0 {
			var failures []error
			for key, err := range l.distillFailures {
				keySession, _, _ := strings.Cut(key, "\x00")
				if sessionID == "" || keySession == sessionID {
					failures = append(failures, err)
					delete(l.distillFailures, key)
				}
			}
			l.distillMu.Unlock()
			return errors.Join(failures...)
		}
		l.distillMu.Unlock()
		for _, jobDone := range done {
			select {
			case <-jobDone:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}
}

// discardDistillationState removes frozen conversation content and completed
// source-message watermarks for one session after a destructive boundary has
// joined all of its jobs. An empty sessionID discards every session.
func (l *Loop) discardDistillationState(sessionID string) {
	if l == nil {
		return
	}
	sessionID = strings.TrimSpace(sessionID)
	l.distillMu.Lock()
	defer l.distillMu.Unlock()
	for key, snapshot := range l.distillPending {
		if sessionID == "" || strings.TrimSpace(snapshot.sessionID) == sessionID {
			delete(l.distillPending, key)
			delete(l.distillInFlight, key)
		}
	}
	for key := range l.distillWatermark {
		keySession, _, _ := strings.Cut(key, "\x00")
		if sessionID == "" || keySession == sessionID {
			delete(l.distillWatermark, key)
			delete(l.distillInFlight, key)
		}
	}
	for key := range l.distillFailures {
		keySession, _, _ := strings.Cut(key, "\x00")
		if sessionID == "" || keySession == sessionID {
			delete(l.distillFailures, key)
		}
	}
}

// CancelAndWaitForDistillation is the destructive-boundary helper used by
// session deletion and shutdown: request cancellation, then join even a
// stubborn provider before the repository is swept. Once joined, it also
// drops frozen pending content so a later catch-all flush cannot send deleted
// conversation text to the provider.
func (l *Loop) CancelAndWaitForDistillation(ctx context.Context, sessionID string) error {
	if l == nil {
		return nil
	}
	l.CancelDistillation(sessionID)
	waitErr := l.WaitForDistillation(ctx, sessionID)
	// A live wait timeout means a provider may still hold the frozen snapshot;
	// retain state and force the destructive caller to fail closed. A completed
	// provider error, by contrast, must not make a user's delete impossible:
	// discard the pending snapshot and its error once every job has exited.
	if ctx != nil && ctx.Err() != nil {
		return waitErr
	}
	l.discardDistillationState(sessionID)
	return nil
}

// recordCompletedTurn durably connects successful Loop turns to RecallMemory.
// It intentionally runs only on the clean final-answer path: cancelled,
// provider-error, loop-detected and max-iteration exits can contain partial or
// repaired assistant output that should not become remembered conversation.
func (l *Loop) recordCompletedTurn(ctx context.Context) (*distillSnapshot, error) {
	if l == nil {
		return nil, nil
	}
	l.mu.RLock()
	snapshot := &distillSnapshot{
		repository: l.Memory,
		provider:   l.Provider,
		turn:       transcript.CountTurns(l.Messages),
	}
	snapshot.userMsg, snapshot.assistantMsg = lastExchangeLocked(l.Messages)
	sessionGeneration := l.autoCompactSessionGeneration
	l.mu.RUnlock()
	if snapshot.repository == nil || strings.TrimSpace(snapshot.userMsg) == "" || strings.TrimSpace(snapshot.assistantMsg) == "" {
		return nil, nil
	}
	if l.CurrentStateSnapshot != nil {
		snapshot.sessionID = strings.TrimSpace(l.CurrentStateSnapshot().SessionID)
	}
	l.mu.RLock()
	stillCurrentSession := l.autoCompactSessionGeneration == sessionGeneration
	l.mu.RUnlock()
	if !stillCurrentSession {
		return nil, nil
	}
	if snapshot.sessionID != "" {
		// A UUID remains stable in the frozen snapshot and unique after transcript
		// compaction, /undo, session resume, or process restart. CountTurns is UI
		// state, not a durable identity and can move backwards at those boundaries.
		snapshot.sourceMessageID = snapshot.sessionID + "/message/" + uuid.NewString()
	}
	if err := snapshot.repository.RecordTurn(
		ctx,
		snapshot.sessionID,
		snapshot.sourceMessageID,
		snapshot.userMsg,
		snapshot.assistantMsg,
	); err != nil {
		return nil, err
	}
	l.mu.RLock()
	distillationEnabled := l.DistillEvery > 0
	l.mu.RUnlock()
	if distillationEnabled {
		l.queuePendingDistillation(*snapshot)
	}
	return snapshot, nil
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
	return lastExchangeLocked(l.Messages)
}

func lastExchangeLocked(messages []llm.Message) (userMsg, asstMsg string) {
	for i := len(messages) - 1; i >= 0; i-- {
		m := messages[i]
		if m.Role == llm.RoleAssistant && asstMsg == "" {
			asstMsg = textOf(m)
			continue
		}
		if m.Role == llm.RoleUser && asstMsg != "" {
			userMsg = visibleTextOf(m)
			if userMsg == "" {
				continue
			}
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
		t := visibleTextOf(m)
		if t == "" {
			continue
		}
		return t
	}
	return ""
}

// visibleTextOf returns only person-authored text. Runtime attachments such
// as <auto-retrieve> are persisted in user-role history for provider-prefix
// caching, but must never recursively become the next retrieval query or a
// newly distilled memory.
func visibleTextOf(m llm.Message) string {
	var parts []string
	for _, b := range m.Content {
		if b.Type != "text" || b.Text == "" || b.Synthetic {
			continue
		}
		if text := transcript.VisibleUserText(b.Text); text != "" {
			parts = append(parts, text)
		}
	}
	return strings.Join(parts, "\n")
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
	// Stamp sub-agent events at the source: the trace hook must see the
	// parent's tool_use_id on the child's OWN events (the forwarded copy
	// alone bypassed the hook, so the recorded spawn-tree never linked).
	if ev.SubAgentParentID == "" {
		ev.SubAgentParentID = ParentToolUseIDFromContext(ctx)
	}
	// Internal trace ownership is independent of the provider tool_use_id.
	// Child loops inherit this process-unique value through context, so their
	// late tokens/text cannot be reassigned when Desktop switches sessions or
	// when another provider response reuses the same public ID.
	if ev.TraceInvocationID == "" {
		ev.TraceInvocationID = TraceInvocationIDFromContext(ctx)
	}
	if ch == nil {
		return
	}
	if ctx == nil {
		ch <- ev
		return
	}
	// Usage is durable accounting, not an expendable rendering delta. If the
	// provider completed just as Stop canceled the turn, both a writable send
	// and ctx.Done can be ready; a single select would choose randomly and can
	// make Desktop miss the usage that the trace hook still records. Prefer an
	// immediately writable consumer without ever delaying cancellation.
	if ev.Kind == EventTokens && tryEmitEvent(ch, ev) {
		notifyTraceHook(ev)
		return
	}
	select {
	case ch <- ev:
	case <-ctx.Done():
		// The consumer may have freed capacity concurrently with cancellation.
		// Make one final best-effort delivery for usage only; a full or absent
		// receiver must never hold up cancellation.
		if ev.Kind == EventTokens {
			tryEmitEvent(ch, ev)
		}
	}
	notifyTraceHook(ev)
}

func tryEmitEvent(ch chan<- Event, ev Event) bool {
	select {
	case ch <- ev:
		return true
	default:
		return false
	}
}
