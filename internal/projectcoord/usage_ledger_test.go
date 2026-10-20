package projectcoord

import (
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"testing"
	"time"
)

type usageLedgerFixture struct {
	store     *Store
	workspace string
	run       Run
	item      WorkItem
	now       *time.Time
}

func newUsageLedgerFixture(t *testing.T, capUSD float64) usageLedgerFixture {
	t.Helper()
	now := time.Now().UTC()
	workspace := t.TempDir()
	store := NewStore(t.TempDir(), withClock(func() time.Time { return now }))
	run, err := store.Create(CreateInput{Workspace: workspace, Goal: "Durable provider usage", MaxBudgetUSD: capUSD, Items: []WorkItemInput{{ID: "work", Subject: "Work", Prompt: "Do the work"}}})
	if err != nil {
		t.Fatal(err)
	}
	item, err := store.ClaimNext(workspace, run.ID, ClaimOptions{Worker: "original", Lease: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	return usageLedgerFixture{store, workspace, run, item, &now}
}

func (f usageLedgerFixture) begin(t *testing.T) UsageRequestRecord {
	t.Helper()
	id, err := NewUsageRequestID()
	if err != nil {
		t.Fatal(err)
	}
	record, err := f.store.BeginUsageRequest(f.workspace, f.run.ID, BeginUsageRequestInput{
		ClientRequestID: id, ItemID: f.item.ID, Worker: f.item.Owner, ClaimToken: f.item.Lease.Token,
		Provider: "selected-provider", EndpointSHA256: strings.Repeat("a", 64), Model: "selected-model", RequestSHA256: strings.Repeat("b", 64),
		Prices: UsagePriceSnapshot{InputPerMillionUSD: 2, OutputPerMillionUSD: 4, CacheReadPerMillionUSD: 0.5, CacheWritePerMillionUSD: 2.5, Source: "catalogue-test-snapshot"},
	})
	if err != nil {
		t.Fatal(err)
	}
	return record
}

func usageSnapshot(record UsageRequestRecord, usage TokenUsage, status UsageRequestStatus) UsageSnapshotInput {
	return UsageSnapshotInput{ClientRequestID: record.ClientRequestID, ItemID: record.ItemID, Worker: record.Worker, ClaimToken: record.ClaimToken, ProviderRequestID: record.ProviderRequestID, ResponseID: record.ResponseID, Usage: usage, UsageReported: true, Status: status}
}

func usageTestReceipt(record UsageRequestRecord, id string, usage TokenUsage, actual *float64) UsageReceipt {
	return UsageReceipt{ReceiptID: id, EvidenceSHA256: strings.Repeat("c", 64), Authority: "operator-imported-provider-account-export", Provider: record.Provider, EndpointSHA256: record.EndpointSHA256, Model: record.Model, ClientRequestID: record.ClientRequestID, ProviderRequestID: record.ProviderRequestID, ResponseID: record.ResponseID, Usage: usage, ActualUSD: actual}
}

func usageAssertUSD(t *testing.T, got, want float64) {
	t.Helper()
	if math.Abs(got-want) > 1e-10 {
		t.Fatalf("USD = %.12f, want %.12f", got, want)
	}
}

func TestUsageRequestIsPersistedBeforeDispatchAndIdentityCannotBeReused(t *testing.T) {
	f := newUsageLedgerFixture(t, 0)
	record := f.begin(t)
	if len(record.ClientRequestID) != 32 {
		t.Fatal("provider client ID does not have 128 bits")
	}
	after, err := NewStore(f.store.Root()).Get(f.workspace, f.run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := UnresolvedUsageRequests(after); len(got) != 1 || got[0].Status != UsageRequestStarted || got[0].Usage != (TokenUsage{}) || got[0].EstimatedUSD != 0 || got[0].ActualUSD != nil {
		t.Fatalf("unknown request usage was lost or invented: %+v", got)
	}
	if _, err := f.store.BeginUsageRequest(f.workspace, f.run.ID, record.BeginUsageRequestInput); err == nil {
		t.Fatal("duplicate dispatch reservation accepted")
	}
	*f.now = f.now.Add(2 * time.Minute)
	id, _ := NewUsageRequestID()
	input := record.BeginUsageRequestInput
	input.ClientRequestID = id
	if _, err := f.store.BeginUsageRequest(f.workspace, f.run.ID, input); err == nil {
		t.Fatal("expired lease dispatched a new provider request")
	}
	if _, err := f.store.Renew(f.workspace, f.run.ID, record.ItemID, record.Worker, record.ClaimToken, time.Minute); err == nil {
		t.Fatal("expired lease revived by accounting")
	}
}

func TestUsageSnapshotsAreAbsoluteAndCacheReclassificationDoesNotDoubleCount(t *testing.T) {
	f := newUsageLedgerFixture(t, 0)
	record := f.begin(t)
	first := usageSnapshot(record, TokenUsage{Input: 100_000, Output: 20_000, CacheRead: 200_000, CacheWrite: 30_000}, UsageRequestStarted)
	updated, err := f.store.RecordUsageSnapshot(f.workspace, f.run.ID, first)
	if err != nil {
		t.Fatal(err)
	}
	usageAssertUSD(t, updated.EstimatedUSD, 0.455)
	*f.now = f.now.Add(time.Second)
	repeated, err := f.store.RecordUsageSnapshot(f.workspace, f.run.ID, first)
	if err != nil || repeated.UpdatedAt != updated.UpdatedAt {
		t.Fatalf("identical snapshot was not a no-op: %+v %v", repeated, err)
	}
	// All 330k input tokens remain the same; 99k previously uncached tokens
	// are now disclosed as cache reads. The latest estimate replaces the
	// original charge in both the request and all aggregate budgets.
	classified := first
	classified.Usage.Input, classified.Usage.CacheRead = 1_000, 299_000
	classified.Status = UsageRequestCompleted
	updated, err = f.store.RecordUsageSnapshot(f.workspace, f.run.ID, classified)
	if err != nil {
		t.Fatal(err)
	}
	usageAssertUSD(t, updated.EstimatedUSD, 0.3065)
	usageAssertUSD(t, updated.BudgetUSD, 0.3065)
	after, err := f.store.Get(f.workspace, f.run.ID)
	if err != nil {
		t.Fatal(err)
	}
	usageAssertUSD(t, after.SpentUSD, 0.3065)
	usageAssertUSD(t, after.Items[0].Usage[0].RequestSpentUSD, 0.3065)
	if len(UnresolvedUsageRequests(after)) != 0 {
		t.Fatal("observed completed request remained unresolved")
	}
	for _, lower := range []TokenUsage{{Input: 0, CacheRead: 299_000, CacheWrite: 30_000, Output: 20_000}, {Input: 1_000, CacheRead: 299_000, CacheWrite: 30_000, Output: 19_999}} {
		classified.Usage = lower
		if _, err := f.store.RecordUsageSnapshot(f.workspace, f.run.ID, classified); err == nil {
			t.Fatal("decreasing absolute token snapshot accepted")
		}
	}
}

func TestUsageLateSnapshotAndReceiptStayOnOriginalExpiredAttempt(t *testing.T) {
	f := newUsageLedgerFixture(t, 0)
	record := f.begin(t)
	*f.now = f.now.Add(2 * time.Minute)
	if _, err := f.store.ClaimNext(f.workspace, f.run.ID, ClaimOptions{Worker: "replacement"}); !errors.Is(err, ErrNoReadyWork) {
		t.Fatalf("unknown physical request was automatically reclaimed: %v", err)
	}
	input := usageSnapshot(record, TokenUsage{Input: 100_000}, UsageRequestInterrupted)
	if _, err := f.store.RecordUsageSnapshot(f.workspace, f.run.ID, input); err != nil {
		t.Fatal(err)
	}
	cost := 0.9
	updated, err := f.store.ImportUsageReceipt(f.workspace, f.run.ID, usageTestReceipt(record, "late-receipt", TokenUsage{Input: 200_000}, &cost))
	if err != nil {
		t.Fatal(err)
	}
	if updated.Status != UsageRequestReconciled || updated.ClaimToken != record.ClaimToken {
		t.Fatal("receipt lost request identity")
	}
	if _, err := f.store.Recover(f.workspace, f.run.ID, record.ItemID, "Late accounting settled the old physical request", false); err != nil {
		t.Fatal(err)
	}
	current, err := f.store.ClaimNext(f.workspace, f.run.ID, ClaimOptions{Worker: "replacement"})
	if err != nil {
		t.Fatal(err)
	}
	// Replaying the old receipt cannot change the replacement's ownership.
	if _, err := f.store.ImportUsageReceipt(f.workspace, f.run.ID, usageTestReceipt(record, "late-receipt", TokenUsage{Input: 200_000}, &cost)); err != nil {
		t.Fatal(err)
	}
	after, err := f.store.Get(f.workspace, f.run.ID)
	if err != nil {
		t.Fatal(err)
	}
	item := itemByID(t, after, record.ItemID)
	if item.Lease == nil || item.Lease.Token != current.Lease.Token {
		t.Fatal("late accounting changed replacement worker ownership")
	}
	usageAssertUSD(t, item.Usage[0].SpentUSD, 0.9)
	usageAssertUSD(t, item.Usage[1].SpentUSD, 0)
	usageAssertUSD(t, after.SpentUSD, 0.9)
	if _, err := f.store.Renew(f.workspace, f.run.ID, record.ItemID, record.Worker, record.ClaimToken, time.Minute); err == nil {
		t.Fatal("receipt let old worker renew its expired lease")
	}
}

func TestUsageReceiptsAreAtomicAndIdempotentAcrossConcurrentStores(t *testing.T) {
	f := newUsageLedgerFixture(t, 0)
	record := f.begin(t)
	cost := 0.7
	receipt := usageTestReceipt(record, "same-export-row", TokenUsage{Input: 100_000, Output: 10_000}, &cost)
	var group sync.WaitGroup
	errors := make(chan error, 16)
	for range 16 {
		group.Add(1)
		go func() {
			defer group.Done()
			_, err := NewStore(f.store.Root()).ImportUsageReceipt(f.workspace, f.run.ID, receipt)
			errors <- err
		}()
	}
	group.Wait()
	close(errors)
	for err := range errors {
		if err != nil {
			t.Fatal(err)
		}
	}
	after, err := f.store.Get(f.workspace, f.run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(after.UsageRequests[0].Receipts) != 1 {
		t.Fatal("same receipt was appended repeatedly")
	}
	usageAssertUSD(t, after.SpentUSD, cost)
	usageAssertUSD(t, after.UsageRequests[0].EstimatedUSD, 0.24)
	for _, change := range []func(*UsageReceipt){
		func(r *UsageReceipt) { r.EvidenceSHA256 = strings.Repeat("d", 64) },
		func(r *UsageReceipt) { r.Provider = "other" },
		func(r *UsageReceipt) { r.Usage.Input++ },
		func(r *UsageReceipt) { value := 0.8; r.ActualUSD = &value },
	} {
		conflict := cloneUsageReceipt(receipt)
		change(&conflict)
		if _, err := f.store.ImportUsageReceipt(f.workspace, f.run.ID, conflict); err == nil {
			t.Fatal("same receipt ID with conflicting evidence was accepted")
		}
	}
}

func TestUsageReceiptScopeAndPreviouslyRecordedRemoteIDsAreRequired(t *testing.T) {
	f := newUsageLedgerFixture(t, 0)
	record := f.begin(t)
	receipt := usageTestReceipt(record, "scope", TokenUsage{Input: 1}, nil)
	for i, change := range []func(*UsageReceipt){
		func(r *UsageReceipt) { r.Provider = "another-account" },
		func(r *UsageReceipt) { r.EndpointSHA256 = strings.Repeat("d", 64) },
		func(r *UsageReceipt) { r.Model = "another-model" },
		func(r *UsageReceipt) { r.ClientRequestID = strings.Repeat("e", 32) },
		func(r *UsageReceipt) { r.ClientRequestID = ""; r.ResponseID = "never-observed" },
	} {
		wrong := receipt
		wrong.ReceiptID = fmt.Sprintf("wrong-scope-%d", i)
		change(&wrong)
		if _, err := f.store.ImportUsageReceipt(f.workspace, f.run.ID, wrong); err == nil {
			t.Fatalf("receipt matched an unrelated request: %+v", wrong)
		}
	}
	snapshot := usageSnapshot(record, TokenUsage{Input: 1}, UsageRequestInterrupted)
	snapshot.ProviderRequestID = "provider-request-1"
	updated, err := f.store.RecordUsageSnapshot(f.workspace, f.run.ID, snapshot)
	if err != nil {
		t.Fatal(err)
	}
	receipt = usageTestReceipt(updated, "remote-id", TokenUsage{Input: 20}, nil)
	receipt.ClientRequestID = ""
	receipt.ResponseID = "response-revealed-on-receipt"
	resolved, err := f.store.ImportUsageReceipt(f.workspace, f.run.ID, receipt)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.ResponseID != receipt.ResponseID {
		t.Fatal("receipt could not teach the response ID through an already known request ID")
	}
}

func TestUsageAccountingDoesNotDoubleCountLegacyMainStreamAndSideRequests(t *testing.T) {
	f := newUsageLedgerFixture(t, 0)
	record := f.begin(t)
	if err := f.store.RecordSpend(f.workspace, f.run.ID, f.item.ID, f.item.Owner, f.item.Lease.Token, 0.3); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.RecordUsageSnapshot(f.workspace, f.run.ID, usageSnapshot(record, TokenUsage{Input: 150_000}, UsageRequestCompleted)); err != nil {
		t.Fatal(err)
	}
	side := f.begin(t)
	if _, err := f.store.RecordUsageSnapshot(f.workspace, f.run.ID, usageSnapshot(side, TokenUsage{Input: 50_000}, UsageRequestCompleted)); err != nil {
		t.Fatal(err)
	}
	if err := f.store.RecordSpend(f.workspace, f.run.ID, f.item.ID, f.item.Owner, f.item.Lease.Token, 0.35); err != nil {
		t.Fatal("main-stream cumulative amount was compared to combined request total:", err)
	}
	after, err := f.store.Get(f.workspace, f.run.ID)
	if err != nil {
		t.Fatal(err)
	}
	usageAssertUSD(t, after.SpentUSD, 0.4)
	usageAssertUSD(t, after.Items[0].Usage[0].LegacySpentUSD, 0.35)
	usageAssertUSD(t, after.Items[0].Usage[0].RequestSpentUSD, 0.4)
	if err := f.store.RecordSpend(f.workspace, f.run.ID, f.item.ID, f.item.Owner, f.item.Lease.Token, 0.34); err == nil {
		t.Fatal("legacy cumulative spend decreased")
	}
}

func TestUsageReceiptActualCostStopsNextRequestWithoutPretendingEstimateIsBill(t *testing.T) {
	f := newUsageLedgerFixture(t, 0.5)
	record := f.begin(t)
	cost := 0.6
	updated, err := f.store.ImportUsageReceipt(f.workspace, f.run.ID, usageTestReceipt(record, "actual-over-cap", TokenUsage{Input: 10_000}, &cost))
	if err != nil {
		t.Fatal(err)
	}
	usageAssertUSD(t, updated.EstimatedUSD, 0.02)
	usageAssertUSD(t, *updated.ActualUSD, 0.6)
	usageAssertUSD(t, updated.BudgetUSD, 0.6)
	after, err := f.store.Get(f.workspace, f.run.ID)
	if err != nil {
		t.Fatal(err)
	}
	item := itemByID(t, after, record.ItemID)
	if after.Status != RunBlocked || item.Failure == nil || item.Failure.Code != "task_budget_exceeded" {
		t.Fatalf("actual receipt cost did not enforce persisted cap: %+v", item)
	}
	input := record.BeginUsageRequestInput
	input.ClientRequestID, _ = NewUsageRequestID()
	if _, err := f.store.BeginUsageRequest(f.workspace, f.run.ID, input); err == nil {
		t.Fatal("request dispatched after receipt exhausted budget")
	}
}

func TestUsageValidationRejectsNegativeNonFiniteAndOverflowingSnapshots(t *testing.T) {
	f := newUsageLedgerFixture(t, 0)
	record := f.begin(t)
	for _, usage := range []TokenUsage{{Input: -1}, {Output: -1}, {CacheRead: -1}, {CacheWrite: -1}, {Input: int(^uint(0) >> 1), CacheRead: 1}} {
		if _, err := f.store.RecordUsageSnapshot(f.workspace, f.run.ID, usageSnapshot(record, usage, UsageRequestStarted)); err == nil {
			t.Fatal("invalid token usage accepted:", usage)
		}
	}
	for i, actual := range []float64{-1, math.NaN(), math.Inf(1)} {
		if _, err := f.store.ImportUsageReceipt(f.workspace, f.run.ID, usageTestReceipt(record, fmt.Sprintf("invalid-cost-%d", i), TokenUsage{}, &actual)); err == nil {
			t.Fatal("invalid receipt cost accepted:", actual)
		}
	}
	for _, bad := range []float64{-1, math.NaN(), math.Inf(1)} {
		input := record.BeginUsageRequestInput
		input.ClientRequestID, _ = NewUsageRequestID()
		input.Prices.InputPerMillionUSD = bad
		if _, err := f.store.BeginUsageRequest(f.workspace, f.run.ID, input); err == nil {
			t.Fatal("invalid catalogue snapshot accepted")
		}
	}
	input := record.BeginUsageRequestInput
	input.ClientRequestID, _ = NewUsageRequestID()
	input.Prices.OutputPerMillionUSD = math.MaxFloat64
	if _, err := f.store.RecordUsageSnapshot(f.workspace, f.run.ID, usageSnapshot(record, TokenUsage{}, UsageRequestCompleted)); err != nil {
		t.Fatal(err)
	}
	overflow, err := f.store.BeginUsageRequest(f.workspace, f.run.ID, input)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.RecordUsageSnapshot(f.workspace, f.run.ID, usageSnapshot(overflow, TokenUsage{Output: 2}, UsageRequestCompleted)); err == nil {
		t.Fatal("estimate overflow accepted")
	}
}

func TestUsageSnapshotsCannotChangeOwnershipRemoteIdentityOrResolvedState(t *testing.T) {
	f := newUsageLedgerFixture(t, 0)
	record := f.begin(t)
	base := usageSnapshot(record, TokenUsage{Input: 10}, UsageRequestStarted)
	base.ResponseID = "response-1"
	if _, err := f.store.RecordUsageSnapshot(f.workspace, f.run.ID, base); err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(*UsageSnapshotInput){
		func(r *UsageSnapshotInput) { r.ItemID = "another-item" },
		func(r *UsageSnapshotInput) { r.Worker = "another-worker" },
		func(r *UsageSnapshotInput) { r.ClaimToken = "another-claim" },
		func(r *UsageSnapshotInput) { r.ResponseID = "another-response" },
		func(r *UsageSnapshotInput) { r.Status = UsageRequestReconciled },
	} {
		wrong := base
		change(&wrong)
		if _, err := f.store.RecordUsageSnapshot(f.workspace, f.run.ID, wrong); err == nil {
			t.Fatal("unbound/authoritative snapshot accepted:", wrong)
		}
	}
	receipt := usageTestReceipt(record, "resolved", TokenUsage{Input: 20}, nil)
	if _, err := f.store.ImportUsageReceipt(f.workspace, f.run.ID, receipt); err != nil {
		t.Fatal(err)
	}
	base.Usage.Input = 30
	base.Status = UsageRequestCompleted
	if _, err := f.store.RecordUsageSnapshot(f.workspace, f.run.ID, base); err == nil {
		t.Fatal("local snapshot rewrote authoritative reconciled usage")
	}
}

func TestUsageReceiptsRequireOrderedCorrectionAndUseAuthoritativeCost(t *testing.T) {
	f := newUsageLedgerFixture(t, 0)
	record := f.begin(t)
	if _, err := f.store.RecordUsageSnapshot(f.workspace, f.run.ID, usageSnapshot(record, TokenUsage{Input: 100_000}, UsageRequestInterrupted)); err != nil {
		t.Fatal(err)
	}
	cost := 0.15
	first := usageTestReceipt(record, "first", TokenUsage{CacheRead: 100_000}, &cost)
	updated, err := f.store.ImportUsageReceipt(f.workspace, f.run.ID, first)
	if err != nil {
		t.Fatal(err)
	}
	usageAssertUSD(t, updated.EstimatedUSD, 0.05)
	usageAssertUSD(t, updated.BudgetUSD, 0.15)
	for _, receipt := range []UsageReceipt{usageTestReceipt(record, "lower-tokens", TokenUsage{CacheRead: 99_999}, nil), usageTestReceipt(record, "lower-cost", TokenUsage{CacheRead: 100_000}, ptrUsageUSD(0.14))} {
		if _, err := f.store.ImportUsageReceipt(f.workspace, f.run.ID, receipt); err == nil {
			t.Fatal("lower cumulative receipt accepted")
		}
	}
	second := usageTestReceipt(record, "second", TokenUsage{CacheRead: 200_000}, ptrUsageUSD(0.3))
	if _, err := f.store.ImportUsageReceipt(f.workspace, f.run.ID, second); err != nil {
		t.Fatal(err)
	}
	after, err := NewStore(f.store.Root()).Get(f.workspace, f.run.ID)
	if err != nil {
		t.Fatal(err)
	}
	usageAssertUSD(t, after.SpentUSD, 0.3)
	if len(after.UsageRequests[0].Receipts) != 2 {
		t.Fatal("receipt evidence was overwritten")
	}
}

func ptrUsageUSD(value float64) *float64 { return &value }

func TestUsageLedgerReadValidationAndDeepClone(t *testing.T) {
	f := newUsageLedgerFixture(t, 0)
	record := f.begin(t)
	receipt := usageTestReceipt(record, "clone", TokenUsage{Input: 100}, ptrUsageUSD(0.1))
	if _, err := f.store.ImportUsageReceipt(f.workspace, f.run.ID, receipt); err != nil {
		t.Fatal(err)
	}
	after, err := f.store.Get(f.workspace, f.run.ID)
	if err != nil {
		t.Fatal(err)
	}
	clone := cloneRun(after)
	*clone.UsageRequests[0].ActualUSD = 900
	*clone.UsageRequests[0].Receipts[0].ActualUSD = 999
	usageAssertUSD(t, *after.UsageRequests[0].ActualUSD, 0.1)
	usageAssertUSD(t, *after.UsageRequests[0].Receipts[0].ActualUSD, 0.1)
	for _, change := range []func(*Run){
		func(r *Run) { r.UsageRequests[0].ClaimToken = "unowned" },
		func(r *Run) { r.UsageRequests[0].EstimatedUSD = 99 },
		func(r *Run) { r.UsageRequests[0].Receipts[0].Provider = "unrelated" },
		func(r *Run) { r.Items[0].Usage[0].RequestSpentUSD = 99 },
		func(r *Run) { *r.UsageRequests[0].ActualUSD = 0.09 },
		func(r *Run) { r.UsageRequests = append(r.UsageRequests, cloneUsageRequest(r.UsageRequests[0])) },
	} {
		bad := cloneRun(after)
		change(&bad)
		if err := validateUsageLedger(bad); err == nil {
			t.Fatal("invalid persisted ledger accepted")
		}
	}
}

func TestUsageHeaderOnlySnapshotsPreserveUsageAndUnknownCompletionCanBeReconciled(t *testing.T) {
	f := newUsageLedgerFixture(t, 0)
	record := f.begin(t)
	first := usageSnapshot(record, TokenUsage{Input: 50}, UsageRequestStarted)
	if _, err := f.store.RecordUsageSnapshot(f.workspace, f.run.ID, first); err != nil {
		t.Fatal(err)
	}
	header := usageSnapshot(record, TokenUsage{}, UsageRequestStarted)
	header.UsageReported, header.ResponseID = false, "late-response-id"
	updated, err := f.store.RecordUsageSnapshot(f.workspace, f.run.ID, header)
	if err != nil {
		t.Fatal(err)
	}
	if !updated.UsageReported || updated.Usage.Input != 50 || updated.ResponseID != header.ResponseID {
		t.Fatalf("identity-only observation erased earlier usage: %+v", updated)
	}
	// A different work item may independently hold unknown usage while
	// this item's first physical request is still active.
	if _, err := f.store.AddWorkItem(f.workspace, f.run.ID, WorkItemInput{ID: "unknown", Subject: "Unknown usage", Prompt: "work"}); err != nil {
		t.Fatal(err)
	}
	other, err := f.store.ClaimNext(f.workspace, f.run.ID, ClaimOptions{Worker: "other"})
	if err != nil {
		t.Fatal(err)
	}
	otherFixture := f
	otherFixture.item = other
	unknown := otherFixture.begin(t)
	completion := usageSnapshot(unknown, TokenUsage{}, UsageRequestCompleted)
	completion.UsageReported = false
	updated, err = f.store.RecordUsageSnapshot(f.workspace, f.run.ID, completion)
	if err != nil {
		t.Fatal(err)
	}
	if updated.UsageReported {
		t.Fatal("missing usage field was represented as reported zero usage")
	}
	after, err := f.store.Get(f.workspace, f.run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(UnresolvedUsageRequests(after)) != 2 {
		t.Fatal("completed response without usage escaped reconciliation queue")
	}
	updated, err = f.store.ImportUsageReceipt(f.workspace, f.run.ID, usageTestReceipt(unknown, "authoritative-zero", TokenUsage{}, ptrUsageUSD(0)))
	if err != nil {
		t.Fatal(err)
	}
	if !updated.UsageReported || updated.Status != UsageRequestReconciled || updated.ActualUSD == nil || *updated.ActualUSD != 0 {
		t.Fatal("authoritative zero usage/cost did not resolve missing observation")
	}
}

func TestUsageBeginRechecksLeaseAfterPostMutationReconciliation(t *testing.T) {
	f := newUsageLedgerFixture(t, 0)
	id, _ := NewUsageRequestID()
	input := BeginUsageRequestInput{ClientRequestID: id, ItemID: f.item.ID, Worker: f.item.Owner, ClaimToken: f.item.Lease.Token, Provider: "selected", EndpointSHA256: strings.Repeat("a", 64), Model: "model", RequestSHA256: strings.Repeat("b", 64), Prices: UsagePriceSnapshot{Source: "test"}}
	clockCalls := 0
	store := NewStore(f.store.Root(), withClock(func() time.Time {
		clockCalls++
		if clockCalls >= 3 {
			return f.item.Lease.ExpiresAt.Add(time.Nanosecond)
		}
		return f.item.Lease.ExpiresAt.Add(-time.Nanosecond)
	}))
	if _, err := store.BeginUsageRequest(f.workspace, f.run.ID, input); err == nil {
		t.Fatal("callback's stale claim authorized provider network I/O")
	}
	// Reservation remains as unknown evidence (a crash can occur between
	// reservation and dispatch), while the old claim cannot dispatch again.
	after, err := store.Get(f.workspace, f.run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := itemByID(t, after, f.item.ID); got.Lease != nil || got.Status != ItemFailed || got.Failure.Code != FailureProviderUsageUnresolved {
		t.Fatal("expired reservation was not held during the transaction")
	}
	if len(UnresolvedUsageRequests(after)) != 1 || after.SpentUSD != 0 {
		t.Fatal("undispatched reservation was charged or lost")
	}
}

func TestUsageReceiptOverflowCannotPersistHalfTransactionDuringLeaseRecovery(t *testing.T) {
	f := newUsageLedgerFixture(t, 0)
	first := f.begin(t)
	maxCost := math.MaxFloat64
	if _, err := f.store.ImportUsageReceipt(f.workspace, f.run.ID, usageTestReceipt(first, "finite-largest", TokenUsage{}, &maxCost)); err != nil {
		t.Fatal(err)
	}
	second := f.begin(t)
	*f.now = f.now.Add(2 * time.Minute)
	if _, err := f.store.ImportUsageReceipt(f.workspace, f.run.ID, usageTestReceipt(second, "aggregate-overflow", TokenUsage{}, &maxCost)); err == nil {
		t.Fatal("infinite aggregate receipt cost accepted")
	}
	after, err := f.store.Get(f.workspace, f.run.ID)
	if err != nil {
		t.Fatal("failed mutation wrote inconsistent request/attempt totals:", err)
	}
	if after.UsageRequests[1].Status != UsageRequestStarted || after.UsageRequests[1].ActualUSD != nil || len(after.UsageRequests[1].Receipts) != 0 || after.SpentUSD != math.MaxFloat64 {
		t.Fatal("failed accounting transaction saved partial receipt evidence")
	}
	if got := itemByID(t, after, f.item.ID); got.Status != ItemFailed || got.Lease != nil || got.Failure.Code != FailureProviderUsageUnresolved {
		t.Fatal("automatic expiry usage hold was lost on failed import")
	}
}

func TestUsageRejectedWithoutReportedUsageStaysUnknownUntilReceipt(t *testing.T) {
	f := newUsageLedgerFixture(t, 0)
	record := f.begin(t)
	rejection := usageSnapshot(record, TokenUsage{}, UsageRequestRejected)
	rejection.UsageReported = false
	updated, err := f.store.RecordUsageSnapshot(f.workspace, f.run.ID, rejection)
	if err != nil {
		t.Fatal(err)
	}
	if updated.UsageReported || updated.ActualUSD != nil {
		t.Fatal("HTTP rejection without usage was represented as known zero billing")
	}
	after, err := f.store.Get(f.workspace, f.run.ID)
	if err != nil {
		t.Fatal(err)
	}
	unknown := UnresolvedUsageRequests(after)
	if len(unknown) != 1 || unknown[0].ClientRequestID != record.ClientRequestID || unknown[0].Status != UsageRequestRejected {
		t.Fatal("unknown upstream consumption after gateway rejection escaped reconciliation")
	}
	// The zero charge is asserted by bound provider evidence, not inferred
	// from an HTTP status or the absence of a token counter in its response.
	updated, err = f.store.ImportUsageReceipt(f.workspace, f.run.ID, usageTestReceipt(record, "rejection-authoritative-zero", TokenUsage{}, ptrUsageUSD(0)))
	if err != nil {
		t.Fatal(err)
	}
	if !updated.UsageReported || updated.Status != UsageRequestReconciled || updated.ActualUSD == nil || *updated.ActualUSD != 0 {
		t.Fatal("authoritative zero receipt did not resolve unknown rejected request")
	}
	after, err = f.store.Get(f.workspace, f.run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(UnresolvedUsageRequests(after)) != 0 {
		t.Fatal("reconciled rejected request remained in unknown usage queue")
	}
}
