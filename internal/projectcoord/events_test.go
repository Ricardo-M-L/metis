package projectcoord

import (
	"context"
	"sync"
	"testing"
)

func TestExternalEventDeduplicatesAcrossStoresAndReopensCompletedRun(t *testing.T) {
	workspace, root := t.TempDir(), t.TempDir()
	store := NewStore(root)
	run, item := singleClaim(t, store, workspace, PhaseCustom, []string{"exit 0"}, nil)
	if _, err := store.Complete(context.Background(), workspace, run.ID, item.ID, completionFor(item)); err != nil {
		t.Fatal(err)
	}
	input := EventInput{Source: "ci", ID: "build-42", AgentProfile: "maintainer", VerifierAgentProfile: "reviewer", Subject: "Repair failed build", Prompt: "Inspect and fix the CI failure"}
	var group sync.WaitGroup
	for i := 0; i < 6; i++ {
		group.Add(1)
		go func() {
			defer group.Done()
			if _, err := NewStore(root).EnqueueEvent(workspace, run.ID, input); err != nil {
				t.Error(err)
			}
		}()
	}
	group.Wait()
	after, err := NewStore(root).Get(workspace, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(after.Items) != 3 || after.Status != RunActive || !after.CompletedAt.IsZero() {
		t.Fatalf("duplicate/lost event: %+v", after)
	}
	work, err := store.ClaimNext(workspace, run.ID, ClaimOptions{Worker: "worker"})
	if err != nil {
		t.Fatal(err)
	}
	if work.AgentProfile != "maintainer" || work.EventID != input.ID {
		t.Fatalf("event routing lost: %+v", work)
	}
	if _, err := store.Complete(context.Background(), workspace, run.ID, work.ID, completionFor(work)); err != nil {
		t.Fatal(err)
	}
	verify, err := store.ClaimNext(workspace, run.ID, ClaimOptions{Worker: "reviewer"})
	if err != nil || verify.Phase != PhaseVerification || verify.AgentProfile != "reviewer" {
		t.Fatalf("missing verification: %+v, %v", verify, err)
	}
	if _, err := store.Complete(context.Background(), workspace, run.ID, verify.ID, completionFor(verify)); err != nil {
		t.Fatal(err)
	}
	input.VerifierAgentProfile = "different-reviewer"
	if _, err := store.EnqueueEvent(workspace, run.ID, input); err == nil {
		t.Fatal("same event ID changed the verifier's permissions")
	}
	input.VerifierAgentProfile = "reviewer"
	input.Prompt = "Replace the earlier event payload"
	if _, err := store.EnqueueEvent(workspace, run.ID, input); err == nil {
		t.Fatal("same event ID rewrote existing work")
	}
}
