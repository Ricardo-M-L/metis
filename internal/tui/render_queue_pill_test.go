package tui

// Queue-pill rendering tests (task #37) and queue state-machine
// invariants (task #35). The submit-side path (Enter while
// turnActive → push) and the dequeue-on-finalize path are exercised
// by unit-level reads of m.queuedPrompts; the actual handleSubmit
// round-trip needs a full Model + agent loop, covered by e2e.

import (
	"strings"
	"testing"
)

func TestRenderQueuePill_EmptyQueueRendersNothing(t *testing.T) {
	m := &Model{}
	if got := renderQueuePill(m); got != "" {
		t.Errorf("expected empty string for empty queue; got %q", got)
	}
}

func TestRenderQueuePill_ShowsCountAndPeek(t *testing.T) {
	m := &Model{
		width:         100,
		queuedPrompts: []string{"finish writing the integration test", "then format the diff"},
	}
	got := renderQueuePill(m)
	if got == "" {
		t.Fatalf("non-empty queue should render a pill")
	}
	if !strings.Contains(got, "queued × 2") {
		t.Errorf("expected count; got: %q", got)
	}
	if !strings.Contains(got, "finish writing") {
		t.Errorf("expected head peek; got: %q", got)
	}
}

func TestRenderQueuePill_TruncatesLongPeek(t *testing.T) {
	long := strings.Repeat("a", 200)
	m := &Model{width: 80, queuedPrompts: []string{long}}
	got := renderQueuePill(m)
	if !strings.Contains(got, "…") {
		t.Errorf("expected ellipsis on long peek; got: %q", got)
	}
}

func TestTruncatePeek_RuneSafe(t *testing.T) {
	s := "你好世界abcd"
	got := truncatePeek(s, 4)
	rs := []rune(got)
	if len(rs) > 4 {
		t.Errorf("rune count exceeded: %d in %q", len(rs), got)
	}
	if !strings.Contains(got, "…") {
		t.Errorf("expected ellipsis; got %q", got)
	}
}
