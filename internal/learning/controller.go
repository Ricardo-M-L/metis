package learning

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const maxControllerBytes = 16 * 1024 * 1024

// ControllerIdentity pins the executable and the selected, secret-free provider
// configuration. Changing the host's route requires a new explicit policy.
type ControllerIdentity struct {
	BinaryPath           string `json:"binary_path"`
	BinarySHA256         string `json:"binary_sha256"`
	Provider             string `json:"provider"`
	Model                string `json:"model"`
	ProviderConfigSHA256 string `json:"provider_config_sha256"`
}

type ControllerPolicySpec struct {
	Kind            string             `json:"kind"`
	Name            string             `json:"name"`
	ArtifactRoot    string             `json:"artifact_root"`
	Suite           Suite              `json:"suite"`
	Identity        ControllerIdentity `json:"identity"`
	AutoPromote     bool               `json:"auto_promote"`
	MaxEvaluations  int                `json:"max_evaluations"`
	MaxTotalTokens  int64              `json:"max_total_tokens"`
	WorkflowFixture json.RawMessage    `json:"workflow_fixture,omitempty"`
}

// A policy contains the frozen checker, not a path that an author can replace.
// Identical configure calls retain the revision and its spent budget.
type ControllerPolicy struct {
	ControllerPolicySpec
	Revision    uint64    `json:"revision"`
	SHA256      string    `json:"sha256"`
	SuiteSHA256 string    `json:"suite_sha256"`
	Configured  time.Time `json:"configured_at"`
}

type ControllerReceipt struct {
	ID              string    `json:"id"`
	PolicyKey       string    `json:"policy_key"`
	PolicyRevision  uint64    `json:"policy_revision"`
	PolicySHA256    string    `json:"policy_sha256"`
	ActiveEpoch     uint64    `json:"active_epoch"`
	BaselineVersion string    `json:"baseline_version,omitempty"`
	CandidateSHA256 string    `json:"candidate_sha256"`
	Status          string    `json:"status"`
	ReservedTokens  int64     `json:"reserved_tokens"`
	ChargedTokens   int64     `json:"charged_tokens"`
	ReportedTokens  int64     `json:"reported_tokens"`
	EvaluationID    string    `json:"evaluation_id,omitempty"`
	Reason          string    `json:"reason,omitempty"`
	StartedAt       time.Time `json:"started_at"`
	FinishedAt      time.Time `json:"finished_at,omitempty"`
}

type ControllerLease struct {
	PID       int       `json:"pid"`
	StartedAt time.Time `json:"started_at"`
}

type ControllerState struct {
	Schema           int                                 `json:"schema"`
	Policies         map[string]ControllerPolicy         `json:"policies"`
	Receipts         []ControllerReceipt                 `json:"receipts,omitempty"`
	Lease            *ControllerLease                    `json:"lease,omitempty"`
	ArchivedBudget   map[string]ControllerArchivedBudget `json:"archived_budget,omitempty"`
	ArchivedReceipts uint64                              `json:"archived_receipts,omitempty"`
}

type ControllerArchivedBudget struct {
	PolicyRevision uint64 `json:"policy_revision"`
	Attempts       int    `json:"attempts"`
	ChargedTokens  int64  `json:"charged_tokens"`
}

type Controller struct{ dir string }

func NewController(dir string) *Controller { return &Controller{dir: dir} }
func (c *Controller) Dir() string          { return c.dir }

func controllerKey(kind, name string) string { return kind + "/" + name }
func controllerReceiptID(p ControllerPolicy, epoch uint64, candidate string) string {
	return controllerReceiptDigest(controllerKey(p.Kind, p.Name), p.Revision, epoch, candidate)
}
func controllerReceiptDigest(key string, revision, epoch uint64, candidate string) string {
	return Digest([]byte(fmt.Sprintf("%s:%d:%d:%s", key, revision, epoch, candidate)))
}
func controllerSpecDigest(p ControllerPolicySpec) string {
	b, _ := json.Marshal(p)
	return Digest(b)
}
func controllerDigestValid(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, ch := range s {
		if !(ch >= '0' && ch <= '9') && !(ch >= 'a' && ch <= 'f') {
			return false
		}
	}
	return true
}
func (p ControllerPolicySpec) Validate() error {
	if p.Kind != "skill" && p.Kind != "workflow" {
		return errors.New("controller kind must be skill or workflow")
	}
	if err := ValidateName(p.Name); err != nil {
		return err
	}
	if !filepath.IsAbs(p.ArtifactRoot) {
		return errors.New("controller requires a fixed absolute artifact root")
	}
	if err := p.Suite.Validate(); err != nil {
		return err
	}
	if len(p.WorkflowFixture) > 0 {
		var fixture map[string]json.RawMessage
		if p.Kind != "workflow" || len(p.WorkflowFixture) > 1024*1024 || json.Unmarshal(p.WorkflowFixture, &fixture) != nil || fixture == nil {
			return errors.New("controller workflow fixture must be a bounded JSON object for a workflow policy")
		}
	}
	i := p.Identity
	if !filepath.IsAbs(i.BinaryPath) || !controllerDigestValid(i.BinarySHA256) || !controllerDigestValid(i.ProviderConfigSHA256) || strings.TrimSpace(i.Provider) == "" || strings.TrimSpace(i.Model) == "" {
		return errors.New("controller policy requires a pinned CLI and provider route")
	}
	if p.MaxEvaluations < 1 || p.MaxEvaluations > 256 || p.MaxTotalTokens < int64(p.Suite.MaxTokens)*2 || p.MaxTotalTokens > 1000000000 {
		return errors.New("controller requires 1..256 evaluations and a bounded token allowance for both versions")
	}
	return nil
}

func (c *Controller) Read() (ControllerState, error) {
	f, err := os.Open(filepath.Join(c.dir, "state.json"))
	if os.IsNotExist(err) {
		return ControllerState{Schema: 2, Policies: map[string]ControllerPolicy{}}, nil
	}
	if err != nil {
		return ControllerState{}, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxControllerBytes+1))
	if err != nil {
		return ControllerState{}, err
	}
	if len(b) > maxControllerBytes {
		return ControllerState{}, errors.New("controller state exceeds storage limit")
	}
	var st ControllerState
	if err := json.Unmarshal(b, &st); err != nil {
		return st, err
	}
	if (st.Schema != 1 && st.Schema != 2) || st.Policies == nil || len(st.Policies) > 128 || len(st.Receipts) > 4096 || len(st.ArchivedBudget) > 128 {
		return st, errors.New("invalid controller state")
	}
	for key, budget := range st.ArchivedBudget {
		p, ok := st.Policies[key]
		if !ok || budget.PolicyRevision != p.Revision || budget.Attempts < 0 || budget.ChargedTokens < 0 {
			return st, errors.New("invalid archived controller budget")
		}
	}
	for key, p := range st.Policies {
		if p.Validate() != nil || key != controllerKey(p.Kind, p.Name) || p.Revision == 0 || p.SHA256 != controllerSpecDigest(p.ControllerPolicySpec) || p.SuiteSHA256 != SuiteDigest(p.Suite) {
			return st, errors.New("controller policy digest mismatch")
		}
	}
	seen := map[string]bool{}
	for _, r := range st.Receipts {
		kind, name, ok := strings.Cut(r.PolicyKey, "/")
		if !ok || (kind != "skill" && kind != "workflow") || ValidateName(name) != nil || r.ID != controllerReceiptDigest(r.PolicyKey, r.PolicyRevision, r.ActiveEpoch, r.CandidateSHA256) {
			return st, errors.New("controller receipt identity mismatch")
		}
		if !controllerDigestValid(r.ID) || seen[r.ID] || !controllerDigestValid(r.CandidateSHA256) || !controllerDigestValid(r.PolicySHA256) || r.PolicyRevision == 0 || r.ReservedTokens <= 0 || r.ChargedTokens < 0 || r.ReportedTokens < 0 {
			return st, errors.New("invalid controller receipt")
		}
		seen[r.ID] = true
		switch r.Status {
		case "reserved", "unknown", "evaluated", "rejected", "promoted", "promotion_failed":
		default:
			return st, errors.New("invalid controller receipt status")
		}
	}
	return st, nil
}

func (c *Controller) save(st ControllerState) error {
	// Old schema-1 writers cannot preserve frozen fixtures, charged archive
	// budgets or quality policy. Fence them on the first actual mutation.
	st.Schema = 2
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	if len(b) > maxControllerBytes {
		return errors.New("controller state exceeds storage limit")
	}
	f, err := os.CreateTemp(c.dir, ".controller-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err = f.Chmod(0o600); err == nil {
		_, err = f.Write(b)
	}
	if err == nil {
		err = f.Sync()
	}
	err = errors.Join(err, f.Close())
	if err != nil {
		return err
	}
	if err := os.Rename(f.Name(), filepath.Join(c.dir, "state.json")); err != nil {
		return err
	}
	d, err := os.Open(c.dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
func (c *Controller) locked(ctx context.Context) (func(), error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := os.MkdirAll(c.dir, 0o700); err != nil {
		return nil, err
	}
	return lock(ctx, filepath.Join(c.dir, ".controller.lock"))
}

func (c *Controller) Configure(ctx context.Context, spec ControllerPolicySpec) (ControllerPolicy, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := spec.Validate(); err != nil {
		return ControllerPolicy{}, err
	}
	unlock, err := c.locked(ctx)
	if err != nil {
		return ControllerPolicy{}, err
	}
	defer unlock()
	st, err := c.Read()
	if err != nil {
		return ControllerPolicy{}, err
	}
	if err := ctx.Err(); err != nil {
		return ControllerPolicy{}, err
	}
	key := controllerKey(spec.Kind, spec.Name)
	digest := controllerSpecDigest(spec)
	old, exists := st.Policies[key]
	if exists && old.SHA256 == digest {
		return old, nil
	}
	if !exists && len(st.Policies) >= 128 {
		return ControllerPolicy{}, errors.New("controller policy limit reached")
	}
	p := ControllerPolicy{ControllerPolicySpec: spec, Revision: old.Revision + 1, SHA256: digest, SuiteSHA256: SuiteDigest(spec.Suite), Configured: time.Now().UTC()}
	st.Policies[key] = p
	delete(st.ArchivedBudget, key)
	return p, c.save(st)
}

// Backend is the host-owned adapter to the candidate stores and the real CLI
// evaluator. The controller never exposes its checker to an author query.
type ControllerBackend struct {
	Store    func(ControllerPolicy) (*Store, error)
	Evaluate func(context.Context, ControllerPolicy, string) (Evaluation, error)
}
type ControllerTick struct {
	Receipt         *ControllerReceipt `json:"receipt,omitempty"`
	BudgetExhausted []string           `json:"budget_exhausted,omitempty"`
	QualityPaused   []string           `json:"quality_paused,omitempty"`
	Idle            bool               `json:"idle"`
}

// RunOnce attempts at most one new candidate. The OS lock is its exclusive
// controller lease; a process crash releases the lock, but not a reservation.
// An uncertain attempt is never automatically retried or refunded.
func (c *Controller) RunOnce(ctx context.Context, backend ControllerBackend) (ControllerTick, error) {
	return c.runOnce(ctx, backend, "")
}

// RunPolicyOnce selects one already configured host policy. It cannot replace
// the frozen suite or increase that policy revision's evaluation allowance.
func (c *Controller) RunPolicyOnce(ctx context.Context, backend ControllerBackend, kind, name string) (ControllerTick, error) {
	if (kind != "skill" && kind != "workflow") || ValidateName(name) != nil {
		return ControllerTick{}, errors.New("invalid controller policy selector")
	}
	return c.runOnce(ctx, backend, controllerKey(kind, name))
}

func (c *Controller) runOnce(ctx context.Context, backend ControllerBackend, selection string) (result ControllerTick, err error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if backend.Store == nil || backend.Evaluate == nil {
		return result, errors.New("controller requires a host evaluator backend")
	}
	unlock, err := c.locked(ctx)
	if err != nil {
		return result, err
	}
	defer unlock()
	st, err := c.Read()
	if err != nil {
		return result, err
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if selection != "" {
		if _, ok := st.Policies[selection]; !ok {
			return result, errors.New("learning controller policy absent")
		}
	}
	needsRecoverySave := st.Lease != nil
	if len(st.Receipts) >= 3072 {
		if _, err := c.archiveReceiptsState(&st, 256); err != nil {
			return result, err
		}
		needsRecoverySave = true
	}
	// This process owns the lock, so every surviving reservation belongs to an
	// interrupted predecessor. Retain its full charge and mark the uncertainty.
	st.Lease = nil
	for i := range st.Receipts {
		if st.Receipts[i].Status == "reserved" {
			st.Receipts[i].Status = "unknown"
			st.Receipts[i].Reason = "controller interrupted; reserved allowance retained"
			needsRecoverySave = true
		}
	}
	// Empty polls are read-only. Rewriting a large checker/history record on
	// every idle interval would make a permanent watcher needlessly expensive.
	defer func() {
		if needsRecoverySave || st.Lease != nil {
			st.Lease = nil
			err = errors.Join(err, c.save(st))
		}
	}()
	keys := make([]string, 0, len(st.Policies))
	for key := range st.Policies {
		if selection != "" && key != selection {
			continue
		}
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		p := st.Policies[key]
		archived := st.ArchivedBudget[key]
		attempts, charged := archived.Attempts, archived.ChargedTokens
		seen := map[string]bool{}
		for _, r := range st.Receipts {
			if r.PolicyKey == key && r.PolicyRevision == p.Revision {
				attempts++
				charged = controllerAddTokens(charged, r.ChargedTokens)
				seen[r.ID] = true
			}
		}
		reservation := int64(p.Suite.MaxTokens) * 2
		if attempts >= p.MaxEvaluations || charged > p.MaxTotalTokens-reservation {
			result.BudgetExhausted = append(result.BudgetExhausted, key)
			continue
		}
		store, err := backend.Store(p)
		if err != nil {
			return result, err
		}
		candidateState, err := store.Read(p.Name)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return result, err
		}
		if candidateState.Suspended || (candidateState.Quality != nil && candidateState.Quality.Paused) {
			result.QualityPaused = append(result.QualityPaused, key)
			continue
		}
		var candidates []Version
		for _, v := range candidateState.Versions {
			if v.ID != candidateState.ActiveVersion && v.Parent == candidateState.ActiveVersion && v.ParentEpoch == candidateState.ActivationEpoch && !seen[controllerReceiptID(p, candidateState.ActivationEpoch, v.ID)] {
				if _, err := c.readArchivedReceipt(controllerReceiptID(p, candidateState.ActivationEpoch, v.ID)); err == nil {
					continue
				} else if !os.IsNotExist(err) {
					return result, err
				}
				candidates = append(candidates, v)
			}
		}
		sort.Slice(candidates, func(i, j int) bool {
			if candidates[i].CreatedAt.Equal(candidates[j].CreatedAt) {
				return candidates[i].ID < candidates[j].ID
			}
			return candidates[i].CreatedAt.Before(candidates[j].CreatedAt)
		})
		if len(candidates) == 0 {
			continue
		}
		if len(st.Receipts) >= 4096 {
			return result, errors.New("controller receipt limit reached; archive before further evaluation")
		}
		v := candidates[0]
		r := ControllerReceipt{ID: controllerReceiptID(p, candidateState.ActivationEpoch, v.ID), PolicyKey: key, PolicyRevision: p.Revision, PolicySHA256: p.SHA256, ActiveEpoch: candidateState.ActivationEpoch, BaselineVersion: candidateState.ActiveVersion, CandidateSHA256: v.ID, Status: "reserved", ReservedTokens: reservation, ChargedTokens: reservation, StartedAt: time.Now().UTC()}
		st.Lease = &ControllerLease{PID: os.Getpid(), StartedAt: r.StartedAt}
		st.Receipts = append(st.Receipts, r)
		if err := c.save(st); err != nil {
			return result, err
		}
		e, evalErr := backend.Evaluate(ctx, p, v.ID)
		r.FinishedAt = time.Now().UTC()
		r.Status = "unknown"
		r.Reason = "evaluation outcome or usage unavailable; reserved allowance retained"
		if controllerEvaluationBound(e, p, r) {
			r.EvaluationID = e.ID
			tokens, complete := controllerEvaluationTokens(e)
			r.ReportedTokens = tokens
			r.ChargedTokens = max(tokens, reservation)
			if complete && evalErr == nil {
				r.ChargedTokens = tokens
				r.Status = "rejected"
				r.Reason = "candidate did not pass the host suite"
				if e.Passed && tokens > p.MaxTotalTokens-charged {
					r.Reason = "evaluation exceeded the policy's remaining token allowance"
				} else if e.Passed {
					r.Status = "evaluated"
					r.Reason = ""
					if verifyErr := e.Verify(); verifyErr != nil {
						r.Status = "unknown"
						r.Reason = "evaluation integrity check failed"
						r.ChargedTokens = max(tokens, reservation)
					} else if p.AutoPromote {
						if promoteErr := store.Promote(ctx, p.Name, v.ID, e.ID); promoteErr != nil {
							r.Status = "promotion_failed"
							r.Reason = "active state changed or promotion could not be persisted"
						} else {
							r.Status = "promoted"
						}
					}
				}
			}
		}
		st.Receipts[len(st.Receipts)-1] = r
		result.Receipt = &r
		return result, c.save(st)
	}
	result.Idle = true
	return result, nil
}

func controllerEvaluationBound(e Evaluation, p ControllerPolicy, r ControllerReceipt) bool {
	id := e.ID
	e.ID = ""
	b, err := json.Marshal(e)
	if err != nil || Digest(b) != id {
		return false
	}
	profile := p.Identity.ProviderConfigSHA256
	if len(p.WorkflowFixture) > 0 {
		profile = EvaluationProfileDigest(profile, EvaluationFixtureDigest(p.WorkflowFixture))
	}
	return controllerDigestValid(id) && e.Kind == p.Kind && e.SkillName == p.Name && e.CandidateVersion == r.CandidateSHA256 && e.BaselineVersion == r.BaselineVersion && e.BaselineEpoch == r.ActiveEpoch && e.SuiteSHA256 == p.SuiteSHA256 && SuiteDigest(e.Suite) == p.SuiteSHA256 && e.BinarySHA256 == p.Identity.BinarySHA256 && e.ProfileSHA256 == profile && e.Provider == p.Identity.Provider && e.Model == p.Identity.Model
}
func controllerEvaluationTokens(e Evaluation) (int64, bool) {
	var total int64
	complete := true
	for _, version := range []VersionResult{e.Baseline, e.Candidate} {
		if len(version.Cases) != len(e.Suite.Cases) {
			complete = false
		}
		for _, result := range version.Cases {
			// A workflow with no active baseline does not execute a model query.
			// Its explicit empty baseline receipt carries no paid reservation to
			// settle; a baseline that did issue a query still needs real usage.
			if version.Version == "" && e.BaselineVersion == "" && result.ModelRequests == 0 && !result.UsageReported && result.Usage == (Usage{}) {
				continue
			}
			u := result.Usage
			if !result.UsageReported || u.Input < 0 || u.Output < 0 || u.CacheRead < 0 || u.CacheCreate < 0 || u == (Usage{}) {
				complete = false
				continue
			}
			for _, tokens := range []int{result.Usage.Input, result.Usage.Output, result.Usage.CacheRead, result.Usage.CacheCreate} {
				if int64(tokens) > math.MaxInt64-total {
					return math.MaxInt64, false
				}
				total += int64(tokens)
			}
		}
	}
	return total, complete
}

func controllerAddTokens(a, b int64) int64 {
	if b > math.MaxInt64-a {
		return math.MaxInt64
	}
	return a + b
}
