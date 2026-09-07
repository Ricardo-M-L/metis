package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/config"
	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/memory"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

type headlessBoundaryStream struct {
	events []llm.StreamEvent
	index  int
}

func (s *headlessBoundaryStream) Recv() (llm.StreamEvent, error) {
	if s.index >= len(s.events) {
		return llm.StreamEvent{}, io.EOF
	}
	event := s.events[s.index]
	s.index++
	return event, nil
}

func (*headlessBoundaryStream) Close() error { return nil }

type headlessBoundaryProvider struct{}

func (*headlessBoundaryProvider) Name() string          { return "headless-boundary" }
func (*headlessBoundaryProvider) ModelID() string       { return "headless-boundary-model" }
func (*headlessBoundaryProvider) MaxContextTokens() int { return 200_000 }
func (*headlessBoundaryProvider) Complete(context.Context, llm.Request) (*llm.Response, error) {
	return nil, errors.New("headless boundary provider only supports Stream")
}
func (*headlessBoundaryProvider) Stream(context.Context, llm.Request) (llm.StreamReader, error) {
	return &headlessBoundaryStream{events: []llm.StreamEvent{
		{Type: "text_delta", TextDelta: "The durable release codename is White Finch."},
		{Type: "message_delta", StopReason: "end_turn"},
		{Type: "message_stop"},
	}}, nil
}

type headlessBoundaryRepository struct {
	memory.Repository

	mu             sync.Mutex
	recorded       []string
	distilled      []string
	distillStarted chan struct{}
}

func (r *headlessBoundaryRepository) RecordTurn(
	_ context.Context,
	sessionID, _, _, _ string,
) error {
	r.mu.Lock()
	r.recorded = append(r.recorded, sessionID)
	r.mu.Unlock()
	return nil
}

func (r *headlessBoundaryRepository) DistillTurnWithMetadata(
	_ context.Context,
	_ llm.Provider,
	sessionID, _, _, _ string,
) error {
	r.mu.Lock()
	r.distilled = append(r.distilled, sessionID)
	r.mu.Unlock()
	select {
	case r.distillStarted <- struct{}{}:
	default:
	}
	return nil
}

func (r *headlessBoundaryRepository) snapshot() (recorded, distilled []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.recorded...), append([]string(nil), r.distilled...)
}

func TestSuccessfulHeadlessOneTurnFlushesResidualDistillation(t *testing.T) {
	const sessionID = "run-session-white-finch"
	manager, err := memory.NewMemoryManager(t.TempDir())
	if err != nil {
		t.Fatalf("memory manager: %v", err)
	}
	repository := &headlessBoundaryRepository{
		Repository:     manager,
		distillStarted: make(chan struct{}, 1),
	}
	loop := agent.NewLoop(&headlessBoundaryProvider{}, tools.NewRegistry(), nil, nil, "system", 3)
	loop.Memory = repository
	loop.DistillEvery = 5 // one successful turn stays residual until a boundary flush
	loop.CurrentStateSnapshot = func() agent.RuntimeStateSnapshot {
		return agent.RuntimeStateSnapshot{SessionID: sessionID}
	}
	loop.AppendUser("Remember that the durable release codename is White Finch.")

	events := make(chan agent.Event, 16)
	done := make(chan error, 1)
	go func() {
		done <- loop.Run(context.Background(), events)
		close(events)
	}()
	for range events {
	}
	if err := <-done; err != nil {
		t.Fatalf("one-turn loop: %v", err)
	}

	select {
	case <-repository.distillStarted:
		t.Fatal("turn reached distillation before the headless boundary")
	default:
	}

	rt := &runtime{loop: loop, sessionID: sessionID}
	if err := rt.persistHeadlessMemoryBoundary("metis run", time.Second); err != nil {
		t.Fatalf("persist headless boundary: %v", err)
	}

	recorded, distilled := repository.snapshot()
	if len(recorded) != 1 || recorded[0] != sessionID {
		t.Fatalf("recorded sessions = %v, want [%s]", recorded, sessionID)
	}
	if len(distilled) != 1 || distilled[0] != sessionID {
		t.Fatalf("distilled sessions = %v, want exactly [%s]", distilled, sessionID)
	}
}

type recordingHeadlessBoundary struct {
	mu       sync.Mutex
	calls    []string
	waitFunc func(context.Context, string) error
}

func (b *recordingHeadlessBoundary) FlushPendingDistillation(sessionID string) int {
	b.mu.Lock()
	b.calls = append(b.calls, "flush:"+sessionID)
	b.mu.Unlock()
	return 1
}

func (b *recordingHeadlessBoundary) WaitForDistillation(ctx context.Context, sessionID string) error {
	b.mu.Lock()
	b.calls = append(b.calls, "wait:"+sessionID)
	b.mu.Unlock()
	if b.waitFunc != nil {
		return b.waitFunc(ctx, sessionID)
	}
	return nil
}

func (b *recordingHeadlessBoundary) CancelDistillation(sessionID string) {
	b.mu.Lock()
	b.calls = append(b.calls, "cancel:"+sessionID)
	b.mu.Unlock()
}

func (b *recordingHeadlessBoundary) snapshot() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.calls...)
}

func TestPersistHeadlessMemoryBoundaryOrdersAndScopesMCPRequest(t *testing.T) {
	boundary := &recordingHeadlessBoundary{}
	if err := persistHeadlessMemoryBoundary(boundary, "mcp-request-b", "mcp run_task", time.Second); err != nil {
		t.Fatal(err)
	}
	want := []string{"flush:mcp-request-b", "wait:mcp-request-b"}
	got := boundary.snapshot()
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("boundary calls = %v, want %v", got, want)
	}
}

func TestPersistHeadlessMemoryBoundaryBoundsWait(t *testing.T) {
	boundary := &recordingHeadlessBoundary{waitFunc: func(ctx context.Context, _ string) error {
		<-ctx.Done()
		return ctx.Err()
	}}
	start := time.Now()
	err := persistHeadlessMemoryBoundary(boundary, "daemon-session", "daemon task", 20*time.Millisecond)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want deadline exceeded", err)
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("bounded wait took %s", elapsed)
	}
}

func TestCollectHeadlessEventsClosesAfterRunnerCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan struct{})
	type result struct {
		text string
		err  error
	}
	done := make(chan result, 1)
	go func() {
		text, err := collectHeadlessEvents(func(events chan<- agent.Event) error {
			events <- agent.Event{Kind: agent.EventTextDelta, TextDelta: "partial"}
			close(started)
			<-ctx.Done()
			return ctx.Err()
		})
		done <- result{text: text, err: err}
	}()

	<-started
	cancel()
	select {
	case got := <-done:
		if !errors.Is(got.err, context.Canceled) {
			t.Fatalf("error = %v, want context canceled", got.err)
		}
		if got.text != "partial" {
			t.Fatalf("text = %q, want partial", got.text)
		}
	case <-time.After(time.Second):
		t.Fatal("headless event collector remained blocked after runner cancellation")
	}
}

func TestCollectHeadlessEventsRejectsIncompleteTerminal(t *testing.T) {
	text, err := collectHeadlessEvents(func(events chan<- agent.Event) error {
		events <- agent.Event{Kind: agent.EventTextDelta, TextDelta: "partial"}
		events <- agent.Event{Kind: agent.EventLoopDone, StopReason: "max_tokens"}
		return nil
	})
	if text != "partial" {
		t.Fatalf("text = %q, want partial", text)
	}
	if err == nil || !strings.Contains(err.Error(), "max_tokens") {
		t.Fatalf("error = %v, want max_tokens incomplete", err)
	}
}

func TestSuccessfulHeadlessTaskDoesNotFailForOptionalMemoryProvider(t *testing.T) {
	const sessionID = "completed-before-memory-provider-error"
	manager, err := memory.NewMemoryManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	// Stream completes the user's task; Complete fails only when the separate
	// optional fact-extraction call runs at the success boundary.
	loop := agent.NewLoop(&headlessBoundaryProvider{}, tools.NewRegistry(), nil, nil, "system", 3)
	loop.Memory = manager
	loop.DistillEvery = 5
	loop.CurrentStateSnapshot = func() agent.RuntimeStateSnapshot {
		return agent.RuntimeStateSnapshot{SessionID: sessionID}
	}
	rt := &runtime{loop: loop, sessionID: sessionID}
	text, err := runHeadlessOneShot(context.Background(), rt,
		"Remember that the durable release codename is White Finch.", "metis daemon task")
	if err != nil {
		t.Fatalf("completed task was failed by optional memory extraction: %v", err)
	}
	if !strings.Contains(text, "White Finch") {
		t.Fatalf("completed task response was lost: %q", text)
	}
}

func TestCompleteHeadlessMemoryBoundaryClassifiesOnlyOptionalErrors(t *testing.T) {
	providerErr := &memory.DistillationProviderError{Err: errors.New("server_is_overloaded private-provider-token")}
	storageErr := &os.PathError{Op: "write", Path: "/private/memory", Err: errors.New("disk full")}
	for _, tc := range []struct {
		name     string
		err      error
		optional bool
	}{
		{"success", nil, false},
		{"provider", providerErr, true},
		{"wrapped provider", fmt.Errorf("wrapped: %w", providerErr), true},
		{"provider timeout", &memory.DistillationProviderError{Err: context.DeadlineExceeded}, true},
		{"provider cancellation", &memory.DistillationProviderError{Err: context.Canceled}, true},
		{"multiple providers", errors.Join(providerErr, providerErr), true},
		{"storage", storageErr, false},
		{"mixed", errors.Join(providerErr, storageErr), false},
		{"nested mixed", errors.Join(providerErr, errors.Join(providerErr, storageErr)), false},
		{"storage deadline", errors.Join(context.DeadlineExceeded), false},
		{"unclassified deadline", context.DeadlineExceeded, false},
		{"unclassified cancellation", context.Canceled, false},
		{"untyped provider-looking error", errors.New("distill: provider error: not a marker"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			boundary := &recordingHeadlessBoundary{waitFunc: func(context.Context, string) error { return tc.err }}
			var warnings bytes.Buffer
			err := completeHeadlessMemoryBoundary(boundary, "private-session", "unknown private-source", time.Second, &warnings)
			if tc.optional {
				if err != nil {
					t.Fatalf("optional enrichment failed task: %v", err)
				}
				var got map[string]string
				if err := json.Unmarshal(warnings.Bytes(), &got); err != nil {
					t.Fatalf("structured warning: %v", err)
				}
				if got["kind"] != "memory_enrichment_warning" || got["task_status"] != "completed" || got["memory_status"] != "incomplete" || got["reason"] != "provider_error" || got["source"] != "headless" {
					t.Fatalf("warning = %v", got)
				}
				for _, secret := range []string{"private-provider-token", "private-session", "private-source"} {
					if strings.Contains(warnings.String(), secret) {
						t.Fatalf("warning leaked %q", secret)
					}
				}
			} else {
				if !errors.Is(err, tc.err) {
					t.Fatalf("error = %v, want preserved %v", err, tc.err)
				}
				if warnings.Len() != 0 {
					t.Fatalf("nonoptional outcome emitted success warning: %s", warnings.String())
				}
			}
			if got := boundary.snapshot(); len(got) != 2 {
				t.Fatalf("unexpected retry/cancel: %v", got)
			}
		})
	}
}

func TestCompleteHeadlessMemoryBoundaryTimeoutCancelsOwningSession(t *testing.T) {
	boundary := &recordingHeadlessBoundary{waitFunc: func(ctx context.Context, _ string) error {
		<-ctx.Done()
		return ctx.Err()
	}}
	var warnings bytes.Buffer
	start := time.Now()
	if err := completeHeadlessMemoryBoundary(boundary, "owned-session", "metis run", 20*time.Millisecond, &warnings); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("bounded join took %v", elapsed)
	}
	if got := boundary.snapshot(); len(got) != 3 || got[2] != "cancel:owned-session" {
		t.Fatalf("calls = %v", got)
	}
	var got map[string]string
	if err := json.Unmarshal(warnings.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got["reason"] != "join_timeout" || got["task_status"] != "completed" || got["memory_status"] != "incomplete" {
		t.Fatalf("warning = %v", got)
	}
}

func TestCompleteHeadlessMemoryBoundaryTimeoutPreservesCompletedErrors(t *testing.T) {
	providerErr := &memory.DistillationProviderError{Err: errors.New("server_is_overloaded")}
	storageErr := &os.PathError{Op: "write", Path: "/private/memory", Err: errors.New("disk full")}
	for _, tc := range []struct {
		name      string
		completed error
		optional  bool
	}{
		{"provider and local timeout", providerErr, true},
		{"storage and local timeout", storageErr, false},
		{"mixed and local timeout", errors.Join(providerErr, storageErr), false},
		{"storage deadline and local timeout", context.DeadlineExceeded, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			boundary := &recordingHeadlessBoundary{waitFunc: func(ctx context.Context, _ string) error {
				<-ctx.Done()
				return &agent.DistillationWaitError{WaitErr: ctx.Err(), CompletedErrors: tc.completed}
			}}
			var warnings bytes.Buffer
			err := completeHeadlessMemoryBoundary(boundary, "owned-session", "metis run", 5*time.Millisecond, &warnings)
			if tc.optional {
				if err != nil || !strings.Contains(warnings.String(), `"reason":"join_timeout"`) {
					t.Fatalf("optional timeout outcome: error=%v warning=%s", err, warnings.String())
				}
			} else {
				if !errors.Is(err, tc.completed) || !errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("completed error lost behind timeout: %v", err)
				}
				if warnings.Len() != 0 {
					t.Fatalf("storage failure emitted success warning: %s", warnings.String())
				}
			}
			if got := boundary.snapshot(); len(got) != 3 || got[2] != "cancel:owned-session" {
				t.Fatalf("calls = %v", got)
			}
		})
	}
	t.Run("untagged completed error stays fatal even if local context also expires", func(t *testing.T) {
		boundary := &recordingHeadlessBoundary{waitFunc: func(ctx context.Context, _ string) error {
			<-ctx.Done()
			return errors.Join(context.DeadlineExceeded)
		}}
		var warnings bytes.Buffer
		err := completeHeadlessMemoryBoundary(boundary, "owned-session", "metis run", 5*time.Millisecond, &warnings)
		if !errors.Is(err, context.DeadlineExceeded) || warnings.Len() != 0 {
			t.Fatalf("unproven timeout downgraded: %v, %s", err, warnings.String())
		}
	})
}

type brokenMemoryWarningWriter struct{}

func (brokenMemoryWarningWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

func TestCompleteHeadlessMemoryBoundaryDoesNotSilentlyLoseWarning(t *testing.T) {
	boundary := &recordingHeadlessBoundary{waitFunc: func(context.Context, string) error {
		return &memory.DistillationProviderError{Err: errors.New("overloaded")}
	}}
	err := completeHeadlessMemoryBoundary(boundary, "owned", "metis run", time.Second, brokenMemoryWarningWriter{})
	if !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("error = %v, want warning write failure", err)
	}
}

type headlessOutcomeProvider struct {
	headlessBoundaryProvider
	taskErr       error
	stopReason    string
	completeFunc  func(context.Context) error
	completeCalls atomic.Int32
}

func (p *headlessOutcomeProvider) Stream(ctx context.Context, req llm.Request) (llm.StreamReader, error) {
	if p.taskErr != nil {
		return nil, p.taskErr
	}
	if p.stopReason != "" {
		return &headlessBoundaryStream{events: []llm.StreamEvent{
			{Type: "text_delta", TextDelta: "The partial response is not completed."},
			{Type: "message_delta", StopReason: p.stopReason},
			{Type: "message_stop"},
		}}, nil
	}
	return p.headlessBoundaryProvider.Stream(ctx, req)
}

func (p *headlessOutcomeProvider) Complete(ctx context.Context, _ llm.Request) (*llm.Response, error) {
	p.completeCalls.Add(1)
	if p.completeFunc != nil {
		return nil, p.completeFunc(ctx)
	}
	return nil, errors.New("server_is_overloaded")
}

func TestHeadlessMemoryJoinTimeoutStillAllowsBoundedWorkerCleanup(t *testing.T) {
	provider := &headlessOutcomeProvider{completeFunc: func(ctx context.Context) error {
		<-ctx.Done()
		return ctx.Err()
	}}
	rt := newHeadlessOutcomeRuntime(t, provider)
	rt.loop.AppendUser("Remember that the durable release codename is White Finch.")
	if _, err := collectHeadlessEvents(func(events chan<- agent.Event) error {
		return rt.loop.Run(context.Background(), events)
	}); err != nil {
		t.Fatal(err)
	}
	var warnings bytes.Buffer
	if err := completeHeadlessMemoryBoundary(rt.loop, rt.sessionID, "metis daemon task", 20*time.Millisecond, &warnings); err != nil {
		t.Fatal(err)
	}
	cleanupCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := rt.loop.CancelAndWaitForDistillation(cleanupCtx, rt.sessionID); err != nil {
		t.Fatalf("worker did not stop at cleanup: %v", err)
	}
	if got := rt.loop.FlushPendingDistillation(rt.sessionID); got != 0 {
		t.Fatalf("cleanup retained %d pending jobs", got)
	}
	if calls := provider.completeCalls.Load(); calls != 1 {
		t.Fatalf("provider calls = %d, want exactly 1", calls)
	}
	if !strings.Contains(warnings.String(), `"reason":"join_timeout"`) {
		t.Fatalf("warning = %s", warnings.String())
	}
}

func newHeadlessOutcomeRuntime(t *testing.T, provider llm.Provider) *runtime {
	t.Helper()
	manager, err := memory.NewMemoryManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	loop := agent.NewLoop(provider, tools.NewRegistry(), nil, nil, "system", 3)
	loop.Memory = manager
	loop.DistillEvery = 5
	const sessionID = "headless-outcome"
	loop.CurrentStateSnapshot = func() agent.RuntimeStateSnapshot { return agent.RuntimeStateSnapshot{SessionID: sessionID} }
	return &runtime{loop: loop, sessionID: sessionID, cfg: &config.Config{Session: config.Session{Dir: t.TempDir()}}}
}

func TestHeadlessSuccessRoutesKeepCompletedTaskWhenMemoryProviderFails(t *testing.T) {
	t.Setenv("METIS_NOTIFY_CHANNEL", "off")
	const prompt = "Remember that the durable release codename is White Finch."
	for _, route := range []string{"run boundary", "mcp boundary", "daemon", "coordinator", "cron"} {
		t.Run(route, func(t *testing.T) {
			provider := &headlessOutcomeProvider{}
			rt := newHeadlessOutcomeRuntime(t, provider)
			var text string
			var err error
			switch route {
			case "cron":
				history := map[string][]llm.Message{}
				job := &agent.CronJob{ID: "memory-warning-job", Prompt: prompt, Silent: true, SessionMode: agent.SessionModePersistent}
				err = executeCronJob(context.Background(), rt, job, history, map[string][]llm.Message{})
				if len(history[job.ID]) == 0 {
					t.Fatal("successful cron history was discarded")
				}
			case "coordinator":
				text, err = runOneShotForCoordinator(context.Background(), rt, prompt)
			case "daemon":
				text, err = runHeadlessOneShot(context.Background(), rt, prompt, "metis daemon task")
			default:
				// Main and MCP have runtime setup around this success seam. Keep
				// their production checkpoint defers outside optional handling.
				rt.loop.AppendUser(prompt)
				text, err = collectHeadlessEvents(func(events chan<- agent.Event) error { return rt.loop.Run(context.Background(), events) })
				if err == nil {
					source := "metis run"
					if route == "mcp boundary" {
						source = "metis mcp-serve run_task"
					}
					err = rt.persistHeadlessMemoryBoundary(source, time.Second)
				}
			}
			if err != nil {
				t.Fatalf("completed %s task failed: %v", route, err)
			}
			if route != "cron" && !strings.Contains(text, "White Finch") {
				t.Fatalf("completed response lost: %q", text)
			}
			if calls := provider.completeCalls.Load(); calls != 1 {
				t.Fatalf("Complete calls = %d, want 1 (no extra retries)", calls)
			}
			if err := rt.loop.CancelAndWaitForDistillation(context.Background(), rt.sessionID); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestHeadlessTaskFailuresDoNotBecomeMemoryWarnings(t *testing.T) {
	for _, tc := range []struct {
		name       string
		taskErr    error
		stopReason string
	}{
		{"main error", errors.New("main task failed"), ""},
		{"main cancellation", context.Canceled, ""},
		{"main deadline", context.DeadlineExceeded, ""},
		{"incomplete", nil, "max_tokens"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			provider := &headlessOutcomeProvider{taskErr: tc.taskErr, stopReason: tc.stopReason}
			rt := newHeadlessOutcomeRuntime(t, provider)
			_, err := runHeadlessOneShot(context.Background(), rt, "Please run this task to completion.", "metis daemon task")
			if err == nil {
				t.Fatal("failed task returned success")
			}
			if tc.taskErr != nil && !errors.Is(err, tc.taskErr) {
				t.Fatalf("error = %v, want %v", err, tc.taskErr)
			}
			if calls := provider.completeCalls.Load(); calls != 0 {
				t.Fatalf("failed task launched %d memory calls", calls)
			}
		})
	}
}
