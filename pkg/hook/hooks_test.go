package hook

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

func TestRegistry_PreToolUse_Intercept(t *testing.T) {
	r := NewRegistry()
	r.Register(PreToolUseHandler(func(_ context.Context, _ Context, _ *PreToolUse) *ModifiedPreToolUse {
		return &ModifiedPreToolUse{Output: &Output{Content: "blocked", IsError: true}}
	}))
	mod := r.EmitPreToolUse(context.Background(), Context{}, &PreToolUse{Tool: "Bash"})
	if mod == nil || mod.Output == nil || mod.Output.Content != "blocked" {
		t.Errorf("expected intercept, got %+v", mod)
	}
}

func TestRegistry_PreToolUse_NilProceeds(t *testing.T) {
	r := NewRegistry()
	r.Register(PreToolUseHandler(func(_ context.Context, _ Context, _ *PreToolUse) *ModifiedPreToolUse {
		return nil
	}))
	if mod := r.EmitPreToolUse(context.Background(), Context{}, &PreToolUse{}); mod != nil {
		t.Errorf("nil-returning handler should yield nil ModifiedPreToolUse; got %+v", mod)
	}
}

func TestRegistry_FirstInterceptWins(t *testing.T) {
	r := NewRegistry()
	r.Register(PreToolUseHandler(func(_ context.Context, _ Context, _ *PreToolUse) *ModifiedPreToolUse {
		return &ModifiedPreToolUse{Output: &Output{Content: "first"}}
	}))
	r.Register(PreToolUseHandler(func(_ context.Context, _ Context, _ *PreToolUse) *ModifiedPreToolUse {
		return &ModifiedPreToolUse{Output: &Output{Content: "second"}}
	}))
	mod := r.EmitPreToolUse(context.Background(), Context{}, &PreToolUse{})
	if mod.Output.Content != "first" {
		t.Errorf("first registration should win short-circuit; got %s", mod.Output.Content)
	}
}

func TestRegistry_PostToolUseFanOut(t *testing.T) {
	r := NewRegistry()
	var n atomic.Int32
	for i := 0; i < 5; i++ {
		r.Register(PostToolUseHandler(func(_ context.Context, _ Context, _ *PostToolUse) {
			n.Add(1)
		}))
	}
	r.EmitPostToolUse(context.Background(), Context{}, &PostToolUse{})
	if n.Load() != 5 {
		t.Errorf("expected 5 fan-outs, got %d", n.Load())
	}
}

func TestRegistry_PreCompactFanOut(t *testing.T) {
	r := NewRegistry()
	var got atomic.Int32
	var trigger atomic.Value
	for i := 0; i < 3; i++ {
		r.Register(PreCompactHandler(func(_ context.Context, _ Context, p *PreCompact) {
			got.Add(1)
			trigger.Store(p.Trigger)
		}))
	}
	r.EmitPreCompact(context.Background(), Context{}, &PreCompact{
		Trigger:         "auto",
		MessageCount:    42,
		EstimatedTokens: 9000,
	})
	if got.Load() != 3 {
		t.Errorf("expected 3 PreCompact fan-outs, got %d", got.Load())
	}
	if trigger.Load() != "auto" {
		t.Errorf("expected trigger=auto, got %v", trigger.Load())
	}
}

func TestRegistry_PreCompactAsyncDetaches(t *testing.T) {
	r := NewRegistry()
	release := make(chan struct{})
	started := make(chan struct{})
	r.RegisterAsync(PreCompactHandler(func(_ context.Context, _ Context, _ *PreCompact) {
		close(started)
		<-release // would block the emitter if run inline
	}))
	done := make(chan struct{})
	go func() {
		r.EmitPreCompact(context.Background(), Context{}, &PreCompact{Trigger: "manual"})
		close(done)
	}()
	<-started // handler is running
	select {
	case <-done: // emitter returned without waiting for the handler — async worked
	case <-time.After(2 * time.Second):
		t.Fatal("EmitPreCompact blocked on async handler")
	}
	close(release)
}

func TestRegistry_UnknownHandlerSilentlyDropped(t *testing.T) {
	// Plugin authors might pass any{} that isn't a recognized handler type
	// — Register should silently ignore rather than panic.
	r := NewRegistry()
	r.Register(func() {}) // wrong signature
	r.Register("not a handler")
	// No-op verification: calling Emit on an empty list shouldn't panic.
	r.EmitPreToolUse(context.Background(), Context{}, &PreToolUse{})
}

func TestRegistry_AllEventTypesRound(t *testing.T) {
	r := NewRegistry()
	var sessionStarted, sessionEnded, turnStart, turnEnd, looped, errored atomic.Int32
	r.Register(SessionStartHandler(func(_ context.Context, _ Context, _, _ string) { sessionStarted.Add(1) }))
	r.Register(SessionEndHandler(func(_ context.Context, _ Context, _ int, _ string) { sessionEnded.Add(1) }))
	r.Register(TurnStartHandler(func(_ context.Context, _ Context, _ int) { turnStart.Add(1) }))
	r.Register(TurnEndHandler(func(_ context.Context, _ Context, _ int) { turnEnd.Add(1) }))
	r.Register(LoopEndHandler(func(_ context.Context, _ Context, _ string) { looped.Add(1) }))
	r.Register(ErrorHandler(func(_ context.Context, _ Context, _ error) { errored.Add(1) }))

	ctx := context.Background()
	r.EmitSessionStart(ctx, Context{}, "sys", "model")
	r.EmitTurnStart(ctx, Context{}, 1)
	r.EmitTurnEnd(ctx, Context{}, 1)
	r.EmitLoopEnd(ctx, Context{}, "end_turn")
	r.EmitError(ctx, Context{}, errors.New("boom"))
	r.EmitSessionEnd(ctx, Context{}, 5, "ok")

	if sessionStarted.Load() != 1 || sessionEnded.Load() != 1 ||
		turnStart.Load() != 1 || turnEnd.Load() != 1 ||
		looped.Load() != 1 || errored.Load() != 1 {
		t.Errorf("not all events fired: ss=%d se=%d ts=%d te=%d le=%d err=%d",
			sessionStarted.Load(), sessionEnded.Load(),
			turnStart.Load(), turnEnd.Load(),
			looped.Load(), errored.Load())
	}
}

func TestRegistry_ConcurrentRegisterAndEmit(t *testing.T) {
	// Registry must be safe under concurrent Register / Emit pairs.
	r := NewRegistry()
	done := make(chan struct{})
	go func() {
		for i := 0; i < 100; i++ {
			r.Register(PreToolUseHandler(func(_ context.Context, _ Context, _ *PreToolUse) *ModifiedPreToolUse { return nil }))
		}
		close(done)
	}()
	for i := 0; i < 100; i++ {
		r.EmitPreToolUse(context.Background(), Context{}, &PreToolUse{})
	}
	<-done
}
