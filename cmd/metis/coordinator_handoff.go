package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Ricardo-M-L/metis/internal/handoff"
	"github.com/Ricardo-M-L/metis/internal/principal"
	"github.com/Ricardo-M-L/metis/internal/projectcoord"
)

// coordinatorServiceJobIDs is an explicit host allowlist. An empty list keeps
// the pre-existing event-only service behaviour.
type coordinatorServiceJobIDs []string

func (v *coordinatorServiceJobIDs) String() string { return strings.Join(*v, ",") }
func (v *coordinatorServiceJobIDs) Set(id string) error {
	id = strings.TrimSpace(id)
	if id == "" || len(id) > 128 || strings.ContainsAny(id, "\x00\r\n") {
		return errors.New("coordinator service --job requires a saved job ID")
	}
	for _, old := range *v {
		if old == id {
			return nil
		}
	}
	*v = append(*v, id)
	return nil
}

func loadCoordinatorServiceJobs(opts coordinatorServiceOptions, home string) (map[string]principal.Binding, error) {
	frozen := make(map[string]principal.Binding, len(opts.jobIDs))
	jobs := principal.NewStore(filepath.Join(home, "jobs"))
	for _, id := range opts.jobIDs {
		job, err := jobs.Get(id)
		if err != nil {
			return nil, fmt.Errorf("coordinator service job validation: %w", err)
		}
		binding := job.Binding()
		if err = principal.ValidateBinding(&binding, opts.workspace); err != nil {
			return nil, err
		}
		// Paused jobs are valid service configuration. Readiness is checked before
		// admission and again by the existing durable worker execution guards.
		if err = validatePersistentJobRoute(binding); err != nil {
			return nil, err
		}
		frozen[id] = binding
	}
	return frozen, nil
}

func (s *coordinatorService) initializeHandoffs() error {
	bindings, err := loadCoordinatorServiceJobs(s.opts, s.home)
	if err != nil {
		return err
	}
	s.jobBindings = bindings
	s.jobCursors = make(map[string]string, len(bindings))
	s.handler.jobBindings = bindings
	if len(bindings) == 0 {
		s.handoffStore = nil
		return nil
	}
	s.handoffStore, err = handoff.NewStore(filepath.Join(s.home, "handoff"))
	return err
}

func coordinatorHandoffAssignment(opts coordinatorServiceOptions, binding principal.Binding, messageID string) handoff.BusinessAssignment {
	id := "handoff-" + messageID
	return handoff.BusinessAssignment{JobID: binding.JobID, Workspace: opts.workspace, RunID: opts.runID, WorkItemID: id, VerificationItemID: id + "-verify", ConfigSHA256: binding.ConfigSHA256}
}

// Another host can freeze a different assignment or advance this assignment
// after our mailbox/graph read. Keep its durable receipt and refresh next poll.
func coordinatorHandoffProjectionSuperseded(err error) bool {
	return errors.Is(err, handoff.ErrBusinessStale) || errors.Is(err, handoff.ErrBusinessConflict)
}

func (s *coordinatorService) reserveHandoffBusiness(ctx context.Context, messageID string, assignment handoff.BusinessAssignment) (handoff.Record, bool, error) {
	rec, err := s.handoffStore.ReconcileBusiness(ctx, messageID, handoff.BusinessUpdate{Assignment: assignment, State: handoff.BusinessAccepted})
	if coordinatorHandoffProjectionSuperseded(err) {
		return handoff.Record{}, false, nil
	}
	return rec, err == nil, err
}

func (s *coordinatorService) checkJobAdmission(binding principal.Binding) error {
	if err := validatePersistentJobRoute(binding); err != nil {
		return coordinatorAdmissionError{reason: handoff.WaitingRouteUnavailable, err: err}
	}
	jobs := principal.NewStore(filepath.Join(s.home, "jobs"))
	job, err := jobs.Get(binding.JobID)
	if err != nil {
		return coordinatorAdmissionError{reason: handoff.WaitingJobUnavailable, err: err}
	}
	if !reflect.DeepEqual(job.Binding(), binding) {
		return coordinatorAdmissionError{reason: handoff.WaitingDefinitionChanged, err: errors.New("persistent job definition changed from service allowlist")}
	}
	if job.Status == principal.Retired {
		return coordinatorAdmissionError{reason: "job_retired", err: principal.ErrInactive}
	}
	if err := syncPersistentJobUsage(s.store, binding); err != nil {
		return coordinatorAdmissionError{reason: handoff.WaitingUsageCheckFailed, err: err}
	}
	if err := checkPersistentJobReady(&binding); err != nil {
		reason := handoff.WaitingJobUnavailable
		switch {
		case errors.Is(err, principal.ErrInactive):
			reason = handoff.WaitingJobPaused
		case errors.Is(err, principal.ErrUsageUnknown):
			reason = handoff.WaitingUsageUnresolved
		case errors.Is(err, principal.ErrBudget):
			reason = handoff.WaitingBudgetExhausted
		}
		return coordinatorAdmissionError{reason: reason, err: err}
	}
	job, err = jobs.Get(binding.JobID)
	if err != nil {
		return coordinatorAdmissionError{reason: handoff.WaitingJobUnavailable, err: err}
	}
	if job.Lease != nil && job.Lease.ExpiresAt.After(time.Now()) {
		return coordinatorAdmissionError{reason: handoff.WaitingJobBusy, err: principal.ErrBusy}
	}
	return nil
}

// Admission errors carry a closed public code separately from private runtime
// diagnostics. Configuration errors may contain paths or credential references.
type coordinatorAdmissionError struct {
	reason string
	err    error
}

func (e coordinatorAdmissionError) Error() string { return e.err.Error() }
func (e coordinatorAdmissionError) Unwrap() error { return e.err }

func (s *coordinatorService) handoffAdmissionReady(ctx context.Context, rec handoff.Record, assignment handoff.BusinessAssignment, run projectcoord.Run) (bool, error) {
	if run.Status == projectcoord.RunCancelled && rec.Business == nil {
		// Cancellation owns only this run's reserved work. Fresh messages must
		// remain eligible for another explicitly allowlisted coordinator run.
		_, err := s.handoffStore.SetAdmissionWaiting(ctx, rec.ID, assignment, handoff.WaitingRunCancelled)
		return false, err
	}
	var admissionErr error
	if run.Status != projectcoord.RunCancelled {
		admissionErr = s.checkJobAdmission(s.jobBindings[assignment.JobID])
		if admissionErr == nil {
			return true, nil
		}
	}
	reason := handoff.WaitingHostUnavailable
	var detail coordinatorAdmissionError
	if errors.As(admissionErr, &detail) {
		reason = detail.reason
	}
	if run.Status == projectcoord.RunCancelled || reason == "job_retired" {
		// These are terminal host decisions, not an automatic paid retry or a
		// temporary queue hold. Reserve first to freeze the receipt's authority.
		if rec.Business == nil {
			var err error
			var reserved bool
			rec, reserved, err = s.reserveHandoffBusiness(ctx, rec.ID, assignment)
			if err != nil {
				return false, err
			}
			if !reserved {
				return false, nil
			}
		}
		state, code, summary := handoff.BusinessCancelled, "task_cancelled", "The assigned project was cancelled before admission."
		if reason == "job_retired" && run.Status != projectcoord.RunCancelled {
			state, code, summary = handoff.BusinessRejected, "job_retired", "The assigned job was retired before admission."
		}
		_, err := s.handoffStore.ReconcileBusiness(ctx, rec.ID, handoff.BusinessUpdate{Assignment: assignment, SourceRevision: run.Revision, State: state, Failure: coordinatorHandoffFailure(code, summary, []any{assignment, run.Status})})
		if coordinatorHandoffProjectionSuperseded(err) {
			return false, nil
		}
		return false, err
	}
	_, err := s.handoffStore.SetAdmissionWaiting(ctx, rec.ID, assignment, reason)
	return false, err
}

// reconcileHandoffs runs on startup, wake, poll and child completion. The
// handoff reservation and graph admission are separate idempotent transactions;
// no two stores are ever locked together. A crash between them is repaired by
// the same frozen assignment, including after the message was acknowledged.
func (s *coordinatorService) reconcileHandoffs(ctx context.Context) error {
	if s.handoffStore == nil {
		return nil
	}
	if s.jobCursors == nil {
		s.jobCursors = make(map[string]string, len(s.jobBindings))
	}
	var first error
	for id, binding := range s.jobBindings {
		records, nextCursor, err := s.handoffStore.ListBusinessPage(ctx, "jobs", "job:"+id, s.jobCursors[id], 1024)
		if err != nil {
			if first == nil {
				first = err
			}
			continue
		}
		s.jobCursors[id] = nextCursor
		for _, rec := range records {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			assignment := coordinatorHandoffAssignment(s.opts, binding, rec.ID)
			if rec.Business != nil && rec.Business.Assignment != assignment {
				continue
			}
			if rec.Business == nil {
				current, readErr := s.store.Get(s.opts.workspace, s.opts.runID)
				if readErr != nil {
					_, _ = s.handoffStore.SetAdmissionWaiting(ctx, rec.ID, assignment, handoff.WaitingHostUnavailable)
					if first == nil {
						first = readErr
					}
					continue
				}
				ready, admissionErr := s.handoffAdmissionReady(ctx, rec, assignment, current)
				if admissionErr != nil && !coordinatorHandoffProjectionSuperseded(admissionErr) && first == nil {
					first = admissionErr
				}
				if !ready {
					continue
				}
				var reserved bool
				rec, reserved, err = s.reserveHandoffBusiness(ctx, rec.ID, assignment)
				if err != nil {
					if first == nil {
						first = err
					}
					continue
				}
				if !reserved {
					continue
				}
			}
			run, getErr := s.store.Get(s.opts.workspace, s.opts.runID)
			if getErr != nil {
				if first == nil {
					first = getErr
				}
				continue
			}
			impl, verify := coordinatorHandoffItems(run, assignment)
			if impl == nil {
				if run.Status == projectcoord.RunCancelled {
					_, err = s.handoffStore.ReconcileBusiness(ctx, rec.ID, handoff.BusinessUpdate{Assignment: assignment, SourceRevision: run.Revision, State: handoff.BusinessCancelled, Failure: coordinatorHandoffFailure("task_cancelled", "The assigned project was cancelled before admission.", run.Status)})
					if err != nil && !coordinatorHandoffProjectionSuperseded(err) && first == nil {
						first = err
					}
					continue
				}
				if rec.Business.State != handoff.BusinessAccepted {
					continue
				}
				ready, admissionErr := s.handoffAdmissionReady(ctx, rec, assignment, run)
				if admissionErr != nil && !coordinatorHandoffProjectionSuperseded(admissionErr) && first == nil {
					first = admissionErr
				}
				if !ready {
					continue
				}
				_, err = s.store.EnqueueHandoff(s.opts.workspace, s.opts.runID, projectcoord.HandoffInput{Envelope: rec.Envelope, PrincipalBinding: &binding, VerifierAgentProfile: s.opts.verifierProfile})
				if err != nil {
					// Bad sender input is a durable rejection, never a paid retry. Storage
					// failures leave the reservation intact for restart reconciliation.
					if errors.Is(err, projectcoord.ErrHandoffInputInvalid) {
						u := handoff.BusinessUpdate{Assignment: assignment, SourceRevision: run.Revision, State: handoff.BusinessRejected, Failure: coordinatorHandoffFailure("handoff_input_invalid", "The declared input artifact failed host integrity verification.", rec.ContentSHA256)}
						_, err = s.handoffStore.ReconcileBusiness(ctx, rec.ID, u)
					}
					if err != nil && !coordinatorHandoffProjectionSuperseded(err) && first == nil {
						first = err
					}
					continue
				}
				run, err = s.store.Get(s.opts.workspace, s.opts.runID)
				if err != nil {
					if first == nil {
						first = err
					}
					continue
				}
				impl, verify = coordinatorHandoffItems(run, assignment)
			}
			if err = s.handoffStore.ClearAdmissionWaiting(ctx, rec.ID, assignment); err != nil {
				if first == nil {
					first = err
				}
				continue
			}
			if verify != nil && verify.AgentProfile != s.opts.verifierProfile {
				if first == nil {
					first = errors.New("handoff verifier differs from the service host profile")
				}
				continue
			}
			update, err := coordinatorHandoffProjection(rec, run, impl, verify, binding)
			if err != nil {
				if first == nil {
					first = err
				}
				continue
			}
			if update == nil {
				continue
			}
			_, err = s.handoffStore.ReconcileBusiness(ctx, rec.ID, *update)
			if coordinatorHandoffProjectionSuperseded(err) {
				continue
			}
			if err != nil && first == nil {
				first = err
			}
		}
	}
	return first
}

func coordinatorHandoffItems(run projectcoord.Run, a handoff.BusinessAssignment) (*projectcoord.WorkItem, *projectcoord.WorkItem) {
	var impl, verify *projectcoord.WorkItem
	for i := range run.Items {
		if run.Items[i].ID == a.WorkItemID {
			impl = &run.Items[i]
		}
		if run.Items[i].ID == a.VerificationItemID {
			verify = &run.Items[i]
		}
	}
	return impl, verify
}
func coordinatorHandoffFailure(code, summary string, evidence any) *handoff.BusinessFailure {
	hash, _ := handoff.BusinessEvidenceSHA256(evidence)
	return &handoff.BusinessFailure{Code: code, Summary: summary, EvidenceSHA256: hash}
}
func coordinatorHandoffTextSHA(v string) string {
	sum := sha256.Sum256([]byte(v))
	return hex.EncodeToString(sum[:])
}

func coordinatorHandoffProjection(rec handoff.Record, run projectcoord.Run, impl, verify *projectcoord.WorkItem, binding principal.Binding) (*handoff.BusinessUpdate, error) {
	if rec.Business == nil || impl == nil || verify == nil {
		return nil, errors.New("handoff assignment has no complete graph pair")
	}
	a := rec.Business.Assignment
	if rec.Scope != "jobs" || rec.To != "job:"+a.JobID || a.JobID != binding.JobID || a.ConfigSHA256 != binding.ConfigSHA256 || run.ID != a.RunID || run.Workspace != a.Workspace || impl.ID != a.WorkItemID || verify.ID != a.VerificationItemID || impl.Handoff == nil || impl.Handoff.MessageID != rec.ID || impl.Handoff.ContentSHA256 != rec.ContentSHA256 || impl.PrincipalBinding == nil || !reflect.DeepEqual(*impl.PrincipalBinding, binding) || verify.PrincipalBinding != nil || verify.Phase != projectcoord.PhaseVerification || len(verify.DependsOn) != 1 || verify.DependsOn[0] != impl.ID {
		return nil, errors.New("handoff graph pair differs from its frozen assignment")
	}
	old := rec.Business.BusinessUpdate
	if old.State == handoff.BusinessCompleted || old.State == handoff.BusinessCancelled {
		return nil, nil
	}
	u := handoff.BusinessUpdate{Assignment: a, SourceRevision: run.Revision, State: handoff.BusinessAccepted}
	if run.Status == projectcoord.RunCancelled || impl.Status == projectcoord.ItemCancelled || verify.Status == projectcoord.ItemCancelled {
		u.State = handoff.BusinessCancelled
		u.Failure = coordinatorHandoffFailure("task_cancelled", "The assigned work was cancelled.", []any{impl.Status, verify.Status, run.Status})
	} else {
		for _, item := range []*projectcoord.WorkItem{impl, verify} {
			if item.Status == projectcoord.ItemFailed || item.Status == projectcoord.ItemBlocked {
				u.State = handoff.BusinessFailed
				code, summary := "task_failed", "The assigned work did not complete."
				if item.Failure != nil {
					code, summary = item.Failure.Code, item.Failure.Summary
				}
				if item.Status == projectcoord.ItemBlocked || code == projectcoord.FailureProviderUsageUnresolved || code == projectcoord.FailureOperationUncertain {
					u.State = handoff.BusinessBlocked
				}
				if item.Acceptance != nil && item.Acceptance.Status == "failed" {
					u.State = handoff.BusinessRejected
					code = "acceptance_failed"
					summary = "The assigned work failed independent host acceptance."
				}
				if handoff.ValidateAddress(code) != nil {
					code = "task_failed"
				}
				summary = strings.ReplaceAll(strings.ToValidUTF8(summary, "�"), "\x00", "�")
				if strings.TrimSpace(summary) == "" {
					summary = "The assigned work did not complete."
				}
				if len(summary) > 4096 {
					summary = summary[:4096]
					for !utf8.ValidString(summary) {
						summary = summary[:len(summary)-1]
					}
				}
				u.Failure = coordinatorHandoffFailure(code, summary, []any{item.ID, item.Failure, item.Acceptance})
				break
			}
		}
		if u.Failure == nil {
			if impl.Status == projectcoord.ItemCompleted && verify.Status == projectcoord.ItemCompleted {
				if rec.State != "acknowledged" {
					return nil, nil
				}
				if verify.Acceptance == nil || verify.Acceptance.Status != "passed" || len(verify.Acceptance.Checks) == 0 {
					return nil, errors.New("handoff completion has no passed independent host checks")
				}
				u.State = handoff.BusinessCompleted
				u.Output = impl.Output
				if strings.TrimSpace(u.Output) == "" {
					u.Output = "Completed the assigned work; all independent host acceptance checks passed."
				}
				if len(u.Output) > 32<<10 || strings.ContainsRune(u.Output, 0) || !utf8.ValidString(u.Output) {
					u.Output = "Completed the assigned work. Full output SHA-256: " + coordinatorHandoffTextSHA(impl.Output)
				}
				u.OutputSHA256 = coordinatorHandoffTextSHA(u.Output)
				for _, artifact := range impl.Artifacts {
					u.Artifacts = append(u.Artifacts, handoff.Artifact{Path: artifact.Path, SHA256: artifact.SHA256})
				}
				hash, _ := handoff.BusinessEvidenceSHA256(verify.Acceptance)
				u.Acceptance = &handoff.BusinessAcceptance{Passed: true, Source: "host", EvidenceSHA256: hash, Summary: "Independent verification and host checks passed."}
				for _, check := range verify.Acceptance.Checks {
					u.Acceptance.Checks = append(u.Acceptance.Checks, handoff.BusinessCheck{Command: check.Command, ExitCode: check.ExitCode, OutputSHA256: check.OutputSHA256})
				}
			} else if impl.Attempts > 0 || verify.Attempts > 0 || impl.Status == projectcoord.ItemRunning || verify.Status == projectcoord.ItemRunning {
				u.State = handoff.BusinessRunning
			}
		}
	}
	// A larger unrelated graph revision cannot silently retry rejected work.
	// The durable recovery marker survives the bounded event ring.
	recoveryRevision := max(impl.LastRecoveryRevision, verify.LastRecoveryRevision)
	if u.State != handoff.BusinessCancelled && (old.State == handoff.BusinessBlocked || old.State == handoff.BusinessFailed || old.State == handoff.BusinessRejected) {
		if recoveryRevision <= old.SourceRevision {
			return nil, nil
		}
		u.Recovery = true
	}
	comparable := u
	comparable.SourceRevision = old.SourceRevision
	if old.SourceRevision != 0 && reflect.DeepEqual(comparable, old) {
		return nil, nil
	}
	return &u, nil
}
