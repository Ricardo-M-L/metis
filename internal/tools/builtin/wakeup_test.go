package builtin

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/tasks"
)

// newTestWakeup constructs a wakeup tool wired to an in-tempdir cron
// service so each test gets a fresh on-disk job set.
func newTestWakeup(t *testing.T) (ScheduleWakeup, *agent.CronService) {
	t.Helper()
	svc, err := agent.NewCronService(t.TempDir())
	if err != nil {
		t.Fatalf("cron service init: %v", err)
	}
	gate := permission.New(permission.ModeBypass)
	return NewScheduleWakeup(gate, svc).WithContinuation(agent.ContinuationIdentity{SessionID: "wakeup-test", WorkDir: t.TempDir(), Provider: "fake", Model: "test"}), svc
}

func TestScheduleWakeup_RejectsUnboundAndMismatchedSession(t *testing.T) {
	tool, svc := newTestWakeup(t)
	in := map[string]any{"delaySeconds": 60, "prompt": "check", "reason": "build"}
	unbound := NewScheduleWakeup(permission.New(permission.ModeBypass), svc)
	if res, err := unbound.Execute(context.Background(), in); err != nil || !res.IsError {
		t.Fatal("unbound wakeup accepted", err)
	}
	if res, err := tool.Execute(tasks.WithSessionID(context.Background(), "wrong"), in); err != nil || !res.IsError {
		t.Fatal("cross-session accepted", err)
	}
	if len(svc.List()) != 0 {
		t.Fatal("invalid binding persisted work")
	}
}

func TestScheduleWakeup_CreatesOneShotCronJob(t *testing.T) {
	tool, svc := newTestWakeup(t)
	res, err := tool.Execute(context.Background(), map[string]any{
		"delaySeconds": 60,
		"prompt":       "check the build",
		"reason":       "long bun build, sampling progress",
	})
	if err != nil || res.IsError {
		t.Fatalf("Execute err=%v isError=%v output=%q", err, res.IsError, res.Output)
	}
	jobs := svc.List()
	if len(jobs) != 1 {
		t.Fatalf("expected 1 job, got %d", len(jobs))
	}
	job := jobs[0]
	if job.Kind != agent.CronKindContinuation || job.OriginSessionID != "wakeup-test" || job.Ephemeral {
		t.Fatalf("not durable original continuation: %+v", job)
	}
	reloaded, err := agent.NewCronService(svc.Root())
	if err != nil {
		t.Fatal(err)
	}
	if saved, ok := reloaded.Get(job.ID); !ok || saved.OriginSessionID != job.OriginSessionID {
		t.Fatal("continuation not durable")
	}
	if job.Schedule.Kind != "at" {
		t.Errorf("Kind = %q, want at (one-shot)", job.Schedule.Kind)
	}
	if job.Repeat != 1 {
		t.Errorf("Repeat = %d, want 1 so the job self-disables after firing", job.Repeat)
	}
	if !strings.HasPrefix(job.Name, "wakeup:") {
		t.Errorf("Name should be prefixed with `wakeup:`, got %q", job.Name)
	}
	// NextRun should land roughly delaySeconds in the future.
	d := time.Until(job.NextRun)
	if d < 30*time.Second || d > 90*time.Second {
		t.Errorf("NextRun delta = %v; want ~60s", d)
	}
}

func TestScheduleWakeup_ClampsTooSmallDelay(t *testing.T) {
	tool, svc := newTestWakeup(t)
	res, err := tool.Execute(context.Background(), map[string]any{
		"delaySeconds": 5, // below 30s floor
		"prompt":       "spin",
		"reason":       "tight loop attempt",
	})
	if err != nil || res.IsError {
		t.Fatalf("Execute err=%v output=%q", err, res.Output)
	}
	job := svc.List()[0]
	d := time.Until(job.NextRun)
	if d < 25*time.Second {
		t.Errorf("clamp didn't raise to 30s floor; got %v", d)
	}
}

func TestScheduleWakeup_ClampsTooLargeDelay(t *testing.T) {
	tool, svc := newTestWakeup(t)
	res, err := tool.Execute(context.Background(), map[string]any{
		"delaySeconds": 99999999, // way over 24h ceiling
		"prompt":       "wake later",
		"reason":       "long quiet wait",
	})
	if err != nil || res.IsError {
		t.Fatalf("Execute err=%v output=%q", err, res.Output)
	}
	job := svc.List()[0]
	d := time.Until(job.NextRun)
	if d > 25*time.Hour {
		t.Errorf("clamp didn't cap at 24h; got %v", d)
	}
}

func TestScheduleWakeup_RejectsEmptyFields(t *testing.T) {
	tool, _ := newTestWakeup(t)
	cases := []map[string]any{
		{"delaySeconds": 60, "prompt": "", "reason": "x"},
		{"delaySeconds": 60, "prompt": "x", "reason": ""},
		{"delaySeconds": "garbage", "prompt": "x", "reason": "y"},
	}
	for i, in := range cases {
		res, _ := tool.Execute(context.Background(), in)
		if !res.IsError {
			t.Errorf("case %d: expected IsError for %v", i, in)
		}
	}
}
