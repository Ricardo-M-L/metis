package projectcoord

import (
	"testing"
)

func TestInterruptedWorkerIsRequeuedWithinItsAttemptBudget(t *testing.T) {
	workspace := t.TempDir()
	store := NewStore(t.TempDir())
	run, old := singleClaim(t, store, workspace, PhaseCustom, nil, nil)
	interrupted := Failure{Code: "worker_interrupted", Summary: "Service was stopped during work"}
	item, err := store.Fail(workspace, run.ID, old.ID, old.Owner, old.Lease.Token, interrupted, "partial durable evidence")
	if err != nil {
		t.Fatal(err)
	}
	if item.Status != ItemReady || item.Failure == nil || !item.Failure.AutoRecovered || item.Output != "partial durable evidence" {
		t.Fatalf("interrupted work cannot be reclaimed: %+v", item)
	}
	current, err := store.ClaimNext(workspace, run.ID, ClaimOptions{Worker: old.Owner})
	if err != nil {
		t.Fatal(err)
	}
	if current.Attempts != 2 || current.Lease.Token == old.Lease.Token {
		t.Fatal("retry did not fence old worker")
	}
	if _, err := store.Fail(workspace, run.ID, old.ID, old.Owner, old.Lease.Token, interrupted, "stale"); err == nil {
		t.Fatal("old interrupted worker overwrote retry")
	}
	item, err = store.Fail(workspace, run.ID, current.ID, current.Owner, current.Lease.Token, interrupted, "second interruption")
	if err != nil {
		t.Fatal(err)
	}
	if item.Status != ItemFailed || item.Failure.AutoRecovered {
		t.Fatal("interruption bypassed configured attempt cap")
	}
}
