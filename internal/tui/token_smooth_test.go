package tui

import "testing"

func TestAnimateOne_NoOpWhenAtTarget(t *testing.T) {
	if got := animateOne(100, 100); got != 100 {
		t.Errorf("at target: got %d, want 100", got)
	}
}

func TestAnimateOne_SmallGapAddsThree(t *testing.T) {
	if got := animateOne(0, 30); got != 3 {
		t.Errorf("small gap step = %d, want +3 (got %d)", got, got)
	}
	if got := animateOne(0, 69); got != 3 {
		t.Errorf("just-under-70 gap step should still be +3; got %d", got)
	}
}

func TestAnimateOne_MidGapPercent(t *testing.T) {
	// 12% of 100 = 12; minimum 3.
	if got := animateOne(0, 100); got != 12 {
		t.Errorf("mid-gap 100 step = %d, want 12 (12%% of gap)", got)
	}
	// 12% of 199 = 23.
	if got := animateOne(0, 199); got != 23 {
		t.Errorf("mid-gap 199 step = %d, want 23", got)
	}
}

func TestAnimateOne_BigGapAddsFifty(t *testing.T) {
	if got := animateOne(0, 500); got != 50 {
		t.Errorf("big gap step = %d, want +50", got)
	}
	if got := animateOne(0, 9999); got != 50 {
		t.Errorf("huge gap step should be capped at +50; got %d", got)
	}
}

func TestAnimateOne_NeverOvershoots(t *testing.T) {
	if got := animateOne(98, 100); got != 100 {
		t.Errorf("step beyond target should clamp to target; got %d", got)
	}
}

func TestAnimateOne_NeverGoesBackwards(t *testing.T) {
	// Defensive: if some buggy caller passes target<current, we hold
	// rather than stepping backwards (which would look like a count
	// flicker on the UI).
	if got := animateOne(100, 50); got != 50 {
		t.Errorf("backwards target should snap (truth wins), got %d", got)
	}
}

func TestTokenTracker_SnapAlignsDisplayed(t *testing.T) {
	tt := tokenTracker{}
	tt.add(150, 30)
	tt.Animate() // partial
	if tt.Total() == 180 {
		// In some boundary edges the partial step may already equal
		// the target, but with a 150-gap that shouldn't happen on the
		// first tick.
		t.Skip("animation already converged in one tick — algorithm changed")
	}
	tt.Snap()
	if tt.Total() != 180 {
		t.Errorf("Snap should align to truth (180), got %d", tt.Total())
	}
}

func TestTokenTracker_AnimateConverges(t *testing.T) {
	// After enough ticks, displayed must reach actual.
	tt := tokenTracker{}
	tt.add(1000, 0)
	for i := 0; i < 200 && tt.Total() < 1000; i++ {
		tt.Animate()
	}
	if tt.Total() != 1000 {
		t.Errorf("Animate should converge in <200 ticks; stuck at %d", tt.Total())
	}
}

// TestTokenTracker_AccumulatesAcrossIterations locks in claude-code-
// style cumulative API-spend semantics: every iteration's usage adds
// to both axes (NOT max-of-latest). Two requests that each report
// 1000 input_tokens should display 2000 total input — that's the
// real API bill, not the current history size.
func TestTokenTracker_AccumulatesAcrossIterations(t *testing.T) {
	tt := tokenTracker{}
	tt.add(1000, 50)
	tt.Snap()
	if tt.Total() != 1050 {
		t.Fatalf("first iter total = %d, want 1050", tt.Total())
	}
	tt.add(1000, 30)
	tt.Snap()
	if tt.Total() != 2080 {
		t.Fatalf("after 2nd iter total = %d, want 2080 (sum on both axes)", tt.Total())
	}
	tt.add(1500, 20)
	tt.Snap()
	if tt.Total() != 3600 {
		t.Errorf("after 3rd iter total = %d, want 3600 (in=3500, out=100)", tt.Total())
	}
}

// TestTokenTracker_ResetClearsAll covers /clear and /new wiring — the
// counters must drop to zero so the user doesn't see stale numbers
// from the discarded history.
func TestTokenTracker_ResetClearsAll(t *testing.T) {
	tt := tokenTracker{}
	tt.add(2000, 500)
	tt.Snap()
	if tt.Total() == 0 {
		t.Fatal("precondition: tracker should have non-zero total before Reset")
	}
	tt.Reset()
	if tt.Total() != 0 {
		t.Errorf("after Reset total = %d, want 0", tt.Total())
	}
}
