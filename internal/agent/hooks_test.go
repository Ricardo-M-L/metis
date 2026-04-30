package agent

import (
	"context"
	"sync/atomic"
	"testing"
)

func TestHookRegistry_RegisterAndEmit(t *testing.T) {
	r := NewHookRegistry()
	var called atomic.Int32

	// Explicit type conversion preserves function type identity for Go's type switch.
	r.Register(PreToolUseHandler(func(_ context.Context, tc HookContext, in *PreToolUseHook) *ModifiedPreToolUse {
		if in.Tool != "Bash" {
			t.Errorf("want tool=Bash, got %q", in.Tool)
		}
		called.Add(1)
		return nil
	}))
	r.Register(TurnStartHandler(func(_ context.Context, tc HookContext, turn int) {
		if turn != 5 {
			t.Errorf("want turn=5, got %d", turn)
		}
		called.Add(10)
	}))

	tc := HookContext{SessionID: "s1", Model: "test", Turn: 5}
	r.EmitPreToolUse(context.Background(), tc, &PreToolUseHook{Context: tc, Tool: "Bash", Input: nil})
	if called.Load() != 1 {
		t.Errorf("PreToolUse called: want 1, got %d", called.Load())
	}
	r.EmitTurnStart(context.Background(), tc, 5)
	if called.Load() != 11 {
		t.Errorf("TurnStart called: want 11, got %d", called.Load())
	}
}

func TestHookRegistry_PreToolUse_Intercept(t *testing.T) {
	r := NewHookRegistry()
	r.Register(PreToolUseHandler(func(_ context.Context, tc HookContext, in *PreToolUseHook) *ModifiedPreToolUse {
		return &ModifiedPreToolUse{Output: &HookOutput{Content: "intercepted", IsError: false}}
	}))
	tc := HookContext{}
	mod := r.EmitPreToolUse(context.Background(), tc, &PreToolUseHook{Context: tc, Tool: "Bash", Input: nil})
	if mod == nil || mod.Output == nil || mod.Output.Content != "intercepted" {
		t.Errorf("intercept should have fired")
	}
}

func TestHookRegistry_MultipleHandlers(t *testing.T) {
	r := NewHookRegistry()
	var count atomic.Int32
	for i := 0; i < 3; i++ {
		r.Register(PreToolUseHandler(func(_ context.Context, _ HookContext, _ *PreToolUseHook) *ModifiedPreToolUse {
			count.Add(1)
			return nil
		}))
	}
	r.EmitPreToolUse(context.Background(), HookContext{}, &PreToolUseHook{})
	if count.Load() != 3 {
		t.Errorf("want 3 handlers called, got %d", count.Load())
	}
}

func TestHookRegistry_SessionStart_Async(t *testing.T) {
	r := NewHookRegistry()
	done := make(chan struct{})
	r.Register(SessionStartHandler(func(_ context.Context, _ HookContext, sys, model string) {
		if sys != "sys" || model != "m1" {
			t.Errorf("sys/model mismatch: %q %q", sys, model)
		}
		close(done)
	}))
	r.EmitSessionStart(context.Background(), HookContext{}, "sys", "m1")
	<-done
}
