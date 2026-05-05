package hook

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestRegisterAsync_PostToolUseRunsInGoroutine verifies that an async
// PostToolUse handler doesn't block the EmitPostToolUse call site.
func TestRegisterAsync_PostToolUseRunsInGoroutine(t *testing.T) {
	r := NewRegistry()

	var blockUntilSignal sync.Mutex
	blockUntilSignal.Lock() // hold so handler will block until we release

	called := atomic.Bool{}

	r.RegisterAsync(PostToolUseHandler(func(ctx context.Context, _ Context, _ *PostToolUse) {
		blockUntilSignal.Lock() // would block forever in sync mode
		defer blockUntilSignal.Unlock()
		called.Store(true)
	}))

	// Emit should return immediately (handler is async).
	done := make(chan struct{})
	go func() {
		r.EmitPostToolUse(context.Background(), Context{}, &PostToolUse{Tool: "X"})
		close(done)
	}()
	select {
	case <-done:
		// fast — async path returned without waiting for handler
	case <-time.After(500 * time.Millisecond):
		t.Fatal("EmitPostToolUse blocked despite async handler")
	}

	// Now let the handler finish so the goroutine doesn't leak.
	blockUntilSignal.Unlock()
	// Give the goroutine a moment to actually run.
	time.Sleep(50 * time.Millisecond)
	if !called.Load() {
		t.Error("async handler never executed")
	}
}

// TestRegisterAsync_SyncStillBlocks verifies that the default Register
// path keeps the handler synchronous.
func TestRegisterAsync_SyncStillBlocks(t *testing.T) {
	r := NewRegistry()

	var ran atomic.Bool
	r.Register(PostToolUseHandler(func(ctx context.Context, _ Context, _ *PostToolUse) {
		time.Sleep(50 * time.Millisecond)
		ran.Store(true)
	}))

	start := time.Now()
	r.EmitPostToolUse(context.Background(), Context{}, &PostToolUse{})
	if !ran.Load() {
		t.Error("sync handler should have run before Emit returned")
	}
	if time.Since(start) < 50*time.Millisecond {
		t.Error("sync handler should have blocked Emit for at least 50ms")
	}
}

// TestRegisterAsync_PreToolUseStaysSync verifies that even when called
// via RegisterAsync, PreToolUse stays synchronous (its return value
// gates the dispatch).
func TestRegisterAsync_PreToolUseStaysSync(t *testing.T) {
	r := NewRegistry()
	var ran atomic.Bool
	r.RegisterAsync(PreToolUseHandler(func(ctx context.Context, _ Context, _ *PreToolUse) *ModifiedPreToolUse {
		ran.Store(true)
		return nil
	}))
	r.EmitPreToolUse(context.Background(), Context{}, &PreToolUse{})
	if !ran.Load() {
		t.Error("PreToolUse should run synchronously regardless of RegisterAsync")
	}
}
