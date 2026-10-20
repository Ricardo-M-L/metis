package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/internal/config"
	"github.com/Ricardo-M-L/metis/internal/principal"
	"github.com/Ricardo-M-L/metis/internal/projectcoord"
)

func TestJobRotationRejectsActiveAssignmentAfterArchivedUsageReturnsToHotTail(t *testing.T) {
	fixture, store, _, _ := newPersistentJobCLIFixture(t, 0)
	var providerRequests atomic.Int64
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		providerRequests.Add(1)
		http.Error(w, "rotation must reject before provider dispatch", http.StatusInternalServerError)
	}))
	defer provider.Close()
	// Keep even a regressed CLI confined to a local provider trap. Freeze a
	// fresh job against this route so route validation cannot mask the guard.
	path := filepath.Join(config.Home(), "config.toml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	data = []byte(strings.ReplaceAll(string(data), "https://example.invalid/v1", provider.URL))
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("JOB_TEST_KEY", "fixture-local-provider-key")
	definition := fixture.Definition
	if err := inJobWorkspace(definition.Workspace, func() error {
		cfg, _, err := config.Load()
		if err != nil {
			return err
		}
		definition.ProviderConfigSHA256, _, _, err = jobProviderFingerprint(cfg, definition.Provider)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	jobs := persistentJobStore()
	job, err := jobs.Create("history rotation regression", definition)
	if err != nil {
		t.Fatal(err)
	}
	binding := job.Binding()
	newRun := func(goal string) projectcoord.Run {
		t.Helper()
		run, err := store.Create(projectcoord.CreateInput{Workspace: definition.Workspace, Goal: goal, Items: []projectcoord.WorkItemInput{{ID: "assignment", Subject: goal, Prompt: goal, AgentProfile: definition.Profile.Name, PrincipalBinding: &binding}}})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := jobs.RegisterRun(binding, run.ID); err != nil {
			t.Fatal(err)
		}
		return run
	}
	old := newRun("old A")
	item, err := store.ClaimNext(old.Workspace, old.ID, projectcoord.ClaimOptions{Worker: "rotation-fixture", Lease: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Complete(context.Background(), old.Workspace, old.ID, item.ID, projectcoord.CompletionInput{Worker: item.Owner, Token: item.Lease.Token, Output: "Completed A before historical archival."}); err != nil {
		t.Fatal(err)
	}
	if err := syncPersistentJobUsage(store, binding); err != nil {
		t.Fatal(err)
	}
	// Enough subsequent settled runs put actual completed A into the immutable
	// history. Their summaries need no coordinator refresh once settled.
	for i := 0; i < principal.HotRunHistoryLimit; i++ {
		id := fmt.Sprintf("run-rotation-history-%03d", i)
		if _, err := jobs.RegisterRun(binding, id); err != nil {
			t.Fatal(err)
		}
		if _, err := jobs.RecordRunUsage(binding, principal.RunUsage{RunID: id, SourceRevision: 1, Status: "completed"}); err != nil {
			t.Fatal(err)
		}
	}
	job, err = jobs.Get(job.ID)
	if err != nil || job.RunHistory.Count == 0 {
		t.Fatalf("old A was not archived: %+v %v", job.RunHistory, err)
	}
	for _, r := range job.Runs {
		if r.RunID == old.ID {
			t.Fatal("old A remained hot; fixture did not exercise archival")
		}
	}
	active := newRun("new B remains active")
	oldUsage, err := jobs.GetRunUsage(job.ID, old.ID)
	if err != nil {
		t.Fatal(err)
	}
	oldUsage.SourceRevision++
	oldUsage.UnresolvedRequests = 1
	if _, err := jobs.RecordRunUsage(binding, oldUsage); err != nil {
		t.Fatal(err)
	}
	oldUsage.SourceRevision++
	oldUsage.UnresolvedRequests = 0
	job, err = jobs.RecordRunUsage(binding, oldUsage)
	if err != nil {
		t.Fatal(err)
	}
	tail := job.Runs[len(job.Runs)-1]
	if tail.RunID != old.ID || tail.Status != "completed" || job.UnresolvedRequests != 0 {
		t.Fatalf("fixture did not restore settled old A at the hot tail: %+v", tail)
	}
	activeUsage, err := jobs.GetRunUsage(job.ID, active.ID)
	if err != nil || activeUsage.Status != "active" {
		t.Fatalf("new B must still be active: %+v %v", activeUsage, err)
	}
	before, err := store.List(definition.Workspace)
	if err != nil {
		t.Fatal(err)
	}
	// A cancelled context is another guard against accidental provider work.
	// It cannot make this test pass: only the exact rotation eligibility error
	// for B is accepted, and creating any new run fails the assertions below.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err = cmdJobRun(ctx, []string{job.ID, "--prompt", "C must not be created"}, true)
	afterJob, jobErr := jobs.Get(job.ID)
	after, listErr := store.List(definition.Workspace)
	want := "previous job run " + active.ID + " is active; resume it with --run-id before rotating"
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("rotation eligibility failed to reject active B: got %v; job runs before=%d after=%d, project runs before=%d after=%d, provider requests=%d", err, job.RunCount(), afterJob.RunCount(), len(before), len(after), providerRequests.Load())
	}
	if jobErr != nil || listErr != nil {
		t.Fatalf("read after rejected rotation: %v %v", jobErr, listErr)
	}
	if afterJob.RunCount() != job.RunCount() || len(after) != len(before) || providerRequests.Load() != 0 {
		t.Fatal("rejected rotation created C or dispatched a provider request")
	}
}
