package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/internal/handoff"
	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/principal"
	"github.com/Ricardo-M-L/metis/internal/projectcoord"
	"github.com/Ricardo-M-L/metis/internal/tools/builtin"
	pubtool "github.com/Ricardo-M-L/metis/pkg/tool"
)

func TestCoordinatorHandoffCLIStatusReportsPreAdmissionWait(t *testing.T) {
	s, job := coordinatorHandoffFixture(t)
	if _, err := persistentJobStore().SetStatus(job.ID, principal.Paused); err != nil {
		t.Fatal(err)
	}
	rec := coordinatorHandoffSend(t, s, job, &handoff.Task{ID: "status-wait", Objective: "show why this task has not started"})
	coordinatorHandoffReconcile(t, s)
	output, err := captureCoordinatorOperationOutput(t, func() error {
		return cmdHandoff(context.Background(), []string{"status", rec.ID})
	})
	if err != nil {
		t.Fatal(err)
	}
	var got handoff.Record
	if err = json.Unmarshal([]byte(output), &got); err != nil {
		t.Fatal(err)
	}
	if got.Admission == nil || got.Admission.WaitingReason != handoff.WaitingJobPaused || got.Admission.LastCheckedAt.IsZero() || got.Admission.RecoveryHint == "" || got.Business != nil {
		t.Fatalf("CLI omitted pre-admission queue status: %s", output)
	}
}

func TestCoordinatorHandoffStatusRemainsFreshAcrossWorkerRecovery(t *testing.T) {
	f := newCoordinatorAdapterFixture(t)
	tool := builtin.NewMessageTeammate(permission.New(permission.ModeDefault), nil)
	input := map[string]any{"operation": "status", "message_id": "msg-" + strings.Repeat("a", 64)}
	if coordinatorGuardedOperation(tool, input) {
		t.Fatal("read-only business receipt lookup was classified as a writing operation")
	}
	first, err := f.guard.Execute(f.ctx, tool, input, func(context.Context) (*pubtool.Result, error) {
		return &pubtool.Result{Output: `{"business":{"state":"accepted"}}`}, nil
	})
	if err != nil || !strings.Contains(first.Output, "accepted") {
		t.Fatalf("initial status: result=%v err=%v", first, err)
	}
	if _, err := f.store.Fail(f.run.Workspace, f.run.ID, f.item.ID, f.item.Owner, f.item.Lease.Token, projectcoord.Failure{Code: projectcoord.FailureWorkerInterrupted, Summary: "receipt reader restarted"}, ""); err != nil {
		t.Fatal(err)
	}
	item, err := f.store.ClaimNext(f.run.Workspace, f.run.ID, projectcoord.ClaimOptions{Worker: "status-replacement", Lease: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := coordinatorOperationContext(context.Background(), f.store, f.run.Workspace, f.run.ID, item, item.Owner)
	defer cancel(nil)
	called := false
	second, err := pubtool.OperationExecutorFromContext(ctx).Execute(ctx, tool, input, func(context.Context) (*pubtool.Result, error) {
		called = true
		return &pubtool.Result{Output: `{"business":{"state":"completed"}}`}, nil
	})
	if err != nil || !called || !strings.Contains(second.Output, "completed") {
		t.Fatalf("replacement replayed an obsolete receipt: called=%v result=%v err=%v", called, second, err)
	}
	if operations := f.latest(t).Operations; len(operations) != 0 {
		t.Fatalf("status lookup polluted writing-operation journal: %d", len(operations))
	}
	if !coordinatorGuardedOperation(tool, map[string]any{"to": "job:example", "body": "execute task"}) {
		t.Fatal("adding status lookup removed send-side operation protection")
	}
}
