package agent

import (
	"sync/atomic"
	"testing"
)

func TestLoopDetector_GenericRepeatTriggersWarning(t *testing.T) {
	d := NewLoopDetector()
	d.WarningThreshold = 3
	d.CriticalThreshold = 5
	d.GlobalThreshold = 100

	var warnings int32
	var lastKind LoopDetectorKind
	d.OnWarning(func(k LoopDetectorKind, _ int, _ string) {
		atomic.AddInt32(&warnings, 1)
		lastKind = k
	})

	for i := 0; i < 3; i++ {
		d.Record("Read", map[string]any{"path": "/tmp/x"})
	}
	if atomic.LoadInt32(&warnings) == 0 {
		t.Fatal("expected warning at threshold")
	}
	if lastKind != LoopGenericRepeat && lastKind != LoopPollNoProgress {
		t.Errorf("unexpected kind: %s", lastKind)
	}
}

func TestLoopDetector_CriticalAtThreshold(t *testing.T) {
	d := NewLoopDetector()
	d.WarningThreshold = 100
	d.CriticalThreshold = 4
	d.GlobalThreshold = 1000

	var crits int32
	d.OnCritical(func(_ LoopDetectorKind, _ int, _ string) {
		atomic.AddInt32(&crits, 1)
	})

	for i := 0; i < 4; i++ {
		d.Record("Edit", map[string]any{"path": "/x"})
	}
	if atomic.LoadInt32(&crits) == 0 {
		t.Fatal("expected critical at threshold")
	}
}

func TestLoopDetector_GlobalCircuitBreakerAborts(t *testing.T) {
	d := NewLoopDetector()
	d.WarningThreshold = 1000
	d.CriticalThreshold = 1000
	d.GlobalThreshold = 5

	for i := 0; i < 5; i++ {
		d.Record("DiffTool"+itoa(i), map[string]any{})
	}
	if !d.ShouldAbort() {
		t.Fatal("expected ShouldAbort once global threshold reached")
	}
}

func TestLoopDetector_RecordProgressResetsCounts(t *testing.T) {
	d := NewLoopDetector()
	d.WarningThreshold = 10
	d.CriticalThreshold = 100
	d.GlobalThreshold = 100

	for i := 0; i < 5; i++ {
		d.Record("Read", map[string]any{})
	}
	stats := d.Stats()
	if stats.ToolCounts["Read"] != 5 {
		t.Fatalf("expected 5 Read calls, got %d", stats.ToolCounts["Read"])
	}

	d.RecordProgress()
	stats2 := d.Stats()
	if got := stats2.ToolCounts["Read"]; got != 0 {
		t.Errorf("expected per-tool counter reset, got %d", got)
	}
	// Global count is intentionally NOT reset on progress (cumulative).
	if stats2.GlobalCount != 5 {
		t.Errorf("global count should persist across progress, got %d", stats2.GlobalCount)
	}
}

func TestLoopDetector_PingPong(t *testing.T) {
	d := NewLoopDetector()
	d.WarningThreshold = 3
	d.CriticalThreshold = 100
	d.GlobalThreshold = 100

	var fired int32
	var kind LoopDetectorKind
	d.OnWarning(func(k LoopDetectorKind, _ int, _ string) {
		if k == LoopPingPong {
			atomic.AddInt32(&fired, 1)
			kind = k
		}
	})

	// A,B,A,B,A,B — pair "A->B" appears 3 times, threshold 3
	for i := 0; i < 3; i++ {
		d.Record("ToolA", nil)
		d.Record("ToolB", nil)
	}

	if atomic.LoadInt32(&fired) == 0 {
		t.Fatal("expected ping-pong warning")
	}
	if kind != LoopPingPong {
		t.Errorf("unexpected kind: %s", kind)
	}
}

func TestLoopDetector_ToolSeqRingBufferBounded(t *testing.T) {
	d := NewLoopDetector()
	for i := 0; i < 200; i++ {
		d.Record("X", nil)
	}
	if got := len(d.toolSeq); got > 100 {
		t.Errorf("toolSeq should be capped at 100, got %d", got)
	}
}

// Regression for the bug where Loop.Run never called RecordProgress on
// non-tool turns. The detector kept incrementing per-tool counters across
// user turns and false-triggered loop_detected.
func TestLoopDetector_RecordProgressClearsCountersBetweenTurns(t *testing.T) {
	d := NewLoopDetector()
	d.WarningThreshold = 100
	d.CriticalThreshold = 100
	d.GlobalThreshold = 100

	for i := 0; i < 5; i++ {
		d.Record("Read", nil)
	}
	d.RecordProgress() // simulating Loop seeing stop != "tool_use"
	for i := 0; i < 5; i++ {
		d.Record("Read", nil)
	}
	stats := d.Stats()
	// Per-tool counter should reflect only the second batch (5), not the
	// cumulative 10.
	if stats.ToolCounts["Read"] != 5 {
		t.Errorf("expected 5 Reads after progress reset, got %d", stats.ToolCounts["Read"])
	}
	// Global counter, by contrast, is monotonic and should be 10.
	if stats.GlobalCount != 10 {
		t.Errorf("global count should accumulate, got %d", stats.GlobalCount)
	}
}

func TestLoopDetector_NilCallbacksDoNotPanic(t *testing.T) {
	d := NewLoopDetector()
	d.WarningThreshold = 1
	d.CriticalThreshold = 1
	// onWarning and onCritical not set
	d.Record("X", nil)
	d.Record("X", nil)
	// no panic = pass
}

// TestLoopDetector_DefaultGlobalThresholdDisabled — pins the 2026-05-15
// refactor C: NewLoopDetector() now defaults GlobalThreshold to 0 so
// the loop is bounded only by the signature-window detector + the
// progress_detector's diminishing-returns check, NOT a raw tool-call
// count. The 40-minute multi-agent audit task previously died at call
// 81 mid-Phase-2 because the count cap fired before progress_detector
// got a chance.
func TestLoopDetector_DefaultGlobalThresholdDisabled(t *testing.T) {
	d := NewLoopDetector()
	if d.GlobalThreshold != 0 {
		t.Fatalf("NewLoopDetector default GlobalThreshold = %d, want 0", d.GlobalThreshold)
	}

	// 10_000 unique tool calls (no signature repeats) must NOT abort
	// when the count cap is disabled. Earlier code would have aborted
	// at call 80, then 250, then 500.
	for i := 0; i < 10000; i++ {
		// Each call uses a distinct tool name so callCounts never
		// reaches CriticalThreshold and signatureWindow never
		// matches itself (signature looks at result too — none set
		// here so all signatures are empty / skipped).
		d.Record("Tool"+itoa(i), map[string]any{})
	}
	if d.ShouldAbort() {
		t.Errorf("ShouldAbort=true after 10000 distinct calls with Global disabled; the count cap was supposed to be off")
	}
	if d.AbortReason() != "" {
		t.Errorf("AbortReason = %q, want empty (no rule fired)", d.AbortReason())
	}
}

// TestLoopDetector_GlobalOptInStillWorks — when a user explicitly sets
// GlobalThreshold > 0 they get the old runaway-backstop behavior.
// Belt-and-suspenders use case: long batch jobs that want a "kill at
// N tool calls" emergency stop.
func TestLoopDetector_GlobalOptInStillWorks(t *testing.T) {
	d := NewLoopDetector()
	d.GlobalThreshold = 7 // explicit opt-in

	for i := 0; i < 7; i++ {
		d.Record("Tool"+itoa(i), map[string]any{})
	}
	if !d.ShouldAbort() {
		t.Fatal("explicit GlobalThreshold=7 should abort at call 7")
	}
	if d.AbortReason() != LoopGlobalCircuitBreaker {
		t.Errorf("AbortReason = %q, want %q", d.AbortReason(), LoopGlobalCircuitBreaker)
	}
}
