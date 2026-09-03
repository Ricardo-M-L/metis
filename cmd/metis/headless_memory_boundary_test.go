package main

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/internal/agent"
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
