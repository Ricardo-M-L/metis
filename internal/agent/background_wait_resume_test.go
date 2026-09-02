package agent

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/internal/jobs"
	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

type awaitedNotificationTool struct{ tools.BaseTool }

func (awaitedNotificationTool) Name() string        { return "AwaitedJob" }
func (awaitedNotificationTool) Description() string { return "start an awaited background job" }
func (awaitedNotificationTool) InputSchema() map[string]any {
	return map[string]any{"type": "object"}
}

type immediatelyCompletingAwaitedTool struct {
	tools.BaseTool
	notify chan<- jobs.Notification
}

func (immediatelyCompletingAwaitedTool) Name() string { return "ImmediateAwaitedJob" }
func (immediatelyCompletingAwaitedTool) Description() string {
	return "start an awaited background job that has already completed"
}
func (immediatelyCompletingAwaitedTool) InputSchema() map[string]any {
	return map[string]any{"type": "object"}
}
func (immediatelyCompletingAwaitedTool) Concurrency(map[string]any) tools.Concurrency {
	return tools.ConcurrencyExclusive
}
func (immediatelyCompletingAwaitedTool) CanUse(context.Context, map[string]any) (tools.Permission, string) {
	return tools.PermissionAllow, ""
}
func (t immediatelyCompletingAwaitedTool) Execute(context.Context, map[string]any) (*tools.Result, error) {
	t.notify <- jobs.Notification{
		JobID:    "bg_immediate_test",
		Status:   jobs.StatusCompleted,
		ExitCode: 0,
		Elapsed:  3 * time.Second,
		Command:  "sleep 3; printf IMMEDIATE_MARKER",
	}
	return &tools.Result{
		Output: "[command running in background, job_id=bg_immediate_test]",
		Presentation: map[string]any{
			"kind":             "background_job",
			"job_id":           "bg_immediate_test",
			"await_completion": true,
		},
	}, nil
}
func (awaitedNotificationTool) Concurrency(map[string]any) tools.Concurrency {
	return tools.ConcurrencyExclusive
}
func (awaitedNotificationTool) CanUse(context.Context, map[string]any) (tools.Permission, string) {
	return tools.PermissionAllow, ""
}
func (awaitedNotificationTool) Execute(context.Context, map[string]any) (*tools.Result, error) {
	return &tools.Result{
		Output: "[command running in background, job_id=bg_wait_test]",
		Presentation: map[string]any{
			"kind":             "background_job",
			"job_id":           "bg_wait_test",
			"await_completion": true,
		},
	}, nil
}

// notifyingStream publishes a completion while the model's "waiting" reply
// is being consumed. The notification therefore arrives after the iteration's
// initial drain but before the loop considers end_turn, reproducing the real
// headless metis run race without wall-clock sleeps.
type notifyingStream struct {
	inner        llm.StreamReader
	notify       chan<- jobs.Notification
	notification jobs.Notification
	once         sync.Once
}

func (s *notifyingStream) Recv() (llm.StreamEvent, error) {
	s.once.Do(func() {
		notification := s.notification
		if notification.JobID == "" {
			notification = jobs.Notification{
				JobID:    "bg_wait_test",
				Status:   jobs.StatusCompleted,
				ExitCode: 0,
				Elapsed:  3 * time.Second,
				Command:  "sleep 3; printf METIS_WAIT_MARKER",
			}
		}
		s.notify <- notification
	})
	return s.inner.Recv()
}

func (s *notifyingStream) Close() error { return s.inner.Close() }

func TestLoopWaitsForAwaitedBackgroundJobNotificationBeforeEnding(t *testing.T) {
	notify := make(chan jobs.Notification, 4)
	provider := &queuedStreamProvider{streams: []llm.StreamReader{
		&mockStream{events: toolBatchEvents(llm.ContentBlock{
			Type: "tool_use", ToolUseID: "wait-tool-1", ToolName: "AwaitedJob",
		})},
		&notifyingStream{inner: textStream("waiting for the completion notification"), notify: notify},
		textStream("FINAL_METIS_WAIT_OK"),
	}}
	registry := tools.NewRegistry()
	registry.Register(awaitedNotificationTool{})
	loop := NewLoop(provider, registry, permission.New(permission.ModeBypassPermissions), nil, "system", 6)
	loop.JobNotify = notify
	loop.AppendUser("wait for the background marker, then finish")

	out := make(chan Event, 64)
	if err := loop.Run(context.Background(), out); err != nil {
		t.Fatalf("Run: %v", err)
	}
	close(out)

	requests := provider.capturedRequests()
	if len(requests) != 3 {
		t.Fatalf("provider calls = %d, want tool + waiting + notification continuation", len(requests))
	}
	if !requestContains(requests[2], "<job_notification>") ||
		!requestContains(requests[2], "bg_wait_test") {
		t.Fatal("continuation request did not contain the awaited job notification")
	}
	if got := assistantText(filterAssistantBlocks(loop.History())); !strings.Contains(got, "FINAL_METIS_WAIT_OK") {
		t.Fatalf("history missing final continuation marker: %q", got)
	}
}

func TestLoopAwaitedBackgroundJobHonorsTurnWallClockDeadline(t *testing.T) {
	t.Setenv("METIS_TURN_MAX_SECONDS", "1")

	notify := make(chan jobs.Notification, 4)
	unrelated := jobs.Notification{
		JobID: "bg_unrelated_deadline", Status: jobs.StatusCompleted,
		Command: "printf UNRELATED_DEADLINE_MARKER",
	}
	provider := &queuedStreamProvider{streams: []llm.StreamReader{
		&mockStream{events: toolBatchEvents(llm.ContentBlock{
			Type: "tool_use", ToolUseID: "wait-tool-deadline", ToolName: "AwaitedJob",
		})},
		&notifyingStream{
			inner:        textStream("waiting for a job that never finishes"),
			notify:       notify,
			notification: unrelated,
		},
	}}
	registry := tools.NewRegistry()
	registry.Register(awaitedNotificationTool{})
	loop := NewLoop(provider, registry, permission.New(permission.ModeBypassPermissions), nil, "system", 6)
	loop.JobNotify = notify
	loop.AppendUser("wait for the background job")

	// The outer deadline only prevents a broken implementation from hanging the
	// test forever. The Run-local one-second wall-clock limit must win first and
	// terminate through the normal turn_wall_clock path.
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	out := make(chan Event, 64)
	if err := loop.Run(ctx, out); err != nil {
		t.Fatalf("Run: %v", err)
	}
	close(out)

	var stopReason string
	var sawInjectionEvent bool
	for event := range out {
		if event.Kind == EventLoopDone {
			stopReason = event.StopReason
		}
		if event.Kind == EventInfo && strings.Contains(event.Info, "notification(s) injected") {
			sawInjectionEvent = true
		}
	}
	if stopReason != "turn_wall_clock" {
		t.Fatalf("stop reason = %q, want turn_wall_clock", stopReason)
	}
	if !sawInjectionEvent {
		t.Fatal("missing injected-notification EventInfo before turn deadline")
	}
	if !historyContainsText(loop.History(), unrelated.JobID) {
		t.Fatalf("history lost unrelated notification %s at turn deadline", unrelated.JobID)
	}
}

func TestLoopInjectsAwaitedNotificationThatCompletedBeforeNextIteration(t *testing.T) {
	notify := make(chan jobs.Notification, 4)
	provider := &queuedStreamProvider{streams: []llm.StreamReader{
		&mockStream{events: toolBatchEvents(llm.ContentBlock{
			Type: "tool_use", ToolUseID: "immediate-tool-1", ToolName: "ImmediateAwaitedJob",
		})},
		textStream("FINAL_IMMEDIATE_WAIT_OK"),
	}}
	registry := tools.NewRegistry()
	registry.Register(immediatelyCompletingAwaitedTool{notify: notify})
	loop := NewLoop(provider, registry, permission.New(permission.ModeBypassPermissions), nil, "system", 4)
	loop.JobNotify = notify
	loop.AppendUser("finish after the background notification")

	out := make(chan Event, 32)
	if err := loop.Run(context.Background(), out); err != nil {
		t.Fatalf("Run: %v", err)
	}
	close(out)

	requests := provider.capturedRequests()
	if len(requests) != 2 {
		t.Fatalf("provider calls = %d, want tool + notification continuation", len(requests))
	}
	if !requestContains(requests[1], "<job_notification>") ||
		!requestContains(requests[1], "bg_immediate_test") {
		t.Fatal("next request did not contain the already-completed awaited notification")
	}
}

func TestOrdinaryBackgroundJobDoesNotKeepTurnOpen(t *testing.T) {
	pending := map[string]struct{}{}
	recordAwaitedBackgroundJobs([]llm.ContentBlock{{
		Type: "tool_result",
		Presentation: map[string]any{
			"kind":   "background_job",
			"job_id": "bg_server",
		},
	}}, pending)
	if len(pending) != 0 {
		t.Fatalf("ordinary background server was marked awaited: %#v", pending)
	}
}

func TestAwaitedBackgroundWaitHonorsCancellation(t *testing.T) {
	loop := &Loop{JobNotify: make(chan jobs.Notification)}
	pending := map[string]struct{}{"bg_never_finishes": {}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	started := time.Now()
	completed, err := loop.waitForAwaitedJobNotifications(ctx, nil, pending)
	if completed {
		t.Fatal("cancelled wait unexpectedly reported a completion")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled wait error = %v, want context.Canceled", err)
	}
	if elapsed := time.Since(started); elapsed > 100*time.Millisecond {
		t.Fatalf("cancelled wait returned too slowly: %s", elapsed)
	}
}

func TestAwaitedBackgroundWaitFlushesCollectedNotificationOnCancellation(t *testing.T) {
	notify := make(chan jobs.Notification)
	out := make(chan Event, 8)
	loop := &Loop{JobNotify: notify}
	pending := map[string]struct{}{"bg_awaited_a": {}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	type waitResult struct {
		completed bool
		err       error
	}
	done := make(chan waitResult, 1)
	go func() {
		completed, err := loop.waitForAwaitedJobNotifications(ctx, out, pending)
		done <- waitResult{completed: completed, err: err}
	}()

	notify <- jobs.Notification{
		JobID: "bg_unrelated_b", Status: jobs.StatusCompleted,
		Command: "printf UNRELATED_CANCEL_MARKER",
	}
	cancel()
	result := <-done
	if result.completed || !errors.Is(result.err, context.Canceled) {
		t.Fatalf("wait result = (%v, %v), want (false, context.Canceled)", result.completed, result.err)
	}
	if _, ok := pending["bg_awaited_a"]; !ok || len(pending) != 1 {
		t.Fatalf("awaited job A was incorrectly cleared: %#v", pending)
	}
	if !historyContainsText(loop.History(), "bg_unrelated_b") {
		t.Fatal("history lost unrelated notification B on cancellation")
	}
	select {
	case event := <-out:
		if event.Kind != EventInfo || !strings.Contains(event.Info, "notification(s) injected") {
			t.Fatalf("injection event = %#v", event)
		}
	default:
		t.Fatal("missing injected-notification EventInfo on cancellation")
	}
}

func TestAwaitedBackgroundWaitRecoversDroppedCompletionFromRegistry(t *testing.T) {
	pool := jobs.NewRegistryBuffered(t.TempDir(), 1)
	t.Cleanup(func() { pool.Shutdown(0) })

	spawn := func(command string) *jobs.Job {
		t.Helper()
		ctx, cancel := context.WithCancel(context.Background())
		job, err := pool.Spawn(jobs.SpawnArgs{
			Command: command,
			Cmd:     exec.CommandContext(ctx, "sh", "-c", command),
			Cancel:  cancel,
		})
		if err != nil {
			t.Fatalf("spawn %q: %v", command, err)
		}
		return job
	}
	waitTerminal := func(jobID string) jobs.Job {
		t.Helper()
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			job, ok := pool.Get(jobID)
			if ok && job.Status != jobs.StatusRunning {
				return job
			}
			time.Sleep(5 * time.Millisecond)
		}
		t.Fatalf("job %s did not become terminal", jobID)
		return jobs.Job{}
	}

	// Fill the one-slot notification buffer first. The awaited job then
	// completes while the channel is full, so Registry.publish deliberately
	// drops its edge notification while retaining the terminal Job state.
	filler := spawn("printf FILLER_NOTIFICATION")
	waitTerminal(filler.ID)
	if got := len(pool.Notify()); got != 1 {
		t.Fatalf("notification buffer length = %d, want full buffer", got)
	}
	awaited := spawn("printf DROPPED_WAIT_MARKER")
	waitTerminal(awaited.ID)
	if got := len(pool.Notify()); got != 1 {
		t.Fatalf("awaited completion unexpectedly entered full buffer; len=%d", got)
	}

	loop := &Loop{JobNotify: pool.Notify(), Jobs: pool}
	pending := map[string]struct{}{awaited.ID: {}}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	started := time.Now()
	completed, err := loop.waitForAwaitedJobNotifications(ctx, nil, pending)
	if err != nil {
		t.Fatalf("wait: %v", err)
	}
	if !completed {
		t.Fatal("wait did not recover the dropped completion from Registry state")
	}
	if elapsed := time.Since(started); elapsed > 200*time.Millisecond {
		t.Fatalf("Registry recovery took %s", elapsed)
	}
	if len(pending) != 0 {
		t.Fatalf("awaited job remained pending: %#v", pending)
	}

	history := loop.History()
	if len(history) != 1 || len(history[0].Content) != 1 {
		t.Fatalf("notification history = %#v", history)
	}
	body := history[0].Content[0].Text
	if !strings.Contains(body, awaited.ID) || !strings.Contains(body, "DROPPED_WAIT_MARKER") {
		t.Fatalf("recovered notification omitted awaited job/output: %s", body)
	}
}

func filterAssistantBlocks(messages []llm.Message) []llm.ContentBlock {
	var blocks []llm.ContentBlock
	for _, message := range messages {
		if message.Role == llm.RoleAssistant {
			blocks = append(blocks, message.Content...)
		}
	}
	return blocks
}

func historyContainsText(messages []llm.Message, needle string) bool {
	for _, message := range messages {
		for _, block := range message.Content {
			if strings.Contains(block.Text, needle) {
				return true
			}
		}
	}
	return false
}
