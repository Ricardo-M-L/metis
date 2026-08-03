package tui

import (
	"strings"
	"testing"
	"time"
)

// TestCompactionExtras_LayoutMatchesImage19 — when spinnerOverride is
// the "Compacting conversation..." label, renderSpinnerStatus must emit
// three rows: the spinner verb line, an INDETERMINATE sliding bar, and
// a sub-line announcing the auto-window threshold and configure command.
//
// C3 (2026-08-02): switched from a percentage bar (bytes/80 capped at
// 95%) to an indeterminate sliding bar. The percentage version was a
// lie — summaries usually finish in 1-3s, the bar visually jumped
// 0→95% between two frames, and the user complained "压缩进度条还没长
// 就过了" (BUG-C). An indeterminate bar telegraphs "work in progress"
// without pretending to track real progress.
func TestCompactionExtras_LayoutMatchesImage19(t *testing.T) {
	m := minimalModel(200_000)
	m.spinnerActive = true
	m.spinnerStartedAt = m.startTime
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

	// Row 2: filled + empty progress glyphs (the sliding block). The
	// exact position depends on time.Since(spinnerStartedAt), so we
	// assert only that BOTH glyphs appear (some block somewhere on
	// the track), not WHERE.
	if !strings.Contains(out, "▰") {
		t.Fatalf("expected filled progress glyph ▰; got:\n%s", out)
	}
	if !strings.Contains(out, "▱") {
		t.Fatalf("expected empty progress glyph ▱; got:\n%s", out)
	}
	// C3: NO percentage in output — indeterminate bar by design.
	if strings.Contains(out, "%") {
		t.Fatalf("C3 indeterminate bar must not show a percentage; got:\n%s", out)
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

// TestCompactionExtras_SlidingBlockMoves — the indeterminate bar's
// block position must change over time. If the bar froze at one spot,
// the user would think compaction stalled. We snapshot the bar at two
// different spinnerStartedAt values and assert the rendered string
// differs (i.e. the block has moved along the track).
func TestCompactionExtras_SlidingBlockMoves(t *testing.T) {
	m1 := minimalModel(200_000)
	m1.spinnerActive = true
	m1.spinnerStartedAt = time.Now() // NOT m1.startTime — that's zero, collapses t=0/t=1s
	m1.spinnerOverride = "Compacting conversation..."
	out1 := stripANSI(renderSpinnerStatus(m1))

	// Advance start time by ~1 second — should land the block at a
	// different position on the track.
	m2 := minimalModel(200_000)
	m2.spinnerActive = true
	m2.spinnerStartedAt = time.Now().Add(-1 * time.Second)
	m2.spinnerOverride = "Compacting conversation..."
	out2 := stripANSI(renderSpinnerStatus(m2))

	if out1 == out2 {
		t.Fatalf("indeterminate bar should move as time advances; got identical output:\n--- t=0 ---\n%s\n--- t=1s ---\n%s", out1, out2)
	}
}
