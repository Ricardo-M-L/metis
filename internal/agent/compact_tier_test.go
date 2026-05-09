package agent

// compact_tier_test.go pins the seven-bucket window→tier table and
// the tightening-only application semantics of applyTier. The
// motivation per `00-SUMMARY.md` #6 (compressToolHistory): metis's
// 800-char snip cap was a single hard-coded value that bled context
// for small-window OpenAI-compat providers — the user's default
// MiniMax + DeepSeek + Kimi setup all sit in the painful 16-64k
// range.

import "testing"

func TestTierForWindow_Buckets(t *testing.T) {
	cases := []struct {
		window   int
		wantName string
	}{
		{0, "unknown"},
		{-1, "unknown"},
		{8_000, "tier-16k"},
		{16_000, "tier-16k"},
		{16_001, "tier-32k"},
		{32_000, "tier-32k"},
		{50_000, "tier-64k"},
		{64_000, "tier-64k"},
		{100_000, "tier-128k"},
		{128_000, "tier-128k"},
		{180_000, "tier-200k"},
		{200_000, "tier-200k"},
		{500_000, "tier-500k"},
		{1_000_000, "tier-500k"},
	}
	for _, c := range cases {
		t.Run(c.wantName, func(t *testing.T) {
			got := tierForWindow(c.window)
			if got.Name != c.wantName {
				t.Errorf("tierForWindow(%d).Name = %q, want %q", c.window, got.Name, c.wantName)
			}
		})
	}
}

// TestTierForWindow_MonotonicSnipChars — the SnipMaxToolResultChars
// must increase as the window grows (smaller windows = stricter cap).
// Pin so a future tier-table edit doesn't accidentally invert the
// curve.
func TestTierForWindow_MonotonicSnipChars(t *testing.T) {
	prev := 0
	for _, w := range []int{16_000, 32_000, 64_000, 128_000, 200_000, 500_000} {
		t.Run(tierForWindow(w).Name, func(t *testing.T) {
			cur := tierForWindow(w).SnipMaxToolResultChars
			if cur <= prev {
				t.Errorf("tier for window=%d has SnipMaxToolResultChars=%d, "+
					"should be > previous tier's %d", w, cur, prev)
			}
			prev = cur
		})
	}
}

// TestApplyTier_Tightens — applyTier must override defaults when the
// tier specifies stricter values. Default Compactor has
// SnipMaxToolResultChars=800; the 16k tier (200) must take effect.
func TestApplyTier_Tightens(t *testing.T) {
	c := NewCompactor(DefaultCompactionConfig(), "test", 16_000, nil)
	if c.SnipMaxToolResultChars != 800 {
		t.Fatalf("default SnipMaxToolResultChars = %d, expected 800", c.SnipMaxToolResultChars)
	}
	c.ApplyWindowTier(16_000)
	if c.SnipMaxToolResultChars != 200 {
		t.Errorf("after ApplyWindowTier(16k): SnipMaxToolResultChars = %d, want 200",
			c.SnipMaxToolResultChars)
	}
	if c.SnipThreshold != 0.60 {
		t.Errorf("after ApplyWindowTier(16k): SnipThreshold = %v, want 0.60",
			c.SnipThreshold)
	}
}

// TestApplyTier_DoesNotTouchProtectLast — ProtectLast is intentionally
// out of scope for tier overrides (it's a UX setting not a budget
// setting). Pin so a future tier-table edit can't sneak it back in.
func TestApplyTier_DoesNotTouchProtectLast(t *testing.T) {
	c := NewCompactor(DefaultCompactionConfig(), "test", 16_000, nil)
	c.ProtectLast = 8
	c.ApplyWindowTier(16_000)
	if c.ProtectLast != 8 {
		t.Errorf("tier should NOT modify ProtectLast; got %d (was 8)", c.ProtectLast)
	}
}

// TestApplyTier_UnknownIsNoOp — a window of 0 (couldn't determine)
// must NOT change defaults. Otherwise a transient init bug erases
// the user's whole compactor config.
func TestApplyTier_UnknownIsNoOp(t *testing.T) {
	c := NewCompactor(DefaultCompactionConfig(), "test", 0, nil)
	before := c.SnipMaxToolResultChars
	c.ApplyWindowTier(0)
	if c.SnipMaxToolResultChars != before {
		t.Errorf("unknown window shouldn't modify SnipMaxToolResultChars; got %d (was %d)",
			c.SnipMaxToolResultChars, before)
	}
}

// TestApplyTier_LargeWindowLoosens — symmetric: 200k tier should
// loosen the 800-char default to 3000, since a 200k window has
// the budget for richer context.
func TestApplyTier_LargeWindowLoosens(t *testing.T) {
	c := NewCompactor(DefaultCompactionConfig(), "test", 200_000, nil)
	if c.SnipMaxToolResultChars != 800 {
		t.Fatalf("default = %d, want 800", c.SnipMaxToolResultChars)
	}
	c.ApplyWindowTier(200_000)
	if c.SnipMaxToolResultChars != 3000 {
		t.Errorf("after 200k tier: SnipMaxToolResultChars = %d, want 3000",
			c.SnipMaxToolResultChars)
	}
}

// TestApplyTier_Idempotent — applying the same tier twice must
// converge. Otherwise a hot-reload that re-applies the tier could
// mutate values unpredictably.
func TestApplyTier_Idempotent(t *testing.T) {
	c := NewCompactor(DefaultCompactionConfig(), "test", 32_000, nil)
	c.ApplyWindowTier(32_000)
	first := *c
	c.ApplyWindowTier(32_000)
	second := *c
	if first.SnipMaxToolResultChars != second.SnipMaxToolResultChars ||
		first.SnipThreshold != second.SnipThreshold ||
		first.ProtectLast != second.ProtectLast {
		t.Errorf("ApplyWindowTier not idempotent; first=%+v second=%+v", first, second)
	}
}
