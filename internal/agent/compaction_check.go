package agent

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/Ricardo-M-L/metis/internal/llm"
)

// maybeCompact runs auto-compaction on the loop's history when the
// configured threshold is exceeded. No-op when l.Compactor is nil
// (compaction disabled in this build / for this session).
//
// Lock window matches the original Run() inline block exactly:
// l.mu held across ShouldCompact + Compact + Messages assignment, so
// concurrent History() / Reset() calls can't see a torn intermediate
// state. Compactor.Compact is allowed to be expensive (it makes an
// LLM call internally); holding mu through that is intentional —
// any caller hitting RLock during compaction simply waits.
func (l *Loop) maybeCompact(ctx context.Context, out chan<- Event) {
	if l.Compactor == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()

	// A compaction attempt and an applied context reduction are different
	// lifecycle facts. Claude Code emits compact_end from a finally block to
	// stop progress UI, while callers only replace their message list after a
	// valid result is returned. Keep the same split here:
	//
	//   EventCompactionEnd      — the attempt stopped (success or failure)
	//   EventContextCompacted   — smaller history was installed successfully
	//
	// Cache the post-reduction estimate before emitting the success event. The
	// TUI consumes events while this function still owns l.mu, so its
	// non-blocking EstimateContextTokens call must be able to read the new
	// value instead of the pre-compact cache.
	emitContextReduced := func(tier string, beforeTokens, afterTokens int) {
		l.estTokens.Store(int64(afterTokens))
		emit(ctx, out, Event{
			Kind:                  EventContextCompacted,
			Info:                  tier,
			PreviousContextTokens: beforeTokens,
			ContextTokens:         afterTokens,
		})
	}
	endCompactionAttempt := func(tier string, err error) {
		emit(ctx, out, Event{Kind: EventCompactionEnd, Info: tier, Err: err})
	}

	// Tier 0 — Image prune: drop image content blocks past the most
	// recent keepN (1/2/3 depending on the model's context window).
	// Cheap, no LLM, no threshold gate — runs every iteration because:
	//   * inline image payloads inflate context faster than anything
	//     else (cu screenshot ~130KB JPEG ≈ 90-100k REAL server tokens —
	//     the local estimator under-counted these until 2026-05-27)
	//   * keeping only the most recent N is the same strategy
	//     Anthropic's computer-use-demo and claude-code both ship
	//     (see compact_images.go docstring)
	//   * pruning's a no-op when image count ≤ keepN so non-cu
	//     workloads pay zero cost
	// keepN scales with maxCtx — Kimi-K2.6 (262k) gets keep=1 or it
	// blows the cap on the 3rd screenshot; Claude/MiniMax (1M+) get
	// keep=3 for richer state-machine reasoning.
	keepN := keepRecentImagesFor(l.Compactor.MaxContextTokens)
	if keepN > 0 {
		beforeTokens := estimateTokens(l.Messages)
		pruned, n := PruneOldImages(l.Messages, keepN)
		// METIS_DEBUG_IMG_PRUNE=1 surfaces the per-iteration prune
		// telemetry to stderr. Useful when investigating "why
		// didn't my model session prune" — left in as an opt-in
		// (zero perf cost when unset) because diagnosing
		// MCP-tool-result image extraction is annoying without it.
		if os.Getenv("METIS_DEBUG_IMG_PRUNE") != "" {
			imgCount := 0
			for _, m := range l.Messages {
				for _, c := range m.Content {
					if c.Type == "image" {
						imgCount++
					}
					for _, sub := range c.ToolResultBlocks {
						if sub.Type == "image" {
							imgCount++
						}
					}
				}
			}
			fmt.Fprintf(os.Stderr, "[img-prune] maxCtx=%d keepN=%d totalImages=%d pruned=%d beforeBytes=%d\n",
				l.Compactor.MaxContextTokens, keepN, imgCount, n, beforeTokens)
		}
		if n > 0 {
			l.Messages = pruned
			afterTokens := estimateTokens(pruned)
			emitContextReduced("image-prune", beforeTokens, afterTokens)
			emit(ctx, out, Event{
				Kind: EventInfo,
				Info: fmt.Sprintf("image-pruned %d older screenshots (kept %d most recent): ~%d → ~%d tokens",
					n, keepN, beforeTokens, afterTokens),
			})
		}
	}

	// Tier 1 — Snip: cheap tool-result truncation, no LLM call. Runs
	// at a lower threshold than full compaction so a tool-heavy
	// session gets shrunk WITHOUT paying the summarizer's API cost.
	// We only emit an event if Snip actually shortened anything (the
	// estimator size dropped) — otherwise it's invisible bookkeeping
	// and not worth a chat-line interruption.
	if l.Compactor.ShouldSnip(l.Messages) {
		beforeTokens := estimateTokens(l.Messages)
		snipped := l.Compactor.Snip(l.Messages)
		afterTokens := estimateTokens(snipped)
		if afterTokens < beforeTokens {
			l.Messages = snipped
			emitContextReduced("snip", beforeTokens, afterTokens)
			emit(ctx, out, Event{
				Kind: EventInfo,
				Info: fmt.Sprintf("context snipped: ~%d → ~%d tokens", beforeTokens, afterTokens),
			})
		}
	}
	// Tier 1.5 — Time-based Microcompact: when more than IdleMaxSeconds
	// have passed since the previous Microcompact pass, force a fresh
	// one even if SnipThreshold isn't crossed. Models the boundary at
	// which the provider's prompt cache has probably aged out the older
	// tool_result blocks (CC's services/compact/timeBasedMC uses 60min
	// for the same reason). Without this, a long-idle conversation that
	// later resumes pays full re-send of multi-MB tool_results that the
	// cache has already evicted. Off when IdleMaxSeconds <= 0.
	if l.Compactor.IdleMaxSeconds > 0 && l.Compactor.MicrocompactDir != "" {
		idle := time.Since(l.lastTimeBasedMicrocompactAt)
		if idle > time.Duration(l.Compactor.IdleMaxSeconds)*time.Second {
			beforeTokens := estimateTokens(l.Messages)
			offloaded := l.Compactor.Microcompact(l.Messages)
			afterTokens := estimateTokens(offloaded)
			if afterTokens < beforeTokens {
				l.Messages = offloaded
				emitContextReduced("microcompact-idle", beforeTokens, afterTokens)
				emit(ctx, out, Event{
					Kind: EventInfo,
					Info: fmt.Sprintf("context microcompacted (time-based, idle %.0fs): ~%d → ~%d tokens", idle.Seconds(), beforeTokens, afterTokens),
				})
			}
			l.lastTimeBasedMicrocompactAt = time.Now()
		}
	}

	// Tier 2 — Microcompact: off-load still-large tool_result blocks
	// to disk. Lossless from the model's POV — it can Read the cached
	// path to recover. Runs in same threshold window as Snip.
	if l.Compactor.ShouldMicrocompact(l.Messages) {
		beforeTokens := estimateTokens(l.Messages)
		offloaded := l.Compactor.Microcompact(l.Messages)
		afterTokens := estimateTokens(offloaded)
		if afterTokens < beforeTokens {
			l.Messages = offloaded
			emitContextReduced("microcompact", beforeTokens, afterTokens)
			emit(ctx, out, Event{
				Kind: EventInfo,
				Info: fmt.Sprintf("context microcompacted: ~%d → ~%d tokens (cached to disk, recoverable via Read)", beforeTokens, afterTokens),
			})
			// Threshold-driven Microcompact also resets the time-based
			// window — no need to fire again immediately.
			l.lastTimeBasedMicrocompactAt = time.Now()
		}
	}

	// PreCompact hook — fires exactly once per maybeCompact pass, right
	// before the first LLM-driven summarization tier (Collapse or
	// Compact, whichever runs first). Observers use it to back up / log
	// the transcript before it's folded. Trigger "auto": threshold
	// crossed. Manual /compact emits its own with "manual".
	//
	// The emit happens with l.mu RELEASED, even though maybeCompact holds
	// it for the rest of the body. A PreCompact handler's natural move is
	// to read the transcript (l.History) to back it up — and History
	// takes l.mu, so emitting under the lock would deadlock the agent on
	// the obvious handler. We snapshot the size first (under the lock),
	// drop the lock for the emit, then reacquire. The transcript is in a
	// consistent between-tier state at this point; the tier that runs
	// next re-reads l.Messages fresh.
	preCompactFired := false
	firePreCompact := func() {
		if preCompactFired || l.Hooks == nil {
			return
		}
		preCompactFired = true
		pc := &PreCompact{
			Trigger:         "auto",
			MessageCount:    len(l.Messages),
			EstimatedTokens: estimateTokens(l.Messages),
		}
		tc := HookContext{Model: l.Model, Turn: l.turnIdx}
		l.mu.Unlock()
		l.Hooks.EmitPreCompact(ctx, tc, pc)
		l.mu.Lock()
	}

	// firePostCompact mirrors firePreCompact's drop-lock emit for the
	// AFTER side of an LLM-driven tier (collapse / compact). Fired only
	// when the tier actually reduced the history — the cheap model-free
	// tiers (snip / microcompact / image-prune) never destroy
	// conversational content wholesale and stay silent. Handlers may
	// return AdditionalContext; it's appended as a user message under
	// the lock so the next request re-anchors on hook-contributed facts
	// (bug §28.11's "compact 后 inject 上下文" half). Appending AFTER
	// emitContextReduced means the token estimate must be refreshed —
	// injected config text is small but not free.
	firePostCompact := func(tier string, beforeMsgs, afterMsgs, beforeToks, afterToks int) {
		if l.Hooks == nil {
			return
		}
		pc := &PostCompact{
			Trigger:        "auto",
			Tier:           tier,
			BeforeMessages: beforeMsgs,
			AfterMessages:  afterMsgs,
			BeforeTokens:   beforeToks,
			AfterTokens:    afterToks,
		}
		tc := HookContext{Model: l.Model, Turn: l.turnIdx}
		l.mu.Unlock()
		extra := l.Hooks.EmitPostCompact(ctx, tc, pc)
		l.mu.Lock()
		if strings.TrimSpace(extra) != "" {
			l.Messages = append(l.Messages, llm.Message{
				Role: llm.RoleUser,
				Content: []llm.ContentBlock{{
					Type: "text",
					Text: "[post-compact hook context] " + extra,
				}},
			})
			l.estTokens.Store(int64(estimateTokens(l.Messages)))
		}
	}

	// Tier 3 — Context-collapse: fold early middle into a summary
	// (lighter than full Compact, heavier than snip). Runs at 0.78
	// vs Compact's 0.85 so a long-thread session gets an early
	// trim before we resort to summarizing most of the convo.
	//
	// If collapse FAILS we skip the full Compact attempt in this
	// iteration: the summarizer is already broken, doubling the
	// breaker burn rate by trying again here just trips it faster
	// for no actual gain. Collapse already counted the failure via
	// recordCompactResult, so the next iteration sees the updated
	// counter and decides accordingly.
	if l.Compactor.ShouldCollapse(l.Messages) {
		before := len(l.Messages)
		beforeTokens := estimateTokens(l.Messages)
		firePreCompact()
		// Tell the UI we're entering an LLM-driven phase so it can swap
		// the spinner label. Without this the TUI shows "Thinking..." for
		// 5-30s while summarize streams, which looks like a hang — see
		// 2026-05-15 user report (screenshot #3, "input area frozen").
		emit(ctx, out, Event{Kind: EventCompactionStart, Info: "collapse"})
		// Stamp the forwarder onto ctx so summarizeOnce can stream
		// EventCompactionProgress (cumulative byte counter) into the
		// same channel for the TUI's "(N tokens streamed)" suffix.
		collapseCtx := WithEventOut(ctx, out)
		collapsed, err := l.Compactor.CollapseMiddle(collapseCtx, l.Messages)
		if err != nil {
			endCompactionAttempt("collapse", err)
			return // skip Compact attempt — see comment above
		}
		if len(collapsed) < before {
			l.Messages = collapsed
			afterTokens := estimateTokens(collapsed)
			emitContextReduced("collapse", beforeTokens, afterTokens)
			emit(ctx, out, Event{
				Kind: EventInfo,
				Info: fmt.Sprintf("context collapsed (early middle): %d → %d messages · ~%d → ~%d tokens",
					before, len(collapsed), beforeTokens, afterTokens),
			})
			// PostCompact hook for the collapse tier — after the final
			// history is installed, before the compact tier's own
			// threshold check runs (so a hook that ALSO shrinks nothing
			// here doesn't see a half-state).
			firePostCompact("collapse", before, len(collapsed), beforeTokens, afterTokens)
		}
		endCompactionAttempt("collapse", nil)
	}
	if !l.Compactor.ShouldCompact(l.Messages) {
		// Surface the tripped breaker exactly once per state change.
		// Without this notice the user just sees the context keep
		// growing past the threshold with no explanation.
		if l.Compactor.CircuitTripped() && !l.compactCircuitNoticeSent {
			l.compactCircuitNoticeSent = true
			emit(ctx, out, Event{
				Kind: EventInfo,
				Info: fmt.Sprintf("auto-compaction disabled after %d failures — /clear to reset", MaxConsecutiveCompactFailures),
			})
		}
		return
	}
	before := len(l.Messages)
	beforeTokens := estimateTokens(l.Messages)
	firePreCompact() // no-op if collapse already fired it this pass
	emit(ctx, out, Event{Kind: EventCompactionStart, Info: "compact"})
	compactCtx := WithEventOut(ctx, out)
	compacted, err := l.Compactor.Compact(compactCtx, l.Messages)
	if err != nil {
		endCompactionAttempt("compact", err)
		return
	}
	if len(compacted) >= before {
		endCompactionAttempt("compact", nil)
		return
	}
	l.Messages = compacted
	l.compactCircuitNoticeSent = false // success → re-arm the notice

	// Post-compact budget enforcement. Compact reduced the message
	// COUNT but the protected tail (last N messages) might still hold
	// a fat tool_result that puts the assembled request over the wire
	// cap. Two-stage guard:
	//   1. Hard cap each tool_result at PostCompactMaxToolResultChars
	//      (5000) — a single 10MB grep dump in the tail can't
	//      single-handedly re-trigger overflow.
	//   2. If the total estimate is still over PostCompactTokenBudget
	//      OR the model's effective input cap, fall through to
	//      SnipAll which uses the tighter turn-time snip cap.
	//
	// Without this, the user gets the "Conversation compacted (39→9)"
	// success line followed instantly by `(2013) request entity too
	// large` — exactly the report image #9 showed. The compaction
	// looked successful but the next request still bounced.
	clamped := l.Compactor.EnforcePostCompactBudget(l.Messages)
	if !sameSlice(clamped, l.Messages) {
		l.Messages = clamped
		emit(ctx, out, Event{
			Kind: EventInfo,
			Info: fmt.Sprintf("post-compact: capped tail tool_results at %d chars each", PostCompactMaxToolResultChars),
		})
	}
	cap := l.Compactor.effectiveInputCap()
	postBytes := estimateTokens(l.Messages)
	overCap := cap > 0 && postBytes >= cap
	overBudget := postBytes >= PostCompactTokenBudget
	if overCap || overBudget {
		snippedAll := l.Compactor.SnipAll(l.Messages)
		afterAll := estimateTokens(snippedAll)
		if afterAll < postBytes {
			l.Messages = snippedAll
			emit(ctx, out, Event{
				Kind: EventInfo,
				Info: fmt.Sprintf("post-compact size guard: ~%d → ~%d tokens (target ≤ %d)", postBytes, afterAll, PostCompactTokenBudget),
			})
		} else if overCap {
			// Couldn't shrink further — surface so the user understands
			// why the next request might still bounce.
			emit(ctx, out, Event{
				Kind: EventInfo,
				Info: fmt.Sprintf("post-compact ~%d tokens still exceeds %d cap; next request may overflow", postBytes, cap),
			})
		}
	}

	// Report success only after the final post-compact history is installed
	// and all size guards have run. The estimate on this event is therefore
	// the context the next model request will actually inherit, not the token
	// usage of the summarization API call itself.
	afterTokens := estimateTokens(l.Messages)
	emitContextReduced("compact", beforeTokens, afterTokens)
	endCompactionAttempt("compact", nil)
	emit(ctx, out, Event{
		Kind: EventInfo,
		Info: fmt.Sprintf("context compacted: %d → %d messages · ~%d → ~%d tokens",
			before, len(l.Messages), beforeTokens, afterTokens),
	})
	// PostCompact hook for the full-compact tier — last, so handlers see
	// the FINAL post-guard history (message count includes any budget
	// clamps above) and any AdditionalContext lands after every guard.
	firePostCompact("compact", before, len(l.Messages), beforeTokens, afterTokens)
}

// sameSlice returns true when two []llm.Message refer to the same
// underlying array (Compactor methods that "no-op" return the input
// slice verbatim — comparing the header pointers tells us whether
// any work happened without paying for a deep equal).
func sameSlice(a, b []llm.Message) bool {
	if len(a) != len(b) {
		return false
	}
	if len(a) == 0 {
		return true
	}
	return &a[0] == &b[0]
}

// tryRecoverOverflow inspects an error returned by Provider.Stream and,
// if it looks like a context-window overflow, force-compacts the history
// and reports true so the caller can retry the request once. When
// compaction is disabled or the error doesn't look like an overflow,
// returns false.
//
// Routing through ClassifyError keeps a single source of truth for
// "what does this provider's overflow error look like". Earlier this
// function used its own substring list ("context", "too many tokens",
// "exceeds limit") which missed MiniMax's user-facing format
// "invalid params, request entity too large (2013)" — the auto-retry
// path silently never fired and the user was stuck after every
// compaction. (User report 2026-05-10 image #9.)
//
// In addition to messages-as-overflow recovery, when Compact already
// produced the minimum protected slice (ProtectFirst + ProtectLast)
// we ALSO try Snip + Microcompact one more time, in case the residual
// payload is tool_results in the protected tail (the "last 5 turns"
// happens to contain a 5MB grep dump). Without this we'd return false
// and the user sees the raw 2013 with no recovery hint.
func (l *Loop) tryRecoverOverflow(ctx context.Context, err error, out chan<- Event) bool {
	if l.Compactor == nil || err == nil {
		return false
	}
	if ClassifyError(err) != ErrContextOverflow {
		return false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	before := len(l.Messages)

	// First-line attempt: aggressive Snip even if the threshold check
	// would normally skip it. The protected tail is included this time
	// because we already KNOW the request bounced — no point in
	// preserving recoverability for a request that didn't go out.
	beforeBytes := estimateTokens(l.Messages)
	snipped := l.Compactor.SnipAll(l.Messages)
	afterBytes := estimateTokens(snipped)
	if afterBytes < beforeBytes {
		l.Messages = snipped
		emit(ctx, out, Event{
			Kind: EventInfo,
			Info: fmt.Sprintf("context overflow: aggressively snipped tool results ~%d → ~%d tokens, retrying", beforeBytes, afterBytes),
		})
		return true
	}

	// Second-line: full summarization compact. Skip if there's not
	// enough material to compact; bubble the error up in that case.
	if before <= l.Compactor.ProtectFirst+l.Compactor.ProtectLast+2 {
		return false
	}
	compacted, cerr := l.Compactor.Compact(ctx, l.Messages)
	if cerr != nil || len(compacted) >= before {
		return false
	}
	l.Messages = compacted
	emit(ctx, out, Event{
		Kind: EventInfo,
		Info: fmt.Sprintf("context overflow: force-compacted %d → %d messages, retrying", before, len(compacted)),
	})
	return true
}
