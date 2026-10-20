package handoff

import (
	"context"
	"errors"
	"time"
)

// AdmissionCheckInterval bounds disk writes while preserving a durable last
// check heartbeat. A changed reason is recorded immediately.
const AdmissionCheckInterval = time.Minute

const (
	WaitingJobPaused         = "job_paused"
	WaitingJobBusy           = "job_busy"
	WaitingBudgetExhausted   = "budget_exhausted"
	WaitingUsageUnresolved   = "usage_unresolved"
	WaitingUsageCheckFailed  = "usage_check_failed"
	WaitingDefinitionChanged = "job_definition_changed"
	WaitingRouteUnavailable  = "route_unavailable"
	WaitingJobUnavailable    = "job_unavailable"
	WaitingHostUnavailable   = "host_unavailable"
	WaitingRunCancelled      = "run_cancelled"
)

// AdmissionView is safe for model/UI display. Its text is selected from a
// closed host vocabulary, never copied from provider/configuration errors.
type AdmissionView struct {
	State         string    `json:"state"`
	WaitingReason string    `json:"waiting_reason"`
	LastCheckedAt time.Time `json:"last_checked_at"`
	RecoveryHint  string    `json:"recovery_hint"`
}

// AdmissionStatus concerns only work that has not entered the graph. It is
// independent of transport claims and of the eventual business outcome.
type AdmissionStatus struct {
	AdmissionView
	Assignment BusinessAssignment `json:"assignment"`
}

func (a *AdmissionStatus) SafeStatus() AdmissionView {
	if a == nil {
		return AdmissionView{}
	}
	return a.AdmissionView
}

func admissionHint(reason string) string {
	switch reason {
	case WaitingJobPaused:
		return "Resume the saved job; the resident service will check it again automatically."
	case WaitingJobBusy:
		return "Wait for the active job execution to finish or its lease to expire."
	case WaitingBudgetExhausted:
		return "Review the lifetime usage and create an explicitly budgeted successor job; retries do not reset spent usage."
	case WaitingUsageUnresolved:
		return "Reconcile the unresolved provider usage receipts before admitting more work."
	case WaitingUsageCheckFailed:
		return "Restore access to the saved usage ledgers and let the service check them again."
	case WaitingDefinitionChanged:
		return "Review the job definition and explicitly restart the service with the intended saved job."
	case WaitingRouteUnavailable:
		return "Restore the saved provider, credential reference and host policy, or create and allow a successor job."
	case WaitingJobUnavailable:
		return "Restore access to the saved job and restart the service if its allowlist changed."
	case WaitingHostUnavailable:
		return "Restore access to the coordinator run and let the service check it again."
	case WaitingRunCancelled:
		return "Start a service for an active coordinator run; the unassigned message remains available."
	default:
		return ""
	}
}

func validateAdmission(r Record) error {
	a := r.Admission
	if a == nil {
		return nil
	}
	if a.State != "waiting" || a.LastCheckedAt.IsZero() || admissionHint(a.WaitingReason) == "" || a.RecoveryHint != admissionHint(a.WaitingReason) {
		return errors.New("invalid handoff admission diagnostics")
	}
	if err := validateBusinessUpdate(BusinessUpdate{Assignment: a.Assignment, State: BusinessAccepted}, r.Task); err != nil {
		return err
	}
	if r.Task == nil || r.Scope != "jobs" || r.To != "job:"+a.Assignment.JobID || r.State == "acknowledged" {
		return ErrBusinessConflict
	}
	if r.Business != nil && (r.Business.Assignment != a.Assignment || r.Business.State != BusinessAccepted || r.Business.SourceRevision != 0) {
		return ErrBusinessConflict
	}
	return nil
}

// SetAdmissionWaiting records a host admission check without reserving work,
// claiming a transport delivery, or changing attempts/budget. After graph
// admission it refuses to overwrite the business receipt with queue status.
func (s *Store) SetAdmissionWaiting(ctx context.Context, messageID string, assignment BusinessAssignment, reason string) (Record, error) {
	var result Record
	err := s.locked(ctx, func() error {
		r, err := s.read(messageID)
		if err != nil {
			return err
		}
		now := s.now().UTC()
		next := &AdmissionStatus{AdmissionView: AdmissionView{State: "waiting", WaitingReason: reason, LastCheckedAt: now, RecoveryHint: admissionHint(reason)}, Assignment: assignment}
		old := r.Admission
		r.Admission = next
		if err = validateAdmission(r); err != nil {
			return err
		}
		if old != nil && old.Assignment == assignment && old.WaitingReason == reason && now.Before(old.LastCheckedAt.Add(AdmissionCheckInterval)) {
			r.Admission = old
			result = r
			return nil
		}
		if err = s.write(r); err != nil {
			return err
		}
		r.Version = SchemaVersion
		result = r
		return nil
	})
	return result, err
}

// ClearAdmissionWaiting is used once the graph has durably accepted this exact
// assignment. It cannot clear another run's reservation or wait observation.
func (s *Store) ClearAdmissionWaiting(ctx context.Context, messageID string, assignment BusinessAssignment) error {
	return s.locked(ctx, func() error {
		r, err := s.read(messageID)
		if err != nil {
			return err
		}
		if r.Business == nil || r.Business.Assignment != assignment {
			return ErrBusinessConflict
		}
		if r.Admission == nil {
			return nil
		}
		if r.Admission.Assignment != assignment {
			return ErrBusinessConflict
		}
		r.Admission = nil
		return s.write(r)
	})
}
