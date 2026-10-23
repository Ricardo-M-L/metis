package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/internal/config"
	"github.com/Ricardo-M-L/metis/internal/handoff"
	"github.com/Ricardo-M-L/metis/internal/principal"
	"github.com/Ricardo-M-L/metis/internal/projectcoord"
)

func coordinatorHandoffFixture(t *testing.T) (*coordinatorService, principal.Job) {
	t.Helper()
	job, store, _, _ := newPersistentJobCLIFixture(t, 0)
	run := coordinatorHandoffEmptyRun(t, store, job.Definition.Workspace)
	opts := coordinatorServiceOptions{runID: run.ID, workspace: run.Workspace, agentProfile: "host-writer", verifierProfile: "reviewer", jobIDs: coordinatorServiceJobIDs{job.ID}, workers: 2, poll: 50 * time.Millisecond, lease: time.Minute}
	service := newCoordinatorService(store, opts, coordinatorServiceTestToken, "/unused-metis", config.Home(), io.Discard)
	if err := service.initializeHandoffs(); err != nil {
		t.Fatal(err)
	}
	return service, job
}
func coordinatorHandoffEmptyRun(t *testing.T, store *projectcoord.Store, workspace string) projectcoord.Run {
	t.Helper()
	run, err := store.Create(projectcoord.CreateInput{Workspace: workspace, Goal: "host handoff test", Items: []projectcoord.WorkItemInput{{ID: "seed", Subject: "seed", Prompt: "seed"}}, CheckCommands: []string{"exit 0"}})
	if err != nil {
		t.Fatal(err)
	}
	seed, err := store.ClaimNext(workspace, run.ID, projectcoord.ClaimOptions{Worker: "fixture"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.Complete(context.Background(), workspace, run.ID, seed.ID, projectcoord.CompletionInput{Worker: seed.Owner, Token: seed.Lease.Token, Output: "seed complete"}); err != nil {
		t.Fatal(err)
	}
	run, err = store.Get(workspace, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	return run
}
func coordinatorHandoffSend(t *testing.T, s *coordinatorService, job principal.Job, task *handoff.Task) handoff.Record {
	t.Helper()
	record, _, err := s.handoffStore.Send(context.Background(), handoff.SendRequest{Scope: "jobs", From: "sender", To: "job:" + job.ID, Body: "Please perform this structured task.", Task: task})
	if err != nil {
		t.Fatal(err)
	}
	return record
}
func coordinatorHandoffReconcile(t *testing.T, s *coordinatorService) {
	t.Helper()
	if err := s.reconcileHandoffs(context.Background()); err != nil {
		t.Fatal(err)
	}
}
func coordinatorHandoffRun(t *testing.T, s *coordinatorService) projectcoord.Run {
	t.Helper()
	run, err := s.store.Get(s.opts.workspace, s.opts.runID)
	if err != nil {
		t.Fatal(err)
	}
	return run
}

func TestCoordinatorHandoffOnlyAllowlistedStructuredTasksWakeJobs(t *testing.T) {
	s, job := coordinatorHandoffFixture(t)
	coordinatorHandoffSend(t, s, job, nil)
	other, err := persistentJobStore().Create("other", job.Definition)
	if err != nil {
		t.Fatal(err)
	}
	coordinatorHandoffSend(t, s, other, &handoff.Task{ID: "task-other", Objective: "not allowlisted"})
	task := coordinatorHandoffSend(t, s, job, &handoff.Task{ID: "task", Objective: "Do the work"})
	coordinatorHandoffReconcile(t, s)
	run := coordinatorHandoffRun(t, s)
	if len(run.Items) != 3 {
		t.Fatalf("unexpected admission count: %d", len(run.Items))
	}
	rec, err := s.handoffStore.Get(context.Background(), task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Business == nil || rec.Business.State != handoff.BusinessAccepted || rec.Business.Assignment.JobID != job.ID || rec.Business.SourceRevision == 0 {
		t.Fatalf("missing accepted graph receipt: %+v", rec.Business)
	}
	if run.Items[1].PrincipalBinding == nil || run.Items[1].PrincipalBinding.JobID != job.ID || run.Items[2].AgentProfile != s.opts.verifierProfile || run.Items[2].PrincipalBinding != nil {
		t.Fatal("frozen job/host verification routing changed")
	}
	// The default event-only service never admits tasks from the durable mailbox.
	run2 := coordinatorHandoffEmptyRun(t, s.store, s.opts.workspace)
	opts := s.opts
	opts.runID = run2.ID
	opts.jobIDs = nil
	legacy := newCoordinatorService(s.store, opts, coordinatorServiceTestToken, "unused", s.home, io.Discard)
	if err = legacy.initializeHandoffs(); err != nil {
		t.Fatal(err)
	}
	coordinatorHandoffReconcile(t, legacy)
	if got := coordinatorHandoffRun(t, legacy); len(got.Items) != 1 {
		t.Fatal("empty allowlist started paid work")
	}
}

func TestCoordinatorHandoffPauseBusyBudgetAndUnknownUsageHoldUntilAllowed(t *testing.T) {
	for _, condition := range []string{"paused", "busy", "budget", "unknown"} {
		t.Run(condition, func(t *testing.T) {
			s, job := coordinatorHandoffFixture(t)
			if condition == "budget" {
				d := job.Definition
				d.Budget.MaxInputTokens = 1
				var err error
				job, err = persistentJobStore().Create("budget", d)
				if err != nil {
					t.Fatal(err)
				}
				s.opts.jobIDs = coordinatorServiceJobIDs{job.ID}
				if err = s.initializeHandoffs(); err != nil {
					t.Fatal(err)
				}
			}
			var lease principal.Lease
			switch condition {
			case "paused":
				if _, err := persistentJobStore().SetStatus(job.ID, principal.Paused); err != nil {
					t.Fatal(err)
				}
			case "busy":
				var err error
				lease, err = persistentJobStore().ClaimExecution(job.Binding(), "other-execution", "other-worker", time.Minute)
				if err != nil {
					t.Fatal(err)
				}
			case "budget", "unknown":
				binding := job.Binding()
				r, err := s.store.Create(projectcoord.CreateInput{Workspace: s.opts.workspace, Goal: "existing accounting", Items: []projectcoord.WorkItemInput{{ID: "usage", Subject: "usage", Prompt: "usage", AgentProfile: binding.Definition.Profile.Name, PrincipalBinding: &binding}}})
				if err != nil {
					t.Fatal(err)
				}
				if _, err = persistentJobStore().RegisterRun(binding, r.ID); err != nil {
					t.Fatal(err)
				}
				p := principal.RunUsage{RunID: r.ID, SourceRevision: 100, Status: "blocked"}
				if condition == "budget" {
					p.InputTokens = 1
				} else {
					p.UnresolvedRequests = 1
				}
				if _, err = persistentJobStore().RecordRunUsage(binding, p); err != nil {
					t.Fatal(err)
				}
			}
			rec := coordinatorHandoffSend(t, s, job, &handoff.Task{ID: "task", Objective: "do work"})
			coordinatorHandoffReconcile(t, s)
			if len(coordinatorHandoffRun(t, s).Items) != 1 {
				t.Fatal("held principal received a graph assignment")
			}
			stored, _ := s.handoffStore.Get(context.Background(), rec.ID)
			if stored.Business != nil {
				t.Fatal("held message was reserved prematurely")
			}
			expectedReason := map[string]string{"paused": handoff.WaitingJobPaused, "busy": handoff.WaitingJobBusy, "budget": handoff.WaitingBudgetExhausted, "unknown": handoff.WaitingUsageUnresolved}[condition]
			if stored.Admission == nil || stored.Admission.WaitingReason != expectedReason || stored.Admission.LastCheckedAt.IsZero() || stored.Admission.RecoveryHint == "" || stored.Attempts != 0 {
				t.Fatalf("missing safe admission diagnostics: %+v", stored)
			}
			// A service restart must preserve the hold and its original budget/
			// unresolved usage. Rechecking is not a retry attempt.
			restarted := newCoordinatorService(s.store, s.opts, coordinatorServiceTestToken, "unused", s.home, io.Discard)
			if err := restarted.initializeHandoffs(); err != nil {
				t.Fatal(err)
			}
			coordinatorHandoffReconcile(t, restarted)
			afterRestart, err := restarted.handoffStore.Get(context.Background(), rec.ID)
			if err != nil || afterRestart.Admission == nil || afterRestart.Admission.LastCheckedAt != stored.Admission.LastCheckedAt || afterRestart.Attempts != 0 {
				t.Fatalf("restart reset or rewrote waiting state: %+v %v", afterRestart, err)
			}
			switch condition {
			case "paused":
				if _, err := persistentJobStore().SetStatus(job.ID, principal.Active); err != nil {
					t.Fatal(err)
				}
				// A restarted service may start while its allowlisted principal is paused.
			case "busy":
				if err := persistentJobStore().ReleaseExecution(job.Binding(), lease); err != nil {
					t.Fatal(err)
				}
			default:
				return
			}
			coordinatorHandoffReconcile(t, s)
			if len(coordinatorHandoffRun(t, s).Items) != 3 {
				t.Fatal("restored principal did not resume automatically")
			}
			admitted, err := s.handoffStore.Get(context.Background(), rec.ID)
			if err != nil || admitted.Admission != nil || admitted.Business == nil || admitted.Business.State != handoff.BusinessAccepted {
				t.Fatalf("successful admission kept stale waiting reason: %+v %v", admitted, err)
			}
		})
	}
}

func TestCoordinatorHandoffRouteDriftWaitsWithoutLeakingConfigAndRecovers(t *testing.T) {
	s, job := coordinatorHandoffFixture(t)
	rec := coordinatorHandoffSend(t, s, job, &handoff.Task{ID: "route-hold", Objective: "wait for the saved route"})
	path := filepath.Join(s.home, "config.toml")
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	changed := strings.Replace(string(original), "example.invalid", "private-route.invalid", 1)
	if changed == string(original) {
		t.Fatal("fixture did not contain the expected route")
	}
	if err = os.WriteFile(path, []byte(changed), 0600); err != nil {
		t.Fatal(err)
	}
	coordinatorHandoffReconcile(t, s)
	held, err := s.handoffStore.Get(context.Background(), rec.ID)
	if err != nil || held.Admission == nil || held.Admission.WaitingReason != handoff.WaitingRouteUnavailable || held.Business != nil {
		t.Fatalf("route drift was silently skipped or admitted: %+v %v", held, err)
	}
	view, _ := json.Marshal(held.Admission.SafeStatus())
	if strings.Contains(string(view), "private-route.invalid") || strings.Contains(string(view), s.home) || strings.Contains(string(view), "JOB_TEST_KEY") {
		t.Fatalf("private configuration leaked in waiting status: %s", view)
	}
	if len(coordinatorHandoffRun(t, s).Items) != 1 {
		t.Fatal("route drift admitted work")
	}
	if err = os.WriteFile(path, original, 0600); err != nil {
		t.Fatal(err)
	}
	restarted := newCoordinatorService(s.store, s.opts, coordinatorServiceTestToken, "unused", s.home, io.Discard)
	if err = restarted.initializeHandoffs(); err != nil {
		t.Fatal(err)
	}
	coordinatorHandoffReconcile(t, restarted)
	admitted, err := restarted.handoffStore.Get(context.Background(), rec.ID)
	if err != nil || admitted.Admission != nil || admitted.Business == nil || len(coordinatorHandoffRun(t, restarted).Items) != 3 {
		t.Fatalf("restored route did not resume after restart: %+v %v", admitted, err)
	}
}

func TestCoordinatorHandoffRetiredJobRejectsRatherThanWaitingForever(t *testing.T) {
	s, job := coordinatorHandoffFixture(t)
	rec := coordinatorHandoffSend(t, s, job, &handoff.Task{ID: "retired-hold", Objective: "do not start retired work"})
	if _, err := persistentJobStore().SetStatus(job.ID, principal.Retired); err != nil {
		t.Fatal(err)
	}
	coordinatorHandoffReconcile(t, s)
	stored, err := s.handoffStore.Get(context.Background(), rec.ID)
	if err != nil || stored.Business == nil || stored.Business.State != handoff.BusinessRejected || stored.Business.Failure.Code != "job_retired" || stored.Admission != nil || stored.Attempts != 0 {
		t.Fatalf("retired job did not produce a terminal receipt: %+v %v", stored, err)
	}
	coordinatorHandoffReconcile(t, s)
	if len(coordinatorHandoffRun(t, s).Items) != 1 {
		t.Fatal("retired job was admitted")
	}
}

func TestCoordinatorHandoffReservationAndEnqueueSurviveServiceRestart(t *testing.T) {
	s, job := coordinatorHandoffFixture(t)
	rec := coordinatorHandoffSend(t, s, job, &handoff.Task{ID: "crash-window", Objective: "work", Deliverables: []string{"result.txt"}})
	a := coordinatorHandoffAssignment(s.opts, job.Binding(), rec.ID)
	// Crash after the handoff-store commit, before the graph-store commit.
	if _, err := s.handoffStore.ReconcileBusiness(context.Background(), rec.ID, handoff.BusinessUpdate{Assignment: a, State: handoff.BusinessAccepted}); err != nil {
		t.Fatal(err)
	}
	restarted := newCoordinatorService(s.store, s.opts, coordinatorServiceTestToken, "unused", s.home, io.Discard)
	if err := restarted.initializeHandoffs(); err != nil {
		t.Fatal(err)
	}
	coordinatorHandoffReconcile(t, restarted)
	// Crash after enqueue, before inbox ACK and before receipt projection.
	coordinatorHandoffReconcile(t, restarted)
	if len(coordinatorHandoffRun(t, restarted).Items) != 3 {
		t.Fatal("restart duplicated graph pair")
	}
	claim, err := restarted.handoffStore.ClaimID(context.Background(), rec.ID, "jobs", "job:"+job.ID, "receiver", time.Minute, a)
	if err != nil || claim == nil {
		t.Fatalf("claim=%v error=%v", claim, err)
	}
	if err = restarted.handoffStore.Ack(context.Background(), rec.ID, claim.Token, strings.Repeat("a", 64)); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(s.opts.workspace, "result.txt"), []byte("verified output"), 0600); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		item, err := s.store.ClaimNext(s.opts.workspace, s.opts.runID, projectcoord.ClaimOptions{Worker: "receiver"})
		if err != nil {
			t.Fatal(err)
		}
		if _, err = s.store.Complete(context.Background(), s.opts.workspace, s.opts.runID, item.ID, projectcoord.CompletionInput{Worker: item.Owner, Token: item.Lease.Token, Output: "Completed with concrete evidence."}); err != nil {
			t.Fatal(err)
		}
	}
	// No normal child exit is needed: a fresh reconciler projects archived ACKs.
	final := newCoordinatorService(s.store, s.opts, coordinatorServiceTestToken, "unused", s.home, io.Discard)
	if err = final.initializeHandoffs(); err != nil {
		t.Fatal(err)
	}
	coordinatorHandoffReconcile(t, final)
	got, err := s.handoffStore.Get(context.Background(), rec.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != "acknowledged" || got.Business == nil || got.Business.State != handoff.BusinessCompleted || len(got.Business.Artifacts) != 1 || got.Business.Acceptance == nil || len(got.Business.Acceptance.Checks) != 1 || !got.Business.Acceptance.Passed {
		t.Fatalf("business completion lost: %+v", got.Business)
	}
	before := got.Business.CompletedAt
	coordinatorHandoffReconcile(t, final)
	got, _ = s.handoffStore.Get(context.Background(), rec.ID)
	if !got.Business.CompletedAt.Equal(before) {
		t.Fatal("completion was rewritten")
	}
}

func TestCoordinatorHandoffCompetitionAcrossServicesAndRunsHasOneAssignment(t *testing.T) {
	s, job := coordinatorHandoffFixture(t)
	rec := coordinatorHandoffSend(t, s, job, &handoff.Task{ID: "one", Objective: "exactly one assignment"})
	const n = 8
	services := make([]*coordinatorService, n)
	runs := map[string]bool{}
	for i := range services {
		opts := s.opts
		if i%2 == 1 {
			run := coordinatorHandoffEmptyRun(t, s.store, s.opts.workspace)
			opts.runID = run.ID
		}
		services[i] = newCoordinatorService(projectCoordinatorStore(), opts, coordinatorServiceTestToken, "unused", s.home, io.Discard)
		if err := services[i].initializeHandoffs(); err != nil {
			t.Fatal(err)
		}
		runs[opts.runID] = true
	}
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for _, service := range services {
		wg.Add(1)
		go func(s *coordinatorService) { defer wg.Done(); errs <- s.reconcileHandoffs(context.Background()) }(service)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.handoffStore.Get(context.Background(), rec.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Business == nil {
		t.Fatal("no winning assignment")
	}
	count := 0
	for id := range runs {
		run, err := s.store.Get(s.opts.workspace, id)
		if err != nil {
			t.Fatal(err)
		}
		for _, item := range run.Items {
			if item.Handoff != nil && item.Handoff.MessageID == rec.ID {
				count++
			}
		}
	}
	if count != 1 {
		t.Fatalf("message admitted %d times across competing runs", count)
	}
}

func TestCoordinatorHandoffStaleReservationCannotRegressAdvancedReceipt(t *testing.T) {
	for _, state := range []string{handoff.BusinessAccepted, handoff.BusinessRunning} {
		t.Run(state, func(t *testing.T) {
			s, job := coordinatorHandoffFixture(t)
			stale := coordinatorHandoffSend(t, s, job, &handoff.Task{ID: "stale", Objective: "one authoritative assignment"})
			assignment := coordinatorHandoffAssignment(s.opts, job.Binding(), stale.ID)
			// A second host finishes admission and projects a graph revision after
			// this host has already read a message with no business reservation.
			coordinatorHandoffReconcile(t, s)
			if state == handoff.BusinessRunning {
				if _, err := s.store.ClaimNext(s.opts.workspace, s.opts.runID, projectcoord.ClaimOptions{Worker: "winning-host"}); err != nil {
					t.Fatal(err)
				}
				coordinatorHandoffReconcile(t, s)
			}
			before, err := s.handoffStore.Get(context.Background(), stale.ID)
			if err != nil || stale.Business != nil || before.Business == nil || before.Business.State != state || before.Business.SourceRevision == 0 {
				t.Fatalf("fixture did not advance past the caller's snapshot: %+v %v", before.Business, err)
			}
			graphBefore := coordinatorHandoffRun(t, s)
			// A loaded Run contains an unexported archive lookup callback, which
			// cannot be compared with DeepEqual. Compare its durable JSON state.
			graphBeforeJSON, err := json.Marshal(graphBefore)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.handoffStore.ReconcileBusiness(context.Background(), stale.ID, handoff.BusinessUpdate{Assignment: assignment, State: handoff.BusinessAccepted}); !errors.Is(err, handoff.ErrBusinessStale) {
				t.Fatalf("store allowed revision zero to overwrite an advanced receipt: %v", err)
			}
			if _, reserved, err := s.reserveHandoffBusiness(context.Background(), stale.ID, assignment); err != nil || reserved {
				t.Fatalf("old host must defer to the advanced receipt: reserved=%t err=%v", reserved, err)
			}
			loser := assignment
			loser.RunID = "another-project"
			if _, reserved, err := s.reserveHandoffBusiness(context.Background(), stale.ID, loser); err != nil || reserved {
				t.Fatalf("another run acquired the winning assignment: reserved=%t err=%v", reserved, err)
			}
			coordinatorHandoffReconcile(t, s)
			after, err := s.handoffStore.Get(context.Background(), stale.ID)
			graphAfter := coordinatorHandoffRun(t, s)
			graphAfterJSON, marshalErr := json.Marshal(graphAfter)
			if err != nil || marshalErr != nil || !reflect.DeepEqual(before, after) || string(graphBeforeJSON) != string(graphAfterJSON) {
				t.Fatal("stale admission changed receipt, timestamps or durable graph pair")
			}
		})
	}
}

func TestCoordinatorHandoffReservationRetainsValidationAndStorageErrors(t *testing.T) {
	s, job := coordinatorHandoffFixture(t)
	rec := coordinatorHandoffSend(t, s, job, &handoff.Task{ID: "errors", Objective: "do not hide host errors"})
	assignment := coordinatorHandoffAssignment(s.opts, job.Binding(), rec.ID)
	invalid := assignment
	invalid.WorkItemID = ""
	if _, reserved, err := s.reserveHandoffBusiness(context.Background(), rec.ID, invalid); err == nil || reserved {
		t.Fatal("reservation hid invalid host assignment")
	}
	if _, reserved, err := s.reserveHandoffBusiness(context.Background(), "msg-"+strings.Repeat("f", 64), assignment); !errors.Is(err, os.ErrNotExist) || reserved {
		t.Fatal("reservation hid missing durable input")
	}
}

func TestCoordinatorHandoffStaleAdmissionDecisionKeepsNewerGraphReceipt(t *testing.T) {
	for _, decision := range []string{"retired", "cancelled"} {
		t.Run(decision, func(t *testing.T) {
			s, job := coordinatorHandoffFixture(t)
			rec := coordinatorHandoffSend(t, s, job, &handoff.Task{ID: "stale-decision", Objective: "keep newer host evidence"})
			assignment := coordinatorHandoffAssignment(s.opts, job.Binding(), rec.ID)
			stale, err := s.handoffStore.ReconcileBusiness(context.Background(), rec.ID, handoff.BusinessUpdate{Assignment: assignment, State: handoff.BusinessAccepted})
			if err != nil {
				t.Fatal(err)
			}
			oldGraph := coordinatorHandoffRun(t, s)
			coordinatorHandoffReconcile(t, s)
			before, err := s.handoffStore.Get(context.Background(), rec.ID)
			if err != nil || before.Business == nil || before.Business.SourceRevision <= oldGraph.Revision {
				t.Fatal("winning host did not advance its graph receipt")
			}
			if decision == "retired" {
				if _, err := persistentJobStore().SetStatus(job.ID, principal.Retired); err != nil {
					t.Fatal(err)
				}
			} else {
				oldGraph.Status = projectcoord.RunCancelled
			}
			if ready, err := s.handoffAdmissionReady(context.Background(), stale, assignment, oldGraph); err != nil || ready {
				t.Fatalf("stale terminal decision surfaced a service error: ready=%t err=%v", ready, err)
			}
			after, err := s.handoffStore.Get(context.Background(), rec.ID)
			if err != nil || !reflect.DeepEqual(before, after) {
				t.Fatal("stale terminal decision replaced newer graph evidence")
			}
		})
	}
}

func TestCoordinatorHandoffHostFailureCannotBecomeCompletionWithoutExplicitRecovery(t *testing.T) {
	s, job := coordinatorHandoffFixture(t)
	rec := coordinatorHandoffSend(t, s, job, &handoff.Task{ID: "failure", Objective: "fail then recover"})
	coordinatorHandoffReconcile(t, s)
	item, err := s.store.ClaimNext(s.opts.workspace, s.opts.runID, projectcoord.ClaimOptions{Worker: "worker"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.store.Fail(s.opts.workspace, s.opts.runID, item.ID, item.Owner, item.Lease.Token, projectcoord.Failure{Code: projectcoord.FailureProviderUsageUnresolved, Summary: "Earlier physical request is not settled."}, ""); err != nil {
		t.Fatal(err)
	}
	coordinatorHandoffReconcile(t, s)
	got, _ := s.handoffStore.Get(context.Background(), rec.ID)
	if got.Business.State != handoff.BusinessBlocked {
		t.Fatalf("unsettled usage projected %s", got.Business.State)
	}
	blockedRevision := got.Business.SourceRevision
	if _, err = s.store.AddWorkItem(s.opts.workspace, s.opts.runID, projectcoord.WorkItemInput{ID: "unrelated", Subject: "unrelated", Prompt: "unrelated"}); err != nil {
		t.Fatal(err)
	}
	coordinatorHandoffReconcile(t, s)
	got, _ = s.handoffStore.Get(context.Background(), rec.ID)
	if got.Business.State != handoff.BusinessBlocked || got.Business.SourceRevision != blockedRevision {
		t.Fatal("unrelated revision cleared safety hold")
	}
	if _, err = s.store.Recover(s.opts.workspace, s.opts.runID, item.ID, "provider authoritatively settled", false); err != nil {
		t.Fatal(err)
	}
	coordinatorHandoffReconcile(t, s)
	got, _ = s.handoffStore.Get(context.Background(), rec.ID)
	if got.Business.State != handoff.BusinessRunning || !got.Business.Recovery {
		t.Fatalf("explicit recovery did not resume: %+v", got.Business)
	}
	// Completed prose without passed host checks is never business completion.
	run := coordinatorHandoffRun(t, s)
	impl, verify := coordinatorHandoffItems(run, got.Business.Assignment)
	impl.Status = projectcoord.ItemCompleted
	verify.Status = projectcoord.ItemCompleted
	verify.Acceptance = &projectcoord.Acceptance{Status: "failed"}
	if update, err := coordinatorHandoffProjection(got, run, impl, verify, job.Binding()); err == nil && update != nil && update.State == handoff.BusinessCompleted {
		t.Fatal("failed host acceptance completed business")
	}
}

func TestCoordinatorEventJobRoutingUsesFrozenAllowlistAndConflictsOnChanges(t *testing.T) {
	s, job := coordinatorHandoffFixture(t)
	other, err := persistentJobStore().Create("other", job.Definition)
	if err != nil {
		t.Fatal(err)
	}
	base := map[string]any{"source": "host-test", "id": "event-1", "subject": "work", "prompt": "do work", "job_id": job.ID}
	request := func(v map[string]any) int {
		data, _ := json.Marshal(v)
		return coordinatorServiceRequest(s.handler, "POST", "/v1/events", string(data), coordinatorServiceTestToken).Code
	}
	if status := request(base); status != 202 {
		t.Fatalf("bound event status=%d", status)
	}
	if status := request(base); status != 202 {
		t.Fatalf("bound event replay status=%d", status)
	}
	run := coordinatorHandoffRun(t, s)
	if len(run.Items) != 3 || run.Items[1].PrincipalBinding == nil || run.Items[1].PrincipalBinding.JobID != job.ID || run.Items[1].AgentProfile != job.Definition.Profile.Name || run.Items[2].PrincipalBinding != nil {
		t.Fatal("event escaped job binding")
	}
	base["job_id"] = other.ID
	if status := request(base); status != 403 {
		t.Fatalf("unallowlisted job status=%d", status)
	}
	s.handler.jobBindings[other.ID] = other.Binding()
	if status := request(base); status != 409 {
		t.Fatalf("same event changed principal status=%d", status)
	}
	delete(base, "job_id")
	if status := request(base); status != 409 {
		t.Fatalf("bound event downgraded to generic status=%d", status)
	}
	base["id"] = "generic"
	if status := request(base); status != 202 {
		t.Fatalf("legacy event status=%d", status)
	}
	run = coordinatorHandoffRun(t, s)
	if run.Items[len(run.Items)-2].PrincipalBinding != nil {
		t.Fatal("legacy event unexpectedly bound")
	}
}

func TestCoordinatorServiceJobStartupAcceptsPauseRejectsWrongWorkspaceAndRoute(t *testing.T) {
	s, job := coordinatorHandoffFixture(t)
	if _, err := persistentJobStore().SetStatus(job.ID, principal.Paused); err != nil {
		t.Fatal(err)
	}
	if _, err := loadCoordinatorServiceJobs(s.opts, s.home); err != nil {
		t.Fatalf("paused job prevented startup: %v", err)
	}
	opts := s.opts
	opts.workspace = t.TempDir()
	if _, err := loadCoordinatorServiceJobs(opts, s.home); err == nil {
		t.Fatal("cross-workspace job accepted")
	}
	opts = s.opts
	opts.jobIDs = coordinatorServiceJobIDs{"job-missing"}
	if _, err := loadCoordinatorServiceJobs(opts, s.home); !errors.Is(err, principal.ErrNotFound) {
		t.Fatalf("missing job startup: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(s.home, "config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	data = []byte(strings.Replace(string(data), "example.invalid", "changed.invalid", 1))
	if err = os.WriteFile(filepath.Join(s.home, "config.toml"), data, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadCoordinatorServiceJobs(s.opts, s.home); err == nil {
		t.Fatal("changed route accepted")
	}
}

func TestCoordinatorSupervisorPersistsJobAllowlistInLaunchArguments(t *testing.T) {
	// This is pure launch-argument serialization; no POSIX supervisor fixture
	// or real shell, file permission, signal or launchctl support is required.
	root := t.TempDir()
	opts := coordinatorServiceOptions{runID: "run-id", workspace: filepath.Join(root, "project"), agentProfile: "implementation", verifierProfile: "verify", jobIDs: coordinatorServiceJobIDs{"job-one", "job-two"}}
	manifest := coordinatorSupervisorManifest{Executable: filepath.Join(root, "metis"), Label: "label", UserHome: root, MetisHome: filepath.Join(root, "metis-home")}
	data, err := coordinatorSupervisorPlist(manifest, opts, "private-token")
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	if strings.Count(text, "<string>--job</string>") != 2 || !strings.Contains(text, "<string>job-one</string>") || !strings.Contains(text, "<string>job-two</string>") {
		t.Fatal("launchd restart lost allowlisted jobs")
	}
}

func TestCoordinatorHandoffInvalidInputIsRejectedWithoutPaidAdmission(t *testing.T) {
	for _, kind := range []string{"mismatch", "missing", "directory", "escape", "overlap"} {
		t.Run(kind, func(t *testing.T) {
			s, job := coordinatorHandoffFixture(t)
			path := "input.txt"
			if err := os.WriteFile(filepath.Join(s.opts.workspace, path), []byte("input"), 0600); err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "missing":
				path = "missing.txt"
			case "directory":
				path = "directory"
				if err := os.Mkdir(filepath.Join(s.opts.workspace, path), 0700); err != nil {
					t.Fatal(err)
				}
			case "escape":
				path = "../outside"
			}
			task := &handoff.Task{ID: "bad-input", Objective: "reject invalid input", Artifacts: []handoff.Artifact{{Path: path, SHA256: strings.Repeat("a", 64)}}}
			if kind == "overlap" {
				task.Deliverables = []string{path}
			}
			rec := coordinatorHandoffSend(t, s, job, task)
			coordinatorHandoffReconcile(t, s)
			current, err := s.handoffStore.Get(context.Background(), rec.ID)
			if err != nil {
				t.Fatal(err)
			}
			if current.Business == nil || current.Business.State != handoff.BusinessRejected || current.Business.Failure == nil || current.Business.Failure.Code != "handoff_input_invalid" {
				t.Fatalf("invalid input has no durable rejection: %+v", current.Business)
			}
			if len(coordinatorHandoffRun(t, s).Items) != 1 {
				t.Fatal("invalid input admitted paid graph nodes")
			}
			coordinatorHandoffReconcile(t, s)
			if len(coordinatorHandoffRun(t, s).Items) != 1 {
				t.Fatal("rejected input silently retried")
			}
		})
	}
}

func TestCoordinatorHandoffLongUnicodeFailureRemainsAValidReceipt(t *testing.T) {
	s, job := coordinatorHandoffFixture(t)
	rec := coordinatorHandoffSend(t, s, job, &handoff.Task{ID: "unicode-failure", Objective: "report failure safely"})
	coordinatorHandoffReconcile(t, s)
	item, err := s.store.ClaimNext(s.opts.workspace, s.opts.runID, projectcoord.ClaimOptions{Worker: "worker"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.store.Fail(s.opts.workspace, s.opts.runID, item.ID, item.Owner, item.Lease.Token, projectcoord.Failure{Code: "task_failed", Summary: strings.Repeat("a", 4095) + "中"}, ""); err != nil {
		t.Fatal(err)
	}
	coordinatorHandoffReconcile(t, s)
	got, err := s.handoffStore.Get(context.Background(), rec.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Business == nil || got.Business.State != handoff.BusinessFailed || got.Business.Failure == nil || len(got.Business.Failure.Summary) != 4095 {
		t.Fatalf("UTF8 failure receipt lost: %+v", got.Business)
	}
}

func TestCoordinatorHandoffCancelledReservationReconcilesWithoutAdmittingNewMessages(t *testing.T) {
	s, job := coordinatorHandoffFixture(t)
	old := coordinatorHandoffSend(t, s, job, &handoff.Task{ID: "reserved-before-crash", Objective: "old work"})
	assignment := coordinatorHandoffAssignment(s.opts, job.Binding(), old.ID)
	if _, err := s.handoffStore.ReconcileBusiness(context.Background(), old.ID, handoff.BusinessUpdate{Assignment: assignment, State: handoff.BusinessAccepted}); err != nil {
		t.Fatal(err)
	}
	// A host cancellation journal observed after a process was offline.
	run := coordinatorHandoffRun(t, s)
	run.Status = projectcoord.RunCancelled
	run.Revision++
	run.CompletedAt = time.Time{}
	data, err := json.Marshal(run)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(s.store.Root(), coordinatorHandoffTextSHA(s.opts.workspace)[:32], run.ID+".json")
	if err = os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	fresh := coordinatorHandoffSend(t, s, job, &handoff.Task{ID: "new-after-cancel", Objective: "new task must remain unassigned"})
	restarted := newCoordinatorService(s.store, s.opts, coordinatorServiceTestToken, "unused", s.home, io.Discard)
	if err = restarted.initializeHandoffs(); err != nil {
		t.Fatal(err)
	}
	coordinatorHandoffReconcile(t, restarted)
	got, err := s.handoffStore.Get(context.Background(), old.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Business == nil || got.Business.State != handoff.BusinessCancelled {
		t.Fatalf("old reservation lost cancellation: %+v", got.Business)
	}
	got, err = s.handoffStore.Get(context.Background(), fresh.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Business != nil || len(coordinatorHandoffRun(t, s).Items) != 1 {
		t.Fatal("cancelled run consumed a fresh task")
	}
	if got.Admission == nil || got.Admission.WaitingReason != handoff.WaitingRunCancelled {
		t.Fatalf("unassigned message lost cancelled-host waiting reason: %+v", got.Admission)
	}
	activeRun := coordinatorHandoffEmptyRun(t, s.store, s.opts.workspace)
	activeOpts := s.opts
	activeOpts.runID = activeRun.ID
	active := newCoordinatorService(s.store, activeOpts, coordinatorServiceTestToken, "unused", s.home, io.Discard)
	if err = active.initializeHandoffs(); err != nil {
		t.Fatal(err)
	}
	coordinatorHandoffReconcile(t, active)
	got, err = active.handoffStore.Get(context.Background(), fresh.ID)
	if err != nil || got.Admission != nil || got.Business == nil || got.Business.Assignment.RunID != activeRun.ID || len(coordinatorHandoffRun(t, active).Items) != 3 {
		t.Fatalf("cancelled host prevented an allowed successor from admitting fresh work: %+v %v", got, err)
	}
}

func TestCoordinatorServeUsesFixedWorkspaceForProviderOverlay(t *testing.T) {
	s, _ := coordinatorHandoffFixture(t)
	// Creating the job reads the selected workspace's overlay, while this CLI is
	// intentionally invoked from a different directory with --cwd.
	if err := os.WriteFile(filepath.Join(s.opts.workspace, ".metis", "config.toml"), []byte("[provider.custom.fixture]\ntransport=\"anthropic_messages\"\nbase_url=\"https://workspace-overlay.invalid/v1\"\nmodel=\"model\"\napi_key_env=\"JOB_TEST_KEY\"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	output, err := captureCoordinatorOperationOutput(t, func() error {
		return cmdJobCreate([]string{"--name", "workspace overlay", "--cwd", s.opts.workspace, "--agent", "reviewer", "--json"})
	})
	if err != nil {
		t.Fatal(err)
	}
	var job principal.Job
	if err = json.Unmarshal([]byte(output), &job); err != nil {
		t.Fatal(err)
	}
	opts := s.opts
	opts.jobIDs = coordinatorServiceJobIDs{job.ID}
	if _, err = loadCoordinatorServiceJobs(opts, s.home); err == nil {
		t.Fatal("fixture did not distinguish caller and selected workspace routes")
	}
	token := filepath.Join(s.home, "service-token")
	if err = os.WriteFile(token, []byte(coordinatorServiceTestToken), 0600); err != nil {
		t.Fatal(err)
	}
	before, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()
	err = cmdProjectCoordinatorServe(ctx, []string{s.opts.runID, "--cwd", s.opts.workspace, "--agent", "reviewer", "--token-file", token, "--job", job.ID, "--poll", "50ms"})
	if err != nil {
		t.Fatalf("correct workspace overlay prevented startup: %v", err)
	}
	after, err := os.Getwd()
	if err != nil || after != before {
		t.Fatalf("service did not restore caller cwd: before=%s after=%s error=%v", before, after, err)
	}
}

func TestCoordinatorHandoffFastRecoveryPublishesNewFailureAndCanCancel(t *testing.T) {
	s, job := coordinatorHandoffFixture(t)
	rec := coordinatorHandoffSend(t, s, job, &handoff.Task{ID: "fast-recovery", Objective: "new failure evidence must be visible"})
	coordinatorHandoffReconcile(t, s)
	fail := func(summary string) {
		t.Helper()
		item, err := s.store.ClaimNext(s.opts.workspace, s.opts.runID, projectcoord.ClaimOptions{Worker: "worker"})
		if err != nil {
			t.Fatal(err)
		}
		if _, err = s.store.Fail(s.opts.workspace, s.opts.runID, item.ID, item.Owner, item.Lease.Token, projectcoord.Failure{Code: "task_failed", Summary: summary}, ""); err != nil {
			t.Fatal(err)
		}
	}
	fail("first failure")
	coordinatorHandoffReconcile(t, s)
	before, err := s.handoffStore.Get(context.Background(), rec.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.store.Recover(s.opts.workspace, s.opts.runID, before.Business.Assignment.WorkItemID, "changed host condition", false); err != nil {
		t.Fatal(err)
	}
	// The new attempt fails before the next service poll observes running.
	fail("second failure")
	coordinatorHandoffReconcile(t, s)
	after, err := s.handoffStore.Get(context.Background(), rec.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Business.State != handoff.BusinessFailed || !after.Business.Recovery || after.Business.Failure.Summary != "second failure" || after.Business.SourceRevision <= before.Business.SourceRevision {
		t.Fatalf("fast recovery lost new failure: %+v", after.Business)
	}
	run := coordinatorHandoffRun(t, s)
	run.Status = projectcoord.RunCancelled
	run.Revision++
	run.CompletedAt = time.Time{}
	data, err := json.Marshal(run)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(s.store.Root(), coordinatorHandoffTextSHA(s.opts.workspace)[:32], run.ID+".json")
	if err = os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	token := filepath.Join(s.home, "service-token")
	if err = os.WriteFile(token, []byte(coordinatorServiceTestToken), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()
	if err = cmdProjectCoordinatorServe(ctx, []string{s.opts.runID, "--cwd", s.opts.workspace, "--agent", "reviewer", "--token-file", token, "--job", job.ID, "--poll", "50ms"}); err != nil {
		t.Fatalf("cancelled assignment could not reconcile after restart: %v", err)
	}
	after, err = s.handoffStore.Get(context.Background(), rec.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Business.State != handoff.BusinessCancelled {
		t.Fatalf("failed receipt did not enter cancellation: %+v", after.Business)
	}
}

func TestCoordinatorHandoffPaginationCannotStarveNewTasksBehindOtherRuns(t *testing.T) {
	s, job := coordinatorHandoffFixture(t)
	original := coordinatorHandoffSend(t, s, job, &handoff.Task{ID: "historical", Objective: "owned by another run"})
	foreignOpts := s.opts
	foreignOpts.runID = "other-service-run"
	assignment := coordinatorHandoffAssignment(foreignOpts, job.Binding(), original.ID)
	history, err := s.handoffStore.ReconcileBusiness(context.Background(), original.ID, handoff.BusinessUpdate{Assignment: assignment, State: handoff.BusinessAccepted})
	if err != nil {
		t.Fatal(err)
	}
	// Build a valid private-store fixture past one full 1024-row dispatch page.
	// These records model reservations whose owning service has not restarted.
	for i := 1; i < 1024; i++ {
		rec := history
		rec.ID = fmt.Sprintf("msg-%064x", i)
		b := *history.Business
		b.Assignment = coordinatorHandoffAssignment(foreignOpts, job.Binding(), rec.ID)
		rec.Business = &b
		data, err := json.Marshal(rec)
		if err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(filepath.Join(s.handoffStore.Root(), "messages", rec.ID+".json"), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	fresh := coordinatorHandoffSend(t, s, job, &handoff.Task{ID: "new-arrival", Objective: "must reach admission"})
	coordinatorHandoffReconcile(t, s)
	if len(coordinatorHandoffRun(t, s).Items) != 1 || s.jobCursors[job.ID] == "" {
		t.Fatal("first dispatch page did not advance over foreign reservations")
	}
	coordinatorHandoffReconcile(t, s)
	got, err := s.handoffStore.Get(context.Background(), fresh.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Business == nil || got.Business.Assignment.RunID != s.opts.runID || len(coordinatorHandoffRun(t, s).Items) != 3 {
		t.Fatal("historical reservations starved the new task")
	}
}
