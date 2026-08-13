package tui

// input_mouse_selection.go implements screen-style selection for the
// composer. It deliberately models a copied screen range, not an editor range:
// dragging highlights complete grapheme clusters and copies them, while a bare
// click still positions the textarea cursor. Typing does not replace the
// selected range; it dismisses the screen highlight and continues editing.

import (
	"strings"
	"unicode"

	"github.com/mattn/go-runewidth"
	"github.com/rivo/uniseg"
)

type mouseGestureOwner uint8

// writeInputSelectionClipboard is a narrow seam for deterministic gesture and
// key-priority tests. Production always points at the regular OSC52/native/file
// clipboard fan-out.
var writeInputSelectionClipboard = writeClipboard

type inputSelectionCursorSyncMsg struct{}

func (m *Model) inputMouseBlocked() bool {
	return m.permActive || m.copyMode || m.activeScreen != nil ||
		m.effortPicker != nil || m.showSearch || m.askUserActive ||
		m.showHistory || m.showTaskPanel
}

const (
	mouseOwnerNone mouseGestureOwner = iota
	mouseOwnerChat
	mouseOwnerInput
	mouseOwnerStrip
)

// inputSelectionPoint is both a source-text boundary and its position in the
// textarea's visual layout. sourceOffset is a byte boundary in Value();
// runeColumn is what bubbles/textarea.SetCursorColumn expects.
type inputSelectionPoint struct {
	sourceOffset int
	logicalRow   int
	runeColumn   int
	visualRow    int
	displayCol   int
}

type inputVisualToken struct {
	start       inputSelectionPoint
	end         inputSelectionPoint
	displayText string
}

type inputVisualRow struct {
	tokens       []inputVisualToken
	defaultPoint inputSelectionPoint
}

// inputSelectionHit keeps the two source boundaries around the visible cell.
// Selection uses before/after so the cell under the mouse is included; caret
// placement uses caret so a plain click keeps editor-like insertion behavior.
type inputSelectionHit struct {
	before inputSelectionPoint
	after  inputSelectionPoint
	caret  inputSelectionPoint
}

type inputSelectionSurface struct {
	value        string
	rows         []inputVisualRow
	width        int
	scrollOffset int
}

// inputMouseSelection keeps the immutable source snapshot that was on screen
// when the gesture began. A subsequent edit invalidates it automatically.
type inputMouseSelection struct {
	value      string
	anchor     inputSelectionPoint
	focus      inputSelectionPoint
	pressHit   inputSelectionHit
	dragging   bool
	moved      bool
	hasContent bool
	pressX     int
	pressY     int
}

func (s *inputMouseSelection) Begin(value string, hit inputSelectionHit, x, y int) {
	*s = inputMouseSelection{
		value:    value,
		anchor:   hit.caret,
		focus:    hit.caret,
		pressHit: hit,
		dragging: true,
		pressX:   x,
		pressY:   y,
	}
}

func (s *inputMouseSelection) Drag(value string, hit inputSelectionHit, x, y int) {
	if !s.dragging || s.value != value {
		return
	}
	// A motion report that stays on the press cell is not a drag. This keeps a
	// plain click available for textarea caret positioning.
	if x == s.pressX && y == s.pressY {
		return
	}
	s.extend(hit, x, y)
}

func (s *inputMouseSelection) Finish(value string, hit inputSelectionHit, x, y int) bool {
	if !s.dragging || s.value != value {
		s.Clear()
		return false
	}
	if x != s.pressX || y != s.pressY {
		s.extend(hit, x, y)
	}
	s.dragging = false
	if !s.moved {
		s.hasContent = false
		return false
	}
	lo, hi := s.anchor.sourceOffset, s.focus.sourceOffset
	if lo > hi {
		lo, hi = hi, lo
	}
	s.hasContent = lo >= 0 && hi <= len(value) && lo < hi
	return s.hasContent
}

func (s *inputMouseSelection) extend(hit inputSelectionHit, x, y int) {
	s.moved = true
	if y > s.pressY || (y == s.pressY && x >= s.pressX) {
		s.anchor = s.pressHit.before
		s.focus = hit.after
		return
	}
	s.anchor = s.pressHit.after
	s.focus = hit.before
}

func (s *inputMouseSelection) Clear() { *s = inputMouseSelection{} }

func (s inputMouseSelection) Range(value string) (lo, hi int, ok bool) {
	if s.value != value {
		return 0, 0, false
	}
	if !s.dragging && !s.hasContent {
		return 0, 0, false
	}
	lo, hi = s.anchor.sourceOffset, s.focus.sourceOffset
	if lo > hi {
		lo, hi = hi, lo
	}
	if lo < 0 || hi > len(value) || lo == hi {
		return 0, 0, false
	}
	return lo, hi, true
}

func (s inputMouseSelection) HasSelection(value string) bool {
	_, _, ok := s.Range(value)
	return ok
}

func (s inputMouseSelection) SelectedText(value string) string {
	lo, hi, ok := s.Range(value)
	if !ok {
		return ""
	}
	return value[lo:hi]
}

type inputLayoutRune struct {
	displayText string
	sourceStart int
	sourceEnd   int
	runeStart   int
	runeEnd     int
	logicalRow  int
}

// rebuildInputSelectionSurface reproduces the pinned bubbles/textarea wrap
// layout with source metadata attached. It is pure: probing the live textarea
// through a value copy is unsafe because the widget contains a shared viewport
// pointer and cursor navigation would mutate the real composer's scroll.
func (m *Model) rebuildInputSelectionSurface() {
	value := m.input.Value()
	width := m.input.Width()
	scrollOffset := m.input.ScrollYOffset()
	if len(m.inputSurface.rows) > 0 && m.inputSurface.value == value && m.inputSurface.width == width {
		// Scrolling changes only which already-built rows are visible; it must
		// not pay the full Unicode wrap/layout cost again.
		m.inputSurface.scrollOffset = scrollOffset
		return
	}
	logicalLines := strings.Split(value, "\n")

	surface := inputSelectionSurface{
		value:        value,
		width:        width,
		scrollOffset: scrollOffset,
	}
	byteBase := 0

	for logicalRow, line := range logicalLines {
		lineRunes := make([]inputLayoutRune, 0, len([]rune(line)))
		lineRuneColumn := 0
		graphemes := uniseg.NewGraphemes(line)
		for graphemes.Next() {
			from, to := graphemes.Positions()
			clusterRunes := []rune(graphemes.Str())
			for _, r := range clusterRunes {
				displayText := string(r)
				if unicode.IsSpace(r) {
					displayText = " "
				}
				lineRunes = append(lineRunes, inputLayoutRune{
					displayText: displayText,
					sourceStart: byteBase + from,
					sourceEnd:   byteBase + to,
					runeStart:   lineRuneColumn,
					runeEnd:     lineRuneColumn + len(clusterRunes),
					logicalRow:  logicalRow,
				})
			}
			lineRuneColumn += len(clusterRunes)
		}

		wrapped := wrapInputLayoutRunes(lineRunes, width)
		lineEnd := inputSelectionPoint{
			sourceOffset: byteBase + len(line),
			logicalRow:   logicalRow,
			runeColumn:   lineRuneColumn,
			visualRow:    len(surface.rows),
			displayCol:   0,
		}
		for _, layoutRow := range wrapped {
			visualRow := len(surface.rows)
			row := buildInputVisualRow(layoutRow, visualRow, width, lineEnd)
			surface.rows = append(surface.rows, row)
		}

		byteBase += len(line)
		if logicalRow < len(logicalLines)-1 {
			byteBase++ // the hard newline between logical lines
		}
	}
	m.inputSurface = surface
}

// wrapInputLayoutRunes follows bubbles/textarea v2.1.0's MIT-licensed wrap
// semantics, including its navigation-only trailing space, while retaining a
// source identity for each displayed rune. Keeping this small adaptation next
// to its parity tests makes upstream changes visible rather than silently
// corrupting mouse coordinates.
func wrapInputLayoutRunes(runes []inputLayoutRune, width int) [][]inputLayoutRune {
	if width < 1 {
		width = 1
	}
	lines := [][]inputLayoutRune{{}}
	word := []inputLayoutRune{}
	spaces := []inputLayoutRune{}
	row := 0

	for _, r := range runes {
		if strings.TrimSpace(r.displayText) == "" {
			r.displayText = " "
			spaces = append(spaces, r)
		} else {
			word = append(word, r)
		}

		if len(spaces) > 0 {
			if inputLayoutWidth(lines[row])+inputLayoutWidth(word)+len(spaces) > width {
				row++
				lines = append(lines, []inputLayoutRune{})
			}
			lines[row] = append(lines[row], word...)
			lines[row] = append(lines[row], spaces...)
			spaces = nil
			word = nil
		} else if len(word) > 0 {
			lastWidth := runewidth.RuneWidth([]rune(word[len(word)-1].displayText)[0])
			if inputLayoutWidth(word)+lastWidth > width {
				if len(lines[row]) > 0 {
					row++
					lines = append(lines, []inputLayoutRune{})
				}
				lines[row] = append(lines[row], word...)
				word = nil
			}
		}
	}

	if inputLayoutWidth(lines[row])+inputLayoutWidth(word)+len(spaces) >= width {
		lines = append(lines, []inputLayoutRune{})
		row++
	}
	lines[row] = append(lines[row], word...)
	lines[row] = append(lines[row], spaces...)
	// textarea adds one synthetic trailing space for cursor navigation. It has
	// no source range and is intentionally excluded from hit/copy tokens.
	lines[row] = append(lines[row], inputLayoutRune{displayText: " ", sourceStart: -1})
	return lines
}

func inputLayoutWidth(runes []inputLayoutRune) int {
	var display strings.Builder
	for _, r := range runes {
		display.WriteString(r.displayText)
	}
	return uniseg.StringWidth(display.String())
}

func buildInputVisualRow(layout []inputLayoutRune, visualRow, maxWidth int, fallback inputSelectionPoint) inputVisualRow {
	row := inputVisualRow{defaultPoint: fallback}
	row.defaultPoint.visualRow = visualRow
	displayCol := 0
	for i := 0; i < len(layout); {
		first := layout[i]
		j := i + 1
		for j < len(layout) && first.sourceStart >= 0 &&
			layout[j].sourceStart == first.sourceStart && layout[j].sourceEnd == first.sourceEnd {
			j++
		}
		var fragment strings.Builder
		for _, r := range layout[i:j] {
			fragment.WriteString(r.displayText)
		}
		text := fragment.String()
		cellWidth := uniseg.StringWidth(text)
		if first.sourceStart >= 0 && cellWidth > 0 && displayCol+cellWidth <= maxWidth {
			start := inputSelectionPoint{
				sourceOffset: first.sourceStart,
				logicalRow:   first.logicalRow,
				runeColumn:   first.runeStart,
				visualRow:    visualRow,
				displayCol:   displayCol,
			}
			last := layout[j-1]
			end := inputSelectionPoint{
				sourceOffset: last.sourceEnd,
				logicalRow:   last.logicalRow,
				runeColumn:   last.runeEnd,
				visualRow:    visualRow,
				displayCol:   displayCol + cellWidth,
			}
			row.tokens = append(row.tokens, inputVisualToken{start: start, end: end, displayText: text})
		}
		displayCol += cellWidth
		i = j
	}
	if len(row.tokens) > 0 {
		row.defaultPoint = row.tokens[0].start
	}
	return row
}

func (r inputVisualRow) hit(displayCol int) inputSelectionHit {
	if len(r.tokens) == 0 {
		p := r.defaultPoint
		p.displayCol = max(displayCol, 0)
		return inputSelectionHit{before: p, after: p, caret: p}
	}
	if displayCol < r.tokens[0].start.displayCol {
		p := r.tokens[0].start
		return inputSelectionHit{before: p, after: p, caret: p}
	}
	for _, token := range r.tokens {
		if displayCol >= token.start.displayCol && displayCol < token.end.displayCol {
			caret := token.start
			width := token.end.displayCol - token.start.displayCol
			if width > 1 && displayCol-token.start.displayCol >= width/2 {
				caret = token.end
			}
			return inputSelectionHit{before: token.start, after: token.end, caret: caret}
		}
		if displayCol < token.start.displayCol {
			p := token.start
			return inputSelectionHit{before: p, after: p, caret: p}
		}
	}
	p := r.tokens[len(r.tokens)-1].end
	return inputSelectionHit{before: p, after: p, caret: p}
}

// inputPointAt maps terminal coordinates into the immutable composer frame.
// clampY is used while a drag is already owned by the composer so moving just
// outside its top/bottom keeps extending toward the nearest visible edge.
func (m *Model) inputPointAt(x, y int, clampY bool) (inputSelectionHit, bool) {
	if m.inputBodyStartY < 0 || m.inputBodyHeight <= 0 ||
		m.inputSurface.value != m.input.Value() || len(m.inputSurface.rows) == 0 {
		return inputSelectionHit{}, false
	}
	relY := y - m.inputBodyStartY
	if clampY {
		if relY < 0 {
			relY = 0
		}
		if relY >= m.inputBodyHeight {
			relY = m.inputBodyHeight - 1
		}
	} else if relY < 0 || relY >= m.inputBodyHeight {
		return inputSelectionHit{}, false
	}
	if x < m.inputContentStartX {
		if !clampY {
			return inputSelectionHit{}, false
		}
		x = m.inputContentStartX
	}
	if m.inputContentEndX <= m.inputContentStartX {
		return inputSelectionHit{}, false
	}
	if x >= m.inputContentEndX {
		if !clampY {
			return inputSelectionHit{}, false
		}
		x = m.inputContentEndX - 1
	}
	globalRow := m.inputSurface.scrollOffset + relY
	if globalRow < 0 {
		globalRow = 0
	}
	if globalRow >= len(m.inputSurface.rows) {
		if !clampY {
			return inputSelectionHit{}, false
		}
		globalRow = len(m.inputSurface.rows) - 1
	}
	return m.inputSurface.rows[globalRow].hit(x - m.inputContentStartX), true
}

func (m *Model) moveInputCursorTo(p inputSelectionPoint) {
	// Navigate from the currently visible cursor instead of resetting to the
	// beginning. Resetting would also reset textarea's viewport; a click on a
	// scrolled ten-line draft would then move the logical caret correctly but
	// jump the screen back to row zero.
	current, ok := m.currentInputVisualRow()
	if !ok {
		return
	}
	for steps := 0; current != p.visualRow && steps < len(m.inputSurface.rows)+1; steps++ {
		before := current
		if current < p.visualRow {
			m.input.CursorDown()
		} else {
			m.input.CursorUp()
		}
		current, ok = m.currentInputVisualRow()
		if !ok || current == before {
			break
		}
	}
	m.input.SetCursorColumn(p.runeColumn)
	// SetCursorColumn intentionally does not reposition bubbles' viewport.
	// Run one no-op widget update so a click at a soft-wrap boundary cannot
	// leave the native cursor outside the visible composer window.
	updated, _ := m.input.Update(inputSelectionCursorSyncMsg{})
	m.input = updated
}

// scrollInputSelectionToward advances the textarea one visual row when a
// composer-owned drag leaves the visible body. Bubble Tea emits repeated cell
// motion reports while the pointer remains in motion, producing familiar
// edge-scroll behavior without a separate timer or hidden viewport mutation.
func (m *Model) scrollInputSelectionToward(y int) bool {
	if m.inputBodyHeight <= 0 || len(m.inputSurface.rows) <= m.inputBodyHeight {
		return false
	}
	before := m.input.ScrollYOffset()
	switch {
	case y < m.inputBodyStartY && before > 0:
		m.input.CursorUp()
	case y >= m.inputBodyStartY+m.inputBodyHeight && before+m.inputBodyHeight < len(m.inputSurface.rows):
		m.input.CursorDown()
	default:
		return false
	}
	if m.input.ScrollYOffset() == before {
		return false
	}
	m.rebuildInputSelectionSurface()
	return true
}

func (m *Model) currentInputVisualRow() (int, bool) {
	logicalRow := m.input.Line()
	base := -1
	for i := range m.inputSurface.rows {
		if m.inputSurface.rows[i].defaultPoint.logicalRow == logicalRow {
			base = i
			break
		}
	}
	if base < 0 {
		return 0, false
	}
	return base + m.input.LineInfo().RowOffset, true
}

// inputSelectionColumns returns the selected display-column span on one
// global visual row. Tokens are whole grapheme clusters, so the renderer never
// colors only the trailing cell of a wide character.
func (m *Model) inputSelectionColumns(globalRow, start, end int) (lo, hi int, ok bool) {
	if start >= end || globalRow < 0 || globalRow >= len(m.inputSurface.rows) {
		return 0, 0, false
	}
	lo = int(^uint(0) >> 1)
	for _, token := range m.inputSurface.rows[globalRow].tokens {
		if token.start.sourceOffset < end && token.end.sourceOffset > start {
			if token.start.displayCol < lo {
				lo = token.start.displayCol
			}
			if token.end.displayCol > hi {
				hi = token.end.displayCol
			}
		}
	}
	return lo, hi, lo < hi
}
