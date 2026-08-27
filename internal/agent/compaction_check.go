package agent

import (
	"context"
	"fmt"

	"github.com/Ricardo-M-L/metis/internal/llm"
)

const (
	// Re-arm automatic summarization after roughly 5% of the input trigger has
	// arrived as new transcript material. Clamp the range so small/local model
	// windows can recover promptly while large coding windows require a genuinely
	// useful chunk of new work rather than one tiny status message.
	autoCompactRearmGrowthDivisor = 20
	autoCompactRearmMinTokens     = 256
	autoCompactRearmMaxTokens     = 4_096
)

// maybeCompact runs auto-compaction on the loop's history when the
// configured threshold is exceeded. No-op when l.Compactor is nil
// (compaction disabled in this build / for this session).
//
// CompactNow owns locking, hooks, durable commit ordering and lifecycle
// events, so every automatic pass has the same semantics as manual and
// overflow-triggered compaction.
func (l *Loop) maybeCompact(ctx context.Context, out chan<- Event) {
	// toolSpecs may hydrate lazy MCP state and therefore must run outside the
	// CompactNow mutex. Tests and embedders that call maybeCompact directly get
	// the same real-request pressure calculation as Loop.Run.
	var specs []llm.ToolSpec
	if l.Registry != nil {
		specs = l.toolSpecs()
	}
	l.maybeCompactWithPressure(ctx, out, l.EstimateRequestContextTokens(specs))
}

func (l *Loop) maybeCompactWithPressure(ctx context.Context, out chan<- Event, pressureTokens int) {
	if !l.compactorAvailable(false) {
		return
	}
	if l.deferRepeatedAutoCompact(pressureTokens) {
		return
	}
	sessionGeneration := l.autoCompactGenerationSnapshot()
	result, err := l.CompactNow(ctx, CompactOptions{
		Trigger:                "auto",
		EstimatedContextTokens: pressureTokens,
		Emit: func(ev Event) {
			emit(ctx, out, ev)
		},
	})
	if err == nil && result.Applied {
		l.noteAutoCompactPressure(result, pressureTokens, sessionGeneration)
	}
	if err != nil {
		// CompactNow already emitted the lifecycle error. The circuit notice is
		// deliberately separate and one-shot so users know how to recover.
		if l.takeCompactCircuitNotice() {
			emit(ctx, out, Event{
				Kind: EventInfo,
				Info: fmt.Sprintf("auto-compaction disabled after %d failures — /clear to reset", MaxConsecutiveCompactFailures),
			})
		}
	}
}

// compactorAvailable snapshots the replaceable Compactor pointer under the
// same lock used by RebindProviderRuntime. Compactor instances are immutable
// with respect to their provider/config binding after installation; runtime
// summary/circuit state is serialized separately by compactMu.
func (l *Loop) compactorAvailable(requireProvider bool) bool {
	if l == nil {
		return false
	}
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.Compactor != nil && (!requireProvider || l.Compactor.Provider != nil)
}

// takeCompactCircuitNotice atomically observes the current compactor's mutable
// circuit state and claims the one-shot notice. The lock order intentionally
// matches CompactNow (compactMu, then mu), so a provider/model rebind cannot
// race the pointer read and another compaction cannot race circuit mutation.
// Do not wait behind a different slow compaction merely to render a notice;
// this also keeps an event callback that re-enters an auto gate non-blocking.
func (l *Loop) takeCompactCircuitNotice() bool {
	if l == nil {
		return false
	}
	if !l.compactMu.TryLock() {
		return false
	}
	defer l.finishCompactorCriticalSection()
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.Compactor == nil || !l.Compactor.CircuitTripped() || l.compactCircuitNoticeSent {
		return false
	}
	l.compactCircuitNoticeSent = true
	return true
}

func autoCompactRearmGrowthTokens(trigger int) int {
	growth := trigger / autoCompactRearmGrowthDivisor
	if growth < autoCompactRearmMinTokens {
		return autoCompactRearmMinTokens
	}
	if growth > autoCompactRearmMaxTokens {
		return autoCompactRearmMaxTokens
	}
	return growth
}

// deferRepeatedAutoCompact prevents the fixed system/memory/tool portion of a
// request from repeatedly paying to summarize an unchanged checkpoint. Forced
// overflow and manual compaction bypass this gate through their direct
// CompactNow calls.
func (l *Loop) deferRepeatedAutoCompact(pressureTokens int) bool {
	if l == nil {
		return false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.Compactor == nil {
		return false
	}
	trigger := l.Compactor.TriggerTokens()
	requiredGrowth := autoCompactRearmGrowthTokens(trigger)
	if !l.autoCompactPressurePinned {
		return false
	}
	if trigger <= 0 || pressureTokens < trigger {
		l.clearAutoCompactPressureLocked()
		return false
	}
	currentHistoryTokens := estimateTokens(l.Messages)
	currentOverheadTokens := pressureTokens - currentHistoryTokens
	if currentOverheadTokens < 0 {
		currentOverheadTokens = 0
	}
	if l.historyRevision != l.autoCompactHistoryRevision {
		// Another CompactNow/Restore/Rewind replaced the transcript. Treat that
		// successful replacement as the new irreducible-pressure checkpoint,
		// rather than comparing it with a baseline from the old history. Preserve
		// growth appended after the replacement so a large new prompt can re-arm
		// compaction immediately instead of being swallowed by the lazy rebase.
		l.autoCompactHistoryTokens = l.historyReplacementTokens
		l.autoCompactOverheadTokens = currentOverheadTokens
		l.autoCompactHistoryRevision = l.historyRevision
		if currentHistoryTokens-l.autoCompactHistoryTokens < requiredGrowth {
			return true
		}
		l.clearAutoCompactPressureLocked()
		return false
	}
	if currentOverheadTokens-l.autoCompactOverheadTokens >= requiredGrowth {
		// Tool/plugin discovery, memory or volatile state changed materially.
		// Permit one fresh attempt against the genuinely different request.
		l.clearAutoCompactPressureLocked()
		return false
	}
	if currentOverheadTokens < l.autoCompactOverheadTokens {
		// A smaller fixed prefix is not a reason to resummarize unchanged history,
		// but it is the correct baseline for detecting later material growth.
		l.autoCompactOverheadTokens = currentOverheadTokens
	}
	if currentHistoryTokens-l.autoCompactHistoryTokens < requiredGrowth {
		return true
	}
	// Material new history has arrived. Clear before the attempt so a failed
	// summary follows the ordinary circuit-breaker policy; a successful but
	// still pressure-pinned result arms a fresh watermark below.
	l.clearAutoCompactPressureLocked()
	return false
}

// noteAutoCompactPressure arms the growth watermark only when the successful
// history replacement still leaves the full request above the automatic
// boundary. That is the irreducible-overhead case; ordinary compactions that
// return below the boundary retain the normal percentage trigger behavior.
func (l *Loop) noteAutoCompactPressure(result CompactResult, pressureTokens int, sessionGeneration uint64) {
	if l == nil || !result.Applied {
		return
	}
	overheadTokens := pressureTokens - result.BeforeTokens
	if overheadTokens < 0 {
		overheadTokens = 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.Compactor == nil {
		return
	}
	if l.autoCompactSessionGeneration != sessionGeneration || !historyHasPrefix(l.Messages, result.History) {
		return
	}
	postPressure := result.AfterTokens + overheadTokens
	pinned := postPressure >= l.Compactor.TriggerTokens()
	l.autoCompactPressurePinned = pinned
	if pinned {
		l.autoCompactHistoryTokens = result.AfterTokens
		l.autoCompactOverheadTokens = overheadTokens
		l.autoCompactHistoryRevision = l.historyRevision
	} else {
		l.clearAutoCompactPressureLocked()
	}
}

func (l *Loop) autoCompactGenerationSnapshot() uint64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.autoCompactSessionGeneration
}

func (l *Loop) clearAutoCompactPressureLocked() {
	l.autoCompactPressurePinned = false
	l.autoCompactHistoryTokens = 0
	l.autoCompactOverheadTokens = 0
	l.autoCompactHistoryRevision = 0
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
// The emergency option permits a final SnipAll fallback when the regular
// checkpoint cannot make progress, while retaining the same hooks,
// persistence and event ordering as every other trigger.
func (l *Loop) tryRecoverOverflow(ctx context.Context, err error, out chan<- Event) bool {
	if err == nil || !l.compactorAvailable(false) {
		return false
	}
	if ClassifyError(err) != ErrContextOverflow {
		return false
	}
	sessionGeneration := l.autoCompactGenerationSnapshot()
	result, compactErr := l.CompactNow(ctx, CompactOptions{
		Trigger:   "overflow",
		Force:     true,
		Emergency: true,
		Emit: func(ev Event) {
			emit(ctx, out, ev)
		},
	})
	if compactErr == nil && result.Applied {
		pressureTokens := result.BeforeTokens + int(l.requestOverheadTokens.Load())
		l.noteAutoCompactPressure(result, pressureTokens, sessionGeneration)
	}
	return compactErr == nil && result.Applied
}
