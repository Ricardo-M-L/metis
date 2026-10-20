package budget

import (
	"math"
	"testing"
)

func TestObservedSpendDoesNotDoubleChargeLegacyUsage(t *testing.T) {
	tracker := NewTracker(25, Rates{InputPerMTok: 1})
	var persisted []float64
	tracker.SetPersistence(func(spent float64) error { persisted = append(persisted, spent); return nil })
	tracker.ObserveSpend(10)
	if err := tracker.AddUsage(10_000_000, 0, 0, 0); err != nil {
		t.Fatal(err)
	}
	if tracker.SpentUSD() != 10 || persisted[0] != 10 {
		t.Fatalf("duplicate request accounting: spent=%v persisted=%v", tracker.SpentUSD(), persisted)
	}
	tracker.ObserveSpend(23)
	if err := tracker.AddUsage(5_000_000, 0, 0, 0); err != nil {
		t.Fatal(err)
	}
	if tracker.SpentUSD() != 23 || persisted[1] != 15 {
		t.Fatalf("late receipt and legacy were added: spent=%v persisted=%v", tracker.SpentUSD(), persisted)
	}
	tracker.ObserveSpend(25)
	if !tracker.Exceeded() || tracker.TakeWarning() == "" {
		t.Fatal("durable receipt did not constrain live budget")
	}
	tracker.Reset()
	if tracker.SpentUSD() != 0 || tracker.Exceeded() {
		t.Fatal("reset retained old request budget")
	}
}

func TestObservedSpendRejectsInvalidFloor(t *testing.T) {
	tracker := NewTracker(1, Rates{})
	tracker.ObserveSpend(math.Inf(1))
	tracker.ObserveSpend(math.NaN())
	tracker.ObserveSpend(-1)
	if tracker.SpentUSD() != 0 || tracker.Exceeded() {
		t.Fatal("invalid durable floor changed budget")
	}
}

func TestAbsoluteObservedSpendAllowsCorrectionWithoutErasingLegacyUsage(t *testing.T) {
	tracker := NewTracker(.1, Rates{InputPerMTok: 1})
	tracker.SetObservedSpend(.2)
	if !tracker.Exceeded() {
		t.Fatal("durable estimate did not constrain budget")
	}
	tracker.SetObservedSpend(.04)
	if tracker.Exceeded() || tracker.SpentUSD() != .04 {
		t.Fatal("latest absolute correction retained old high-water spend")
	}
	if err := tracker.AddUsage(50_000, 0, 0, 0); err != nil {
		t.Fatal(err)
	}
	tracker.SetObservedSpend(0)
	if tracker.SpentUSD() != .05 {
		t.Fatal("durable correction erased independent legacy usage")
	}
	for _, invalid := range []float64{-1, math.Inf(1), math.NaN()} {
		tracker.SetObservedSpend(invalid)
	}
	if tracker.SpentUSD() != .05 {
		t.Fatal("invalid absolute projection changed budget")
	}
}
