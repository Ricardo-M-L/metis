package projectcoord

import (
	"context"
	"errors"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestProviderStreamRetryPreservesDeliveryUsageAndFencesOldClaim(t *testing.T) {
	workspace, root := t.TempDir(), t.TempDir()
	prior := []byte("previous accepted delivery\n")
	if err := os.WriteFile(filepath.Join(workspace, "prior.txt"), prior, 0600); err != nil {
		t.Fatal(err)
	}
	store := NewStore(root)
	run, err := store.Create(CreateInput{Workspace: workspace, Goal: "Retry verification only", MaxBudgetUSD: 1, CheckCommands: []string{"exit 0"}, Items: []WorkItemInput{
		{ID: "delivery", Phase: PhaseImplementation, Subject: "Delivery", Prompt: "Existing delivery", Deliverables: []string{"prior.txt"}},
		{ID: "verify", Phase: PhaseVerification, Subject: "Verify", Prompt: "Verify delivery", DependsOn: []string{"delivery"}, MaxAttempts: 2},
	}})
	if err != nil {
		t.Fatal(err)
	}
	delivery, err := store.ClaimNext(workspace, run.ID, ClaimOptions{Worker: "same-worker"})
	if err != nil {
		t.Fatal(err)
	}
	delivery, err = store.Complete(context.Background(), workspace, run.ID, delivery.ID, CompletionInput{Worker: delivery.Owner, Token: delivery.Lease.Token, Output: "Accepted prior delivery"})
	if err != nil {
		t.Fatal(err)
	}
	old, err := store.ClaimNext(workspace, run.ID, ClaimOptions{Worker: "same-worker"})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.RecordSpend(workspace, run.ID, old.ID, old.Owner, old.Lease.Token, 0.2); err != nil {
		t.Fatal(err)
	}
	interruption := Failure{Code: "provider_stream_interrupted", Summary: "Provider ended before completion"}
	requeued, err := store.Fail(workspace, run.ID, old.ID, old.Owner, old.Lease.Token, interruption, "partial verification evidence")
	if err != nil || requeued.Status != ItemReady || !requeued.Failure.AutoRecovered || !requeued.Failure.Recoverable {
		t.Fatalf("stream interruption cannot retry: status=%s error=%v", requeued.Status, err)
	}
	store = NewStore(root)
	after, err := store.Get(workspace, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := itemByID(t, after, delivery.ID); !reflect.DeepEqual(got, delivery) {
		t.Fatal("retry changed a completed prerequisite or its artifact evidence")
	}
	if requeued.Output != "partial verification evidence" || math.Abs(after.SpentUSD-0.2) > 1e-9 {
		t.Fatal("retry reset prior evidence or usage")
	}
	current, err := store.ClaimNext(workspace, run.ID, ClaimOptions{Worker: old.Owner})
	if err != nil || current.ID != old.ID || current.Attempts != 2 || current.Lease.Token == old.Lease.Token {
		t.Fatalf("replacement claim did not preserve attempt/fencing contract: %v", err)
	}
	if _, err := store.Complete(context.Background(), workspace, run.ID, old.ID, CompletionInput{Worker: old.Owner, Token: old.Lease.Token, Output: "Stale completion"}); err == nil {
		t.Fatal("old stream attempt overwrote replacement completion")
	}
	if _, err := store.Renew(workspace, run.ID, old.ID, old.Owner, old.Lease.Token, time.Second); err == nil {
		t.Fatal("old stream attempt renewed replacement")
	}
	if _, err := store.Fail(workspace, run.ID, old.ID, old.Owner, old.Lease.Token, interruption, "Stale failure"); err == nil {
		t.Fatal("old stream attempt overwrote replacement failure")
	}
	// Late cumulative usage is admitted without reviving the old result token.
	if err := store.RecordSpend(workspace, run.ID, old.ID, old.Owner, old.Lease.Token, 0.3); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordSpend(workspace, run.ID, current.ID, current.Owner, current.Lease.Token, 0.1); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Complete(context.Background(), workspace, run.ID, current.ID, CompletionInput{Worker: current.Owner, Token: current.Lease.Token, Output: "Independent host verification passed"}); err != nil {
		t.Fatal(err)
	}
	finished, err := NewStore(root).Get(workspace, run.ID)
	if err != nil || finished.Status != RunCompleted || math.Abs(finished.SpentUSD-0.4) > 1e-9 {
		t.Fatalf("retry completion/usage invalid: status=%s spend=%f err=%v", finished.Status, finished.SpentUSD, err)
	}
	data, err := os.ReadFile(filepath.Join(workspace, "prior.txt"))
	if err != nil || string(data) != string(prior) {
		t.Fatal("retry discarded prior delivery file")
	}
}

func TestProviderStreamRecoveryStopsAtAttemptAndBudgetLimits(t *testing.T) {
	workspace := t.TempDir()
	store := NewStore(t.TempDir())
	run, old := singleClaim(t, store, workspace, PhaseCustom, nil, nil)
	failure := Failure{Code: "provider_stream_interrupted", Summary: "Stream stopped"}
	if _, err := store.Fail(workspace, run.ID, old.ID, old.Owner, old.Lease.Token, failure, "partial"); err != nil {
		t.Fatal(err)
	}
	current, err := store.ClaimNext(workspace, run.ID, ClaimOptions{Worker: old.Owner})
	if err != nil {
		t.Fatal(err)
	}
	failed, err := store.Fail(workspace, run.ID, current.ID, current.Owner, current.Lease.Token, failure, "second stream failure")
	if err != nil || failed.Status != ItemFailed || failed.Failure.AutoRecovered || failed.Attempts != 2 {
		t.Fatalf("stream retries exceeded attempt cap: %+v %v", failed, err)
	}
	if _, err := store.ClaimNext(workspace, run.ID, ClaimOptions{Worker: old.Owner}); !errors.Is(err, ErrNoReadyWork) {
		t.Fatal("exhausted stream attempt was reclaimed")
	}
	limited, err := store.Create(CreateInput{Workspace: workspace, Goal: "Keep durable budget", MaxBudgetUSD: 0.3, Items: []WorkItemInput{{ID: "limited", Phase: PhaseCustom, Subject: "Limited", Prompt: "Work", MaxAttempts: 2}}})
	if err != nil {
		t.Fatal(err)
	}
	claim, err := store.ClaimNext(workspace, limited.ID, ClaimOptions{Worker: "same-worker"})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.RecordSpend(workspace, limited.ID, claim.ID, claim.Owner, claim.Lease.Token, 0.2); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Fail(workspace, limited.ID, claim.ID, claim.Owner, claim.Lease.Token, failure, "partial"); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordSpend(workspace, limited.ID, claim.ID, claim.Owner, claim.Lease.Token, 0.3); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ClaimNext(workspace, limited.ID, ClaimOptions{Worker: claim.Owner}); !errors.Is(err, ErrNoReadyWork) {
		t.Fatal("stream retry bypassed persisted budget")
	}
	after, err := store.Get(workspace, limited.ID)
	if err != nil || itemByID(t, after, claim.ID).Failure.Code != "task_budget_exceeded" {
		t.Fatalf("budget failure lost priority: %v", err)
	}
}

func TestProviderStreamPolicyDoesNotTrustCallerRecoverableFlag(t *testing.T) {
	for _, code := range []string{FailureWorkerExecution, "authentication_error", "invalid_request_error", "context_length_exceeded", "acceptance_failed"} {
		t.Run(code, func(t *testing.T) {
			workspace := t.TempDir()
			store := NewStore(t.TempDir())
			run, claim := singleClaim(t, store, workspace, PhaseCustom, nil, nil)
			item, err := store.Fail(workspace, run.ID, claim.ID, claim.Owner, claim.Lease.Token, Failure{Code: code, Summary: "Terminal failure", Recoverable: true}, "evidence")
			if err != nil || item.Status != ItemFailed || item.Failure.Recoverable || item.Failure.AutoRecovered {
				t.Fatalf("terminal failure was made retryable: %s %v", code, err)
			}
		})
	}
}
