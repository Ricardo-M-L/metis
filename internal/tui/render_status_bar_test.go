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
func (p *fakeContextProvider) Complete(context.Context, llm.Request) (*llm.Response, error) {
	return nil, errors.New("not implemented")
}
func (p *fakeContextProvider) Stream(context.Context, llm.Request) (llm.StreamReader, error) {
	return nil, errors.New("not implemented")
}

// minimalModel builds the smallest *Model that renderStatusBar needs.
// Width is set wide enough that left + right + gap fit comfortably.
func minimalModel(maxCtx int) *Model {
	gate := permission.New(permission.ModeAuto)
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

// TestStatusBar_CacheIncluded — second pillar of plan A: prompt-cache
// tokens MUST be added to the input side. A session that hits the
// prompt cache hard would otherwise show a deceptively small bottom-
// right number while the live context is huge.
//
// Setup: 500 input (only the new uncached delta) + 1000 cache_create
// + 30000 cache_read + 200 output. Expected: 31500 (16%). NOT 500
// (which would be ignoring cache) and NOT 31700 (which would be
// including output).
func TestStatusBar_CacheIncluded(t *testing.T) {
	m := minimalModel(200000)
	m.totalTokens.add(500, 200, 1000, 30000)

	bar := stripANSI(renderStatusBar(m))
	want := "31500 tokens"
	if !strings.Contains(bar, want) {
		t.Errorf("status bar missing %q (input + cache, plan A); got:\n%s", want, bar)
	}
	if strings.Contains(bar, "500 tokens") && !strings.Contains(bar, "31500 tokens") {
		t.Errorf("status bar showing only raw input (500), cache not added:\n%s", bar)
	}
	if !strings.Contains(bar, "(15%)") {
		t.Errorf("status bar percentage should be 15%% (31500/200000); got:\n%s", bar)
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

// TestStatusBar_PercentMatchesCC — pin the % calculation formula. CC
// docs: used_percentage = (input + cache_creation + cache_read) / context_window_size.
// We verify metis uses the same denominator (Provider.MaxContextTokens())
// and the same numerator (ContextUsage()).
func TestStatusBar_PercentMatchesCC(t *testing.T) {
	cases := []struct {
		input, output, cacheCreate, cacheRead, maxCtx int
		wantPct                                       string
	}{
		{1000, 0, 0, 0, 100000, "(1%)"},        // 1000 / 100000
		{50000, 0, 0, 0, 200000, "(25%)"},      // 50000 / 200000
		{180000, 0, 0, 0, 200000, "(90%)"},     // 180000 / 200000
		{500, 9999, 0, 99500, 200000, "(50%)"}, // 100000 / 200000 — output excluded, cache included
	}
	for _, tc := range cases {
		m := minimalModel(tc.maxCtx)
		m.totalTokens.add(tc.input, tc.output, tc.cacheCreate, tc.cacheRead)
		bar := stripANSI(renderStatusBar(m))
		if !strings.Contains(bar, tc.wantPct) {
			t.Errorf("input=%d output=%d cacheCreate=%d cacheRead=%d maxCtx=%d: want %s in bar; got:\n%s",
				tc.input, tc.output, tc.cacheCreate, tc.cacheRead, tc.maxCtx, tc.wantPct, bar)
		}
	}
}
