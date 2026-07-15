package budget

import "testing"

func TestSetRatesResolverChangesFuturePricingWithoutResettingSpend(t *testing.T) {
	tracker := NewTracker(10, Rates{InputPerMTok: 1})
	tracker.AddUsage(1_000_000, 0, 0, 0)

	resolverCalls := 0
	tracker.SetRatesResolver(func() (Rates, bool) {
		resolverCalls++
		return Rates{InputPerMTok: 2}, true
	})
	tracker.AddUsage(1_000_000, 0, 0, 0)
	tracker.AddUsage(1_000_000, 0, 0, 0)

	if got := tracker.SpentUSD(); got != 5 {
		t.Fatalf("spent after model-price switch = %v, want 5", got)
	}
	if resolverCalls != 1 {
		t.Fatalf("final resolver calls = %d, want 1", resolverCalls)
	}
}

func TestSetRatesResolverNilUsesSafeZeroRates(t *testing.T) {
	tracker := NewTracker(10, Rates{InputPerMTok: 5})
	tracker.SetRatesResolver(nil)
	tracker.AddUsage(1_000_000, 0, 0, 0)
	if got := tracker.SpentUSD(); got != 0 {
		t.Fatalf("nil resolver spend = %v, want 0", got)
	}
}
