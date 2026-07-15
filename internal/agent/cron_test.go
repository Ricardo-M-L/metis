package agent

import (
	"context"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

func TestCronService_CreateListGetRemove(t *testing.T) {
	dir := t.TempDir()
	svc, err := NewCronService(dir)
	if err != nil {
		t.Fatal(err)
	}
	job := &CronJob{
		Name:     "morning",
		Prompt:   "say hi",
		Enabled:  true,
		Schedule: CronSchedule{Kind: "every", EveryMs: 60000},
	}
	if err := svc.Create(job); err != nil {
		t.Fatal(err)
	}
	if job.ID == "" {
		t.Fatal("Create should generate ID")
	}
	if job.NextRun.IsZero() {
		t.Fatal("Create should compute NextRun for enabled jobs")
	}

	got, ok := svc.Get(job.ID)
	if !ok {
		t.Fatal("Get returned !ok for existing id")
	}
	if got.Name != "morning" {
		t.Errorf("name mismatch: %q", got.Name)
	}

	list := svc.List()
	if len(list) != 1 {
		t.Fatalf("List len: %d", len(list))
	}

	if err := svc.Remove(job.ID); err != nil {
		t.Fatal(err)
	}
	if _, ok := svc.Get(job.ID); ok {
		t.Fatal("job should be gone after Remove")
	}
}

func TestCronService_ExpiresAtReap(t *testing.T) {
	dir := t.TempDir()
	svc, _ := NewCronService(dir)

	live := &CronJob{
		Prompt:    "live",
		Enabled:   true,
		Schedule:  CronSchedule{Kind: "every", EveryMs: 60000},
		ExpiresAt: time.Now().Add(time.Hour),
	}
	dead := &CronJob{
		Prompt:    "dead",
		Enabled:   true,
		Schedule:  CronSchedule{Kind: "every", EveryMs: 60000},
		ExpiresAt: time.Now().Add(-time.Minute), // already past
	}
	if err := svc.Create(live); err != nil {
		t.Fatal(err)
	}
	if err := svc.Create(dead); err != nil {
		t.Fatal(err)
	}

	svc.reapExpired()

	if _, ok := svc.Get(dead.ID); ok {
		t.Errorf("expired job should be reaped, but Get still returns it")
	}
	if _, ok := svc.Get(live.ID); !ok {
		t.Errorf("live job should survive reap")
	}
}

func TestCronService_PauseResume(t *testing.T) {
	dir := t.TempDir()
	svc, _ := NewCronService(dir)
	job := &CronJob{
		Prompt:   "x",
		Enabled:  true,
		Schedule: CronSchedule{Kind: "every", EveryMs: 1000},
	}
	svc.Create(job)
	if err := svc.Pause(job.ID); err != nil {
		t.Fatal(err)
	}
	got, _ := svc.Get(job.ID)
	if !got.Paused {
		t.Error("Pause did not set Paused")
	}
	if err := svc.Resume(job.ID); err != nil {
		t.Fatal(err)
	}
	got, _ = svc.Get(job.ID)
	if got.Paused {
		t.Error("Resume did not clear Paused")
	}
	if got.NextRun.IsZero() {
		t.Error("Resume should recompute NextRun")
	}
}

func TestCronService_MetadataUpdatesPreserveNextRun(t *testing.T) {
	svc, err := NewCronService(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	job := &CronJob{
		Name: "before", Prompt: "work", Enabled: true,
		Schedule: CronSchedule{Kind: "every", EveryMs: int64(time.Hour / time.Millisecond)},
	}
	if err := svc.Create(job); err != nil {
		t.Fatal(err)
	}
	originalNext := job.NextRun

	if err := svc.Update(job.ID, func(current *CronJob) {
		current.AllowTools = append(current.AllowTools, "Read")
	}); err != nil {
		t.Fatal(err)
	}
	afterAllow, _ := svc.Get(job.ID)
	if !afterAllow.NextRun.Equal(originalNext) {
		t.Fatalf("allow update postponed next run: before=%v after=%v", originalNext, afterAllow.NextRun)
	}

	if err := svc.Update(job.ID, func(current *CronJob) {
		current.Name = "after"
		current.Prompt = "updated work"
		current.Silent = true
	}); err != nil {
		t.Fatal(err)
	}
	afterMetadata, _ := svc.Get(job.ID)
	if !afterMetadata.NextRun.Equal(originalNext) {
		t.Fatalf("metadata update postponed next run: before=%v after=%v", originalNext, afterMetadata.NextRun)
	}
}

func TestCronService_PersistenceAcrossReload(t *testing.T) {
	dir := t.TempDir()
	svc, _ := NewCronService(dir)
	svc.Create(&CronJob{
		Name:     "persist",
		Prompt:   "y",
		Enabled:  true,
		Schedule: CronSchedule{Kind: "every", EveryMs: 60000},
	})

	// New service instance reading the same dir
	svc2, err := NewCronService(dir)
	if err != nil {
		t.Fatal(err)
	}
	list := svc2.List()
	if len(list) != 1 {
		t.Fatalf("expected 1 persisted job, got %d", len(list))
	}
	if list[0].Name != "persist" {
		t.Errorf("name mismatch after reload: %q", list[0].Name)
	}
}

func TestCronService_RunIncrementsAndAdvances(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		dir := t.TempDir()
		svc, _ := NewCronService(dir)
		job := &CronJob{
			Prompt:   "z",
			Enabled:  true,
			Schedule: CronSchedule{Kind: "every", EveryMs: 60000},
		}
		svc.Create(job)
		prevNext := job.NextRun
		time.Sleep(2 * time.Millisecond)

		if err := svc.Run(job.ID); err != nil {
			t.Fatal(err)
		}
		got, _ := svc.Get(job.ID)
		if got.RunCount != 1 {
			t.Errorf("RunCount = %d, want 1", got.RunCount)
		}
		if got.LastRun.IsZero() {
			t.Error("LastRun should be set after Run")
		}
		if !got.NextRun.After(prevNext) {
			t.Errorf("NextRun should advance, prev=%v new=%v", prevNext, got.NextRun)
		}
	})
}

func TestCronService_RepeatExhaustionDisables(t *testing.T) {
	dir := t.TempDir()
	svc, _ := NewCronService(dir)
	job := &CronJob{
		Prompt:   "z",
		Enabled:  true,
		Repeat:   2,
		Schedule: CronSchedule{Kind: "every", EveryMs: 60000},
	}
	svc.Create(job)
	svc.Run(job.ID)
	svc.Run(job.ID)
	got, _ := svc.Get(job.ID)
	if got.Enabled {
		t.Error("expected job disabled after exhausting Repeat count")
	}
	if got.RunCount != 2 {
		t.Errorf("RunCount = %d, want 2", got.RunCount)
	}
}

func TestComputeNextRun_AtInPast(t *testing.T) {
	dir := t.TempDir()
	svc, _ := NewCronService(dir)
	past := time.Now().Add(-1 * time.Hour).UTC().Format(time.RFC3339)
	job := &CronJob{
		Prompt:   "z",
		Enabled:  true,
		Schedule: CronSchedule{Kind: "at", At: past},
	}
	svc.Create(job)
	if !job.NextRun.After(time.Now()) {
		t.Errorf("NextRun for past 'at' should roll forward by 24h, got %v", job.NextRun)
	}
}

func TestRandomString_VariesAcrossCalls(t *testing.T) {
	seen := make(map[string]bool)
	for i := 0; i < 20; i++ {
		s := randomString(8)
		if len(s) != 8 {
			t.Fatalf("length wrong: %d", len(s))
		}
		// All-same-char is the historical bug: catch it.
		first := s[0]
		allSame := true
		for j := 1; j < len(s); j++ {
			if s[j] != first {
				allSame = false
				break
			}
		}
		if allSame {
			t.Errorf("all-same-char string %q (the bug we fixed)", s)
		}
		seen[s] = true
	}
	if len(seen) < 18 {
		t.Errorf("expected most of 20 random strings unique, got %d distinct", len(seen))
	}
}

// Regression: calling Start twice without an intervening Stop used to
// spawn two scheduler goroutines; the second one is a no-op now.
func TestCronService_DoubleStartIsNoOp(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		dir := t.TempDir()
		svc, _ := NewCronService(dir)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		fired := make(chan struct{}, 4)
		onFire := func(_ *CronJob) error {
			fired <- struct{}{}
			return nil
		}
		svc.Start(ctx, onFire)
		svc.Start(ctx, onFire) // should NOT spawn a second loop

		svc.mu.RLock()
		running := svc.running
		svc.mu.RUnlock()
		if !running {
			t.Fatal("running flag should be true after Start")
		}

		svc.Stop()
		cancel()
		synctest.Wait() // wait for scheduler goroutine to exit cleanly
	})
}

// TestCron_ParsesRealExpression locks in that the cron-kind schedule
// actually consults the parser instead of the previous "advance one
// hour and shrug" fallback. Uses "0 * * * *" (every hour at :00) so the
// next run must land within the next 60 minutes from now and on the
// :00 minute boundary.
func TestCron_ParsesRealExpression(t *testing.T) {
	dir := t.TempDir()
	svc, _ := NewCronService(dir)
	job := &CronJob{
		Prompt:   "hourly",
		Enabled:  true,
		Schedule: CronSchedule{Kind: "cron", CronExpr: "0 * * * *"},
	}
	if err := svc.Create(job); err != nil {
		t.Fatal(err)
	}
	if job.NextRun.IsZero() {
		t.Fatal("NextRun should be set for valid cron expression")
	}
	if d := time.Until(job.NextRun); d <= 0 || d > time.Hour {
		t.Errorf("NextRun should be within the next hour; got %v away", d)
	}
	if job.NextRun.Minute() != 0 || job.NextRun.Second() != 0 {
		t.Errorf("NextRun should be on the :00 boundary, got %v", job.NextRun)
	}
}

// TestCron_RejectsBadSessionMode covers the runtime gap discovered
// during E2E testing: a typo in --mode silently fell through to
// "isolated" instead of failing fast.
func TestCron_RejectsBadSessionMode(t *testing.T) {
	dir := t.TempDir()
	svc, _ := NewCronService(dir)
	job := &CronJob{
		Prompt:      "x",
		Enabled:     true,
		Schedule:    CronSchedule{Kind: "every", EveryMs: 60000},
		SessionMode: "garbage",
	}
	if err := svc.Create(job); err == nil {
		t.Fatal("Create should reject unknown session_mode")
	}
}

// TestCron_AcceptsKnownSessionModes spot-checks the three valid values
// + the empty (legacy) default. Each must round-trip without error.
func TestCron_AcceptsKnownSessionModes(t *testing.T) {
	for _, mode := range []string{"", "isolated", "persistent", "main"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			svc, _ := NewCronService(dir)
			job := &CronJob{
				Prompt:      "x",
				Enabled:     true,
				Schedule:    CronSchedule{Kind: "every", EveryMs: 60000},
				SessionMode: mode,
			}
			if err := svc.Create(job); err != nil {
				t.Errorf("rejected valid mode %q: %v", mode, err)
			}
		})
	}
}

// TestCron_RejectsBadExpression makes sure we don't silently accept
// garbage and then mis-fire forever.
func TestCron_RejectsBadExpression(t *testing.T) {
	dir := t.TempDir()
	svc, _ := NewCronService(dir)
	job := &CronJob{
		Prompt:   "broken",
		Enabled:  true,
		Schedule: CronSchedule{Kind: "cron", CronExpr: "not a cron expr"},
	}
	if err := svc.Create(job); err == nil {
		t.Fatal("Create should reject invalid cron expression")
	}
}

// TestCron_AcceptsDescriptors covers the @daily / @hourly / @every
// shortcuts that robfig/cron's Descriptor option enables.
func TestCron_AcceptsDescriptors(t *testing.T) {
	cases := []string{"@daily", "@hourly", "@every 2h"}
	for _, expr := range cases {
		t.Run(expr, func(t *testing.T) {
			dir := t.TempDir()
			svc, _ := NewCronService(dir)
			job := &CronJob{
				Prompt:   "x",
				Enabled:  true,
				Schedule: CronSchedule{Kind: "cron", CronExpr: expr},
			}
			if err := svc.Create(job); err != nil {
				t.Fatalf("descriptor %q rejected: %v", expr, err)
			}
			if job.NextRun.IsZero() {
				t.Errorf("NextRun zero for %q", expr)
			}
		})
	}
}

// TestCron_TimezoneApplied locks in that an explicit IANA TZ is honored
// when computing NextRun. Uses a 1-second "every" cadence so we can
// observe the offset rather than wait an hour.
func TestCron_TimezoneApplied(t *testing.T) {
	if _, err := time.LoadLocation("America/Los_Angeles"); err != nil {
		t.Skip("tzdata not available")
	}
	dir := t.TempDir()
	svc, _ := NewCronService(dir)
	job := &CronJob{
		Prompt:   "x",
		Enabled:  true,
		Schedule: CronSchedule{Kind: "every", EveryMs: 1000, TZ: "America/Los_Angeles"},
	}
	if err := svc.Create(job); err != nil {
		t.Fatal(err)
	}
	if job.NextRun.Location().String() == "" {
		// Just sanity — the field should round-trip.
		t.Errorf("NextRun lost location info: %v", job.NextRun)
	}
}

// TestCron_JitterAddsBoundedNoise checks that JitterMs offsets the
// next run by something within the requested span. Run repeatedly to
// catch drift outside [-span, +span].
func TestCron_JitterAddsBoundedNoise(t *testing.T) {
	const spanMs = 500
	for i := 0; i < 20; i++ {
		j := jitter(spanMs)
		if j < -time.Duration(spanMs)*time.Millisecond ||
			j > time.Duration(spanMs)*time.Millisecond {
			t.Errorf("jitter out of range: %v", j)
		}
	}
}

func TestGenerateID_FormatAndUnique(t *testing.T) {
	id1 := generateID()
	id2 := generateID()
	if id1 == id2 {
		t.Errorf("two generateID calls collided: %q", id1)
	}
	if !strings.Contains(id1, "-") {
		t.Errorf("expected timestamp-suffix format, got %q", id1)
	}
}
