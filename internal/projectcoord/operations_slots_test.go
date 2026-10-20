package projectcoord

import (
	"encoding/json"
	"testing"
	"time"
)

func TestOperationOccurrenceAllowsIntentionalRepeatAndReplaysEachSlot(t *testing.T) {
	f := newOperationFixture(t, 2)
	input := operationTestInput("same command deliberately called twice")
	first := f.begin(t, input) // legacy/default zero means slot one
	if first.Occurrence != 1 {
		t.Fatalf("default occurrence = %d, want one", first.Occurrence)
	}
	firstResult := json.RawMessage(`{"Output":"Receipt for the first deliberate call"}`)
	if _, err := f.store.FinishOperation(f.run.Workspace, f.run.ID, f.item.ID, f.item.Owner, f.item.Lease.Token, first.ID, firstResult); err != nil {
		t.Fatal(err)
	}
	input.Occurrence = 1
	defaultReplay := f.begin(t, input)
	if defaultReplay.ID != first.ID || defaultReplay.Status != OperationCompleted || !sameOperationJSON(defaultReplay.ResultJSON, firstResult) {
		t.Fatalf("explicit slot one changed the legacy/default identity: %+v", defaultReplay)
	}
	input.Occurrence = 2
	second := f.begin(t, input)
	if second.ID == first.ID || second.Status != OperationStarted || second.Occurrence != 2 {
		t.Fatalf("intentional second call reused the first result: %+v", second)
	}
	secondResult := json.RawMessage(`{"Output":"Receipt for the second deliberate call"}`)
	if _, err := f.store.FinishOperation(f.run.Workspace, f.run.ID, f.item.ID, f.item.Owner, f.item.Lease.Token, second.ID, secondResult); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.Fail(f.run.Workspace, f.run.ID, f.item.ID, f.item.Owner, f.item.Lease.Token, Failure{Code: FailureWorkerInterrupted, Summary: "Restart after two completed write calls"}, ""); err != nil {
		t.Fatal(err)
	}
	var err error
	f.item, err = f.store.ClaimNext(f.run.Workspace, f.run.ID, ClaimOptions{Worker: "writer", Lease: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []struct {
		occurrence int
		id         string
		result     json.RawMessage
	}{{1, first.ID, firstResult}, {2, second.ID, secondResult}} {
		input.Occurrence = expected.occurrence
		op := f.begin(t, input)
		if op.ID != expected.id || op.Status != OperationCompleted || op.ExecutionAttempts != 1 || !sameOperationJSON(op.ResultJSON, expected.result) {
			t.Fatalf("recovery did not replay the matching deliberate-call slot: %+v", op)
		}
	}
	if len(f.get(t).Operations) != 2 {
		t.Fatal("slot replay appended new writing operations")
	}
	legacy := f.get(t)
	legacy.Operations[0].Occurrence = 0
	if err := validateOperations(legacy); err != nil {
		t.Fatalf("legacy slot-one ID with occurrence omitted is no longer readable: %v", err)
	}
	legacy.Operations[1].Occurrence = 0
	if err := validateOperations(legacy); err == nil {
		t.Fatal("slot-two identity could be changed to legacy slot one")
	}
}

func TestOperationOccurrenceCannotBypassUnknownWrite(t *testing.T) {
	f := newOperationFixture(t, 2)
	input := operationTestInput("call with unknown outcome")
	f.begin(t, input)
	f.now = f.now.Add(2 * time.Second)
	input.Occurrence = 2
	if _, err := f.store.BeginOperation(f.run.Workspace, f.run.ID, f.item.ID, f.item.Owner, f.item.Lease.Token, input); err == nil {
		t.Fatal("a new occurrence bypassed an unknown remote write")
	}
	if len(f.get(t).Operations) != 1 {
		t.Fatal("unknown operation safety hold appended a new occurrence")
	}
}

func TestOperationOccurrenceRejectsInvalidSlots(t *testing.T) {
	for _, occurrence := range []int{-1} {
		input := operationTestInput("invalid slot")
		input.Occurrence = occurrence
		if _, err := normalizeOperationInput(input); err == nil {
			t.Fatalf("invalid occurrence %d was accepted", occurrence)
		}
	}
}
