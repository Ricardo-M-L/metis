package agent

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/llm"
)

// TestLoop_MaybeCompact_CircuitBreakerEndToEnd — drives Loop.maybeCompact
// directly (not just Compactor.Compact in isolation) to prove the breaker
// works at the layer the runtime actually uses. Asserts:
//
//  1. Three failed attempts trip the breaker
//  2. The 4th call short-circuits (no extra summarizer hit)
//  3. The "auto-compaction disabled" EventInfo emits exactly once
//  4. Loop.Reset clears the breaker so a fresh attempt is allowed
func TestLoop_MaybeCompact_CircuitBreakerEndToEnd(t *testing.T) {
	failer := &errSummarizer{err: errors.New("simulated upstream timeout")}
	cfg := DefaultCompactionConfig()
	cfg.ProtectFirst = 1
	cfg.ProtectLast = 3
	c := NewCompactor(cfg, "test-model", 1000, failer)

	l := &Loop{Compactor: c}
	// Build a >threshold conversation so ShouldCompact returns true.
	l.Messages = []llm.Message{msg(llm.RoleUser, "system seed")}
	for i := 0; i < 12; i++ {
		l.Messages = append(l.Messages, msg(llm.RoleUser, strings.Repeat("x", 600)))
		l.Messages = append(l.Messages, msg(llm.RoleAssistant, strings.Repeat("y", 600)))
	}
	if !c.ShouldCompact(l.Messages) {
		t.Fatalf("precondition: convo should trigger compaction")
	}

	// Drain channel into a slice so we can inspect emissions.
	out := make(chan Event, 64)
	for i := 1; i <= MaxConsecutiveCompactFailures; i++ {
		l.maybeCompact(context.Background(), out)
	}
	if !c.CircuitTripped() {
		t.Fatalf("breaker should be open after %d failures", MaxConsecutiveCompactFailures)
	}

	// Now drive maybeCompact a 4th + 5th time. ShouldCompact returns
	// false → maybeCompact emits the "disabled" notice exactly ONCE.
	l.maybeCompact(context.Background(), out)
	l.maybeCompact(context.Background(), out)
	close(out)

	disabledNotices := 0
	for ev := range out {
		if ev.Kind == EventInfo && strings.Contains(ev.Info, "auto-compaction disabled") {
			disabledNotices++
		}
	}
	if disabledNotices != 1 {
		t.Errorf("disabled-notice should emit exactly once across 5 calls; got %d", disabledNotices)
	}

	// Loop.Reset must clear the breaker AND re-arm the notice gate
	// so a future trip would re-emit.
	l.Reset()
	if c.CircuitTripped() {
		t.Errorf("Loop.Reset() did not clear the Compactor breaker")
	}
	if l.compactCircuitNoticeSent {
		t.Errorf("Loop.Reset() did not re-arm the compactCircuitNoticeSent gate")
	}
}

// TestLoop_MaybeCompact_RecoversAfterTransientFailure — a single failure
// should NOT trip the breaker. Verifies the "transient errors don't
// permanently lock out" promise from the unit tests, but at the Loop
// layer where the runtime drives it.
func TestLoop_MaybeCompact_RecoversAfterTransientFailure(t *testing.T) {
	// First call uses fail-provider; swap to a good summarizer for the
	// second call. Simulates a one-off network blip.
	failer := &errSummarizer{err: errors.New("flaky")}
	cfg := DefaultCompactionConfig()
	cfg.ProtectFirst = 1
	cfg.ProtectLast = 3
	c := NewCompactor(cfg, "test-model", 1000, failer)
	l := &Loop{Compactor: c}
	l.Messages = []llm.Message{msg(llm.RoleUser, "seed")}
	for i := 0; i < 10; i++ {
		l.Messages = append(l.Messages, msg(llm.RoleUser, strings.Repeat("x", 600)))
		l.Messages = append(l.Messages, msg(llm.RoleAssistant, strings.Repeat("y", 600)))
	}

	out := make(chan Event, 16)
	l.maybeCompact(context.Background(), out)
	if c.CircuitTripped() {
		t.Fatalf("single failure must NOT trip the breaker")
	}
	if c.consecutiveFailures != 1 {
		t.Errorf("counter should be 1 after one failure; got %d", c.consecutiveFailures)
	}

	// Now swap in a good summarizer. Compact will succeed and the
	// counter must reset to 0.
	c.Provider = &fakeSummarizer{}
	l.maybeCompact(context.Background(), out)
	close(out)

	if c.consecutiveFailures != 0 {
		t.Errorf("success must reset counter; got %d", c.consecutiveFailures)
	}
}
