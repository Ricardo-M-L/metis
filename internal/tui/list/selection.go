package list

// selection.go — mouse-driven text selection inside the list viewport.
//
// Mirrors crush's chat.go selection model (HandleMouseDown / Drag / Up,
// selectWord / selectLine, getHighlightRange) but without crush's
// dependency on ultraviolet's ScreenBuffer. crush draws each item into
// a cell-buffer and applies AttrReverse to selected cells; metis stays
// string-based and overlays the reverse SGR escape (`\x1b[7m` /
// `\x1b[27m`) using `ansi.Cut` for column-precise, ANSI-aware splitting.
//
// Coordinates: selection points are (itemIdx, lineWithinItem, col)
// where col is the **display width** column (East-Asian wide chars and
// emoji count as 2). This matches what `ansi.StringWidth` reports and
// what the user perceives as "the N-th column on screen".

import (
	"strings"
	"unicode"

	"github.com/charmbracelet/x/ansi"
)

// SelectionPoint is one end of a text selection.
type SelectionPoint struct {
	ItemIdx int // -1 means unset
	Line    int // 0-based line within the item's rendered content
	Col     int // 0-based display column
}

// selectionState holds mouse-down anchor + current drag focus. A live
// selection is one where Anchor.ItemIdx >= 0 and either Dragging is
// true (user still holding the button) or HasContent is true (a finished
// drag whose highlight should remain visible until ClearSelection).
//
// stagedAnchor + hasStaged implement the "preserve highlight" behavior:
// when HandleMouseDown fires while a finalized selection is on screen,
// we stage the new anchor instead of replacing the live state. The
// first HandleMouseDrag promotes the stage (clears old, starts new);
// HandleMouseUp without a drag discards the stage and the old
// selection stays visible. Without this, every random click on the
// transcript would wipe the user's just-selected text.
type selectionState struct {
	Anchor     SelectionPoint
	Focus      SelectionPoint
	Dragging   bool
	HasContent bool // set by HandleMouseUp when anchor != focus

	stagedAnchor SelectionPoint
	hasStaged    bool

	// anchorSpan, when set, makes drag extend the selection by
	// word-units (kind=spanWord) or line-units (kind=spanLine) rather
	// than character-by-character. Mirrors Claude Code's anchorSpan
	// — double-click+drag selects word-by-word.
	anchorSpan     spanRange
	hasAnchorSpan  bool
	anchorSpanKind spanKind
}

type spanKind int

const (
	spanNone spanKind = iota
	spanWord
	spanLine
)

// spanRange is the "frozen" word/line range from the initial double or
// triple click. drag extensions union the current word/line at focus
// with this range so the original selection always stays included
// even when the user drags backward past it.
type spanRange struct {
	Lo SelectionPoint
	Hi SelectionPoint
}

func emptySelectionState() selectionState {
	return selectionState{
		Anchor: SelectionPoint{ItemIdx: -1},
		Focus:  SelectionPoint{ItemIdx: -1},
	}
}

// HandleMouseDown begins a selection at the viewport-relative (x, y).
// Returns true when the click landed on a real list item; the caller
// uses the false return to fall through to other handlers (e.g. drag-
// the-scrollbar). For double/triple click, callers pass clickCount >= 2
// and the function snaps the selection to the word / line at (x, y).
//
// "Preserve highlight until next drag" — if there's a finalized
// selection (mouse-up after a drag, or a double/triple-click result),
// a fresh single-click does NOT clear it immediately. The pending
// anchor is staged in stagedAnchor; the moment the user actually
// drags (HandleMouseDrag) or the click count reaches 2/3, that
// staged anchor takes over and the old selection is cleared. Mirrors
// Claude Code's iTerm2-compatible "click-doesn't-touch-clipboard"
// behavior: cmd+C still works after a drag because the highlight is
// still there to copy.
func (l *List) HandleMouseDown(x, y, clickCount int) bool {
	itemIdx, itemY := l.ItemAtY(y)
	if itemIdx < 0 {
		return false
	}
	// noSelect rows (separators / spinners / chrome) — click is a no-op
	// for selection. Caller falls through to other handlers (e.g.
	// click-to-copy may still grab the row if it wants).
	if l.itemNoSelect(itemIdx) {
		return false
	}
	switch clickCount {
	case 2:
		return l.selectWord(itemIdx, itemY, x)
	case 3:
		return l.selectLine(itemIdx, itemY)
	}
	// Single-click. 2026-05-23: user feedback — previous behavior
	// preserved the prior selection on the screen on the theory that
	// cmd+C should still work after drag. Empirically that conflicts
	// with the "click-anywhere-to-deselect" intuition every terminal
	// app trains: select text, click somewhere else, selection clears.
	// Since MouseReleaseMsg already copied the dragged selection to
	// the clipboard (copySelectionAndReport runs before this next
	// click), the only thing the staged-anchor preservation bought
	// was a visible highlight — and that visible highlight is
	// exactly what the user wants gone when they click elsewhere.
	//
	// New behavior: always clear prior selection on single-click
	// then start a fresh drag-tracking session anchored at this
	// click. If the click is a no-drag click (just a tap to
	// deselect), HandleMouseUp leaves the empty selection alone.
	l.sel = selectionState{
		Anchor:   SelectionPoint{ItemIdx: itemIdx, Line: itemY, Col: x},
		Focus:    SelectionPoint{ItemIdx: itemIdx, Line: itemY, Col: x},
		Dragging: true,
	}
	return true
}

// HandleMouseDrag updates the selection focus to (x, y). Caller must
// have already issued HandleMouseDown; otherwise this is a no-op (we
// don't start drags out of nowhere).
//
// Promotion of staged anchor: if a previous selection was preserved
// across a click (HandleMouseDown saw HasContent and parked the new
// anchor in stagedAnchor), the first drag promotes that anchor —
// the old selection is cleared and a fresh drag begins.
//
// anchorSpan extension: when the selection started as a double-click
// (spanWord) or triple-click (spanLine), drag extends in word or line
// units instead of character-precise. The current word/line at focus
// is unioned with the anchor span so the original double/triple-click
// content stays in even when the user drags backward.
func (l *List) HandleMouseDrag(x, y int) bool {
	if l.sel.hasStaged {
		// Promote: the user is now actually dragging from the staged
		// anchor. Wipe the prior selection and start fresh.
		anchor := l.sel.stagedAnchor
		l.sel = selectionState{
			Anchor:   anchor,
			Focus:    anchor,
			Dragging: true,
		}
	}
	if !l.sel.Dragging {
		return false
	}
	// Auto-scroll: drag past the top edge scrolls up, past the bottom
	// edge scrolls down. Step size grows with how far past the edge the
	// pointer is — small overshoot = 1 line/event (gentle), large
	// overshoot = up to 4 lines/event (fast). The OS-level mouse-motion
	// rate gives us natural pacing; no timer needed.
	//
	// Anchors are stored by (itemIdx, line, col), which is independent
	// of scroll offset — so when the visible window shifts, the anchor
	// "scrolls off" automatically without losing track. This is metis's
	// equivalent of crush's scrolledOff buffer: ScrollBy moves
	// offsetIdx/offsetLine, the anchor coords stay frozen, and the
	// highlight on the now-visible portion of the selection just keeps
	// painting itself from applySelectionToLine.
	if y < 0 {
		step := -y
		if step > 4 {
			step = 4
		}
		l.ScrollBy(-step)
	} else if y >= l.height {
		step := y - l.height + 1
		if step > 4 {
			step = 4
		}
		l.ScrollBy(step)
	}
	itemIdx, itemY := l.ItemAtY(y)
	if itemIdx < 0 {
		// Drag still out of view after scroll (e.g. already at the top
		// or bottom of the buffer). Clamp the focus to the visible
		// extreme so the selection doesn't lose its endpoint.
		startIdx, endIdx := l.VisibleItemIndices()
		if y < 0 {
			itemIdx = startIdx
			itemY = 0
		} else {
			itemIdx = endIdx
			it := l.getItem(itemIdx)
			itemY = max(it.height-1, 0)
		}
	}
	if l.sel.hasAnchorSpan {
		l.extendSpanSelection(itemIdx, itemY, x)
	} else {
		l.sel.Focus = SelectionPoint{ItemIdx: itemIdx, Line: itemY, Col: x}
	}
	return true
}

// HandleMouseUp finalizes the drag. Returns true when the resulting
// selection has visible content (so the caller can copy + show "copied
// N chars"); returns false when the user just clicked without dragging
// (preserved selection stays untouched in that case).
func (l *List) HandleMouseUp() bool {
	// Bare click on a preserved selection — discard the stage, keep
	// the existing highlight intact. Caller treats this as "no drag,
	// nothing new to copy" but the user can still cmd+C the prior
	// selection (clipboard already has it from the previous mouse-up).
	if l.sel.hasStaged && !l.sel.Dragging {
		l.sel.hasStaged = false
		return false
	}
	if !l.sel.Dragging {
		return false
	}
	l.sel.Dragging = false
	// "Has content" iff anchor and focus are not the exact same point.
	a, f := l.sel.Anchor, l.sel.Focus
	if a.ItemIdx == f.ItemIdx && a.Line == f.Line && a.Col == f.Col &&
		!l.sel.hasAnchorSpan {
		l.sel = emptySelectionState()
		return false
	}
	l.sel.HasContent = true
	return true
}

// ClearSelection wipes any active or finalized selection.
func (l *List) ClearSelection() { l.sel = emptySelectionState() }

// HasSelection reports whether the list currently has a visible
// highlight (drag in progress or completed).
func (l *List) HasSelection() bool {
	a, f := l.sel.Anchor, l.sel.Focus
	if a.ItemIdx < 0 {
		return false
	}
	return l.sel.Dragging || l.sel.HasContent ||
		!(a.ItemIdx == f.ItemIdx && a.Line == f.Line && a.Col == f.Col)
}

// SelectionRange returns the normalized [start, end] selection where
// start ≤ end in document order. Returns -1 in startItemIdx when no
// selection is active.
func (l *List) SelectionRange() (start, end SelectionPoint) {
	if !l.HasSelection() {
		return SelectionPoint{ItemIdx: -1}, SelectionPoint{ItemIdx: -1}
	}
	a, f := l.sel.Anchor, l.sel.Focus
	if pointBefore(a, f) {
		return a, f
	}
	return f, a
}

// SelectedText returns the plain-text content (ANSI stripped) of the
// current selection across all spanned items, joined by "\n". Empty
// when no selection.
func (l *List) SelectedText() string {
	start, end := l.SelectionRange()
	if start.ItemIdx < 0 {
		return ""
	}

	var b strings.Builder
	for idx := start.ItemIdx; idx <= end.ItemIdx; idx++ {
		// noSelect items contribute nothing to the clipboard payload —
		// drag-spanning a separator / spinner just stitches the rows
		// before and after together.
		if l.itemNoSelect(idx) {
			continue
		}
		item := l.getItem(idx)
		lines := strings.Split(item.content, "\n")

		var fromLine, toLine, fromCol, toCol int
		switch {
		case idx == start.ItemIdx && idx == end.ItemIdx:
			fromLine, fromCol = start.Line, start.Col
			toLine, toCol = end.Line, end.Col
		case idx == start.ItemIdx:
			fromLine, fromCol = start.Line, start.Col
			toLine, toCol = len(lines)-1, -1
		case idx == end.ItemIdx:
			fromLine, fromCol = 0, 0
			toLine, toCol = end.Line, end.Col
		default:
			fromLine, fromCol = 0, 0
			toLine, toCol = len(lines)-1, -1
		}

		for li := fromLine; li <= toLine && li < len(lines); li++ {
			line := lines[li]
			lo := 0
			if li == fromLine {
				lo = fromCol
			}
			hi := ansi.StringWidth(line)
			if li == toLine && toCol >= 0 {
				hi = toCol
			}
			if lo >= hi {
				if li < toLine {
					b.WriteString("\n")
				}
				continue
			}
			seg := ansi.Strip(ansi.Cut(line, lo, hi))
			b.WriteString(seg)
			if li < toLine || idx < end.ItemIdx {
				b.WriteString("\n")
			}
		}
	}
	return b.String()
}

// selectionBgOpen / selectionBgClose paint the highlight as a SOLID
// background (xterm-256 colour 238 — the same dim grey Claude Code
// paints in iTerm2 / VS Code terminal). We used to emit `\x1b[7m`
// (reverse video) but that flips whatever fg/bg pair the cell already
// has — so a syntax-highlighted assistant message gets a different
// "selected" appearance than a plain bash row, and reverse on top of
// reverse cancels back to normal in some cells.
//
// Solid bg is uniform regardless of the underlying content. `\x1b[49m`
// resets the bg attribute only (don't use `\x1b[0m` — that would also
// wipe fg / bold from inner content). Outer wrappers survive
// `ansi.Cut`'s state-aware splits because each segment carries its own
// SGR prelude.
const (
	selectionBgOpen  = "\x1b[48;5;238m"
	selectionBgClose = "\x1b[49m"
)

// applySelectionToLine returns the line with the in-selection columns
// wrapped in solid-bg SGR. lineY is the line within the **item** (not
// viewport). When nothing on this line is in range, returns the input
// unchanged.
//
// Three cases for col bounds on this line:
//   - This line is BEFORE the selection's first line in this item, or
//     AFTER its last line → no highlight.
//   - This line is the FIRST in the selection (within the item span):
//     highlight from selStartCol to end-of-line.
//   - This line is the LAST in the selection: highlight from 0 to
//     selEndCol.
//   - This line is in between (or single-line selection): clamp to the
//     intersection of [selStartCol, selEndCol] and [0, lineWidth].
func (l *List) applySelectionToLine(line string, itemIdx, lineY int) string {
	if !l.HasSelection() {
		return line
	}
	// noSelect items render plain even when the drag spans across them —
	// the highlight visually skips the row, matching what the user gets
	// when they copy (the row is also excluded from SelectedText).
	if l.itemNoSelect(itemIdx) {
		return line
	}
	start, end := l.SelectionRange()
	if itemIdx < start.ItemIdx || itemIdx > end.ItemIdx {
		return line
	}

	lineWidth := ansi.StringWidth(line)

	// Determine col range on THIS line.
	var lo, hi int
	switch {
	case itemIdx == start.ItemIdx && itemIdx == end.ItemIdx:
		// Single-item selection.
		if lineY < start.Line || lineY > end.Line {
			return line
		}
		lo = 0
		hi = lineWidth
		if lineY == start.Line {
			lo = start.Col
		}
		if lineY == end.Line {
			hi = end.Col
		}
	case itemIdx == start.ItemIdx:
		if lineY < start.Line {
			return line
		}
		lo = 0
		hi = lineWidth
		if lineY == start.Line {
			lo = start.Col
		}
	case itemIdx == end.ItemIdx:
		if lineY > end.Line {
			return line
		}
		lo = 0
		hi = lineWidth
		if lineY == end.Line {
			hi = end.Col
		}
	default:
		// Middle item — every line fully highlighted.
		lo = 0
		hi = lineWidth
	}

	if lo >= hi || hi <= 0 {
		return line
	}
	if lo > lineWidth {
		return line
	}
	if hi > lineWidth {
		hi = lineWidth
	}

	before := ansi.Cut(line, 0, lo)
	mid := ansi.Cut(line, lo, hi)
	after := ansi.Cut(line, hi, lineWidth)
	return before + selectionBgOpen + reinjectSelectionBg(mid) + selectionBgClose + after
}

// reinjectSelectionBg keeps the selection's solid grey background
// applied across every cell of the mid-segment, even when the
// underlying content emits its own SGR resets.
//
// Without this fix, lipgloss-styled chat items render as a sequence
// of `\x1b[<style>m...\x1b[0m` chunks. Wrapping the whole mid with a
// single `\x1b[48;5;238m...\x1b[49m` envelope LOOKS right in code but
// the inner `\x1b[0m` resets ALSO clear the bg attribute mid-stream,
// so only the cells before the first reset show the grey. The user
// sees fragmented "checkerboard" highlighting instead of a uniform
// selection band (the 2026-05-10 image #2 user report).
//
// Strategy: walk the bytes and re-emit selectionBgOpen after every
// CSI-m sequence that includes a `0` (reset all) or `49` (bg reset)
// parameter. Compound forms like `\x1b[0;1;31m` also strip bg, so
// they trigger the reinjection too.
//
// We deliberately do NOT touch the caller-supplied prefix/suffix
// envelope — selectionBgOpen sits OUTSIDE this function's input and
// selectionBgClose runs after, so callers don't double-wrap.
func reinjectSelectionBg(mid string) string {
	if mid == "" {
		return mid
	}
	var b strings.Builder
	b.Grow(len(mid) + 16)
	i := 0
	for i < len(mid) {
		// Look for the next CSI ("\x1b[").
		idx := strings.Index(mid[i:], "\x1b[")
		if idx < 0 {
			b.WriteString(mid[i:])
			break
		}
		b.WriteString(mid[i : i+idx])
		csiStart := i + idx
		// Find the final byte of the CSI (anything in 0x40-0x7E).
		j := csiStart + 2
		for j < len(mid) {
			c := mid[j]
			if c >= 0x40 && c <= 0x7E {
				break
			}
			j++
		}
		if j >= len(mid) {
			b.WriteString(mid[csiStart:])
			break
		}
		csi := mid[csiStart : j+1]
		b.WriteString(csi)
		// CSI-m sequence with reset/bg-reset semantics → re-arm bg.
		// Final byte 'm' identifies the SGR family; param body is
		// between "[" and "m". Empty body == "[m" == reset all.
		if mid[j] == 'm' && csiKillsSelectionBg(csi) {
			b.WriteString(selectionBgOpen)
		}
		i = j + 1
	}
	return b.String()
}

// csiKillsSelectionBg reports whether an SGR sequence (CSI ... m)
// would clear the selection's background attribute. The sequence is
// the exact byte slice including the leading "\x1b[" and trailing
// "m". Cases that kill bg:
//   - "\x1b[m"            bare CSI = reset all
//   - "\x1b[0m"           explicit reset all
//   - "\x1b[49m"          reset bg only
//   - "\x1b[0;...m"       compound starting with 0
//   - "\x1b[...;0m"       compound ending with 0
//   - "\x1b[...;49;...m"  any param == 49
//
// Anything else (e.g. "\x1b[31m" pure fg-red, "\x1b[1m" bold) leaves
// bg alone and we don't reinject — keeps the fast path lean for
// typical syntax-highlighted lines.
func csiKillsSelectionBg(csi string) bool {
	// Strip "\x1b[" prefix and "m" suffix → just the param body.
	if len(csi) < 3 {
		return false
	}
	body := csi[2 : len(csi)-1]
	if body == "" || body == "0" {
		return true // bare or explicit reset
	}
	for _, p := range strings.Split(body, ";") {
		switch strings.TrimSpace(p) {
		case "0", "49":
			return true
		}
	}
	return false
}

// selectWord snaps the selection to the word-grapheme boundaries
// containing (lineY, colX). Triggered by double-click. Three-class
// model (whitespace / word / punctuation) — see charClass.
//
// Sets Dragging=true so a subsequent HandleMouseDrag without an
// intervening MouseUp extends the selection word-by-word
// (anchorSpan=spanWord). MouseUp finalizes with HasContent=true
// regardless of drag distance — a bare double-click already has a
// non-empty word selection.
func (l *List) selectWord(itemIdx, lineY, colX int) bool {
	item := l.getItem(itemIdx)
	lines := strings.Split(item.content, "\n")
	if lineY < 0 || lineY >= len(lines) {
		return false
	}
	plain := ansi.Strip(lines[lineY])
	startCol, endCol := wordBoundsAtCol(plain, colX)
	span := spanRange{
		Lo: SelectionPoint{ItemIdx: itemIdx, Line: lineY, Col: startCol},
		Hi: SelectionPoint{ItemIdx: itemIdx, Line: lineY, Col: endCol},
	}
	l.sel = selectionState{
		Anchor:         span.Lo,
		Focus:          span.Hi,
		Dragging:       true,
		HasContent:     true,
		anchorSpan:     span,
		hasAnchorSpan:  true,
		anchorSpanKind: spanWord,
	}
	return true
}

// selectLine selects the entire line at (itemIdx, lineY). Triggered by
// triple-click. Sets anchorSpan so subsequent drag extends line by
// line (typical iTerm2 / xterm behavior).
func (l *List) selectLine(itemIdx, lineY int) bool {
	item := l.getItem(itemIdx)
	lines := strings.Split(item.content, "\n")
	if lineY < 0 || lineY >= len(lines) {
		return false
	}
	width := ansi.StringWidth(lines[lineY])
	span := spanRange{
		Lo: SelectionPoint{ItemIdx: itemIdx, Line: lineY, Col: 0},
		Hi: SelectionPoint{ItemIdx: itemIdx, Line: lineY, Col: width},
	}
	l.sel = selectionState{
		Anchor:         span.Lo,
		Focus:          span.Hi,
		Dragging:       true,
		HasContent:     true,
		anchorSpan:     span,
		hasAnchorSpan:  true,
		anchorSpanKind: spanLine,
	}
	return true
}

// extendSpanSelection grows the selection to cover the union of the
// initial anchorSpan (set by selectWord/selectLine) and the
// word/line at the new focus point. Mirrors Claude Code's behavior:
// double-click + drag extends by whole words, triple-click + drag by
// whole lines. The original word/line stays selected even if the user
// drags BACKWARD past it.
func (l *List) extendSpanSelection(itemIdx, lineY, colX int) {
	if !l.sel.hasAnchorSpan {
		return
	}
	item := l.getItem(itemIdx)
	lines := strings.Split(item.content, "\n")
	if lineY < 0 || lineY >= len(lines) {
		return
	}

	var focusLo, focusHi SelectionPoint
	switch l.sel.anchorSpanKind {
	case spanWord:
		plain := ansi.Strip(lines[lineY])
		s, e := wordBoundsAtCol(plain, colX)
		if s == e {
			// Empty match (out of range / whitespace edge) — use bare
			// point so user dragging into trailing whitespace still
			// shrinks/grows naturally.
			focusLo = SelectionPoint{ItemIdx: itemIdx, Line: lineY, Col: colX}
			focusHi = focusLo
		} else {
			focusLo = SelectionPoint{ItemIdx: itemIdx, Line: lineY, Col: s}
			focusHi = SelectionPoint{ItemIdx: itemIdx, Line: lineY, Col: e}
		}
	case spanLine:
		focusLo = SelectionPoint{ItemIdx: itemIdx, Line: lineY, Col: 0}
		focusHi = SelectionPoint{ItemIdx: itemIdx, Line: lineY, Col: ansi.StringWidth(lines[lineY])}
	default:
		focusLo = SelectionPoint{ItemIdx: itemIdx, Line: lineY, Col: colX}
		focusHi = focusLo
	}

	// Union(anchorSpan, focusSpan). The lower-numbered point becomes
	// Anchor, the higher becomes Focus — SelectionRange normalizes
	// anyway, but stable order helps tests + debug printing.
	lo := l.sel.anchorSpan.Lo
	hi := l.sel.anchorSpan.Hi
	if pointBefore(focusLo, lo) {
		lo = focusLo
	}
	if pointBefore(hi, focusHi) {
		hi = focusHi
	}
	l.sel.Anchor = lo
	l.sel.Focus = hi
}

// charClass implements Claude Code's three-class double-click model
// (selection.ts:142-155). Cells of the same class as the clicked one
// are part of the same "word"; class boundaries terminate the word.
//
// Class 0 — whitespace (space, tab, newline). Double-click on a space
//
//	selects the whitespace run.
//
// Class 1 — word characters: letters (any script), digits, and the
//
//	punctuation that iTerm2 treats as part of a word by default
//	(`/-+~_.\\`). This is the difference that makes
//	double-clicking `/usr/bin/bash` or `~/.metis/config.toml`
//	select the WHOLE path — same behavior macOS terminal users
//	already have muscle memory for.
//
// Class 2 — other punctuation (parens, quotes, operators…). Double-
//
//	click on `->` selects `->`; on `(` selects `(`.
type charClass int

const (
	classSpace      charClass = 0
	classWord       charClass = 1
	classPunct      charClass = 2
	classOutOfRange charClass = -1
)

// classifyRune returns which class the rune belongs to.
func classifyRune(r rune) charClass {
	if unicode.IsSpace(r) {
		return classSpace
	}
	// Letters (any script — \p{L}), digits (\p{N}), underscore, and
	// path/identifier punctuation. Mirrors CC's WORD_CHAR pattern
	// `[\p{L}\p{N}_/.\-+~\\]`.
	if unicode.IsLetter(r) || unicode.IsDigit(r) {
		return classWord
	}
	switch r {
	case '_', '/', '.', '-', '+', '~', '\\':
		return classWord
	}
	return classPunct
}

// runeDisplayWidth is a shim around ansi.StringWidth for a single rune.
// Allocating a 1-char string per rune is wasteful in tight loops but
// the scan is O(line length) once per double-click — fine.
func runeDisplayWidth(r rune) int {
	return ansi.StringWidth(string(r))
}

// wordBoundsAtCol walks the (already ANSI-stripped) line and returns
// the [start, end) display-column range of the word containing col.
// Three-class semantics — see charClass docs.
//
// "step back to wide-char head" handling: when the click lands on the
// trailing half of a CJK / emoji (display width 2) cell, we treat it
// as a click on the head cell so the word scan sees the actual rune
// class. Without this, a click on the right half of `中` would land
// on a class boundary and double-click would select nothing useful.
func wordBoundsAtCol(plain string, col int) (start, end int) {
	if col < 0 || plain == "" {
		return 0, 0
	}
	// First pass: collect runes with their starting column. We need
	// random access so we can step back from a wide-char tail.
	type runeAt struct {
		r       rune
		col     int
		w       int
		class   charClass
		isTrail bool // synthesized: trailing half of width-2 rune
	}
	cells := make([]runeAt, 0, len(plain))
	curCol := 0
	for _, r := range plain {
		w := runeDisplayWidth(r)
		cls := classifyRune(r)
		cells = append(cells, runeAt{r: r, col: curCol, w: w, class: cls})
		// Insert a phantom trailer cell for width-2 runes so col
		// lookups land somewhere; we'll never seek the trailer's class
		// directly — see step-back below.
		if w == 2 {
			cells = append(cells, runeAt{
				r: r, col: curCol + 1, w: 0, class: cls, isTrail: true,
			})
		}
		curCol += w
	}
	totalWidth := curCol

	// Find the cell at col. If col is past EOL, return (totalWidth,
	// totalWidth) — caller treats that as "no word here".
	if col >= totalWidth {
		return totalWidth, totalWidth
	}

	hitIdx := -1
	for i, c := range cells {
		if c.col == col {
			hitIdx = i
			break
		}
	}
	if hitIdx == -1 {
		return col, col
	}
	// Step back from a wide-char tail to its head (CJK / emoji).
	if cells[hitIdx].isTrail && hitIdx > 0 {
		hitIdx--
	}

	clickClass := cells[hitIdx].class

	// Expand left while same class, skipping trailers.
	lo := hitIdx
	for lo > 0 {
		prev := cells[lo-1]
		if prev.isTrail {
			// trailer's "head" is at lo-2; take that index's class
			if lo-2 < 0 {
				break
			}
			if cells[lo-2].class != clickClass {
				break
			}
			lo -= 2
			continue
		}
		if prev.class != clickClass {
			break
		}
		lo--
	}
	// Expand right while same class, including trailers (they belong
	// to the wide-char head we just included).
	hi := hitIdx
	for hi < len(cells)-1 {
		next := cells[hi+1]
		if next.isTrail {
			hi++
			continue
		}
		if next.class != clickClass {
			break
		}
		hi++
	}

	startCol := cells[lo].col
	endCol := cells[hi].col + cells[hi].w
	if cells[hi].isTrail {
		// hi landed on a trailer; the head was at hi-1 with full
		// width 2 → endCol = headCol + 2.
		endCol = cells[hi-1].col + cells[hi-1].w
	}
	return startCol, endCol
}

// pointBefore reports whether a comes strictly before b in document
// order ((itemIdx, line, col) lexicographic).
func pointBefore(a, b SelectionPoint) bool {
	if a.ItemIdx != b.ItemIdx {
		return a.ItemIdx < b.ItemIdx
	}
	if a.Line != b.Line {
		return a.Line < b.Line
	}
	return a.Col < b.Col
}
