package projectcoord

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/internal/execution"
)

func TestRunPersistsGraphAndPromotesDependencies(t *testing.T) {
	workspace := t.TempDir()
	root := filepath.Join(t.TempDir(), "project-coordinator")
	store := NewStore(root)
	run, err := store.Create(CreateInput{Workspace: workspace, Goal: "Ship a durable coordinator"})
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != RunActive {
		t.Fatalf("new run status = %s, want active", run.Status)
	}
	if got := itemByID(t, run, "research").Status; got != ItemReady {
		t.Fatalf("research status = %s, want ready", got)
	}
	if got := itemByID(t, run, "synthesis").Status; got != ItemPending {
		t.Fatalf("synthesis status = %s, want pending", got)
	}

	// Reopening through a fresh Store proves that the scheduler is not only
	// held in the CLI process that created it.
	reopened := NewStore(root)
	claimed, err := reopened.ClaimNext(workspace, run.ID, ClaimOptions{Worker: "researcher"})
	if err != nil {
		t.Fatal(err)
	}
	if claimed.ID != "research" || claimed.Status != ItemRunning || claimed.Attempts != 1 {
		t.Fatalf("claimed = %+v", claimed)
	}
	if _, err := reopened.Complete(workspace, run.ID, claimed.ID, "researcher", "Repository is a Go module; tests are available."); err != nil {
		t.Fatal(err)
	}
	progress, err := reopened.Get(workspace, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := itemByID(t, progress, "synthesis").Status; got != ItemReady {
		t.Fatalf("synthesis after research = %s, want ready", got)
	}
	if got := itemByID(t, progress, "implementation").Status; got != ItemPending {
		t.Fatalf("implementation after research = %s, want pending", got)
	}

	for _, worker := range []string{"planner", "builder", "verifier"} {
		item, err := reopened.ClaimNext(workspace, run.ID, ClaimOptions{Worker: worker})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := reopened.Complete(workspace, run.ID, item.ID, worker, "evidence from "+worker); err != nil {
			t.Fatal(err)
		}
	}
	finished, err := reopened.Get(workspace, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if finished.Status != RunCompleted || finished.CompletedAt.IsZero() {
		t.Fatalf("finished run = %+v", finished)
	}
	if len(finished.Events) < 9 {
		t.Fatalf("events = %d, want lifecycle audit trail", len(finished.Events))
	}

	dir, err := reopened.projectDir(workspace)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(dir, run.ID+".json"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("run file mode = %o, want 600", info.Mode().Perm())
	}
}

func TestFailureIsDurableAndKnownEnvironmentRuleRecoversOnce(t *testing.T) {
	workspace := t.TempDir()
	store := NewStore(filepath.Join(t.TempDir(), "state"))

	blockedRun, err := store.Create(CreateInput{Workspace: workspace, Goal: "Exercise failures"})
	if err != nil {
		t.Fatal(err)
	}
	item, err := store.ClaimNext(workspace, blockedRun.ID, ClaimOptions{Worker: "worker-a"})
	if err != nil {
		t.Fatal(err)
	}
	failed, err := store.Fail(workspace, blockedRun.ID, item.ID, "worker-a", Failure{Code: "provider_rejected", Summary: "Provider rejected the request."}, "provider output")
	if err != nil {
		t.Fatal(err)
	}
	if failed.Status != ItemFailed || failed.Failure == nil || failed.Failure.Recoverable {
		t.Fatalf("failed work item = %+v", failed)
	}
	persisted, err := NewStore(store.Root()).Get(workspace, blockedRun.ID)
	if err != nil {
		t.Fatal(err)
	}
	if persisted.Status != RunBlocked || itemByID(t, persisted, item.ID).Failure.Summary != "Provider rejected the request." {
		t.Fatalf("persisted failure = %+v", persisted)
	}
	recovered, err := store.Recover(workspace, blockedRun.ID, item.ID, "Provider settings fixed.", false)
	if err != nil {
		t.Fatal(err)
	}
	if recovered.Status != ItemReady {
		t.Fatalf("manual recovery status = %s, want ready", recovered.Status)
	}

	knownRun, err := store.Create(CreateInput{Workspace: workspace, Goal: "Recover known environment failure"})
	if err != nil {
		t.Fatal(err)
	}
	knownItem, err := store.ClaimNext(workspace, knownRun.ID, ClaimOptions{Worker: "worker-b"})
	if err != nil {
		t.Fatal(err)
	}
	auto, err := store.Fail(workspace, knownRun.ID, knownItem.ID, "worker-b", Failure{
		Code:    string(execution.FailureWorktreeRequiresGit),
		Summary: "Git worktree isolation is unavailable because this workspace is not a Git repository.",
	}, "the child requested worktree isolation")
	if err != nil {
		t.Fatal(err)
	}
	if auto.Status != ItemReady || auto.Failure == nil || !auto.Failure.AutoRecovered || !auto.Failure.Recoverable {
		t.Fatalf("known environment recovery = %+v", auto)
	}
	if auto.Attempts != 1 {
		t.Fatalf("automatic recovery changed attempts = %d, want 1", auto.Attempts)
	}
}

func TestExpiredLeaseRequeuesAcrossRestart(t *testing.T) {
	workspace := t.TempDir()
	now := time.Date(2026, time.September, 20, 10, 0, 0, 0, time.UTC)
	root := filepath.Join(t.TempDir(), "state")
	store := NewStore(root, withClock(func() time.Time { return now }))
	run, err := store.Create(CreateInput{Workspace: workspace, Goal: "Recover a crashed worker"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.ClaimNext(workspace, run.ID, ClaimOptions{Worker: "crashed-worker", Lease: time.Minute}); err != nil {
		t.Fatal(err)
	}
	now = now.Add(2 * time.Minute)
	restarted := NewStore(root, withClock(func() time.Time { return now }))
	got, err := restarted.Get(workspace, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	requeued := itemByID(t, got, "research")
	if requeued.Status != ItemReady || requeued.Failure == nil || requeued.Failure.Code != FailureLeaseExpired || !requeued.Failure.AutoRecovered {
		t.Fatalf("expired lease recovery = %+v", requeued)
	}
	if next, err := restarted.ClaimNext(workspace, run.ID, ClaimOptions{Worker: "replacement"}); err != nil || next.ID != "research" {
		t.Fatalf("replacement claim = %+v, %v", next, err)
	}
}

func TestLeaseExpiryStopsAfterAttemptBudget(t *testing.T) {
	workspace := t.TempDir()
	now := time.Date(2026, time.September, 20, 10, 0, 0, 0, time.UTC)
	root := filepath.Join(t.TempDir(), "state")
	store := NewStore(root, withClock(func() time.Time { return now }))
	run, err := store.Create(CreateInput{Workspace: workspace, Goal: "Do not loop crashed workers"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.ClaimNext(workspace, run.ID, ClaimOptions{Worker: "first", Lease: time.Minute}); err != nil {
		t.Fatal(err)
	}
	now = now.Add(2 * time.Minute)
	firstExpiry, err := store.Get(workspace, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := itemByID(t, firstExpiry, "research"); got.Status != ItemReady || got.Attempts != 1 || got.Failure == nil || !got.Failure.AutoRecovered {
		t.Fatalf("first expiry = %+v", got)
	}
	if _, err := store.ClaimNext(workspace, run.ID, ClaimOptions{Worker: "second", Lease: time.Minute}); err != nil {
		t.Fatal(err)
	}
	now = now.Add(2 * time.Minute)
	secondExpiry, err := store.Get(workspace, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	item := itemByID(t, secondExpiry, "research")
	if item.Status != ItemFailed || item.Attempts != item.MaxAttempts || item.Failure == nil || item.Failure.AutoRecovered {
		t.Fatalf("second expiry = %+v", item)
	}
	if secondExpiry.Status != RunBlocked {
		t.Fatalf("run status after exhausted lease retries = %s, want blocked", secondExpiry.Status)
	}
}

func TestForcedRecoveryIsAvailableOnlyOnceAfterAttemptBudget(t *testing.T) {
	workspace := t.TempDir()
	store := NewStore(filepath.Join(t.TempDir(), "state"))
	run, err := store.Create(CreateInput{Workspace: workspace, Goal: "Bound explicit recovery"})
	if err != nil {
		t.Fatal(err)
	}
	fail := func(worker string) {
		t.Helper()
		item, err := store.ClaimNext(workspace, run.ID, ClaimOptions{Worker: worker})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.Fail(workspace, run.ID, item.ID, worker, Failure{Code: "provider_rejected", Summary: "Provider rejected this attempt."}, ""); err != nil {
			t.Fatal(err)
		}
	}
	fail("first")
	if _, err := store.Recover(workspace, run.ID, "research", "Changed provider settings.", false); err != nil {
		t.Fatal(err)
	}
	fail("second")
	forced, err := store.Recover(workspace, run.ID, "research", "Applied one final corrective change.", true)
	if err != nil {
		t.Fatal(err)
	}
	if forced.ForcedRecoveries != 1 || forced.Status != ItemReady {
		t.Fatalf("forced recovery = %+v", forced)
	}
	fail("third")
	if _, err := store.Recover(workspace, run.ID, "research", "Try again anyway.", true); err == nil {
		t.Fatal("second forced recovery was accepted")
	}
}

func TestKnownProjectFailureAlsoUpdatesExecutionMemory(t *testing.T) {
	workspace := t.TempDir()
	memory := execution.NewStore(filepath.Join(t.TempDir(), "execution"))
	store := NewStore(filepath.Join(t.TempDir(), "projects"), WithExecutionMemory(memory))
	run, err := store.Create(CreateInput{Workspace: workspace, Goal: "Remember execution constraint"})
	if err != nil {
		t.Fatal(err)
	}
	item, err := store.ClaimNext(workspace, run.ID, ClaimOptions{Worker: "worker"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Fail(workspace, run.ID, item.ID, "worker", Failure{
		Code:            string(execution.FailureWorktreeRequiresGit),
		Summary:         "Git worktree isolation is unavailable because this workspace is not a Git repository.",
		SuggestedAction: "Use direct execution with the same working directory.",
	}, ""); err != nil {
		t.Fatal(err)
	}
	profile, found, err := memory.Load(workspace)
	if err != nil {
		t.Fatal(err)
	}
	if !found || profile.LastFailure == nil || profile.LastFailure.Code != execution.FailureWorktreeRequiresGit {
		t.Fatalf("execution memory = %+v, found=%t", profile, found)
	}
}

func TestConcurrentWorkersCannotClaimSameWorkItem(t *testing.T) {
	workspace := t.TempDir()
	root := filepath.Join(t.TempDir(), "state")
	created, err := NewStore(root).Create(CreateInput{Workspace: workspace, Goal: "Prove atomic claiming"})
	if err != nil {
		t.Fatal(err)
	}

	start := make(chan struct{})
	results := make(chan error, 2)
	var wg sync.WaitGroup
	for _, worker := range []string{"one", "two"} {
		worker := worker
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, err := NewStore(root).ClaimNext(workspace, created.ID, ClaimOptions{Worker: worker})
			results <- err
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	var claimed, idle int
	for err := range results {
		switch {
		case err == nil:
			claimed++
		case errors.Is(err, ErrNoReadyWork):
			idle++
		default:
			t.Fatalf("concurrent claim error = %v", err)
		}
	}
	if claimed != 1 || idle != 1 {
		t.Fatalf("claims: claimed=%d idle=%d", claimed, idle)
	}
}

func TestGraphRejectsUnknownDependencyAndCycle(t *testing.T) {
	workspace := t.TempDir()
	store := NewStore(filepath.Join(t.TempDir(), "state"))
	_, err := store.Create(CreateInput{Workspace: workspace, Goal: "bad plan", Items: []WorkItemInput{{ID: "a", Phase: PhaseCustom, Subject: "A", Prompt: "do A", DependsOn: []string{"missing"}}}})
	if err == nil {
		t.Fatal("unknown dependency was accepted")
	}
	_, err = store.Create(CreateInput{Workspace: workspace, Goal: "cycle", Items: []WorkItemInput{
		{ID: "a", Phase: PhaseCustom, Subject: "A", Prompt: "do A", DependsOn: []string{"b"}},
		{ID: "b", Phase: PhaseCustom, Subject: "B", Prompt: "do B", DependsOn: []string{"a"}},
	}})
	if err == nil {
		t.Fatal("dependency cycle was accepted")
	}
}

func TestBuildWorkerPromptCarriesEnvironmentRuleAndDependencyEvidence(t *testing.T) {
	now := time.Now().UTC()
	run := Run{
		Workspace: "/tmp/workspace",
		Goal:      "Make the worker reliable",
		Environment: execution.Profile{LastFailure: &execution.Failure{
			Summary:         "Worktree isolation is unavailable in this directory.",
			SuggestedAction: "Use direct execution.",
		}},
		Items: []WorkItem{
			{ID: "research", Subject: "Research", Output: "The workspace has no Git metadata.", Status: ItemCompleted, CreatedAt: now, UpdatedAt: now},
			{ID: "implementation", Subject: "Implement", Prompt: "Make the change.", DependsOn: []string{"research"}, Phase: PhaseImplementation, Status: ItemReady, CreatedAt: now, UpdatedAt: now},
		},
	}
	prompt := BuildWorkerPrompt(run, run.Items[1])
	for _, want := range []string{"Make the worker reliable", "Use direct execution.", "The workspace has no Git metadata.", "METIS_PROJECT_STATUS: blocked"} {
		if !contains(prompt, want) {
			t.Fatalf("worker prompt missing %q:\n%s", want, prompt)
		}
	}
}

func itemByID(t *testing.T, run Run, id string) WorkItem {
	t.Helper()
	item, err := run.item(id)
	if err != nil {
		t.Fatal(err)
	}
	return *item
}

func contains(value, want string) bool {
	return len(want) == 0 || (len(value) >= len(want) && index(value, want) >= 0)
}

func index(value, want string) int {
	for i := 0; i+len(want) <= len(value); i++ {
		if value[i:i+len(want)] == want {
			return i
		}
	}
	return -1
}
