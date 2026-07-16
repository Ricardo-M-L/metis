package agent

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/memory"
)

// TestDreamLock_IntegrationAdvancesOnSuccess — end-to-end: with all
// gates set to permissive defaults via env and a populated sessions
// dir, a single OnLoopEnd should clear the gate, acquire the lock,
// run the fork to completion, and leave the lock with mtime ~= now
// and an empty PID body (the post-Release marker). This is the
// regression check for "Phase A wiring still works after future
// edits to OnLoopEnd / runOnce".
func TestDreamLock_IntegrationAdvancesOnSuccess(t *testing.T) {
	t.Setenv("METIS_DREAM_INTERVAL_HOURS", "0.000001") // ~3.6 ms — effectively no time floor
	t.Setenv("METIS_DREAM_MIN_SESSIONS", "1")          // single session is enough

	prov := &scriptedProvider{
		resps: []*llm.Response{
			{Content: []llm.ContentBlock{{Type: "text", Text: "x"}}, StopReason: "end_turn"},
		},
	}
	memRoot := t.TempDir()
	_, ext := newTestExtractor(t, prov, memRoot)

	// Lock starts not-yet-existing → LastSuccessAt is zero.
	lock := memory.NewDreamLock(memRoot)
	if !lock.LastSuccessAt().IsZero() {
		t.Fatalf("pre-run lock should be zero, got %v", lock.LastSuccessAt())
	}

	before := time.Now()
	ext.OnLoopEnd(context.Background(), "end_turn")
	// TotalExtractions advances before index regeneration, post-run
	// hygiene, and the deferred lock release. Wait for the outer fork
	// goroutine too; otherwise t.TempDir cleanup can race those final
	// writes and fail with "directory not empty" on macOS.
	waitFor(t, 2*time.Second, func() bool {
		return ext.Stats().TotalExtractions == 1 && ForkInflight() == 0
	})

	// Post-run: lock mtime should be advanced to "around now" and body
	// should be empty (Release truncates the PID after success).
	body, err := os.ReadFile(lock.Path())
	if err != nil {
		t.Fatalf("read post-run lock: %v", err)
	}
	if len(body) != 0 {
		t.Fatalf("post-run lock body should be empty, got %q", body)
	}
	mtime := lock.LastSuccessAt()
	if mtime.Before(before) {
		t.Fatalf("post-run mtime %v earlier than test start %v — lock didn't advance", mtime, before)
	}
	if time.Since(mtime) > 5*time.Second {
		t.Fatalf("post-run mtime %v too far from now (Δ=%v) — Release may not have fired",
			mtime, time.Since(mtime))
	}
}
