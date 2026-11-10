package agent

import (
	"context"
	"fmt"

	"github.com/Ricardo-M-L/metis/internal/llm"
)

// admitBuiltRequest closes the interval between preflight and immutable request
// construction. It meters that concrete request without repeating runtime
// callbacks. New pressure crossing an automatic boundary gets a fresh decision;
// an input exceeding a verified hard limit is never dispatched unreduced.
// Repository locks are held only by the short build/validation snapshots, never
// while a summarizer runs. Rebuilds are bounded when a host keeps changing input.
func (l *Loop) admitBuiltRequest(ctx context.Context, out chan<- Event, specs []llm.ToolSpec, checkedPressure int, req llm.Request, anchor contextRequestAnchor) (llm.Request, contextRequestAnchor, error) {
	posture := anchor
	for attempts := 0; ; attempts++ {
		pressure := anchor.requestContextTokens
		inputLimit := anchor.maxInput
		if anchor.maxContext > 0 && (inputLimit <= 0 || anchor.maxContext < inputLimit) {
			inputLimit = anchor.maxContext
		}
		overInput := inputLimit > 0 && pressure > inputLimit
		l.mu.RLock()
		compactor := l.Compactor
		aboveAuto := false
		if compactor != nil {
			trigger := compactor.TriggerTokens()
			minimum := compactor.MinimumTokens
			if minimum > trigger {
				minimum = trigger
			}
			aboveAuto = trigger > 0 && pressure >= trigger && (minimum <= 0 || pressure >= minimum)
		}
		l.mu.RUnlock()
		if !overInput && (pressure <= checkedPressure || !aboveAuto) {
			return req, anchor, nil
		}
		if !overInput && l.deferRepeatedAutoCompact(pressure) {
			return req, anchor, nil
		}
		if attempts >= 2 {
			return llm.Request{}, contextRequestAnchor{}, fmt.Errorf("context admission: request input changed repeatedly while preparing dispatch; retry after memory/runtime updates settle")
		}
		if compactor == nil {
			return llm.Request{}, contextRequestAnchor{}, fmt.Errorf("context admission: estimated input %d exceeds limit %d and compaction is unavailable", pressure, inputLimit)
		}
		generation := l.autoCompactGenerationSnapshot()
		result, err := l.CompactNow(ctx, CompactOptions{Trigger: "admission", Force: overInput, EstimatedContextTokens: pressure, Emit: func(ev Event) { emit(ctx, out, ev) }})
		if err != nil {
			return llm.Request{}, contextRequestAnchor{}, err
		}
		if !result.Applied {
			if overInput {
				return llm.Request{}, contextRequestAnchor{}, fmt.Errorf("context admission: estimated input %d still exceeds limit %d after compaction", pressure, inputLimit)
			}
			// Irreducible prefixes and a tripped automatic-summary circuit keep
			// the existing soft-threshold behavior below a hard provider limit.
			return req, anchor, nil
		}
		l.noteAutoCompactPressure(result, pressure, generation)
		checkedPressure = pressure
		l.mu.Lock()
		if l.autoCompactSessionGeneration != posture.sessionGeneration || l.routingRevision != posture.routingRevision || l.Model != posture.requestModel || identifyContextProvider(l.Provider) != posture.providerID {
			l.mu.Unlock()
			return llm.Request{}, contextRequestAnchor{}, fmt.Errorf("context admission: session or provider changed while preparing dispatch")
		}
		l.withMemoryGuardLocked(func() { req, anchor = l.buildRequestMemoryLocked(specs, &posture) })
		l.mu.Unlock()
	}
}
