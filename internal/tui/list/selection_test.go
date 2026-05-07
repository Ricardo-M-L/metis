package list

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

// staticItem is a minimal Item for tests. The renderer just returns
// pre-computed content; height is whatever the content's line count is.
type staticItem struct{ content string }

func (s *staticItem) Render(int) string { return s.content }

func makeListWithItems(items ...string) *List {
	wrapped := make([]Item, len(items))
	for i, c := range items {
		wrapped[i] = &staticItem{content: c}
	}
	l := NewList(wrapped...)
	l.SetSize(80, 30)
	return l
}

// TestSelection_DragHighlightsAndExtractsText — drag from item 0 col 0
// to item 1 mid-line should yield SelectedText spanning both items.
func TestSelection_DragHighlightsAndExtractsText(t *testing.T) {
	l := makeListWithItems(
		"hello world",
		"second line text",
		"third line",
	)

	// Anchor at (item 0, line 0, col 0) — start of "hello"
	l.HandleMouseDown(0, 0, 1)
	// Drag to (item 1, line 0, col 6) — middle of "second line text"
	// y=0 is item 0; y=1 is item 1's first (only) line
	l.HandleMouseDrag(6, 1)
	if !l.HasSelection() {
		t.Fatalf("expected HasSelection() = true mid-drag")
	}

	got := l.SelectedText()
	want := "hello world\nsecond"
	if got != want {
		t.Errorf("SelectedText cross-item drag\n  got:  %q\n  want: %q", got, want)
	}

	// Render must include the solid-bg highlight escape for the
	// highlighted span. (We migrated from `\x1b[7m` reverse to solid
	// bg 238 — see selectionBgOpen.)
	rendered := l.Render()
	if !strings.Contains(rendered, "\x1b[48;5;238m") {
		t.Errorf("Render() should emit solid-bg SGR (\\x1b[48;5;238m) when selection active; got:\n%s",
			ansi.Strip(rendered))
	}
}

// TestSelection_DoubleClickSelectsWord — double-click in the middle of
// "world" should snap selection to that word's display-column bounds.
func TestSelection_DoubleClickSelectsWord(t *testing.T) {
	l := makeListWithItems("hello world today")
	// "hello world today"
	//  0     6     12     ← display columns
	// Double-click at col 7 (inside "world").
	l.HandleMouseDown(7, 0, 2)
	if !l.HasSelection() {
		t.Fatalf("expected selection after double-click")
	}
	got := l.SelectedText()
	if got != "world" {
		t.Errorf("double-click word selection: got %q want %q", got, "world")
	}
}

// TestSelection_TripleClickSelectsLine — triple-click selects the
// entire line at the click's y coordinate.
func TestSelection_TripleClickSelectsLine(t *testing.T) {
	l := makeListWithItems("first row\nsecond row")
	// Triple-click at (col 3, line 1) — anywhere on "second row"
	l.HandleMouseDown(3, 1, 3)
	got := l.SelectedText()
	if got != "second row" {
		t.Errorf("triple-click line selection: got %q want %q", got, "second row")
	}
}

// TestSelection_SingleClickNoCopy — a single mouse-down without a
// follow-up drag must NOT register as having a selection (the
// row-copy fallback in tui_update only fires on mouse-up).
func TestSelection_SingleClickNoCopy(t *testing.T) {
	l := makeListWithItems("hello")
	l.HandleMouseDown(2, 0, 1)
	// Without a drag, anchor == focus. HasSelection treats Dragging
	// as live (so highlight CAN render, but range is empty).
	if l.HasSelection() && l.SelectedText() != "" {
		t.Errorf("zero-distance click should produce empty selection text")
	}
	// MouseUp on identical point clears the selection.
	if l.HandleMouseUp() {
		t.Errorf("MouseUp on a zero-drag click should return false")
	}
	if l.HasSelection() {
		t.Errorf("HasSelection should be false after empty MouseUp")
	}
}

// TestSelection_ClearAfterCopy — explicit ClearSelection wipes state.
func TestSelection_ClearAfterCopy(t *testing.T) {
	l := makeListWithItems("hello world")
	l.HandleMouseDown(0, 0, 1)
	l.HandleMouseDrag(5, 0)
	l.HandleMouseUp()
	if !l.HasSelection() {
		t.Fatalf("setup: expected selection after up")
	}
	l.ClearSelection()
	if l.HasSelection() {
		t.Errorf("ClearSelection should empty the state")
	}
	if got := l.SelectedText(); got != "" {
		t.Errorf("SelectedText after Clear should be empty; got %q", got)
	}
}

// TestSelection_BackwardDragNormalizes — dragging from a later position
// back to an earlier one should produce text in document order, not
// reversed. (Mirrors crush's getHighlightRange normalization.)
func TestSelection_BackwardDragNormalizes(t *testing.T) {
	l := makeListWithItems("alpha beta gamma")
	// Anchor at col 11 ("gamma"), drag back to col 0 ("alpha").
	l.HandleMouseDown(11, 0, 1)
	l.HandleMouseDrag(0, 0)
	got := l.SelectedText()
	want := "alpha beta "
	if got != want {
		t.Errorf("backward drag should still produce forward text\n  got:  %q\n  want: %q", got, want)
	}
}

// noSelectableItem — a fake transcript-chrome row (separator / spinner)
// that opts out of selection via the NoSelectable interface.
type noSelectableItem struct{ content string }

func (n *noSelectableItem) Render(int) string { return n.content }
func (n *noSelectableItem) IsNoSelect() bool  { return true }

// TestSelection_DragBelowViewportAutoScrolls — dragging past the bottom
// edge while content extends further down should auto-scroll the list
// (matching iTerm2 / VS Code rubber-band behavior). The anchor is on a
// row that scrolls OFF — but it stays tracked because anchors are
// stored by item index, not screen y (metis's scrolledOff equivalent).
func TestSelection_DragBelowViewportAutoScrolls(t *testing.T) {
	// Build a list with more rows than fit in the viewport. Height=10
	// shows ~10 rows; we add 30, so plenty to scroll through.
	items := make([]Item, 30)
	for i := range items {
		items[i] = &staticItem{content: "row " + string(rune('a'+i%26))}
	}
	l := NewList(items...)
	l.SetSize(80, 10)

	// Start drag on row 0 (visible at top), then drag below viewport.
	l.HandleMouseDown(0, 0, 1)
	beforeOffset := l.offsetIdx
	// y = 30 is far below height=10 — should trigger max-step autoscroll.
	l.HandleMouseDrag(0, 30)
	if l.offsetIdx <= beforeOffset {
		t.Errorf("expected list to autoscroll past offset %d; got %d",
			beforeOffset, l.offsetIdx)
	}
	// Anchor must still be on item 0 even though item 0 scrolled off.
	if l.sel.Anchor.ItemIdx != 0 {
		t.Errorf("anchor should remain on item 0 across autoscroll; got %d",
			l.sel.Anchor.ItemIdx)
	}
	if !l.HasSelection() {
		t.Errorf("selection should still be live after autoscroll")
	}
}

// TestSelection_NoSelectItemSkipped — drag that spans across a noSelect
// row (e.g. the timestamp separator between two chat turns) should:
//   1. include the surrounding selectable rows in the clipboard,
//   2. NOT include the noSelect row's content,
//   3. NOT highlight the noSelect row visually.
func TestSelection_NoSelectItemSkipped(t *testing.T) {
	l := NewList(
		&staticItem{content: "first row"},
		&noSelectableItem{content: "===separator==="},
		&staticItem{content: "third row"},
	)
	l.SetSize(80, 30)

	// Drag from (item 0, line 0, col 0) to (item 2, line 0, col 9).
	l.HandleMouseDown(0, 0, 1)
	l.HandleMouseDrag(9, 2)
	got := l.SelectedText()
	want := "first row\nthird row"
	if got != want {
		t.Errorf("noSelect row should be skipped in clipboard payload\n  got:  %q\n  want: %q", got, want)
	}

	// The noSelect row's content must NOT have the highlight escape
	// painted over it. (Plain compare on Render output.)
	rendered := l.Render()
	sepLine := "===separator==="
	if !strings.Contains(rendered, sepLine) {
		t.Fatalf("separator should be visible in render; got:\n%s", ansi.Strip(rendered))
	}
	// Look for the highlight escape directly preceding the separator
	// content — if present, that's a regression.
	if strings.Contains(rendered, "\x1b[48;5;238m===separator===") {
		t.Errorf("noSelect row was highlighted; got:\n%s", rendered)
	}
}

// TestSelection_NoSelectItemRejectsClick — single-click on a noSelect
// row must return false (caller falls through to other handlers, e.g.
// click-to-copy can still own the row even when selection opts out).
func TestSelection_NoSelectItemRejectsClick(t *testing.T) {
	l := NewList(
		&staticItem{content: "selectable"},
		&noSelectableItem{content: "chrome"},
	)
	l.SetSize(80, 30)
	if l.HandleMouseDown(0, 1, 1) {
		t.Errorf("click on noSelect row should return false")
	}
	if l.HasSelection() {
		t.Errorf("click on noSelect row must not start a selection")
	}
}

// TestApplySelectionToLine_Standalone — bypass the state machine,
// just verify the col-precise highlight wrapping. ANSI-aware: a styled
// input must keep its colors AND get reverse video on the cut range.
func TestApplySelectionToLine_Standalone(t *testing.T) {
	l := makeListWithItems("just one item")
	l.sel = selectionState{
		Anchor:     SelectionPoint{ItemIdx: 0, Line: 0, Col: 5},
		Focus:      SelectionPoint{ItemIdx: 0, Line: 0, Col: 8},
		HasContent: true,
	}
	plain := "hello world"
	got := l.applySelectionToLine(plain, 0, 0)
	if !strings.Contains(got, "\x1b[48;5;238m") || !strings.Contains(got, "\x1b[49m") {
		t.Errorf("missing solid-bg SGR wrapping; got %q", got)
	}
	// The plain text must round-trip when ANSI is stripped.
	if ansi.Strip(got) != plain {
		t.Errorf("ansi-strip of highlighted line should equal original; got %q", ansi.Strip(got))
	}
}

// TestWordBoundsAtCol — covers the three-class double-click model
// (whitespace / word / punctuation). Mirrors Claude Code's iTerm2-
// compatible behavior: paths and identifiers stay together,
// punctuation runs select as a unit, double-click on a space selects
// the whitespace run.
func TestWordBoundsAtCol(t *testing.T) {
	cases := []struct {
		name    string
		line    string
		col     int
		s, e    int
		comment string
	}{
		// Word-class basics.
		{"first-word-mid", "hello world today", 2, 0, 5, "inside hello"},
		{"second-word-start", "hello world today", 6, 6, 11, "start of world"},
		{"second-word-end", "hello world today", 10, 6, 11, "end of world"},
		{"trailing", "hello", 4, 0, 5, "last char of last word"},
		{"empty", "", 0, 0, 0, "empty line"},

		// Whitespace-class run (CC parity — double-click on a space
		// selects the whitespace run, not (col, col)).
		{"single-space", "hello world today", 5, 5, 6, "single space between words"},
		{"multi-space", "ab    cd", 4, 2, 6, "4-space run"},

		// Path / identifier — the marquee improvement vs the old
		// `unicode.IsSpace`-only algorithm. `/.\-+~_\\` are word-class
		// so the whole path stays together.
		{"abs-path", "see /usr/bin/bash now", 8, 4, 17, "double-click in /usr/bin/bash"},
		{"home-path", "edit ~/.metis/config.toml file", 12, 5, 25, "tilde/dot path"},
		{"version", "tag v0.1.3-14-g15fc3ba ok", 5, 4, 22, "version+commit dash run"},

		// Punctuation-class run. `<=` is two consecutive punct chars,
		// so they form a run. `->` is mixed (word `-` + punct `>`)
		// because `-` is in WORD_CHAR — so double-click on `>` only
		// selects `>` itself, not the whole `->`. Same as CC.
		{"comparison", "if x <= y then", 6, 5, 7, "<= punct run"},
		{"arrow-tail", "go a -> b done", 6, 6, 7, "double-click `>` — `-` is word-class"},
		{"arrow-dash", "go a -> b done", 5, 5, 6, "double-click `-` — only `-` (word neighbors are space/punct)"},

		// Punctuation single char (other class neighbors).
		{"paren-start", "fn(arg)", 2, 2, 3, "lone `(`"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gs, ge := wordBoundsAtCol(tc.line, tc.col)
			if gs != tc.s || ge != tc.e {
				t.Errorf("(%s) wordBoundsAtCol(%q, %d) = (%d,%d); want (%d,%d)",
					tc.comment, tc.line, tc.col, gs, ge, tc.s, tc.e)
			}
		})
	}
}

// TestWordBoundsAtCol_WideChar — clicking on the trailing half of a
// CJK / emoji cell must step back to the head and select the whole
// CJK run. Without step-back the trailer would land on a class
// boundary or empty rune and select nothing useful.
func TestWordBoundsAtCol_WideChar(t *testing.T) {
	// "你好 world" — 你 occupies cols 0-1, 好 occupies cols 2-3,
	// space at col 4, "world" at cols 5-9.
	line := "你好 world"
	cases := []struct {
		col, ws, we int
		desc        string
	}{
		{0, 0, 4, "head of 你"},
		{1, 0, 4, "trailer of 你 — step back to 你"},
		{2, 0, 4, "head of 好"},
		{3, 0, 4, "trailer of 好 — step back"},
		{5, 5, 10, "first char of world"},
	}
	for _, tc := range cases {
		t.Run(tc.desc, func(t *testing.T) {
			gs, ge := wordBoundsAtCol(line, tc.col)
			if gs != tc.ws || ge != tc.we {
				t.Errorf("col %d (%s): got (%d,%d), want (%d,%d)",
					tc.col, tc.desc, gs, ge, tc.ws, tc.we)
			}
		})
	}
}
