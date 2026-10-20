package projectcoord

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestRenewalKeepsLongWorkerAcrossStoreRestart(t *testing.T) {
	now := time.Date(2026, 10, 6, 1, 0, 0, 0, time.UTC)
	workspace, root := t.TempDir(), filepath.Join(t.TempDir(), "state")
	store := NewStore(root, withClock(func() time.Time { return now }))
	run, item := singleClaim(t, store, workspace, PhaseCustom, nil, nil)
	for i := 0; i < 4; i++ {
		now = now.Add(8 * time.Minute)
		store = NewStore(root, withClock(func() time.Time { return now }))
		if _, err := store.Renew(workspace, run.ID, item.ID, item.Owner, item.Lease.Token, 15*time.Minute); err != nil {
			t.Fatalf("renewal at +%dm: %v", (i+1)*8, err)
		}
	}
	if _, err := store.Complete(context.Background(), workspace, run.ID, item.ID, completionFor(item)); err != nil {
		t.Fatal(err)
	}
}

func TestOldClaimCannotRenewCompleteOrFailReplacement(t *testing.T) {
	now := time.Now().UTC()
	workspace := t.TempDir()
	store := NewStore(t.TempDir(), withClock(func() time.Time { return now }))
	run, old := singleClaim(t, store, workspace, PhaseCustom, nil, nil)
	now = now.Add(16 * time.Minute)
	current, err := store.ClaimNext(workspace, run.ID, ClaimOptions{Worker: old.Owner})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Renew(workspace, run.ID, old.ID, old.Owner, old.Lease.Token, time.Hour); err == nil {
		t.Fatal("old claim renewed replacement")
	}
	if _, err := store.Complete(context.Background(), workspace, run.ID, old.ID, completionFor(old)); err == nil {
		t.Fatal("old claim completed replacement")
	}
	if _, err := store.Fail(workspace, run.ID, old.ID, old.Owner, old.Lease.Token, Failure{Summary: "stale worker"}, ""); err == nil {
		t.Fatal("old claim failed replacement")
	}
	if _, err := store.Complete(context.Background(), workspace, run.ID, current.ID, completionFor(current)); err != nil {
		t.Fatal(err)
	}
}

func TestKeepLeaseRenewsAndStopsWithWorker(t *testing.T) {
	workspace := t.TempDir()
	store := NewStore(t.TempDir())
	run, item := singleClaim(t, store, workspace, PhaseCustom, nil, nil)
	ctx, stop, err := store.KeepLease(context.Background(), workspace, run.ID, item, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = stop() })
	select {
	case <-ctx.Done():
		t.Fatal(context.Cause(ctx))
	case <-time.After(1400 * time.Millisecond):
	}
	if _, err := store.Complete(ctx, workspace, run.ID, item.ID, completionFor(item)); err != nil {
		t.Fatal(err)
	}
	_ = stop()
	select {
	case <-ctx.Done():
	default:
		t.Fatal("stop did not cancel worker context")
	}
}

func TestKeepLeaseCancelsAfterClaimLoss(t *testing.T) {
	workspace := t.TempDir()
	store := NewStore(t.TempDir())
	run, item := singleClaim(t, store, workspace, PhaseCustom, nil, nil)
	ctx, stop, err := store.KeepLease(context.Background(), workspace, run.ID, item, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = stop() })
	if _, err := store.Fail(workspace, run.ID, item.ID, item.Owner, item.Lease.Token, Failure{Summary: "worker cancelled"}, ""); err != nil {
		t.Fatal(err)
	}
	select {
	case <-ctx.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("lease loss did not cancel work")
	}
	if err := stop(); err == nil {
		t.Fatal("lease loss was not reported")
	}
}

func singleClaim(t *testing.T, store *Store, workspace string, phase Phase, commands, deliverables []string) (Run, WorkItem) {
	t.Helper()
	run, err := store.Create(CreateInput{Workspace: workspace, Goal: "Reliable completion", CheckCommands: commands, Items: []WorkItemInput{{ID: "work", Phase: phase, Subject: "Work", Prompt: "Perform the task", Deliverables: deliverables}}})
	if err != nil {
		t.Fatal(err)
	}
	item, err := store.ClaimNext(workspace, run.ID, ClaimOptions{Worker: "worker"})
	if err != nil {
		t.Fatal(err)
	}
	return run, item
}

func completionFor(item WorkItem) CompletionInput {
	return CompletionInput{Worker: item.Owner, Token: item.Lease.Token, Output: "Concrete result evidence"}
}
