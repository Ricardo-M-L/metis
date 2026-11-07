package agent

import (
	"errors"
	"os"
	"sync"
	"testing"
	"time"
)

func TestContinuationClaimExecutionReceipt(t *testing.T) {
	root := t.TempDir()
	s, _ := NewCronService(root)
	identity := ContinuationIdentity{SessionID: "original", WorkDir: "/workspace", Provider: "fake", Model: "model"}
	job := &CronJob{Name: "wakeup", Kind: CronKindContinuation, OriginSessionID: identity.SessionID, WorkDir: identity.WorkDir, Provider: identity.Provider, Model: identity.Model, Enabled: true, Repeat: 1, Schedule: CronSchedule{Kind: "at", At: time.Now().Add(-time.Minute).Format(time.RFC3339)}}
	if err := s.Create(job); err != nil {
		t.Fatal(err)
	}
	if id, _ := s.nextJob(); id != "" {
		t.Fatal("daemon must not consume continuation")
	}
	if run, err := s.RunNow(job.ID); err == nil || run != nil {
		t.Fatal("manual cron run must not consume continuation")
	}
	other, _ := NewCronService(root)
	wrong := identity
	wrong.SessionID = "wrong"
	if run, err := other.AcquireDueContinuation(wrong, time.Now()); err != nil || run != nil {
		t.Fatal("cross-session acquisition", err)
	}
	run, err := s.AcquireDueContinuation(identity, time.Now())
	if err != nil || run == nil {
		t.Fatal("missing due continuation", err)
	}
	claimed, _ := s.Get(job.ID)
	if claimed.RunCount != 0 {
		t.Fatal("claim counted as execution")
	}
	if duplicate, err := other.AcquireDueContinuation(identity, time.Now()); err != nil || duplicate != nil {
		t.Fatal("duplicate claim", err)
	}
	if err := run.Start(); err != nil {
		t.Fatal(err)
	}
	if err := run.Start(); err == nil {
		t.Fatal("duplicate start accepted")
	}
	if err := run.Finish("succeeded", "done", "answer", nil); err != nil {
		t.Fatal(err)
	}
	restarted, _ := NewCronService(root)
	done, _ := restarted.Get(job.ID)
	if done.RunCount != 1 || done.ContinuationStatus != "succeeded" {
		t.Fatalf("bad persisted outcome: %+v", done)
	}
	records, err := ListCronRuns(root, job.ID, 10)
	if err != nil || len(records) != 1 || records[0].SessionID != "original" || records[0].Status != "succeeded" {
		t.Fatalf("missing receipt: %+v %v", records, err)
	}
}

func TestContinuationConcurrentClaimAndFailure(t *testing.T) {
	root := t.TempDir()
	s, _ := NewCronService(root)
	identity := ContinuationIdentity{SessionID: "original", WorkDir: "/workspace", Provider: "fake", Model: "model"}
	j := &CronJob{Kind: CronKindContinuation, OriginSessionID: identity.SessionID, WorkDir: identity.WorkDir, Provider: identity.Provider, Model: identity.Model, Enabled: true, Schedule: CronSchedule{Kind: "at", At: time.Now().Add(-time.Minute).Format(time.RFC3339)}}
	if err := s.Create(j); err != nil {
		t.Fatal(err)
	}
	services := []*CronService{s}
	for n := 0; n < 7; n++ {
		other, err := NewCronService(root)
		if err != nil {
			t.Fatal(err)
		}
		services = append(services, other)
	}
	var wg sync.WaitGroup
	runs := make(chan *ContinuationRun, 8)
	errs := make(chan error, 8)
	for _, svc := range services {
		wg.Add(1)
		go func(svc *CronService) {
			defer wg.Done()
			run, err := svc.AcquireDueContinuation(identity, time.Now())
			if err != nil {
				errs <- err
			}
			if run != nil {
				runs <- run
			}
		}(svc)
	}
	wg.Wait()
	close(runs)
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	if len(runs) != 1 {
		t.Fatalf("got %d admissions", len(runs))
	}
	run := <-runs
	if err := run.Finish("failed", "checkpoint failed", "", errors.New("disk full")); err != nil {
		t.Fatal(err)
	}
	restarted, _ := NewCronService(root)
	done, _ := restarted.Get(j.ID)
	if done.RunCount != 0 || done.ContinuationStatus != "failed" {
		t.Fatalf("failure counted execution: %+v", done)
	}
	if pending, err := restarted.PendingContinuations(identity, time.Now()); err != nil || len(pending) != 0 {
		t.Fatalf("failure still pending: %+v %v", pending, err)
	}
	if err := restarted.Update(j.ID, func(j *CronJob) { j.Enabled = true }); err != nil {
		t.Fatal(err)
	}
	if duplicate, err := restarted.AcquireDueContinuation(identity, time.Now()); err != nil || duplicate != nil {
		t.Fatal("terminal rearmed", err)
	}
}

func TestContinuationCrashClaimIsAuditable(t *testing.T) {
	s, _ := NewCronService(t.TempDir())
	identity := ContinuationIdentity{SessionID: "original", WorkDir: "/workspace", Provider: "fake", Model: "model"}
	j := &CronJob{Kind: CronKindContinuation, OriginSessionID: identity.SessionID, WorkDir: identity.WorkDir, Provider: identity.Provider, Model: identity.Model, Enabled: true, Schedule: CronSchedule{Kind: "at", At: time.Now().Add(-time.Minute).Format(time.RFC3339)}}
	if err := s.Create(j); err != nil {
		t.Fatal(err)
	}
	run, err := s.AcquireDueContinuation(identity, time.Now())
	if err != nil || run == nil {
		t.Fatal(err)
	}
	// Simulate OS-released ownership after process exit, leaving the running receipt.
	_ = unlockCronFile(run.receipt.lock)
	_ = run.receipt.lock.Close()
	run.receipt.lock = nil
	restarted, _ := NewCronService(s.Root())
	pending, err := restarted.PendingContinuations(identity, time.Now())
	if err != nil || len(pending) != 0 {
		t.Fatal("lost claim replayed", err)
	}
	done, _ := restarted.Get(j.ID)
	if done.ContinuationStatus != "interrupted" || done.RunCount != 0 {
		t.Fatalf("lost claim not audited: %+v", done)
	}
}

func TestContinuationFinishRetriesMetadataCommit(t *testing.T) {
	s, _ := NewCronService(t.TempDir())
	identity := ContinuationIdentity{SessionID: "original", WorkDir: "/workspace", Provider: "fake", Model: "model"}
	j := &CronJob{Kind: CronKindContinuation, OriginSessionID: identity.SessionID, WorkDir: identity.WorkDir, Provider: identity.Provider, Model: identity.Model, Enabled: true, Schedule: CronSchedule{Kind: "at", At: time.Now().Add(-time.Minute).Format(time.RFC3339)}}
	if err := s.Create(j); err != nil {
		t.Fatal(err)
	}
	run, err := s.AcquireDueContinuation(identity, time.Now())
	if err != nil || run == nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(s.path(j.ID))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(s.path(j.ID)); err != nil {
		t.Fatal(err)
	}
	if err := run.Finish("failed", "not run", "", errors.New("checkpoint")); err == nil {
		t.Fatal("expected metadata commit failure")
	}
	receipt, err := ReadCronRun(s.Root(), j.ID, run.Job.ContinuationRunID)
	if err != nil || receipt.Status != "failed" {
		t.Fatalf("receipt lost: %+v %v", receipt, err)
	}
	if err := os.WriteFile(s.path(j.ID), data, 0600); err != nil {
		t.Fatal(err)
	}
	if err := run.Finish("failed", "not run", "", errors.New("checkpoint")); err != nil {
		t.Fatal(err)
	}
	done, _ := s.Get(j.ID)
	if done.ContinuationStatus != "failed" {
		t.Fatalf("retry failed: %+v", done)
	}
}

func TestContinuationIdentityAndPending(t *testing.T) {
	s, _ := NewCronService(t.TempDir())
	identity := ContinuationIdentity{SessionID: "original", WorkDir: "/workspace", Provider: "fake", Model: "model"}
	job := &CronJob{Kind: CronKindContinuation, OriginSessionID: identity.SessionID, WorkDir: identity.WorkDir, Provider: identity.Provider, Model: identity.Model, Enabled: true, Schedule: CronSchedule{Kind: "at", At: time.Now().Add(-time.Minute).Format(time.RFC3339)}}
	if err := s.Create(job); err != nil {
		t.Fatal(err)
	}
	wrong := identity
	wrong.Provider = "other"
	if run, err := s.AcquireDueContinuation(wrong, time.Now()); err != nil || run != nil {
		t.Fatal("provider mismatch consumed", err)
	}
	pending, err := s.PendingContinuations(identity, time.Now())
	if err != nil || len(pending) != 1 || pending[0].NextRun.After(time.Now()) {
		t.Fatalf("overdue not observable: %+v %v", pending, err)
	}
}
