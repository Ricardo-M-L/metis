package agent

import (
	"context"
	"fmt"
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

	// Tier 1 — Snip: cheap tool-result truncation, no LLM call. Runs
	// at a lower threshold than full compaction so a tool-heavy
	// session gets shrunk WITHOUT paying the summarizer's API cost.
	// We only emit an event if Snip actually shortened anything (the
	// estimator size dropped) — otherwise it's invisible bookkeeping
	// and not worth a chat-line interruption.
	if l.Compactor.ShouldSnip(l.Messages) {
		beforeBytes := estimateTokens(l.Messages)
		snipped := l.Compactor.Snip(l.Messages)
		afterBytes := estimateTokens(snipped)
		if afterBytes < beforeBytes {
			l.Messages = snipped
			emit(ctx, out, Event{
				Kind: EventInfo,
				Info: fmt.Sprintf("context snipped: ~%d → ~%d tokens", beforeBytes, afterBytes),
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
			beforeBytes := estimateTokens(l.Messages)
			offloaded := l.Compactor.Microcompact(l.Messages)
			afterBytes := estimateTokens(offloaded)
			if afterBytes < beforeBytes {
				l.Messages = offloaded
				emit(ctx, out, Event{
					Kind: EventInfo,
					Info: fmt.Sprintf("context microcompacted (time-based, idle %.0fs): ~%d → ~%d tokens", idle.Seconds(), beforeBytes, afterBytes),
				})
			}
			l.lastTimeBasedMicrocompactAt = time.Now()
		}
	}

	// Tier 2 — Microcompact: off-load still-large tool_result blocks
	// to disk. Lossless from the model's POV — it can Read the cached
	// path to recover. Runs in same threshold window as Snip.
	if l.Compactor.ShouldMicrocompact(l.Messages) {
		beforeBytes := estimateTokens(l.Messages)
		offloaded := l.Compactor.Microcompact(l.Messages)
		afterBytes := estimateTokens(offloaded)
		if afterBytes < beforeBytes {
			l.Messages = offloaded
			emit(ctx, out, Event{
				Kind: EventInfo,
				Info: fmt.Sprintf("context microcompacted: ~%d → ~%d tokens (cached to disk, recoverable via Read)", beforeBytes, afterBytes),
			})
			// Threshold-driven Microcompact also resets the time-based
			// window — no need to fire again immediately.
			l.lastTimeBasedMicrocompactAt = time.Now()
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
		collapsed, err := l.Compactor.CollapseMiddle(ctx, l.Messages)
		if err != nil {
			return // skip Compact attempt — see comment above
		}
		if len(collapsed) < before {
			l.Messages = collapsed
			emit(ctx, out, Event{
				Kind: EventInfo,
				Info: fmt.Sprintf("context collapsed (early middle): %d → %d messages", before, len(collapsed)),
			})
		}
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
	compacted, err := l.Compactor.Compact(ctx, l.Messages)
	if err != nil || len(compacted) >= before {
		return
	}
	l.Messages = compacted
	l.compactCircuitNoticeSent = false // success → re-arm the notice
	emit(ctx, out, Event{
		Kind: EventInfo,
		Info: fmt.Sprintf("context compacted: %d → %d messages", before, len(compacted)),
	})

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
