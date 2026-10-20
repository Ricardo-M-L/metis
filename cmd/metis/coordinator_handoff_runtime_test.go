package main

import (
	"context"
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/handoff"
	"github.com/Ricardo-M-L/metis/internal/principal"
	"github.com/Ricardo-M-L/metis/internal/projectcoord"
)

func coordinatorHandoffRuntimeFixture(t *testing.T) (*runtime, projectcoord.Run, projectcoord.WorkItem, handoff.Record) {
	t.Helper()
	job, _, run, item := newPersistentJobCLIFixture(t, 0)
	store, err := openHandoffStore()
	if err != nil {
		t.Fatal(err)
	}
	record, _, err := store.Send(context.Background(), handoff.SendRequest{Scope: "jobs", From: "requester", To: "job:" + job.ID, Body: "process the assigned report", Task: &handoff.Task{ID: "task-one", Objective: "report result", Artifacts: []handoff.Artifact{{Path: "input.json", SHA256: strings.Repeat("a", 64)}}, Deliverables: []string{"output.json"}}})
	if err != nil {
		t.Fatal(err)
	}
	item.ID, item.Phase = "handoff-"+record.ID, projectcoord.PhaseImplementation
	item.Deliverables = append([]string(nil), record.Task.Deliverables...)
	item.Handoff = &projectcoord.HandoffBinding{MessageID: record.ID, TaskID: record.Task.ID, ContentSHA256: record.ContentSHA256, InputArtifacts: []projectcoord.Artifact{{Path: "input.json", SHA256: strings.Repeat("a", 64), Size: 10}}}
	a := handoff.BusinessAssignment{JobID: job.ID, Workspace: run.Workspace, RunID: run.ID, WorkItemID: item.ID, VerificationItemID: item.ID + "-verify", ConfigSHA256: job.ConfigSHA256}
	if _, err := store.ReconcileBusiness(context.Background(), record.ID, handoff.BusinessUpdate{Assignment: a, State: handoff.BusinessAccepted}); err != nil {
		t.Fatal(err)
	}
	return &runtime{sessionID: job.SessionID, loop: &agent.Loop{}}, run, item, record
}

func TestConfigureCoordinatorHandoffUsesExactFrozenAssignment(t *testing.T) {
	rt, run, item, record := coordinatorHandoffRuntimeFixture(t)
	if err := configureCoordinatorHandoff(rt, run, item); err != nil {
		t.Fatal(err)
	}
	b := rt.loop.DurablePeer
	if b == nil || b.MessageID != record.ID || b.Scope != "jobs" || b.Recipient != record.To || b.ContentSHA256 != record.ContentSHA256 || b.NotificationsOnly || b.Assignment == nil {
		t.Fatalf("inexact inbox configuration: %+v", b)
	}
	if a := b.Assignment; a.JobID != item.PrincipalBinding.JobID || a.RunID != run.ID || a.Workspace != run.Workspace || a.WorkItemID != item.ID || a.VerificationItemID != item.ID+"-verify" || a.ConfigSHA256 != item.PrincipalBinding.ConfigSHA256 {
		t.Fatalf("wrong expected assignment: %+v", a)
	}
	row, err := b.Store.Get(context.Background(), record.ID)
	if err != nil || row.State != "pending" || row.Attempts != 0 {
		t.Fatalf("configuration consumed input before transcript checkpoint: %+v %v", row, err)
	}
}

func TestConfigureCoordinatorHandoffRejectsWrongRuntimeAndContract(t *testing.T) {
	for name, mutate := range map[string]func(*runtime, *projectcoord.Run, *projectcoord.WorkItem){
		"unbound": func(_ *runtime, _ *projectcoord.Run, i *projectcoord.WorkItem) { i.PrincipalBinding = nil },
		"item":    func(_ *runtime, _ *projectcoord.Run, i *projectcoord.WorkItem) { i.ID = "different" },
		"phase": func(_ *runtime, _ *projectcoord.Run, i *projectcoord.WorkItem) {
			i.Phase = projectcoord.PhaseVerification
		},
		"message": func(_ *runtime, _ *projectcoord.Run, i *projectcoord.WorkItem) {
			i.Handoff.MessageID = "msg-" + strings.Repeat("0", 64)
		},
		"task": func(_ *runtime, _ *projectcoord.Run, i *projectcoord.WorkItem) { i.Handoff.TaskID = "another-task" },
		"digest": func(_ *runtime, _ *projectcoord.Run, i *projectcoord.WorkItem) {
			i.Handoff.ContentSHA256 = strings.Repeat("0", 64)
		},
		"workspace": func(_ *runtime, r *projectcoord.Run, _ *projectcoord.WorkItem) { r.Workspace = "/different" },
		"run":       func(_ *runtime, r *projectcoord.Run, _ *projectcoord.WorkItem) { r.ID = "different-run" },
		"job": func(_ *runtime, _ *projectcoord.Run, i *projectcoord.WorkItem) {
			i.PrincipalBinding.JobID = "different-job"
		},
		"config": func(_ *runtime, _ *projectcoord.Run, i *projectcoord.WorkItem) {
			i.PrincipalBinding.ConfigSHA256 = strings.Repeat("0", 64)
		},
		"transcript": func(rt *runtime, _ *projectcoord.Run, _ *projectcoord.WorkItem) { rt.sessionID = "wrong-session" },
		"input-sha": func(_ *runtime, _ *projectcoord.Run, i *projectcoord.WorkItem) {
			i.Handoff.InputArtifacts[0].SHA256 = strings.Repeat("0", 64)
		},
		"input-set":  func(_ *runtime, _ *projectcoord.Run, i *projectcoord.WorkItem) { i.Handoff.InputArtifacts = nil },
		"output-set": func(_ *runtime, _ *projectcoord.Run, i *projectcoord.WorkItem) { i.Deliverables[0] = "other.json" },
	} {
		t.Run(name, func(t *testing.T) {
			rt, run, item, _ := coordinatorHandoffRuntimeFixture(t)
			mutate(rt, &run, &item)
			if err := configureCoordinatorHandoff(rt, run, item); err == nil {
				t.Fatal("invalid handoff runtime was accepted")
			}
			if rt.loop.DurablePeer != nil {
				t.Fatal("invalid contract published an inbox")
			}
		})
	}
}

func TestConfigureCoordinatorOrdinaryPrincipalReceivesOnlyNotifications(t *testing.T) {
	job, _, run, item := newPersistentJobCLIFixture(t, 0)
	rt := &runtime{sessionID: job.SessionID, loop: &agent.Loop{}}
	if err := configureCoordinatorHandoff(rt, run, item); err != nil {
		t.Fatal(err)
	}
	b := rt.loop.DurablePeer
	if b == nil || !b.NotificationsOnly || b.MessageID != "" || b.Assignment != nil || b.Recipient != "job:"+job.ID {
		t.Fatalf("ordinary principal may consume structured assignments: %+v", b)
	}
}

func TestConfigureCoordinatorVerifierDoesNotAcquirePrincipalInbox(t *testing.T) {
	rt := &runtime{loop: &agent.Loop{}}
	item := projectcoord.WorkItem{ID: "verify", Phase: projectcoord.PhaseVerification}
	if err := configureCoordinatorHandoff(rt, projectcoord.Run{}, item); err != nil {
		t.Fatal(err)
	}
	if rt.loop.DurablePeer != nil {
		t.Fatal("independent verifier acquired a principal inbox")
	}
	// Malformed bound work does require a functioning runtime.
	if err := configureCoordinatorHandoff(nil, projectcoord.Run{}, projectcoord.WorkItem{PrincipalBinding: &principal.Binding{}}); err == nil {
		t.Fatal("missing bound runtime accepted")
	}
}
