package agent

import (
	"context"
	"fmt"
	"strings"
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
}

// tryRecoverOverflow inspects an error returned by Provider.Stream and, if
// it looks like a context-window overflow, force-compacts the history and
// reports true so the caller can retry the request once. When compaction
// is disabled or the error doesn't look like an overflow, returns false.
//
// Detection is provider-agnostic: we string-match on the common phrasing
// ("context window", "exceeds limit", "too many tokens", "context_length")
// because Anthropic, OpenAI, and the various Anthropic-compat gateways
// (MiniMax with code 2013, OpenRouter, ...) each surface a slightly
// different error body. False positives just mean a wasted compaction
// cycle, which is cheap.
func (l *Loop) tryRecoverOverflow(ctx context.Context, err error, out chan<- Event) bool {
	if l.Compactor == nil || err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	if !strings.Contains(msg, "context") &&
		!strings.Contains(msg, "too many tokens") &&
		!strings.Contains(msg, "exceeds limit") {
		return false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	before := len(l.Messages)
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
