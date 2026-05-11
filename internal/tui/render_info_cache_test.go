package tui

// render_info_cache_test.go — locks the /context "Cache · prompt-
// prefix reuse" block (CACHE-A). Hand-builds a tokenTracker with
// realistic-shape data so the renderer is exercised without booting
// the full TUI / waiting for a real API call.

import (
	"strings"
	"testing"
)

// TestRenderContextCacheBlock_FiresWithRealData — the user-visible
// signal we worked all afternoon to surface: real cache_read tokens
// translated to a "hit X%" string. Verified on 2026-05-11 with live
// metis run against MiniMax: cache_read=6799 / total=7249 = 94% hit.
// This test pins the same payload through the renderer.
func TestRenderContextCacheBlock_FiresWithRealData(t *testing.T) {
	m := &Model{}
	// Simulate a turn where MiniMax served 6799 cached + 10 created
	// + 440 fresh — exact shape of the dump we verified live.
	m.totalTokens.add(440, 67, 10, 6799)

	out := renderContextCacheBlock(m)

	if out == "" {
		t.Fatalf("Cache block should render when cache data exists; got empty")
	}
	if !strings.Contains(out, "Cache") {
		t.Errorf("output should carry the section header; got: %q", out)
	}
	if !strings.Contains(out, "6,799") && !strings.Contains(out, "6799") {
		t.Errorf("output should show the cache_read count; got: %q", out)
	}
	// Hit-rate format: "hit 94%" — anchored on the live-test value.
	if !strings.Contains(out, "94%") && !strings.Contains(out, "93%") {
		t.Errorf("expected hit rate ~94%%; got: %q", out)
	}
}

// TestRenderContextCacheBlock_OmittedWhenNoActivity — a fresh session
// has 0 across the board; the block must not pollute /context with a
// "Cache: 0 tokens (0%)" row before the first API call lands.
func TestRenderContextCacheBlock_OmittedWhenNoActivity(t *testing.T) {
	m := &Model{}
	out := renderContextCacheBlock(m)
	if out != "" {
		t.Errorf("Cache block must be empty before any API activity; got: %q", out)
	}
}

// TestRenderContextCacheBlock_HasThisTurnAndSession — the contract is
// two rows: per-turn (lastIn/lastCacheRead) and session-cumulative
// (in/cacheRead). Pin that both rows render so a future refactor
// can't silently drop one.
func TestRenderContextCacheBlock_HasThisTurnAndSession(t *testing.T) {
	m := &Model{}
	// Two turns: first writes cache (10 create, 0 read), second
	// reads it (6799 read, 0 create). Tracker accumulates session
	// totals; last-* trackers reflect just the second turn.
	m.totalTokens.add(440, 67, 10, 0)
	m.totalTokens.add(440, 67, 0, 6799)

	out := renderContextCacheBlock(m)
	if !strings.Contains(out, "this turn") {
		t.Errorf("missing 'this turn' row; got: %q", out)
	}
	if !strings.Contains(out, "session avg") {
		t.Errorf("missing 'session avg' row; got: %q", out)
	}
}

// TestLastCacheHitRate_NoActivityReturnsZero — defensive: the
// per-turn rate must return 0 on a fresh tracker rather than NaN
// (denominator = 0). Mirrors the session-level CacheHitRate
// safeguard.
func TestLastCacheHitRate_NoActivityReturnsZero(t *testing.T) {
	var tr tokenTracker
	if got := tr.LastCacheHitRate(); got != 0 {
		t.Errorf("LastCacheHitRate on empty tracker should be 0; got %v", got)
	}
}

// TestLastCacheHitRate_Compute — the actual formula. With 6799 cached
// / (6799 + 10 + 440) total = 0.938... → returns ~0.94.
func TestLastCacheHitRate_Compute(t *testing.T) {
	var tr tokenTracker
	tr.add(440, 67, 10, 6799)
	got := tr.LastCacheHitRate()
	if got < 0.93 || got > 0.95 {
		t.Errorf("LastCacheHitRate should be ~0.94; got %v", got)
	}
}
