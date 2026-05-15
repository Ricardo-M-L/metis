package agent

// teammates_recently_finished_test.go pins the Fix 2 (2026-05-15)
// behavior: Unregister moves a teammate into recentlyFinished
// instead of hard-deleting it, so SubAgentOutput / SubAgentList
// callers can still observe its final status briefly after the
// sub-loop exits.

import (
	"strconv"
	"testing"
)

func TestRoster_UnregisterPreservesInRecentlyFinished(t *testing.T) {
	r := NewRoster(5)
	tm := &Teammate{Name: "alice", AgentID: "agt-aaaa1111"}
	if err := r.Register(tm); err != nil {
		t.Fatalf("register: %v", err)
	}

	// Live lookup works.
	if got, ok := r.LookupByAgentID("agt-aaaa1111"); !ok || got != tm {
		t.Fatalf("live lookup: ok=%v got=%v", ok, got)
	}

	r.Unregister("alice")

	// After Unregister, the cap-counted Count() goes back to 0
	// (it's not live anymore).
	if c := r.Count(); c != 0 {
		t.Errorf("Count after Unregister = %d, want 0 (finished doesn't count toward cap)", c)
	}

	// But LookupByAgentID should still find it for SubAgentOutput
	// to read.
	got, ok := r.LookupByAgentID("agt-aaaa1111")
	if !ok {
		t.Fatal("LookupByAgentID lost agent immediately after Unregister — the bug Fix 2 is supposed to fix")
	}
	if got.AgentID != "agt-aaaa1111" {
		t.Errorf("found wrong teammate: %s", got.AgentID)
	}
}

func TestRoster_ListIncludesRecentlyFinished(t *testing.T) {
	r := NewRoster(5)
	live := &Teammate{Name: "live-one", AgentID: "agt-live"}
	finished := &Teammate{Name: "done-one", AgentID: "agt-done"}
	if err := r.Register(live); err != nil {
		t.Fatal(err)
	}
	if err := r.Register(finished); err != nil {
		t.Fatal(err)
	}
	r.Unregister("done-one")

	list := r.List()
	if len(list) != 2 {
		t.Fatalf("expected 2 teammates (1 live + 1 finished); got %d", len(list))
	}
	names := map[string]bool{}
	for _, tm := range list {
		names[tm.Name] = true
	}
	if !names["live-one"] || !names["done-one"] {
		t.Errorf("List dropped a teammate: %v", names)
	}
}

func TestRoster_FinishedLRU_EvictsOldestAtCap(t *testing.T) {
	// Register more than RosterFinishedKeep, Unregister them all,
	// confirm only the most recent K survive.
	r := NewRoster(0) // unlimited cap so we can register a lot
	total := RosterFinishedKeep + 10
	for i := 0; i < total; i++ {
		name := "tm-" + strconv.Itoa(i)
		id := "agt-" + strconv.Itoa(i)
		tm := &Teammate{Name: name, AgentID: id}
		if err := r.Register(tm); err != nil {
			t.Fatalf("register %d: %v", i, err)
		}
		r.Unregister(name)
	}

	// The first 10 should have been evicted; the last RosterFinishedKeep
	// should remain findable.
	for i := 0; i < 10; i++ {
		id := "agt-" + strconv.Itoa(i)
		if _, ok := r.LookupByAgentID(id); ok {
			t.Errorf("expected %s evicted, but still findable", id)
		}
	}
	for i := total - RosterFinishedKeep; i < total; i++ {
		id := "agt-" + strconv.Itoa(i)
		if _, ok := r.LookupByAgentID(id); !ok {
			t.Errorf("expected %s preserved, but not found", id)
		}
	}
}

func TestRoster_FinishedDoesNotCountTowardCap(t *testing.T) {
	// Register cap=2 named teammates, finish them both, then verify
	// we can still spawn 2 more without ErrCapacityExceeded.
	r := NewRoster(2)
	for _, name := range []string{"a", "b"} {
		if err := r.Register(&Teammate{Name: name, AgentID: "agt-" + name}); err != nil {
			t.Fatalf("register %s: %v", name, err)
		}
	}
	// Cap full.
	if err := r.Register(&Teammate{Name: "c", AgentID: "agt-c"}); err == nil {
		t.Fatal("expected ErrCapacityExceeded at cap=2 fully utilized")
	}
	// Finish both.
	r.Unregister("a")
	r.Unregister("b")
	// Now we should be able to spawn 2 more.
	if err := r.Register(&Teammate{Name: "c", AgentID: "agt-c"}); err != nil {
		t.Fatalf("after finishing a+b, should have capacity for c: %v", err)
	}
	if err := r.Register(&Teammate{Name: "d", AgentID: "agt-d"}); err != nil {
		t.Fatalf("after finishing a+b, should have capacity for d: %v", err)
	}
	// And a, b should still be discoverable via LookupByAgentID for polling.
	if _, ok := r.LookupByAgentID("agt-a"); !ok {
		t.Error("agt-a should still be findable in recentlyFinished")
	}
}
