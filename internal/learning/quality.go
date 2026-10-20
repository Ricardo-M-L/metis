package learning

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// QualityPolicy is host configuration, never model-authored policy. A task's
// result is admitted only with a prior activation binding and host evidence.
type QualityPolicy struct {
	Kind           string  `json:"kind"`
	WindowSize     int     `json:"window_size"`
	MinimumSamples int     `json:"minimum_samples"`
	MaxFailureRate float64 `json:"max_failure_rate"`
	Action         string  `json:"action"` // rollback (falling back to pause) or pause
	Verifier       string  `json:"verifier"`
}

func (p QualityPolicy) Validate() error {
	if (p.Kind != "skill" && p.Kind != "workflow") || p.WindowSize < 1 || p.WindowSize > 256 || p.MinimumSamples < 1 || p.MinimumSamples > p.WindowSize || math.IsNaN(p.MaxFailureRate) || math.IsInf(p.MaxFailureRate, 0) || p.MaxFailureRate < 0 || p.MaxFailureRate >= 1 || (p.Action != "rollback" && p.Action != "pause") || strings.TrimSpace(p.Verifier) == "" || len(p.Verifier) > 128 {
		return errors.New("quality policy requires kind, host verifier, window 1..256, minimum samples, failure allowance [0,1), and rollback or pause")
	}
	return nil
}

type OutcomeBinding struct {
	ID              string    `json:"id"`
	OutcomeID       string    `json:"outcome_id"`
	Kind            string    `json:"kind"`
	Name            string    `json:"name"`
	VersionSHA256   string    `json:"version_sha256"`
	ActivationEpoch uint64    `json:"activation_epoch"`
	PolicyRevision  uint64    `json:"policy_revision"`
	BoundAt         time.Time `json:"bound_at"`
}

// HostOutcome must come from a host-owned checker. InvocationSHA256 is the
// digest of a trace proving this bound artifact was used, not a model's claim.
// EvidenceSHA256 pins the independent checker receipt (including its result).
type HostOutcome struct {
	Passed           bool   `json:"passed"`
	Verifier         string `json:"verifier"`
	InvocationSHA256 string `json:"invocation_sha256"`
	EvidenceSHA256   string `json:"evidence_sha256"`
	ReasonCode       string `json:"reason_code,omitempty"`
}
type QualityDecision struct {
	FeedbackID  string    `json:"feedback_id"`
	Action      string    `json:"action"` // observed, ignored_stale, rolled_back, paused
	Samples     int       `json:"samples"`
	Failures    int       `json:"failures"`
	FailureRate float64   `json:"failure_rate"`
	FromVersion string    `json:"from_version,omitempty"`
	ToVersion   string    `json:"to_version,omitempty"`
	At          time.Time `json:"at"`
}
type QualityRecord struct {
	Binding  OutcomeBinding   `json:"binding"`
	Outcome  *HostOutcome     `json:"outcome,omitempty"`
	Decision *QualityDecision `json:"decision,omitempty"`
}
type QualityState struct {
	Policy          QualityPolicy   `json:"policy"`
	PolicyRevision  uint64          `json:"policy_revision"`
	Paused          bool            `json:"paused"`
	PausedReason    string          `json:"paused_reason,omitempty"`
	Records         []QualityRecord `json:"records,omitempty"`
	ArchivedRecords uint64          `json:"archived_records,omitempty"`
}

func qualityBindingID(kind, name, outcomeID string) string {
	return Digest([]byte(kind + "\x00" + name + "\x00" + outcomeID))
}
func qualityFeedbackID(b OutcomeBinding, o HostOutcome) string {
	content, _ := json.Marshal(struct {
		Binding OutcomeBinding
		Outcome HostOutcome
	}{b, o})
	return Digest(content)
}
func validateQualityState(st State) error {
	q := st.Quality
	if q == nil {
		if st.Suspended {
			return errors.New("suspended artifact requires quality state")
		}
		return nil
	}
	if q.Policy.Validate() != nil || q.PolicyRevision == 0 || len(q.Records) > 1024 {
		return errors.New("invalid learning quality state")
	}
	seen := map[string]bool{}
	for _, r := range q.Records {
		b := r.Binding
		if b.Kind != q.Policy.Kind || b.Name != st.Name || b.ID != qualityBindingID(b.Kind, b.Name, b.OutcomeID) || len(b.OutcomeID) == 0 || len(b.OutcomeID) > 256 || !controllerDigestValid(b.VersionSHA256) || b.ActivationEpoch == 0 || b.PolicyRevision == 0 || b.BoundAt.IsZero() || seen[b.ID] {
			return errors.New("invalid quality outcome binding")
		}
		seen[b.ID] = true
		if (r.Outcome == nil) != (r.Decision == nil) {
			return errors.New("incomplete quality receipt")
		}
		if r.Outcome != nil && (validateHostOutcome(*r.Outcome) != nil || r.Decision.FeedbackID != qualityFeedbackID(b, *r.Outcome)) {
			return errors.New("quality feedback evidence mismatch")
		}
	}
	return nil
}
func validateHostOutcome(o HostOutcome) error {
	if strings.TrimSpace(o.Verifier) == "" || len(o.Verifier) > 128 || !controllerDigestValid(o.InvocationSHA256) || !controllerDigestValid(o.EvidenceSHA256) || len(o.ReasonCode) > 128 || strings.ContainsAny(o.ReasonCode, "\r\n") {
		return errors.New("quality feedback requires independent host verifier and invocation/evidence SHA256")
	}
	return nil
}

func (s *Store) ConfigureQuality(ctx context.Context, name string, p QualityPolicy) (QualityState, error) {
	if err := p.Validate(); err != nil {
		return QualityState{}, err
	}
	var out QualityState
	err := s.mutate(ctx, name, func(st *State) error {
		if st.ActiveVersion == "" {
			return errors.New("quality policy requires an activated artifact")
		}
		if st.Quality != nil && st.Quality.Policy.Kind != p.Kind {
			return errors.New("quality artifact kind cannot change")
		}
		if st.Quality == nil {
			st.Quality = &QualityState{Policy: p, PolicyRevision: 1}
		} else if st.Quality.Policy != p {
			st.Quality.Policy = p
			st.Quality.PolicyRevision++
		}
		// Reconfiguration is not approval to reactivate a suspended deployment.
		out = *st.Quality
		return nil
	})
	return out, err
}

// BindOutcome is called before business execution, or from a host trace that
// captured the exact activation before execution. A candidate cannot bind.
func (s *Store) BindOutcome(ctx context.Context, name, outcomeID string) (OutcomeBinding, error) {
	return s.bindOutcome(ctx, name, outcomeID, "", 0)
}

// BindOutcomeForActivation is the invocation hook's CAS. The host reads the
// activation matching the actually loaded manifest and supplies both fields;
// a concurrent promotion or resume makes binding fail before tool execution.
func (s *Store) BindOutcomeForActivation(ctx context.Context, name, outcomeID, expectedVersion string, expectedEpoch uint64) (OutcomeBinding, error) {
	if !controllerDigestValid(expectedVersion) || expectedEpoch == 0 {
		return OutcomeBinding{}, errors.New("quality invocation requires an exact version and activation epoch")
	}
	return s.bindOutcome(ctx, name, outcomeID, expectedVersion, expectedEpoch)
}

func (s *Store) bindOutcome(ctx context.Context, name, outcomeID, expectedVersion string, expectedEpoch uint64) (OutcomeBinding, error) {
	if len(outcomeID) == 0 || len(outcomeID) > 256 {
		return OutcomeBinding{}, errors.New("outcome ID requires 1..256 bytes")
	}
	var out OutcomeBinding
	err := s.mutate(ctx, name, func(st *State) error {
		q := st.Quality
		if q == nil {
			return errors.New("quality policy absent")
		}
		id := qualityBindingID(q.Policy.Kind, name, outcomeID)
		for _, r := range q.Records {
			if r.Binding.ID == id {
				if expectedVersion != "" && (r.Binding.VersionSHA256 != expectedVersion || r.Binding.ActivationEpoch != expectedEpoch) {
					return errors.New("business outcome already bound to a different activation")
				}
				out = r.Binding
				return nil
			}
		}
		if r, err := s.readArchivedQuality(name, id); err == nil {
			if expectedVersion != "" && (r.Binding.VersionSHA256 != expectedVersion || r.Binding.ActivationEpoch != expectedEpoch) {
				return errors.New("business outcome already bound to a different activation")
			}
			out = r.Binding
			return nil
		} else if !os.IsNotExist(err) {
			return err
		}
		if st.ActiveVersion == "" || st.Suspended {
			return errors.New("artifact has no executable active deployment")
		}
		if expectedVersion != "" && (st.ActiveVersion != expectedVersion || st.ActivationEpoch != expectedEpoch) {
			return errors.New("artifact activation changed before business invocation")
		}
		if len(q.Records) >= 1024 {
			if err := s.compactQuality(st); err != nil {
				return err
			}
		}
		if len(q.Records) >= 1024 {
			return errors.New("quality has 1024 unresolved task bindings; finish or explicitly reconcile pending tasks")
		}
		out = OutcomeBinding{ID: id, OutcomeID: outcomeID, Kind: q.Policy.Kind, Name: name, VersionSHA256: st.ActiveVersion, ActivationEpoch: st.ActivationEpoch, PolicyRevision: q.PolicyRevision, BoundAt: time.Now().UTC()}
		q.Records = append(q.Records, QualityRecord{Binding: out})
		return nil
	})
	return out, err
}

func (s *Store) RecordOutcome(ctx context.Context, name string, b OutcomeBinding, o HostOutcome) (QualityDecision, error) {
	if err := validateHostOutcome(o); err != nil {
		return QualityDecision{}, err
	}
	var out QualityDecision
	err := s.mutate(ctx, name, func(st *State) error {
		q := st.Quality
		if q == nil {
			return errors.New("quality policy absent")
		}
		index := -1
		for i, r := range q.Records {
			if r.Binding.ID == b.ID {
				index = i
				break
			}
		}
		var record QualityRecord
		if index < 0 {
			var err error
			record, err = s.readArchivedQuality(name, b.ID)
			if err != nil {
				return errors.New("quality feedback requires a persisted activation binding")
			}
		} else {
			record = q.Records[index]
		}
		if record.Binding != b {
			return errors.New("quality feedback binding does not match the captured deployment")
		}
		feedbackID := qualityFeedbackID(b, o)
		if record.Outcome != nil {
			if record.Decision.FeedbackID != feedbackID {
				return errors.New("conflicting quality result for the same business outcome")
			}
			out = *record.Decision
			return nil
		}
		if o.Verifier != q.Policy.Verifier {
			return errors.New("quality outcome verifier differs from host policy")
		}
		out = QualityDecision{FeedbackID: feedbackID, Action: "observed", At: time.Now().UTC()}
		record.Outcome = &o
		if b.ActivationEpoch != st.ActivationEpoch || b.VersionSHA256 != st.ActiveVersion || b.PolicyRevision != q.PolicyRevision || q.Paused || st.Suspended {
			out.Action = "ignored_stale"
		} else {
			// Window order is host receipt commit order; duplicate deliveries never
			// count twice, including after restart or archival.
			window := []bool{o.Passed}
			past := append([]QualityRecord(nil), q.Records...)
			sort.SliceStable(past, func(i, j int) bool {
				if past[i].Decision == nil {
					return false
				}
				if past[j].Decision == nil {
					return true
				}
				return past[i].Decision.At.After(past[j].Decision.At)
			})
			for i := 0; i < len(past) && len(window) < q.Policy.WindowSize; i++ {
				r := past[i]
				if r.Binding.ID != b.ID && r.Outcome != nil && r.Binding.ActivationEpoch == b.ActivationEpoch && r.Binding.PolicyRevision == b.PolicyRevision && r.Binding.VersionSHA256 == b.VersionSHA256 && r.Decision.Action != "ignored_stale" {
					window = append(window, r.Outcome.Passed)
				}
			}
			out.Samples = len(window)
			for _, passed := range window {
				if !passed {
					out.Failures++
				}
			}
			out.FailureRate = float64(out.Failures) / float64(out.Samples)
			if out.Samples >= q.Policy.MinimumSamples && out.FailureRate > q.Policy.MaxFailureRate {
				q.Paused = true
				q.PausedReason = "host_quality_failure_rate_exceeded"
				out.FromVersion = st.ActiveVersion
				target := st.Versions[st.ActiveVersion].Parent
				if q.Policy.Action == "rollback" && target != "" {
					if _, ok := st.Versions[target]; !ok {
						return errors.New("quality rollback target absent; refusing an unverified rollback")
					}
					st.ActiveVersion = target
					st.ActivationEpoch++
					out.ToVersion = target
					out.Action = "rolled_back"
				} else {
					st.Suspended = true
					st.ActivationEpoch++
					out.Action = "paused"
				}
				st.Transitions = append(st.Transitions, Transition{Kind: "quality_" + out.Action, From: out.FromVersion, To: out.ToVersion, FeedbackID: feedbackID, At: out.At})
			}
		}
		record.Decision = &out
		q.Records[index] = record
		return s.compactQuality(st)
	})
	return out, err
}

// ResumeQuality requires host approval and advances the epoch. Outcomes still
// in flight for the old deployment cannot become samples in this new window.
func (s *Store) ResumeQuality(ctx context.Context, name string) error {
	return s.mutate(ctx, name, func(st *State) error {
		if st.Quality == nil {
			return errors.New("quality policy absent")
		}
		if st.Quality.Paused || st.Suspended {
			st.Quality.Paused = false
			st.Quality.PausedReason = ""
			st.Suspended = false
			st.ActivationEpoch++
			st.Transitions = append(st.Transitions, Transition{Kind: "quality_resumed", To: st.ActiveVersion, At: time.Now().UTC()})
		}
		return nil
	})
}

func (s *Store) qualityArchivePath(name, id string) string {
	return filepath.Join(s.dir, name, "archive", "quality", id+".json")
}
func (s *Store) readArchivedQuality(name, id string) (QualityRecord, error) {
	var r QualityRecord
	if !controllerDigestValid(id) {
		return r, errors.New("invalid quality binding ID")
	}
	data, err := readLearningArchive(s.qualityArchivePath(name, id), 8192)
	if err != nil {
		return r, err
	}
	if len(data) > 8192 || json.Unmarshal(data, &r) != nil || r.Binding.Name != name || r.Binding.ID != id || id != qualityBindingID(r.Binding.Kind, name, r.Binding.OutcomeID) || r.Outcome == nil || r.Decision == nil || validateHostOutcome(*r.Outcome) != nil || r.Decision.FeedbackID != qualityFeedbackID(r.Binding, *r.Outcome) {
		return QualityRecord{}, errors.New("invalid archived quality evidence")
	}
	if err := validateQualityLookupRecord(name, id, r); err != nil {
		return QualityRecord{}, err
	}
	return r, nil
}

// LookupOutcome inspects the same immutable binding in hot or cold storage.
// It never creates a state or changes an earlier business verdict when a host
// checker is later recovered and rerun. Pending records have no Outcome.
func (s *Store) LookupOutcome(name, bindingID string) (QualityRecord, error) {
	if !controllerDigestValid(bindingID) {
		return QualityRecord{}, errors.New("invalid quality binding ID")
	}
	st, err := s.Read(name)
	if err != nil {
		return QualityRecord{}, err
	}
	if st.Quality != nil {
		for _, r := range st.Quality.Records {
			if r.Binding.ID == bindingID {
				if err := validateQualityLookupRecord(name, bindingID, r); err != nil {
					return QualityRecord{}, err
				}
				return r, nil
			}
		}
	}
	return s.readArchivedQuality(name, bindingID)
}

func validateQualityLookupRecord(name, id string, r QualityRecord) error {
	b := r.Binding
	if ValidateName(name) != nil || b.Name != name || b.ID != id || (b.Kind != "skill" && b.Kind != "workflow") || len(b.OutcomeID) == 0 || len(b.OutcomeID) > 256 || id != qualityBindingID(b.Kind, name, b.OutcomeID) || !controllerDigestValid(b.VersionSHA256) || b.ActivationEpoch == 0 || b.PolicyRevision == 0 || b.BoundAt.IsZero() || (r.Outcome == nil) != (r.Decision == nil) {
		return errors.New("invalid quality outcome lookup evidence")
	}
	if r.Outcome == nil {
		return nil
	}
	d := r.Decision
	if validateHostOutcome(*r.Outcome) != nil || d.FeedbackID != qualityFeedbackID(b, *r.Outcome) || d.At.IsZero() || d.Samples < 0 || d.Samples > 256 || d.Failures < 0 || d.Failures > d.Samples || math.IsNaN(d.FailureRate) || math.IsInf(d.FailureRate, 0) || d.FailureRate < 0 || d.FailureRate > 1 {
		return errors.New("quality outcome lookup receipt mismatch")
	}
	if d.Samples > 0 && d.FailureRate != float64(d.Failures)/float64(d.Samples) {
		return errors.New("quality outcome lookup summary mismatch")
	}
	switch d.Action {
	case "observed":
		if d.Samples == 0 || d.FromVersion != "" || d.ToVersion != "" {
			return errors.New("invalid observed quality receipt")
		}
	case "ignored_stale":
		if d.Samples != 0 || d.Failures != 0 || d.FailureRate != 0 || d.FromVersion != "" || d.ToVersion != "" {
			return errors.New("invalid stale quality receipt")
		}
	case "rolled_back":
		if d.Samples == 0 || d.FromVersion != b.VersionSHA256 || !controllerDigestValid(d.ToVersion) {
			return errors.New("invalid quality rollback receipt")
		}
	case "paused":
		if d.Samples == 0 || d.FromVersion != b.VersionSHA256 || d.ToVersion != "" {
			return errors.New("invalid quality pause receipt")
		}
	default:
		return errors.New("invalid quality outcome action")
	}
	return nil
}
func (s *Store) compactQuality(st *State) error {
	q := st.Quality
	if q == nil || len(q.Records) <= 512 {
		return nil
	}
	// Preserve the full current statistics window and every unfinished binding.
	keepCompleted := max(128, q.Policy.WindowSize)
	var complete []QualityRecord
	for _, r := range q.Records {
		if r.Outcome != nil {
			complete = append(complete, r)
		}
	}
	if len(complete) <= keepCompleted {
		return nil
	}
	sort.SliceStable(complete, func(i, j int) bool { return complete[i].Decision.At.After(complete[j].Decision.At) })
	keep := map[string]bool{}
	for _, r := range complete[:keepCompleted] {
		keep[r.Binding.ID] = true
	}
	retained := make([]QualityRecord, 0, len(q.Records))
	for _, r := range q.Records {
		if r.Outcome != nil && !keep[r.Binding.ID] {
			data, err := json.Marshal(r)
			if err != nil {
				return err
			}
			if err := writeLearningArchive(s.qualityArchivePath(st.Name, r.Binding.ID), data); err != nil {
				return fmt.Errorf("archive quality evidence: %w", err)
			}
			q.ArchivedRecords++
		} else {
			retained = append(retained, r)
		}
	}
	q.Records = retained
	return nil
}

// RollbackCurrent is the host UI's reviewed-deployment CAS. It cannot roll a
// different activation back because an operator viewed an earlier status.
func (s *Store) RollbackCurrent(ctx context.Context, name, expectedVersion string, expectedEpoch uint64, note string) error {
	if !controllerDigestValid(expectedVersion) || expectedEpoch == 0 || strings.TrimSpace(note) == "" || len(note) > 512 {
		return errors.New("reviewed rollback requires exact activation and bounded host note")
	}
	return s.mutate(ctx, name, func(st *State) error {
		if st.ActiveVersion != expectedVersion || st.ActivationEpoch != expectedEpoch {
			return errors.New("artifact activation changed since host review")
		}
		target := st.Versions[st.ActiveVersion].Parent
		if target == "" || target == st.ActiveVersion {
			return errors.New("current activation has no prior version to restore")
		}
		if _, ok := st.Versions[target]; !ok {
			return errors.New("prior activated version absent")
		}
		st.Transitions = append(st.Transitions, Transition{Kind: "host_rollback", From: st.ActiveVersion, To: target, Note: note, At: time.Now().UTC()})
		st.ActiveVersion = target
		st.ActivationEpoch++
		st.Suspended = false
		if st.Quality != nil {
			st.Quality.Paused = true
			st.Quality.PausedReason = "host_reviewed_rollback"
		}
		return nil
	})
}
func (s *Store) ResumeQualityCurrent(ctx context.Context, name, expectedVersion string, expectedEpoch uint64, note string) error {
	if !controllerDigestValid(expectedVersion) || expectedEpoch == 0 || strings.TrimSpace(note) == "" || len(note) > 512 {
		return errors.New("quality resume requires exact reviewed activation and bounded host note")
	}
	return s.mutate(ctx, name, func(st *State) error {
		if st.ActiveVersion != expectedVersion || st.ActivationEpoch != expectedEpoch {
			return errors.New("artifact activation changed since host review")
		}
		if st.Quality == nil {
			return errors.New("quality policy absent")
		}
		if st.Quality.Paused || st.Suspended {
			st.Quality.Paused = false
			st.Quality.PausedReason = ""
			st.Suspended = false
			st.ActivationEpoch++
			st.Transitions = append(st.Transitions, Transition{Kind: "quality_resumed", To: st.ActiveVersion, Note: note, At: time.Now().UTC()})
		}
		return nil
	})
}
