package projectcoord

import (
	"context"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestVerificationNeedsPassingHostChecks(t *testing.T) {
	for _, tc := range []struct {
		name     string
		commands []string
		pass     bool
	}{
		{"missing", nil, false}, {"failed", []string{"exit 3"}, false}, {"passed", []string{"exit 0"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			workspace := t.TempDir()
			store := NewStore(t.TempDir())
			run, item := singleClaim(t, store, workspace, PhaseVerification, tc.commands, nil)
			_, err := store.Complete(context.Background(), workspace, run.ID, item.ID, completionFor(item))
			if (err == nil) != tc.pass {
				t.Fatalf("completion error=%v", err)
			}
			after, err := NewStore(store.Root()).Get(workspace, run.ID)
			if err != nil {
				t.Fatal(err)
			}
			got := itemByID(t, after, item.ID)
			if got.Acceptance == nil {
				t.Fatal("acceptance evidence not persisted")
			}
			if tc.pass {
				if after.Status != RunCompleted || got.Acceptance.Status != "passed" || got.Acceptance.Checks[0].ExitCode != 0 {
					t.Fatalf("false acceptance: %+v", got)
				}
			} else if got.Status != ItemRunning || got.Acceptance.Status != "failed" {
				t.Fatalf("failed acceptance advanced graph: %+v", got)
			}
		})
	}
}

func TestMissingDeliverableAndChangesDuringChecksRejectCompletion(t *testing.T) {
	workspace := t.TempDir()
	store := NewStore(t.TempDir())
	run, item := singleClaim(t, store, workspace, PhaseVerification, []string{"echo changed>result.txt"}, []string{"result.txt"})
	if _, err := store.Complete(context.Background(), workspace, run.ID, item.ID, completionFor(item)); err == nil {
		t.Fatal("missing file accepted")
	}
	if err := os.WriteFile(filepath.Join(workspace, "result.txt"), []byte("original"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Complete(context.Background(), workspace, run.ID, item.ID, completionFor(item)); err == nil {
		t.Fatal("mutated file accepted")
	}
	if _, err := store.SetChecks(workspace, run.ID, []string{"exit 0"}); err != nil {
		t.Fatal(err)
	}
	completed, err := store.Complete(context.Background(), workspace, run.ID, item.ID, completionFor(item))
	if err != nil {
		t.Fatal(err)
	}
	if len(completed.Artifacts) != 1 || len(completed.Artifacts[0].SHA256) != 64 {
		t.Fatal("delivery manifest absent")
	}
}

func TestDeliverableSymlinkCannotLeaveWorkspace(t *testing.T) {
	workspace := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside.txt")
	if err := os.WriteFile(outside, []byte("outside"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(workspace, "result.txt")); err != nil {
		t.Skip(err)
	}
	store := NewStore(t.TempDir())
	run, item := singleClaim(t, store, workspace, PhaseCustom, nil, []string{"result.txt"})
	if _, err := store.Complete(context.Background(), workspace, run.ID, item.ID, completionFor(item)); err == nil {
		t.Fatal("outside file accepted")
	}
}

func TestAcceptanceCancellationStopsCheck(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix subprocess cancellation fixture")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	started := time.Now()
	if _, err := runAcceptanceCheck(ctx, t.TempDir(), "sleep 30"); err == nil {
		t.Fatal("cancelled check accepted")
	}
	if time.Since(started) > 3*time.Second {
		t.Fatal("cancelled check did not stop promptly")
	}
}

func TestTaskSpendSurvivesRestartRetryAndLateWorkerUsage(t *testing.T) {
	now := time.Now().UTC()
	workspace := t.TempDir()
	root := t.TempDir()
	store := NewStore(root, withClock(func() time.Time { return now }))
	run, err := store.Create(CreateInput{Workspace: workspace, Goal: "Persistent cap", MaxBudgetUSD: 1, Items: []WorkItemInput{{ID: "work", Subject: "Work", Prompt: "Do work", MaxBudgetUSD: 0.8}}})
	if err != nil {
		t.Fatal(err)
	}
	old, err := store.ClaimNext(workspace, run.ID, ClaimOptions{Worker: "worker"})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.RecordSpend(workspace, run.ID, old.ID, old.Owner, old.Lease.Token, 0.3); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordSpend(workspace, run.ID, old.ID, old.Owner, old.Lease.Token, 0.3); err != nil {
		t.Fatal(err)
	}
	now = now.Add(16 * time.Minute)
	store = NewStore(root, withClock(func() time.Time { return now }))
	next, err := store.ClaimNext(workspace, run.ID, ClaimOptions{Worker: "worker"})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.RecordSpend(workspace, run.ID, old.ID, old.Owner, old.Lease.Token, 0.4); err != nil {
		t.Fatal(err)
	}
	after, err := store.Get(workspace, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(after.SpentUSD-0.4) > 1e-9 || math.Abs(RemainingBudget(after, itemByID(t, after, next.ID))-0.4) > 1e-9 {
		t.Fatalf("spend reset/doubled: %+v", after)
	}
	if err := store.RecordSpend(workspace, run.ID, next.ID, next.Owner, next.Lease.Token, 0.5); err != nil {
		t.Fatal(err)
	}
	after, err = NewStore(root, withClock(func() time.Time { return now })).Get(workspace, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := itemByID(t, after, next.ID); got.Status != ItemFailed || got.Failure.Code != "task_budget_exceeded" {
		t.Fatalf("cap not enforced: %+v", got)
	}
	if _, err := store.Complete(context.Background(), workspace, run.ID, next.ID, completionFor(next)); err == nil {
		t.Fatal("over-budget task completed")
	}
}

func TestPersistedDeadlineBlocksClaimsAfterRestart(t *testing.T) {
	now := time.Now().UTC()
	workspace := t.TempDir()
	store := NewStore(t.TempDir(), withClock(func() time.Time { return now }))
	run, err := store.Create(CreateInput{Workspace: workspace, Goal: "Deadline", DeadlineAt: now.Add(time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(2 * time.Minute)
	store = NewStore(store.Root(), withClock(func() time.Time { return now }))
	if _, err := store.ClaimNext(workspace, run.ID, ClaimOptions{Worker: "late"}); err == nil {
		t.Fatal("expired task claimed")
	}
	after, err := store.Get(workspace, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Status != RunBlocked {
		t.Fatalf("expired run=%s", after.Status)
	}
}

func TestFollowupWorkMayUpdateItsDeclaredPrerequisiteDeliverable(t *testing.T) {
	workspace := t.TempDir()
	path := filepath.Join(workspace, "result.txt")
	if err := os.WriteFile(path, []byte("first"), 0600); err != nil {
		t.Fatal(err)
	}
	store := NewStore(t.TempDir())
	run, err := store.Create(CreateInput{Workspace: workspace, Goal: "Update a delivered file", Items: []WorkItemInput{
		{ID: "first", Phase: PhaseImplementation, Subject: "Initial output", Prompt: "Write initial output", Deliverables: []string{"result.txt"}},
		{ID: "second", Phase: PhaseImplementation, Subject: "Followup output", Prompt: "Update output", DependsOn: []string{"first"}, Deliverables: []string{"result.txt"}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	item, err := store.ClaimNext(workspace, run.ID, ClaimOptions{Worker: "worker"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Complete(context.Background(), workspace, run.ID, item.ID, completionFor(item)); err != nil {
		t.Fatal(err)
	}
	claimed, err := store.ClaimNext(workspace, run.ID, ClaimOptions{Worker: "worker"})
	if err != nil || claimed.ID != "second" {
		t.Fatalf("claim: %+v %v", claimed, err)
	}
	if err := os.WriteFile(path, []byte("second"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Complete(context.Background(), workspace, run.ID, claimed.ID, completionFor(claimed)); err != nil {
		t.Fatal(err)
	}
}
