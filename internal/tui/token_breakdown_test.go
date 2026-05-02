package tui

import (
	"strings"
	"testing"
)

// TestTokenTracker_SessionCacheCumulative — pre-fix: only per-call cache
// fields existed (lastCacheCreate / lastCacheRead). /cost couldn't surface
// session-cumulative cache numbers, leaving a blind spot when the user
// wanted to see how much of the bill was actually cache-discounted.
// Post-fix: add() also accumulates into cacheCreate / cacheRead totals.
func TestTokenTracker_SessionCacheCumulative(t *testing.T) {
	var tt tokenTracker
	tt.add(1000, 200, 500, 8000)
	tt.add(800, 150, 0, 12000)
	tt.add(2000, 100, 300, 0)

	if got := tt.CacheCreate(); got != 800 {
		t.Errorf("session CacheCreate = %d, want 800 (500+0+300)", got)
	}
	if got := tt.CacheRead(); got != 20000 {
		t.Errorf("session CacheRead = %d, want 20000 (8000+12000+0)", got)
	}
	// Last-call fields reflect ONLY the most recent add().
	if got := tt.LastCacheCreate(); got != 300 {
		t.Errorf("LastCacheCreate = %d, want 300 (third add only)", got)
	}
	if got := tt.LastCacheRead(); got != 0 {
		t.Errorf("LastCacheRead = %d, want 0 (third add only)", got)
	}
}

// TestTokenTracker_ResetClearsSessionCache — /clear must zero session
// cumulative cache too, otherwise /cost would carry forward a stale
// pre-clear cache total.
func TestTokenTracker_ResetClearsSessionCache(t *testing.T) {
	var tt tokenTracker
	tt.add(5000, 300, 1000, 15000)
	if tt.CacheCreate() == 0 || tt.CacheRead() == 0 {
		t.Fatal("precondition: cache totals should be non-zero before Reset")
	}
	tt.Reset()
	if got := tt.CacheCreate(); got != 0 {
		t.Errorf("after Reset CacheCreate = %d, want 0", got)
	}
	if got := tt.CacheRead(); got != 0 {
		t.Errorf("after Reset CacheRead = %d, want 0", got)
	}
}

// TestRenderCost_ShowsCacheWhenPresent — when the session has used
// prompt caching, /cost surfaces the cache rows so the user can see
// where the spend actually went. Without cache, those rows stay hidden
// to keep the box tight for non-Anthropic providers.
func TestRenderCost_ShowsCacheWhenPresent(t *testing.T) {
	m := minimalModel(200000)
	m.totalTokens.add(5000, 300, 1000, 15000)

	out := stripANSI(renderCost(m))
	if !strings.Contains(out, "cache_create") {
		t.Errorf("renderCost should surface cache_create row when cache used; got:\n%s", out)
	}
	if !strings.Contains(out, "cache_read") {
		t.Errorf("renderCost should surface cache_read row; got:\n%s", out)
	}
	if !strings.Contains(out, "1,000") {
		t.Errorf("cache_create value (1000) should appear formatted with thousands sep; got:\n%s", out)
	}
}

// TestRenderCost_HidesCacheWhenZero — keep /cost lean for the common
// case (no cache hits). Cache rows only appear when they have signal.
func TestRenderCost_HidesCacheWhenZero(t *testing.T) {
	m := minimalModel(200000)
	m.totalTokens.add(5000, 300, 0, 0)

	out := stripANSI(renderCost(m))
	if strings.Contains(out, "cache_create") || strings.Contains(out, "cache_read") {
		t.Errorf("renderCost should NOT surface cache rows when cache totals are 0; got:\n%s", out)
	}
}

// TestSpinnerRow_SplitsInputOutput — spinner row was previously
// "↓ 95k tokens" which conflated input + output and caused the user to
// confuse it with the bottom-right (input + cache) number. New form:
// "↑ 90k · ↓ 5k tokens" makes the relationship explicit.
func TestSpinnerRow_SplitsInputOutput(t *testing.T) {
	m := minimalModel(200000)
	m.totalTokens.add(90000, 5000, 0, 0)
	m.spinnerActive = true
	m.spinnerStartedAt = m.startTime
	m.spinnerVerb = "thinking"

	out := stripANSI(renderSpinnerStatus(m))
	if !strings.Contains(out, "↑") {
		t.Errorf("spinner row should contain ↑ glyph for input; got:\n%s", out)
	}
	if !strings.Contains(out, "↓") {
		t.Errorf("spinner row should contain ↓ glyph for output; got:\n%s", out)
	}
	// Look for the values (formatTokens still abbreviates here).
	if !strings.Contains(out, "90k") {
		t.Errorf("spinner row should show input value 90k; got:\n%s", out)
	}
	if !strings.Contains(out, "5.0k") && !strings.Contains(out, "5k") {
		t.Errorf("spinner row should show output value (~5k); got:\n%s", out)
	}
}

// TestSpinnerRow_NoTokensYet — pre-first-call render shows no token
// section at all. Without this guard the spinner would render
// "↑ 0 · ↓ 0 tokens" which is just noise.
func TestSpinnerRow_NoTokensYet(t *testing.T) {
	m := minimalModel(200000)
	m.spinnerActive = true
	m.spinnerStartedAt = m.startTime
	m.spinnerVerb = "thinking"
	out := stripANSI(renderSpinnerStatus(m))
	if strings.Contains(out, "tokens") {
		t.Errorf("spinner row should not show 'tokens' before first call; got:\n%s", out)
	}
}
