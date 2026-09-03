package hook

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"
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

func TestRegistry_PostToolUseContextIsBoundedAndUTF8Safe(t *testing.T) {
	r := NewRegistry()
	r.Register(PostToolUseContextHandler(func(context.Context, Context, *PostToolUse) *ModifiedPostToolUse {
		return &ModifiedPostToolUse{AdditionalContext: strings.Repeat("界", 20*1024)}
	}))
	r.Register(PostToolUseContextHandler(func(context.Context, Context, *PostToolUse) *ModifiedPostToolUse {
		return &ModifiedPostToolUse{AdditionalContext: "SECOND_SENTINEL"}
	}))
	r.Register(PostToolUseContextHandler(func(context.Context, Context, *PostToolUse) *ModifiedPostToolUse {
		return &ModifiedPostToolUse{AdditionalContext: strings.Repeat("x", 32*1024)}
	}))

	got := r.EmitPostToolUseContext(context.Background(), Context{}, &PostToolUse{})
	if len(got) > maxPostToolUseContextTotal {
		t.Fatalf("context has %d bytes, want at most %d", len(got), maxPostToolUseContextTotal)
	}
	if !utf8.ValidString(got) {
		t.Fatal("truncation split a UTF-8 sequence")
	}
	if !strings.Contains(got, postToolUseContextTruncated) {
		t.Fatal("truncated context did not include a visible marker")
	}
	if strings.Count(got, "SECOND_SENTINEL") != 1 {
		t.Fatalf("per-handler cap should preserve later feedback, got sentinel count %d", strings.Count(got, "SECOND_SENTINEL"))
	}
}

func TestRegistry_PostToolUseContextMarksAggregateOverflowAndCallsAllHandlers(t *testing.T) {
	r := NewRegistry()
	var calls atomic.Int32
	for _, value := range []string{
		strings.Repeat("a", maxPostToolUseContextPerHandler),
		strings.Repeat("b", maxPostToolUseContextTotal-maxPostToolUseContextPerHandler-1),
		"LATE_SENTINEL",
	} {
		value := value
		r.Register(PostToolUseContextHandler(func(context.Context, Context, *PostToolUse) *ModifiedPostToolUse {
			calls.Add(1)
			return &ModifiedPostToolUse{AdditionalContext: value}
		}))
	}

	got := r.EmitPostToolUseContext(context.Background(), Context{}, &PostToolUse{})
	if calls.Load() != 3 {
		t.Fatalf("handlers called = %d, want 3", calls.Load())
	}
	if len(got) > maxPostToolUseContextTotal {
		t.Fatalf("context has %d bytes, want at most %d", len(got), maxPostToolUseContextTotal)
	}
	if !strings.HasSuffix(got, postToolUseContextOmitted) {
		t.Fatalf("aggregate overflow was silent: suffix=%q", got[max(0, len(got)-64):])
	}
}

func TestPostToolUseIDContextRoundTrip(t *testing.T) {
	if got := PostToolUseIDFromContext(nil); got != "" {
		t.Fatalf("nil context ID = %q", got)
	}
	ctx := WithPostToolUseID(context.Background(), "call-42")
	if got := PostToolUseIDFromContext(ctx); got != "call-42" {
		t.Fatalf("context ID = %q, want call-42", got)
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

// TestRegistry_PostCompactJoinsAdditionalContext — PostCompact is
// feedback-capable: every handler's AdditionalContext contributes, in
// registration order, joined by newlines; nil returns and blank strings
// are skipped. The compaction path appends the joined string as a user
// message after the summary boundary.
func TestRegistry_PostCompactJoinsAdditionalContext(t *testing.T) {
	r := NewRegistry()
	r.Register(PostCompactHandler(func(_ context.Context, _ Context, p *PostCompact) *ModifiedPostCompact {
		if p.Tier != "compact" || p.Trigger != "auto" {
			t.Errorf("unexpected payload: tier=%q trigger=%q", p.Tier, p.Trigger)
		}
		return &ModifiedPostCompact{AdditionalContext: "branch: main"}
	}))
	r.Register(PostCompactHandler(func(_ context.Context, _ Context, _ *PostCompact) *ModifiedPostCompact {
		return nil // observer — contributes nothing
	}))
	r.Register(PostCompactHandler(func(_ context.Context, _ Context, _ *PostCompact) *ModifiedPostCompact {
		return &ModifiedPostCompact{AdditionalContext: "   "} // blank — skipped
	}))
	r.Register(PostCompactHandler(func(_ context.Context, _ Context, _ *PostCompact) *ModifiedPostCompact {
		return &ModifiedPostCompact{AdditionalContext: "run: make test"}
	}))
	got := r.EmitPostCompact(context.Background(), Context{}, &PostCompact{
		Trigger:        "auto",
		Tier:           "compact",
		BeforeMessages: 30,
		AfterMessages:  6,
		BeforeTokens:   9000,
		AfterTokens:    1200,
	})
	want := "branch: main\nrun: make test"
	if got != want {
		t.Errorf("EmitPostCompact joined %q, want %q", got, want)
	}
}

// TestRegistry_PostCompactNoHandlers — zero handlers returns "" so the
// compaction path skips injection entirely.
func TestRegistry_PostCompactNoHandlers(t *testing.T) {
	r := NewRegistry()
	if got := r.EmitPostCompact(context.Background(), Context{}, &PostCompact{Trigger: "auto", Tier: "compact"}); got != "" {
		t.Errorf("expected empty join with no handlers, got %q", got)
	}
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
