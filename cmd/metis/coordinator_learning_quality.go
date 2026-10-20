package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Ricardo-M-L/metis/internal/config"
	"github.com/Ricardo-M-L/metis/internal/learning"
	"github.com/Ricardo-M-L/metis/internal/projectcoord"
)

const coordinatorQualityVerifier = "coordinator-host-checks"

type coordinatorLearningInvocation struct {
	Schema       int                     `json:"schema"`
	Invocation   learning.Invocation     `json:"invocation"`
	Binding      learning.OutcomeBinding `json:"binding"`
	WorkAttempt  int                     `json:"work_attempt,omitempty"`
	ClaimSHA256  string                  `json:"claim_sha256,omitempty"`
	ResultSHA256 string                  `json:"result_sha256,omitempty"`
}

func coordinatorLearningDirectory(workspace, runID, itemID string) string {
	return filepath.Join(config.Home(), "learning-business", learning.Digest([]byte(workspace)), learning.Digest([]byte(runID)), learning.Digest([]byte(itemID)))
}

// Binding is durable before the tool executes; the result digest is written
// afterwards. An interrupted tool cannot earn a fabricated quality observation.
func coordinatorLearningContext(ctx context.Context, store *projectcoord.Store, workspace, runID string, item projectcoord.WorkItem) context.Context {
	return learning.WithInvocationObserver(ctx, func(callCtx context.Context, in learning.Invocation) (learning.InvocationFinish, error) {
		noop := func(context.Context, []byte) error { return nil }
		if in.VersionSHA256 == "" {
			return noop, nil
		}
		artifact := learning.NewStore(in.ArtifactRoot)
		state, err := artifact.Read(in.Name)
		if os.IsNotExist(err) {
			// Legacy saved workflows have a content digest but no learning
			// activation. They retain their ordinary execution behavior.
			return noop, nil
		}
		if err != nil {
			return nil, err
		}
		if state.Quality == nil || state.Quality.Policy.Verifier != coordinatorQualityVerifier {
			return noop, nil
		}
		if state.ActiveVersion != in.VersionSHA256 || (in.ActivationEpoch != 0 && state.ActivationEpoch != in.ActivationEpoch) {
			return nil, errors.New("learning activation changed before invocation")
		}
		if err := coordinatorLearningAssertClaim(callCtx, store, workspace, runID, item); err != nil {
			return nil, err
		}
		claimSHA := learning.Digest([]byte(item.Lease.Token))
		identity, _ := json.Marshal([]any{workspace, runID, item.ID, item.Attempts, claimSHA, in.Kind, in.Name, in.VersionSHA256, state.ActivationEpoch})
		binding, err := artifact.BindOutcomeForActivation(callCtx, in.Name, learning.Digest(identity), in.VersionSHA256, state.ActivationEpoch)
		if err != nil {
			return nil, err
		}
		in.ActivationEpoch = state.ActivationEpoch
		record := coordinatorLearningInvocation{Schema: 2, Invocation: in, Binding: binding, WorkAttempt: item.Attempts, ClaimSHA256: claimSHA}
		path := filepath.Join(coordinatorLearningDirectory(workspace, runID, item.ID), binding.ID+".json")
		if old, err := readCoordinatorLearningInvocation(path); err == nil {
			if old.Schema != record.Schema || old.Binding != binding || old.Invocation != in || old.WorkAttempt != record.WorkAttempt || old.ClaimSHA256 != record.ClaimSHA256 {
				return nil, errors.New("learning invocation identity conflict")
			}
			record = old
		} else if !os.IsNotExist(err) {
			return nil, err
		}
		if err := writeCoordinatorLearningInvocation(path, record); err != nil {
			return nil, err
		}
		return func(finishCtx context.Context, result []byte) error {
			if err := coordinatorLearningAssertClaim(finishCtx, store, workspace, runID, item); err != nil {
				return err
			}
			if len(result) == 0 {
				return errors.New("learning invocation result evidence is empty")
			}
			record.ResultSHA256 = learning.Digest(result)
			return writeCoordinatorLearningInvocation(path, record)
		}, nil
	})
}

func coordinatorLearningAssertClaim(ctx context.Context, store *projectcoord.Store, workspace, runID string, item projectcoord.WorkItem) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	run, err := store.Get(workspace, runID)
	if err != nil {
		return err
	}
	for _, current := range run.Items {
		if current.ID == item.ID && current.Status == projectcoord.ItemRunning && current.Attempts == item.Attempts && current.Lease != nil && item.Lease != nil && current.Owner == item.Owner && current.Lease.Token == item.Lease.Token && current.Lease.ExpiresAt.After(time.Now()) {
			return nil
		}
	}
	return errors.New("learning observation requires the current live worker claim")
}

func readCoordinatorLearningInvocation(path string) (coordinatorLearningInvocation, error) {
	var r coordinatorLearningInvocation
	info, err := os.Lstat(path)
	if err != nil {
		return r, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || !coordinatorUsageOwned(info) {
		return r, errors.New("unsafe learning invocation evidence")
	}
	f, err := os.Open(path)
	if err != nil {
		return r, err
	}
	defer f.Close()
	current, err := f.Stat()
	if err != nil || !os.SameFile(info, current) {
		return r, errors.New("learning invocation evidence changed")
	}
	data, err := io.ReadAll(io.LimitReader(f, 32<<10))
	if err != nil {
		return r, err
	}
	if err = json.Unmarshal(data, &r); err != nil {
		return r, err
	}
	if (r.Schema != 1 && r.Schema != 2) || r.Binding.ID == "" || filepath.Base(path) != r.Binding.ID+".json" {
		return r, errors.New("invalid learning invocation evidence")
	}
	if r.Schema == 2 && (r.WorkAttempt < 1 || len(r.ClaimSHA256) != 64) {
		return r, errors.New("learning invocation is missing its claim provenance")
	}
	return r, nil
}

func writeCoordinatorLearningInvocation(path string, r coordinatorLearningInvocation) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	if info, err := os.Lstat(dir); err != nil || !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return errors.New("unsafe learning invocation directory")
	}
	data, err := json.Marshal(r)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".invocation-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(data)
	}
	if err == nil {
		err = f.Sync()
	}
	err = errors.Join(err, f.Close())
	if err != nil {
		return err
	}
	if err = os.Rename(f.Name(), path); err != nil {
		return err
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

// Reconciliation derives quality only from independent persisted host checks.
// The graph remains the authority; retries after a crash replay the same record.
func reconcileCoordinatorLearning(ctx context.Context, store *projectcoord.Store, workspace, runID string) error {
	run, err := store.Get(workspace, runID)
	if err != nil {
		return err
	}
	for _, verifier := range run.Items {
		if verifier.Phase != projectcoord.PhaseVerification || verifier.Acceptance == nil || len(verifier.Acceptance.Checks) == 0 {
			continue
		}
		acceptance := verifier.Acceptance
		if acceptance.Status != "passed" && acceptance.Status != "failed" {
			continue
		}
		passed := acceptance.Status == "passed"
		// A killed, timed out, or unstartable checker has no business verdict.
		// A rejection requires an actual positive exit from the host checker;
		// incidental failures after successful checks remain unclassified.
		failedVerdict, infrastructureFailure := false, false
		for _, check := range acceptance.Checks {
			infrastructureFailure = infrastructureFailure || check.ExitCode < 0
			failedVerdict = failedVerdict || check.ExitCode > 0
		}
		if infrastructureFailure || (!passed && !failedVerdict) || (passed && failedVerdict) {
			continue
		}
		evidence, _ := json.Marshal([]any{run.ID, verifier.ID, verifier.Attempts, acceptance, verifier.Artifacts})
		for _, dependency := range verifier.DependsOn {
			var completed *projectcoord.WorkItem
			for i := range run.Items {
				if run.Items[i].ID == dependency && run.Items[i].Status == projectcoord.ItemCompleted {
					completed = &run.Items[i]
					break
				}
			}
			if completed == nil || len(completed.Usage) == 0 {
				continue
			}
			// Every claim appends an AttemptUsage token, even for an entirely
			// local tool task with no paid provider requests.
			claimSHA := learning.Digest([]byte(completed.Usage[len(completed.Usage)-1].Token))
			entries, err := os.ReadDir(coordinatorLearningDirectory(workspace, runID, dependency))
			if os.IsNotExist(err) {
				continue
			}
			if err != nil {
				return err
			}
			for _, entry := range entries {
				if !strings.HasSuffix(entry.Name(), ".json") {
					continue
				}
				r, err := readCoordinatorLearningInvocation(filepath.Join(coordinatorLearningDirectory(workspace, runID, dependency), entry.Name()))
				if err != nil {
					return err
				}
				if r.Schema != 2 || r.ResultSHA256 == "" || r.WorkAttempt != completed.Attempts || r.ClaimSHA256 != claimSHA {
					continue
				}
				s := learning.NewStore(r.Invocation.ArtifactRoot)
				known, err := s.LookupOutcome(r.Binding.Name, r.Binding.ID)
				if err != nil && !os.IsNotExist(err) {
					return err
				}
				if err == nil && known.Outcome != nil {
					continue
				}
				proof, _ := json.Marshal(r)
				reason := "host_checks_passed"
				if !passed {
					reason = "host_checks_failed"
				}
				_, err = s.RecordOutcome(ctx, r.Binding.Name, r.Binding, learning.HostOutcome{Passed: passed, Verifier: coordinatorQualityVerifier, InvocationSHA256: learning.Digest(proof), EvidenceSHA256: learning.Digest(evidence), ReasonCode: reason})
				if err != nil {
					return err
				}
			}
		}
	}
	return nil
}
