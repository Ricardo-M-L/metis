package agent

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// CronJobStatus is presentation-only; it never migrates or executes legacy jobs.
func CronJobStatus(j *CronJob, now time.Time) string {
	if j.Kind == CronKindContinuation {
		if j.ContinuationStatus != "" && j.ContinuationStatus != "pending" {
			return j.ContinuationStatus
		}
		if !j.ExpiresAt.IsZero() && now.After(j.ExpiresAt) {
			return "expired"
		}
		if j.Paused {
			return "paused"
		}
		if !j.Enabled {
			return "disabled"
		}
		if !j.NextRun.IsZero() && !j.NextRun.After(now) {
			return "overdue (awaiting original session consumer)"
		}
		return "pending"
	}
	if j.Paused {
		return "paused"
	}
	if !j.Enabled {
		return "disabled"
	}
	if strings.HasPrefix(j.Name, "wakeup:") && j.OriginSessionID == "" && !j.NextRun.IsZero() && !j.NextRun.After(now) {
		return "overdue legacy wakeup (no origin; inspect and reschedule manually)"
	}
	return "enabled"
}

const CronKindContinuation = "continuation"

// ContinuationIdentity scopes a wakeup to the authorized conversation and runtime.
// It deliberately contains no credentials and does not grant tool permissions.
type ContinuationIdentity struct {
	SessionID string
	WorkDir   string
	Provider  string
	Model     string
}

func continuationWorkDir(path string) string {
	if path == "" {
		return ""
	}
	if absolute, err := filepath.Abs(path); err == nil {
		path = absolute
	}
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		path = resolved
	}
	return filepath.Clean(path)
}

func validateContinuation(j *CronJob) error {
	if j.OriginSessionID == "" || j.WorkDir == "" || j.Provider == "" || j.Model == "" {
		return fmt.Errorf("continuation requires original session, workspace, provider and model identity")
	}
	if j.Ephemeral || j.Schedule.Kind != "at" {
		return fmt.Errorf("continuation must be a durable one-shot at schedule")
	}
	j.WorkDir = continuationWorkDir(j.WorkDir)
	j.Repeat = 1
	return nil
}

func matchesContinuation(j *CronJob, i ContinuationIdentity) bool {
	return i.SessionID != "" && j.Kind == CronKindContinuation && j.OriginSessionID == i.SessionID && continuationWorkDir(j.WorkDir) == continuationWorkDir(i.WorkDir) && j.Provider == i.Provider && j.Model == i.Model
}

// ContinuationRun owns a durable, at-most-once admission. Start is separate from
// admission so a queued/dropped prompt never masquerades as a real execution.
type ContinuationRun struct {
	Job            *CronJob
	mu             sync.Mutex
	service        *CronService
	receipt        *CronRun
	started        bool
	finished       bool
	terminalStatus string
}

// AcquireDueContinuation never starts a model or tools. The caller must retain
// this handle until its actual turn completes, using its normal permission gate.
func (s *CronService) AcquireDueContinuation(identity ContinuationIdentity, now time.Time) (*ContinuationRun, error) {
	var result *ContinuationRun
	err := withCronStorageLock(s.root, func() error {
		s.mu.Lock()
		defer s.mu.Unlock()
		if err := s.reloadDurableLocked(); err != nil {
			return err
		}
		var due []*CronJob
		for _, j := range s.jobs {
			if !j.ExpiresAt.IsZero() && now.After(j.ExpiresAt) {
				continue
			}
			if matchesContinuation(j, identity) && j.Enabled && !j.Paused && !j.NextRun.IsZero() && !j.NextRun.After(now) && (j.ContinuationStatus == "" || j.ContinuationStatus == "pending") {
				due = append(due, j)
			}
		}
		sort.Slice(due, func(a, b int) bool {
			if due[a].NextRun.Equal(due[b].NextRun) {
				return due[a].ID < due[b].ID
			}
			return due[a].NextRun.Before(due[b].NextRun)
		})
		if len(due) == 0 {
			return nil
		}
		j := cloneCronJob(due[0])
		runID := generateID()
		receipt, err := BeginCronRun(s.root, j.ID, runID, "scheduled")
		if err != nil {
			return err
		}
		if err := receipt.SetSessionID(j.OriginSessionID); err != nil {
			_ = receipt.Finish("failed", "session binding failed", "", err)
			return err
		}
		j.ContinuationStatus = "claimed"
		j.ContinuationRunID = runID
		j.Enabled = false
		if err := s.saveJob(j); err != nil {
			_ = receipt.Finish("failed", "claim persistence failed", "", err)
			return err
		}
		s.jobs[j.ID] = j
		result = &ContinuationRun{Job: cloneCronJob(j), service: s, receipt: receipt}
		return nil
	})
	return result, err
}

func (r *ContinuationRun) Start() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.started || r.finished {
		return fmt.Errorf("continuation already started or finished")
	}
	err := withCronStorageLock(r.service.root, func() error {
		r.service.mu.Lock()
		defer r.service.mu.Unlock()
		var j CronJob
		if err := r.service.loadJob(r.Job.ID, &j); err != nil {
			return err
		}
		if !matchesContinuation(&j, ContinuationIdentity{SessionID: r.Job.OriginSessionID, WorkDir: r.Job.WorkDir, Provider: r.Job.Provider, Model: r.Job.Model}) || j.ContinuationRunID != r.Job.ContinuationRunID || j.ContinuationStatus != "claimed" || j.Paused || (!j.ExpiresAt.IsZero() && time.Now().After(j.ExpiresAt)) {
			return fmt.Errorf("continuation claim no longer eligible")
		}
		j.RunCount++
		j.LastRun = time.Now()
		j.ContinuationStatus = "running"
		if err := r.service.saveJob(&j); err != nil {
			return err
		}
		r.service.jobs[j.ID] = &j
		return nil
	})
	if err != nil {
		return err
	}
	r.started = true
	return nil
}

func (r *ContinuationRun) Finish(status, summary, output string, runErr error) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.finished {
		return nil
	}
	if status == "succeeded" && !r.started {
		return fmt.Errorf("unstarted continuation cannot succeed")
	}
	if r.terminalStatus == "" {
		if err := r.receipt.Finish(status, summary, output, runErr); err != nil {
			return err
		}
		// Finish releases execution ownership even on a write error. A retry
		// must use the authoritative receipt (possibly recovered interrupted),
		// never turn a lost terminal write into an invented success.
		record, err := ReadCronRun(r.service.root, r.Job.ID, r.Job.ContinuationRunID)
		if err != nil {
			return err
		}
		r.terminalStatus = record.Status
	}
	if err := r.service.Update(r.Job.ID, func(j *CronJob) { j.ContinuationStatus = r.terminalStatus; j.Enabled = false }); err != nil {
		return err
	}
	r.finished = true
	return nil
}

// PendingContinuations reloads durable state, returning only unclaimed work.
// Past NextRun means overdue/unconsumed, not executed. Crashed claims are never
// retried automatically: their interrupted run receipts remain inspectable.
func (s *CronService) PendingContinuations(identity ContinuationIdentity, now time.Time) ([]*CronJob, error) {
	if err := s.reloadDurableFromDisk(); err != nil {
		return nil, err
	}
	var pending []*CronJob
	for _, j := range s.List() {
		if !matchesContinuation(j, identity) {
			continue
		}
		if j.ContinuationRunID != "" && (j.ContinuationStatus == "claimed" || j.ContinuationStatus == "running") {
			receipt, err := ReadCronRun(s.root, j.ID, j.ContinuationRunID)
			if err != nil {
				return nil, err
			}
			if receipt.Status != "running" {
				if err := s.Update(j.ID, func(current *CronJob) { current.ContinuationStatus = receipt.Status; current.Enabled = false }); err != nil {
					return nil, err
				}
			}
		}
		if !j.ExpiresAt.IsZero() && now.After(j.ExpiresAt) {
			continue
		}
		if j.Enabled && !j.Paused && (j.ContinuationStatus == "" || j.ContinuationStatus == "pending") {
			pending = append(pending, j)
		}
	}
	sort.Slice(pending, func(a, b int) bool { return pending[a].NextRun.Before(pending[b].NextRun) })
	return pending, nil
}
