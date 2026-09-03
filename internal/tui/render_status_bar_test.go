package tui

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

// fakeContextProvider stubs just enough of llm.Provider for the status
// bar's percentage block. MaxContextTokens fixes the denominator at a
// known value so the test can predict the % output exactly.
type fakeContextProvider struct {
	maxCtx int
}

func (p *fakeContextProvider) Name() string          { return "fake" }
func (p *fakeContextProvider) MaxContextTokens() int { return p.maxCtx }
func (p *fakeContextProvider) ModelID() string       { return "" }
func (p *fakeContextProvider) Complete(context.Context, llm.Request) (*llm.Response, error) {
	return nil, errors.New("not implemented")
}
func (p *fakeContextProvider) Stream(context.Context, llm.Request) (llm.StreamReader, error) {
	return nil, errors.New("not implemented")
}

type changingContextProvider struct {
	calls int
}

func (p *changingContextProvider) Name() string    { return "changing" }
func (p *changingContextProvider) ModelID() string { return "shared-model" }
func (p *changingContextProvider) MaxContextTokens() int {
	p.calls++
	if p.calls%2 == 1 {
		return 100_000
	}
	return 200_000
}
func (p *changingContextProvider) Complete(context.Context, llm.Request) (*llm.Response, error) {
	return nil, errors.New("not implemented")
}
func (p *changingContextProvider) Stream(context.Context, llm.Request) (llm.StreamReader, error) {
	return nil, errors.New("not implemented")
}

// minimalModel builds the smallest *Model that renderStatusBar needs.
// Width is set wide enough that left + right + gap fit comfortably.
func minimalModel(maxCtx int) *Model {
	gate := permission.New(permission.ModeAcceptEdits)
	loop := agent.NewLoop(&fakeContextProvider{maxCtx: maxCtx}, tools.NewRegistry(), gate, nil, "test", 5)
	loop.ContextWindow = maxCtx
	return &Model{
		gate:  gate,
		loop:  loop,
		model: "test-model",
		width: 120,
	}
}

// setCanonicalContextTokens creates a local-history estimate with an exact
// token count under agent's CJK-aware estimator (4 message + 8 block overhead,
// then one token per Han rune). Status rendering must read this canonical loop
// value rather than the session-cumulative tokenTracker.
func setCanonicalContextTokens(t *testing.T, m *Model, tokens int) {
	t.Helper()
	if tokens < 12 {
		t.Fatalf("canonical context fixture needs at least 12 tokens, got %d", tokens)
	}
	m.loop.AppendUser(strings.Repeat("界", tokens-12))
	if got := m.loop.EstimateContextTokens(); got != tokens {
		t.Fatalf("canonical context fixture = %d, want %d", got, tokens)
	}
}

// TestStatusBar_RenderRawInteger — happy path: a single API call landed
// 38000 input tokens (no cache, no output bookkeeping). Bottom-right
// must show "38000 tokens" raw, not "38k", and the percentage against
// the 200k context window must be 19%.
func TestStatusBar_RenderRawInteger(t *testing.T) {
	m := minimalModel(200000)
	setCanonicalContextTokens(t, m, 38000)
	m.totalTokens.add(38000, 200, 0, 0)

	bar := stripANSI(renderStatusBar(m))
	if !strings.Contains(bar, "38000 tokens") {
		t.Errorf("status bar missing raw integer '38000 tokens'; got:\n%s", bar)
	}
	if strings.Contains(bar, "38k") {
		t.Errorf("status bar still using 'k' abbreviation:\n%s", bar)
	}
	if !strings.Contains(bar, "(19%)") {
		t.Errorf("status bar missing '(19%%)'; got:\n%s", bar)
	}
}

func TestStatusBarUsesSessionBoundContextWindowInsteadOfRequeryingProvider(t *testing.T) {
	gate := permission.New(permission.ModeAcceptEdits)
	provider := &changingContextProvider{}
	loop := agent.NewLoop(provider, tools.NewRegistry(), gate, nil, "test", 5)
	loop.Model = provider.ModelID()
	loop.ContextWindow = 100_000
	m := &Model{gate: gate, loop: loop, model: provider.ModelID(), width: 120}
	setCanonicalContextTokens(t, m, 10_000)

	first := stripANSI(renderStatusBar(m))
	second := stripANSI(renderStatusBar(m))
	if !strings.Contains(first, "(10%)") || !strings.Contains(second, "(10%)") {
		t.Fatalf("session-bound percentage changed across idle renders:\nfirst: %s\nsecond: %s", first, second)
	}
	if provider.calls != 0 {
		t.Fatalf("status render re-queried provider MaxContextTokens %d times", provider.calls)
	}
}

// Session-cumulative/per-call tracker spend must not override the canonical
// active context supplied by Loop. Provider response output is already part of
// that canonical snapshot when applicable; this fixture pins source selection.
func TestStatusBarTrackerSpendDoesNotOverrideCanonicalContext(t *testing.T) {
	m := minimalModel(200000)
	setCanonicalContextTokens(t, m, 38000)
	m.totalTokens.add(38000, 5000, 0, 0)

	bar := stripANSI(renderStatusBar(m))
	if !strings.Contains(bar, "38000 tokens") {
		t.Errorf("status bar should show canonical 38000, not tracker spend:\n%s", bar)
	}
	if strings.Contains(bar, "43000 tokens") {
		t.Errorf("tracker input+output leaked into canonical context:\n%s", bar)
	}
	if !strings.Contains(bar, "(19%)") {
		t.Errorf("percentage should be 19%% (38000/200000); got:\n%s", bar)
	}
}

// Cache buckets are provider-normalized and mutually exclusive, so both cache
// create and cache read contribute exactly once to the prompt occupancy.
//
// Setup: 500 input + 1000 cache_create + 30000 cache_read + 200 output.
// Expected raw API numerator: 31500.
func TestStatusBar_NormalizedCacheBucketsIncludedOnce(t *testing.T) {
	m := minimalModel(200000)
	setCanonicalContextTokens(t, m, 31500)
	m.totalTokens.add(500, 200, 1000, 30000)

	bar := stripANSI(renderStatusBar(m))
	want := "31500 tokens"
	if !strings.Contains(bar, want) {
		t.Errorf("status bar missing %q (normalized input + cache buckets); got:\n%s", want, bar)
	}
}

// TestStatusBar_NoTokensYet — pre-first-API-call render. Bottom-right
// should be empty (not "0 tokens (0%)" — too noisy at idle).
func TestStatusBar_NoTokensYet(t *testing.T) {
	m := minimalModel(200000)
	bar := stripANSI(renderStatusBar(m))
	if strings.Contains(bar, "tokens") {
		t.Errorf("status bar should not show 'tokens' before first API call; got:\n%s", bar)
	}
}

// Percentage uses the latest normalized prompt buckets over the model window.
// Output and session-cumulative spend remain excluded.
func TestStatusBar_PercentFormula(t *testing.T) {
	cases := []struct {
		input, output, cacheCreate, cacheRead, maxCtx int
		wantPct                                       string
	}{
		{1000, 0, 0, 0, 100000, "(1%)"},        // 1000 / 100000
		{50000, 0, 0, 0, 200000, "(25%)"},      // 50000 / 200000
		{180000, 0, 0, 0, 200000, "(90%)"},     // 180000 / 200000
		{500, 9999, 0, 99500, 200000, "(50%)"}, // normalized uncached + cache read = 100000
		{500, 0, 9500, 0, 200000, "(5%)"},      // cache_creation IS counted: 10000 / 200000
	}
	for _, tc := range cases {
		m := minimalModel(tc.maxCtx)
		setCanonicalContextTokens(t, m, tc.input+tc.cacheCreate+tc.cacheRead)
		m.totalTokens.add(tc.input, tc.output, tc.cacheCreate, tc.cacheRead)
		bar := stripANSI(renderStatusBar(m))
		if !strings.Contains(bar, tc.wantPct) {
			t.Errorf("input=%d output=%d cacheCreate=%d cacheRead=%d maxCtx=%d: want %s in bar; got:\n%s",
				tc.input, tc.output, tc.cacheCreate, tc.cacheRead, tc.maxCtx, tc.wantPct, bar)
		}
	}
}

func TestStatusBarRejectedMalformedUsageCannotLeakFromTracker(t *testing.T) {
	m := minimalModel(192_000)
	setCanonicalContextTokens(t, m, 2_500)
	// Simulate the raw event shape from a broken Anthropic-compatible gateway.
	// The canonical loop rejected it; presentation must not revive 302K tokens.
	m.totalTokens.add(2_000, 500, 0, 300_000)

	bar := stripANSI(renderStatusBar(m))
	if !strings.Contains(bar, "2500 tokens") {
		t.Fatalf("status bar did not use canonical fallback:\n%s", bar)
	}
	if strings.Contains(bar, "302000 tokens") {
		t.Fatalf("rejected raw usage leaked into status bar:\n%s", bar)
	}
}
