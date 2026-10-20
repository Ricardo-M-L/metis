package projectcoord

import (
	"strconv"
	"testing"
	"time"
)

func TestUsagePhysicalHTTPStatusIsBoundWithoutInventingUsage(t *testing.T) {
	for _, code := range []int{200, 403} {
		t.Run(strconv.Itoa(code), func(t *testing.T) {
			f := newUsageLedgerFixture(t, 0)
			record := f.begin(t)
			if record.ResponseStatusCode != 0 || record.UsageReported {
				t.Fatal("unobserved response was assigned an HTTP status or usage")
			}
			input := usageSnapshot(record, TokenUsage{}, UsageRequestStarted)
			input.UsageReported = false
			input.Sequence, input.ResponseStatusCode = 1, code
			updated, err := f.store.RecordUsageSnapshot(f.workspace, f.run.ID, input)
			if err != nil {
				t.Fatal(err)
			}
			if updated.ResponseStatusCode != code || updated.UsageReported || updated.BudgetUSD != 0 {
				t.Fatal("HTTP metadata invented accounting evidence")
			}
			*f.now = f.now.Add(time.Second)
			replayed, err := f.store.RecordUsageSnapshot(f.workspace, f.run.ID, input)
			if err != nil || replayed.UpdatedAt != updated.UpdatedAt {
				t.Fatalf("HTTP observation replay was not idempotent: %+v %v", replayed, err)
			}
			input.Sequence = 2
			input.ResponseStatusCode = 0
			input.Status = UsageRequestInterrupted
			updated, err = f.store.RecordUsageSnapshot(f.workspace, f.run.ID, input)
			if err != nil || updated.ResponseStatusCode != code || updated.UsageReported {
				t.Fatalf("omitted status lost previous identity or invented tokens: %+v %v", updated, err)
			}
			disk, err := NewStore(f.store.Root()).Get(f.workspace, f.run.ID)
			if err != nil || disk.UsageRequests[0].ResponseStatusCode != code || len(UnresolvedUsageRequests(disk)) != 1 {
				t.Fatalf("status was not durably preserved with unknown usage: %+v %v", disk.UsageRequests, err)
			}
			input.Sequence = 3
			input.ResponseStatusCode = 500
			if _, err := f.store.RecordUsageSnapshot(f.workspace, f.run.ID, input); err == nil {
				t.Fatal("a different HTTP response replaced this physical request's status")
			}
		})
	}
}

func TestUsageHTTPStatusRejectsInvalidAndSequencedConflicts(t *testing.T) {
	f := newUsageLedgerFixture(t, 0)
	record := f.begin(t)
	input := usageSnapshot(record, TokenUsage{}, UsageRequestStarted)
	input.UsageReported = false
	input.Sequence = 1
	for _, invalid := range []int{-1, 99, 600, 777} {
		input.ResponseStatusCode = invalid
		if _, err := f.store.RecordUsageSnapshot(f.workspace, f.run.ID, input); err == nil {
			t.Fatalf("invalid HTTP status accepted: %d", invalid)
		}
	}
	input.ResponseStatusCode = 0
	if _, err := f.store.RecordUsageSnapshot(f.workspace, f.run.ID, input); err != nil {
		t.Fatal(err)
	}
	input.ResponseStatusCode = 200
	if _, err := f.store.RecordUsageSnapshot(f.workspace, f.run.ID, input); err == nil {
		t.Fatal("same sequence accepted new status contents")
	}
	input.Sequence = 2
	if _, err := f.store.RecordUsageSnapshot(f.workspace, f.run.ID, input); err != nil {
		t.Fatal(err)
	}
	disk, err := f.store.Get(f.workspace, f.run.ID)
	if err != nil {
		t.Fatal(err)
	}
	bad := cloneRun(disk)
	bad.UsageRequests[0].ResponseStatusCode = 777
	if err := validateUsageLedger(bad); err == nil {
		t.Fatal("invalid persisted HTTP response status accepted")
	}
	bad.UsageRequests[0].ResponseStatusCode = 0
	if err := validateUsageLedger(bad); err != nil {
		t.Fatalf("legacy unknown HTTP status stopped being readable: %v", err)
	}
}
