package agent

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"

	"github.com/Ricardo-M-L/metis/internal/llm"
)

// CompactOptions describes why the single compaction pipeline is running.
// Every trigger (automatic pressure, manual command, overflow recovery and
// iteration-budget second wind) uses this shape so retention and lifecycle
// semantics cannot drift between CLI and Desktop.
type CompactOptions struct {
	Trigger      string
	Force        bool
	Emergency    bool
	Instructions string
	// EstimatedContextTokens is the non-mutating request preflight total
	// (history plus system/state/memory/tools). Zero falls back to history-only
	// behavior for direct/manual callers. It affects only the auto decision;
	// result token deltas continue to describe the history being replaced.
	EstimatedContextTokens int

	// Persist, when non-nil, must durably commit the final replacement before
	// it becomes observable as the loop's live history. Manual CLI/Desktop
	// callers use it to avoid an in-memory/disk split on write failure.
	Persist func([]llm.Message) error

	// Emit receives lifecycle/progress events. It may be nil for synchronous
	// command surfaces that render CompactResult directly.
	Emit func(Event)
}

type CompactResult struct {
	Applied        bool
	Trigger        string
	BeforeMessages int
	AfterMessages  int
	BeforeTokens   int
	AfterTokens    int
	History        []llm.Message
}

// ErrCompactionInProgress is returned when a second compaction reaches the
// same Loop while an existing transaction still owns its summarizer state.
// Returning immediately is important for event, hook and persistence
// callbacks: waiting on a non-reentrant mutex from inside the active
// transaction would self-deadlock forever.
var ErrCompactionInProgress = errors.New("compact: compaction already in progress")

// CompactNow is the sole Loop-level context replacement pipeline. It owns
// threshold/force policy, hooks, progress lifecycle, final budget guards
// (inside Compactor.Compact), durable commit ordering and live installation.
func (l *Loop) CompactNow(ctx context.Context, opts CompactOptions) (result CompactResult, err error) {
	if l == nil {
		return result, fmt.Errorf("compact: compactor/provider unavailable")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	trigger := strings.TrimSpace(opts.Trigger)
	if trigger == "" {
		trigger = "manual"
	}
	result.Trigger = trigger

	// Compactor carries iterative-summary and circuit-breaker state, so two
	// compactions must never overlap even though the slow/external portions of
	// this transaction deliberately run without Loop.mu. Do not queue here:
	// Emit/hooks/Persist are user callbacks and may synchronously re-enter this
	// method. A blocking mutex would make that callback self-deadlock.
	if !l.compactMu.TryLock() {
		return result, ErrCompactionInProgress
	}
	defer l.finishCompactorCriticalSection()

	l.mu.Lock()
	compactor := l.Compactor
	if compactor == nil || compactor.Provider == nil {
		l.mu.Unlock()
		return result, fmt.Errorf("compact: compactor/provider unavailable")
	}
	hooks := l.Hooks
	checkpoint := l.CompactionCheckpoint
	model, turn := l.Model, l.turnIdx
	routingRevision := l.routingRevision
	requestOverhead := int(l.requestOverheadTokens.Load())

	result.BeforeMessages = len(l.Messages)
	result.BeforeTokens = estimateTokens(l.Messages)
	decisionTokens := result.BeforeTokens
	if opts.EstimatedContextTokens > decisionTokens {
		decisionTokens = opts.EstimatedContextTokens
	}
	if !opts.Force && !compactor.ShouldCompactTokens(decisionTokens) {
		result.AfterMessages = result.BeforeMessages
		result.AfterTokens = result.BeforeTokens
		result.History = cloneMessages(l.Messages)
		l.mu.Unlock()
		return result, nil
	}
	if len(l.Messages) <= 2 || compactor.CircuitTripped() {
		result.AfterMessages = result.BeforeMessages
		result.AfterTokens = result.BeforeTokens
		result.History = cloneMessages(l.Messages)
		l.mu.Unlock()
		return result, nil
	}

	beforeIdentity := l.Messages
	before := cloneMessages(l.Messages)
	priorSummary := compactor.LastSummary
	priorFailures := compactor.consecutiveFailures
	l.mu.Unlock()

	restoreCompactorState := func() {
		compactor.LastSummary = priorSummary
		compactor.consecutiveFailures = priorFailures
	}
	setResultHistory := func(history []llm.Message) {
		result.AfterMessages = len(history)
		result.AfterTokens = estimateTokens(history)
		result.History = cloneMessages(history)
	}
	currentHistory := func() []llm.Message {
		l.mu.Lock()
		defer l.mu.Unlock()
		return cloneMessages(l.Messages)
	}
	beforeStillCurrent := func() bool {
		l.mu.Lock()
		defer l.mu.Unlock()
		return sameHistoryIdentity(l.Messages, beforeIdentity) &&
			l.routingRevision == routingRevision && l.Compactor == compactor
	}

	// Hooks may inspect Loop.History, so never invoke them under l.mu.
	if hooks != nil {
		pc := &PreCompact{
			Trigger:         trigger,
			MessageCount:    result.BeforeMessages,
			EstimatedTokens: result.BeforeTokens,
		}
		hooks.EmitPreCompact(ctx, HookContext{Model: model, Turn: turn}, pc)
		// A hook is read-only by contract, but another goroutine could have
		// replaced history while the mutex was released. Refuse to compact a
		// stale snapshot instead of overwriting the newer conversation.
		if !beforeStillCurrent() {
			setResultHistory(currentHistory())
			return result, fmt.Errorf("compact: history changed while pre-compact hook was running")
		}
	}

	// Progress forwarding runs in a helper goroutine while lifecycle events
	// are emitted by this goroutine. Serialize the user callback: most event
	// sinks (and callers collecting a slice) are intentionally not required
	// to be concurrency-safe.
	var emitMu sync.Mutex
	emitCompact := func(ev Event) {
		if opts.Emit != nil {
			emitMu.Lock()
			opts.Emit(ev)
			emitMu.Unlock()
		}
	}

	compactCtx := ctx
	var progress chan Event
	var progressDone chan struct{}
	closeProgress := func() {
		if progress != nil {
			close(progress)
			<-progressDone
			progress = nil
		}
	}
	emitCompact(Event{Kind: EventCompactionStart, Info: trigger})
	defer func() {
		closeProgress()
		emitCompact(Event{Kind: EventCompactionEnd, Info: trigger, Err: err})
	}()
	// Emit is an arbitrary external callback. It may inspect or even mutate the
	// loop; reject a stale source snapshot before spending a provider call.
	if !beforeStillCurrent() {
		setResultHistory(currentHistory())
		return result, fmt.Errorf("compact: history changed while compaction start was emitted")
	}
	if opts.Emit != nil {
		progress = make(chan Event, 128)
		progressDone = make(chan struct{})
		compactCtx = WithEventOut(ctx, progress)
		go func() {
			defer close(progressDone)
			for ev := range progress {
				emitCompact(ev)
			}
		}()
	}

	// Provider summarization may take seconds and may itself call back into the
	// loop. compactMu protects Compactor state while Loop.mu remains available.
	candidate, compactErr := compactor.compactWithRequestOverhead(
		compactCtx, before, opts.Instructions, requestOverhead, decisionTokens, opts.Force,
	)
	// Drain all provider progress callbacks before the history CAS. Otherwise a
	// queued callback could mutate history between the check and installation.
	closeProgress()
	if compactErr != nil {
		// CompactWithInstructions may publish a candidate summary before a
		// later validation rejects the checkpoint. An unsuccessful Loop-level
		// transaction must never replace the last durable summary seed.
		compactor.LastSummary = priorSummary

		// User cancellation and deadline expiry describe the lifetime of this
		// attempt, not summarizer health. CompactWithInstructions records every
		// error uniformly, so restore the breaker only for context termination;
		// genuine provider/summary failures keep their accumulated count.
		// Only the caller's lifetime is neutral to summarizer health. A provider
		// watchdog or the compactor's own SummaryTimeout also returns
		// DeadlineExceeded, but that is a real failed attempt and must advance the
		// circuit breaker; otherwise an over-threshold session can pay another
		// full timeout on every agent iteration forever.
		callerTerminated := errors.Is(ctx.Err(), context.Canceled) ||
			errors.Is(ctx.Err(), context.DeadlineExceeded)
		if callerTerminated {
			compactor.consecutiveFailures = priorFailures
		}
		return result, compactErr
	}
	// Provider-owned continuation IDs describe the exact pre-compaction
	// transcript. Once the prefix is summarized/replaced, replaying any ID from
	// that transcript can make a stateful Responses endpoint reject the turn.
	candidate = stripProviderState(candidate)
	result.AfterMessages = len(candidate)
	result.AfterTokens = estimateTokens(candidate)
	if opts.Emergency && result.AfterTokens >= result.BeforeTokens && result.AfterMessages >= result.BeforeMessages {
		candidate = compactor.SnipAll(before)
		result.AfterMessages = len(candidate)
		result.AfterTokens = estimateTokens(candidate)
	}
	if result.AfterTokens >= result.BeforeTokens && result.AfterMessages >= result.BeforeMessages {
		restoreCompactorState()
		result.AfterMessages = result.BeforeMessages
		result.AfterTokens = result.BeforeTokens
		setResultHistory(currentHistory())
		return result, nil
	}

	// Install temporarily so existing PostCompact hooks that call History()
	// observe the same post-summary state as the historical auto path. Any
	// persistence failure below rolls this back before returning.
	l.mu.Lock()
	if !sameHistoryIdentity(l.Messages, beforeIdentity) ||
		l.routingRevision != routingRevision || l.Compactor != compactor {
		live := cloneMessages(l.Messages)
		routingChanged := l.routingRevision != routingRevision || l.Compactor != compactor
		l.mu.Unlock()
		restoreCompactorState()
		setResultHistory(live)
		if routingChanged {
			return result, fmt.Errorf("compact: provider/model changed while summary was being generated")
		}
		return result, fmt.Errorf("compact: history changed while summary was being generated")
	}
	l.Messages = candidate
	l.storeContextEstimateFromHistory(result.AfterTokens)
	candidateIdentity := l.Messages
	candidateSnapshot := cloneMessages(candidate)
	l.mu.Unlock()

	extra := ""
	if hooks != nil {
		pc := &PostCompact{
			Trigger:        trigger,
			Tier:           "compact",
			BeforeMessages: result.BeforeMessages,
			AfterMessages:  result.AfterMessages,
			BeforeTokens:   result.BeforeTokens,
			AfterTokens:    result.AfterTokens,
		}
		extra = hooks.EmitPostCompact(ctx, HookContext{Model: model, Turn: turn}, pc)
	}

	// Reconcile the PostCompact window under the lock. AppendUser is a safe,
	// monotonic extension of the installed candidate, so retain it as a suffix.
	// Restore is a replacement: it wins, and this stale transaction aborts.
	l.mu.Lock()
	liveAfterHook := l.Messages
	if !sameHistoryIdentity(liveAfterHook, candidateIdentity) &&
		!historyHasPrefix(liveAfterHook, candidateSnapshot) {
		live := cloneMessages(liveAfterHook)
		l.mu.Unlock()
		restoreCompactorState()
		setResultHistory(live)
		return result, fmt.Errorf("compact: history replaced while post-compact hook was running")
	}
	postHookSuffix := cloneMessages(liveAfterHook[len(candidateSnapshot):])
	final := cloneMessages(candidateSnapshot)
	if strings.TrimSpace(extra) != "" {
		final = append(final, llm.Message{
			Role: llm.RoleUser,
			Content: []llm.ContentBlock{{
				Type:      "text",
				Text:      "[post-compact hook context] " + extra,
				Synthetic: true,
			}},
		})
	}
	final = append(final, postHookSuffix...)
	final = stripProviderState(final)
	rollbackBase := append(cloneMessages(before), postHookSuffix...)
	if historyCap, constrained, budgetErr := compactor.postCompactHistoryCap(requestOverhead); budgetErr != nil ||
		(constrained && estimateTokens(final) >= historyCap) {
		if budgetErr == nil {
			budgetErr = fmt.Errorf("compact: post-compact hook/suffix exceeds effective history budget (%d >= %d tokens; request overhead=%d)",
				estimateTokens(final), historyCap, requestOverhead)
		}
		l.Messages = rollbackBase
		live := cloneMessages(l.Messages)
		l.storeContextEstimateFromHistory(estimateTokens(live))
		l.mu.Unlock()
		restoreCompactorState()
		setResultHistory(live)
		return result, budgetErr
	}
	l.Messages = final
	result.AfterMessages = len(final)
	result.AfterTokens = estimateTokens(final)
	l.storeContextEstimateFromHistory(result.AfterTokens)
	finalIdentity := l.Messages
	l.mu.Unlock()

	// The loop-level checkpoint receives both sides of the replacement so
	// session stores can append an unpersisted raw tail before committing the
	// logical history_replace snapshot. It supersedes the legacy final-only
	// callback; invoking both would advance two independent cursors.
	persistKind := "replacement"
	if checkpoint != nil {
		persistKind = "checkpoint"
	}
	hasPersistence := checkpoint != nil || opts.Persist != nil
	persist := func(rawBefore, replacement []llm.Message) error {
		if checkpoint != nil {
			return checkpoint(cloneMessages(rawBefore), cloneMessages(replacement))
		}
		if opts.Persist != nil {
			return opts.Persist(cloneMessages(replacement))
		}
		return nil
	}
	if persistErr := persist(before, final); persistErr != nil {
		// The callback ran without Loop.mu, so preserve any concurrent suffix
		// while rolling the failed compacted prefix back to its raw source.
		l.mu.Lock()
		live := l.Messages
		switch {
		case sameHistoryIdentity(live, finalIdentity) || historiesEqual(live, final):
			l.Messages = rollbackBase
		case historyHasPrefix(live, final):
			persistSuffix := cloneMessages(live[len(final):])
			l.Messages = append(cloneMessages(rollbackBase), persistSuffix...)
			// A concurrent Restore is authoritative. Leaving it untouched is safer
			// than replacing it with either side of this stale transaction.
		}
		live = cloneMessages(l.Messages)
		l.storeContextEstimateFromHistory(estimateTokens(live))
		l.mu.Unlock()
		restoreCompactorState()
		setResultHistory(live)
		return result, fmt.Errorf("compact: persist %s: %w", persistKind, persistErr)
	}

	// Persistence was intentionally performed without Loop.mu. If AppendUser
	// extended the compacted snapshot in that window, write a compensating
	// replacement so the durable cursor catches up. A concurrent Restore is
	// also compensated, but wins the transaction and produces an explicit
	// conflict instead of being silently overwritten in memory.
	durable := final
	durableIdentity := finalIdentity
	replacementConflict := false
	const maxPersistReconciliations = 4
	for reconciliation := 0; ; reconciliation++ {
		l.mu.Lock()
		liveIdentity := l.Messages
		if sameHistoryIdentity(liveIdentity, durableIdentity) || historiesEqual(liveIdentity, durable) {
			live := cloneMessages(liveIdentity)
			if replacementConflict {
				l.mu.Unlock()
				restoreCompactorState()
				setResultHistory(live)
				return result, fmt.Errorf("compact: history replaced while compacted checkpoint was persisted")
			}
			l.storeContextEstimateFromHistory(estimateTokens(live))
			l.compactCircuitNoticeSent = false
			l.mu.Unlock()
			final = live
			break
		}

		live := cloneMessages(liveIdentity)
		if !historyHasPrefix(liveIdentity, durable) {
			replacementConflict = true
		}
		l.mu.Unlock()

		if !hasPersistence {
			if replacementConflict {
				restoreCompactorState()
				setResultHistory(live)
				return result, fmt.Errorf("compact: history replaced while compacted result was installed")
			}
			// With no durable sink there is nothing to compensate; the monotonic
			// live suffix is already the correct final history.
			final = live
			break
		}
		if reconciliation >= maxPersistReconciliations {
			if replacementConflict {
				restoreCompactorState()
			}
			setResultHistory(live)
			return result, fmt.Errorf("compact: history kept changing during persist reconciliation")
		}
		if persistErr := persist(live, live); persistErr != nil {
			if replacementConflict {
				restoreCompactorState()
			}
			setResultHistory(live)
			return result, fmt.Errorf("compact: persist compensation %s: %w", persistKind, persistErr)
		}
		durable = live
		durableIdentity = liveIdentity
	}

	// The history prefix was replaced. Force the next provider request (and
	// any fork snapshot taken before it) to publish one complete runtime-state
	// section even when the semantic fields themselves are unchanged.
	l.InvalidateRuntimeState()
	result.Applied = true
	setResultHistory(final)
	// Drain progress before publishing the applied checkpoint. Besides making
	// event order deterministic, this prevents a late progress event from
	// racing the success/end lifecycle at non-thread-safe consumers.
	closeProgress()
	emitCompact(Event{
		Kind:                  EventContextCompacted,
		Info:                  trigger,
		PreviousContextTokens: result.BeforeTokens,
		ContextTokens:         result.AfterTokens,
	})
	emitCompact(Event{
		Kind: EventInfo,
		Info: fmt.Sprintf("context compacted (%s): %d -> %d messages | ~%d -> ~%d tokens",
			trigger, result.BeforeMessages, result.AfterMessages, result.BeforeTokens, result.AfterTokens),
	})
	return result, nil
}

func cloneMessages(messages []llm.Message) []llm.Message {
	if messages == nil {
		return nil
	}
	return append([]llm.Message(nil), messages...)
}

func stripProviderState(messages []llm.Message) []llm.Message {
	out := cloneMessages(messages)
	for i := range out {
		out[i].Content = stripProviderStateBlocks(out[i].Content)
	}
	return out
}

func stripProviderStateBlocks(blocks []llm.ContentBlock) []llm.ContentBlock {
	out := make([]llm.ContentBlock, 0, len(blocks))
	for _, block := range blocks {
		if block.Type == "provider_state" {
			continue
		}
		if len(block.ToolResultBlocks) > 0 {
			block.ToolResultBlocks = stripProviderStateBlocks(block.ToolResultBlocks)
		}
		out = append(out, block)
	}
	return out
}

func historiesEqual(a, b []llm.Message) bool {
	return reflect.DeepEqual(a, b)
}

func historyHasPrefix(history, prefix []llm.Message) bool {
	return len(history) >= len(prefix) && historiesEqual(history[:len(prefix)], prefix)
}

// sameHistoryIdentity is intentionally cheap. While the pre-hook mutex is
// released, every supported replacement changes either length or the backing
// slice; ordinary append also changes length. Deep equality would copy/scan
// potentially hundreds of megabytes immediately before summarization.
func sameHistoryIdentity(a, b []llm.Message) bool {
	if len(a) != len(b) {
		return false
	}
	if len(a) == 0 {
		return true
	}
	return &a[0] == &b[0]
}
