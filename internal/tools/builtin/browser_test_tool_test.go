package builtin

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/internal/tools"
)

func TestBrowserTestRejectsUntrustedArguments(t *testing.T) {
	calls := 0
	b := NewBrowserTest(func(context.Context, string) (string, bool, error) { calls++; return "{}", true, nil }, nil)
	for _, in := range []map[string]any{{}, {"suite": "shell"}, {"suite": "smoke", "url": "http://evil"}, {"suite": "smoke", "command": "id"}} {
		res, err := b.Execute(context.Background(), in)
		if err != nil || !res.IsError {
			t.Fatalf("bad arguments accepted: %v, %v", res, err)
		}
	}
	if calls != 0 {
		t.Fatalf("runner called %d times", calls)
	}
}

func TestBrowserTestCannotFinishBeforeRunner(t *testing.T) {
	started, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	b := NewBrowserTest(func(ctx context.Context, suite string) (string, bool, error) {
		close(started)
		select {
		case <-release:
			return `{"status":"completed"}`, true, nil
		case <-ctx.Done():
			return "", false, ctx.Err()
		}
	}, nil)
	go func() {
		defer close(done)
		_, _ = b.Execute(context.Background(), map[string]any{"suite": "endurance"})
	}()
	<-started
	select {
	case <-done:
		t.Fatal("returned before test completed")
	default:
	}
	close(release)
	<-done
	if b.Concurrency(nil) != tools.ConcurrencyExclusive {
		t.Fatal("must serialize with source writes")
	}
	if b.TimeoutMs() != int((35*time.Minute)/time.Millisecond) {
		t.Fatal("tool budget must cover preparation, 15-minute suite and cleanup")
	}
}

func TestBrowserTestFailureAndCancellation(t *testing.T) {
	b := NewBrowserTest(func(context.Context, string) (string, bool, error) { return `{"status":"failed"}`, false, nil }, nil)
	res, err := b.Execute(context.Background(), map[string]any{"suite": "controls"})
	if err != nil || !res.IsError {
		t.Fatalf("failure became success: %v %v", res, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = b.Execute(ctx, map[string]any{"suite": "smoke"})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation = %v", err)
	}
}

func TestTaskClockUsesLiveTimeAndInvocationElapsed(t *testing.T) {
	clock := NewTaskClock(time.Now().Add(-2*time.Second), time.Time{})
	// A tool/turn timeout must not be misreported as the whole run deadline.
	toolCtx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	res, err := clock.Execute(toolCtx, nil)
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Now      string  `json:"current_time_utc"`
		Elapsed  float64 `json:"invocation_elapsed_seconds"`
		Deadline *string `json:"run_deadline_utc"`
	}
	if err = json.Unmarshal([]byte(res.Output), &got); err != nil {
		t.Fatal(err)
	}
	now, err := time.Parse(time.RFC3339Nano, got.Now)
	if err != nil || time.Since(now) > time.Second || got.Elapsed < 2 || got.Deadline != nil {
		t.Fatalf("bad clock: %s", res.Output)
	}
}
