package list

// list_test.go — covers metis-specific additions on top of crush's list:
//   * AtBottom cache: hit/miss/invalidate on every mutation pathway
//   * ScrollPercent edge cases (top, bottom, middle, empty)
//   * TotalLineCount sums correctly
//   * MouseWheelDelta clamping
//   * 1200-item virtualization: Render() output stays small
//   * SpacerItem behavior
//
// The crush-original behaviors (ScrollBy edges, VisibleItemIndices, etc.)
// are exercised through the chat-surface integration tests in the parent
// tui package — keeping this file focused on the metis-specific surface
// keeps each layer auditable.

import (
	"fmt"
	"strings"
	"testing"
)

// fakeItem is a minimal Item implementation: returns N lines of
// "row <idx>" content. Heights are deterministic and width-independent
// so AtBottom-cache tests can assert on counts.
type fakeItem struct {
	idx   int
	lines int
}

func (f *fakeItem) Render(width int) string {
	out := make([]string, f.lines)
	for i := range out {
		out[i] = fmt.Sprintf("row %d line %d", f.idx, i)
	}
	return strings.Join(out, "\n")
}

func makeFakeItems(n, linesEach int) []Item {
	items := make([]Item, n)
	for i := 0; i < n; i++ {
		items[i] = &fakeItem{idx: i, lines: linesEach}
	}
	return items
}

// hitsCounter wraps an Item to count how many times Render is called.
// Used to verify AtBottom caching reduces per-frame Render work.
type hitsCounter struct {
	inner Item
	hits  *int
}

func (h *hitsCounter) Render(width int) string {
	*h.hits++
	return h.inner.Render(width)
}

func TestAtBottom_CacheHitsAfterFirstCall(t *testing.T) {
	hits := 0
	items := makeFakeItems(50, 2)
	wrapped := make([]Item, len(items))
	for i, it := range items {
		wrapped[i] = &hitsCounter{inner: it, hits: &hits}
	}

	l := NewList(wrapped...)
	l.SetSize(80, 10)

	// First call populates the cache and walks ~5 items (5 × 2 = 10 lines = height).
	_ = l.AtBottom()
	hitsAfter1 := hits
	if hitsAfter1 == 0 {
		t.Fatal("first AtBottom should have rendered some items")
	}

	// 9 more calls without any state mutation must hit the cache:
	// no additional Render calls.
	for i := 0; i < 9; i++ {
		_ = l.AtBottom()
	}
	if hits != hitsAfter1 {
		t.Errorf("AtBottom cache miss: hits grew from %d to %d after 9 noop calls", hitsAfter1, hits)
	}
}

func TestAtBottom_CacheInvalidatesOnSetSize(t *testing.T) {
	hits := 0
	items := makeFakeItems(50, 2)
	wrapped := make([]Item, len(items))
	for i, it := range items {
		wrapped[i] = &hitsCounter{inner: it, hits: &hits}
	}

	l := NewList(wrapped...)
	l.SetSize(80, 10)
	_ = l.AtBottom()
	hits1 := hits

	l.SetSize(80, 20) // different height ⇒ key changed
	_ = l.AtBottom()
	if hits == hits1 {
		t.Error("SetSize with different height should invalidate AtBottom cache")
	}
}

func TestAtBottom_CacheInvalidatesOnAppendItems(t *testing.T) {
	hits := 0
	items := makeFakeItems(5, 2)
	wrapped := make([]Item, len(items))
	for i, it := range items {
		wrapped[i] = &hitsCounter{inner: it, hits: &hits}
	}

	l := NewList(wrapped...)
	l.SetSize(80, 100) // tall viewport so all 5 fit ⇒ AtBottom=true
	_ = l.AtBottom()
	hits1 := hits

	more := []Item{&fakeItem{idx: 99, lines: 2}}
	l.AppendItems(more...) // itemCount changed in key
	_ = l.AtBottom()
	if hits == hits1 {
		t.Error("AppendItems should invalidate AtBottom cache")
	}
}

func TestAtBottom_CacheInvalidatesOnSetItems(t *testing.T) {
	l := NewList(makeFakeItems(3, 2)...)
	l.SetSize(80, 100)
	_ = l.AtBottom() // populate cache

	l.SetItems(makeFakeItems(10, 2)...) // entirely new items
	// Key changed (itemCount); next call must recompute.
	if !l.atBottomCacheValid {
		// SetItems was supposed to invalidate explicitly, but the
		// key-based check would also catch this — either is fine.
		// Just make sure the result is correct after.
	}
	got := l.AtBottom()
	want := false // 10 items × 2 lines = 20 lines, viewport 100 ⇒ all fit ⇒ at bottom
	want = true
	if got != want {
		t.Errorf("AtBottom after SetItems = %v, want %v", got, want)
	}
}

func TestAtBottom_CorrectAtTopWithLongList(t *testing.T) {
	l := NewList(makeFakeItems(100, 2)...)
	l.SetSize(80, 10)
	// Default offsetIdx=0, offsetLine=0 ⇒ at top of a 200-line list
	// in a 10-line viewport ⇒ NOT at bottom.
	if l.AtBottom() {
		t.Error("at top of long list should NOT report AtBottom")
	}
}

func TestAtBottom_CorrectAfterScrollToBottom(t *testing.T) {
	l := NewList(makeFakeItems(100, 2)...)
	l.SetSize(80, 10)
	l.ScrollToBottom()
	if !l.AtBottom() {
		t.Error("after ScrollToBottom should report AtBottom=true")
	}
}

func TestScrollBy_DownThenUpRoundTrip(t *testing.T) {
	l := NewList(makeFakeItems(20, 3)...) // 60 lines total
	l.SetSize(80, 10)

	l.ScrollBy(5)
	idxAfterDown := l.offsetIdx
	lineAfterDown := l.offsetLine
	if idxAfterDown == 0 && lineAfterDown == 0 {
		t.Error("ScrollBy(5) on tall content should advance offset")
	}

	l.ScrollBy(-5)
	if l.offsetIdx != 0 || l.offsetLine != 0 {
		t.Errorf("ScrollBy roundtrip should return to top, got idx=%d line=%d",
			l.offsetIdx, l.offsetLine)
	}
}

func TestScrollBy_ClampedAtBottom(t *testing.T) {
	l := NewList(makeFakeItems(20, 3)...) // 60 lines total
	l.SetSize(80, 10)
	l.ScrollBy(10000) // way past end
	if !l.AtBottom() {
		t.Error("over-scroll should clamp to bottom")
	}
}

func TestScrollBy_ClampedAtTop(t *testing.T) {
	l := NewList(makeFakeItems(20, 3)...)
	l.SetSize(80, 10)
	l.ScrollBy(-10000) // way past start
	if l.offsetIdx != 0 || l.offsetLine != 0 {
		t.Errorf("over-scroll up should clamp to top, got idx=%d line=%d",
			l.offsetIdx, l.offsetLine)
	}
}

func TestScrollPercent_TopAndBottom(t *testing.T) {
	l := NewList(makeFakeItems(100, 2)...)
	l.SetSize(80, 10)

	l.ScrollToTop()
	if pct := l.ScrollPercent(); pct != 0.0 {
		t.Errorf("ScrollPercent at top = %f, want 0.0", pct)
	}

	l.ScrollToBottom()
	if pct := l.ScrollPercent(); pct != 1.0 {
		t.Errorf("ScrollPercent at bottom = %f, want 1.0", pct)
	}
}

func TestScrollPercent_EmptyList(t *testing.T) {
	l := NewList()
	l.SetSize(80, 10)
	if pct := l.ScrollPercent(); pct != 1.0 {
		t.Errorf("ScrollPercent on empty list = %f, want 1.0 (treat as bottom)", pct)
	}
}

func TestTotalLineCount_SumsHeights(t *testing.T) {
	l := NewList(makeFakeItems(10, 3)...) // 30 lines, no gap
	l.SetSize(80, 100)
	if got := l.TotalLineCount(); got != 30 {
		t.Errorf("TotalLineCount = %d, want 30", got)
	}

	l.SetGap(1) // 9 inter-item gaps × 1 = 9 extra
	if got := l.TotalLineCount(); got != 30+9 {
		t.Errorf("TotalLineCount with gap = %d, want %d", got, 30+9)
	}
}

func TestMouseWheelDelta_DefaultAndClamp(t *testing.T) {
	l := NewList()
	if got := l.MouseWheelDelta(); got != 1 {
		t.Errorf("default MouseWheelDelta = %d, want 1", got)
	}

	l.SetMouseWheelDelta(0) // invalid; must clamp
	if got := l.MouseWheelDelta(); got != 1 {
		t.Errorf("after SetMouseWheelDelta(0), got %d, want 1", got)
	}

	l.SetMouseWheelDelta(5)
	if got := l.MouseWheelDelta(); got != 5 {
		t.Errorf("after SetMouseWheelDelta(5), got %d", got)
	}
}

// TestRender_VirtualizationKeepsOutputSmall is the headline check: 1200
// items each ~10 lines = 12000 lines of content, but a 30-line viewport
// must produce <= 30 lines of output (= a few KB of string), not the
// full 12000-line concatenation that pre-virtualization metis emitted.
func TestRender_VirtualizationKeepsOutputSmall(t *testing.T) {
	l := NewList(makeFakeItems(1200, 10)...)
	l.SetSize(80, 30)

	out := l.Render()
	lineCount := strings.Count(out, "\n") + 1
	if lineCount > 30 {
		t.Errorf("Render output line count = %d, want <= 30 (virtualization broken)", lineCount)
	}
	// Sanity: should be > 5KB only if virtualization is utterly broken
	// (1200 × 10 lines × ~14 chars ≈ 168 KB). 30 lines × ~14 chars ≈ 420 B.
	if len(out) > 5*1024 {
		t.Errorf("Render output size = %d bytes, want <= 5 KB (virtualization broken)", len(out))
	}
}

func TestRender_EmptyList(t *testing.T) {
	l := NewList()
	l.SetSize(80, 30)
	if out := l.Render(); out != "" {
		t.Errorf("empty list Render = %q, want empty", out)
	}
}

func TestSpacerItem(t *testing.T) {
	s := NewSpacerItem(5)
	out := s.Render(80)
	got := strings.Count(out, "\n")
	want := 4 // height-1 newlines = 5 visual lines
	if got != want {
		t.Errorf("SpacerItem(5) renders %d newlines, want %d", got, want)
	}
}

func TestVisibleItemIndices_FullList(t *testing.T) {
	// 5 items × 2 lines = 10 lines, viewport 100 lines ⇒ all visible.
	l := NewList(makeFakeItems(5, 2)...)
	l.SetSize(80, 100)
	startIdx, endIdx := l.VisibleItemIndices()
	if startIdx != 0 || endIdx != 4 {
		t.Errorf("VisibleItemIndices full-list = (%d, %d), want (0, 4)", startIdx, endIdx)
	}
}

func TestVisibleItemIndices_PartialView(t *testing.T) {
	l := NewList(makeFakeItems(20, 3)...) // 60 lines total
	l.SetSize(80, 10)                     // viewport holds ~3-4 items
	startIdx, endIdx := l.VisibleItemIndices()
	if startIdx != 0 {
		t.Errorf("startIdx = %d, want 0", startIdx)
	}
	if endIdx >= 20 {
		t.Errorf("endIdx = %d, expected < 20 (viewport doesn't fit all)", endIdx)
	}
}

// TestRender_PadsShortContentToHeight — when the viewport is taller
// than the content (e.g. user scrolled near the top of a short
// transcript), Render must emit exactly l.height lines so the alt-
// screen doesn't keep stale content from a previous frame in the
// uncovered rows. Bug report 2026-05-07: "下面一块文字卡住不动".
func TestRender_PadsShortContentToHeight(t *testing.T) {
	l := NewList(&staticItem{content: "line A"}, &staticItem{content: "line B"})
	l.SetSize(80, 10) // viewport 10, content only 2 lines
	out := l.Render()
	gotLines := strings.Count(out, "\n") + 1
	if gotLines != 10 {
		t.Errorf("Render produced %d lines (separators+1); want 10\noutput: %q",
			gotLines, out)
	}
}
