// Package budget tracks cumulative USD spend across LLM round-trips
// and enforces a session-level cap. Mirrors claude-code's maxBudgetUsd
// (QueryEngine.ts:145-149,362-365): when the cap is hit the loop ends
// the turn gracefully instead of burning more requests; at 90% the
// model receives one system-reminder so it can wrap up on its own
// terms first.
//
// Pricing comes from the models.dev catalog (internal/llm/catalog
// Cost — USD per million tokens). The Tracker holds the resolved
// rates; it never does catalog lookups itself, so the agent loop
// stays free of catalog dependencies. Sub-agents share the PARENT's
// tracker: a child's spend draws down the same pool, which is the
// behavior a "this task may cost at most $X" cap implies.
package budget

import (
	"fmt"
	"sync"
)

// Rates is the USD price per million tokens, by token class. A zero
// Rates (unknown model pricing) makes every AddUsage a no-op spend —
// the cap then never trips, which is the safe direction: an unknown
// model must not kill a session with a bogus $0-budget calculation.
type Rates struct {
	InputPerMTok      float64
	OutputPerMTok     float64
	CacheReadPerMTok  float64
	CacheWritePerMTok float64
}

// Tracker accumulates spend. Safe for concurrent use — parent and
// background sub-agent loops add usage from their own goroutines.
type Tracker struct {
	mu     sync.Mutex
	rates  Rates
	maxUSD float64 // 0 = unlimited (tracking only)
	spent  float64
	warned bool

	// ratesFn lazily re-resolves pricing until it reports a FINAL
	// answer. Needed because the models.dev catalog warms up in a
	// BACKGROUND goroutine: at BuildAgentLoop time the lookup usually
	// misses, and a tracker frozen at zero rates would never trip
	// (caught live 2026-06-11: --max-budget-usd 0.0001 sailed through
	// a 538k-token run). The bool return lets the fn also finalize a
	// MISS once the catalog is warm — without it, a model the catalog
	// never prices would re-scan the full provider map on every
	// round-trip forever. nil = rates are fixed at construction.
	ratesFn  func() (Rates, bool)
	resolved bool
}

// warnFraction is where the one-shot heads-up fires. Same spirit as
// the iter-nudge 90% threshold (internal/agent/iter_nudge.go).
const warnFraction = 0.9

// NewTracker returns a tracker enforcing maxUSD (0 = track only,
// never trip) at the given fixed rates.
func NewTracker(maxUSD float64, rates Rates) *Tracker {
	return &Tracker{maxUSD: maxUSD, rates: rates, resolved: true}
}

// NewTrackerLazy returns a tracker that resolves pricing through fn
// on each AddUsage until fn reports final=true, after which the
// returned Rates (possibly zero — "this model is unpriceable") are
// cached. Use when pricing comes from an async-warmed source (the
// models.dev catalog).
func NewTrackerLazy(maxUSD float64, fn func() (Rates, bool)) *Tracker {
	return &Tracker{maxUSD: maxUSD, ratesFn: fn}
}

// SetRatesResolver changes the pricing source used for future usage without
// resetting spend or the configured cap. A long-lived TUI can switch models
// inside one session; keeping the old model's resolved rates would make every
// subsequent request (and the budget warning/cap) use the wrong price.
//
// The new resolver is evaluated lazily on the next AddUsage call, matching
// NewTrackerLazy's catalogue warm-up behaviour. A nil resolver deliberately
// installs fixed zero rates, which is the safe fallback for an unpriceable
// model: usage remains counted by the caller, but no bogus USD amount is
// charged.
func (t *Tracker) SetRatesResolver(fn func() (Rates, bool)) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.rates = Rates{}
	t.ratesFn = fn
	t.resolved = fn == nil
}

// currentRates returns the effective rates, re-resolving lazily until
// the source declares its answer final. Caller holds t.mu.
func (t *Tracker) currentRates() Rates {
	if !t.resolved && t.ratesFn != nil {
		if r, final := t.ratesFn(); final {
			t.rates = r
			t.resolved = true
		}
	}
	return t.rates
}

// AddUsage records one LLM round-trip's token usage.
func (t *Tracker) AddUsage(in, out, cacheRead, cacheWrite int) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	r := t.currentRates()
	const mtok = 1_000_000
	t.spent += float64(in)/mtok*r.InputPerMTok +
		float64(out)/mtok*r.OutputPerMTok +
		float64(cacheRead)/mtok*r.CacheReadPerMTok +
		float64(cacheWrite)/mtok*r.CacheWritePerMTok
}

// Reset starts a new top-level session budget while preserving the configured
// cap and pricing resolver.
func (t *Tracker) Reset() {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.spent = 0
	t.warned = false
}

// SpentUSD returns the cumulative spend so far.
func (t *Tracker) SpentUSD() float64 {
	if t == nil {
		return 0
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.spent
}

// Exceeded reports whether the cap has been reached.
func (t *Tracker) Exceeded() bool {
	if t == nil {
		return false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.maxUSD > 0 && t.spent >= t.maxUSD
}

// TakeWarning returns a one-shot reminder body when spend first
// crosses warnFraction of the cap, and "" on every other call. The
// caller injects it as a <system-reminder> user message.
func (t *Tracker) TakeWarning() string {
	if t == nil {
		return ""
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.maxUSD <= 0 || t.warned || t.spent < t.maxUSD*warnFraction {
		return ""
	}
	t.warned = true
	return fmt.Sprintf(
		"<system-reminder>Budget check: $%.4f of the $%g USD budget for this session is spent (%.0f%%). Wrap up: finish the essential remaining work and summarize, rather than starting new exploration.</system-reminder>",
		t.spent, t.maxUSD, t.spent/t.maxUSD*100,
	)
}

// ExceededMessage is the user-facing line emitted when the loop stops
// on budget. Kept here so CLI and TUI render the same wording.
func (t *Tracker) ExceededMessage() string {
	if t == nil {
		return ""
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return fmt.Sprintf("USD budget reached ($%.4f spent of $%g cap) — stopping before the next LLM call. Raise --max-budget-usd to continue.", t.spent, t.maxUSD)
}
