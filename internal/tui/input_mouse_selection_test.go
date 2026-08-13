package tui

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

func makeInputSelectionModel(t *testing.T, value string) *Model {
	t.Helper()
	m := newE2EModel(t, 80, 30, 0)
	m.input = newComposer()
	m.input.SetValue(value)
	_ = m.View() // captures the input body's screen geometry
	if m.inputBodyStartY <= 0 || m.inputBodyHeight <= 0 {
		t.Fatalf("input geometry was not captured: startY=%d height=%d", m.inputBodyStartY, m.inputBodyHeight)
	}
	return m
}

func dragInputSelection(m *Model, fromX, fromY, toX, toY int) {
	m.Update(tea.MouseClickMsg{X: fromX, Y: fromY, Button: tea.MouseLeft})
	m.Update(tea.MouseMotionMsg{X: toX, Y: toY, Button: tea.MouseLeft})
	m.Update(tea.MouseReleaseMsg{X: toX, Y: toY, Button: tea.MouseLeft})
}

func spyInputSelectionClipboard(t *testing.T) *[]string {
	t.Helper()
	old := writeInputSelectionClipboard
	writes := []string{}
	writeInputSelectionClipboard = func(text string) { writes = append(writes, text) }
	t.Cleanup(func() { writeInputSelectionClipboard = old })
	return &writes
}

func TestInputMouseSelection_DragHighlightsAndAutoCopies(t *testing.T) {
	home := t.TempDir()
	t.Setenv("METIS_HOME", home)
	m := makeInputSelectionModel(t, "alpha beta")

	y := m.inputBodyStartY
	x := m.inputContentStartX
	dragInputSelection(m, x, y, x+4, y)

	if !m.inputSelection.HasSelection(m.input.Value()) {
		t.Fatal("input selection should stay highlighted after mouse release")
	}
	body, err := os.ReadFile(filepath.Join(home, "clipboard.txt"))
	if err != nil {
		t.Fatalf("clipboard fallback: %v", err)
	}
	if got := string(body); got != "alpha" {
		t.Fatalf("copied input selection = %q, want %q", got, "alpha")
	}
	if got := m.View().Content; !strings.Contains(got, "\x1b[48;5;238m") {
		t.Fatalf("selected input is missing the visible selection background:\n%q", got)
	}
}

func TestInputMouseSelection_CJKAndEmojiStayOnGraphemeBoundaries(t *testing.T) {
	home := t.TempDir()
	t.Setenv("METIS_HOME", home)
	m := makeInputSelectionModel(t, "A你🙂B")

	// Display columns: A=[0,1), 你=[1,3), 🙂=[3,5), B=[5,6).
	y := m.inputBodyStartY
	x := m.inputContentStartX
	dragInputSelection(m, x+1, y, x+4, y)

	body, err := os.ReadFile(filepath.Join(home, "clipboard.txt"))
	if err != nil {
		t.Fatalf("clipboard fallback: %v", err)
	}
	if got := string(body); got != "你🙂" {
		t.Fatalf("copied wide-grapheme selection = %q, want %q", got, "你🙂")
	}
}

func TestInputMouseSelection_WideCellEndpointsAreInclusive(t *testing.T) {
	cases := []struct {
		name     string
		from, to int
	}{
		{name: "forward first halves", from: 1, to: 3},
		{name: "forward second halves", from: 2, to: 4},
		{name: "backward first halves", from: 3, to: 1},
		{name: "backward second halves", from: 4, to: 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			writes := spyInputSelectionClipboard(t)
			m := makeInputSelectionModel(t, "A你🙂B")
			y := m.inputBodyStartY
			x := m.inputContentStartX
			dragInputSelection(m, x+tc.from, y, x+tc.to, y)
			if len(*writes) != 1 || (*writes)[0] != "你🙂" {
				t.Fatalf("clipboard writes = %#v, want one complete wide-grapheme range", *writes)
			}
		})
	}
}

func TestInputMouseSelection_CombiningAndZWJGraphemesStayWhole(t *testing.T) {
	home := t.TempDir()
	t.Setenv("METIS_HOME", home)
	m := makeInputSelectionModel(t, "Ae\u0301👩‍💻B")

	// Display columns: A=[0,1), é=[1,2), 👩‍💻=[2,4), B=[4,5).
	y := m.inputBodyStartY
	x := m.inputContentStartX
	dragInputSelection(m, x+1, y, x+3, y)

	body, err := os.ReadFile(filepath.Join(home, "clipboard.txt"))
	if err != nil {
		t.Fatalf("clipboard fallback: %v", err)
	}
	if got := string(body); got != "e\u0301👩‍💻" {
		t.Fatalf("copied grapheme selection = %q, want %q", got, "e\u0301👩‍💻")
	}
}

func TestInputMouseSelection_SoftWrappedZWJFragmentsCopyAsOneGrapheme(t *testing.T) {
	home := t.TempDir()
	t.Setenv("METIS_HOME", home)
	m := newE2EModel(t, 30, 30, 0)
	_ = m.View()
	contentWidth := m.input.Width()
	if contentWidth < 6 {
		t.Fatalf("input content width is too small for wrap test: %d", contentWidth)
	}
	prefix := strings.Repeat("a", contentWidth-3)
	cluster := "👩‍💻"
	m.input.SetValue(prefix + cluster + "Z")
	_ = m.View()

	clusterStart := len(prefix)
	clusterEnd := clusterStart + len(cluster)
	var target *inputVisualToken
	targetRow := -1
	fragments := 0
	for rowIdx := range m.inputSurface.rows {
		for tokenIdx := range m.inputSurface.rows[rowIdx].tokens {
			token := &m.inputSurface.rows[rowIdx].tokens[tokenIdx]
			if token.start.sourceOffset == clusterStart && token.end.sourceOffset == clusterEnd {
				fragments++
				target = token
				targetRow = rowIdx
			}
		}
	}
	if target == nil || fragments < 2 {
		t.Fatalf("expected a ZWJ grapheme split across visual rows, fragments=%d surface=%+v", fragments, m.inputSurface.rows)
	}

	y := m.inputBodyStartY + targetRow - m.inputSurface.scrollOffset
	x := m.inputContentStartX + target.start.displayCol
	dragInputSelection(m, x, y, x+target.end.displayCol-target.start.displayCol-1, y)
	body, err := os.ReadFile(filepath.Join(home, "clipboard.txt"))
	if err != nil {
		t.Fatalf("clipboard fallback: %v", err)
	}
	if got := string(body); got != cluster {
		t.Fatalf("soft-wrapped ZWJ selection = %q, want full grapheme %q", got, cluster)
	}
}

func TestInputMouseSelection_SoftWrapFragmentClicksKeepCaretVisible(t *testing.T) {
	for _, clickSecondCell := range []bool{false, true} {
		t.Run(fmt.Sprintf("second_cell_%v", clickSecondCell), func(t *testing.T) {
			m := newE2EModel(t, 30, 30, 0)
			m.input = newComposer()
			_ = m.View()
			prefix := strings.Repeat("a", m.input.Width()-3)
			cluster := "👩‍💻"
			m.input.SetValue(prefix + cluster + "Z")
			_ = m.View()
			clusterStart := len(prefix)
			clusterEnd := clusterStart + len(cluster)

			for rowIdx, row := range m.inputSurface.rows {
				for _, token := range row.tokens {
					if token.start.sourceOffset != clusterStart || token.end.sourceOffset != clusterEnd {
						continue
					}
					y := m.inputBodyStartY + rowIdx - m.inputSurface.scrollOffset
					x := m.inputContentStartX + token.start.displayCol
					if clickSecondCell && token.end.displayCol-token.start.displayCol > 1 {
						x++
					}
					m.Update(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
					m.Update(tea.MouseReleaseMsg{X: x, Y: y, Button: tea.MouseLeft})
					current, ok := m.currentInputVisualRow()
					if !ok {
						t.Fatal("clicked soft-wrap fragment lost cursor row")
					}
					offset := m.input.ScrollYOffset()
					if current < offset || current >= offset+m.input.Height() {
						t.Fatalf("clicked fragment left caret row %d outside viewport [%d,%d)", current, offset, offset+m.input.Height())
					}
				}
			}
		})
	}
}

func TestRebuildInputSelectionSurfaceDoesNotMutateLiveTextarea(t *testing.T) {
	m := makeInputSelectionModel(t, strings.Repeat("abcdefghij", 120))
	m.input.CursorUp()
	m.input.CursorDown()
	_ = m.input.View()
	beforeOffset := m.input.ScrollYOffset()
	beforeLine := m.input.Line()
	beforeColumn := m.input.Column()

	m.inputSurface = inputSelectionSurface{}
	m.rebuildInputSelectionSurface()
	if got := m.input.ScrollYOffset(); got != beforeOffset {
		t.Fatalf("surface rebuild changed textarea scroll offset: got %d want %d", got, beforeOffset)
	}
	if got := m.input.Line(); got != beforeLine {
		t.Fatalf("surface rebuild changed logical line: got %d want %d", got, beforeLine)
	}
	if got := m.input.Column(); got != beforeColumn {
		t.Fatalf("surface rebuild changed cursor column: got %d want %d", got, beforeColumn)
	}
}

func TestInputMouseSelection_HardNewlineIsPreserved(t *testing.T) {
	home := t.TempDir()
	t.Setenv("METIS_HOME", home)
	m := makeInputSelectionModel(t, "one\ntwo")

	secondVisualRow := -1
	for rowIdx, row := range m.inputSurface.rows {
		if len(row.tokens) > 0 && row.tokens[0].start.logicalRow == 1 {
			secondVisualRow = rowIdx
			break
		}
	}
	if secondVisualRow < 0 {
		t.Fatal("second logical input line was not mapped to the screen")
	}
	y1 := m.inputBodyStartY
	y2 := m.inputBodyStartY + secondVisualRow - m.inputSurface.scrollOffset
	x := m.inputContentStartX
	dragInputSelection(m, x, y1, x+2, y2)

	body, err := os.ReadFile(filepath.Join(home, "clipboard.txt"))
	if err != nil {
		t.Fatalf("clipboard fallback: %v", err)
	}
	if got := string(body); got != "one\ntwo" {
		t.Fatalf("copied multiline selection = %q, want %q", got, "one\ntwo")
	}
}

func TestInputMouseSelection_SoftWrapDoesNotInventNewline(t *testing.T) {
	home := t.TempDir()
	t.Setenv("METIS_HOME", home)
	value := strings.Repeat("abcdefghij", 10)
	m := makeInputSelectionModel(t, value)
	if len(m.inputSurface.rows) < 2 {
		t.Fatalf("long input did not soft-wrap: rows=%d width=%d", len(m.inputSurface.rows), m.input.Width())
	}

	first := m.inputSurface.rows[0].hit(3)
	second := m.inputSurface.rows[1].hit(5)
	y1 := m.inputBodyStartY - m.inputSurface.scrollOffset
	y2 := y1 + 1
	x := m.inputContentStartX
	dragInputSelection(m, x+3, y1, x+5, y2)

	body, err := os.ReadFile(filepath.Join(home, "clipboard.txt"))
	if err != nil {
		t.Fatalf("clipboard fallback: %v", err)
	}
	want := value[first.before.sourceOffset:second.after.sourceOffset]
	if got := string(body); got != want {
		t.Fatalf("soft-wrapped selection = %q, want source slice %q", got, want)
	}
	if strings.Contains(string(body), "\n") {
		t.Fatalf("soft wrap was copied as a hard newline: %q", body)
	}
}

func TestInputMouseSelection_BackwardDragNormalizesRange(t *testing.T) {
	home := t.TempDir()
	t.Setenv("METIS_HOME", home)
	m := makeInputSelectionModel(t, "alpha")
	y := m.inputBodyStartY
	x := m.inputContentStartX
	dragInputSelection(m, x+4, y, x+1, y)

	body, err := os.ReadFile(filepath.Join(home, "clipboard.txt"))
	if err != nil {
		t.Fatalf("clipboard fallback: %v", err)
	}
	if got := string(body); got != "lpha" {
		t.Fatalf("backward selection = %q, want %q", got, "lpha")
	}
}

func TestInputMouseSelection_SingleClickMovesCursorWithoutSelecting(t *testing.T) {
	writes := spyInputSelectionClipboard(t)
	m := makeInputSelectionModel(t, "cursor here")
	y := m.inputBodyStartY
	x := m.inputContentStartX
	m.Update(tea.MouseClickMsg{X: x + 7, Y: y, Button: tea.MouseLeft})
	m.Update(tea.MouseReleaseMsg{X: x + 7, Y: y, Button: tea.MouseLeft})

	if got := m.input.Column(); got != 7 {
		t.Fatalf("cursor column after click = %d, want 7", got)
	}
	if m.inputSelection.HasSelection(m.input.Value()) {
		t.Fatal("a bare click must position the cursor without creating a selection")
	}
	if len(*writes) != 0 {
		t.Fatalf("a bare click wrote to clipboard: %#v", *writes)
	}
}

func TestInputMouseSelection_ClickInScrolledDraftKeepsCaretVisible(t *testing.T) {
	m := makeInputSelectionModel(t, strings.Repeat("abcdefghij", 120))
	// SetValue precedes the viewport's first content render in this test helper;
	// one vertical round-trip applies the same repositioning a real key event
	// would perform after the first frame.
	m.input.CursorUp()
	m.input.CursorDown()
	_ = m.View()
	beforeOffset := m.input.ScrollYOffset()
	if beforeOffset <= 0 {
		t.Fatalf("long draft did not scroll: rows=%d height=%d offset=%d",
			len(m.inputSurface.rows), m.input.Height(), beforeOffset)
	}

	y := m.inputBodyStartY // top visible wrapped row
	x := m.inputContentStartX + 3
	target, ok := m.inputPointAt(x, y, false)
	if !ok {
		t.Fatal("top visible input cell was not hit-testable")
	}
	m.Update(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
	m.Update(tea.MouseReleaseMsg{X: x, Y: y, Button: tea.MouseLeft})

	current, ok := m.currentInputVisualRow()
	if !ok || current != target.caret.visualRow {
		t.Fatalf("cursor visual row after scrolled click = %d (ok=%v), want %d", current, ok, target.caret.visualRow)
	}
	afterOffset := m.input.ScrollYOffset()
	if current < afterOffset || current >= afterOffset+m.input.Height() {
		t.Fatalf("clicked cursor row %d is outside viewport [%d,%d)", current, afterOffset, afterOffset+m.input.Height())
	}
}

func TestInputMouseSelection_DragPastViewportAutoScrolls(t *testing.T) {
	value := strings.Repeat("abcdefghij", 180)
	for _, direction := range []string{"up", "down"} {
		t.Run(direction, func(t *testing.T) {
			_ = spyInputSelectionClipboard(t)
			m := makeInputSelectionModel(t, value)
			if len(m.inputSurface.rows) <= m.inputBodyHeight {
				t.Fatalf("draft did not exceed viewport: rows=%d height=%d", len(m.inputSurface.rows), m.inputBodyHeight)
			}
			if direction == "down" {
				m.input.MoveToBegin()
				updated, _ := m.input.Update(inputSelectionCursorSyncMsg{})
				m.input = updated
				_ = m.View()
			} else {
				m.input.CursorUp()
				m.input.CursorDown()
				_ = m.View()
			}

			startOffset := m.input.ScrollYOffset()
			y := m.inputBodyStartY + m.inputBodyHeight/2
			x := m.inputContentStartX + 2
			m.Update(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
			outsideY := m.inputBodyStartY - 1
			if direction == "down" {
				outsideY = m.inputBodyStartY + m.inputBodyHeight
			}
			for range m.inputBodyHeight + 5 {
				m.Update(tea.MouseMotionMsg{X: x, Y: outsideY, Button: tea.MouseLeft})
				_ = m.View()
			}
			m.Update(tea.MouseReleaseMsg{X: x, Y: outsideY, Button: tea.MouseLeft})
			endOffset := m.input.ScrollYOffset()
			if direction == "up" && endOffset >= startOffset {
				t.Fatalf("upward edge drag did not scroll: %d -> %d", startOffset, endOffset)
			}
			if direction == "down" && endOffset <= startOffset {
				t.Fatalf("downward edge drag did not scroll: %d -> %d", startOffset, endOffset)
			}
			if !m.inputSelection.HasSelection(m.input.Value()) {
				t.Fatal("edge-scrolled drag did not retain a selection")
			}
		})
	}
}

func TestInputMouseSelection_CtrlCCopiesAndClearsInsteadOfQuitting(t *testing.T) {
	writes := spyInputSelectionClipboard(t)
	m := makeInputSelectionModel(t, "copy me")
	y := m.inputBodyStartY
	x := m.inputContentStartX
	dragInputSelection(m, x, y, x+3, y)
	if len(*writes) != 1 {
		t.Fatalf("mouse-up clipboard writes = %#v, want exactly one", *writes)
	}
	*writes = nil

	updated, cmd := m.handleKey(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})
	if updated != m || cmd != nil {
		t.Fatalf("Ctrl+C with a selection should be consumed in-place, got model=%T cmd=%v", updated, cmd)
	}
	if m.inputSelection.HasSelection(m.input.Value()) {
		t.Fatal("manual copy should clear the visual selection")
	}
	if len(*writes) != 1 || (*writes)[0] != "copy" {
		t.Fatalf("Ctrl+C clipboard writes = %#v, want [copy]", *writes)
	}
}

func TestInputMouseSelection_CtrlCPrecedesActiveTurnCancellation(t *testing.T) {
	writes := spyInputSelectionClipboard(t)
	m := makeInputSelectionModel(t, "copy me")
	y := m.inputBodyStartY
	x := m.inputContentStartX
	dragInputSelection(m, x, y, x+3, y)
	*writes = nil

	cancelled := 0
	m.turnActive = true
	m.turnCancel = func() { cancelled++ }
	_, firstCmd := m.handleKey(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})
	if firstCmd != nil || cancelled != 0 || len(*writes) != 1 || (*writes)[0] != "copy" {
		t.Fatalf("first Ctrl+C: cmd=%v cancelled=%d writes=%#v; want copy only", firstCmd, cancelled, *writes)
	}
	_, secondCmd := m.handleKey(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})
	if secondCmd != nil || cancelled != 1 {
		t.Fatalf("second Ctrl+C: cmd=%v cancelled=%d; want turn cancellation", secondCmd, cancelled)
	}
}

func TestInputMouseSelection_WhitespaceOnlyCopiesExactly(t *testing.T) {
	for _, value := range []string{"   ", "\n"} {
		t.Run(strings.ReplaceAll(value, "\n", "newline"), func(t *testing.T) {
			writes := spyInputSelectionClipboard(t)
			m := makeInputSelectionModel(t, value)
			if value == "   " {
				y := m.inputBodyStartY
				x := m.inputContentStartX
				dragInputSelection(m, x, y, x+2, y)
			} else {
				first := m.inputSurface.rows[0].defaultPoint
				second := m.inputSurface.rows[1].defaultPoint
				m.inputSelection = inputMouseSelection{
					value: value, anchor: first, focus: second, hasContent: true,
				}
				_, _ = m.handleKey(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})
			}
			if len(*writes) != 1 || (*writes)[0] != value {
				t.Fatalf("clipboard writes = %#v, want exact whitespace %q", *writes, value)
			}
		})
	}
}

func TestInputMouseSelection_OrdinaryTypingClearsHighlight(t *testing.T) {
	m := makeInputSelectionModel(t, "draft")
	y := m.inputBodyStartY
	x := m.inputContentStartX
	dragInputSelection(m, x, y, x+2, y)

	_, _ = m.handleKey(tea.KeyPressMsg{Text: "x", Code: 'x'})
	if m.inputSelection.HasSelection(m.input.Value()) {
		t.Fatal("ordinary editor input must dismiss the stale screen selection")
	}
}

func TestInputMouseSelection_GestureOwnerPreventsChatTakeover(t *testing.T) {
	home := t.TempDir()
	t.Setenv("METIS_HOME", home)
	m := makeInputSelectionModel(t, "alpha")
	y := m.inputBodyStartY
	x := m.inputContentStartX

	// Start in the composer, then drag upward across the chat. The composer
	// keeps ownership until release; transcript selection must not start.
	m.Update(tea.MouseClickMsg{X: x + 5, Y: y, Button: tea.MouseLeft})
	m.Update(tea.MouseMotionMsg{X: x, Y: chatStartY, Button: tea.MouseLeft})
	m.Update(tea.MouseReleaseMsg{X: x, Y: chatStartY, Button: tea.MouseLeft})

	if m.chatList.HasSelection() {
		t.Fatal("a composer-owned drag must not become a transcript selection")
	}
	if got := m.inputSelection.SelectedText(m.input.Value()); got != "alpha" {
		t.Fatalf("composer-owned cross-region selection = %q, want %q", got, "alpha")
	}
}

func TestInputMouseSelection_UsesActiveTranscriptGeometry(t *testing.T) {
	home := t.TempDir()
	t.Setenv("METIS_HOME", home)
	m := makeChatTestModel(t)
	m.input.SetValue("active draft")
	_ = m.View()
	y := m.inputBodyStartY
	x := m.inputContentStartX
	dragInputSelection(m, x, y, x+5, y)

	body, err := os.ReadFile(filepath.Join(home, "clipboard.txt"))
	if err != nil {
		t.Fatalf("clipboard fallback: %v", err)
	}
	if got := string(body); got != "active" {
		t.Fatalf("active-frame input selection = %q, want %q", got, "active")
	}
}

func TestInputMouseSelection_OverlayOwnersBlockMouseGestures(t *testing.T) {
	states := []struct {
		name  string
		apply func(*Model)
	}{
		{name: "transcript search", apply: func(m *Model) { m.showSearch = true }},
		{name: "ask user", apply: func(m *Model) { m.askUserActive = true }},
	}
	for _, tc := range states {
		t.Run(tc.name, func(t *testing.T) {
			writes := spyInputSelectionClipboard(t)
			m := makeInputSelectionModel(t, "blocked draft")
			beforeLine, beforeCol := m.input.Line(), m.input.Column()
			tc.apply(m)
			y := m.inputBodyStartY
			x := m.inputContentStartX
			dragInputSelection(m, x, y, x+6, y)
			if m.mouseOwner != mouseOwnerNone || m.inputSelection.HasSelection(m.input.Value()) {
				t.Fatalf("overlay allowed composer selection: owner=%v selection=%+v", m.mouseOwner, m.inputSelection)
			}
			if m.input.Line() != beforeLine || m.input.Column() != beforeCol {
				t.Fatalf("overlay mouse gesture moved caret: (%d,%d) -> (%d,%d)", beforeLine, beforeCol, m.input.Line(), m.input.Column())
			}
			if len(*writes) != 0 {
				t.Fatalf("overlay mouse gesture copied text: %#v", *writes)
			}
		})
	}
}

func TestInputMouseSelection_NarrowTerminalCannotHitClippedCells(t *testing.T) {
	for _, width := range []int{20, 21, 22} {
		t.Run(fmt.Sprintf("width_%d", width), func(t *testing.T) {
			m := newE2EModel(t, width, 20, 0)
			m.input = newComposer()
			m.input.SetValue(strings.Repeat("x", 40))
			view := m.View()
			lines := strings.Split(ansi.Strip(view.Content), "\n")
			if m.inputBodyStartY < 0 || m.inputBodyStartY >= len(lines) {
				t.Fatalf("input body row %d outside %d-line frame", m.inputBodyStartY, len(lines))
			}
			visibleWidth := ansi.StringWidth(lines[m.inputBodyStartY])
			for x := visibleWidth; x < width+4; x++ {
				if _, ok := m.inputPointAt(x, m.inputBodyStartY, false); ok {
					t.Fatalf("clipped terminal cell x=%d (visible width=%d) remained hit-testable", x, visibleWidth)
				}
			}
		})
	}
}
