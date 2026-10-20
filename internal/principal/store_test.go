package principal

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func testDefinition(t *testing.T) Definition {
	t.Helper()
	workspace, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return Definition{Workspace: workspace, Provider: "fixture", ProviderConfigSHA256: strings.Repeat("a", 64), Profile: Profile{Name: "reviewer", Model: "fixture-model", PermissionMode: "plan", Source: "project", Tools: []string{"Read"}, SystemPrompt: "frozen role"}, Credentials: []CredentialRef{{Kind: "provider_account", Name: "fixture"}}}
}

func TestJobSnapshotAndWorkspaceSurviveRestartWithoutAliasing(t *testing.T) {
	root := t.TempDir()
	s := NewStore(root)
	d := testDefinition(t)
	j, err := s.Create("stable reviewer", d)
	if err != nil {
		t.Fatal(err)
	}
	originalHash := j.ConfigSHA256
	d.Profile.Tools[0] = "Bash"
	j.Definition.Profile.Tools[0] = "Write"
	reloaded, err := NewStore(root).Get(j.ID)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.Definition.Profile.Tools[0] != "Read" || reloaded.ConfigSHA256 != originalHash || reloaded.SessionID != j.SessionID {
		t.Fatalf("definition changed after restart: %+v", reloaded)
	}
	b := reloaded.Binding()
	b.Definition.Profile.Tools[0] = "Edit"
	if reloaded.Definition.Profile.Tools[0] != "Read" {
		t.Fatal("binding aliases principal definition")
	}
	if err := ValidateBinding(&b, reloaded.Definition.Workspace); err == nil {
		t.Fatal("changed tool permissions accepted under old configuration hash")
	}
	if err := ValidateBinding(ptr(reloaded.Binding()), t.TempDir()); err == nil {
		t.Fatal("principal crossed its bound workspace")
	}
	info, err := os.Stat(filepath.Join(root, j.ID+".json"))
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("state permissions: %v %v", info, err)
	}
}

func TestJobLeaseIsExclusiveAcrossIndependentStoresAndFencesOldWorkers(t *testing.T) {
	root := t.TempDir()
	s := NewStore(root)
	j, err := s.Create("worker", testDefinition(t))
	if err != nil {
		t.Fatal(err)
	}
	const n = 20
	leases := make(chan Lease, n)
	errs := make(chan error, n)
	var wg sync.WaitGroup
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			lease, err := NewStore(root).ClaimExecution(j.Binding(), "exec", "worker", time.Minute)
			if err != nil {
				errs <- err
			} else {
				leases <- lease
			}
		}()
	}
	wg.Wait()
	close(leases)
	close(errs)
	if len(leases) != 1 {
		t.Fatalf("concurrent stores acquired %d leases", len(leases))
	}
	lease := <-leases
	for err := range errs {
		if !errors.Is(err, ErrBusy) {
			t.Fatal(err)
		}
	}
	if err := s.ReleaseExecution(j.Binding(), lease); err != nil {
		t.Fatal(err)
	}
	next, err := NewStore(root).ClaimExecution(j.Binding(), "replacement", "worker", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if next.Token == lease.Token {
		t.Fatal("replacement reused stale credential")
	}
	if err := s.RenewExecution(j.Binding(), lease, time.Minute); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("stale renewal: %v", err)
	}
	if err := s.ReleaseExecution(j.Binding(), lease); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("stale release: %v", err)
	}
}

func TestJobBudgetIsIsolatedAcrossRunsRestartAndAbsoluteSnapshots(t *testing.T) {
	root := t.TempDir()
	s := NewStore(root)
	d := testDefinition(t)
	d.Budget = Budget{MaxUSD: 1, MaxInputTokens: 100}
	a, err := s.Create("a", d)
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.Create("b", d)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"run-1", "run-2"} {
		if _, err = s.RegisterRun(a.Binding(), id); err != nil {
			t.Fatal(err)
		}
	}
	usage := RunUsage{RunID: "run-1", SourceRevision: 1, Status: "completed", InputTokens: 20, CacheReadTokens: 40, OutputTokens: 5, SpentUSD: .2}
	for range 3 {
		if _, err = s.RecordRunUsage(a.Binding(), usage); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = s.RecordRunUsage(a.Binding(), RunUsage{RunID: "run-2", SourceRevision: 1, Status: "completed", InputTokens: 40, OutputTokens: 2, SpentUSD: .3}); err != nil {
		t.Fatal(err)
	}
	a, err = NewStore(root).Get(a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if a.InputTokens != 100 || a.OutputTokens != 7 || a.SpentUSD != .5 {
		t.Fatalf("double-accounted absolute projections: %+v", a)
	}
	if _, err = NewStore(root).ClaimExecution(a.Binding(), "next", "a", time.Minute); !errors.Is(err, ErrBudget) {
		t.Fatalf("lifetime token budget did not survive restart: %v", err)
	}
	b, err = NewStore(root).Get(b.ID)
	if err != nil {
		t.Fatal(err)
	}
	if b.InputTokens != 0 || b.SpentUSD != 0 {
		t.Fatal("another principal paid for a's work")
	}
	if _, err = NewStore(root).ClaimExecution(b.Binding(), "next", "b", time.Minute); err != nil {
		t.Fatal(err)
	}
}

func TestJobUnresolvedUsageBlocksRestartUntilAuthoritativeRunSettles(t *testing.T) {
	s := NewStore(t.TempDir())
	j, err := s.Create("job", testDefinition(t))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.RegisterRun(j.Binding(), "run"); err != nil {
		t.Fatal(err)
	}
	if _, err = s.RecordRunUsage(j.Binding(), RunUsage{RunID: "run", SourceRevision: 1, Status: "blocked", InputTokens: 15, UnresolvedRequests: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err = NewStore(s.root).ClaimExecution(j.Binding(), "restart", "worker", time.Minute); !errors.Is(err, ErrUsageUnknown) {
		t.Fatalf("unsettled provider request dispatched again: %v", err)
	}
	if _, err = s.RecordRunUsage(j.Binding(), RunUsage{RunID: "run", SourceRevision: 2, Status: "completed", InputTokens: 25}); err != nil {
		t.Fatal(err)
	}
	if _, err = s.ClaimExecution(j.Binding(), "restart", "worker", time.Minute); err != nil {
		t.Fatal(err)
	}
}

func TestJobProjectionRejectsStaleStatusAndAcceptsNewerAbsoluteCorrection(t *testing.T) {
	s := NewStore(t.TempDir())
	j, err := s.Create("job", testDefinition(t))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.RegisterRun(j.Binding(), "run"); err != nil {
		t.Fatal(err)
	}
	newer := RunUsage{RunID: "run", SourceRevision: 2, Status: "active", InputTokens: 80, UnresolvedRequests: 1}
	if _, err = s.RecordRunUsage(j.Binding(), newer); err != nil {
		t.Fatal(err)
	}
	// A slow `job status` read of revision 1 finishes after the observer has
	// persisted revision 2. It must not clear the unknown-usage budget hold.
	stale := RunUsage{RunID: "run", SourceRevision: 1, Status: "active", InputTokens: 1}
	updated, err := NewStore(s.root).RecordRunUsage(j.Binding(), stale)
	if err != nil {
		t.Fatal(err)
	}
	if updated.InputTokens != 80 || updated.UnresolvedRequests != 1 {
		t.Fatal("old status snapshot overwrote new provider accounting")
	}
	final := RunUsage{RunID: "run", SourceRevision: 3, Status: "completed", InputTokens: 20}
	updated, err = s.RecordRunUsage(j.Binding(), final)
	if err != nil {
		t.Fatal(err)
	}
	if updated.InputTokens != 20 || updated.UnresolvedRequests != 0 {
		t.Fatal("newer legitimate absolute correction was rejected")
	}
	final.InputTokens = 5
	if _, err = s.RecordRunUsage(j.Binding(), final); err == nil {
		t.Fatal("conflicting accounting under the same coordinator revision was accepted")
	}
}

func TestPauseCancelsLeaseAndRetirementCannotBeReversed(t *testing.T) {
	s := NewStore(t.TempDir())
	j, err := s.Create("job", testDefinition(t))
	if err != nil {
		t.Fatal(err)
	}
	l, err := s.ClaimExecution(j.Binding(), "exec", "worker", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	ctx, stop := s.KeepExecution(context.Background(), j.Binding(), l, time.Second)
	defer stop()
	if _, err = s.SetStatus(j.ID, Paused); err != nil {
		t.Fatal(err)
	}
	select {
	case <-ctx.Done():
		if !errors.Is(context.Cause(ctx), ErrInactive) {
			t.Fatal(context.Cause(ctx))
		}
	case <-time.After(2 * time.Second):
		t.Fatal("pause left active work running")
	}
	stop()
	if _, err = s.SetStatus(j.ID, Active); err != nil {
		t.Fatal(err)
	}
	if _, err = s.ClaimExecution(j.Binding(), "resumed", "worker", time.Second); err != nil {
		t.Fatal(err)
	}
	if _, err = s.SetStatus(j.ID, Retired); err != nil {
		t.Fatal(err)
	}
	if _, err = s.SetStatus(j.ID, Active); err == nil {
		t.Fatal("retired principal resurrected silently")
	}
}

func TestJobRejectsSecretLikeCredentialReferencesAndSymlinkState(t *testing.T) {
	s := NewStore(t.TempDir())
	d := testDefinition(t)
	d.Credentials = []CredentialRef{{Kind: "env", Name: "sk-example-secret-value"}}
	if _, err := s.Create("secret", d); err == nil {
		t.Fatal("secret value accepted as credential reference")
	}
	d = testDefinition(t)
	j, err := s.Create("job", d)
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(s.root, j.ID+".json")
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(t.TempDir(), "state.json")
	if err = os.WriteFile(other, data, 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.Remove(p); err != nil {
		t.Fatal(err)
	}
	if err = os.Symlink(other, p); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Get(j.ID); err == nil {
		t.Fatal("symlinked job state accepted")
	}
}
