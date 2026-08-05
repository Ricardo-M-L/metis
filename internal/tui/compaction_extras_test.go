package tui

import (
	"strings"
	"testing"
	"time"
)

// TestCompactionExtras_LayoutMatchesImage19 — when spinnerOverride is
// the "Compacting conversation..." label, renderSpinnerStatus must emit
// three rows: the spinner verb line, a monotonic progress bar, and
// a sub-line announcing the auto-window threshold and configure command.
//
// There is intentionally no percentage: compaction progress events carry
// cumulative output bytes, but no final byte count from which to derive a
// truthful percentage.
func TestCompactionExtras_LayoutMatchesImage19(t *testing.T) {
	m := minimalModel(200_000)
	m.spinnerActive = true
	m.compactionStartedAt = time.Now().Add(-2 * time.Second)
	m.spinnerVerb = "thinking" // would normally render
	m.spinnerOverride = "Compacting conversation..."

	out := stripANSI(renderSpinnerStatus(m))

	// Row 1: the override verb wins over the thinking verb.
	if !strings.Contains(out, "Compacting conversation...") {
		t.Fatalf("expected literal 'Compacting conversation...'; got:\n%s", out)
	}
	if strings.Contains(out, "thinking") {
		t.Fatalf("override should suppress the generic thinking verb; got:\n%s", out)
	}

	// Row 2: filled + empty progress glyphs.
	if !strings.Contains(out, "▰") {
		t.Fatalf("expected filled progress glyph ▰; got:\n%s", out)
	}
	if !strings.Contains(out, "▱") {
		t.Fatalf("expected empty progress glyph ▱; got:\n%s", out)
	}
	// No percentage: the bar is an estimate, not measured completion.
	if strings.Contains(out, "%") {
		t.Fatalf("estimated progress bar must not show a percentage; got:\n%s", out)
	}

	// Row 3: literal sub-line announcing the auto-window threshold and
	// the configure command (matches CC image #19).
	if !strings.Contains(out, "└ Compacting at auto window (170k tokens) · /autocompact to configure") {
		t.Fatalf("expected sub-line with 170k auto window; got:\n%s", out)
	}
}

// TestCompactionExtras_NotShownOutsideCompaction — when spinnerOverride
// is empty (regular thinking turn), the progress bar + sub-line must
// not appear. Regression guard for accidentally bleeding the compaction
// chrome into every spinner frame.
func TestCompactionExtras_NotShownOutsideCompaction(t *testing.T) {
	m := minimalModel(200_000)
	m.spinnerActive = true
	m.spinnerStartedAt = m.startTime
	m.spinnerVerb = "thinking"

	out := stripANSI(renderSpinnerStatus(m))
	if strings.Contains(out, "▰") || strings.Contains(out, "▱") {
		t.Fatalf("non-compaction spinner must not render progress bar; got:\n%s", out)
	}
	if strings.Contains(out, "auto window") {
		t.Fatalf("non-compaction spinner must not render auto-window sub-line; got:\n%s", out)
	}
}

func TestCompactionProgressCells_MonotonicAndCapped(t *testing.T) {
	const width = 22
	previous := 0
	for elapsed := -time.Second; elapsed <= 20*time.Second; elapsed += 40 * time.Millisecond {
		filled := compactionProgressCells(elapsed, width)
		if filled < previous {
			t.Fatalf("progress moved backward at %v: %d → %d", elapsed, previous, filled)
		}
		if filled < 1 || filled > width-2 {
			t.Fatalf("progress at %v = %d, want within [1,%d]", elapsed, filled, width-2)
		}
		previous = filled
	}
	if got := compactionProgressCells(8*time.Second, width); got != width-2 {
		t.Fatalf("progress at cap = %d, want %d", got, width-2)
	}
	if got := compactionProgressCells(time.Hour, width); got != width-2 {
		t.Fatalf("progress after a long wait = %d, want stable cap %d", got, width-2)
	}
}

// The rendered bar must grow from the left and then hold before 100%;
// it must never become a moving block or sweep back toward the start.
func TestCompactionExtras_GrowsLeftToRightThenWaits(t *testing.T) {
	renderAt := func(elapsed time.Duration) string {
		m := minimalModel(200_000)
		m.spinnerActive = true
		m.compactionStartedAt = time.Now().Add(-elapsed)
		m.spinnerOverride = "Compacting conversation..."
		return stripANSI(renderCompactionExtras(m))
	}

	early := renderAt(time.Second)
	later := renderAt(4 * time.Second)
	waiting := renderAt(20 * time.Second)
	if strings.Count(later, "▰") <= strings.Count(early, "▰") {
		t.Fatalf("bar did not grow left-to-right:\n--- early ---\n%s\n--- later ---\n%s", early, later)
	}
	if !strings.Contains(waiting, strings.Repeat("▰", 20)+strings.Repeat("▱", 2)) {
		t.Fatalf("waiting bar must hold at 20/22 cells, got:\n%s", waiting)
	}
}
