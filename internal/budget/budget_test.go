package budget

import (
	"strings"
	"sync"
	"testing"
)

func centRates() Rates {
	// $10 per MTok across all classes — keeps expected math obvious.
	return Rates{InputPerMTok: 10, OutputPerMTok: 10, CacheReadPerMTok: 10, CacheWritePerMTok: 10}
}

func TestAddUsageAccumulates(t *testing.T) {
	tr := NewTracker(1.0, Rates{InputPerMTok: 3, OutputPerMTok: 15})
	tr.AddUsage(1_000_000, 1_000_000, 0, 0)
	if got := tr.SpentUSD(); got != 18 {
		t.Fatalf("SpentUSD = %v, want 18", got)
	}
}

func TestExceededAndWarning(t *testing.T) {
	tr := NewTracker(1.0, centRates()) // $1 cap, $10/MTok
	tr.AddUsage(50_000, 0, 0, 0)       // $0.50
	if tr.Exceeded() {
		t.Fatal("should not exceed at 50%")
	}
	if w := tr.TakeWarning(); w != "" {
		t.Fatalf("no warning expected at 50%%, got %q", w)
	}
	tr.AddUsage(42_000, 0, 0, 0) // $0.92 total — past 90%
	w := tr.TakeWarning()
	if w == "" {
		t.Fatal("warning expected past 90%")
	}
	if !strings.Contains(w, "system-reminder") {
		t.Errorf("warning should be a system-reminder, got %q", w)
	}
	if again := tr.TakeWarning(); again != "" {
		t.Errorf("warning must be one-shot, got %q", again)
	}
	if tr.Exceeded() {
		t.Fatal("not yet exceeded at 92%")
	}
	tr.AddUsage(10_000, 0, 0, 0) // $1.02
	if !tr.Exceeded() {
		t.Fatal("should exceed past cap")
	}
	if msg := tr.ExceededMessage(); !strings.Contains(msg, "budget") {
		t.Errorf("ExceededMessage = %q", msg)
	}
}

// Zero cap = tracking only — never warns, never trips.
func TestZeroCapNeverTrips(t *testing.T) {
	tr := NewTracker(0, centRates())
	tr.AddUsage(100_000_000, 100_000_000, 0, 0)
	if tr.Exceeded() {
		t.Fatal("zero cap must never exceed")
	}
	if w := tr.TakeWarning(); w != "" {
		t.Fatalf("zero cap must never warn, got %q", w)
	}
	if tr.SpentUSD() == 0 {
		t.Fatal("spend should still be tracked")
	}
}

// Unknown pricing (zero rates) must not trip the cap with bogus math.
func TestZeroRatesNeverTrip(t *testing.T) {
	tr := NewTracker(0.01, Rates{})
	tr.AddUsage(100_000_000, 100_000_000, 0, 0)
	if tr.Exceeded() {
		t.Fatal("zero rates must not accumulate spend")
	}
}

// Lazy rates: zero until the source warms up, then cached and applied
// to subsequent usage — mirrors the async catalog warm-up.
func TestLazyRatesResolveAfterWarmup(t *testing.T) {
	warm := false
	tr := NewTrackerLazy(1.0, func() (Rates, bool) {
		if !warm {
			return Rates{}, false
		}
		return Rates{InputPerMTok: 10}, true
	})
	tr.AddUsage(1_000_000, 0, 0, 0) // pre-warmup — $0
	if got := tr.SpentUSD(); got != 0 {
		t.Fatalf("pre-warmup spend = %v, want 0", got)
	}
	warm = true
	tr.AddUsage(1_000_000, 0, 0, 0) // post-warmup — $10
	if got := tr.SpentUSD(); got != 10 {
		t.Fatalf("post-warmup spend = %v, want 10", got)
	}
	if !tr.Exceeded() {
		t.Fatal("cap should trip once real rates land")
	}
	// fn must not be consulted again after resolution.
	warm = false
	tr.AddUsage(1_000_000, 0, 0, 0)
	if got := tr.SpentUSD(); got != 20 {
		t.Fatalf("cached rates should persist; spend = %v, want 20", got)
	}
}

// A final MISS (catalog warm, model unpriced) must also stop the
// re-resolution — no full-catalog rescan per round-trip forever.
func TestLazyFinalMissStopsRescan(t *testing.T) {
	calls := 0
	tr := NewTrackerLazy(1.0, func() (Rates, bool) {
		calls++
		return Rates{}, true // warm catalog, model not priced — final
	})
	tr.AddUsage(1, 0, 0, 0)
	tr.AddUsage(1, 0, 0, 0)
	tr.AddUsage(1, 0, 0, 0)
	if calls != 1 {
		t.Fatalf("ratesFn called %d times after final miss, want 1", calls)
	}
	if tr.Exceeded() {
		t.Fatal("unpriced model must never trip the cap")
	}
}

// nil receiver is safe everywhere — loops without budgets pass nil.
func TestNilTrackerSafe(t *testing.T) {
	var tr *Tracker
	tr.AddUsage(1, 2, 3, 4)
	if tr.Exceeded() || tr.TakeWarning() != "" || tr.SpentUSD() != 0 || tr.ExceededMessage() != "" {
		t.Fatal("nil tracker must be inert")
	}
}

// Parent + concurrent sub-agents share one tracker.
func TestConcurrentAddUsage(t *testing.T) {
	tr := NewTracker(0, centRates())
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tr.AddUsage(1_000_000, 0, 0, 0) // $10 each
		}()
	}
	wg.Wait()
	if got := tr.SpentUSD(); got != 500 {
		t.Fatalf("SpentUSD = %v, want 500", got)
	}
}
