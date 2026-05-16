package tui

import (
	"strings"
	"testing"
	"time"
)

// TestCompactionExtras_LayoutMatchesImage19 — when spinnerOverride is
// the "Compacting conversation..." label, renderSpinnerStatus must emit
// three rows: the spinner verb line, a progress bar with %, and a
// sub-line announcing the auto-window threshold and configure command.
// Format mirrors claude-code's image #19 (user feedback 2026-05-16).
func TestCompactionExtras_LayoutMatchesImage19(t *testing.T) {
	m := minimalModel(200_000)
	m.spinnerActive = true
	m.spinnerStartedAt = m.startTime
	m.spinnerVerb = "thinking" // would normally render
	m.spinnerOverride = "Compacting conversation..."
	m.spinnerCompactionBytes = 4000 // → ~50% on the 8000-byte heuristic

	out := stripANSI(renderSpinnerStatus(m))

	// Row 1: the override verb wins over the thinking verb.
	if !strings.Contains(out, "Compacting conversation...") {
		t.Fatalf("expected literal 'Compacting conversation...'; got:\n%s", out)
	}
	if strings.Contains(out, "thinking") {
		t.Fatalf("override should suppress the generic thinking verb; got:\n%s", out)
	}

	// Row 2: filled progress glyph + a percentage. The exact % depends
	// on bytes/80 saturation; assert it lies in 1–99 inclusive so we
	// don't fight rounding but still catch a "0%" or "100%" regression.
	if !strings.Contains(out, "▰") {
		t.Fatalf("expected filled progress glyph ▰; got:\n%s", out)
	}
	if !strings.Contains(out, "▱") {
		t.Fatalf("expected empty progress glyph ▱; got:\n%s", out)
	}
	if !strings.Contains(out, "%") {
		t.Fatalf("expected percentage in output; got:\n%s", out)
	}
	// 4000 / 80 = 50.
	if !strings.Contains(out, "50%") {
		t.Fatalf("expected 50%% from 4000 bytes; got:\n%s", out)
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
	m.spinnerCompactionBytes = 4000 // would render 50% IF compacting

	out := stripANSI(renderSpinnerStatus(m))
	if strings.Contains(out, "▰") || strings.Contains(out, "▱") {
		t.Fatalf("non-compaction spinner must not render progress bar; got:\n%s", out)
	}
	if strings.Contains(out, "auto window") {
		t.Fatalf("non-compaction spinner must not render auto-window sub-line; got:\n%s", out)
	}
}

// TestCompactionExtras_SaturatesAt95 — bytes well past the 8KB target
// must not roll the bar over to 100% (we never know the actual finish
// until EventContextCompacted fires; a 100% bar that keeps spinning
// would feel broken).
func TestCompactionExtras_SaturatesAt95(t *testing.T) {
	m := minimalModel(200_000)
	m.spinnerActive = true
	m.spinnerStartedAt = m.startTime.Add(-3 * time.Second)
	m.spinnerOverride = "Compacting conversation..."
	m.spinnerCompactionBytes = 100_000 // pathological — way past target

	out := stripANSI(renderSpinnerStatus(m))
	if !strings.Contains(out, "95%") {
		t.Fatalf("expected saturating cap at 95%% for huge byte counts; got:\n%s", out)
	}
	if strings.Contains(out, "100%") {
		t.Fatalf("bar must not show 100%% while call is in flight; got:\n%s", out)
	}
}
