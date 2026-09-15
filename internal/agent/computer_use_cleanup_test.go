package agent

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

type cuCleanupProvider struct {
	stream func(context.Context) (llm.StreamReader, error)
}

func (*cuCleanupProvider) Name() string          { return "cu-cleanup-test" }
func (*cuCleanupProvider) ModelID() string       { return "test-model" }
func (*cuCleanupProvider) MaxContextTokens() int { return 200_000 }
func (*cuCleanupProvider) Complete(context.Context, llm.Request) (*llm.Response, error) {
	return nil, errors.New("unexpected non-streaming completion")
}
func (p *cuCleanupProvider) Stream(ctx context.Context, _ llm.Request) (llm.StreamReader, error) {
	return p.stream(ctx)
}

func TestLoopComputerUseCleanupRunsOnSuccessAndProviderError(t *testing.T) {
	providerFailure := errors.New("401 unauthorized test provider")
	for _, wantErr := range []error{nil, providerFailure} {
		name := "success"
		if wantErr != nil {
			name = "provider-error"
		}
		t.Run(name, func(t *testing.T) {
			provider := &cuCleanupProvider{stream: func(context.Context) (llm.StreamReader, error) {
				if wantErr != nil {
					return nil, wantErr
				}
				return textStream("done"), nil
			}}
			loop := NewLoop(provider, tools.NewRegistry(), permission.New(permission.ModeAcceptEdits), nil, "sys", 10)
			var finished atomic.Int32
			loop.OnRunFinished = func() { finished.Add(1) }
			for run := 1; run <= 2; run++ {
				loop.AppendUser("finish this turn")
				err := loop.Run(context.Background(), make(chan Event, 128))
				if !errors.Is(err, wantErr) {
					t.Fatalf("Run error=%v, want %v", err, wantErr)
				}
				if got := finished.Load(); got != int32(run) {
					t.Fatalf("cleanup calls after run %d = %d", run, got)
				}
			}
		})
	}
}

func TestLoopComputerUseCleanupRunsAfterProviderCancellation(t *testing.T) {
	started := make(chan struct{})
	provider := &cuCleanupProvider{stream: func(ctx context.Context) (llm.StreamReader, error) {
		close(started)
		<-ctx.Done()
		return nil, ctx.Err()
	}}
	loop := NewLoop(provider, tools.NewRegistry(), permission.New(permission.ModeAcceptEdits), nil, "sys", 10)
	loop.AppendUser("perform a cancellable turn")
	var finished atomic.Int32
	loop.OnRunFinished = func() { finished.Add(1) }
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	result := make(chan error, 1)
	go func() { result <- loop.Run(ctx, make(chan Event, 128)) }()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("provider did not start")
	}
	if finished.Load() != 0 {
		t.Fatal("cleanup ran while the provider was still active")
	}
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) || finished.Load() != 1 {
			t.Fatalf("cancelled turn: err=%v cleanup=%d", err, finished.Load())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cancelled turn did not complete cleanup")
	}
}

func TestLoopComputerUseCleanupWithPendingSteerAndAbandonedConsumer(t *testing.T) {
	started := make(chan struct{})
	provider := &cuCleanupProvider{stream: func(ctx context.Context) (llm.StreamReader, error) {
		close(started)
		<-ctx.Done()
		return nil, ctx.Err()
	}}
	loop := NewLoop(provider, tools.NewRegistry(), permission.New(permission.ModeAcceptEdits), nil, "sys", 10)
	loop.AppendUser("perform a cancellable turn")
	var finished atomic.Int32
	loop.OnRunFinished = func() { finished.Add(1) }
	ctx, cancel := context.WithCancel(context.Background())
	out := make(chan Event, 128)
	result := make(chan error, 1)
	done := make(chan struct{})
	go func() {
		result <- loop.Run(ctx, out)
		close(done)
	}()
	// Drain only during failure cleanup, so an implementation that blocks in a
	// deferred output send cannot leave its test goroutine behind.
	t.Cleanup(func() {
		cancel()
		timeout := time.NewTimer(time.Second)
		defer timeout.Stop()
		for {
			select {
			case <-done:
				return
			default:
			}
			select {
			case <-done:
				return
			case <-out:
			case <-timeout.C:
				return
			}
		}
	})
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("provider did not start")
	}
	// The consumer has gone away while Stream is still in flight. Fill every
	// remaining slot without receiving any events, then queue a steering message
	// that only Run's shutdown defer can discard.
fillOutput:
	for {
		select {
		case out <- Event{Kind: EventInfo, Info: "unread event"}:
		default:
			break fillOutput
		}
	}
	if !loop.SteerInject("pending steering during provider request") {
		t.Fatal("active turn did not accept steering")
	}
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Run error=%v, want cancellation", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("cancelled turn blocked with abandoned output consumer; cleanup=%d", finished.Load())
	}
	if finished.Load() != 1 {
		t.Fatalf("cleanup calls=%d, want exactly one", finished.Load())
	}
	if loop.SteerInject("too late") {
		t.Fatal("finished turn still accepts steering")
	}
	if pending := loop.SteerInjectDrainForTest(); len(pending) != 0 {
		t.Fatalf("shutdown left pending steering: %v", pending)
	}
	if len(out) != cap(out) {
		t.Fatalf("test unexpectedly drained abandoned output: len=%d cap=%d", len(out), cap(out))
	}
}

func TestLoopComputerUseCleanupIsJoinedBeforeRunReturns(t *testing.T) {
	provider := &cuCleanupProvider{stream: func(context.Context) (llm.StreamReader, error) {
		return textStream("done"), nil
	}}
	loop := NewLoop(provider, tools.NewRegistry(), permission.New(permission.ModeAcceptEdits), nil, "sys", 10)
	loop.AppendUser("finish after cleanup")
	entered, release := make(chan struct{}), make(chan struct{})
	loop.OnRunFinished = func() {
		close(entered)
		<-release
	}
	result := make(chan error, 1)
	go func() { result <- loop.Run(context.Background(), make(chan Event, 128)) }()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		close(release)
		t.Fatal("cleanup was not invoked")
	}
	select {
	case err := <-result:
		close(release)
		t.Fatalf("Run returned before cleanup completed: %v", err)
	default:
	}
	close(release)
	select {
	case err := <-result:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after cleanup completed")
	}
}
