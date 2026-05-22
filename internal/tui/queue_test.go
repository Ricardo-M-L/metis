package tui

import (
	"strings"
	"testing"
)

// TestQueue_EnqueueStampsClock — each enqueue must increment the
// monotonic clock so QueuedAt is strictly ordered. Without this two
// items added in the same tick would compare equal, leaking
// non-determinism into the drain order.
func TestQueue_EnqueueStampsClock(t *testing.T) {
	m := &Model{}
	m.enqueueQueuedItem("first", QueuePriorityNext)
	m.enqueueQueuedItem("second", QueuePriorityNext)
	m.enqueueQueuedItem("third", QueuePriorityNext)
	if got := len(m.queuedPrompts); got != 3 {
		t.Fatalf("expected 3 items; got %d", got)
	}
	for i, want := range []uint64{1, 2, 3} {
		if got := m.queuedPrompts[i].QueuedAt; got != want {
			t.Errorf("queuedPrompts[%d].QueuedAt = %d, want %d", i, got, want)
		}
	}
}

// TestQueue_DrainSingleItem — the baseline case: one item in the
// queue returns as a single-text drain. The (text, count==1) shape
// is what tui_update.go branches on to skip the merge-notice path.
func TestQueue_DrainSingleItem(t *testing.T) {
	m := &Model{}
	m.enqueueQueuedItem("only one", QueuePriorityNext)
	text, n := m.drainNextQueuedBatch()
	if n != 1 {
		t.Errorf("count = %d, want 1", n)
	}
	if text != "only one" {
		t.Errorf("text = %q, want %q", text, "only one")
	}
	if len(m.queuedPrompts) != 0 {
		t.Errorf("queue should be empty after drain; got %d items", len(m.queuedPrompts))
	}
}

// TestQueue_DrainBatchesSamePriority — the headline behaviour: three
// follow-up messages queued at the same priority merge into one
// drain call (joined with blank lines) instead of three separate
// turns. This is the spend-saver — without batching the model would
// see three round-trips for a sequence the user typed as one
// thought-stream.
func TestQueue_DrainBatchesSamePriority(t *testing.T) {
	m := &Model{}
	m.enqueueQueuedItem("first", QueuePriorityNext)
	m.enqueueQueuedItem("second", QueuePriorityNext)
	m.enqueueQueuedItem("third", QueuePriorityNext)

	text, n := m.drainNextQueuedBatch()
	if n != 3 {
		t.Errorf("count = %d, want 3", n)
	}
	want := "first\n\nsecond\n\nthird"
	if text != want {
		t.Errorf("merged text =\n%q\nwant\n%q", text, want)
	}
	if len(m.queuedPrompts) != 0 {
		t.Errorf("queue should be empty after batch drain; got %d", len(m.queuedPrompts))
	}
}

// TestQueue_DrainHonoursPriority — when items of different priorities
// are queued, the highest-priority bucket drains first. claude-code
// parity (`messageQueueManager.peek/dequeue` priority logic).
func TestQueue_DrainHonoursPriority(t *testing.T) {
	m := &Model{}
	// Sequence: Next, Now, Later, Next — Now should win on first
	// drain, then Next×2 batch, then Later.
	m.enqueueQueuedItem("a-next", QueuePriorityNext)
	m.enqueueQueuedItem("b-now", QueuePriorityNow)
	m.enqueueQueuedItem("c-later", QueuePriorityLater)
	m.enqueueQueuedItem("d-next", QueuePriorityNext)

	text, n := m.drainNextQueuedBatch()
	if text != "b-now" || n != 1 {
		t.Errorf("first drain should be the Now item solo; got %q (n=%d)", text, n)
	}

	text, n = m.drainNextQueuedBatch()
	if !strings.Contains(text, "a-next") || !strings.Contains(text, "d-next") || n != 2 {
		t.Errorf("second drain should batch the two Next items; got %q (n=%d)", text, n)
	}
	if !strings.Contains(text, "a-next\n\nd-next") {
		t.Errorf("Next-batch should preserve arrival order; got %q", text)
	}

	text, n = m.drainNextQueuedBatch()
	if text != "c-later" || n != 1 {
		t.Errorf("third drain should be the Later item; got %q (n=%d)", text, n)
	}
}

// TestQueue_DrainEmpty — calling drain on an empty queue must return
// (empty, 0) so the caller's `if batchN > 0` guard works.
func TestQueue_DrainEmpty(t *testing.T) {
	m := &Model{}
	text, n := m.drainNextQueuedBatch()
	if n != 0 || text != "" {
		t.Errorf("empty queue drain = (%q, %d); want (\"\", 0)", text, n)
	}
}

// TestQueue_ZeroPriorityTreatedAsNext — fixture-style bare literals
// (`queuedItem{Text: "x"}`) should NOT inherit priority Now just
// because uint8(0) happens to be the iota base. Verify effective()
// promotes 0 → Next so test fixtures + future programmatic enqueue
// sites can't accidentally jump the queue.
func TestQueue_ZeroPriorityTreatedAsNext(t *testing.T) {
	m := &Model{queuedPrompts: []queuedItem{
		{Text: "zero-prio", Priority: 0},
		{Text: "explicit-now", Priority: QueuePriorityNow},
	}}
	text, n := m.drainNextQueuedBatch()
	if text != "explicit-now" || n != 1 {
		t.Errorf("Now should still win over zero-priority (which maps to Next); got %q (n=%d)", text, n)
	}
}

// TestQueue_RenderPreviewShowsBadges — priority badges (! for Now,
// . for Later) must surface in the queued-preview band so the user
// can see at a glance why something is jumping ahead.
func TestQueue_RenderPreviewShowsBadges(t *testing.T) {
	m := &Model{queuedPrompts: []queuedItem{
		{Text: "urgent", Priority: QueuePriorityNow},
		{Text: "default"},
		{Text: "low", Priority: QueuePriorityLater},
	}}
	out := renderQueuedPreview(m)
	if !strings.Contains(out, "! urgent") {
		t.Errorf("Now item should carry '!' badge; preview:\n%s", out)
	}
	if !strings.Contains(out, ". low") {
		t.Errorf("Later item should carry '.' badge; preview:\n%s", out)
	}
	// Next item (the bare-text literal, mapped to Next via effective)
	// should have no badge — preserves the terse common case.
	if strings.Contains(out, "! default") || strings.Contains(out, ". default") {
		t.Errorf("Next item should have NO badge; preview:\n%s", out)
	}
}
