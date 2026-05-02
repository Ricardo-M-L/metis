// Package list provides a virtualized chat-surface list — items lazy-render,
// only the visible window is composed into a string each frame, and an
// AtBottom-cache keeps repeated viewport queries cheap.
//
// Heavily based on Charm's `crush/internal/ui/list/list.go` (650 lines,
// MIT License). Adaptations for metis:
//   - Compatible with bubbletea v1.3.10 + lipgloss v1.1 (no v2 imports).
//   - AtBottom cache: crush re-walks offsetIdx → end on every call;
//     metis caches by (offsetIdx, offsetLine, itemCount, height, width, gap)
//     since AtBottom is hit ~7 times per frame from the chat surface.
//   - Adds ScrollPercent / TotalLineCount / SetMouseWheelDelta required by
//     the metis scrollbar gutter and mouse wheel forward path.
//   - filterable.go intentionally NOT ported (chat surface has no
//     filter feature; the slash-command palette / @-mention / Ctrl+R
//     history search each have their own dedicated overlays).
//   - highlight.go intentionally NOT ported (chat has no range-selection
//     highlight feature; would have required `ultraviolet` cell-buffer
//     library that the rest of metis does not use).
package list

import (
	"strings"
)

// List represents a list of items that can be lazily rendered. A list is
// always rendered like a chat conversation where items are stacked vertically
// from top to bottom.
type List struct {
	// Viewport size
	width, height int

	// Items in the list
	items []Item

	// Gap between items (0 or less means no gap)
	gap int

	// show list in reverse order
	reverse bool

	// Focus and selection state
	focused     bool
	selectedIdx int // The current selected index -1 means no selection

	// offsetIdx is the index of the first visible item in the viewport.
	offsetIdx int
	// offsetLine is the number of lines of the item at offsetIdx that are
	// scrolled out of view (above the viewport).
	// It must always be >= 0.
	offsetLine int

	// renderCallbacks is a list of callbacks to apply when rendering items.
	renderCallbacks []func(idx, selectedIdx int, item Item) Item

	// mouseWheelDelta is how many lines a single wheel detent scrolls.
	// Defaults to 1 (pixel-precise) — matches viewport.Model's tunable.
	mouseWheelDelta int

	// AtBottom cache: stable for any sequence of repeated AtBottom() calls
	// that don't mutate items / offset / size. Crush re-walks O(visible)
	// per AtBottom; metis hits AtBottom ~7 times per frame from
	// chat-surface auto-scroll detection, so caching is meaningful.
	atBottomCacheValid bool
	atBottomCachedKey  atBottomCacheKey
	atBottomCachedRes  bool
}

// atBottomCacheKey captures every input that AtBottom's calculation reads.
// Equality of two keys means the answer is identical → return cached.
type atBottomCacheKey struct {
	offsetIdx  int
	offsetLine int
	itemCount  int
	height     int
	width      int
	gap        int
}

// renderedItem holds the rendered content and height of an item.
type renderedItem struct {
	content string
	height  int
}

// NewList creates a new lazy-loaded list.
func NewList(items ...Item) *List {
	l := new(List)
	l.items = items
	l.selectedIdx = -1
	l.mouseWheelDelta = 1
	return l
}

// RenderCallback defines a function that can modify an item before it is
// rendered.
type RenderCallback func(idx, selectedIdx int, item Item) Item

// RegisterRenderCallback registers a callback to be called when rendering
// items. This can be used to modify items before they are rendered.
func (l *List) RegisterRenderCallback(cb RenderCallback) {
	l.renderCallbacks = append(l.renderCallbacks, cb)
	l.invalidateAtBottomCache()
}

// SetSize sets the size of the list viewport.
func (l *List) SetSize(width, height int) {
	if l.width != width || l.height != height {
		l.invalidateAtBottomCache()
	}
	l.width = width
	l.height = height
}

// SetGap sets the gap between items.
func (l *List) SetGap(gap int) {
	if l.gap != gap {
		l.invalidateAtBottomCache()
	}
	l.gap = gap
}

// Gap returns the gap between items.
func (l *List) Gap() int {
	return l.gap
}

// invalidateAtBottomCache forces the next AtBottom() call to recompute.
// We invalidate explicitly for renderCallbacks changes (which influence
// item.Render via callback chain); structural mutations (SetItems / Append
// / scroll / resize) flow through the cache key naturally and don't need
// an explicit clear.
func (l *List) invalidateAtBottomCache() {
	l.atBottomCacheValid = false
}

// AtBottom returns whether the list is showing the last item at the bottom.
//
// Cached by a tuple of all inputs the calculation reads. Repeated calls
// at the same scroll/size return in O(1); a single fresh call still
// pays O(visible items) for the height accumulation walk (capped by
// l.height — see the early return at totalHeight > l.height).
func (l *List) AtBottom() bool {
	if len(l.items) == 0 {
		return true
	}

	key := atBottomCacheKey{
		offsetIdx:  l.offsetIdx,
		offsetLine: l.offsetLine,
		itemCount:  len(l.items),
		height:     l.height,
		width:      l.width,
		gap:        l.gap,
	}
	if l.atBottomCacheValid && l.atBottomCachedKey == key {
		return l.atBottomCachedRes
	}

	// Calculate the height from offsetIdx to the end.
	//
	// Bug history (metis fix): crush's original implementation
	// short-circuited with `if totalHeight > l.height { break }`, but
	// the `totalHeight > height` test ignored offsetLine. After
	// ScrollToBottom — which sets offsetLine = lastOffsetItem's
	// lineOffset, possibly several rows worth — the loop would
	// (correctly) detect that the items from offsetIdx onward sum to
	// more than height, but (incorrectly) ignore that offsetLine of
	// those rows are already scrolled out of view. Result: AtBottom
	// reported false right after ScrollToBottom, breaking auto-scroll.
	//
	// Fix: compare `totalHeight - offsetLine` against height, both for
	// the early-break test and the final return. This matches the
	// post-loop formula in crush's original code, just applied at
	// every iteration so we still get the early-exit performance win.
	var totalHeight int
	atBottom := true
	for idx := l.offsetIdx; idx < len(l.items); idx++ {
		item := l.getItem(idx)
		itemHeight := item.height
		if l.gap > 0 && idx > l.offsetIdx {
			itemHeight += l.gap
		}
		totalHeight += itemHeight
		if totalHeight-l.offsetLine > l.height {
			// Items beyond what the viewport can hold — not at bottom.
			atBottom = false
			break
		}
	}
	if atBottom {
		// Loop fell through without ever exceeding the viewport. The
		// final answer reduces to "did the visible items fit?"; same
		// formula as the loop check, just stated explicitly.
		atBottom = totalHeight-l.offsetLine <= l.height
	}

	l.atBottomCacheValid = true
	l.atBottomCachedKey = key
	l.atBottomCachedRes = atBottom
	return atBottom
}

// SetReverse shows the list in reverse order.
func (l *List) SetReverse(reverse bool) {
	if l.reverse != reverse {
		l.invalidateAtBottomCache()
	}
	l.reverse = reverse
}

// Width returns the width of the list viewport.
func (l *List) Width() int {
	return l.width
}

// Height returns the height of the list viewport.
func (l *List) Height() int {
	return l.height
}

// Len returns the number of items in the list.
func (l *List) Len() int {
	return len(l.items)
}

// MouseWheelDelta returns the number of lines a single wheel detent scrolls.
func (l *List) MouseWheelDelta() int {
	if l.mouseWheelDelta < 1 {
		return 1
	}
	return l.mouseWheelDelta
}

// SetMouseWheelDelta configures wheel scroll granularity. Mirrors
// viewport.Model's MouseWheelDelta so callers (NewModel) can keep using
// metis's `mouseWheelLines()` config helper.
func (l *List) SetMouseWheelDelta(n int) {
	if n < 1 {
		n = 1
	}
	l.mouseWheelDelta = n
}

// ScrollPercent returns 0.0 when at top, 1.0 at bottom — used by the
// scrollbar gutter renderer (render_overlay.go) to position its thumb.
//
// Approximation: maps offsetIdx into [0, 1] over the index range. This
// is rough when items have wildly different heights but is good enough
// for thumb positioning (the eye doesn't notice fractional offsets).
func (l *List) ScrollPercent() float64 {
	if len(l.items) == 0 {
		return 1.0
	}
	if l.AtBottom() {
		return 1.0
	}
	if l.offsetIdx == 0 && l.offsetLine == 0 {
		return 0.0
	}
	denom := len(l.items) - 1
	if denom <= 0 {
		return 0.0
	}
	return float64(l.offsetIdx) / float64(denom)
}

// TotalLineCount estimates total content height in lines. Used by the
// scrollbar gutter to size the thumb proportionally.
//
// Honest cost note: this walks all items rendering each one. For 1200
// items in metis's worst case this is ~5ms with the renderCache warm.
// Callers should treat it as "occasional" — render_overlay.go calls it
// once per View() for thumb sizing, which is fine.
func (l *List) TotalLineCount() int {
	if len(l.items) == 0 {
		return 0
	}
	var total int
	for idx := range l.items {
		item := l.getItem(idx)
		total += item.height
		if l.gap > 0 && idx < len(l.items)-1 {
			total += l.gap
		}
	}
	return total
}

// lastOffsetItem returns the index and line offsets of the last item that can
// be partially visible in the viewport.
func (l *List) lastOffsetItem() (int, int, int) {
	var totalHeight int
	var idx int
	for idx = len(l.items) - 1; idx >= 0; idx-- {
		item := l.getItem(idx)
		itemHeight := item.height
		if l.gap > 0 && idx < len(l.items)-1 {
			itemHeight += l.gap
		}
		totalHeight += itemHeight
		if totalHeight > l.height {
			break
		}
	}

	// Calculate line offset within the item
	lineOffset := max(totalHeight-l.height, 0)
	idx = max(idx, 0)

	return idx, lineOffset, totalHeight
}

// getItem renders (if needed) and returns the item at the given index.
func (l *List) getItem(idx int) renderedItem {
	if idx < 0 || idx >= len(l.items) {
		return renderedItem{}
	}

	item := l.items[idx]
	if len(l.renderCallbacks) > 0 {
		for _, cb := range l.renderCallbacks {
			if it := cb(idx, l.selectedIdx, item); it != nil {
				item = it
			}
		}
	}

	rendered := item.Render(l.width)
	rendered = strings.TrimRight(rendered, "\n")
	height := strings.Count(rendered, "\n") + 1
	ri := renderedItem{
		content: rendered,
		height:  height,
	}

	return ri
}

// ScrollToIndex scrolls the list to the given item index.
func (l *List) ScrollToIndex(index int) {
	if index < 0 {
		index = 0
	}
	if index >= len(l.items) {
		index = len(l.items) - 1
	}
	l.offsetIdx = index
	l.offsetLine = 0
}

// ScrollBy scrolls the list by the given number of lines.
func (l *List) ScrollBy(lines int) {
	if len(l.items) == 0 || lines == 0 {
		return
	}

	if l.reverse {
		lines = -lines
	}

	if lines > 0 {
		if l.AtBottom() {
			// Already at bottom
			return
		}

		// Scroll down
		l.offsetLine += lines
		currentItem := l.getItem(l.offsetIdx)
		for l.offsetLine >= currentItem.height {
			l.offsetLine -= currentItem.height
			if l.gap > 0 {
				l.offsetLine = max(0, l.offsetLine-l.gap)
			}

			// Move to next item
			l.offsetIdx++
			if l.offsetIdx > len(l.items)-1 {
				// Reached bottom
				l.ScrollToBottom()
				return
			}
			currentItem = l.getItem(l.offsetIdx)
		}

		lastOffsetIdx, lastOffsetLine, _ := l.lastOffsetItem()
		if l.offsetIdx > lastOffsetIdx || (l.offsetIdx == lastOffsetIdx && l.offsetLine > lastOffsetLine) {
			// Clamp to bottom
			l.offsetIdx = lastOffsetIdx
			l.offsetLine = lastOffsetLine
		}
	} else if lines < 0 {
		// Scroll up
		l.offsetLine += lines // lines is negative
		for l.offsetLine < 0 {
			// Move to previous item
			l.offsetIdx--
			if l.offsetIdx < 0 {
				// Reached top
				l.ScrollToTop()
				break
			}
			prevItem := l.getItem(l.offsetIdx)
			totalHeight := prevItem.height
			if l.gap > 0 {
				totalHeight += l.gap
			}
			l.offsetLine += totalHeight
		}
	}
}

// VisibleItemIndices finds the range of items that are visible in the viewport.
// This is used for checking if selected item is in view.
func (l *List) VisibleItemIndices() (startIdx, endIdx int) {
	if len(l.items) == 0 {
		return 0, 0
	}

	startIdx = l.offsetIdx
	currentIdx := startIdx
	visibleHeight := -l.offsetLine

	for currentIdx < len(l.items) {
		item := l.getItem(currentIdx)
		visibleHeight += item.height
		if l.gap > 0 {
			visibleHeight += l.gap
		}

		if visibleHeight >= l.height {
			break
		}
		currentIdx++
	}

	endIdx = currentIdx
	if endIdx >= len(l.items) {
		endIdx = len(l.items) - 1
	}

	return startIdx, endIdx
}

// Render renders the list and returns the visible lines.
func (l *List) Render() string {
	if len(l.items) == 0 {
		return ""
	}

	var lines []string
	currentIdx := l.offsetIdx
	currentOffset := l.offsetLine

	linesNeeded := l.height

	for linesNeeded > 0 && currentIdx < len(l.items) {
		item := l.getItem(currentIdx)
		itemLines := strings.Split(item.content, "\n")
		itemHeight := len(itemLines)

		if currentOffset >= 0 && currentOffset < itemHeight {
			// Add visible content lines
			lines = append(lines, itemLines[currentOffset:]...)

			// Add gap if this is not the absolute last visual element (conceptually gaps are between items)
			// But in the loop we can just add it and trim later
			if l.gap > 0 {
				for i := 0; i < l.gap; i++ {
					lines = append(lines, "")
				}
			}
		} else {
			// offsetLine starts in the gap
			gapOffset := currentOffset - itemHeight
			gapRemaining := l.gap - gapOffset
			if gapRemaining > 0 {
				for range gapRemaining {
					lines = append(lines, "")
				}
			}
		}

		linesNeeded = l.height - len(lines)
		currentIdx++
		currentOffset = 0 // Reset offset for subsequent items
	}

	l.height = max(l.height, 0)

	if len(lines) > l.height {
		lines = lines[:l.height]
	}

	if l.reverse {
		// Reverse the lines so the list renders bottom-to-top.
		for i, j := 0, len(lines)-1; i < j; i, j = i+1, j-1 {
			lines[i], lines[j] = lines[j], lines[i]
		}
	}

	return strings.Join(lines, "\n")
}

// PrependItems prepends items to the list.
func (l *List) PrependItems(items ...Item) {
	l.items = append(items, l.items...)

	// Keep view position relative to the content that was visible
	l.offsetIdx += len(items)

	// Update selection index if valid
	if l.selectedIdx != -1 {
		l.selectedIdx += len(items)
	}
	l.invalidateAtBottomCache()
}

// SetItems sets the items in the list and resets scroll to the top.
// Use this when the underlying data changed semantically (e.g. user
// switched session) — the previous scroll position is no longer
// meaningful in the new content.
func (l *List) SetItems(items ...Item) {
	l.setItems(true, items...)
}

// SetItemsKeepScroll replaces the items but preserves offsetIdx and
// offsetLine. metis's chat surface calls this every frame: the items
// slice is rebuilt from the chronological merge of m.messages +
// m.toolEvents, but the user's scroll position should be stable across
// rebuilds. Index clamping still happens (in case items shrunk via
// undo / RemoveItem semantics), but offsetLine is left alone so a
// half-scrolled message stays half-scrolled.
func (l *List) SetItemsKeepScroll(items ...Item) {
	l.setItems(false, items...)
}

// setItems sets the items in the list. resetScroll=true clears
// offsetLine to 0 (crush's original behavior); resetScroll=false
// preserves the line offset so callers that re-Set the same logical
// list every frame don't drop the user's scroll position.
func (l *List) setItems(resetScroll bool, items ...Item) {
	l.items = items
	l.selectedIdx = min(l.selectedIdx, len(l.items)-1)
	if l.offsetIdx > len(l.items)-1 {
		l.offsetIdx = max(0, len(l.items)-1)
	}
	if resetScroll {
		l.offsetIdx = min(l.offsetIdx, len(l.items)-1)
		l.offsetLine = 0
	}
	l.invalidateAtBottomCache()
}

// AppendItems appends items to the list.
func (l *List) AppendItems(items ...Item) {
	l.items = append(l.items, items...)
	l.invalidateAtBottomCache()
}

// RemoveItem removes the item at the given index from the list.
func (l *List) RemoveItem(idx int) {
	if idx < 0 || idx >= len(l.items) {
		return
	}

	// Remove the item
	l.items = append(l.items[:idx], l.items[idx+1:]...)

	// Adjust selection if needed
	if l.selectedIdx == idx {
		l.selectedIdx = -1
	} else if l.selectedIdx > idx {
		l.selectedIdx--
	}

	// Adjust offset if needed
	if l.offsetIdx > idx {
		l.offsetIdx--
	} else if l.offsetIdx == idx && l.offsetIdx >= len(l.items) {
		l.offsetIdx = max(0, len(l.items)-1)
		l.offsetLine = 0
	}
	l.invalidateAtBottomCache()
}

// Focused returns whether the list is focused.
func (l *List) Focused() bool {
	return l.focused
}

// Focus sets the focus state of the list.
func (l *List) Focus() {
	l.focused = true
}

// Blur removes the focus state from the list.
func (l *List) Blur() {
	l.focused = false
}

// ScrollToTop scrolls the list to the top.
func (l *List) ScrollToTop() {
	l.offsetIdx = 0
	l.offsetLine = 0
	l.invalidateAtBottomCache()
}

// ScrollToBottom scrolls the list to the bottom.
func (l *List) ScrollToBottom() {
	if len(l.items) == 0 {
		return
	}

	lastOffsetIdx, lastOffsetLine, _ := l.lastOffsetItem()
	l.offsetIdx = lastOffsetIdx
	l.offsetLine = lastOffsetLine
	l.invalidateAtBottomCache()
}

// ScrollToSelected scrolls the list to the selected item.
func (l *List) ScrollToSelected() {
	if l.selectedIdx < 0 || l.selectedIdx >= len(l.items) {
		return
	}

	startIdx, endIdx := l.VisibleItemIndices()
	if l.selectedIdx < startIdx {
		// Selected item is above the visible range
		l.offsetIdx = l.selectedIdx
		l.offsetLine = 0
	} else if l.selectedIdx > endIdx {
		// Selected item is below the visible range
		// Scroll so that the selected item is at the bottom
		var totalHeight int
		for i := l.selectedIdx; i >= 0; i-- {
			item := l.getItem(i)
			totalHeight += item.height
			if l.gap > 0 && i < l.selectedIdx {
				totalHeight += l.gap
			}
			if totalHeight >= l.height {
				l.offsetIdx = i
				l.offsetLine = totalHeight - l.height
				break
			}
		}
		if totalHeight < l.height {
			// All items fit in the viewport
			l.ScrollToTop()
		}
	}
	l.invalidateAtBottomCache()
}

// SelectedItemInView returns whether the selected item is currently in view.
func (l *List) SelectedItemInView() bool {
	if l.selectedIdx < 0 || l.selectedIdx >= len(l.items) {
		return false
	}
	startIdx, endIdx := l.VisibleItemIndices()
	return l.selectedIdx >= startIdx && l.selectedIdx <= endIdx
}

// SetSelected sets the selected item index in the list.
// It returns -1 if the index is out of bounds.
func (l *List) SetSelected(index int) {
	if index < 0 || index >= len(l.items) {
		l.selectedIdx = -1
	} else {
		l.selectedIdx = index
	}
}

// Selected returns the index of the currently selected item. It returns -1 if
// no item is selected.
func (l *List) Selected() int {
	return l.selectedIdx
}

// IsSelectedFirst returns whether the first item is selected.
func (l *List) IsSelectedFirst() bool {
	return l.selectedIdx == 0
}

// IsSelectedLast returns whether the last item is selected.
func (l *List) IsSelectedLast() bool {
	return l.selectedIdx == len(l.items)-1
}

// SelectPrev selects the visually previous item (moves toward visual top).
// It returns whether the selection changed.
func (l *List) SelectPrev() bool {
	if l.reverse {
		// In reverse, visual up = higher index
		if l.selectedIdx < len(l.items)-1 {
			l.selectedIdx++
			return true
		}
	} else {
		// Normal: visual up = lower index
		if l.selectedIdx > 0 {
			l.selectedIdx--
			return true
		}
	}
	return false
}

// SelectNext selects the next item in the list.
// It returns whether the selection changed.
func (l *List) SelectNext() bool {
	if l.reverse {
		// In reverse, visual down = lower index
		if l.selectedIdx > 0 {
			l.selectedIdx--
			return true
		}
	} else {
		// Normal: visual down = higher index
		if l.selectedIdx < len(l.items)-1 {
			l.selectedIdx++
			return true
		}
	}
	return false
}

// SelectFirst selects the first item in the list.
// It returns whether the selection changed.
func (l *List) SelectFirst() bool {
	if len(l.items) == 0 {
		return false
	}
	l.selectedIdx = 0
	return true
}

// SelectLast selects the last item in the list (highest index).
// It returns whether the selection changed.
func (l *List) SelectLast() bool {
	if len(l.items) == 0 {
		return false
	}
	l.selectedIdx = len(l.items) - 1
	return true
}

// WrapToStart wraps selection to the visual start (for circular navigation).
// In normal mode, this is index 0. In reverse mode, this is the highest index.
func (l *List) WrapToStart() bool {
	if len(l.items) == 0 {
		return false
	}
	if l.reverse {
		l.selectedIdx = len(l.items) - 1
	} else {
		l.selectedIdx = 0
	}
	return true
}

// WrapToEnd wraps selection to the visual end (for circular navigation).
// In normal mode, this is the highest index. In reverse mode, this is index 0.
func (l *List) WrapToEnd() bool {
	if len(l.items) == 0 {
		return false
	}
	if l.reverse {
		l.selectedIdx = 0
	} else {
		l.selectedIdx = len(l.items) - 1
	}
	return true
}

// SelectedItem returns the currently selected item. It may be nil if no item
// is selected.
func (l *List) SelectedItem() Item {
	if l.selectedIdx < 0 || l.selectedIdx >= len(l.items) {
		return nil
	}
	return l.items[l.selectedIdx]
}

// SelectFirstInView selects the first item currently in view.
func (l *List) SelectFirstInView() {
	startIdx, _ := l.VisibleItemIndices()
	l.selectedIdx = startIdx
}

// SelectLastInView selects the last item currently in view.
func (l *List) SelectLastInView() {
	_, endIdx := l.VisibleItemIndices()
	l.selectedIdx = endIdx
}

// ItemAt returns the item at the given index.
func (l *List) ItemAt(index int) Item {
	if index < 0 || index >= len(l.items) {
		return nil
	}
	return l.items[index]
}

// ItemIndexAtPosition returns the item at the given viewport-relative y
// coordinate. Returns the item index and the y offset within that item. It
// returns -1, -1 if no item is found.
func (l *List) ItemIndexAtPosition(x, y int) (itemIdx int, itemY int) {
	return l.findItemAtY(x, y)
}

// findItemAtY finds the item at the given viewport y coordinate.
// Returns the item index and the y offset within that item. It returns -1, -1
// if no item is found.
func (l *List) findItemAtY(_, y int) (itemIdx int, itemY int) {
	if y < 0 || y >= l.height {
		return -1, -1
	}

	// Walk through visible items to find which one contains this y
	currentIdx := l.offsetIdx
	currentLine := -l.offsetLine // Negative because offsetLine is how many lines are hidden

	for currentIdx < len(l.items) && currentLine < l.height {
		item := l.getItem(currentIdx)
		itemEndLine := currentLine + item.height

		// Check if y is within this item's visible range
		if y >= currentLine && y < itemEndLine {
			// Found the item, calculate itemY (offset within the item)
			itemY = y - currentLine
			return currentIdx, itemY
		}

		// Move to next item
		currentLine = itemEndLine
		if l.gap > 0 {
			currentLine += l.gap
		}
		currentIdx++
	}

	return -1, -1
}
