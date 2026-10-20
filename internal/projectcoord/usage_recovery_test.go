package projectcoord

import (
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"testing"
	"time"
)

func TestUsageLeaseExpiryHoldsUnknownRequestWithoutReplacementAttempt(t *testing.T) {
	f := newUsageLedgerFixture(t, 0)
	record := f.begin(t)
	partial := usageSnapshot(record, TokenUsage{Input: 50}, UsageRequestStarted)
	partial.Sequence = 1
	if _, err := f.store.RecordUsageSnapshot(f.workspace, f.run.ID, partial); err != nil {
		t.Fatal(err)
	}
	before, err := f.store.Get(f.workspace, f.run.ID)
	if err != nil {
		t.Fatal(err)
	}
	*f.now = f.now.Add(2 * time.Minute)
	after, err := f.store.Get(f.workspace, f.run.ID)
	if err != nil {
		t.Fatal(err)
	}
	item := itemByID(t, after, f.item.ID)
	if item.Status != ItemFailed || item.Failure == nil || item.Failure.Code != "provider_usage_unresolved" || item.Failure.Recoverable || item.Failure.AutoRecovered || item.Lease != nil || item.Owner != "" || item.Attempts != 1 {
		t.Fatalf("killed unknown request was returned to queue: %+v", item)
	}
	if !reflect.DeepEqual(after.UsageRequests, before.UsageRequests) || after.SpentUSD != before.SpentUSD || !reflect.DeepEqual(item.Usage, before.Items[0].Usage) {
		t.Fatal("expiry hold changed persisted physical usage evidence")
	}
	if _, err := f.store.ClaimNext(f.workspace, f.run.ID, ClaimOptions{Worker: "replacement"}); !errors.Is(err, ErrNoReadyWork) {
		t.Fatalf("unknown request consumed replacement claim: %v", err)
	}
	if _, err := f.store.Recover(f.workspace, f.run.ID, item.ID, "Attempt before reconciliation", true); !errors.Is(err, ErrUsageUnresolved) {
		t.Fatalf("manual force recovery bypassed unknown accounting: %v", err)
	}
	// Isolated unit receipt evidence; no real provider/account is contacted.
	if _, err := f.store.ImportUsageReceipt(f.workspace, f.run.ID, usageTestReceipt(record, "isolated-test-expiry-settlement", TokenUsage{Input: 75}, ptrUsageUSD(.00015))); err != nil {
		t.Fatal(err)
	}
	after, err = f.store.Get(f.workspace, f.run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if itemByID(t, after, item.ID).Status != ItemFailed {
		t.Fatal("receipt implicitly requeued a failed work item")
	}
	if _, err := f.store.Recover(f.workspace, f.run.ID, item.ID, "Bound receipt settled the previous request", false); err != nil {
		t.Fatal(err)
	}
	replacement, err := f.store.ClaimNext(f.workspace, f.run.ID, ClaimOptions{Worker: "replacement"})
	if err != nil || replacement.Attempts != 2 || replacement.Lease.Token == f.item.Lease.Token {
		t.Fatalf("settlement followed by explicit recovery failed: %+v %v", replacement, err)
	}
}

func TestUsageLeaseExpiryKnownCompletionStillAutoRecovers(t *testing.T) {
	f := newUsageLedgerFixture(t, 0)
	record := f.begin(t)
	if _, err := f.store.RecordUsageSnapshot(f.workspace, f.run.ID, usageSnapshot(record, TokenUsage{Input: 30}, UsageRequestCompleted)); err != nil {
		t.Fatal(err)
	}
	*f.now = f.now.Add(2 * time.Minute)
	after, err := f.store.Get(f.workspace, f.run.ID)
	if err != nil {
		t.Fatal(err)
	}
	item := itemByID(t, after, f.item.ID)
	if item.Status != ItemReady || item.Failure == nil || item.Failure.Code != FailureLeaseExpired || !item.Failure.AutoRecovered {
		t.Fatalf("settled request incorrectly held a safe crash recovery: %+v", item)
	}
	replacement, err := f.store.ClaimNext(f.workspace, f.run.ID, ClaimOptions{Worker: "replacement"})
	if err != nil || replacement.Attempts != 2 {
		t.Fatalf("settled request prevented legitimate reclaim: %+v %v", replacement, err)
	}
}

func TestUsageInterruptedFailureHoldsUntilLateCompletionAndExplicitRecovery(t *testing.T) {
	f := newUsageLedgerFixture(t, 0)
	record := f.begin(t)
	failed, err := f.store.Fail(f.workspace, f.run.ID, f.item.ID, f.item.Owner, f.item.Lease.Token, Failure{Code: FailureWorkerInterrupted, Summary: "Worker stopped", Recoverable: true, AutoRecovered: true}, "persisted partial delivery")
	if err != nil {
		t.Fatal(err)
	}
	if failed.Status != ItemFailed || failed.Failure == nil || failed.Failure.Code != "provider_usage_unresolved" || failed.Failure.Recoverable || failed.Failure.AutoRecovered || failed.Output != "persisted partial delivery" {
		t.Fatalf("interruption was automatically requeued despite unknown usage: %+v", failed)
	}
	if _, err := f.store.Recover(f.workspace, f.run.ID, f.item.ID, "Retry before settlement", false); !errors.Is(err, ErrUsageUnresolved) {
		t.Fatalf("unsettled request passed explicit recovery: %v", err)
	}
	final := usageSnapshot(record, TokenUsage{Input: 25}, UsageRequestCompleted)
	final.Sequence = 1
	if _, err := f.store.RecordUsageSnapshot(f.workspace, f.run.ID, final); err != nil {
		t.Fatal(err)
	}
	if got := itemByID(t, mustUsageAdmissionRun(t, f), f.item.ID); got.Status != ItemFailed || got.Attempts != 1 {
		t.Fatal("late usage automatically spent a retry attempt")
	}
	if _, err := f.store.Recover(f.workspace, f.run.ID, f.item.ID, "Late completion is persisted", false); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.ClaimNext(f.workspace, f.run.ID, ClaimOptions{Worker: "replacement"}); err != nil {
		t.Fatal(err)
	}
}

func TestUsageRecoveryExplicitUnknownCodeCannotClaimAutomaticRecovery(t *testing.T) {
	f := newUsageLedgerFixture(t, 0)
	f.begin(t)
	failed, err := f.store.Fail(f.workspace, f.run.ID, f.item.ID, f.item.Owner, f.item.Lease.Token, Failure{Code: FailureProviderUsageUnresolved, Summary: "Previous model usage is unsettled", Recoverable: true, AutoRecovered: true}, "")
	if err != nil || failed.Status != ItemFailed || failed.Failure.Recoverable || failed.Failure.AutoRecovered {
		t.Fatalf("caller flags represented unknown usage as automatic recovery: %+v %v", failed, err)
	}
}

func TestUsageRecoveryHeldAfterLegacyAutoRequeue(t *testing.T) {
	f := newUsageLedgerFixture(t, 0)
	f.begin(t)
	run := mustUsageAdmissionRun(t, f)
	item := &run.Items[0]
	item.Status, item.Owner, item.Lease = ItemReady, "", nil
	item.Failure = &Failure{Code: FailureLeaseExpired, Recoverable: true, AutoRecovered: true, ObservedAt: *f.now}
	path, err := f.store.runPath(f.workspace, f.run.ID)
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(run)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.ClaimNext(f.workspace, f.run.ID, ClaimOptions{Worker: "replacement"}); !errors.Is(err, ErrNoReadyWork) {
		t.Fatalf("legacy unknown journal consumed a new worker attempt: %v", err)
	}
	after := mustUsageAdmissionRun(t, f)
	got := itemByID(t, after, f.item.ID)
	if got.Status != ItemFailed || got.Failure.Code != "provider_usage_unresolved" || got.Attempts != 1 {
		t.Fatalf("legacy auto-requeue did not migrate to usage hold: %+v", got)
	}
}

func TestUsageRecoveryOperationUncertaintyKeepsPriorityAndEvidence(t *testing.T) {
	f := newUsageLedgerFixture(t, 0)
	record := f.begin(t)
	op, err := f.store.BeginOperation(f.workspace, f.run.ID, f.item.ID, f.item.Owner, f.item.Lease.Token, operationTestInput("isolated writing operation"))
	if err != nil {
		t.Fatal(err)
	}
	*f.now = f.now.Add(2 * time.Minute)
	after := mustUsageAdmissionRun(t, f)
	item := itemByID(t, after, f.item.ID)
	if item.Status != ItemFailed || item.Failure.Code != FailureOperationUncertain || len(after.Operations) != 1 || after.Operations[0].ID != op.ID || after.Operations[0].Status != OperationUncertain || after.UsageRequests[0].ClientRequestID != record.ClientRequestID || after.UsageRequests[0].UsageReported {
		t.Fatal("usage hold erased operation uncertainty or physical evidence")
	}
	if _, err := f.store.Recover(f.workspace, f.run.ID, item.ID, "Recover before both outcomes are known", true); !errors.Is(err, ErrOperationUncertain) {
		t.Fatalf("usage guard replaced prior operation safety check: %v", err)
	}
}

func TestUsageRecoveryOtherActiveItemDoesNotHoldNoRequestWork(t *testing.T) {
	f := newUsageLedgerFixture(t, 0)
	f.begin(t)
	if _, err := f.store.AddWorkItem(f.workspace, f.run.ID, WorkItemInput{ID: "independent", Subject: "Independent work", Prompt: "work"}); err != nil {
		t.Fatal(err)
	}
	other, err := f.store.ClaimNext(f.workspace, f.run.ID, ClaimOptions{Worker: "other", Lease: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	failed, err := f.store.Fail(f.workspace, f.run.ID, other.ID, other.Owner, other.Lease.Token, Failure{Code: FailureWorkerInterrupted, Summary: "Service stopped before a model request"}, "")
	if err != nil || failed.Status != ItemReady || failed.Failure.Code != FailureWorkerInterrupted || !failed.Failure.AutoRecovered {
		t.Fatalf("another item's physical request blocked unrelated recovery: %+v %v", failed, err)
	}
	current, err := f.store.ClaimNext(f.workspace, f.run.ID, ClaimOptions{Worker: "other", Lease: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	*f.now = f.now.Add(2 * time.Minute)
	after := mustUsageAdmissionRun(t, f)
	if got := itemByID(t, after, current.ID); got.Failure.Code == "provider_usage_unresolved" {
		t.Fatal("expired no-request item inherited another item's unknown usage")
	}
}
