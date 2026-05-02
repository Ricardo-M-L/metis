package tui

import (
	"testing"
)

// TestTokenTracker_ContextUsage_NoCache — no prompt caching: ContextUsage
// equals just the most recent API call's input_tokens. Output is NOT
// included (claude-code statusline parity).
func TestTokenTracker_ContextUsage_NoCache(t *testing.T) {
	var tt tokenTracker
	tt.add(38000, 200, 0, 0)
	if got := tt.ContextUsage(); got != 38000 {
		t.Errorf("ContextUsage with no cache = %d, want 38000 (input only, no output)", got)
	}
	if got := tt.LastTotal(); got != 38200 {
		t.Errorf("LastTotal should still include output: got %d, want 38200", got)
	}
}

// TestTokenTracker_ContextUsage_WithCache — with prompt caching the
// numerator must add cache_creation_input_tokens + cache_read_input_tokens
// to input_tokens, just like claude-code does. Without this, a session
// that hits prompt cache hard would show a low context-load number even
// though the live context is huge.
func TestTokenTracker_ContextUsage_WithCache(t *testing.T) {
	var tt tokenTracker
	// Realistic Anthropic shape: small input_tokens (only the new
	// uncached delta), big cache_read (the bulk of the context).
	tt.add(500, 200, 1000, 30000)
	want := 500 + 1000 + 30000
	if got := tt.ContextUsage(); got != want {
		t.Errorf("ContextUsage with cache = %d, want %d (input + cache_create + cache_read)", got, want)
	}
	// LastTotal stays per-turn (input + output, NO cache).
	if got := tt.LastTotal(); got != 700 {
		t.Errorf("LastTotal should NOT include cache: got %d, want 700", got)
	}
}

// TestTokenTracker_ContextUsage_OverwrittenEachCall — successive add()
// calls overwrite (not accumulate) the per-call counters; ContextUsage
// reflects only the latest call, just like the claude-code statusline.
func TestTokenTracker_ContextUsage_OverwrittenEachCall(t *testing.T) {
	var tt tokenTracker
	tt.add(10000, 100, 0, 5000)
	if got := tt.ContextUsage(); got != 15000 {
		t.Fatalf("after first call: %d, want 15000", got)
	}
	tt.add(20000, 200, 1000, 8000)
	if got := tt.ContextUsage(); got != 29000 {
		t.Errorf("after second call ContextUsage should reflect ONLY 2nd call (20000+1000+8000=29000), got %d", got)
	}
	// Session cumulative still grows.
	if tt.in != 30000 {
		t.Errorf("session cumulative input should be 30000, got %d", tt.in)
	}
}

// TestTokenTracker_ContextUsage_ResetClearsCache — /clear must zero
// the cache fields too, otherwise the bottom-right would still display
// the pre-clear context load.
func TestTokenTracker_ContextUsage_ResetClearsCache(t *testing.T) {
	var tt tokenTracker
	tt.add(5000, 300, 800, 12000)
	if tt.ContextUsage() == 0 {
		t.Fatal("precondition: ContextUsage should be non-zero before Reset")
	}
	tt.Reset()
	if got := tt.ContextUsage(); got != 0 {
		t.Errorf("after Reset ContextUsage = %d, want 0", got)
	}
	if got := tt.LastTotal(); got != 0 {
		t.Errorf("after Reset LastTotal = %d, want 0", got)
	}
}

// TestFormatTokensRaw — bottom-right rendering must show the raw integer,
// not abbreviated. User wants to watch every tick during a turn.
func TestFormatTokensRaw(t *testing.T) {
	cases := []struct {
		in   int
		want string
	}{
		{0, "0"},
		{1, "1"},
		{42, "42"},
		{999, "999"},
		{1000, "1000"},
		{32123, "32123"},
		{38000, "38000"},
		{200000, "200000"},
		{1234567, "1234567"},
	}
	for _, tc := range cases {
		got := formatTokensRaw(tc.in)
		if got != tc.want {
			t.Errorf("formatTokensRaw(%d) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestFormatTokens_KeepsAbbreviationForSpinner — anti-regression. The
// spinner row deliberately still uses k-abbreviation; if a refactor
// accidentally swapped formatTokens for formatTokensRaw globally, this
// test catches it.
func TestFormatTokens_KeepsAbbreviationForSpinner(t *testing.T) {
	if got := formatTokens(38000); got != "38k" {
		t.Errorf("formatTokens(38000) = %q, want %q (spinner row should stay abbreviated)", got, "38k")
	}
	if got := formatTokens(3500); got != "3.5k" {
		t.Errorf("formatTokens(3500) = %q, want %q", got, "3.5k")
	}
	if got := formatTokens(1500000); got != "1.5M" {
		t.Errorf("formatTokens(1500000) = %q, want %q", got, "1.5M")
	}
}
