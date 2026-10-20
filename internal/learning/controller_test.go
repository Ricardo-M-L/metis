package learning

import (
	"context"
	"errors"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func controllerFixture(t *testing.T, auto bool) (*Controller, *Store, Version, ControllerPolicy) {
	t.Helper()
	s, v := stageTest(t)
	c := NewController(t.TempDir())
	p, err := c.Configure(context.Background(), ControllerPolicySpec{Kind: "skill", Name: "test-skill", ArtifactRoot: filepath.Dir(s.Dir()), Suite: testSuite(), Identity: ControllerIdentity{BinaryPath: "/unit/metis", BinarySHA256: strings.Repeat("b", 64), ProviderConfigSHA256: strings.Repeat("c", 64), Provider: "fixture", Model: "unit-model"}, AutoPromote: auto, MaxEvaluations: 3, MaxTotalTokens: 6000})
	if err != nil {
		t.Fatal(err)
	}
	return c, s, v, p
}
func controllerUnitBackend(s *Store, runner Runner) ControllerBackend {
	return ControllerBackend{
		Store: func(ControllerPolicy) (*Store, error) { return s, nil },
		Evaluate: func(ctx context.Context, p ControllerPolicy, version string) (Evaluation, error) {
			return s.Evaluate(ctx, p.Name, version, p.Suite, EvaluatorIdentity{Kind: p.Kind, BinarySHA256: p.Identity.BinarySHA256, ProfileSHA256: p.Identity.ProviderConfigSHA256, Provider: p.Identity.Provider, Model: p.Identity.Model}, runner)
		},
	}
}
func TestControllerNoPolicyDoesNotInventSuiteOrPay(t *testing.T) {
	c := NewController(t.TempDir())
	backend := ControllerBackend{Store: func(ControllerPolicy) (*Store, error) { t.Fatal("unconfigured store opened"); return nil, nil }, Evaluate: func(context.Context, ControllerPolicy, string) (Evaluation, error) {
		t.Fatal("unconfigured paid query")
		return Evaluation{}, nil
	}}
	r, err := c.RunOnce(context.Background(), backend)
	if err != nil || !r.Idle || r.Receipt != nil {
		t.Fatalf("result=%+v err=%v", r, err)
	}
}
func TestControllerEvaluatesAndPromotesExactCandidateOnce(t *testing.T) {
	c, s, v, _ := controllerFixture(t, true)
	var calls atomic.Int32
	backend := controllerUnitBackend(s, func(ctx context.Context, v Version, tc Case) (CaseResult, error) {
		calls.Add(1)
		return testRunner(ctx, v, tc)
	})
	r, err := c.RunOnce(context.Background(), backend)
	if err != nil || r.Receipt == nil || r.Receipt.Status != "promoted" || r.Receipt.ChargedTokens != 22 {
		t.Fatalf("receipt=%+v err=%v", r, err)
	}
	st, _ := s.Read("test-skill")
	if st.ActiveVersion != v.ID {
		t.Fatal("passing candidate not active")
	}
	r, err = NewController(c.Dir()).RunOnce(context.Background(), backend)
	if err != nil || !r.Idle || calls.Load() != 2 {
		t.Fatalf("restart retried paid query: %+v calls=%d err=%v", r, calls.Load(), err)
	}
}
func TestControllerDefaultPolicyDoesNotAutoPromote(t *testing.T) {
	c, s, _, _ := controllerFixture(t, false)
	r, err := c.RunOnce(context.Background(), controllerUnitBackend(s, testRunner))
	if err != nil || r.Receipt.Status != "evaluated" {
		t.Fatalf("%+v %v", r, err)
	}
	st, _ := s.Read("test-skill")
	if st.ActiveVersion != st.BaselineVersion {
		t.Fatal("host did not authorize automatic promotion")
	}
}
func TestControllerRejectedCandidateKeepsActiveAndDoesNotRetry(t *testing.T) {
	c, s, _, _ := controllerFixture(t, true)
	backend := controllerUnitBackend(s, func(ctx context.Context, v Version, tc Case) (CaseResult, error) {
		r, err := testRunner(ctx, v, tc)
		r.Output = "WRONG"
		return r, err
	})
	r, err := c.RunOnce(context.Background(), backend)
	if err != nil || r.Receipt.Status != "rejected" || r.Receipt.ChargedTokens != 22 {
		t.Fatalf("%+v %v", r, err)
	}
	st, _ := s.Read("test-skill")
	if st.ActiveVersion != st.BaselineVersion {
		t.Fatal("rejected content activated")
	}
	r, err = c.RunOnce(context.Background(), backend)
	if err != nil || !r.Idle {
		t.Fatalf("rejected candidate retried: %+v %v", r, err)
	}
}
func TestControllerUnknownRetainsReservationAndNeverRetries(t *testing.T) {
	c, s, _, _ := controllerFixture(t, true)
	backend := controllerUnitBackend(s, testRunner)
	backend.Evaluate = func(context.Context, ControllerPolicy, string) (Evaluation, error) {
		return Evaluation{}, errors.New("upstream outcome unknown")
	}
	r, err := c.RunOnce(context.Background(), backend)
	if err != nil || r.Receipt.Status != "unknown" || r.Receipt.ChargedTokens != 2000 {
		t.Fatalf("%+v %v", r, err)
	}
	backend.Evaluate = func(context.Context, ControllerPolicy, string) (Evaluation, error) {
		t.Fatal("unknown paid query retried")
		return Evaluation{}, nil
	}
	r, err = NewController(c.Dir()).RunOnce(context.Background(), backend)
	if err != nil || !r.Idle {
		t.Fatalf("%+v %v", r, err)
	}
}
func TestControllerInterruptedReservationSurvivesCrash(t *testing.T) {
	c, s, _, _ := controllerFixture(t, true)
	backend := controllerUnitBackend(s, testRunner)
	backend.Evaluate = func(context.Context, ControllerPolicy, string) (Evaluation, error) { panic("unit simulated crash") }
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("expected panic")
			}
		}()
		_, _ = c.RunOnce(context.Background(), backend)
	}()
	st, err := c.Read()
	if err != nil || len(st.Receipts) != 1 || st.Receipts[0].Status != "reserved" || st.Receipts[0].ChargedTokens != 2000 {
		t.Fatalf("missing write-ahead reservation: %+v %v", st, err)
	}
	backend.Evaluate = func(context.Context, ControllerPolicy, string) (Evaluation, error) {
		t.Fatal("interrupted query retried")
		return Evaluation{}, nil
	}
	if _, err := c.RunOnce(context.Background(), backend); err != nil {
		t.Fatal(err)
	}
	st, _ = c.Read()
	if st.Receipts[0].Status != "unknown" || st.Receipts[0].ChargedTokens != 2000 {
		t.Fatalf("crash refunded: %+v", st.Receipts[0])
	}
}
func TestControllerSingleLeaseBlocksConcurrentPaidAttempt(t *testing.T) {
	c, s, _, _ := controllerFixture(t, true)
	started, release := make(chan struct{}), make(chan struct{})
	backend := controllerUnitBackend(s, testRunner)
	base := backend.Evaluate
	backend.Evaluate = func(ctx context.Context, p ControllerPolicy, id string) (Evaluation, error) {
		close(started)
		<-release
		return base(ctx, p, id)
	}
	done := make(chan error, 1)
	go func() { _, err := c.RunOnce(context.Background(), backend); done <- err }()
	<-started
	st, err := c.Read()
	if err != nil || st.Lease == nil || st.Lease.PID != os.Getpid() {
		t.Fatalf("lease absent: %+v %v", st, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	_, err = NewController(c.Dir()).RunOnce(ctx, backend)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("concurrent controller ran: %v", err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
func TestControllerIdenticalConfigureCannotResetBudget(t *testing.T) {
	c, s, _, p := controllerFixture(t, true)
	p.MaxEvaluations = 1
	p, err := c.Configure(context.Background(), p.ControllerPolicySpec)
	if err != nil {
		t.Fatal(err)
	}
	backend := controllerUnitBackend(s, testRunner)
	backend.Evaluate = func(context.Context, ControllerPolicy, string) (Evaluation, error) {
		return Evaluation{}, errors.New("unknown")
	}
	if _, err := c.RunOnce(context.Background(), backend); err != nil {
		t.Fatal(err)
	}
	again, err := c.Configure(context.Background(), p.ControllerPolicySpec)
	if err != nil || again.Revision != p.Revision {
		t.Fatalf("identical configure reset revision: %+v %v", again, err)
	}
	r, err := c.RunOnce(context.Background(), backend)
	if err != nil || len(r.BudgetExhausted) != 1 || r.Receipt != nil {
		t.Fatalf("spent policy budget reset: %+v %v", r, err)
	}
	p.Suite.Cases[0].Expected = "DIFFERENT"
	changed, err := c.Configure(context.Background(), p.ControllerPolicySpec)
	if err != nil || changed.Revision != p.Revision+1 || changed.SuiteSHA256 == p.SuiteSHA256 {
		t.Fatalf("suite revision not frozen: %+v %v", changed, err)
	}
}
func TestControllerOverRemainingReportedBudgetRefusesPromotion(t *testing.T) {
	c, s, _, p := controllerFixture(t, true)
	p.MaxTotalTokens = 2000
	if _, err := c.Configure(context.Background(), p.ControllerPolicySpec); err != nil {
		t.Fatal(err)
	}
	backend := controllerUnitBackend(s, func(ctx context.Context, v Version, tc Case) (CaseResult, error) {
		r, err := testRunner(ctx, v, tc)
		if v.Content == "BASELINE" {
			r.Usage.Input = 2500
		}
		return r, err
	})
	r, err := c.RunOnce(context.Background(), backend)
	if err != nil || r.Receipt.Status != "rejected" || r.Receipt.ChargedTokens != 2512 {
		t.Fatalf("%+v %v", r, err)
	}
	st, _ := s.Read("test-skill")
	if st.ActiveVersion != st.BaselineVersion {
		t.Fatal("over-budget candidate promoted")
	}
	r, err = c.RunOnce(context.Background(), backend)
	if err != nil || len(r.BudgetExhausted) != 1 {
		t.Fatalf("over-budget policy paid again: %+v %v", r, err)
	}
}
func TestControllerConfigOrResultTamperingRetainsReservation(t *testing.T) {
	c, s, _, _ := controllerFixture(t, true)
	backend := controllerUnitBackend(s, testRunner)
	base := backend.Evaluate
	backend.Evaluate = func(ctx context.Context, p ControllerPolicy, id string) (Evaluation, error) {
		e, err := base(ctx, p, id)
		e.ProfileSHA256 = strings.Repeat("d", 64)
		return e, err
	}
	r, err := c.RunOnce(context.Background(), backend)
	if err != nil || r.Receipt.Status != "unknown" || r.Receipt.ChargedTokens != 2000 {
		t.Fatalf("unbound evaluator accepted: %+v %v", r, err)
	}
	st, _ := c.Read()
	p := st.Policies["skill/test-skill"]
	p.Suite.Cases[0].Expected = "TAMPER"
	st.Policies["skill/test-skill"] = p
	if err := c.save(st); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Read(); err == nil {
		t.Fatal("changed checker reused frozen policy hash")
	}
}

func TestControllerPartialUsageDoesNotRefundKnownExcess(t *testing.T) {
	c, s, _, _ := controllerFixture(t, true)
	backend := controllerUnitBackend(s, func(ctx context.Context, v Version, tc Case) (CaseResult, error) {
		r, err := testRunner(ctx, v, tc)
		if v.Content == "BASELINE" {
			r.Usage.Input = 5000
		} else {
			r.UsageReported = false
		}
		return r, err
	})
	r, err := c.RunOnce(context.Background(), backend)
	if err != nil || r.Receipt.Status != "unknown" || r.Receipt.ChargedTokens != 5001 || r.Receipt.ReportedTokens != 5001 {
		t.Fatalf("known consumption refunded: %+v %v", r, err)
	}
}
func TestControllerLatePersistenceErrorStillRetainsKnownConsumption(t *testing.T) {
	c, s, _, _ := controllerFixture(t, true)
	backend := controllerUnitBackend(s, func(ctx context.Context, v Version, tc Case) (CaseResult, error) {
		r, err := testRunner(ctx, v, tc)
		r.Usage.Input = 1500
		return r, err
	})
	base := backend.Evaluate
	backend.Evaluate = func(ctx context.Context, p ControllerPolicy, version string) (Evaluation, error) {
		e, err := base(ctx, p, version)
		return e, errors.Join(err, errors.New("unit late persistence failure"))
	}
	r, err := c.RunOnce(context.Background(), backend)
	if err != nil || r.Receipt.Status != "unknown" || r.Receipt.ReportedTokens != 3002 || r.Receipt.ChargedTokens != 3002 {
		t.Fatalf("late error refunded actual consumption: %+v %v", r, err)
	}
}
func TestControllerSelectedPolicyCannotEvaluateAnotherArtifact(t *testing.T) {
	c, s, _, p := controllerFixture(t, false)
	p.Kind, p.Name = "workflow", "other-workflow"
	if _, err := c.Configure(context.Background(), p.ControllerPolicySpec); err != nil {
		t.Fatal(err)
	}
	backend := controllerUnitBackend(s, testRunner)
	base := backend.Store
	backend.Store = func(p ControllerPolicy) (*Store, error) {
		if p.Kind != "skill" || p.Name != "test-skill" {
			t.Fatal("selector opened unrelated artifact")
		}
		return base(p)
	}
	r, err := c.RunPolicyOnce(context.Background(), backend, "skill", "test-skill")
	if err != nil || r.Receipt == nil || r.Receipt.PolicyKey != "skill/test-skill" {
		t.Fatalf("%+v %v", r, err)
	}
	if _, err := c.RunPolicyOnce(context.Background(), backend, "workflow", "absent"); err == nil {
		t.Fatal("unconfigured selector accepted")
	}
}
func TestControllerStaleCandidateAfterRollbackCannotReuseAttempt(t *testing.T) {
	c, s, v, _ := controllerFixture(t, true)
	backend := controllerUnitBackend(s, testRunner)
	if _, err := c.RunOnce(context.Background(), backend); err != nil {
		t.Fatal(err)
	}
	st, err := s.Read("test-skill")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Rollback(context.Background(), "test-skill", st.BaselineVersion); err != nil {
		t.Fatal(err)
	}
	backend.Evaluate = func(context.Context, ControllerPolicy, string) (Evaluation, error) {
		t.Fatal("stale candidate after rollback paid again")
		return Evaluation{}, nil
	}
	r, err := c.RunOnce(context.Background(), backend)
	if err != nil || !r.Idle {
		t.Fatalf("stale candidate re-evaluated: %+v %v", r, err)
	}
	st, _ = s.Read("test-skill")
	if st.ActivationEpoch == v.ParentEpoch {
		t.Fatal("fixture failed to change activation epoch")
	}
}
func TestControllerEmptyBaselineWithNoQueryDoesNotInventUsage(t *testing.T) {
	e := Evaluation{BaselineVersion: "", Suite: testSuite(), Baseline: VersionResult{Version: "", Cases: []CaseResult{{Error: "no active workflow baseline", ExitCode: -1}}}, Candidate: VersionResult{Version: Digest([]byte("candidate")), Cases: []CaseResult{{UsageReported: true, Usage: Usage{Input: 10, Output: 2}, ModelRequests: 1}}}}
	tokens, complete := controllerEvaluationTokens(e)
	if !complete || tokens != 12 {
		t.Fatalf("empty baseline invented usage: %d complete=%v", tokens, complete)
	}
	e.Baseline.Cases[0].ModelRequests = 1
	if _, complete := controllerEvaluationTokens(e); complete {
		t.Fatal("unreported paid baseline treated as absent")
	}
}

func TestControllerCancellationRetainsPreviouslyReportedExcess(t *testing.T) {
	c, s, _, p := controllerFixture(t, true)
	p.Suite.Cases = append(p.Suite.Cases, Case{ID: "two", Prompt: "second paid case", Expected: "GOOD"})
	if _, err := c.Configure(context.Background(), p.ControllerPolicySpec); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var calls int
	backend := controllerUnitBackend(s, func(ctx context.Context, v Version, tc Case) (CaseResult, error) {
		calls++
		r, err := testRunner(ctx, v, tc)
		r.Usage.Input = 2500
		cancel()
		return r, err
	})
	r, err := c.RunOnce(ctx, backend)
	if err != nil || r.Receipt == nil || r.Receipt.Status != "unknown" || r.Receipt.ReportedTokens != 2501 || r.Receipt.ChargedTokens != 2501 || calls != 1 {
		t.Fatalf("cancel lost reported consumption: receipt=%+v calls=%d err=%v", r.Receipt, calls, err)
	}
}

func TestControllerIdleWatchDoesNotRewriteLargeFrozenState(t *testing.T) {
	c, s, _, _ := controllerFixture(t, false)
	backend := controllerUnitBackend(s, testRunner)
	if _, err := c.RunOnce(context.Background(), backend); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(c.Dir(), "state.json")
	frozenAt := time.Unix(1700000000, 0)
	if err := os.Chtimes(path, frozenAt, frozenAt); err != nil {
		t.Fatal(err)
	}
	r, err := c.RunOnce(context.Background(), backend)
	if err != nil || !r.Idle {
		t.Fatalf("idle poll: %+v %v", r, err)
	}
	info, err := os.Stat(path)
	if err != nil || !info.ModTime().Equal(frozenAt) {
		t.Fatalf("idle watcher rewrote checker/history: %+v %v", info, err)
	}
}

func TestControllerLargeReportedConsumptionIsNeverDroppedOrWrapped(t *testing.T) {
	e := Evaluation{Suite: testSuite(), Baseline: VersionResult{Cases: []CaseResult{{ExitCode: -1}}}, Candidate: VersionResult{Version: Digest([]byte("candidate")), Cases: []CaseResult{{UsageReported: true, Usage: Usage{Input: 2000000000, Output: 1}}}}}
	tokens, complete := controllerEvaluationTokens(e)
	if !complete || tokens != 2000000001 {
		t.Fatalf("large known usage dropped: %d %v", tokens, complete)
	}
	if got := controllerAddTokens(math.MaxInt64, 1); got != math.MaxInt64 {
		t.Fatalf("charged allowance wrapped: %d", got)
	}
}
