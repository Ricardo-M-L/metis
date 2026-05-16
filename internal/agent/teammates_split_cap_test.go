package agent

import (
	"errors"
	"strconv"
	"strings"
	"testing"
)

// TestRoster_SplitCap_NamedFullStillAllowsAnon — named pool at capacity
// must not block new anonymous registrations. Direct regression cover
// for the 2026-05-14 split: pre-split, 5 anon explorations occupied
// the single combined budget and locked out the next named reviewer.
// Post-split the two pools are independent.
func TestRoster_SplitCap_NamedFullStillAllowsAnon(t *testing.T) {
	r := NewRoster(2, 5) // 2 named, 5 anon
	if err := r.Register(&Teammate{Name: "alice"}); err != nil {
		t.Fatalf("alice (1st named): %v", err)
	}
	if err := r.Register(&Teammate{Name: "bob"}); err != nil {
		t.Fatalf("bob (2nd named): %v", err)
	}
	// Named full → next named must reject.
	if err := r.Register(&Teammate{Name: "carol"}); !errors.Is(err, ErrCapacityExceeded) {
		t.Errorf("3rd named should hit ErrCapacityExceeded; got %v", err)
	}
	// Anon path still wide open — register 3 anons, all succeed.
	for i := 0; i < 3; i++ {
		if err := r.Register(&Teammate{}); err != nil {
			t.Errorf("anon spawn #%d should succeed with named-full but anon-open; got %v", i+1, err)
		}
	}
}

// TestRoster_SplitCap_AnonFullStillAllowsNamed — symmetric inverse.
func TestRoster_SplitCap_AnonFullStillAllowsNamed(t *testing.T) {
	r := NewRoster(5, 2) // 5 named, 2 anon
	// Fill the anon pool.
	for i := 0; i < 2; i++ {
		if err := r.Register(&Teammate{}); err != nil {
			t.Fatalf("anon #%d: %v", i+1, err)
		}
	}
	// Anon full → next anon must reject.
	if err := r.Register(&Teammate{}); !errors.Is(err, ErrCapacityExceeded) {
		t.Errorf("3rd anon should hit ErrCapacityExceeded; got %v", err)
	}
	// Named path still open.
	if err := r.Register(&Teammate{Name: "alice"}); err != nil {
		t.Errorf("named spawn should succeed with anon-full but named-open; got %v", err)
	}
}

// TestRoster_SplitCap_ZeroMeansUnlimited — passing 0 for either kind
// disables that cap. Important for tests and for power users who
// want one side unbounded (e.g., named=10, anon=0=unlimited).
func TestRoster_SplitCap_ZeroMeansUnlimited(t *testing.T) {
	r := NewRoster(0, 0)
	for i := 0; i < 100; i++ {
		if err := r.Register(&Teammate{}); err != nil {
			t.Fatalf("anon #%d failed when cap should be unlimited: %v", i+1, err)
		}
	}
	if r.Capacity() != 0 {
		t.Errorf("Capacity() must report 0 (unlimited) when both kinds 0; got %d", r.Capacity())
	}
}

// TestRoster_AutoSuffix_99Exhaustion — the runaway case: 99 collisions
// on the same base name returns the strict error (no infinite loop).
func TestRoster_AutoSuffix_99Exhaustion(t *testing.T) {
	r := NewRoster(0, 0)
	// Register alice, alice-2 … alice-99 manually.
	if err := r.Register(&Teammate{Name: "alice"}); err != nil {
		t.Fatal(err)
	}
	for i := 2; i <= 99; i++ {
		tm := &Teammate{Name: "alice", StrictName: true} // strict so each iter doesn't auto-suffix
		// Cheat: we want each iteration to land on a specific name,
		// so we set Name to the exact slot we want occupied.
		tm.Name = "alice-" + strconv.Itoa(i)
		tm.StrictName = false
		if err := r.Register(tm); err != nil {
			t.Fatalf("preload alice-%d: %v", i, err)
		}
	}
	// 100th attempt at "alice" auto-suffix path has nothing free —
	// must return ErrNameInUse with the "99 suffixes exhausted" note.
	err := r.Register(&Teammate{Name: "alice"})
	if !errors.Is(err, ErrNameInUse) {
		t.Errorf("100th alice should hit ErrNameInUse after 99 exhaustion; got %v", err)
	}
	if err == nil || !strings.Contains(err.Error(), "99 suffixes exhausted") {
		t.Errorf("error should mention the exhaustion; got %v", err)
	}
}
