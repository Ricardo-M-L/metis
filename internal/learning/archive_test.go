package learning

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
)

func TestLearningArchivePreservesCandidateIdentityAndHostRollbackProof(t *testing.T) {
	s, v := stageTest(t)
	old, err := s.Stage(context.Background(), "test-skill", nil, []byte("old proposal"), "unit")
	if err != nil {
		t.Fatal(err)
	}
	e := evaluateTest(t, s, v)
	if err := s.Promote(context.Background(), "test-skill", v.ID, e.ID); err != nil {
		t.Fatal(err)
	}
	a, err := s.Archive(context.Background(), "test-skill")
	if err != nil || a.ArchivedVersions != 1 || a.ArchivedEvaluations != 1 {
		t.Fatalf("archive=%+v err=%v", a, err)
	}
	if _, err := s.Stage(context.Background(), "test-skill", nil, []byte(old.Content), "replayed"); err == nil {
		t.Fatal("archived proposal returned as a new candidate")
	}
	if err := s.Rollback(context.Background(), "test-skill", old.ID); err == nil {
		t.Fatal("archived unactivated proposal became rollback target")
	}
	st, _ := s.Read("test-skill")
	if st.ActiveVersion != v.ID {
		t.Fatal("archive changed current deployment")
	}
	if err := s.Rollback(context.Background(), "test-skill", st.BaselineVersion); err != nil {
		t.Fatal(err)
	}
	// The prior promoted content is no longer the active version or its parent.
	if _, err := s.Archive(context.Background(), "test-skill"); err != nil {
		t.Fatal(err)
	}
	if err := s.Rollback(context.Background(), "test-skill", v.ID); err != nil {
		t.Fatal("lost archived activation proof", err)
	}
	st, _ = s.Read("test-skill")
	if st.ActiveVersion != v.ID {
		t.Fatal("historical validated activation not restored")
	}
}
func TestLearningStageAutomaticallyArchivesColdVersionsAtBound(t *testing.T) {
	s, v := stageTest(t)
	for i := 0; i < 125; i++ {
		if _, err := s.Stage(context.Background(), "test-skill", nil, []byte(fmt.Sprint("proposal-", i)), "unit"); err != nil {
			t.Fatal(err)
		}
	}
	e := evaluateTest(t, s, v)
	if err := s.Promote(context.Background(), "test-skill", v.ID, e.ID); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if _, err := s.Stage(context.Background(), "test-skill", nil, []byte(fmt.Sprint("next-", i)), "unit"); err != nil {
			t.Fatal(err)
		}
	}
	st, _ := s.Read("test-skill")
	if len(st.Versions) > 8 || st.ActiveVersion != v.ID {
		t.Fatalf("cold history stayed hot: versions=%d", len(st.Versions))
	}
}
func TestControllerArchiveRetainsSpentBudgetAndEvaluationDeduplication(t *testing.T) {
	for _, unknown := range []bool{false, true} {
		t.Run(fmt.Sprint(unknown), func(t *testing.T) {
			c, s, _, p := controllerFixture(t, false)
			p.MaxEvaluations = 1
			if _, err := c.Configure(context.Background(), p.ControllerPolicySpec); err != nil {
				t.Fatal(err)
			}
			backend := controllerUnitBackend(s, testRunner)
			if unknown {
				backend.Evaluate = func(context.Context, ControllerPolicy, string) (Evaluation, error) {
					return Evaluation{}, errors.New("lost response")
				}
			}
			tick, err := c.RunOnce(context.Background(), backend)
			if err != nil || tick.Receipt == nil {
				t.Fatalf("%+v %v", tick, err)
			}
			a, err := c.ArchiveReceipts(context.Background())
			if err != nil || a.Archived != 1 || a.Retained != 0 {
				t.Fatalf("%+v %v", a, err)
			}
			receipt, err := c.readArchivedReceipt(tick.Receipt.ID)
			if err != nil || receipt.ID != tick.Receipt.ID {
				t.Fatal("receipt lost", err)
			}
			backend.Evaluate = func(context.Context, ControllerPolicy, string) (Evaluation, error) {
				t.Fatal("archive refunded or repeated query")
				return Evaluation{}, nil
			}
			again, err := NewController(c.Dir()).RunOnce(context.Background(), backend)
			if err != nil || len(again.BudgetExhausted) != 1 {
				t.Fatalf("archive reset budget: %+v %v", again, err)
			}
			st, _ := c.Read()
			if st.ArchivedBudget["skill/test-skill"].ChargedTokens != tick.Receipt.ChargedTokens {
				t.Fatal("charge not retained")
			}
		})
	}
}
func TestLearningArchiveFailureDoesNotDropHotEvidence(t *testing.T) {
	s, v := stageTest(t)
	e := evaluateTest(t, s, v)
	if err := s.Promote(context.Background(), "test-skill", v.ID, e.ID); err != nil {
		t.Fatal(err)
	}
	blocked := s.Dir() + "/test-skill/archive"
	if err := os.WriteFile(blocked, []byte("prevent directory creation"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Archive(context.Background(), "test-skill"); err == nil {
		t.Fatal("archive IO failure hidden")
	}
	st, err := s.Read("test-skill")
	if err != nil || len(st.Evaluations) != 1 || st.ActiveVersion != v.ID {
		t.Fatal("hot evidence lost on archive failure", err)
	}
}

func TestControllerArchivedCandidateDoesNotRunAgainWithRemainingBudget(t *testing.T) {
	c, s, _, _ := controllerFixture(t, false)
	backend := controllerUnitBackend(s, testRunner)
	tick, err := c.RunOnce(context.Background(), backend)
	if err != nil || tick.Receipt == nil {
		t.Fatalf("%+v %v", tick, err)
	}
	if _, err := c.ArchiveReceipts(context.Background()); err != nil {
		t.Fatal(err)
	}
	backend.Evaluate = func(context.Context, ControllerPolicy, string) (Evaluation, error) {
		t.Fatal("archived candidate repeated")
		return Evaluation{}, nil
	}
	again, err := NewController(c.Dir()).RunOnce(context.Background(), backend)
	if err != nil || !again.Idle || again.Receipt != nil || len(again.BudgetExhausted) != 0 {
		t.Fatalf("%+v %v", again, err)
	}
}

func TestLearningArchivedPassingEvaluationRemainsPromotableForSameActivation(t *testing.T) {
	s, v := stageTest(t)
	older := evaluateTest(t, s, v)
	latest := evaluateTest(t, s, v)
	if older.ID == latest.ID {
		t.Fatal("fixture did not produce distinct host evidence")
	}
	a, err := s.Archive(context.Background(), "test-skill")
	if err != nil || a.ArchivedEvaluations != 1 {
		t.Fatalf("%+v %v", a, err)
	}
	evidence, err := s.Evaluation("test-skill", older.ID)
	if err != nil || evidence.ID != older.ID {
		t.Fatal("cold host evidence inaccessible", err)
	}
	if err := s.Promote(context.Background(), "test-skill", v.ID, older.ID); err != nil {
		t.Fatal("valid archived host evidence rejected", err)
	}
	st, _ := s.Read("test-skill")
	if st.ActiveVersion != v.ID {
		t.Fatal("archived promotion failed")
	}
}
