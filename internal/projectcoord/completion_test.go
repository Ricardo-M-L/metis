package projectcoord

import (
	"context"
	"strings"
	"testing"
)

func TestCompletionRejectsEmptyOrBlockedEvidence(t *testing.T) {
	for _, output := range []string{"", " \n\t", "Cannot proceed.\nMETIS_PROJECT_STATUS: blocked", "ACCEPTANCE_INCOMPLETE: tests did not run"} {
		t.Run(strings.ReplaceAll(output, "\n", " "), func(t *testing.T) {
			workspace := t.TempDir()
			store := NewStore(t.TempDir())
			run, err := store.Create(CreateInput{Workspace: workspace, Goal: "Reject false completion"})
			if err != nil {
				t.Fatal(err)
			}
			item, err := store.ClaimNext(workspace, run.ID, ClaimOptions{Worker: "worker"})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := store.Complete(context.Background(), workspace, run.ID, item.ID, CompletionInput{Worker: "worker", Token: item.Lease.Token, Output: output}); err == nil {
				t.Fatal("completion accepted missing or explicitly incomplete evidence")
			}
			after, err := store.Get(workspace, run.ID)
			if err != nil {
				t.Fatal(err)
			}
			if itemByID(t, after, item.ID).Status != ItemRunning || itemByID(t, after, "synthesis").Status != ItemPending {
				t.Fatal("rejected completion advanced the graph")
			}
		})
	}
}

func TestCompletionRequiresOwnerAndClaimToken(t *testing.T) {
	workspace := t.TempDir()
	store := NewStore(t.TempDir())
	run, item := singleClaim(t, store, workspace, PhaseCustom, nil, nil)
	for _, input := range []CompletionInput{
		{Token: item.Lease.Token, Output: "evidence"},
		{Worker: item.Owner, Output: "evidence"},
		{Worker: "another-worker", Token: item.Lease.Token, Output: "evidence"},
	} {
		if _, err := store.Complete(context.Background(), workspace, run.ID, item.ID, input); err == nil {
			t.Fatal("completion bypassed ownership")
		}
	}
}
