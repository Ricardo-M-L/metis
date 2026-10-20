package projectcoord

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func usageAdmissionNextInput(t *testing.T, record UsageRequestRecord, item WorkItem) BeginUsageRequestInput {
	t.Helper()
	input := record.BeginUsageRequestInput
	id, err := NewUsageRequestID()
	if err != nil {
		t.Fatal(err)
	}
	input.ClientRequestID, input.ItemID, input.Worker, input.ClaimToken = id, item.ID, item.Owner, item.Lease.Token
	return input
}

func usageAssertUnresolvedAdmission(t *testing.T, err error) {
	t.Helper()
	var coded interface{ UsageFailureCode() string }
	if err == nil || !errors.Is(err, ErrUsageUnresolved) || !errors.As(err, &coded) || coded.UsageFailureCode() != "provider_usage_unresolved" {
		t.Fatalf("admission error = %v; want safe provider_usage_unresolved classification", err)
	}
}

func TestUsageAdmissionStopsPhysicalRedispatchAfterUnknownHeaderTimeout(t *testing.T) {
	f := newUsageLedgerFixture(t, 0)
	var posts atomic.Int32
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if posts.Add(1) == 1 {
			select {
			case <-r.Context().Done():
			case <-release:
			}
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()
	defer close(release)
	first := f.begin(t)
	client := &http.Client{Timeout: 200 * time.Millisecond}
	req, err := http.NewRequest(http.MethodPost, server.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp, err := client.Do(req); err == nil {
		resp.Body.Close()
		t.Fatal("fixture provider unexpectedly returned headers before timeout")
	}
	interrupted := usageSnapshot(first, TokenUsage{}, UsageRequestInterrupted)
	interrupted.UsageReported, interrupted.Sequence = false, 1
	if _, err := f.store.RecordUsageSnapshot(f.workspace, f.run.ID, interrupted); err != nil {
		t.Fatal(err)
	}
	_, admissionErr := f.store.BeginUsageRequest(f.workspace, f.run.ID, usageAdmissionNextInput(t, first, f.item))
	if admissionErr == nil {
		// This is the actual failed-soak sequence: once a second reservation
		// succeeds the caller can issue another POST before any receipt exists.
		resp, err := http.Post(server.URL, "application/json", nil)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
	}
	if posts.Load() != 1 {
		t.Fatalf("unknown first request allowed physical redispatch: POST count=%d", posts.Load())
	}
	usageAssertUnresolvedAdmission(t, admissionErr)
	disk, err := NewStore(f.store.Root()).Get(f.workspace, f.run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(disk.UsageRequests) != 1 || disk.UsageRequests[0].UsageReported || disk.UsageRequests[0].ActualUSD != nil {
		t.Fatal("blocked dispatch reserved a request or invented zero billing")
	}
}

func TestUsageAdmissionUsesUnresolvedLifecycleRatherThanTokenSize(t *testing.T) {
	for _, tc := range []struct {
		name     string
		status   UsageRequestStatus
		reported bool
		usage    TokenUsage
		blocked  bool
	}{
		{"started_unknown", UsageRequestStarted, false, TokenUsage{}, true},
		{"started_partial", UsageRequestStarted, true, TokenUsage{Input: 10}, true},
		{"started_zero", UsageRequestStarted, true, TokenUsage{}, true},
		{"interrupted_unknown", UsageRequestInterrupted, false, TokenUsage{}, true},
		{"interrupted_partial", UsageRequestInterrupted, true, TokenUsage{Input: 10}, true},
		{"interrupted_zero", UsageRequestInterrupted, true, TokenUsage{}, true},
		{"completed_unknown", UsageRequestCompleted, false, TokenUsage{}, true},
		{"rejected_unknown", UsageRequestRejected, false, TokenUsage{}, true},
		{"completed_reported", UsageRequestCompleted, true, TokenUsage{Input: 10}, false},
		{"completed_explicit_zero", UsageRequestCompleted, true, TokenUsage{}, false},
		{"rejected_explicit_zero", UsageRequestRejected, true, TokenUsage{}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newUsageLedgerFixture(t, 0)
			record := f.begin(t)
			snapshot := usageSnapshot(record, tc.usage, tc.status)
			snapshot.UsageReported, snapshot.Sequence = tc.reported, 1
			if _, err := f.store.RecordUsageSnapshot(f.workspace, f.run.ID, snapshot); err != nil {
				t.Fatal(err)
			}
			input := usageAdmissionNextInput(t, record, f.item)
			input.RequestSHA256 = "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
			input.Provider, input.Model = "another-provider", "another-model"
			input.EndpointSHA256 = "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
			_, err := f.store.BeginUsageRequest(f.workspace, f.run.ID, input)
			if tc.blocked {
				usageAssertUnresolvedAdmission(t, err)
			} else if err != nil {
				t.Fatalf("settled observed request blocked subsequent dispatch: %v", err)
			}
		})
	}
}

func TestUsageAdmissionUnknownAttemptSurvivesTakeoverAndReceiptUnblocks(t *testing.T) {
	f := newUsageLedgerFixture(t, 0)
	record := f.begin(t)
	*f.now = f.now.Add(2 * time.Minute)
	if _, err := f.store.ClaimNext(f.workspace, f.run.ID, ClaimOptions{Worker: "replacement"}); !errors.Is(err, ErrNoReadyWork) {
		t.Fatalf("unsettled killed request consumed a replacement claim: %v", err)
	}
	if _, err := f.store.Recover(f.workspace, f.run.ID, f.item.ID, "Recover before usage settlement", false); !errors.Is(err, ErrUsageUnresolved) {
		t.Fatalf("unsettled receipt passed recovery: %v", err)
	}
	// This receipt is isolated test evidence, never submitted to a real run
	// or used to assert what the timed-out test server consumed.
	if _, err := f.store.ImportUsageReceipt(f.workspace, f.run.ID, usageTestReceipt(record, "isolated-test-account-row", TokenUsage{Input: 10}, ptrUsageUSD(.01))); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.Recover(f.workspace, f.run.ID, f.item.ID, "Bound receipt settled the previous attempt", false); err != nil {
		t.Fatal(err)
	}
	replacement, err := f.store.ClaimNext(f.workspace, f.run.ID, ClaimOptions{Worker: "replacement"})
	if err != nil {
		t.Fatal(err)
	}
	input := usageAdmissionNextInput(t, record, replacement)
	input.RequestSHA256 = "dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"
	if _, err := f.store.BeginUsageRequest(f.workspace, f.run.ID, input); err != nil {
		t.Fatalf("bound receipt failed to unblock current valid claim: %v", err)
	}
	if _, err := f.store.BeginUsageRequest(f.workspace, f.run.ID, record.BeginUsageRequestInput); err == nil {
		t.Fatal("duplicate original request ID dispatched after reconciliation")
	} else {
		var coded interface{ UsageFailureCode() string }
		if errors.As(err, &coded) {
			t.Fatal("duplicate request was misclassified as another unsettled request")
		}
	}
}

func TestUsageAdmissionLegacyClaimStillChecksPriorAttempt(t *testing.T) {
	f := newUsageLedgerFixture(t, 0)
	record := f.begin(t)
	if _, err := f.store.RecordUsageSnapshot(f.workspace, f.run.ID, usageSnapshot(record, TokenUsage{}, UsageRequestCompleted)); err != nil {
		t.Fatal(err)
	}
	*f.now = f.now.Add(2 * time.Minute)
	replacement, err := f.store.ClaimNext(f.workspace, f.run.ID, ClaimOptions{Worker: "replacement"})
	if err != nil {
		t.Fatal(err)
	}
	// Recreate an earlier release's journal that already granted a current
	// live claim while its older physical request was still unsettled. The
	// private unit fixture changes no real run or authoritative receipt.
	if _, err := f.store.withRun(f.workspace, f.run.ID, func(run *Run) (bool, error) {
		previous, _ := usageRequestByID(run, record.ClientRequestID)
		previous.Status, previous.UsageReported = UsageRequestInterrupted, false
		return true, nil
	}); err != nil {
		t.Fatal(err)
	}
	input := usageAdmissionNextInput(t, record, replacement)
	input.RequestSHA256 = "dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"
	_, err = f.store.BeginUsageRequest(f.workspace, f.run.ID, input)
	usageAssertUnresolvedAdmission(t, err)
}

func TestUsageAdmissionLateCompletedSnapshotUnblocksWithoutReceipt(t *testing.T) {
	f := newUsageLedgerFixture(t, 0)
	record := f.begin(t)
	partial := usageSnapshot(record, TokenUsage{Input: 10}, UsageRequestInterrupted)
	partial.Sequence = 1
	if _, err := f.store.RecordUsageSnapshot(f.workspace, f.run.ID, partial); err != nil {
		t.Fatal(err)
	}
	input := usageAdmissionNextInput(t, record, f.item)
	_, err := f.store.BeginUsageRequest(f.workspace, f.run.ID, input)
	usageAssertUnresolvedAdmission(t, err)
	final := usageSnapshot(record, TokenUsage{Input: 8, CacheRead: 2}, UsageRequestCompleted)
	final.Sequence = 2
	if _, err := f.store.RecordUsageSnapshot(f.workspace, f.run.ID, final); err != nil {
		t.Fatal(err)
	}
	usageAssertUSD(t, mustUsageAdmissionRun(t, f).SpentUSD, .000017)
	if _, err := f.store.BeginUsageRequest(f.workspace, f.run.ID, input); err != nil {
		t.Fatalf("late observed completion did not unblock dispatch: %v", err)
	}
	if _, err := f.store.RecordUsageSnapshot(f.workspace, f.run.ID, partial); err == nil {
		t.Fatal("old partial snapshot changed terminal usage after admission")
	}
}

func mustUsageAdmissionRun(t *testing.T, f usageLedgerFixture) Run {
	t.Helper()
	run, err := f.store.Get(f.workspace, f.run.ID)
	if err != nil {
		t.Fatal(err)
	}
	return run
}

func TestUsageAdmissionCompetingReservationsAreAtomic(t *testing.T) {
	f := newUsageLedgerFixture(t, 0)
	// Use a settled request only as a source of the fixture's validated
	// provider, price and attempt identity. It does not hold the admission.
	baseline := f.begin(t)
	if _, err := f.store.RecordUsageSnapshot(f.workspace, f.run.ID, usageSnapshot(baseline, TokenUsage{}, UsageRequestCompleted)); err != nil {
		t.Fatal(err)
	}
	const contenders = 12
	inputs := make([]BeginUsageRequestInput, contenders)
	for i := range inputs {
		inputs[i] = usageAdmissionNextInput(t, baseline, f.item)
	}
	start := make(chan struct{})
	results := make(chan error, contenders)
	var group sync.WaitGroup
	for _, input := range inputs {
		group.Add(1)
		go func() {
			defer group.Done()
			<-start
			_, err := NewStore(f.store.Root()).BeginUsageRequest(f.workspace, f.run.ID, input)
			results <- err
		}()
	}
	close(start)
	group.Wait()
	close(results)
	accepted := 0
	for err := range results {
		if err == nil {
			accepted++
		} else {
			usageAssertUnresolvedAdmission(t, err)
		}
	}
	if accepted != 1 {
		t.Fatalf("atomic provider admission accepted %d competing reservations", accepted)
	}
	disk, err := f.store.Get(f.workspace, f.run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(disk.UsageRequests) != 2 || len(UnresolvedUsageRequests(disk)) != 1 {
		t.Fatal("competing admission left duplicate unresolved requests")
	}
}

func TestUsageAdmissionDoesNotBlockOtherWorkItems(t *testing.T) {
	f := newUsageLedgerFixture(t, 0)
	record := f.begin(t)
	if _, err := f.store.AddWorkItem(f.workspace, f.run.ID, WorkItemInput{ID: "other", Subject: "Other independent work", Prompt: "work"}); err != nil {
		t.Fatal(err)
	}
	other, err := f.store.ClaimNext(f.workspace, f.run.ID, ClaimOptions{Worker: "other-worker"})
	if err != nil {
		t.Fatal(err)
	}
	if other.ID == f.item.ID {
		t.Fatal("fixture did not claim a distinct item")
	}
	if _, err := f.store.BeginUsageRequest(f.workspace, f.run.ID, usageAdmissionNextInput(t, record, other)); err != nil {
		t.Fatalf("unrelated item's unsettled usage blocked independent work: %v", err)
	}
}
