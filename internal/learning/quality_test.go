package learning

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
)

func qualityFixture(t *testing.T, action string) (*Store, Version) {
	t.Helper()
	s, v := stageTest(t)
	e := evaluateTest(t, s, v)
	if err := s.Promote(context.Background(), "test-skill", v.ID, e.ID); err != nil {
		t.Fatal(err)
	}
	_, err := s.ConfigureQuality(context.Background(), "test-skill", QualityPolicy{Kind: "skill", WindowSize: 3, MinimumSamples: 3, MaxFailureRate: .4, Action: action, Verifier: "host-orders-v1"})
	if err != nil {
		t.Fatal(err)
	}
	return s, v
}
func qualityOutcome(passed bool, id string) HostOutcome {
	return HostOutcome{Passed: passed, Verifier: "host-orders-v1", InvocationSHA256: Digest([]byte("invocation:" + id)), EvidenceSHA256: Digest([]byte("host checker:" + id)), ReasonCode: "host_assertion"}
}
func qualityBind(t *testing.T, s *Store, id string) OutcomeBinding {
	t.Helper()
	b, err := s.BindOutcome(context.Background(), "test-skill", id)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
func qualityRecord(t *testing.T, s *Store, b OutcomeBinding, pass bool) QualityDecision {
	t.Helper()
	d, err := s.RecordOutcome(context.Background(), "test-skill", b, qualityOutcome(pass, b.OutcomeID))
	if err != nil {
		t.Fatal(err)
	}
	return d
}
func TestQualityHostBusinessFailuresRollbackExactDeployment(t *testing.T) {
	s, v := qualityFixture(t, "rollback")
	for i, p := range []bool{true, false, false} {
		b := qualityBind(t, s, fmt.Sprint(i))
		d := qualityRecord(t, s, b, p)
		if i < 2 && d.Action != "observed" {
			t.Fatalf("small sample changed activation: %+v", d)
		}
		if i == 2 && (d.Action != "rolled_back" || d.FromVersion != v.ID || d.Samples != 3 || d.Failures != 2) {
			t.Fatalf("no exact rollback: %+v", d)
		}
	}
	st, err := NewStore(strings.TrimSuffix(s.Dir(), "/.learning")).Read("test-skill")
	if err != nil || st.ActiveVersion != st.BaselineVersion || st.Suspended || !st.Quality.Paused || len(st.Transitions) != 2 || st.Transitions[1].FeedbackID == "" {
		t.Fatalf("state=%+v err=%v", st, err)
	}
	if err := s.Promote(context.Background(), "test-skill", v.ID, "arbitrary"); err == nil || !strings.Contains(err.Error(), "quality guard paused") {
		t.Fatalf("guard bypassed: %v", err)
	}
}
func TestQualityPassingSamplesDoNotTriggerAndDuplicateIsIdempotent(t *testing.T) {
	s, v := qualityFixture(t, "rollback")
	b := qualityBind(t, s, "business-one")
	d := qualityRecord(t, s, b, true)
	duplicate := qualityRecord(t, s, b, true)
	if duplicate != d {
		t.Fatal("duplicate changed original receipt")
	}
	if _, err := s.RecordOutcome(context.Background(), "test-skill", b, qualityOutcome(false, b.OutcomeID)); err == nil {
		t.Fatal("same business accepted conflicting outcome")
	}
	for _, id := range []string{"two", "three", "four"} {
		qualityRecord(t, s, qualityBind(t, s, id), true)
	}
	st, _ := s.Read("test-skill")
	if st.ActiveVersion != v.ID || st.Quality.Paused {
		t.Fatal("good deployment paused")
	}
}
func TestQualityRejectsCandidateBindingNoEvidenceAndWrongVerifier(t *testing.T) {
	s, _ := qualityFixture(t, "rollback")
	b := qualityBind(t, s, "checked")
	tampered := b
	tampered.VersionSHA256 = Digest([]byte("unactivated candidate"))
	if _, err := s.RecordOutcome(context.Background(), "test-skill", tampered, qualityOutcome(false, "checked")); err == nil {
		t.Fatal("candidate self attribution accepted")
	}
	for _, o := range []HostOutcome{{Passed: false}, {Passed: false, Verifier: "model-self-report", InvocationSHA256: b.VersionSHA256, EvidenceSHA256: b.VersionSHA256}} {
		if _, err := s.RecordOutcome(context.Background(), "test-skill", b, o); err == nil {
			t.Fatal("untrusted outcome accepted")
		}
	}
	st, _ := s.Read("test-skill")
	if st.Quality.Records[0].Outcome != nil {
		t.Fatal("invalid evidence counted")
	}
	if _, err := s.BindOutcomeForActivation(context.Background(), "test-skill", "other", Digest([]byte("candidate")), b.ActivationEpoch); err == nil {
		t.Fatal("CAS bound wrong manifest")
	}
}
func TestQualityLateOldEpochCannotRollbackNewActivationOrAfterResume(t *testing.T) {
	s, v := qualityFixture(t, "rollback")
	b := qualityBind(t, s, "late")
	st, _ := s.Read("test-skill")
	if err := s.Rollback(context.Background(), "test-skill", st.BaselineVersion); err != nil {
		t.Fatal(err)
	}
	if err := s.Rollback(context.Background(), "test-skill", v.ID); err != nil {
		t.Fatal(err)
	}
	d := qualityRecord(t, s, b, false)
	if d.Action != "ignored_stale" {
		t.Fatal("old epoch not ignored")
	}
	st, _ = s.Read("test-skill")
	if st.ActiveVersion != v.ID || st.Quality.Paused {
		t.Fatal("stale outcome affected new activation")
	}
}
func TestQualityPauseBlocksExecutionAndExplicitResumeAdvancesEpoch(t *testing.T) {
	s, _ := qualityFixture(t, "pause")
	old := qualityBind(t, s, "pending-old")
	for i := 0; i < 3; i++ {
		qualityRecord(t, s, qualityBind(t, s, fmt.Sprint(i)), false)
	}
	st, _ := s.Read("test-skill")
	if !st.Suspended || !st.Quality.Paused {
		t.Fatal("bad deployment not suspended")
	}
	if _, err := s.BindOutcome(context.Background(), "test-skill", "cannot-execute"); err == nil {
		t.Fatal("suspended deployment got task binding")
	}
	if err := s.ResumeQuality(context.Background(), "test-skill"); err != nil {
		t.Fatal(err)
	}
	d := qualityRecord(t, s, old, false)
	if d.Action != "ignored_stale" {
		t.Fatal("pre-resume task contaminated new window")
	}
	st, _ = s.Read("test-skill")
	if st.Suspended || st.Quality.Paused {
		t.Fatal("host resume not durable")
	}
}
func TestQualityCompletedFeedbackArchivesRetainingReplayAndCurrentWindow(t *testing.T) {
	s, _ := qualityFixture(t, "rollback")
	first := qualityBind(t, s, "first")
	original := qualityRecord(t, s, first, true)
	for i := 0; i < 520; i++ {
		b := qualityBind(t, s, fmt.Sprint(i))
		qualityRecord(t, s, b, true)
	}
	st, err := s.Read("test-skill")
	if err != nil || st.Quality.ArchivedRecords == 0 || len(st.Quality.Records) > 512 {
		t.Fatalf("not compacted: records=%d archived=%d err=%v", len(st.Quality.Records), st.Quality.ArchivedRecords, err)
	}
	if _, err := os.Stat(s.qualityArchivePath("test-skill", first.ID)); err != nil {
		t.Fatal(err)
	}
	replay := qualityRecord(t, s, first, true)
	if replay != original {
		t.Fatal("archive erased idempotency receipt")
	}
	rebound := qualityBind(t, s, "first")
	if rebound != first {
		t.Fatal("archive reset binding")
	}
	cold, err := s.LookupOutcome("test-skill", first.ID)
	if err != nil || cold.Outcome == nil || cold.Decision == nil || *cold.Decision != original {
		t.Fatalf("cold readonly lookup lost original verdict: %+v %v", cold, err)
	}
	cold.Decision.Action = "fabricated"
	corrupt, _ := json.Marshal(cold)
	if err := os.WriteFile(s.qualityArchivePath("test-skill", first.ID), corrupt, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.LookupOutcome("test-skill", first.ID); err == nil {
		t.Fatal("cold lookup accepted tampered decision metadata")
	}
}

func TestQualityLookupPendingAndMissingIsReadOnly(t *testing.T) {
	s, _ := qualityFixture(t, "rollback")
	b := qualityBind(t, s, "pending-lookup")
	before, err := os.ReadFile(s.path("test-skill"))
	if err != nil {
		t.Fatal(err)
	}
	r, err := s.LookupOutcome("test-skill", b.ID)
	if err != nil || r.Binding != b || r.Outcome != nil || r.Decision != nil {
		t.Fatalf("pending lookup=%+v err=%v", r, err)
	}
	if _, err := s.LookupOutcome("test-skill", Digest([]byte("missing binding"))); !os.IsNotExist(err) {
		t.Fatalf("missing lookup must report os.ErrNotExist: %v", err)
	}
	after, err := os.ReadFile(s.path("test-skill"))
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("readonly lookup changed activation authority", err)
	}
}

func TestQualityHostReviewCASRejectsStaleUIAndRecordsCausalNote(t *testing.T) {
	s, v := qualityFixture(t, "pause")
	st, _ := s.Read("test-skill")
	if err := s.RollbackCurrent(context.Background(), "test-skill", v.ID, st.ActivationEpoch+1, "reviewed"); err == nil {
		t.Fatal("stale host rollback accepted")
	}
	if err := s.RollbackCurrent(context.Background(), "test-skill", v.ID, st.ActivationEpoch, "operator checked failed deliverable"); err != nil {
		t.Fatal(err)
	}
	if err := s.ResumeQualityCurrent(context.Background(), "test-skill", v.ID, st.ActivationEpoch, "stale page"); err == nil {
		t.Fatal("stale resume accepted")
	}
	current, _ := s.Read("test-skill")
	if err := s.ResumeQualityCurrent(context.Background(), "test-skill", current.ActiveVersion, current.ActivationEpoch, "restored version reviewed"); err != nil {
		t.Fatal(err)
	}
	current, _ = s.Read("test-skill")
	if current.Quality.Paused || current.Transitions[len(current.Transitions)-1].Note != "restored version reviewed" {
		t.Fatal("host review audit absent")
	}
}

func TestControllerQualityGuardPausePreventsPaidEvaluation(t *testing.T) {
	c, s, _, _ := controllerFixture(t, true)
	_, err := s.ConfigureQuality(context.Background(), "test-skill", QualityPolicy{Kind: "skill", WindowSize: 1, MinimumSamples: 1, MaxFailureRate: 0, Action: "pause", Verifier: "host-orders-v1"})
	if err != nil {
		t.Fatal(err)
	}
	qualityRecord(t, s, qualityBind(t, s, "reject-baseline"), false)
	backend := controllerUnitBackend(s, testRunner)
	backend.Evaluate = func(context.Context, ControllerPolicy, string) (Evaluation, error) {
		t.Fatal("quality paused policy still spent query tokens")
		return Evaluation{}, nil
	}
	tick, err := c.RunOnce(context.Background(), backend)
	if err != nil || len(tick.QualityPaused) != 1 || !tick.Idle {
		t.Fatalf("guard status: %+v %v", tick, err)
	}
}

func TestQualityConcurrentDuplicateReceiptTriggersOnlyOneRollback(t *testing.T) {
	s, _ := qualityFixture(t, "rollback")
	p := QualityPolicy{Kind: "skill", WindowSize: 1, MinimumSamples: 1, MaxFailureRate: 0, Action: "rollback", Verifier: "host-orders-v1"}
	if _, err := s.ConfigureQuality(context.Background(), "test-skill", p); err != nil {
		t.Fatal(err)
	}
	b := qualityBind(t, s, "same-business")
	results := make(chan QualityDecision, 2)
	errs := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func() {
			d, err := s.RecordOutcome(context.Background(), "test-skill", b, qualityOutcome(false, b.OutcomeID))
			results <- d
			errs <- err
		}()
	}
	a, z := <-results, <-results
	if err := <-errs; err != nil {
		t.Fatal(err)
	}
	if err := <-errs; err != nil {
		t.Fatal(err)
	}
	if a != z || a.Action != "rolled_back" {
		t.Fatalf("duplicate CAS disagreed: %+v %+v", a, z)
	}
	st, _ := s.Read("test-skill")
	if len(st.Transitions) != 2 {
		t.Fatalf("more than one rollback committed: %+v", st.Transitions)
	}
}

func TestQualityRollbackWithoutEarlierDeploymentFallsBackToPause(t *testing.T) {
	s := NewStore(t.TempDir())
	v, err := s.Stage(context.Background(), "test-skill", nil, []byte("GOOD"), "new learned skill")
	if err != nil {
		t.Fatal(err)
	}
	e := evaluateTest(t, s, v)
	if err := s.Promote(context.Background(), "test-skill", v.ID, e.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ConfigureQuality(context.Background(), "test-skill", QualityPolicy{Kind: "skill", WindowSize: 1, MinimumSamples: 1, MaxFailureRate: 0, Action: "rollback", Verifier: "host-orders-v1"}); err != nil {
		t.Fatal(err)
	}
	d := qualityRecord(t, s, qualityBind(t, s, "failed-first-version"), false)
	st, _ := s.Read("test-skill")
	if d.Action != "paused" || !st.Suspended || st.ActiveVersion != v.ID {
		t.Fatalf("unsafe fallback without old deployment: %+v %+v", d, st)
	}
}
