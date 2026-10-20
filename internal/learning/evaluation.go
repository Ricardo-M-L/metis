package learning

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"reflect"
	"strings"
	"time"
)

// Suite is supplied by the host, never by the candidate. Expected answers are
// kept outside the model prompt. Both versions see identical prompts and limits.
type Suite struct {
	Name               string  `json:"name"`
	Cases              []Case  `json:"cases"`
	MaxTokens          int     `json:"max_tokens"`
	MaxDurationSeconds int     `json:"max_duration_seconds"`
	MaxCostRatio       float64 `json:"max_cost_ratio,omitempty"` // optional token-cost regression ceiling
}
type Case struct {
	ID           string          `json:"id"`
	Prompt       string          `json:"prompt"`
	Expected     string          `json:"expected,omitempty"`
	ExpectedJSON json.RawMessage `json:"expected_json,omitempty"`
}
type Usage struct {
	Input       int `json:"input"`
	Output      int `json:"output"`
	CacheRead   int `json:"cache_read"`
	CacheCreate int `json:"cache_create"`
}

func (u Usage) Tokens() int { return u.Input + u.Output + u.CacheRead + u.CacheCreate }
func (u Usage) valid() bool {
	return u.Input >= 0 && u.Output >= 0 && u.CacheRead >= 0 && u.CacheCreate >= 0 && u.Tokens() > 0
}

type CaseResult struct {
	ID              string        `json:"id"`
	Output          string        `json:"output"`
	ExitCode        int           `json:"exit_code"`
	SkillInvoked    bool          `json:"skill_invoked"`
	WorkflowInvoked bool          `json:"workflow_invoked,omitempty"`
	ModelRequests   int           `json:"model_requests"`
	UsageReported   bool          `json:"usage_reported"`
	Usage           Usage         `json:"usage"`
	DurationMS      int64         `json:"duration_ms"`
	Passed          bool          `json:"passed"`
	Error           string        `json:"error,omitempty"`
	Evidence        QueryEvidence `json:"evidence"`
}

type SkillInvocation struct {
	InvocationID string          `json:"invocation_id"`
	CallID       string          `json:"call_id"`
	Arguments    json.RawMessage `json:"arguments"`
	Result       string          `json:"result"`
	ResultSHA256 string          `json:"result_sha256"`
	RecipeSHA256 string          `json:"recipe_sha256,omitempty"`
}
type QueryEvidence struct {
	Provider            string            `json:"provider"`
	Model               string            `json:"model"`
	SessionID           string            `json:"session_id"`
	ManifestSHA256      string            `json:"manifest_sha256,omitempty"`
	TraceSHA256         string            `json:"trace_sha256"`
	TokenEventIDs       []string          `json:"token_event_ids"`
	SkillInvocations    []SkillInvocation `json:"skill_invocations"`
	WorkflowInvocations []SkillInvocation `json:"workflow_invocations,omitempty"`
	FixtureSHA256       string            `json:"fixture_sha256,omitempty"`
	HostChecksSHA256    string            `json:"host_checks_sha256,omitempty"`
	HostChecksPassed    bool              `json:"host_checks_passed,omitempty"`
	HostChecksReport    json.RawMessage   `json:"host_checks_report,omitempty"`
}
type VersionResult struct {
	Version     string       `json:"version,omitempty"`
	Cases       []CaseResult `json:"cases"`
	PassedCases int          `json:"passed_cases"`
	Score       float64      `json:"score"`
	Tokens      int          `json:"tokens"`
	DurationMS  int64        `json:"duration_ms"`
}
type Evaluation struct {
	ID               string        `json:"id"`
	CandidateVersion string        `json:"candidate_version"`
	BaselineVersion  string        `json:"baseline_version,omitempty"`
	SkillName        string        `json:"skill_name"`
	Kind             string        `json:"kind"`
	BaselineEpoch    uint64        `json:"baseline_epoch"`
	InputsStable     bool          `json:"inputs_stable"`
	SuiteSHA256      string        `json:"suite_sha256"`
	BinarySHA256     string        `json:"binary_sha256"`
	ProfileSHA256    string        `json:"profile_sha256"`
	Provider         string        `json:"provider"`
	Model            string        `json:"model"`
	Suite            Suite         `json:"suite"`
	Baseline         VersionResult `json:"baseline"`
	Candidate        VersionResult `json:"candidate"`
	Passed           bool          `json:"passed"`
	Reason           string        `json:"reason,omitempty"`
	At               time.Time     `json:"at"`
}

// Runner must execute the version's frozen manifest in an isolated context.
// The CLI implementation uses an actual child metis run with only Skill tools.
type Runner func(context.Context, Version, Case) (CaseResult, error)
type EvaluatorIdentity struct {
	BinarySHA256, ProfileSHA256, Provider, Model string
	Kind                                         string
	ValidateInputs                               func() error
}

func (s Suite) Validate() error {
	if strings.TrimSpace(s.Name) == "" || len(s.Cases) == 0 || len(s.Cases) > 32 {
		return errors.New("suite requires a name and 1..32 cases")
	}
	if s.MaxTokens <= 0 || s.MaxTokens > 10000000 || s.MaxDurationSeconds <= 0 || s.MaxDurationSeconds > 3600 {
		return errors.New("suite requires bounded max_tokens and max_duration_seconds")
	}
	if math.IsNaN(s.MaxCostRatio) || math.IsInf(s.MaxCostRatio, 0) || s.MaxCostRatio < 0 {
		return errors.New("invalid max_cost_ratio")
	}
	seen := map[string]bool{}
	for _, c := range s.Cases {
		if c.ID == "" || seen[c.ID] || strings.TrimSpace(c.Prompt) == "" || len(c.Prompt) > 65536 {
			return errors.New("suite cases require unique IDs and bounded prompts")
		}
		seen[c.ID] = true
		if (c.Expected == "") == (len(c.ExpectedJSON) == 0) {
			return errors.New("case requires exactly one expected or expected_json host assertion")
		}
		if len(c.ExpectedJSON) > 0 {
			if _, err := jsonValue(c.ExpectedJSON); err != nil {
				return fmt.Errorf("case %s expected_json: %w", c.ID, err)
			}
		}
	}
	return nil
}
func SuiteDigest(s Suite) string { b, _ := json.Marshal(s); return Digest(b) }

// EvaluationProfileDigest keeps opt-in fixture authority distinct from a
// provider-only evaluator. An automatic controller freezes the same fixture
// bytes as a manual evaluation, so both derive identical evaluator identity.
func EvaluationProfileDigest(providerProfile, fixtureSHA256 string) string {
	if fixtureSHA256 == "" {
		return providerProfile
	}
	return Digest([]byte("workflow-code-fixture-v1\n" + providerProfile + "\n" + fixtureSHA256))
}

// EvaluationFixtureDigest canonicalizes JSON values so RawMessage indentation,
// object ordering and Go's HTML string escaping during policy persistence do
// not change a frozen fixture or host-check identity.
func EvaluationFixtureDigest(data []byte) string {
	if len(data) == 0 {
		return ""
	}
	value, err := jsonValue(data)
	if err != nil {
		return ""
	}
	canonical, err := json.Marshal(value)
	if err != nil {
		return ""
	}
	return Digest(canonical)
}
func jsonValue(b []byte) (any, error) {
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	var v any
	if err := d.Decode(&v); err != nil {
		return nil, err
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		return nil, errors.New("expected a single JSON value")
	}
	return v, nil
}

func check(c Case, r CaseResult) bool {
	if r.ExitCode != 0 || r.Error != "" || (!r.SkillInvoked && !r.WorkflowInvoked) || r.ModelRequests <= 0 || !r.UsageReported || !r.Usage.valid() || r.DurationMS < 0 {
		return false
	}
	if len(c.ExpectedJSON) > 0 {
		expected, err := jsonValue(c.ExpectedJSON)
		if err != nil {
			return false
		}
		actual, err := jsonValue([]byte(strings.TrimSpace(r.Output)))
		return err == nil && reflect.DeepEqual(expected, actual)
	}
	return strings.TrimSpace(r.Output) == strings.TrimSpace(c.Expected)
}

func validEvidence(r CaseResult, version, provider, model, name, kind string) bool {
	p := r.Evidence
	if p.FixtureSHA256 != "" && (kind != "workflow" || len(p.FixtureSHA256) != 64 || len(p.HostChecksSHA256) != 64 || !p.HostChecksPassed || len(p.HostChecksReport) == 0 || EvaluationFixtureDigest(p.HostChecksReport) != p.HostChecksSHA256) {
		return false
	}
	if p.FixtureSHA256 != "" {
		var report struct {
			FixtureSHA256   string            `json:"fixture_sha256"`
			TestsSHA256     string            `json:"tests_sha256"`
			TestsPassed     bool              `json:"tests_passed"`
			TestExitCode    *int              `json:"test_exit_code"`
			ArtifactsPassed bool              `json:"artifacts_passed"`
			SourceSHA256    map[string]string `json:"source_sha256"`
			ArtifactSHA256  map[string]string `json:"artifact_sha256"`
			Error           string            `json:"error"`
		}
		if json.Unmarshal(p.HostChecksReport, &report) != nil || report.FixtureSHA256 != p.FixtureSHA256 || len(report.TestsSHA256) != 64 || !report.TestsPassed || report.TestExitCode == nil || *report.TestExitCode != 0 || !report.ArtifactsPassed || report.Error != "" || len(report.SourceSHA256) == 0 || len(report.ArtifactSHA256) == 0 {
			return false
		}
		for _, digest := range report.SourceSHA256 {
			if len(digest) != 64 {
				return false
			}
		}
		for _, digest := range report.ArtifactSHA256 {
			if len(digest) != 64 {
				return false
			}
		}
	}
	calls := p.SkillInvocations
	if kind == "workflow" {
		calls = p.WorkflowInvocations
		if !r.WorkflowInvoked || len(calls) != 1 {
			return false
		}
	} else if !r.SkillInvoked {
		return false
	}
	if p.Provider != provider || p.Model != model || p.ManifestSHA256 != version || p.SessionID == "" || len(p.TraceSHA256) != 64 || len(p.TokenEventIDs) != r.ModelRequests || len(calls) == 0 {
		return false
	}
	seen := map[string]bool{}
	for _, id := range p.TokenEventIDs {
		if id == "" || seen[id] {
			return false
		}
		seen[id] = true
	}
	for _, call := range calls {
		if call.CallID == "" || call.InvocationID == "" || call.ResultSHA256 != Digest([]byte(call.Result)) {
			return false
		}
		var in struct{ Action, Name, Operation string }
		if json.Unmarshal(call.Arguments, &in) != nil || in.Name != name {
			return false
		}
		if kind == "workflow" {
			if in.Operation != "run_named" || call.RecipeSHA256 != version {
				return false
			}
			var result struct {
				Name         string `json:"name"`
				RecipeSHA256 string `json:"recipe_sha256"`
				Steps        []struct {
					Status   string `json:"status"`
					ExitCode *int   `json:"exit_code"`
					Output   string `json:"output"`
				} `json:"steps"`
			}
			if json.Unmarshal([]byte(call.Result), &result) != nil || result.Name != name || result.RecipeSHA256 != version || len(result.Steps) == 0 {
				return false
			}
			var actual strings.Builder
			for _, step := range result.Steps {
				if step.Status != "ok" || step.ExitCode == nil || *step.ExitCode != 0 {
					return false
				}
				actual.WriteString(step.Output)
			}
			if actual.String() != r.Output {
				return false
			}
		} else if in.Action != "invoke" {
			return false
		}
	}
	return true
}

func (s *Store) Evaluate(ctx context.Context, name, version string, suite Suite, identity EvaluatorIdentity, run Runner) (Evaluation, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := suite.Validate(); err != nil {
		return Evaluation{}, err
	}
	if run == nil || len(identity.BinarySHA256) != 64 || len(identity.ProfileSHA256) != 64 || identity.Provider == "" || identity.Model == "" {
		return Evaluation{}, errors.New("evaluation requires a pinned CLI, provider, model and runner")
	}
	if identity.Kind == "" {
		identity.Kind = "skill"
	}
	if identity.Kind != "skill" && identity.Kind != "workflow" {
		return Evaluation{}, errors.New("unsupported learning artifact kind")
	}
	st, err := s.Read(name)
	if err != nil {
		return Evaluation{}, err
	}
	candidate, ok := st.Versions[version]
	if !ok {
		return Evaluation{}, errors.New("candidate absent")
	}
	if candidate.Parent != st.ActiveVersion || candidate.ParentEpoch != st.ActivationEpoch {
		return Evaluation{}, errors.New("candidate baseline is stale")
	}
	e := Evaluation{ProfileSHA256: identity.ProfileSHA256, Kind: identity.Kind, SkillName: name, BaselineEpoch: st.ActivationEpoch, InputsStable: true, CandidateVersion: version, BaselineVersion: st.ActiveVersion, SuiteSHA256: SuiteDigest(suite), BinarySHA256: identity.BinarySHA256, Provider: identity.Provider, Model: identity.Model, Suite: suite, At: time.Now().UTC()}
	e.Baseline.Version = st.ActiveVersion
	e.Candidate.Version = version
	// Query accounting must outlive cancellation of the author/evaluation.
	// Seal and persist observed usage under a short, independent cleanup
	// context; callers receive it even when persistence itself is unavailable.
	interrupted := func(cause error) (Evaluation, error) {
		e.InputsStable = false
		e.Passed = false
		e.Reason = "evaluation interrupted: " + cause.Error()
		for _, result := range []*VersionResult{&e.Baseline, &e.Candidate} {
			result.Score = float64(result.PassedCases) / float64(len(suite.Cases))
		}
		e.ID = ""
		data, _ := json.Marshal(e)
		e.ID = Digest(data)
		settleCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		persistErr := s.mutate(settleCtx, name, func(current *State) error {
			if current.Versions[version] != candidate {
				return errors.New("candidate changed before partial evaluation settlement")
			}
			if len(current.Evaluations) >= 256 {
				return errors.New("evaluation history limit reached")
			}
			current.Evaluations = append(current.Evaluations, e)
			return nil
		})
		return e, errors.Join(cause, persistErr)
	}
	for _, pair := range []struct {
		v   Version
		out *VersionResult
	}{{st.Versions[st.ActiveVersion], &e.Baseline}, {candidate, &e.Candidate}} {
		pair.out.Version = pair.v.ID
		for _, c := range suite.Cases {
			if err := ctx.Err(); err != nil {
				return interrupted(err)
			}
			r, err := run(ctx, pair.v, c)
			r.ID = c.ID
			if err != nil {
				r.Error = err.Error()
			}
			r.Passed = check(c, r) && validEvidence(r, pair.v.ID, identity.Provider, identity.Model, name, identity.Kind)
			pair.out.Cases = append(pair.out.Cases, r)
			if r.Passed {
				pair.out.PassedCases++
			}
			pair.out.Tokens += r.Usage.Tokens()
			pair.out.DurationMS += r.DurationMS
			if err != nil {
				return interrupted(err)
			}
			if err := ctx.Err(); err != nil {
				return interrupted(err)
			}
		}
		pair.out.Score = float64(pair.out.PassedCases) / float64(len(suite.Cases))
	}
	if identity.ValidateInputs != nil && identity.ValidateInputs() != nil {
		e.InputsStable = false
	}
	if err := ctx.Err(); err != nil {
		return interrupted(err)
	}
	e.Passed, e.Reason = e.gate()
	b, _ := json.Marshal(e)
	e.ID = Digest(b)
	err = s.mutate(ctx, name, func(current *State) error {
		if current.ActiveVersion != e.BaselineVersion || current.ActivationEpoch != e.BaselineEpoch || current.Versions[version] != candidate {
			return errors.New("active or candidate changed during evaluation")
		}
		if len(current.Evaluations) >= 256 {
			return errors.New("evaluation history limit reached")
		}
		current.Evaluations = append(current.Evaluations, e)
		return nil
	})
	if err != nil && ctx.Err() != nil {
		return interrupted(ctx.Err())
	}
	return e, err
}
func (e Evaluation) gate() (bool, string) {
	if !e.InputsStable {
		return false, "host suite or evaluator changed during evaluation"
	}
	if e.BaselineVersion != "" {
		for _, r := range e.Baseline.Cases {
			if r.ExitCode != 0 || r.Error != "" || !r.UsageReported || !r.Usage.valid() || r.ModelRequests <= 0 {
				return false, "baseline query incomplete; comparison is unavailable"
			}
		}
	}
	if e.Candidate.PassedCases != len(e.Suite.Cases) {
		return false, "candidate failed host assertions or real-query evidence"
	}
	if e.Candidate.Score < e.Baseline.Score {
		return false, "quality regressed"
	}
	if e.Candidate.Tokens > e.Suite.MaxTokens {
		return false, "candidate exceeded token budget"
	}
	if e.Candidate.DurationMS > int64(e.Suite.MaxDurationSeconds)*1000 {
		return false, "candidate exceeded duration budget"
	}
	if e.Suite.MaxCostRatio > 0 && e.Baseline.Tokens > 0 && float64(e.Candidate.Tokens) > float64(e.Baseline.Tokens)*e.Suite.MaxCostRatio {
		return false, "token cost regressed beyond suite limit"
	}
	return true, ""
}
func (e Evaluation) Verify() error {
	if ValidateName(e.SkillName) != nil || (e.Kind != "skill" && e.Kind != "workflow") || len(e.BinarySHA256) != 64 || len(e.ProfileSHA256) != 64 || e.Provider == "" || e.Model == "" {
		return errors.New("evaluation identity mismatch")
	}
	if err := e.Suite.Validate(); err != nil {
		return err
	}
	if e.SuiteSHA256 != SuiteDigest(e.Suite) || e.Baseline.Version != e.BaselineVersion || e.Candidate.Version != e.CandidateVersion {
		return errors.New("evaluation suite or version binding mismatch")
	}
	for _, v := range []VersionResult{e.Baseline, e.Candidate} {
		if len(v.Cases) != len(e.Suite.Cases) {
			return errors.New("evaluation cases incomplete")
		}
		passed, tokens := 0, 0
		var duration int64
		for i, c := range e.Suite.Cases {
			r := v.Cases[i]
			if r.ID != c.ID || r.Passed != (check(c, r) && validEvidence(r, v.Version, e.Provider, e.Model, e.SkillName, e.Kind)) {
				return errors.New("evaluation assertion evidence mismatch")
			}
			if r.Passed {
				passed++
			}
			tokens += r.Usage.Tokens()
			duration += r.DurationMS
		}
		if v.PassedCases != passed || v.Score != float64(passed)/float64(len(v.Cases)) || v.Tokens != tokens || v.DurationMS != duration {
			return errors.New("evaluation summary mismatch")
		}
	}
	passed, reason := e.gate()
	if !passed || !e.Passed || e.Reason != reason {
		return errors.New("evaluation did not pass")
	}
	id := e.ID
	e.ID = ""
	b, _ := json.Marshal(e)
	if Digest(b) != id {
		return errors.New("evaluation digest mismatch")
	}
	return nil
}
