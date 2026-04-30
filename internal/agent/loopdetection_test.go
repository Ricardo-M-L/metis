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
