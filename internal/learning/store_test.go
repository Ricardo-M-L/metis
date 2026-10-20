package learning

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// The Runner below is a unit fixture for state/gate contracts only. Real CLI
// provider acceptance is performed separately by the integration harness.
func testSuite() Suite {
	return Suite{Name: "contract", Cases: []Case{{ID: "one", Prompt: "task", Expected: "GOOD"}}, MaxTokens: 1000, MaxDurationSeconds: 30}
}
func testIdentity() EvaluatorIdentity {
	return EvaluatorIdentity{BinarySHA256: strings.Repeat("b", 64), ProfileSHA256: strings.Repeat("p", 64), Provider: "fixture", Model: "unit-model"}
}
func testRunner(ctx context.Context, v Version, c Case) (CaseResult, error) {
	args, _ := json.Marshal(map[string]string{"action": "invoke", "name": "test-skill"})
	result := "Skill body: " + v.Content
	return CaseResult{Output: v.Content, ExitCode: 0, SkillInvoked: true, ModelRequests: 1, UsageReported: true, Usage: Usage{Input: 10, Output: 1}, DurationMS: 5, Evidence: QueryEvidence{Provider: "fixture", Model: "unit-model", SessionID: "fixture-session", ManifestSHA256: v.ID, TraceSHA256: strings.Repeat("a", 64), TokenEventIDs: []string{"token1"}, SkillInvocations: []SkillInvocation{{InvocationID: "invoke1", CallID: "call1", Arguments: args, Result: result, ResultSHA256: Digest([]byte(result))}}}}, nil
}
func stageTest(t *testing.T) (*Store, Version) {
	t.Helper()
	s := NewStore(t.TempDir())
	v, err := s.Stage(context.Background(), "test-skill", []byte("BASELINE"), []byte("GOOD"), "unit-proposal")
	if err != nil {
		t.Fatal(err)
	}
	return s, v
}
func evaluateTest(t *testing.T, s *Store, v Version) Evaluation {
	t.Helper()
	e, err := s.Evaluate(context.Background(), "test-skill", v.ID, testSuite(), testIdentity(), testRunner)
	if err != nil {
		t.Fatal(err)
	}
	return e
}
func TestLearningCandidateCannotActivateWithoutHostEvaluation(t *testing.T) {
	s, v := stageTest(t)
	st, err := s.Read("test-skill")
	if err != nil {
		t.Fatal(err)
	}
	if st.ActiveVersion != Digest([]byte("BASELINE")) {
		t.Fatalf("stage replaced active: %+v", st)
	}
	if err := s.Promote(context.Background(), "test-skill", v.ID, "self-authored-pass"); err == nil {
		t.Fatal("promotion accepted model claim")
	}
	if err := s.Rollback(context.Background(), "test-skill", v.ID); err == nil {
		t.Fatal("rollback accepted unvalidated candidate")
	}
}
func TestLearningPassingVersionPromotesAndRollbackRetainsEvidence(t *testing.T) {
	s, v := stageTest(t)
	e := evaluateTest(t, s, v)
	if !e.Passed || e.Candidate.Score != 1 || e.Baseline.Score != 0 || e.Candidate.Tokens != 11 {
		t.Fatalf("bad comparison %+v", e)
	}
	if err := e.Verify(); err != nil {
		t.Fatal(err)
	}
	if err := s.Promote(context.Background(), "test-skill", v.ID, e.ID); err != nil {
		t.Fatal(err)
	}
	st, _ := s.Read("test-skill")
	if st.ActiveVersion != v.ID {
		t.Fatal("not active")
	}
	if err := s.Rollback(context.Background(), "test-skill", st.BaselineVersion); err != nil {
		t.Fatal(err)
	}
	st, _ = s.Read("test-skill")
	if st.ActiveVersion != st.BaselineVersion || len(st.Evaluations) != 1 || len(st.Transitions) != 2 {
		t.Fatalf("rollback erased history %+v", st)
	}
	if err := s.Promote(context.Background(), "test-skill", v.ID, e.ID); err == nil {
		t.Fatal("accepted stale evidence after rollback ABA")
	}
	if err := s.Rollback(context.Background(), "test-skill", v.ID); err != nil {
		t.Fatal("previously promoted version should be recoverable", err)
	}
}
func TestLearningFailedCandidateDoesNotPolluteActive(t *testing.T) {
	s := NewStore(t.TempDir())
	v, err := s.Stage(context.Background(), "test-skill", []byte("BASELINE"), []byte("WRONG"), "unit-proposal")
	if err != nil {
		t.Fatal(err)
	}
	e := evaluateTest(t, s, v)
	if e.Passed {
		t.Fatal("bad output passed")
	}
	if err := s.Promote(context.Background(), "test-skill", v.ID, e.ID); err == nil {
		t.Fatal("bad candidate promoted")
	}
	st, _ := s.Read("test-skill")
	if st.ActiveVersion != st.BaselineVersion {
		t.Fatal("active polluted")
	}
	if len(st.Evaluations) != 1 || st.Evaluations[0].Candidate.Cases[0].Output != "WRONG" {
		t.Fatal("failed evidence lost")
	}
}
func TestLearningQualityCostAndEvidenceGates(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*CaseResult)
		suite  func(*Suite)
	}{
		{"no-usage", func(r *CaseResult) { r.UsageReported = false }, nil},
		{"negative-usage", func(r *CaseResult) { r.Usage.Input = -1 }, nil},
		{"no-invoke", func(r *CaseResult) { r.SkillInvoked = false }, nil},
		{"wrong-manifest", func(r *CaseResult) { r.Evidence.ManifestSHA256 = strings.Repeat("c", 64) }, nil},
		{"wrong-provider", func(r *CaseResult) { r.Evidence.Provider = "other" }, nil},
		{"wrong-tool", func(r *CaseResult) {
			r.Evidence.SkillInvocations[0].Arguments = json.RawMessage(`{"action":"get","name":"test-skill"}`)
		}, nil},
		{"altered-result", func(r *CaseResult) { r.Evidence.SkillInvocations[0].Result = "forged" }, nil},
		{"missing-request", func(r *CaseResult) { r.ModelRequests = 0 }, nil},
		{"exit-error", func(r *CaseResult) { r.ExitCode = 11 }, nil},
		{"budget", func(r *CaseResult) { r.Usage.Input = 1001 }, nil},
		{"duration", func(r *CaseResult) { r.DurationMS = 31000 }, nil},
		{"cost-ratio", func(r *CaseResult) { r.Usage.Input = 30 }, func(s *Suite) { s.MaxCostRatio = 1.1 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, v := stageTest(t)
			suite := testSuite()
			if tc.suite != nil {
				tc.suite(&suite)
			}
			e, err := s.Evaluate(context.Background(), "test-skill", v.ID, suite, testIdentity(), func(ctx context.Context, ver Version, c Case) (CaseResult, error) {
				r, err := testRunner(ctx, ver, c)
				if ver.ID == v.ID {
					tc.change(&r)
				}
				return r, err
			})
			if err != nil {
				t.Fatal(err)
			}
			if e.Passed {
				t.Fatal("invalid evidence passed")
			}
		})
	}
}
func TestLearningEvaluationRechecksStoredEvidence(t *testing.T) {
	s, v := stageTest(t)
	e := evaluateTest(t, s, v)
	e.Candidate.Cases[0].Output = "WRONG"
	if err := e.Verify(); err == nil {
		t.Fatal("tampered result accepted")
	}
	e = evaluateTest(t, s, v)
	e.Suite.Cases[0].Expected = "WRONG"
	if err := e.Verify(); err == nil {
		t.Fatal("suite drift accepted")
	}
}
func TestLearningHostSuiteDriftPersistsRejectedEvaluation(t *testing.T) {
	s, v := stageTest(t)
	id := testIdentity()
	id.ValidateInputs = func() error { return context.Canceled }
	e, err := s.Evaluate(context.Background(), "test-skill", v.ID, testSuite(), id, testRunner)
	if err != nil {
		t.Fatal(err)
	}
	if e.Passed || e.InputsStable {
		t.Fatal("suite changed but passed")
	}
	if err := s.Promote(context.Background(), "test-skill", v.ID, e.ID); err == nil {
		t.Fatal("changed suite promoted")
	}
}
func TestLearningCompetingPromotionHasOneWinner(t *testing.T) {
	s, v := stageTest(t)
	other, err := s.Stage(context.Background(), "test-skill", nil, []byte("OTHER"), "unit")
	if err != nil {
		t.Fatal(err)
	}
	e := evaluateTest(t, s, v)
	suite := testSuite()
	suite.Cases[0].Expected = "OTHER"
	e2, err := s.Evaluate(context.Background(), "test-skill", other.ID, suite, testIdentity(), testRunner)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for _, x := range []struct {
		v Version
		e Evaluation
	}{{v, e}, {other, e2}} {
		wg.Add(1)
		go func(v Version, e Evaluation) {
			defer wg.Done()
			results <- NewStore(filepath.Dir(s.dir)).Promote(context.Background(), "test-skill", v.ID, e.ID)
		}(x.v, x.e)
	}
	wg.Wait()
	close(results)
	success := 0
	for err := range results {
		if err == nil {
			success++
		}
	}
	if success != 1 {
		t.Fatalf("promotion winners %d", success)
	}
}
func TestLearningActivationChangedDuringEvaluationRefusesCommit(t *testing.T) {
	s, v := stageTest(t)
	e := evaluateTest(t, s, v)
	_, err := s.Evaluate(context.Background(), "test-skill", v.ID, testSuite(), testIdentity(), func(ctx context.Context, ver Version, c Case) (CaseResult, error) {
		if ver.ID == v.ID {
			if err := s.Promote(ctx, "test-skill", v.ID, e.ID); err != nil {
				t.Fatal(err)
			}
		}
		return testRunner(ctx, ver, c)
	})
	if err == nil {
		t.Fatal("baseline changed during evaluation but committed")
	}
}
func TestLearningStorageDigestAndPrivateMode(t *testing.T) {
	s, v := stageTest(t)
	info, err := os.Stat(s.path("test-skill"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("state mode %o", info.Mode().Perm())
	}
	data, _ := os.ReadFile(s.path("test-skill"))
	var st State
	if err := json.Unmarshal(data, &st); err != nil {
		t.Fatal(err)
	}
	x := st.Versions[v.ID]
	x.Content = "EVIL"
	st.Versions[v.ID] = x
	data, _ = json.Marshal(st)
	if err := os.WriteFile(s.path("test-skill"), data, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Read("test-skill"); err == nil {
		t.Fatal("modified content inherited proof")
	}
}
func TestLearningLockWaitHonorsCancellation(t *testing.T) {
	s, _ := stageTest(t)
	unlock, err := lock(context.Background(), filepath.Join(s.dir, "test-skill", ".lock"))
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	_, err = s.Stage(ctx, "test-skill", nil, []byte("ANOTHER"), "unit")
	if err != context.DeadlineExceeded {
		t.Fatalf("lock cancellation %v", err)
	}
}
func TestLearningNewSkillHasNoActiveVersionUntilPassing(t *testing.T) {
	s := NewStore(t.TempDir())
	v, err := s.Stage(context.Background(), "test-skill", nil, []byte("GOOD"), "unit")
	if err != nil {
		t.Fatal(err)
	}
	st, _ := s.Read("test-skill")
	if st.ActiveVersion != "" {
		t.Fatal("new skill active prematurely")
	}
	e := evaluateTest(t, s, v)
	if !e.Passed {
		t.Fatal(e.Reason)
	}
	if err := s.Promote(context.Background(), "test-skill", v.ID, e.ID); err != nil {
		t.Fatal(err)
	}
}
func TestLearningSuiteExactJSONContract(t *testing.T) {
	s, v := stageTest(t)
	suite := testSuite()
	suite.Cases[0].Expected = ""
	suite.Cases[0].ExpectedJSON = json.RawMessage(`{"answer":323}`)
	e, err := s.Evaluate(context.Background(), "test-skill", v.ID, suite, testIdentity(), func(ctx context.Context, ver Version, c Case) (CaseResult, error) {
		r, err := testRunner(ctx, ver, c)
		r.Output = "{\"answer\": 323}"
		return r, err
	})
	if err != nil || !e.Passed {
		t.Fatalf("JSON contract %v %+v", err, e)
	}
	suite.Cases[0].ExpectedJSON = json.RawMessage(`{"answer":323} true`)
	if suite.Validate() == nil {
		t.Fatal("multiple JSON values accepted")
	}
}

func TestLearningCancellationSealsAndPersistsObservedPartialUsage(t *testing.T) {
	s, v := stageTest(t)
	suite := testSuite()
	suite.Cases = append(suite.Cases, Case{ID: "two", Prompt: "more", Expected: "GOOD"})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	calls := 0
	e, err := s.Evaluate(ctx, "test-skill", v.ID, suite, testIdentity(), func(ctx context.Context, version Version, c Case) (CaseResult, error) {
		calls++
		r, err := testRunner(ctx, version, c)
		r.Usage.Input = 3000
		cancel()
		return r, err
	})
	if !errors.Is(err, context.Canceled) || calls != 1 || e.Passed || e.InputsStable || e.Baseline.Tokens != 3001 || e.Baseline.Version != e.BaselineVersion || e.Candidate.Version != v.ID {
		t.Fatalf("cancellation lost evidence: calls=%d error=%v eval=%+v", calls, err, e)
	}
	id := e.ID
	e.ID = ""
	data, _ := json.Marshal(e)
	if id == "" || id != Digest(data) {
		t.Fatal("partial evaluation not sealed")
	}
	e.ID = id
	st, readErr := s.Read("test-skill")
	if readErr != nil || len(st.Evaluations) != 1 || st.Evaluations[0].ID != id || st.Evaluations[0].Baseline.Cases[0].Usage.Input != 3000 {
		t.Fatalf("partial usage not durable: %+v %v", st, readErr)
	}
	if err := s.Promote(context.Background(), "test-skill", v.ID, id); err == nil {
		t.Fatal("canceled evaluation promoted")
	}
	if st.ActiveVersion != st.BaselineVersion {
		t.Fatal("cancel changed active")
	}
}
func TestLearningRunnerErrorPreservesReportedUsageAndStopsDispatch(t *testing.T) {
	s, v := stageTest(t)
	cause := errors.New("query interrupted after usage")
	calls := 0
	e, err := s.Evaluate(context.Background(), "test-skill", v.ID, testSuite(), testIdentity(), func(ctx context.Context, version Version, c Case) (CaseResult, error) {
		calls++
		r, _ := testRunner(ctx, version, c)
		r.Usage.Input = 3000
		return r, cause
	})
	if !errors.Is(err, cause) || calls != 1 || e.Passed || e.InputsStable || e.Baseline.Tokens != 3001 || e.ID == "" {
		t.Fatalf("runner failure lost usage: calls=%d error=%v eval=%+v", calls, err, e)
	}
	st, _ := s.Read("test-skill")
	if len(st.Evaluations) != 1 || st.Evaluations[0].Baseline.Cases[0].Error != cause.Error() {
		t.Fatal("runner error evidence lost")
	}
}
