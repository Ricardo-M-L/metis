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

// minimalModel builds the smallest *Model that renderStatusBar needs.
// Width is set wide enough that left + right + gap fit comfortably.
func minimalModel(maxCtx int) *Model {
	gate := permission.New(permission.ModeAcceptEdits)
	loop := agent.NewLoop(&fakeContextProvider{maxCtx: maxCtx}, tools.NewRegistry(), gate, nil, "test", 5)
	return &Model{
		gate:  gate,
		loop:  loop,
		model: "test-model",
		width: 120,
	}
}

// TestStatusBar_RenderRawInteger — happy path: a single API call landed
// 38000 input tokens (no cache, no output bookkeeping). Bottom-right
// must show "38000 tokens" raw, not "38k", and the percentage against
// the 200k context window must be 19%.
func TestStatusBar_RenderRawInteger(t *testing.T) {
	m := minimalModel(200000)
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

// TestStatusBar_OutputExcluded — regression guard for plan A semantics.
// Output tokens MUST NOT show in the bottom-right. If a future refactor
// accidentally swapped ContextUsage for LastTotal, the percentage would
// reflect input+output and this test fails.
//
// Setup: 38000 input + 5000 output. Plan A renders 38000 (19%); plan B
// would render 43000 (21%). Test pins plan A.
func TestStatusBar_OutputExcluded(t *testing.T) {
	m := minimalModel(200000)
	m.totalTokens.add(38000, 5000, 0, 0)

	bar := stripANSI(renderStatusBar(m))
	if !strings.Contains(bar, "38000 tokens") {
		t.Errorf("plan A: bottom-right should show input only (38000), not input+output:\n%s", bar)
	}
	if strings.Contains(bar, "43000 tokens") {
		t.Errorf("plan A regression: 43000 = input+output appeared (output should be excluded):\n%s", bar)
	}
	if !strings.Contains(bar, "(19%)") {
		t.Errorf("plan A: percentage should be 19%% (38000/200000); got:\n%s", bar)
	}
}

// TestStatusBar_CacheCreateIncluded_ReadExcluded — 2026-05-18 contract
// change: cache_creation IS still added to the numerator (those are
// real fresh tokens this turn), but cache_read is NOT (provider may
// over-report it; the byte-estimate floor in render_chrome.go picks
// up the actual conversation weight). See ContextUsage() in
// commands.go for the rationale.
//
// Setup: 500 input + 1000 cache_create + 30000 cache_read + 200 output.
// Expected raw API numerator: 1500. Display floors with byte estimate,
// which for minimalModel's tiny stub history is < 1500, so 1500 wins.
func TestStatusBar_CacheCreateIncluded_ReadExcluded(t *testing.T) {
	m := minimalModel(200000)
	m.totalTokens.add(500, 200, 1000, 30000)

	bar := stripANSI(renderStatusBar(m))
	want := "1500 tokens"
	if !strings.Contains(bar, want) {
		t.Errorf("status bar missing %q (input + cache_creation, cache_read excluded); got:\n%s", want, bar)
	}
	if strings.Contains(bar, "31500 tokens") {
		t.Errorf("status bar should NOT include cache_read (provider over-reports it); got:\n%s", bar)
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

// TestStatusBar_PercentFormula — pin the % calculation formula post-
// 2026-05-18: numerator = input + cache_creation (cache_read deliberately
// excluded to neutralize MiniMax-style over-reporting; see ContextUsage
// comment). Denominator = Provider.MaxContextTokens(). Output excluded.
func TestStatusBar_PercentFormula(t *testing.T) {
	cases := []struct {
		input, output, cacheCreate, cacheRead, maxCtx int
		wantPct                                       string
	}{
		{1000, 0, 0, 0, 100000, "(1%)"},     // 1000 / 100000
		{50000, 0, 0, 0, 200000, "(25%)"},   // 50000 / 200000
		{180000, 0, 0, 0, 200000, "(90%)"},  // 180000 / 200000
		{500, 9999, 0, 99500, 200000, ""},   // cache_read inflated; numerator stays 500 (well under 1%) — see ImmuneToOverreportedCacheRead test
		{500, 0, 9500, 0, 200000, "(5%)"},   // cache_creation IS counted: 10000 / 200000
	}
	for _, tc := range cases {
		m := minimalModel(tc.maxCtx)
		m.totalTokens.add(tc.input, tc.output, tc.cacheCreate, tc.cacheRead)
		bar := stripANSI(renderStatusBar(m))
		if tc.wantPct == "" {
			// Don't assert a specific percentage; just verify cache_read
			// is NOT inflating the display past ~1%.
			if strings.Contains(bar, "(50%)") || strings.Contains(bar, "(49%)") {
				t.Errorf("case input=%d cacheRead=%d should NOT show inflated %% from cache_read; got:\n%s",
					tc.input, tc.cacheRead, bar)
			}
			continue
		}
		if !strings.Contains(bar, tc.wantPct) {
			t.Errorf("input=%d output=%d cacheCreate=%d cacheRead=%d maxCtx=%d: want %s in bar; got:\n%s",
				tc.input, tc.output, tc.cacheCreate, tc.cacheRead, tc.maxCtx, tc.wantPct, bar)
		}
	}
}
