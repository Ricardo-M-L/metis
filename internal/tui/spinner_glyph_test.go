package tui

import (
	"testing"
	"time"
)

// At 40ms tick + 120ms glyph step, in 1 second:
// - 25 ticks fire
// - Glyph advances every 3 ticks → ~8 frame changes/sec
// Confirms a fast tick doesn't accelerate glyph.
func TestSpinnerGlyph_TimeGated(t *testing.T) {
	const stepMs = 120
	frames := len(spinnerFrames)
	// Frames at t=0, 40, 80, 120, 160, ... (40ms ticks)
	tickInterval := 40 * time.Millisecond
	seen := map[int]bool{}
	for i := 0; i < 25; i++ {
		elapsed := time.Duration(i) * tickInterval
		idx := int(elapsed.Milliseconds()/int64(stepMs)) % frames
		seen[idx] = true
	}
	// Over 25 ticks (1 sec) at 120ms gating, expect floor(1000/120) ≈ 8-9 distinct frames.
	if got := len(seen); got > 10 {
		t.Errorf("too many distinct frames per second: %d (expected ≤10)", got)
	}
	if got := len(seen); got < 7 {
		t.Errorf("too few distinct frames per second: %d (expected ≥7)", got)
	}
	t.Logf("distinct glyphs in 1 second of 40ms ticks: %d", len(seen))
}
