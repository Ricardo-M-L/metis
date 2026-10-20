package handoff

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	BusinessAccepted  = "accepted"
	BusinessRunning   = "running"
	BusinessBlocked   = "blocked"
	BusinessCompleted = "completed"
	BusinessRejected  = "rejected"
	BusinessFailed    = "failed"
	BusinessCancelled = "cancelled"
)

var (
	ErrBusinessStale    = errors.New("handoff business update has an older graph revision")
	ErrBusinessConflict = errors.New("handoff business update conflicts with the durable assignment or result")
)

// BusinessAssignment freezes the graph and principal that own a message. It
// must be derived by the host from a saved job and its durable work graph.
type BusinessAssignment struct {
	JobID              string `json:"job_id"`
	Workspace          string `json:"workspace"`
	RunID              string `json:"run_id"`
	WorkItemID         string `json:"work_item_id"`
	VerificationItemID string `json:"verification_item_id"`
	ConfigSHA256       string `json:"config_sha256"`
}

// BusinessAcceptance is an independent host observation, never a model's
// assertion or a shell command from Task.Acceptance. EvidenceSHA256 identifies
// the saved graph's concrete verification evidence.
type BusinessCheck struct {
	Command      string `json:"command"`
	ExitCode     int    `json:"exit_code"`
	OutputSHA256 string `json:"output_sha256"`
}

type BusinessAcceptance struct {
	Passed         bool            `json:"passed"`
	Source         string          `json:"source"`
	EvidenceSHA256 string          `json:"evidence_sha256"`
	Summary        string          `json:"summary,omitempty"`
	Checks         []BusinessCheck `json:"checks,omitempty"`
}

type BusinessFailure struct {
	Code           string `json:"code"`
	Summary        string `json:"summary"`
	EvidenceSHA256 string `json:"evidence_sha256"`
}

// BusinessUpdate is a host-only projection of a trusted graph snapshot. No
// user/model command should expose this as a directly writable completion API.
// Recovery is true only when the graph records an explicit recovery after a
// previously failed/blocked revision; a larger revision alone is insufficient.
type BusinessUpdate struct {
	Assignment     BusinessAssignment  `json:"assignment"`
	SourceRevision uint64              `json:"source_revision"`
	State          string              `json:"state"`
	Output         string              `json:"output,omitempty"`
	OutputSHA256   string              `json:"output_sha256,omitempty"`
	Artifacts      []Artifact          `json:"artifacts,omitempty"`
	Acceptance     *BusinessAcceptance `json:"acceptance,omitempty"`
	Failure        *BusinessFailure    `json:"failure,omitempty"`
	Recovery       bool                `json:"recovery,omitempty"`
}

type BusinessReceipt struct {
	BusinessUpdate
	AcceptedAt  time.Time `json:"accepted_at"`
	StartedAt   time.Time `json:"started_at,omitempty"`
	CompletedAt time.Time `json:"completed_at,omitempty"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// ReconcileBusiness atomically projects a graph revision without altering the
// independent transport acknowledgement. Assignment changes, stale snapshots,
// implicit retries and rewritten terminal results fail closed. The caller is
// the host reconciler, which must verify graph provenance before calling it.
func (s *Store) ReconcileBusiness(ctx context.Context, messageID string, update BusinessUpdate) (Record, error) {
	var result Record
	err := s.locked(ctx, func() error {
		rec, err := s.read(messageID)
		if err != nil {
			return err
		}
		if err = validateBusinessUpdate(update, rec.Task); err != nil {
			return err
		}
		if update.State == BusinessCompleted && rec.State != "acknowledged" {
			return errors.New("business completion requires a durable transport checkpoint")
		}
		if rec.Scope != "jobs" || rec.To != "job:"+update.Assignment.JobID {
			return ErrBusinessConflict
		}
		previous := rec.Business
		if previous == nil {
			if update.State != BusinessAccepted || update.SourceRevision != 0 || rec.Task == nil || rec.State == "acknowledged" || (rec.State == "claimed" && s.now().Before(rec.LeaseExpires)) {
				return ErrBusinessConflict
			}
		}
		if previous != nil {
			if previous.Assignment != update.Assignment {
				return ErrBusinessConflict
			}
			if update.SourceRevision < previous.SourceRevision {
				return ErrBusinessStale
			}
			if update.SourceRevision == previous.SourceRevision {
				if !sameBusinessUpdate(previous.BusinessUpdate, update) {
					return ErrBusinessConflict
				}
				result = rec
				return nil
			}
			if err = validateBusinessTransition(previous.BusinessUpdate, update); err != nil {
				return err
			}
		}
		now := s.now().UTC()
		next := &BusinessReceipt{BusinessUpdate: update, AcceptedAt: now, UpdatedAt: now}
		if previous != nil {
			next.AcceptedAt = previous.AcceptedAt
			next.StartedAt = previous.StartedAt
			next.CompletedAt = previous.CompletedAt
		}
		if update.State == BusinessRunning && next.StartedAt.IsZero() {
			next.StartedAt = now
		}
		if update.State == BusinessCompleted && next.CompletedAt.IsZero() {
			next.CompletedAt = now
		}
		rec.Business = next
		// Graph-backed receipts supersede pre-admission diagnostics. Revision
		// zero is only the crash-safe reservation, which may still be waiting.
		if update.SourceRevision != 0 || (rec.Admission != nil && rec.Admission.Assignment != update.Assignment) {
			rec.Admission = nil
		}
		if err = s.write(rec); err != nil {
			return err
		}
		rec.Version = SchemaVersion
		result = rec
		return nil
	})
	return result, err
}

func validateBusinessTransition(old, next BusinessUpdate) error {
	// Unrelated graph revisions may be reflected, but a terminal outcome cannot
	// be rewritten. Failed work can resume only after explicit graph recovery.
	if old.State == BusinessCompleted || old.State == BusinessCancelled {
		comparable := next
		comparable.SourceRevision = old.SourceRevision
		if !sameBusinessUpdate(old, comparable) {
			return ErrBusinessConflict
		}
		return nil
	}
	if old.State == BusinessFailed || old.State == BusinessBlocked || old.State == BusinessRejected {
		if next.State == old.State {
			if next.Recovery {
				return nil
			}
			comparable := next
			comparable.SourceRevision = old.SourceRevision
			if !sameBusinessUpdate(old, comparable) {
				return ErrBusinessConflict
			}
			return nil
		}
		if !next.Recovery && next.State != BusinessCancelled {
			return ErrBusinessConflict
		}
	}
	if old.State == BusinessRunning && next.State == BusinessAccepted && !next.Recovery {
		return ErrBusinessConflict
	}
	return nil
}

func validateBusinessUpdate(u BusinessUpdate, task *Task) error {
	a := u.Assignment
	for _, v := range []string{a.JobID, a.RunID, a.WorkItemID, a.VerificationItemID} {
		if err := ValidateAddress(v); err != nil {
			return fmt.Errorf("business assignment: %w", err)
		}
	}
	if a.WorkItemID == a.VerificationItemID {
		return errors.New("business execution and verification items must differ")
	}
	if !filepath.IsAbs(a.Workspace) || filepath.Clean(a.Workspace) != a.Workspace || !validText(a.Workspace, 4096) || !validSHA(a.ConfigSHA256) {
		return errors.New("business assignment requires a canonical absolute workspace and config SHA-256")
	}
	if u.SourceRevision == 0 && u.State != BusinessAccepted {
		return errors.New("only initial business reservation may have graph revision zero")
	}
	switch u.State {
	case BusinessAccepted, BusinessRunning, BusinessBlocked, BusinessCompleted, BusinessRejected, BusinessFailed, BusinessCancelled:
	default:
		return errors.New("invalid handoff business state")
	}
	if u.Output != "" && (!validText(u.Output, 32<<10) || u.OutputSHA256 != digestString(u.Output)) {
		return errors.New("business output requires a matching SHA-256 and at most 32 KiB")
	}
	if u.Output == "" && u.OutputSHA256 != "" {
		return errors.New("business output digest has no corresponding output")
	}
	if len(u.Artifacts) > 64 {
		return errors.New("business receipt has more than 64 artifacts")
	}
	seen := map[string]bool{}
	for _, a := range u.Artifacts {
		if !validDeliverable(a.Path) || !validSHA(a.SHA256) || seen[a.Path] {
			return errors.New("business artifacts require distinct managed relative paths and SHA-256")
		}
		seen[a.Path] = true
	}
	if u.Acceptance != nil {
		a := u.Acceptance
		if a.Source != "host" || !validSHA(a.EvidenceSHA256) || (a.Summary != "" && !validText(a.Summary, 4096)) {
			return errors.New("business acceptance requires independent host evidence")
		}
		if len(a.Checks) > 64 {
			return errors.New("business acceptance exceeds 64 host checks")
		}
		for _, check := range a.Checks {
			if !validText(check.Command, 8192) || !validSHA(check.OutputSHA256) || (a.Passed && check.ExitCode != 0) {
				return errors.New("business acceptance contains invalid host check evidence")
			}
		}
	}
	if u.Failure != nil {
		f := u.Failure
		if err := ValidateAddress(f.Code); err != nil {
			return errors.New("business failure requires a bounded code")
		}
		if !validText(f.Summary, 4096) || !validSHA(f.EvidenceSHA256) {
			return errors.New("business failure requires a summary and evidence SHA-256")
		}
	}
	if u.State == BusinessCompleted {
		if u.Acceptance == nil || !u.Acceptance.Passed || len(u.Acceptance.Checks) == 0 || u.Failure != nil || u.Output == "" || !validSHA(u.OutputSHA256) {
			return errors.New("business completion requires verified output and passed independent host acceptance")
		}
		var paths []string
		if task != nil {
			paths = task.Deliverables
		}
		if len(seen) != len(paths) {
			return errors.New("business completion artifacts must match all declared deliverables")
		}
		for _, p := range paths {
			if !seen[p] {
				return errors.New("business completion is missing a declared deliverable")
			}
		}
	}
	if u.State == BusinessFailed || u.State == BusinessRejected || u.State == BusinessBlocked || u.State == BusinessCancelled {
		if u.Failure == nil {
			return errors.New("business failure state requires durable error evidence")
		}
	} else if u.Failure != nil {
		return errors.New("active or completed business state cannot carry failure evidence")
	}
	return nil
}

func validateBusinessReceipt(r *BusinessReceipt, task *Task) error {
	if r == nil {
		return nil
	}
	if task == nil {
		return errors.New("business receipt requires a structured task")
	}
	if err := validateBusinessUpdate(r.BusinessUpdate, task); err != nil {
		return err
	}
	if r.AcceptedAt.IsZero() || r.UpdatedAt.IsZero() || r.UpdatedAt.Before(r.AcceptedAt) {
		return errors.New("invalid business receipt timestamps")
	}
	if r.State == BusinessCompleted && r.CompletedAt.IsZero() {
		return errors.New("business completion has no completion timestamp")
	}
	if r.State != BusinessCompleted && !r.CompletedAt.IsZero() {
		return errors.New("incomplete business receipt has a completion timestamp")
	}
	return nil
}
func digestString(v string) string { h := sha256.Sum256([]byte(v)); return hex.EncodeToString(h[:]) }
func validText(v string, limit int) bool {
	return strings.TrimSpace(v) != "" && len(v) <= limit && utf8.ValidString(v) && !strings.ContainsRune(v, 0)
}
func validDeliverable(v string) bool {
	if !validText(v, 4096) || v != strings.TrimSpace(v) || filepath.IsAbs(v) || filepath.Clean(v) != v || v == "." || v == ".." || strings.HasPrefix(v, "../") || strings.Contains(v, "\\") {
		return false
	}
	for _, c := range v {
		if unicode.IsControl(c) {
			return false
		}
	}
	return true
}

// BusinessEvidenceSHA256 hashes the host's serialized verification evidence.
// The host must persist that evidence before publishing its hash in a receipt.
func BusinessEvidenceSHA256(v any) (string, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	return digestString(string(b)), nil
}

// ClaimID claims only the preassigned delivery. Ordinary Claim deliberately
// skips these records so a generic mailbox consumer cannot steal a job's input.
func (s *Store) ClaimID(ctx context.Context, id, scope, to, consumer string, lease time.Duration, expected BusinessAssignment) (*Record, error) {
	for _, v := range []string{scope, to, consumer} {
		if err := ValidateAddress(v); err != nil {
			return nil, err
		}
	}
	if lease <= 0 {
		lease = DefaultLease
	}
	if lease > 5*time.Minute {
		return nil, errors.New("handoff lease exceeds 5 minutes")
	}
	var claimed *Record
	err := s.locked(ctx, func() error {
		r, err := s.read(id)
		if err != nil {
			return err
		}
		if r.Scope != scope || r.To != to || r.Business == nil || r.Business.Assignment != expected {
			return ErrBusinessConflict
		}
		if r.State == "acknowledged" || (r.State == "claimed" && s.now().Before(r.LeaseExpires)) {
			return nil
		}
		if r.Business.State == BusinessCompleted || r.Business.State == BusinessCancelled || r.Business.State == BusinessRejected || r.Business.State == BusinessFailed {
			return ErrBusinessConflict
		}
		token, err := randomID("claim-", 16)
		if err != nil {
			return err
		}
		r.State = "claimed"
		r.Consumer = consumer
		r.Token = token
		r.LeaseExpires = s.now().UTC().Add(lease)
		r.Attempts++
		r.Admission = nil
		if err = s.write(r); err != nil {
			return err
		}
		r.Version = SchemaVersion
		claimed = &r
		return nil
	})
	return claimed, err
}

func sameBusinessUpdate(a, b BusinessUpdate) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return string(x) == string(y)
}

// ListBusiness returns at most limit structured messages awaiting assignment or
// already tracked by a live/recoverable business receipt, including acknowledged
// inputs. Completed/cancelled receipts and manually acknowledged unassigned
// tasks are excluded. The limit bounds the returned batch, not directory scans.
func (s *Store) ListBusiness(ctx context.Context, scope, to string, limit int) ([]Record, error) {
	rows, _, err := s.ListBusinessPage(ctx, scope, to, "", limit)
	return rows, err
}

type businessCursor struct {
	CreatedAt    time.Time `json:"created_at"`
	ID           string    `json:"id"`
	FilterSHA256 string    `json:"filter_sha256"`
}

// ListBusinessPage advances over an immutable CreatedAt/ID order, so historical
// failures and assignments owned by another run cannot starve later arrivals.
// An empty next cursor marks the end; restart with an empty cursor on the next
// polling pass to include newly arrived messages earlier in that ordering.
func (s *Store) ListBusinessPage(ctx context.Context, scope, to, cursor string, limit int) ([]Record, string, error) {
	if limit <= 0 || limit > 1024 {
		return nil, "", errors.New("business list limit must be between 1 and 1024")
	}
	for _, v := range []string{scope, to} {
		if v != "" {
			if err := ValidateAddress(v); err != nil {
				return nil, "", err
			}
		}
	}
	filter, _ := json.Marshal([]string{scope, to})
	filterSHA := digestString(string(filter))
	var after businessCursor
	if cursor != "" {
		if len(cursor) > 2048 {
			return nil, "", errors.New("invalid business cursor")
		}
		b, err := base64.RawURLEncoding.DecodeString(cursor)
		if err != nil {
			return nil, "", errors.New("invalid business cursor")
		}
		if err = json.Unmarshal(b, &after); err != nil || after.CreatedAt.IsZero() || !validID(after.ID) || after.FilterSHA256 != filterSHA {
			return nil, "", errors.New("business cursor does not match this mailbox")
		}
	}
	var result []Record
	var next string
	err := s.locked(ctx, func() error {
		rows, err := s.list(scope, to, true)
		if err != nil {
			return err
		}
		more := false
		for _, r := range rows {
			if cursor != "" && (r.CreatedAt.Before(after.CreatedAt) || (r.CreatedAt.Equal(after.CreatedAt) && r.ID <= after.ID)) {
				continue
			}
			if r.Task == nil || (r.Business == nil && r.State == "acknowledged") {
				continue
			}
			if r.Business != nil && (r.Business.State == BusinessCompleted || r.Business.State == BusinessCancelled) {
				continue
			}
			if len(result) == limit {
				more = true
				break
			}
			result = append(result, r)
		}
		if more {
			last := result[len(result)-1]
			b, _ := json.Marshal(businessCursor{CreatedAt: last.CreatedAt, ID: last.ID, FilterSHA256: filterSHA})
			next = base64.RawURLEncoding.EncodeToString(b)
		}
		return nil
	})
	return result, next, err
}
