package agent

import (
	"context"
	"os"
	"sync/atomic"
	"testing"
	"time"
)

func newDurableCronTestJob(id string) *CronJob {
	return &CronJob{
		ID: id, Name: id, Prompt: "work", Enabled: true,
		Schedule: CronSchedule{Kind: "every", EveryMs: int64(time.Hour / time.Millisecond)},
	}
}

func TestCronStorageTransactionPreservesExternalPause(t *testing.T) {
	root := t.TempDir()
	seed, err := NewCronService(root)
	if err != nil {
		t.Fatal(err)
	}
	job := newDurableCronTestJob("pause-race")
	if err := seed.Create(job); err != nil {
		t.Fatal(err)
	}

	staleDaemon, err := NewCronService(root)
	if err != nil {
		t.Fatal(err)
	}
	writer, err := NewCronService(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := writer.Pause(job.ID); err != nil {
		t.Fatal(err)
	}
	if err := staleDaemon.Run(job.ID); err != nil {
		t.Fatal(err)
	}

	fresh, err := NewCronService(root)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := fresh.Get(job.ID)
	if !ok || !got.Paused || got.RunCount != 1 {
		t.Fatalf("stale manual run overwrote external pause: %+v, ok=%v", got, ok)
	}
}

func TestCronStorageTransactionClaimRespectsExternalPause(t *testing.T) {
	root := t.TempDir()
	seed, err := NewCronService(root)
	if err != nil {
		t.Fatal(err)
	}
	job := newDurableCronTestJob("pause-before-claim")
	if err := seed.Create(job); err != nil {
		t.Fatal(err)
	}
	if err := seed.Update(job.ID, func(current *CronJob) {
		current.NextRun = time.Now().Add(-time.Second)
	}); err != nil {
		t.Fatal(err)
	}

	staleDaemon, err := NewCronService(root)
	if err != nil {
		t.Fatal(err)
	}
	writer, err := NewCronService(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := writer.Pause(job.ID); err != nil {
		t.Fatal(err)
	}
	fired, err := staleDaemon.claimDueJob(job.ID, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if fired != nil {
		t.Fatalf("scheduler claimed externally paused job: %+v", fired)
	}

	fresh, err := NewCronService(root)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := fresh.Get(job.ID)
	if !ok || !got.Paused || got.RunCount != 0 {
		t.Fatalf("paused job changed at firing edge: %+v, ok=%v", got, ok)
	}
}

func TestCronStorageTransactionDoesNotResurrectRemovedJob(t *testing.T) {
	root := t.TempDir()
	seed, err := NewCronService(root)
	if err != nil {
		t.Fatal(err)
	}
	job := newDurableCronTestJob("remove-race")
	if err := seed.Create(job); err != nil {
		t.Fatal(err)
	}

	staleDaemon, err := NewCronService(root)
	if err != nil {
		t.Fatal(err)
	}
	writer, err := NewCronService(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := writer.Remove(job.ID); err != nil {
		t.Fatal(err)
	}
	if err := staleDaemon.Run(job.ID); err == nil {
		t.Fatal("stale manual run should reject a job removed by another process")
	}
	if _, err := os.Stat(staleDaemon.path(job.ID)); !os.IsNotExist(err) {
		t.Fatalf("removed job was resurrected: stat err=%v", err)
	}
}

func TestCronStorageTransactionPreservesRunBookkeepingOnExternalUpdate(t *testing.T) {
	root := t.TempDir()
	seed, err := NewCronService(root)
	if err != nil {
		t.Fatal(err)
	}
	job := newDurableCronTestJob("update-race")
	if err := seed.Create(job); err != nil {
		t.Fatal(err)
	}

	runner, err := NewCronService(root)
	if err != nil {
		t.Fatal(err)
	}
	staleWriter, err := NewCronService(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := runner.Run(job.ID); err != nil {
		t.Fatal(err)
	}
	if err := staleWriter.Update(job.ID, func(current *CronJob) {
		current.AllowTools = []string{"Read"}
	}); err != nil {
		t.Fatal(err)
	}

	fresh, err := NewCronService(root)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := fresh.Get(job.ID)
	if !ok || got.RunCount != 1 || got.LastRun.IsZero() || len(got.AllowTools) != 1 || got.AllowTools[0] != "Read" {
		t.Fatalf("external update lost scheduler bookkeeping: %+v, ok=%v", got, ok)
	}
}

func TestCronStorageTransactionReapUsesLatestExpiry(t *testing.T) {
	root := t.TempDir()
	seed, err := NewCronService(root)
	if err != nil {
		t.Fatal(err)
	}
	job := newDurableCronTestJob("expiry-race")
	job.ExpiresAt = time.Now().Add(-time.Minute)
	if err := seed.Create(job); err != nil {
		t.Fatal(err)
	}

	staleReaper, err := NewCronService(root)
	if err != nil {
		t.Fatal(err)
	}
	writer, err := NewCronService(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := writer.Update(job.ID, func(current *CronJob) {
		current.ExpiresAt = time.Now().Add(time.Hour)
	}); err != nil {
		t.Fatal(err)
	}
	staleReaper.reapExpired()

	fresh, err := NewCronService(root)
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := fresh.Get(job.ID); !ok || got.ExpiresAt.Before(time.Now()) {
		t.Fatalf("stale reaper deleted externally extended job: %+v, ok=%v", got, ok)
	}
}

func TestCronStorageTransactionClaimsRepeatOnceOnlyOnce(t *testing.T) {
	root := t.TempDir()
	seed, err := NewCronService(root)
	if err != nil {
		t.Fatal(err)
	}
	job := &CronJob{
		ID: "single-claim", Name: "single-claim", Prompt: "work", Enabled: true, Repeat: 1,
		Schedule: CronSchedule{Kind: "every", EveryMs: 10},
	}
	if err := seed.Create(job); err != nil {
		t.Fatal(err)
	}

	first, err := NewCronService(root)
	if err != nil {
		t.Fatal(err)
	}
	second, err := NewCronService(root)
	if err != nil {
		t.Fatal(err)
	}
	first.refreshInterval = 5 * time.Millisecond
	second.refreshInterval = 5 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer first.Stop()
	defer second.Stop()
	var callbacks atomic.Int32
	onFire := func(*CronJob) error {
		callbacks.Add(1)
		return nil
	}
	first.Start(ctx, onFire)
	second.Start(ctx, onFire)

	waitCronCondition(t, "one scheduler claim", func() bool { return callbacks.Load() > 0 })
	time.Sleep(75 * time.Millisecond)
	if got := callbacks.Load(); got != 1 {
		t.Fatalf("repeat=1 job fired %d times across two schedulers, want 1", got)
	}
	fresh, err := NewCronService(root)
	if err != nil {
		t.Fatal(err)
	}
	stored, ok := fresh.Get(job.ID)
	if !ok || stored.RunCount != 1 || stored.Enabled {
		t.Fatalf("single claim bookkeeping = %+v, ok=%v", stored, ok)
	}
}
