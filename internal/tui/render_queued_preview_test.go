package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

// TestRenderQueuedPreview_EmptyQueueRendersNothing — the chrome row
// must collapse to zero output when the queue is empty so the input
// area doesn't get pushed up by a phantom band.
func TestRenderQueuedPreview_EmptyQueueRendersNothing(t *testing.T) {
	m := &Model{}
	if got := renderQueuedPreview(m); got != "" {
		t.Errorf("empty queue should render empty string; got %q", got)
	}
}

// TestRenderQueuedPreview_OneRowPerQueued — every queued prompt
// surfaces as a discrete row so the user can count what's pending.
// Bare prompts < truncation cap render verbatim.
func TestRenderQueuedPreview_OneRowPerQueued(t *testing.T) {
	m := &Model{queuedPrompts: []queuedItem{{Text: "first message"}, {Text: "second one"}}}
	got := renderQueuedPreview(m)
	plain := ansi.Strip(got)
	if !strings.Contains(plain, "first message") {
		t.Errorf("preview missing first queued prompt: %q", plain)
	}
	if !strings.Contains(plain, "second one") {
		t.Errorf("preview missing second queued prompt: %q", plain)
	}
	if strings.Count(plain, "queued") < 2 {
		t.Errorf("expected 2 ⏵ queued markers; got: %q", plain)
	}
}

// TestRenderQueuedPreview_TruncatesLongPrompts — a multi-paragraph
// or 200-char prompt should compress to one line ending in "…" so
// the preview band stays predictable in height.
func TestRenderQueuedPreview_TruncatesLongPrompts(t *testing.T) {
	long := strings.Repeat("xyz ", 60) // ~240 chars
	m := &Model{queuedPrompts: []queuedItem{{Text: long}}}
	plain := ansi.Strip(renderQueuedPreview(m))
	if !strings.Contains(plain, "…") {
		t.Errorf("long prompt should end with …; got %q", plain)
	}
	// One row only; the truncation should not have spilled to wraps.
	lines := strings.Split(strings.TrimRight(plain, "\n"), "\n")
	if len(lines) != 1 {
		t.Errorf("expected 1 row, got %d: %q", len(lines), lines)
	}
}

// TestRenderQueuedPreview_CJKNotCorrupted — rune-aware truncation
// must not slice mid-codepoint. Same image #14 lesson as
// toolArgsPreview.
func TestRenderQueuedPreview_CJKNotCorrupted(t *testing.T) {
	// 150 runes of CJK + ASCII triggers the 90-rune cap cleanly.
	prompt := strings.Repeat("中a", 75)
	m := &Model{queuedPrompts: []queuedItem{{Text: prompt}}}
	out := renderQueuedPreview(m)
	plain := ansi.Strip(out)
	// Either the whole prompt fit or the truncated tail is "…" —
	// in either case the result must be valid UTF-8 with no mid-
	// codepoint replacement runes.
	if strings.Contains(plain, "�") {
		t.Errorf("truncation produced U+FFFD replacement rune (mid-codepoint cut): %q", plain)
	}
}

// TestRenderQueuedPreview_OverflowSummary — once the queue exceeds
// the visible cap, the surplus collapses into a "+ N more queued"
// row so deep queues don't push other chrome off-screen.
func TestRenderQueuedPreview_OverflowSummary(t *testing.T) {
	m := &Model{queuedPrompts: []queuedItem{{Text: "a"}, {Text: "b"}, {Text: "c"}, {Text: "d"}, {Text: "e"}}}
	plain := ansi.Strip(renderQueuedPreview(m))
	if !strings.Contains(plain, "+ 2 more queued") {
		t.Errorf("expected '+ 2 more queued' summary; got %q", plain)
	}
	// First 3 should still be visible by themselves.
	for _, want := range []string{"queued · a", "queued · b", "queued · c"} {
		if !strings.Contains(plain, want) {
			t.Errorf("missing visible row %q in preview: %q", want, plain)
		}
	}
	// Hidden ones should NOT appear as their own rows.
	if strings.Contains(plain, "queued · d") {
		t.Errorf("overflowed row 'd' should not have rendered: %q", plain)
	}
}

// TestRenderQueuedPreview_NewlineCollapsed — pasting a multi-line
// prompt mid-turn should still produce ONE preview row (the full
// text remains in m.queuedPrompts for the eventual turn). Avoid
// visually expanding the band to N rows for a single message.
func TestRenderQueuedPreview_NewlineCollapsed(t *testing.T) {
	m := &Model{queuedPrompts: []queuedItem{{Text: "line one\nline two\nline three"}}}
	plain := ansi.Strip(renderQueuedPreview(m))
	rows := strings.Split(strings.TrimRight(plain, "\n"), "\n")
	if len(rows) != 1 {
		t.Errorf("multi-line prompt should collapse to 1 preview row; got %d: %q", len(rows), rows)
	}
	if !strings.Contains(plain, "⏎") {
		t.Errorf("collapsed newline marker ⏎ missing: %q", plain)
	}
}
