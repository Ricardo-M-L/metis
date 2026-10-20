package projectcoord

import (
	"testing"
	"time"
)

func TestUsageOrderedSnapshotCorrectsAbsoluteTotalsAndBudget(t *testing.T) {
	f := newUsageLedgerFixture(t, 0)
	record := f.begin(t)
	first := usageSnapshot(record, TokenUsage{Input: 100_000}, UsageRequestStarted)
	first.Sequence = 1
	if _, err := f.store.RecordUsageSnapshot(f.workspace, f.run.ID, first); err != nil {
		t.Fatal(err)
	}
	final := usageSnapshot(record, TokenUsage{CacheRead: 90_000}, UsageRequestCompleted)
	final.Sequence = 2
	updated, err := f.store.RecordUsageSnapshot(f.workspace, f.run.ID, final)
	if err != nil {
		t.Fatalf("latest provider correction rejected: %v", err)
	}
	usageAssertUSD(t, updated.BudgetUSD, .045)
	after, err := NewStore(f.store.Root()).Get(f.workspace, f.run.ID)
	if err != nil {
		t.Fatal(err)
	}
	usageAssertUSD(t, after.SpentUSD, .045)
	usageAssertUSD(t, after.Items[0].SpentUSD, .045)
	usageAssertUSD(t, after.Items[0].Usage[0].SpentUSD, .045)
	usageAssertUSD(t, after.Items[0].Usage[0].RequestSpentUSD, .045)
	*f.now = f.now.Add(time.Second)
	replayed, err := f.store.RecordUsageSnapshot(f.workspace, f.run.ID, final)
	if err != nil || replayed.UpdatedAt != updated.UpdatedAt {
		t.Fatalf("same final snapshot was not idempotent: %+v %v", replayed, err)
	}
	for _, stale := range []UsageSnapshotInput{first, usageSnapshot(record, TokenUsage{}, UsageRequestCompleted)} {
		stale.Sequence = 3
		if _, err := f.store.RecordUsageSnapshot(f.workspace, f.run.ID, stale); err == nil {
			t.Fatal("completed usage was overwritten by a later observation")
		}
	}
}

func TestUsageOrderedSnapshotsFenceStaleAndConflictingObservations(t *testing.T) {
	f := newUsageLedgerFixture(t, 0)
	record := f.begin(t)
	first := usageSnapshot(record, TokenUsage{Input: 556}, UsageRequestStarted)
	first.Sequence = 1
	if _, err := f.store.RecordUsageSnapshot(f.workspace, f.run.ID, first); err != nil {
		t.Fatal(err)
	}
	latest := usageSnapshot(record, TokenUsage{Input: 179, Output: 74, CacheWrite: 512}, UsageRequestStarted)
	latest.Sequence = 2
	if _, err := f.store.RecordUsageSnapshot(f.workspace, f.run.ID, latest); err != nil {
		t.Fatal(err)
	}
	for _, stale := range []UsageSnapshotInput{first, usageSnapshot(record, TokenUsage{}, UsageRequestStarted)} {
		if _, err := f.store.RecordUsageSnapshot(f.workspace, f.run.ID, stale); err == nil {
			t.Fatal("old or unordered snapshot replaced sequenced usage")
		}
	}
	conflict := latest
	conflict.Usage.Input++
	if _, err := f.store.RecordUsageSnapshot(f.workspace, f.run.ID, conflict); err == nil {
		t.Fatal("same sequence accepted conflicting contents")
	}
	zero := usageSnapshot(record, TokenUsage{}, UsageRequestStarted)
	zero.Sequence = 3
	if _, err := f.store.RecordUsageSnapshot(f.workspace, f.run.ID, zero); err != nil {
		t.Fatalf("explicit zero correction rejected: %v", err)
	}
	after, err := f.store.Get(f.workspace, f.run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.SpentUSD != 0 || !after.UsageRequests[0].UsageReported || after.UsageRequests[0].Usage != (TokenUsage{}) {
		t.Fatal("explicit zero did not refund provider estimate")
	}
}

func TestUsageReceiptCorrectionRequiresCurrentEvidenceSupersession(t *testing.T) {
	f := newUsageLedgerFixture(t, 0)
	record := f.begin(t)
	if _, err := f.store.RecordUsageSnapshot(f.workspace, f.run.ID, usageSnapshot(record, TokenUsage{Input: 100_000}, UsageRequestCompleted)); err != nil {
		t.Fatal(err)
	}
	first := usageTestReceipt(record, "first", TokenUsage{CacheRead: 90_000}, ptrUsageUSD(.04))
	if _, err := f.store.ImportUsageReceipt(f.workspace, f.run.ID, first); err != nil {
		t.Fatalf("authoritative final correction rejected: %v", err)
	}
	second := usageTestReceipt(record, "revised", TokenUsage{CacheRead: 80_000}, ptrUsageUSD(.03))
	if _, err := f.store.ImportUsageReceipt(f.workspace, f.run.ID, second); err == nil {
		t.Fatal("unordered receipt lowered settled evidence")
	}
	second.SupersedesReceiptID = first.ReceiptID
	if _, err := f.store.ImportUsageReceipt(f.workspace, f.run.ID, second); err != nil {
		t.Fatalf("explicit current receipt correction rejected: %v", err)
	}
	if _, err := f.store.ImportUsageReceipt(f.workspace, f.run.ID, first); err != nil {
		t.Fatal("original evidence replay stopped being idempotent")
	}
	stale := usageTestReceipt(record, "stale-revision", TokenUsage{}, ptrUsageUSD(0))
	stale.SupersedesReceiptID = first.ReceiptID
	if _, err := f.store.ImportUsageReceipt(f.workspace, f.run.ID, stale); err == nil {
		t.Fatal("stale receipt supersession replaced newer evidence")
	}
	after, err := NewStore(f.store.Root()).Get(f.workspace, f.run.ID)
	if err != nil {
		t.Fatal(err)
	}
	usageAssertUSD(t, after.SpentUSD, .03)
	if len(after.UsageRequests[0].Receipts) != 2 || after.UsageRequests[0].Usage != second.Usage {
		t.Fatal("corrected evidence history was lost")
	}
}

func TestUsageLegacyHighWaterLedgerLoadsAndMigratesOnLatestWrite(t *testing.T) {
	f := newUsageLedgerFixture(t, 0)
	record := f.begin(t)
	first := usageSnapshot(record, TokenUsage{Input: 100_000}, UsageRequestStarted)
	first.Sequence = 1
	if _, err := f.store.RecordUsageSnapshot(f.workspace, f.run.ID, first); err != nil {
		t.Fatal(err)
	}
	// Reproduce an older persisted cache-classification snapshot: corrected
	// token evidence with its original high-water charged budget.
	if _, err := f.store.withRun(f.workspace, f.run.ID, func(run *Run) (bool, error) {
		stored, _ := usageRequestByID(run, record.ClientRequestID)
		stored.AccountingVersion = 0
		stored.Usage = TokenUsage{CacheRead: 100_000}
		stored.EstimatedUSD = .05
		return true, nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := NewStore(f.store.Root()).Get(f.workspace, f.run.ID); err != nil {
		t.Fatalf("compatible legacy ledger could not be read: %v", err)
	}
	next := usageSnapshot(record, TokenUsage{CacheRead: 90_000}, UsageRequestStarted)
	next.Sequence = 2
	updated, err := f.store.RecordUsageSnapshot(f.workspace, f.run.ID, next)
	if err != nil {
		t.Fatal(err)
	}
	if updated.AccountingVersion != 2 {
		t.Fatal("latest write did not migrate accounting version")
	}
	after, err := NewStore(f.store.Root()).Get(f.workspace, f.run.ID)
	if err != nil {
		t.Fatal(err)
	}
	usageAssertUSD(t, after.SpentUSD, .045)
}

func TestUsageCorrectionPreservesOtherRequestsAndLegacyFloor(t *testing.T) {
	f := newUsageLedgerFixture(t, 0)
	first := f.begin(t)
	if _, err := f.store.RecordUsageSnapshot(f.workspace, f.run.ID, usageSnapshot(first, TokenUsage{Input: 100_000}, UsageRequestCompleted)); err != nil {
		t.Fatal(err)
	}
	second := f.begin(t)
	if _, err := f.store.RecordUsageSnapshot(f.workspace, f.run.ID, usageSnapshot(second, TokenUsage{Input: 100_000}, UsageRequestStarted)); err != nil {
		t.Fatal(err)
	}
	if err := f.store.RecordSpend(f.workspace, f.run.ID, f.item.ID, f.item.Owner, f.item.Lease.Token, .3); err != nil {
		t.Fatal(err)
	}
	// Correction to the earlier settled request requires fixture receipt
	// evidence, while the distinct current physical request remains active.
	if _, err := f.store.ImportUsageReceipt(f.workspace, f.run.ID, usageTestReceipt(first, "isolated-test-refund", TokenUsage{}, nil)); err != nil {
		t.Fatal(err)
	}
	after, err := NewStore(f.store.Root()).Get(f.workspace, f.run.ID)
	if err != nil {
		t.Fatal(err)
	}
	usageAssertUSD(t, after.SpentUSD, .3)
	usageAssertUSD(t, after.Items[0].Usage[0].RequestSpentUSD, .2)
	if after.UsageRequests[1].Usage.Input != 100_000 {
		t.Fatal("one physical request correction erased another request")
	}
}

func TestUsageRefundedLedgerHasExactZeroAggregate(t *testing.T) {
	f := newUsageLedgerFixture(t, 0)
	var records []UsageRequestRecord
	for i := range 12 {
		record := f.begin(t)
		records = append(records, record)
		if _, err := f.store.RecordUsageSnapshot(f.workspace, f.run.ID, usageSnapshot(record, TokenUsage{Input: 50_000 * (i + 1)}, UsageRequestCompleted)); err != nil {
			t.Fatal(err)
		}
	}
	for _, record := range records {
		if _, err := f.store.ImportUsageReceipt(f.workspace, f.run.ID, usageTestReceipt(record, "isolated-test-refund-"+record.ClientRequestID, TokenUsage{}, nil)); err != nil {
			t.Fatal(err)
		}
	}
	after, err := f.store.Get(f.workspace, f.run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.SpentUSD != 0 || after.Items[0].SpentUSD != 0 {
		t.Fatalf("refunded usage left a rounding balance: run=%v item=%v", after.SpentUSD, after.Items[0].SpentUSD)
	}
}
