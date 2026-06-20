package agent

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Ephemeral jobs live in memory only — Create must not write them to disk,
// so the standalone daemon (a separate process reading disk) never sees them.
func TestEphemeralCreateDoesNotPersist(t *testing.T) {
	root := t.TempDir()
	svc, err := NewCronService(root)
	if err != nil {
		t.Fatal(err)
	}
	job := &CronJob{
		Name:      "sess",
		Prompt:    "do thing",
		Enabled:   true,
		Ephemeral: true,
		Schedule:  CronSchedule{Kind: "every", EveryMs: 1000},
	}
	if err := svc.Create(job); err != nil {
		t.Fatal(err)
	}
	// In-memory: visible via List.
	if len(svc.List()) != 1 {
		t.Fatalf("ephemeral job should be in memory, List len=%d", len(svc.List()))
	}
	// On disk: nothing.
	ents, _ := os.ReadDir(root)
	for _, e := range ents {
		if filepath.Ext(e.Name()) == ".json" {
			t.Errorf("ephemeral job must not be written to disk, found %s", e.Name())
		}
	}
	// A fresh service over the same root (the daemon's view) sees no jobs.
	svc2, _ := NewCronService(root)
	if len(svc2.List()) != 0 {
		t.Errorf("daemon view should not see ephemeral job, got %d", len(svc2.List()))
	}
}

// Mutating an ephemeral job (Update/Pause) must NOT persist it — saveJob
// guards the chokepoint so no path can leak a session-only job to disk.
func TestEphemeralUpdateDoesNotPersist(t *testing.T) {
	root := t.TempDir()
	svc, _ := NewCronService(root)
	job := &CronJob{
		Name: "sess", Prompt: "p", Enabled: true, Ephemeral: true,
		Schedule: CronSchedule{Kind: "every", EveryMs: 1000},
	}
	if err := svc.Create(job); err != nil {
		t.Fatal(err)
	}
	if err := svc.Pause(job.ID); err != nil {
		t.Fatal(err)
	}
	if err := svc.Update(job.ID, func(j *CronJob) { j.AllowTools = append(j.AllowTools, "Write") }); err != nil {
		t.Fatal(err)
	}
	ents, _ := os.ReadDir(root)
	for _, e := range ents {
		if filepath.Ext(e.Name()) == ".json" {
			t.Errorf("ephemeral job persisted to disk after Update/Pause: %s", e.Name())
		}
	}
}

// A durable job DOES persist and is invisible to FireDueEphemeral.
func TestDurableJobNotFiredByEphemeralPath(t *testing.T) {
	root := t.TempDir()
	svc, _ := NewCronService(root)
	durable := &CronJob{
		Name: "dur", Prompt: "x", Enabled: true,
		Schedule: CronSchedule{Kind: "every", EveryMs: 1},
	}
	if err := svc.Create(durable); err != nil {
		t.Fatal(err)
	}
	time.Sleep(5 * time.Millisecond)
	if fired := svc.FireDueEphemeral(time.Now()); len(fired) != 0 {
		t.Errorf("FireDueEphemeral must skip durable jobs, fired %d", len(fired))
	}
}

func TestFireDueEphemeralAdvancesAndRepeats(t *testing.T) {
	root := t.TempDir()
	svc, _ := NewCronService(root)

	// Recurring ephemeral: fires, then reschedules (stays enabled).
	rec := &CronJob{
		Name: "rec", Prompt: "p", Enabled: true, Ephemeral: true,
		Schedule: CronSchedule{Kind: "every", EveryMs: 60000},
	}
	svc.Create(rec)
	firstNext := rec.NextRun

	fired := svc.FireDueEphemeral(firstNext.Add(time.Second))
	if len(fired) != 1 || fired[0].ID != rec.ID {
		t.Fatalf("recurring due job should fire once, got %d", len(fired))
	}
	if rec.RunCount != 1 {
		t.Errorf("RunCount = %d, want 1", rec.RunCount)
	}
	if !rec.Enabled {
		t.Errorf("recurring job should stay enabled after firing")
	}
	// computeNextRun for "every" is wall-clock now+interval, so after firing
	// the job must have a fresh future next-run (it keeps recurring).
	if !rec.NextRun.After(time.Now()) {
		t.Errorf("recurring job should have a future NextRun after firing, got %v", rec.NextRun)
	}
	_ = firstNext

	// One-shot ephemeral (Repeat=1): fires once, then disables itself.
	// Fresh service so the recurring job above doesn't also come due here.
	svcOne, _ := NewCronService(t.TempDir())
	one := &CronJob{
		Name: "one", Prompt: "p", Enabled: true, Ephemeral: true, Repeat: 1,
		Schedule: CronSchedule{Kind: "every", EveryMs: 60000},
	}
	svcOne.Create(one)
	due := one.NextRun.Add(time.Second)
	if fired := svcOne.FireDueEphemeral(due); len(fired) != 1 {
		t.Fatalf("one-shot should fire once, got %d", len(fired))
	}
	if one.Enabled {
		t.Errorf("one-shot should be disabled after its single fire")
	}
	// Not due again.
	if fired := svcOne.FireDueEphemeral(due.Add(time.Hour)); len(fired) != 0 {
		t.Errorf("disabled one-shot must not fire again, got %d", len(fired))
	}
}
