package budget

import (
	"errors"
	"math"
	"sync"
	"testing"
)

func TestSharedSpendPersistenceIsOrderedAndReturnsFailure(t *testing.T) {
	tracker := NewTracker(1, Rates{InputPerMTok: 1})
	last := 0.0
	tracker.SetPersistence(func(spent float64) error {
		if spent < last {
			t.Error("shared usage writes arrived out of order")
		}
		last = spent
		return nil
	})
	var group sync.WaitGroup
	for i := 0; i < 10; i++ {
		group.Add(1)
		go func() {
			defer group.Done()
			if err := tracker.AddUsage(10000, 0, 0, 0); err != nil {
				t.Error(err)
			}
		}()
	}
	group.Wait()
	if math.Abs(last-0.1) > 1e-9 {
		t.Fatalf("persisted=%f", last)
	}
	want := errors.New("disk failure")
	tracker.SetPersistence(func(float64) error { return want })
	if err := tracker.AddUsage(1, 0, 0, 0); !errors.Is(err, want) {
		t.Fatalf("persistence failure swallowed: %v", err)
	}
}
