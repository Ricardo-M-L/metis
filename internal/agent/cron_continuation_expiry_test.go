package agent

import (
	"testing"
	"time"
)

func TestContinuationExpiryRetainsHistoryAndClaims(t *testing.T) {
	for _, state := range []string{"pending", "claimed", "running"} {
		t.Run(state, func(t *testing.T) {
			s, err := NewCronService(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			identity := ContinuationIdentity{SessionID: "origin", WorkDir: t.TempDir(), Provider: "fake", Model: "test"}
			now := time.Now()
			j := &CronJob{Kind: CronKindContinuation, OriginSessionID: identity.SessionID, WorkDir: identity.WorkDir, Provider: identity.Provider, Model: identity.Model, ContinuationStatus: "pending", Enabled: true, ExpiresAt: now.Add(time.Hour), Schedule: CronSchedule{Kind: "at", At: now.Add(-time.Minute).Format(time.RFC3339)}}
			if err := s.Create(j); err != nil {
				t.Fatal(err)
			}
			var run *ContinuationRun
			if state != "pending" {
				run, err = s.AcquireDueContinuation(identity, now)
				if err != nil || run == nil {
					t.Fatal("claim", err)
				}
				if state == "running" {
					if err := run.Start(); err != nil {
						t.Fatal(err)
					}
				}
			}
			if err := s.Update(j.ID, func(j *CronJob) { j.ExpiresAt = now.Add(-time.Second) }); err != nil {
				t.Fatal(err)
			}
			if state == "pending" {
				current, _ := s.Get(j.ID)
				if got := CronJobStatus(current, now); got != "expired" {
					t.Fatalf("expired shown as %q", got)
				}
			}
			if err := s.refreshDurableState(); err != nil {
				t.Fatal(err)
			}
			restarted, err := NewCronService(s.Root())
			if err != nil {
				t.Fatal(err)
			}
			current, ok := restarted.Get(j.ID)
			if !ok {
				t.Fatal("expiry removed continuation history")
			}
			if state == "pending" {
				if current.Enabled || current.ContinuationStatus != "expired" {
					t.Fatalf("not terminal expired: %+v", current)
				}
				if got, err := restarted.AcquireDueContinuation(identity, now); err != nil || got != nil {
					t.Fatal("expired admitted", err)
				}
				pending, err := restarted.PendingContinuations(identity, now)
				if err != nil || len(pending) != 0 {
					t.Fatal("expired pending", err)
				}
			} else {
				if current.ContinuationStatus != state {
					t.Fatalf("active claim overwritten: %+v", current)
				}
				status := "succeeded"
				if state == "claimed" {
					if err := run.Start(); err == nil {
						t.Fatal("expired claim started")
					}
					status = "cancelled"
				}
				if err := run.Finish(status, "expiry handled", "", nil); err != nil {
					t.Fatal("expiry broke completion", err)
				}
				records, err := ListCronRuns(s.Root(), j.ID, 10)
				if err := s.refreshDurableState(); err != nil {
					t.Fatal(err)
				}
				terminal, ok := s.Get(j.ID)
				if !ok || terminal.ContinuationStatus != status || CronJobStatus(terminal, now) != status {
					t.Fatal("terminal expiry history overwritten")
				}
				if err != nil || len(records) != 1 || records[0].Status != status {
					t.Fatalf("receipt lost: %+v %v", records, err)
				}
			}
		})
	}
}
