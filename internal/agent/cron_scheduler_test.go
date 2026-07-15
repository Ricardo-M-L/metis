package agent

import (
	"context"
	"sync"
	"testing"
	"time"
)

func waitCronCondition(t *testing.T, description string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", description)
}

func TestCronSchedulerReloadsExternalCRUD(t *testing.T) {
	root := t.TempDir()
	daemon, err := NewCronService(root)
	if err != nil {
		t.Fatal(err)
	}
	daemon.refreshInterval = 5 * time.Millisecond

	// A daemon refresh must preserve the live chat's memory-only jobs.
	ephemeral := &CronJob{
		ID: "session-only", Prompt: "later", Enabled: true, Ephemeral: true,
		Schedule: CronSchedule{Kind: "every", EveryMs: int64(time.Hour / time.Millisecond)},
	}
	if err := daemon.Create(ephemeral); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer daemon.Stop()
	daemon.Start(ctx, nil)

	// This instance stands in for later `metis cron` / Desktop invocations.
	writer, err := NewCronService(root)
	if err != nil {
		t.Fatal(err)
	}
	durable := &CronJob{
		ID: "external", Name: "external job", Prompt: "work", Enabled: true,
		Schedule: CronSchedule{Kind: "every", EveryMs: int64(time.Hour / time.Millisecond)},
	}
	if err := writer.Create(durable); err != nil {
		t.Fatal(err)
	}
	waitCronCondition(t, "external add", func() bool {
		job, ok := daemon.Get(durable.ID)
		return ok && job.Name == "external job"
	})

	if err := writer.Pause(durable.ID); err != nil {
		t.Fatal(err)
	}
	waitCronCondition(t, "external pause", func() bool {
		job, ok := daemon.Get(durable.ID)
		return ok && job.Paused
	})

	if err := writer.Resume(durable.ID); err != nil {
		t.Fatal(err)
	}
	waitCronCondition(t, "external resume", func() bool {
		job, ok := daemon.Get(durable.ID)
		return ok && !job.Paused
	})

	if err := writer.Update(durable.ID, func(job *CronJob) {
		job.AllowTools = append(job.AllowTools, "Bash(git status:*)")
	}); err != nil {
		t.Fatal(err)
	}
	waitCronCondition(t, "external allow-list update", func() bool {
		job, ok := daemon.Get(durable.ID)
		return ok && len(job.AllowTools) == 1 && job.AllowTools[0] == "Bash(git status:*)"
	})

	if err := writer.Remove(durable.ID); err != nil {
		t.Fatal(err)
	}
	waitCronCondition(t, "external remove", func() bool {
		_, ok := daemon.Get(durable.ID)
		return !ok
	})
	if _, ok := daemon.Get(ephemeral.ID); !ok {
		t.Fatal("durable refresh removed a live session-only job")
	}
}

func TestCronSchedulerFiresExternallyAddedJob(t *testing.T) {
	root := t.TempDir()
	daemon, err := NewCronService(root)
	if err != nil {
		t.Fatal(err)
	}
	daemon.refreshInterval = 5 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer daemon.Stop()
	fired := make(chan *CronJob, 1)
	daemon.Start(ctx, func(job *CronJob) error {
		fired <- job
		return nil
	})

	writer, err := NewCronService(root)
	if err != nil {
		t.Fatal(err)
	}
	job := &CronJob{
		ID: "late-add", Name: "added after daemon start", Prompt: "work", Enabled: true, Repeat: 1,
		Schedule: CronSchedule{Kind: "every", EveryMs: 10},
	}
	if err := writer.Create(job); err != nil {
		t.Fatal(err)
	}

	select {
	case got := <-fired:
		if got.ID != job.ID || got.RunCount != 1 || got.Enabled {
			t.Fatalf("fired snapshot = %+v", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("daemon did not fire a job added after startup")
	}
	stored, ok := daemon.Get(job.ID)
	if !ok || stored.RunCount != 1 || stored.Enabled {
		t.Fatalf("daemon did not persist fire bookkeeping: %+v, ok=%v", stored, ok)
	}
}

func TestCronSchedulerDoesNotHoldLockDuringOnFire(t *testing.T) {
	svc, err := NewCronService(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	svc.refreshInterval = 5 * time.Millisecond
	job := &CronJob{
		ID: "blocking-callback", Name: "original", Prompt: "work", Enabled: true, Repeat: 1,
		Schedule: CronSchedule{Kind: "every", EveryMs: 5},
	}
	if err := svc.Create(job); err != nil {
		t.Fatal(err)
	}

	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer svc.Stop()
	defer func() { once.Do(func() { close(release) }) }()
	svc.Start(ctx, func(snapshot *CronJob) error {
		// Mutating the callback value must not mutate service-owned state.
		snapshot.Name = "callback mutation"
		close(entered)
		<-release
		return nil
	})

	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("scheduler did not enter callback")
	}
	if current, ok := svc.Get(job.ID); !ok || current.Name != "original" {
		t.Fatalf("callback received service-owned pointer: %+v, ok=%v", current, ok)
	}

	updated := make(chan error, 1)
	go func() {
		updated <- svc.Update(job.ID, func(current *CronJob) {
			current.AllowTools = append(current.AllowTools, "Read")
		})
	}()
	select {
	case err := <-updated:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(300 * time.Millisecond):
		t.Fatal("cron CRUD blocked behind onFire callback")
	}
	once.Do(func() { close(release) })
}

func TestCronServiceSnapshotsAreIndependentAndRaceFree(t *testing.T) {
	svc, err := NewCronService(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	job := &CronJob{
		ID: "snapshot", Name: "original", Prompt: "work", Enabled: true, Ephemeral: true,
		AllowTools: []string{"Read"}, DisabledTools: []string{"Write"}, Skills: []string{"review"},
		Schedule: CronSchedule{Kind: "every", EveryMs: int64(time.Hour / time.Millisecond)},
	}
	if err := svc.Create(job); err != nil {
		t.Fatal(err)
	}

	got, ok := svc.Get(job.ID)
	if !ok {
		t.Fatal("Get did not find created job")
	}
	got.Name = "mutated"
	got.AllowTools[0] = "Bash"
	listed := svc.List()
	listed[0].DisabledTools[0] = "Edit"
	listed[0].Skills[0] = "mutated"
	current, _ := svc.Get(job.ID)
	if current.Name != "original" || current.AllowTools[0] != "Read" ||
		current.DisabledTools[0] != "Write" || current.Skills[0] != "review" {
		t.Fatalf("List/Get leaked service-owned storage: %+v", current)
	}

	var wg sync.WaitGroup
	for reader := 0; reader < 4; reader++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				if snapshot, ok := svc.Get(job.ID); ok {
					snapshot.Name = "reader mutation"
					snapshot.AllowTools[0] = "reader mutation"
				}
				for _, snapshot := range svc.List() {
					_ = snapshot.RunCount
				}
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			if err := svc.Run(job.ID); err != nil {
				t.Errorf("Run: %v", err)
				return
			}
		}
	}()
	wg.Wait()
	current, _ = svc.Get(job.ID)
	if current.RunCount != 200 || current.Name != "original" || current.AllowTools[0] != "Read" {
		t.Fatalf("concurrent snapshots corrupted service state: %+v", current)
	}
}
