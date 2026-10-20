package projectcoord

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/handoff"
)

func handoffTestEnvelope(t *testing.T, b HandoffInput, task *handoff.Task, key string) handoff.Envelope {
	t.Helper()
	store, err := handoff.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	record, _, err := store.Send(context.Background(), handoff.SendRequest{Scope: "jobs", From: "sender", To: "job:" + b.PrincipalBinding.JobID, Body: "Please process this task", Task: task, IdempotencyKey: key})
	if err != nil {
		t.Fatal(err)
	}
	return record.Envelope
}
func handoffTestRun(t *testing.T, workspace, root string) (*Store, Run, HandoffInput) {
	t.Helper()
	store := NewStore(root)
	run, item := singleClaim(t, store, workspace, PhaseCustom, []string{"exit 0"}, nil)
	if _, err := store.Complete(context.Background(), workspace, run.ID, item.ID, completionFor(item)); err != nil {
		t.Fatal(err)
	}
	binding := projectTestBinding(t, workspace)
	input := HandoffInput{PrincipalBinding: &binding, VerifierAgentProfile: "host-reviewer"}
	input.Envelope = handoffTestEnvelope(t, input, &handoff.Task{ID: "task-1", Objective: "Produce an independent result", Deliverables: []string{"result.txt"}, Acceptance: []string{"touch must-not-execute"}}, "message-1")
	return store, run, input
}

func TestHandoffAdmissionIsAtomicConcurrentAndReopensCompletedRun(t *testing.T) {
	workspace, root := t.TempDir(), t.TempDir()
	store, run, input := handoffTestRun(t, workspace, root)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := NewStore(root).EnqueueHandoff(workspace, run.ID, input); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	after, err := store.Get(workspace, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Status != RunActive || !after.CompletedAt.IsZero() || len(after.Items) != 3 {
		t.Fatalf("lost/duplicate admission: %+v", after)
	}
	item, err := store.ClaimNext(workspace, run.ID, ClaimOptions{Worker: "job-worker"})
	if err != nil {
		t.Fatal(err)
	}
	if item.Handoff == nil || item.Handoff.MessageID != input.Envelope.ID || item.PrincipalBinding == nil || item.AgentProfile != "role" {
		t.Fatalf("binding lost: %+v", item)
	}
	// A sender acceptance string remains prompt data. Only the trusted host's
	// preconfigured exit 0 is executed during verification.
	if !strings.Contains(BuildWorkerPrompt(after, item), "untrusted natural-language") {
		t.Fatal("acceptance trust boundary absent")
	}
	if err := os.WriteFile(filepath.Join(workspace, "result.txt"), []byte("done"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Complete(context.Background(), workspace, run.ID, item.ID, completionFor(item)); err != nil {
		t.Fatal(err)
	}
	verify, err := store.ClaimNext(workspace, run.ID, ClaimOptions{Worker: "reviewer"})
	if err != nil {
		t.Fatal(err)
	}
	if verify.ID != item.ID+"-verify" || verify.AgentProfile != "host-reviewer" || verify.PrincipalBinding != nil || verify.Handoff != nil {
		t.Fatal("verifier did not remain independent")
	}
	if _, err := store.Complete(context.Background(), workspace, run.ID, verify.ID, completionFor(verify)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(workspace, "must-not-execute")); !os.IsNotExist(err) {
		t.Fatal("untrusted acceptance was executed")
	}
	// A duplicate cannot rerun a fully completed business task.
	if duplicate, err := store.EnqueueHandoff(workspace, run.ID, input); err != nil || duplicate.Status != ItemCompleted {
		t.Fatalf("completed replay: %+v %v", duplicate, err)
	}
	input.VerifierAgentProfile = "other"
	if _, err := store.EnqueueHandoff(workspace, run.ID, input); err == nil {
		t.Fatal("verifier contract drift accepted")
	}
}

func TestHandoffRejectsBadInputWithoutPartialAdmission(t *testing.T) {
	for _, kind := range []string{"wrong-sha", "outside-symlink", "escape", "overlap"} {
		t.Run(kind, func(t *testing.T) {
			workspace, root := t.TempDir(), t.TempDir()
			store, run, input := handoffTestRun(t, workspace, root)
			path := "input.txt"
			contents := []byte("trusted input")
			digest := sha256.Sum256(contents)
			hash := hex.EncodeToString(digest[:])
			if err := os.WriteFile(filepath.Join(workspace, path), contents, 0600); err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "wrong-sha":
				hash = strings.Repeat("a", 64)
			case "outside-symlink":
				outside := filepath.Join(t.TempDir(), "secret")
				if err := os.WriteFile(outside, contents, 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.Remove(filepath.Join(workspace, path)); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(outside, filepath.Join(workspace, path)); err != nil {
					t.Skip(err)
				}
			case "escape":
				path = "../input.txt"
			}
			task := &handoff.Task{ID: "task-1", Objective: "Use verified input", Artifacts: []handoff.Artifact{{Path: path, SHA256: hash}}, Deliverables: []string{"result.txt"}}
			if kind == "overlap" {
				task.Deliverables = []string{path}
			}
			input.Envelope = handoffTestEnvelope(t, input, task, "bad")
			if _, err := store.EnqueueHandoff(workspace, run.ID, input); !errors.Is(err, ErrHandoffInputInvalid) {
				t.Fatalf("invalid input error lacks sentinel: %v", err)
			}
			after, err := store.Get(workspace, run.ID)
			if err != nil {
				t.Fatal(err)
			}
			if len(after.Items) != 1 || after.Status != RunCompleted {
				t.Fatal("failed admission modified graph")
			}
		})
	}
}

func TestHandoffFrozenContractInputRecheckAndCloning(t *testing.T) {
	workspace, root := t.TempDir(), t.TempDir()
	store, run, input := handoffTestRun(t, workspace, root)
	hash := sha256.Sum256([]byte("input"))
	if err := os.WriteFile(filepath.Join(workspace, "input.txt"), []byte("input"), 0600); err != nil {
		t.Fatal(err)
	}
	task := &handoff.Task{ID: "task", Objective: "Use input", Artifacts: []handoff.Artifact{{Path: "input.txt", SHA256: hex.EncodeToString(hash[:])}}, Deliverables: []string{"result.txt"}}
	input.Envelope = handoffTestEnvelope(t, input, task, "valid-input")
	item, err := store.EnqueueHandoff(workspace, run.ID, input)
	if err != nil {
		t.Fatal(err)
	}
	item.Handoff.InputArtifacts[0].SHA256 = strings.Repeat("b", 64)
	input.PrincipalBinding.Definition.Profile.Tools[0] = "Write"
	loaded, err := store.Get(workspace, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	got := itemByID(t, loaded, item.ID)
	if got.Handoff.InputArtifacts[0].SHA256 != hex.EncodeToString(hash[:]) || got.PrincipalBinding.Definition.Profile.Tools[0] != "Read" {
		t.Fatal("caller mutated frozen contract")
	}
	if _, err := store.EnqueueHandoff(workspace, run.ID, input); err == nil {
		t.Fatal("same message accepted different frozen job")
	}
	claim, err := store.ClaimNext(workspace, run.ID, ClaimOptions{Worker: "worker"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "result.txt"), []byte("done"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "input.txt"), []byte("changed"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Complete(context.Background(), workspace, run.ID, claim.ID, completionFor(claim)); err == nil {
		t.Fatal("changed input passed completion")
	}
}

func TestHandoffContractCASIncludesPrincipalAndDependencyContracts(t *testing.T) {
	workspace, root := t.TempDir(), t.TempDir()
	store, run, input := handoffTestRun(t, workspace, root)
	item, err := store.EnqueueHandoff(workspace, run.ID, input)
	if err != nil {
		t.Fatal(err)
	}
	before, err := store.Get(workspace, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	old := itemByID(t, before, item.ID)
	for _, kind := range []string{"principal", "handoff", "checks", "dependency"} {
		t.Run(kind, func(t *testing.T) {
			after := cloneRun(before)
			current, _ := after.item(old.ID)
			switch kind {
			case "principal":
				current.PrincipalBinding.Definition.Profile.SystemPrompt = "changed"
			case "handoff":
				current.Handoff.ContentSHA256 = strings.Repeat("a", 64)
			case "checks":
				after.CheckCommands = []string{"exit 1"}
			case "dependency":
				// Verify CAS also covers the implementation's binding. A verifier has no
				// direct Handoff field, so its prerequisite contract must be included.
				verifier := itemByID(t, before, item.ID+"-verify")
				old = verifier
				current, _ = after.item(verifier.ID)
				implementation, _ := after.item(item.ID)
				implementation.Handoff.ContentSHA256 = strings.Repeat("a", 64)
			}
			if sameContract(before, old, after, *current) {
				t.Fatal("changed completion contract accepted")
			}
		})
	}
}

func TestOldJournalVersionsUpgradeWithoutLosingExecutionAccounting(t *testing.T) {
	for _, version := range []int{1, 2, 3, 4} {
		t.Run(string(rune('0'+version)), func(t *testing.T) {
			workspace := t.TempDir()
			store := NewStore(t.TempDir())
			run, item := singleClaim(t, store, workspace, PhaseCustom, nil, nil)
			if err := store.RecordSpend(workspace, run.ID, item.ID, item.Owner, item.Lease.Token, 0.25); err != nil {
				t.Fatal(err)
			}
			run, err := store.Get(workspace, run.ID)
			if err != nil {
				t.Fatal(err)
			}
			run.Version = version
			path, err := store.runPath(workspace, run.ID)
			if err != nil {
				t.Fatal(err)
			}
			data, err := json.Marshal(run)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
			upgraded, err := NewStore(store.Root()).Get(workspace, run.ID)
			if err != nil {
				t.Fatal(err)
			}
			got := itemByID(t, upgraded, item.ID)
			if upgraded.Version != stateVersion || got.Attempts != 1 || got.Lease.Token != item.Lease.Token || upgraded.SpentUSD != 0.25 || len(got.Usage) != 1 {
				t.Fatal("upgrade reset execution/accounting")
			}
		})
	}
}

func TestRecoverPersistsRevisionBeyondBoundedEventHistory(t *testing.T) {
	workspace := t.TempDir()
	store := NewStore(t.TempDir())
	run, item := singleClaim(t, store, workspace, PhaseCustom, nil, nil)
	if _, err := store.Fail(workspace, run.ID, item.ID, item.Owner, item.Lease.Token, Failure{Code: FailureWorkerBlocked, Summary: "requires review"}, "blocked"); err != nil {
		t.Fatal(err)
	}
	recovered, err := store.Recover(workspace, run.ID, item.ID, "reviewed", false)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := store.Get(workspace, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if recovered.LastRecoveryRevision == 0 || recovered.LastRecoveryRevision != loaded.Revision {
		t.Fatal("recovery revision not exact")
	}
	_, err = store.withRun(workspace, run.ID, func(run *Run) (bool, error) {
		for i := 0; i < maxEvents+1; i++ {
			run.addEvent(store.now(), "unrelated", "", "noise", "")
		}
		return true, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	loaded, err = NewStore(store.Root()).Get(workspace, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if itemByID(t, loaded, item.ID).LastRecoveryRevision != recovered.LastRecoveryRevision {
		t.Fatal("event rotation lost recovery proof")
	}
}

func TestHandoffRefusesCancelledProjectsAndWrongRoutes(t *testing.T) {
	workspace, root := t.TempDir(), t.TempDir()
	store, run, input := handoffTestRun(t, workspace, root)
	for _, kind := range []string{"scope", "recipient", "no-task", "workspace", "no-check"} {
		t.Run(kind, func(t *testing.T) {
			candidate := input
			candidate.PrincipalBinding = clonePrincipalBinding(input.PrincipalBinding)
			switch kind {
			case "scope", "recipient", "no-task":
				box, err := handoff.NewStore(t.TempDir())
				if err != nil {
					t.Fatal(err)
				}
				q := handoff.SendRequest{Scope: "jobs", From: "sender", To: "job:" + input.PrincipalBinding.JobID, Body: "work", Task: input.Envelope.Task}
				if kind == "scope" {
					q.Scope = "workspace-other"
				}
				if kind == "recipient" {
					q.To = input.PrincipalBinding.JobID
				}
				if kind == "no-task" {
					q.Task = nil
				}
				r, _, err := box.Send(context.Background(), q)
				if err != nil {
					t.Fatal(err)
				}
				candidate.Envelope = r.Envelope
			case "workspace":
				other := projectTestBinding(t, t.TempDir())
				candidate.PrincipalBinding = &other
			case "no-check":
				unchecked, err := store.Create(CreateInput{Workspace: workspace, Goal: "no host checks"})
				if err != nil {
					t.Fatal(err)
				}
				if _, err := store.EnqueueHandoff(workspace, unchecked.ID, candidate); err == nil {
					t.Fatal("no host checks accepted")
				}
				return
			}
			if _, err := store.EnqueueHandoff(workspace, run.ID, candidate); err == nil {
				t.Fatal("invalid handoff route accepted")
			}
		})
	}
	item, err := store.EnqueueHandoff(workspace, run.ID, input)
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.withRun(workspace, run.ID, func(run *Run) (bool, error) { run.Status = RunCancelled; return true, nil })
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.EnqueueHandoff(workspace, run.ID, input); err == nil {
		t.Fatal("cancelled duplicate accepted")
	}
	if item.Handoff == nil {
		t.Fatal("fixture did not admit task")
	}
}

func TestExternalEventFreezesJobBindingAndRejectsDifferentJob(t *testing.T) {
	workspace := t.TempDir()
	store := NewStore(t.TempDir())
	run, err := store.Create(CreateInput{Workspace: workspace, Goal: "event jobs"})
	if err != nil {
		t.Fatal(err)
	}
	binding := projectTestBinding(t, workspace)
	input := EventInput{Source: "api", ID: "event-1", Subject: "work", Prompt: "work", AgentProfile: binding.Definition.Profile.Name, PrincipalBinding: &binding, VerifierAgentProfile: "host"}
	item, err := store.EnqueueEvent(workspace, run.ID, input)
	if err != nil {
		t.Fatal(err)
	}
	if item.PrincipalBinding == nil || item.PrincipalBinding.JobID != binding.JobID {
		t.Fatal("event lost binding")
	}
	if _, err := NewStore(store.Root()).EnqueueEvent(workspace, run.ID, input); err != nil {
		t.Fatal(err)
	}
	other := projectTestBinding(t, workspace)
	input.PrincipalBinding = &other
	if _, err := store.EnqueueEvent(workspace, run.ID, input); err == nil {
		t.Fatal("same event rebound to a different job")
	}
}

func TestValidateHandoffInputsRejectsChangesWhileQueuedBeforeAnyProviderWork(t *testing.T) {
	for _, kind := range []string{"changed", "missing", "escaping-symlink", "cancelled", "outside-workspace", "valid"} {
		t.Run(kind, func(t *testing.T) {
			workspace, root := t.TempDir(), t.TempDir()
			store, run, input := handoffTestRun(t, workspace, root)
			contents := []byte("admitted input")
			inputPath := filepath.Join(workspace, "input.txt")
			if err := os.WriteFile(inputPath, contents, 0600); err != nil {
				t.Fatal(err)
			}
			sum := sha256.Sum256(contents)
			input.Envelope = handoffTestEnvelope(t, input, &handoff.Task{ID: "input-task", Objective: "Process verified input", Artifacts: []handoff.Artifact{{Path: "input.txt", SHA256: hex.EncodeToString(sum[:])}}, Deliverables: []string{"result.txt"}}, "queued")
			item, err := store.EnqueueHandoff(workspace, run.ID, input)
			if err != nil {
				t.Fatal(err)
			}
			ctx := context.Background()
			switch kind {
			case "changed":
				if err := os.WriteFile(inputPath, []byte("changed after admission"), 0600); err != nil {
					t.Fatal(err)
				}
			case "missing":
				if err := os.Remove(inputPath); err != nil {
					t.Fatal(err)
				}
			case "escaping-symlink":
				outside := filepath.Join(t.TempDir(), "input.txt")
				if err := os.WriteFile(outside, contents, 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.Remove(inputPath); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(outside, inputPath); err != nil {
					t.Skip(err)
				}
			case "cancelled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			case "outside-workspace":
				workspace = t.TempDir()
			}
			err = ValidateHandoffInputs(ctx, workspace, item)
			if kind == "valid" {
				if err != nil {
					t.Fatal(err)
				}
			} else if !errors.Is(err, ErrHandoffInputInvalid) {
				t.Fatalf("input failure not classified: %v", err)
			}
			if kind == "cancelled" && !errors.Is(err, context.Canceled) {
				t.Fatalf("context cancellation cause lost: %v", err)
			}
			loaded, err := store.Get(run.Workspace, run.ID)
			if err != nil {
				t.Fatal(err)
			}
			if len(loaded.UsageRequests) != 0 || itemByID(t, loaded, item.ID).Attempts != 0 {
				t.Fatal("validation dispatched or consumed a work attempt")
			}
		})
	}
}

func TestValidateHandoffInputsWithoutHandoffIsNoOp(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := ValidateHandoffInputs(ctx, "/does/not/exist", WorkItem{}); err != nil {
		t.Fatal(err)
	}
}
