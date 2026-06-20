package agent

import (
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/internal/llm"
)

// EstimateContextTokens must never block on l.mu — the TUI calls it every
// render frame, and maybeCompact can hold the write lock for the 5-30s of a
// summarization. A blocking lock here freezes the UI and deadlocks against
// the loop's under-lock emit(). This test holds the write lock (simulating
// compaction) and asserts the call still returns promptly (cached value).
func TestEstimateContextTokens_NonBlockingUnderWriteLock(t *testing.T) {
	l := &Loop{
		Messages: []llm.Message{
			{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text", Text: "hello world, some context here"}}},
		},
	}
	// Prime the cache while the lock is free.
	primed := l.EstimateContextTokens()
	if primed <= 0 {
		t.Fatalf("primed estimate should be > 0, got %d", primed)
	}

	// Simulate maybeCompact holding the write lock for a long operation.
	l.mu.Lock()
	defer l.mu.Unlock()

	done := make(chan int, 1)
	go func() { done <- l.EstimateContextTokens() }()

	select {
	case v := <-done:
		if v != primed {
			t.Errorf("under write lock should return the cached estimate %d, got %d", primed, v)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("EstimateContextTokens blocked while the write lock was held — the compaction deadlock is NOT fixed")
	}
}
