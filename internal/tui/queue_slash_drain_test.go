package tui

import (
	"strings"
	"testing"
)

// TestQueue_DrainSlashesOneAtATime — image #33 repro: user queued
// /tasks /tasks /skills mid-turn; the old drain merged them with
// "\n\n" into a single user message that neither parsed as a slash
// nor reads sensibly. The slash-carve-out fix should peel them off
// one at a time so each hits the dispatcher cleanly.
func TestQueue_DrainSlashesOneAtATime(t *testing.T) {
	m := &Model{}
	m.enqueueQueuedItem("/tasks", QueuePriorityNext)
	m.enqueueQueuedItem("/tasks", QueuePriorityNext)
	m.enqueueQueuedItem("/skills", QueuePriorityNext)

	for i, want := range []string{"/tasks", "/tasks", "/skills"} {
		text, n := m.drainNextQueuedBatch()
		if n != 1 {
			t.Errorf("drain %d: count = %d, want 1 (slash never batches)", i, n)
		}
		if text != want {
			t.Errorf("drain %d: text = %q, want %q", i, text, want)
		}
	}
	if len(m.queuedPrompts) != 0 {
		t.Errorf("queue should be empty after 3 slashes; got %d", len(m.queuedPrompts))
	}
}

// TestQueue_DrainPlainTextStillBatches — regression guard: making
// slashes single-drain must NOT break the plain-text batching
// behaviour. 3 plain follow-ups still merge into one turn.
func TestQueue_DrainPlainTextStillBatches(t *testing.T) {
	m := &Model{}
	m.enqueueQueuedItem("first", QueuePriorityNext)
	m.enqueueQueuedItem("second", QueuePriorityNext)
	m.enqueueQueuedItem("third", QueuePriorityNext)

	text, n := m.drainNextQueuedBatch()
	if n != 3 {
		t.Errorf("plain-text batching broken: count = %d, want 3", n)
	}
	if !strings.Contains(text, "first\n\nsecond\n\nthird") {
		t.Errorf("plain-text batch lost join order: %q", text)
	}
}

// TestQueue_DrainMixedSlashAndText — when a slash is at the head of
// the priority bucket, it goes alone; subsequent drains should then
// batch the trailing plain-text items together. This is the "user
// typed /tasks, then 'and also check X', then 'and Y'" pattern.
func TestQueue_DrainMixedSlashAndText(t *testing.T) {
	m := &Model{}
	m.enqueueQueuedItem("/tasks", QueuePriorityNext)
	m.enqueueQueuedItem("and also check X", QueuePriorityNext)
	m.enqueueQueuedItem("and Y", QueuePriorityNext)

	text, n := m.drainNextQueuedBatch()
	if n != 1 || text != "/tasks" {
		t.Errorf("first drain should be /tasks solo; got %q (n=%d)", text, n)
	}

	text, n = m.drainNextQueuedBatch()
	if n != 2 {
		t.Errorf("second drain should batch the 2 trailing plain texts; got n=%d", n)
	}
	if !strings.Contains(text, "and also check X\n\nand Y") {
		t.Errorf("plain batch wrong: %q", text)
	}
}

// TestQueue_DrainSlashSandwich — head-position slash check vs
// sandwich position: when a slash is in the MIDDLE of same-priority
// items, the first plain text triggers the batch path; the slash
// gets deferred to a future drain. End-to-end, all three items
// surface eventually, none get lost.
func TestQueue_DrainSlashSandwich(t *testing.T) {
	m := &Model{}
	m.enqueueQueuedItem("before", QueuePriorityNext)
	m.enqueueQueuedItem("/tasks", QueuePriorityNext)
	m.enqueueQueuedItem("after", QueuePriorityNext)

	// First drain: head is plain → batch path; sandwich slash is
	// excluded from the batch and stays.
	text, n := m.drainNextQueuedBatch()
	if !strings.Contains(text, "before") || !strings.Contains(text, "after") {
		t.Errorf("first drain should batch before+after; got %q", text)
	}
	if n != 2 {
		t.Errorf("first drain count = %d, want 2", n)
	}

	// Second drain: only the slash remains.
	text, n = m.drainNextQueuedBatch()
	if text != "/tasks" || n != 1 {
		t.Errorf("second drain should be the deferred slash; got %q (n=%d)", text, n)
	}
}

// TestIsSlashItem — exhaustive coverage of the recogniser since the
// drain branch hinges on it.
func TestIsSlashItem(t *testing.T) {
	yes := []string{"/tasks", "/help", "  /tasks", "\t/skills foo", "/x"}
	no := []string{"", "/", "tasks", "  tasks", "  ", "hi /tasks"}
	for _, in := range yes {
		if !isSlashItem(in) {
			t.Errorf("isSlashItem(%q) = false, want true", in)
		}
	}
	for _, in := range no {
		if isSlashItem(in) {
			t.Errorf("isSlashItem(%q) = true, want false", in)
		}
	}
}
