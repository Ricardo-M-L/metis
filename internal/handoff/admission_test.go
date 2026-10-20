package handoff

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestAdmissionWaitingSurvivesRestartAndThrottlesUnchangedChecks(t *testing.T) {
	s, rec, update := businessFixture(t)
	now := time.Date(2026, 10, 16, 1, 2, 3, 0, time.UTC)
	s.now = func() time.Time { return now }
	first, err := s.SetAdmissionWaiting(context.Background(), rec.ID, update.Assignment, WaitingJobPaused)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(s.Root(), "messages", rec.ID+".json")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(AdmissionCheckInterval / 2)
	second, err := s.SetAdmissionWaiting(context.Background(), rec.ID, update.Assignment, WaitingJobPaused)
	if err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) || second.Admission.LastCheckedAt != first.Admission.LastCheckedAt {
		t.Fatal("unchanged polling check rewrote durable record")
	}
	if second.Attempts != 0 || second.Business != nil || second.State != "pending" {
		t.Fatalf("admission observation claimed or reserved work: %+v", second)
	}
	restarted, err := NewStore(s.Root())
	if err != nil {
		t.Fatal(err)
	}
	restarted.now = func() time.Time { return now }
	loaded, err := restarted.Get(context.Background(), rec.ID)
	if err != nil || loaded.Admission == nil || loaded.Admission.WaitingReason != WaitingJobPaused || loaded.Admission.RecoveryHint == "" {
		t.Fatalf("restart lost diagnostics: %+v err=%v", loaded.Admission, err)
	}
	now = now.Add(AdmissionCheckInterval / 2)
	checked, err := restarted.SetAdmissionWaiting(context.Background(), rec.ID, update.Assignment, WaitingJobPaused)
	if err != nil || !checked.Admission.LastCheckedAt.Equal(now) {
		t.Fatalf("heartbeat not persisted after interval: %+v err=%v", checked.Admission, err)
	}
	now = now.Add(time.Second)
	changed, err := restarted.SetAdmissionWaiting(context.Background(), rec.ID, update.Assignment, WaitingUsageUnresolved)
	if err != nil || !changed.Admission.LastCheckedAt.Equal(now) || changed.Admission.WaitingReason != WaitingUsageUnresolved {
		t.Fatalf("changed reason was throttled: %+v err=%v", changed.Admission, err)
	}
	if err = restarted.ClearAdmissionWaiting(context.Background(), rec.ID, update.Assignment); !errors.Is(err, ErrBusinessConflict) {
		t.Fatalf("unreserved message cleared as admitted: %v", err)
	}
	if _, err = restarted.ReconcileBusiness(context.Background(), rec.ID, update); err != nil {
		t.Fatal(err)
	}
	claim, err := restarted.ClaimID(context.Background(), rec.ID, rec.Scope, rec.To, "receiver", time.Minute, update.Assignment)
	if err != nil || claim == nil || claim.Admission != nil || claim.Attempts != 1 {
		t.Fatalf("new claim retained stale waiting: %+v err=%v", claim, err)
	}
}

func TestAdmissionWaitingProtectsGraphReceiptAndPrivateDiagnostics(t *testing.T) {
	s, rec, update := businessFixture(t)
	if _, err := s.SetAdmissionWaiting(context.Background(), rec.ID, update.Assignment, "API_KEY=private-diagnostic"); err == nil {
		t.Fatal("arbitrary error text accepted as public reason")
	}
	wrong := update.Assignment
	wrong.JobID = "not-authorized-mailbox"
	if _, err := s.SetAdmissionWaiting(context.Background(), rec.ID, wrong, WaitingJobBusy); !errors.Is(err, ErrBusinessConflict) {
		t.Fatalf("other mailbox assignment accepted: %v", err)
	}
	waiting, err := s.SetAdmissionWaiting(context.Background(), rec.ID, update.Assignment, WaitingRouteUnavailable)
	if err != nil {
		t.Fatal(err)
	}
	view, err := json.Marshal(waiting.Admission.SafeStatus())
	if err != nil {
		t.Fatal(err)
	}
	for _, private := range []string{update.Assignment.Workspace, "assignment", "config_sha256", "API_KEY"} {
		if strings.Contains(string(view), private) {
			t.Fatalf("public waiting view leaked %q: %s", private, view)
		}
	}
	if _, err = s.ReconcileBusiness(context.Background(), rec.ID, update); err != nil {
		t.Fatal(err)
	}
	update.SourceRevision = 1
	update.State = BusinessBlocked
	update.Failure = &BusinessFailure{Code: "existing_graph_hold", Summary: "Existing graph hold", EvidenceSHA256: strings.Repeat("a", 64)}
	projected, err := s.ReconcileBusiness(context.Background(), rec.ID, update)
	if err != nil || projected.Admission != nil {
		t.Fatalf("graph receipt did not supersede queue diagnostics: %+v %v", projected, err)
	}
	if _, err = s.SetAdmissionWaiting(context.Background(), rec.ID, update.Assignment, WaitingBudgetExhausted); !errors.Is(err, ErrBusinessConflict) {
		t.Fatalf("queue check overwrote graph state: %v", err)
	}
	stored, err := s.Get(context.Background(), rec.ID)
	if err != nil || stored.Business.State != BusinessBlocked || stored.Business.Failure.Code != "existing_graph_hold" {
		t.Fatalf("graph hold changed: %+v %v", stored.Business, err)
	}
}

func TestAdmissionSchemaUpgradeDoesNotPermitLegacyWritersToEraseWaiting(t *testing.T) {
	s, rec, update := businessFixture(t)
	path := filepath.Join(s.Root(), "messages", rec.ID+".json")
	rec.Version = 2
	data, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	upgraded, err := s.SetAdmissionWaiting(context.Background(), rec.ID, update.Assignment, WaitingJobPaused)
	if err != nil || upgraded.Version != SchemaVersion {
		t.Fatalf("legacy waiting upgrade: %+v %v", upgraded, err)
	}
	upgraded.Version = 2
	data, _ = json.Marshal(upgraded)
	if err = os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Get(context.Background(), rec.ID); err == nil {
		t.Fatal("legacy version claimed admission metadata it cannot preserve")
	}
}
