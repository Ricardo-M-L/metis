package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/internal/config"
	"github.com/Ricardo-M-L/metis/internal/learning"
	"github.com/Ricardo-M-L/metis/internal/projectcoord"
	"github.com/Ricardo-M-L/metis/internal/workflow"
)

func TestCoordinatorLearningUsesOnlyCompletedInvocationAndHostChecks(t *testing.T) {
	for _, finished := range []bool{false, true} {
		t.Run(map[bool]string{false: "interrupted_tool", true: "host_rejected"}[finished], func(t *testing.T) {
			t.Setenv("METIS_HOME", t.TempDir())
			workspace, err := projectCoordinatorWorkspace(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			artifactRoot := filepath.Join(config.Home(), "skills")
			artifact := learning.NewStore(artifactRoot)
			if _, err := artifact.Stage(context.Background(), "quality-test", []byte("baseline"), []byte("candidate"), "host"); err != nil {
				t.Fatal(err)
			}
			_, err = artifact.ConfigureQuality(context.Background(), "quality-test", learning.QualityPolicy{Kind: "skill", WindowSize: 2, MinimumSamples: 1, MaxFailureRate: 0, Action: "pause", Verifier: coordinatorQualityVerifier})
			if err != nil {
				t.Fatal(err)
			}
			state, _ := artifact.Read("quality-test")
			store := projectCoordinatorStore()
			run, err := store.Create(projectcoord.CreateInput{Workspace: workspace, Goal: "quality host", CheckCommands: []string{"exit 7"}, Items: []projectcoord.WorkItemInput{{ID: "implementation", Phase: projectcoord.PhaseImplementation, Subject: "implement", Prompt: "implement"}, {ID: "verification", Phase: projectcoord.PhaseVerification, Subject: "verify", Prompt: "verify", DependsOn: []string{"implementation"}}}})
			if err != nil {
				t.Fatal(err)
			}
			item, err := store.ClaimNext(workspace, run.ID, projectcoord.ClaimOptions{Worker: "quality-worker", Lease: time.Minute})
			if err != nil {
				t.Fatal(err)
			}
			ctx := coordinatorLearningContext(context.Background(), store, workspace, run.ID, item)
			finish, err := learning.BeginInvocation(ctx, learning.Invocation{Kind: "skill", ArtifactRoot: artifactRoot, Name: "quality-test", VersionSHA256: state.ActiveVersion, ActivationEpoch: state.ActivationEpoch})
			if err != nil {
				t.Fatal(err)
			}
			if finished {
				if err := finish(ctx, []byte(`{"output":"actual unit tool result"}`)); err != nil {
					t.Fatal(err)
				}
			}
			if err := reconcileCoordinatorLearning(context.Background(), store, workspace, run.ID); err != nil {
				t.Fatal(err)
			}
			before, _ := artifact.Read("quality-test")
			if before.Quality.Paused {
				t.Fatal("model/tool result alone changed quality")
			}
			if _, err := store.Complete(context.Background(), workspace, run.ID, item.ID, projectcoord.CompletionInput{Worker: item.Owner, Token: item.Lease.Token, Output: "unit implementation"}); err != nil {
				t.Fatal(err)
			}
			v, err := store.ClaimNext(workspace, run.ID, projectcoord.ClaimOptions{Worker: "host-verifier", Lease: time.Minute})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := store.Complete(context.Background(), workspace, run.ID, v.ID, projectcoord.CompletionInput{Worker: v.Owner, Token: v.Lease.Token, Output: "model says PASS"}); err == nil {
				t.Fatal("failing host check accepted")
			}
			for i := 0; i < 2; i++ {
				if err := reconcileCoordinatorLearning(context.Background(), store, workspace, run.ID); err != nil {
					t.Fatal(err)
				}
			}
			after, _ := artifact.Read("quality-test")
			if after.Quality.Paused != finished {
				t.Fatalf("pause=%v completed invocation=%v", after.Quality.Paused, finished)
			}
			count := 0
			for _, r := range after.Quality.Records {
				if r.Outcome != nil {
					count++
				}
			}
			if finished && count != 1 {
				t.Fatalf("host check duplicate feedback count=%d", count)
			}
			if !finished && count != 0 {
				t.Fatal("interrupted invocation earned feedback")
			}
		})
	}
}

func TestCoordinatorLearningRejectsChangedActivationAndUnsafeEvidence(t *testing.T) {
	_, store, run, item := newPersistentJobCLIFixture(t, 0)
	root := t.TempDir()
	artifact := learning.NewStore(root)
	if _, err := artifact.Stage(context.Background(), "quality-test", []byte("baseline"), []byte("candidate"), "host"); err != nil {
		t.Fatal(err)
	}
	_, err := artifact.ConfigureQuality(context.Background(), "quality-test", learning.QualityPolicy{Kind: "skill", WindowSize: 2, MinimumSamples: 1, MaxFailureRate: 0, Action: "pause", Verifier: coordinatorQualityVerifier})
	if err != nil {
		t.Fatal(err)
	}
	ctx := coordinatorLearningContext(context.Background(), store, run.Workspace, run.ID, item)
	if _, err := learning.BeginInvocation(ctx, learning.Invocation{Kind: "skill", ArtifactRoot: root, Name: "quality-test", VersionSHA256: learning.Digest([]byte("wrong")), ActivationEpoch: 1}); err == nil {
		t.Fatal("wrong activation bound")
	}
	file := filepath.Join(t.TempDir(), "evidence.json")
	if err := os.WriteFile(file, []byte(`{}`), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := readCoordinatorLearningInvocation(file); err == nil {
		t.Fatal("public evidence accepted")
	}
}

func coordinatorQualityRegressionFixture(t *testing.T, check string) (*projectcoord.Store, projectcoord.Run, projectcoord.WorkItem, *learning.Store, learning.Invocation) {
	t.Helper()
	t.Setenv("METIS_HOME", t.TempDir())
	workspace, err := projectCoordinatorWorkspace(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(config.Home(), "skills")
	artifact := learning.NewStore(root)
	if _, err := artifact.Stage(context.Background(), "quality-test", []byte("baseline"), []byte("proposal"), "host"); err != nil {
		t.Fatal(err)
	}
	if _, err := artifact.ConfigureQuality(context.Background(), "quality-test", learning.QualityPolicy{Kind: "skill", WindowSize: 10, MinimumSamples: 10, MaxFailureRate: 0, Action: "pause", Verifier: coordinatorQualityVerifier}); err != nil {
		t.Fatal(err)
	}
	st, _ := artifact.Read("quality-test")
	store := projectCoordinatorStore()
	run, err := store.Create(projectcoord.CreateInput{Workspace: workspace, Goal: "host quality regression", CheckCommands: []string{check}, Items: []projectcoord.WorkItemInput{{ID: "implementation", Phase: projectcoord.PhaseImplementation, Subject: "implement", Prompt: "implement"}, {ID: "verification", Phase: projectcoord.PhaseVerification, Subject: "verify", Prompt: "verify", DependsOn: []string{"implementation"}}}})
	if err != nil {
		t.Fatal(err)
	}
	item, err := store.ClaimNext(workspace, run.ID, projectcoord.ClaimOptions{Worker: "quality-worker", Lease: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	return store, run, item, artifact, learning.Invocation{Kind: "skill", ArtifactRoot: root, Name: "quality-test", VersionSHA256: st.ActiveVersion, ActivationEpoch: st.ActivationEpoch}
}
func coordinatorQualityOutcomeCount(t *testing.T, s *learning.Store) int {
	t.Helper()
	st, err := s.Read("quality-test")
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, r := range st.Quality.Records {
		if r.Outcome != nil {
			count++
		}
	}
	return count
}
func TestCoordinatorLearningLegacyWorkflowWithoutLearningIsNoop(t *testing.T) {
	store, run, item, _, _ := coordinatorQualityRegressionFixture(t, "true")
	root := filepath.Join(config.Home(), "workflows")
	saved := workflow.NewStore(root)
	if err := saved.Save(workflow.Workflow{Name: "legacy-flow", Steps: []workflow.Step{{Name: "legacy", Command: "printf legacy"}}}); err != nil {
		t.Fatal(err)
	}
	wf, err := saved.Load("legacy-flow")
	if err != nil || wf.VersionSHA256 == "" {
		t.Fatalf("bad legacy fixture: %+v %v", wf, err)
	}
	ctx := coordinatorLearningContext(context.Background(), store, run.Workspace, run.ID, item)
	finish, err := learning.BeginInvocation(ctx, learning.Invocation{Kind: "workflow", ArtifactRoot: root, Name: wf.Name, VersionSHA256: wf.VersionSHA256})
	if err != nil {
		t.Fatalf("ordinary unmanaged workflow was blocked: %v", err)
	}
	if err := finish(ctx, []byte("legacy result")); err != nil {
		t.Fatal(err)
	}
	if _, err := saved.LearningStore().Read(wf.Name); !os.IsNotExist(err) {
		t.Fatalf("noop created learning authority: %v", err)
	}
}
func TestCoordinatorLearningLostClaimCannotFinishInvocationEvidence(t *testing.T) {
	store, run, item, artifact, in := coordinatorQualityRegressionFixture(t, "exit 7")
	ctx := coordinatorLearningContext(context.Background(), store, run.Workspace, run.ID, item)
	finish, err := learning.BeginInvocation(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Fail(run.Workspace, run.ID, item.ID, item.Owner, item.Lease.Token, projectcoord.Failure{Code: projectcoord.FailureWorkerInterrupted, Summary: "worker lost"}, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ClaimNext(run.Workspace, run.ID, projectcoord.ClaimOptions{Worker: "replacement", Lease: time.Minute}); err != nil {
		t.Fatal(err)
	}
	if err := finish(ctx, []byte("late old worker result")); err == nil {
		t.Fatal("old worker persisted completion proof after losing claim")
	}
	if coordinatorQualityOutcomeCount(t, artifact) != 0 {
		t.Fatal("lost claim earned a quality sample")
	}
}
func TestCoordinatorLearningRetryCountsOnlyCompletedCurrentAttempt(t *testing.T) {
	store, run, item, artifact, in := coordinatorQualityRegressionFixture(t, "exit 7")
	ctx := coordinatorLearningContext(context.Background(), store, run.Workspace, run.ID, item)
	finish, err := learning.BeginInvocation(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	if err := finish(ctx, []byte("first tool result")); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Fail(run.Workspace, run.ID, item.ID, item.Owner, item.Lease.Token, projectcoord.Failure{Code: projectcoord.FailureWorkerInterrupted, Summary: "failed later in worker"}, ""); err != nil {
		t.Fatal(err)
	}
	next, err := store.ClaimNext(run.Workspace, run.ID, projectcoord.ClaimOptions{Worker: "replacement", Lease: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	ctx = coordinatorLearningContext(context.Background(), store, run.Workspace, run.ID, next)
	finish, err = learning.BeginInvocation(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	if err := finish(ctx, []byte("second tool result")); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Complete(context.Background(), run.Workspace, run.ID, next.ID, projectcoord.CompletionInput{Worker: next.Owner, Token: next.Lease.Token, Output: "deliverable"}); err != nil {
		t.Fatal(err)
	}
	verify, err := store.ClaimNext(run.Workspace, run.ID, projectcoord.ClaimOptions{Worker: "checker", Lease: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Complete(context.Background(), run.Workspace, run.ID, verify.ID, projectcoord.CompletionInput{Worker: verify.Owner, Token: verify.Lease.Token, Output: "checker response"}); err == nil {
		t.Fatal("fixture checker unexpectedly passed")
	}
	for i := 0; i < 2; i++ {
		if err := reconcileCoordinatorLearning(context.Background(), store, run.Workspace, run.ID); err != nil {
			t.Fatal(err)
		}
	}
	if count := coordinatorQualityOutcomeCount(t, artifact); count != 1 {
		t.Fatalf("one business outcome was counted across attempts: count=%d", count)
	}
}
func TestCoordinatorLearningInfrastructureCheckerFailureIsNotBusinessFailure(t *testing.T) {
	for _, mode := range []string{"timeout", "missing_shell"} {
		t.Run(mode, func(t *testing.T) {
			check := "sleep 10"
			if mode == "missing_shell" {
				check = "true"
			}
			store, run, item, artifact, in := coordinatorQualityRegressionFixture(t, check)
			ctx := coordinatorLearningContext(context.Background(), store, run.Workspace, run.ID, item)
			finish, err := learning.BeginInvocation(ctx, in)
			if err != nil {
				t.Fatal(err)
			}
			if err := finish(ctx, []byte("tool produced deliverable")); err != nil {
				t.Fatal(err)
			}
			if _, err := store.Complete(context.Background(), run.Workspace, run.ID, item.ID, projectcoord.CompletionInput{Worker: item.Owner, Token: item.Lease.Token, Output: "deliverable"}); err != nil {
				t.Fatal(err)
			}
			verify, err := store.ClaimNext(run.Workspace, run.ID, projectcoord.ClaimOptions{Worker: "checker", Lease: time.Minute})
			if err != nil {
				t.Fatal(err)
			}
			completeCtx := context.Background()
			if mode == "timeout" {
				var cancel context.CancelFunc
				completeCtx, cancel = context.WithTimeout(completeCtx, 30*time.Millisecond)
				defer cancel()
			} else {
				t.Setenv("PATH", t.TempDir())
			}
			if _, err := store.Complete(completeCtx, run.Workspace, run.ID, verify.ID, projectcoord.CompletionInput{Worker: verify.Owner, Token: verify.Lease.Token, Output: "verifier reached host checks"}); err == nil {
				t.Fatal("fixture checker unexpectedly passed")
			}
			current, err := store.Get(run.Workspace, run.ID)
			if err != nil {
				t.Fatal(err)
			}
			var checks []projectcoord.CheckResult
			for _, i := range current.Items {
				if i.ID == verify.ID && i.Acceptance != nil {
					checks = i.Acceptance.Checks
				}
			}
			if len(checks) != 1 || checks[0].ExitCode >= 0 {
				t.Fatalf("fixture did not produce infrastructure failure: %+v", checks)
			}
			if err := reconcileCoordinatorLearning(context.Background(), store, run.Workspace, run.ID); err != nil {
				t.Fatal(err)
			}
			if count := coordinatorQualityOutcomeCount(t, artifact); count != 0 {
				t.Fatalf("checker infrastructure failure counted as business rejection: %d", count)
			}
		})
	}
}

func TestCoordinatorLearningRetryWithoutFreshInvocationRemainsUnknown(t *testing.T) {
	store, run, item, artifact, in := coordinatorQualityRegressionFixture(t, "exit 7")
	ctx := coordinatorLearningContext(context.Background(), store, run.Workspace, run.ID, item)
	finish, err := learning.BeginInvocation(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	if err := finish(ctx, []byte("old invocation before retry")); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Fail(run.Workspace, run.ID, item.ID, item.Owner, item.Lease.Token, projectcoord.Failure{Code: projectcoord.FailureWorkerInterrupted, Summary: "retry needed"}, ""); err != nil {
		t.Fatal(err)
	}
	next, err := store.ClaimNext(run.Workspace, run.ID, projectcoord.ClaimOptions{Worker: "replacement", Lease: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	// The replacement may replay an older durable operation without re-running
	// the artifact. That supplies no fresh quality attribution for this attempt.
	if _, err := store.Complete(context.Background(), run.Workspace, run.ID, next.ID, projectcoord.CompletionInput{Worker: next.Owner, Token: next.Lease.Token, Output: "replayed result"}); err != nil {
		t.Fatal(err)
	}
	verify, err := store.ClaimNext(run.Workspace, run.ID, projectcoord.ClaimOptions{Worker: "checker", Lease: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Complete(context.Background(), run.Workspace, run.ID, verify.ID, projectcoord.CompletionInput{Worker: verify.Owner, Token: verify.Lease.Token, Output: "check"}); err == nil {
		t.Fatal("fixture checker unexpectedly passed")
	}
	if err := reconcileCoordinatorLearning(context.Background(), store, run.Workspace, run.ID); err != nil {
		t.Fatal(err)
	}
	if count := coordinatorQualityOutcomeCount(t, artifact); count != 0 {
		t.Fatalf("old invocation was assigned to replacement: count=%d", count)
	}
}
func TestCoordinatorLearningLegacyInvocationEvidenceRemainsUnknown(t *testing.T) {
	store, run, item, artifact, in := coordinatorQualityRegressionFixture(t, "exit 7")
	ctx := coordinatorLearningContext(context.Background(), store, run.Workspace, run.ID, item)
	finish, err := learning.BeginInvocation(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	if err := finish(ctx, []byte("legacy proof without claim provenance")); err != nil {
		t.Fatal(err)
	}
	dir := coordinatorLearningDirectory(run.Workspace, run.ID, item.ID)
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("fixture proof: %v %v", entries, err)
	}
	path := filepath.Join(dir, entries[0].Name())
	r, err := readCoordinatorLearningInvocation(path)
	if err != nil {
		t.Fatal(err)
	}
	r.Schema = 1
	r.WorkAttempt = 0
	r.ClaimSHA256 = ""
	if err := writeCoordinatorLearningInvocation(path, r); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Complete(context.Background(), run.Workspace, run.ID, item.ID, projectcoord.CompletionInput{Worker: item.Owner, Token: item.Lease.Token, Output: "deliverable"}); err != nil {
		t.Fatal(err)
	}
	verify, err := store.ClaimNext(run.Workspace, run.ID, projectcoord.ClaimOptions{Worker: "checker", Lease: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Complete(context.Background(), run.Workspace, run.ID, verify.ID, projectcoord.CompletionInput{Worker: verify.Owner, Token: verify.Lease.Token, Output: "check"}); err == nil {
		t.Fatal("fixture checker unexpectedly passed")
	}
	if err := reconcileCoordinatorLearning(context.Background(), store, run.Workspace, run.ID); err != nil {
		t.Fatal(err)
	}
	if count := coordinatorQualityOutcomeCount(t, artifact); count != 0 {
		t.Fatal("old proof silently gained claim attribution")
	}
}

func TestCoordinatorLearningArchivedOutcomeKeepsOriginalDecisionAfterVerifierRetest(t *testing.T) {
	store, run, item, artifact, in := coordinatorQualityRegressionFixture(t, "exit 7")
	if _, err := artifact.ConfigureQuality(context.Background(), "quality-test", learning.QualityPolicy{Kind: "skill", WindowSize: 10, MinimumSamples: 10, MaxFailureRate: .99, Action: "pause", Verifier: coordinatorQualityVerifier}); err != nil {
		t.Fatal(err)
	}
	ctx := coordinatorLearningContext(context.Background(), store, run.Workspace, run.ID, item)
	finish, err := learning.BeginInvocation(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	if err := finish(ctx, []byte("actual invocation result")); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Complete(context.Background(), run.Workspace, run.ID, item.ID, projectcoord.CompletionInput{Worker: item.Owner, Token: item.Lease.Token, Output: "deliverable"}); err != nil {
		t.Fatal(err)
	}
	verifier, err := store.ClaimNext(run.Workspace, run.ID, projectcoord.ClaimOptions{Worker: "first-checker", Lease: 10 * time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Complete(context.Background(), run.Workspace, run.ID, verifier.ID, projectcoord.CompletionInput{Worker: verifier.Owner, Token: verifier.Lease.Token, Output: "first failed check"}); err == nil {
		t.Fatal("fixture checker unexpectedly passed")
	}
	if err := reconcileCoordinatorLearning(context.Background(), store, run.Workspace, run.ID); err != nil {
		t.Fatal(err)
	}
	original, err := artifact.Read("quality-test")
	if err != nil || len(original.Quality.Records) != 1 || original.Quality.Records[0].Outcome == nil {
		t.Fatalf("original feedback missing: %+v %v", original, err)
	}
	first := original.Quality.Records[0]
	// Produce enough later host-verified outcomes to move the first immutable
	// result out of hot state, without model calls or synthetic ledger edits.
	for i := 0; i < 520; i++ {
		b, err := artifact.BindOutcome(context.Background(), "quality-test", fmt.Sprintf("later-business-%d", i))
		if err != nil {
			t.Fatal(err)
		}
		_, err = artifact.RecordOutcome(context.Background(), "quality-test", b, learning.HostOutcome{Passed: true, Verifier: coordinatorQualityVerifier, InvocationSHA256: learning.Digest([]byte(b.ID + ":invocation")), EvidenceSHA256: learning.Digest([]byte(b.ID + ":passed"))})
		if err != nil {
			t.Fatal(err)
		}
	}
	hot, err := artifact.Read("quality-test")
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range hot.Quality.Records {
		if r.Binding.ID == first.Binding.ID {
			t.Fatal("fixture did not archive original outcome")
		}
	}
	if _, err := store.Fail(run.Workspace, run.ID, verifier.ID, verifier.Owner, verifier.Lease.Token, projectcoord.Failure{Code: projectcoord.FailureWorkerExecution, Summary: "host reviewed checker environment"}, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Recover(run.Workspace, run.ID, verifier.ID, "fixed host checker", false); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SetChecks(run.Workspace, run.ID, []string{"true"}); err != nil {
		t.Fatal(err)
	}
	verifier, err = store.ClaimNext(run.Workspace, run.ID, projectcoord.ClaimOptions{Worker: "second-checker", Lease: 10 * time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Complete(context.Background(), run.Workspace, run.ID, verifier.ID, projectcoord.CompletionInput{Worker: verifier.Owner, Token: verifier.Lease.Token, Output: "changed host checker verdict"}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := reconcileCoordinatorLearning(context.Background(), store, run.Workspace, run.ID); err != nil {
			t.Fatalf("archived business outcome caused perpetual reconciliation conflict after retest: %v", err)
		}
	}
	restored, err := artifact.LookupOutcome("quality-test", first.Binding.ID)
	if err != nil || restored.Outcome == nil || restored.Decision == nil || *restored.Outcome != *first.Outcome || *restored.Decision != *first.Decision {
		t.Fatalf("later host retest changed original business decision: %+v %v", restored, err)
	}
}
