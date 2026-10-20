package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/handoff"
	"github.com/Ricardo-M-L/metis/internal/principal"
)

func TestOperationsSnapshotProjectsWaitingAndExcludesExecutionSecrets(t *testing.T) {
	s, j := coordinatorHandoffFixture(t)
	if _, err := persistentJobStore().SetStatus(j.ID, principal.Paused); err != nil {
		t.Fatal(err)
	}
	r := coordinatorHandoffSend(t, s, j, &handoff.Task{ID: "waiting", Objective: "PRIVATE_TASK_OBJECTIVE"})
	coordinatorHandoffReconcile(t, s)
	view, err := collectOperationsSnapshot(context.Background(), s.opts.workspace)
	if err != nil {
		t.Fatal(err)
	}
	if len(view.Jobs) != 1 || len(view.Handoffs) != 1 || view.Handoffs[0]["id"] != r.ID || view.Handoffs[0]["admission"] == nil {
		t.Fatalf("waiting missing: %+v", view.Handoffs)
	}
	raw, _ := json.Marshal(view)
	for _, private := range []string{"PRIVATE_TASK_OBJECTIVE", "FROZEN_JOB_ROLE", "provider_config_sha256", "credential_refs", "claim_token", "lease_token", "JOB_TEST_KEY", "check_commands"} {
		if strings.Contains(string(raw), private) {
			t.Fatalf("management projection leaked %s", private)
		}
	}
}

func TestOperationsActionRejectsOtherWorkspaceAndArbitraryCommands(t *testing.T) {
	j, _, _, _ := newPersistentJobCLIFixture(t, 0)
	if err := applyOperationsAction(context.Background(), t.TempDir(), operationsAction{Action: "pause_job", ID: j.ID}); err == nil {
		t.Fatal("cross workspace action accepted")
	}
	for _, raw := range []string{`{"action":"run_shell","command":"exit 0"}`, `{"action":"pause_job","id":"x","command":"exit 0"}`, `{"action":"pause_job","id":"x"} {}`, `{"action":"rollback_learning","kind":"skill","name":"xx","note":"review"}`} {
		if _, err := decodeOperationsAction([]byte(raw)); err == nil {
			t.Fatalf("unsafe action accepted: %s", raw)
		}
	}
	if err := applyOperationsAction(context.Background(), j.Definition.Workspace, operationsAction{Action: "pause_job", ID: j.ID}); err != nil {
		t.Fatal(err)
	}
	updated, err := persistentJobStore().Get(j.ID)
	if err != nil || updated.Status != principal.Paused {
		t.Fatalf("pause failed: %s %v", updated.Status, err)
	}
}
