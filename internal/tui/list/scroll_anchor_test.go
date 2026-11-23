package list

import (
	"fmt"
	"strings"
	"testing"
)

type anchoredTestItem struct {
	id    string
	lines int
	wrap  bool
}

func (i *anchoredTestItem) Render(width int) string {
	lines := i.lines
	if i.wrap {
		lines = max(1, 80/max(1, width))
	}
	rows := make([]string, lines)
	for row := range rows {
		rows[row] = fmt.Sprintf("%s-row-%d", i.id, row)
	}
	return strings.Join(rows, "\n")
}

func (i *anchoredTestItem) ScrollAnchorID() string { return i.id }
func (i *anchoredTestItem) ContainsScrollAnchor(id string) bool {
	return id != "" && id == i.id
}

func TestScrollAnchor_RebuildPreservesIdentityAndRow(t *testing.T) {
	l := NewList(&anchoredTestItem{id: "removed", lines: 3}, &anchoredTestItem{id: "reading", lines: 10}, &anchoredTestItem{id: "tail", lines: 20})
	l.SetSize(80, 5)
	l.ScrollToIndex(1)
	l.ScrollBy(4)
	l.SetItemsKeepScroll(&anchoredTestItem{id: "reading", lines: 10}, &anchoredTestItem{id: "tail", lines: 20})
	if l.offsetIdx != 0 || l.offsetLine != 4 || !strings.HasPrefix(l.Render(), "reading-row-4\n") {
		t.Fatalf("anchor changed: item=%d row=%d\n%s", l.offsetIdx, l.offsetLine, l.Render())
	}
}

func TestScrollAnchor_ShrinkingItemKeepsAVisibleRow(t *testing.T) {
	l := NewList(&anchoredTestItem{id: "reading", lines: 20}, &anchoredTestItem{id: "tail", lines: 20})
	l.SetSize(80, 5)
	l.ScrollBy(15)
	l.SetItemsKeepScroll(&anchoredTestItem{id: "reading", lines: 2}, &anchoredTestItem{id: "tail", lines: 20})
	if l.offsetIdx != 0 || l.offsetLine != 1 || !strings.HasPrefix(l.Render(), "reading-row-1\n") {
		t.Fatalf("shorter anchor was skipped: item=%d row=%d\n%s", l.offsetIdx, l.offsetLine, l.Render())
	}
}

func TestScrollAnchor_ResizeClampsRowWithinSameItem(t *testing.T) {
	l := NewList(&anchoredTestItem{id: "reading", wrap: true}, &anchoredTestItem{id: "tail", lines: 20})
	l.SetSize(10, 5)
	l.ScrollBy(6)
	l.SetSize(40, 5)
	if l.offsetIdx != 0 || l.offsetLine != 1 || !strings.HasPrefix(l.Render(), "reading-row-1\n") {
		t.Fatalf("resize skipped anchored content: item=%d row=%d\n%s", l.offsetIdx, l.offsetLine, l.Render())
	}
}

func TestScrollAnchor_RebuildWithMountedCapKeepsSameItem(t *testing.T) {
	items := []Item{
		&anchoredTestItem{id: "old", lines: 3}, &anchoredTestItem{id: "reading", lines: 10},
		&anchoredTestItem{id: "later", lines: 10}, &anchoredTestItem{id: "tail", lines: 10},
	}
	l := NewList()
	l.SetSize(80, 5)
	l.SetMaxMounted(3)
	l.SetItemsKeepScroll(items...)
	l.ScrollBy(4)
	for round := 0; round < 3; round++ {
		l.SetItemsKeepScroll(items...)
		if l.offsetIdx != 0 || l.offsetLine != 4 || !strings.HasPrefix(l.Render(), "reading-row-4\n") {
			t.Fatalf("round %d moved mounted anchor: item=%d row=%d\n%s", round, l.offsetIdx, l.offsetLine, l.Render())
		}
	}
}

func TestScrollAnchor_RebuildPreservesGapRow(t *testing.T) {
	l := NewList(&anchoredTestItem{id: "reading", lines: 2}, &anchoredTestItem{id: "tail", lines: 20})
	l.SetGap(1)
	l.SetSize(80, 5)
	l.ScrollToIndex(1)
	l.ScrollBy(-1)
	if l.offsetIdx != 0 || l.offsetLine != 2 {
		t.Fatalf("setup did not reach gap: item=%d row=%d", l.offsetIdx, l.offsetLine)
	}
	l.SetItemsKeepScroll(&anchoredTestItem{id: "reading", lines: 2}, &anchoredTestItem{id: "tail", lines: 20})
	if l.offsetIdx != 0 || l.offsetLine != 2 || !strings.HasPrefix(l.Render(), "\n") {
		t.Fatalf("rebuild moved gap: item=%d row=%d\n%s", l.offsetIdx, l.offsetLine, l.Render())
	}
}

func TestScrollAnchor_RebuildClearsSelectionWhenCoordinatesChange(t *testing.T) {
	l := NewList(&anchoredTestItem{id: "old", lines: 1}, &anchoredTestItem{id: "reading", lines: 5})
	l.SetSize(80, 4)
	l.ScrollToIndex(1)
	l.HandleMouseDown(0, 0, 1)
	l.HandleMouseDrag(7, 0)
	l.HandleMouseUp()
	if !l.HasSelection() {
		t.Fatal("setup: selection missing")
	}
	l.SetItemsKeepScroll(&anchoredTestItem{id: "reading", lines: 5})
	if l.HasSelection() || l.SelectedText() != "" {
		t.Fatal("selection retained obsolete indices after anchor moved")
	}
}

func TestScrollAnchor_RebuildPreservesSelectionOnUnchangedRows(t *testing.T) {
	l := NewList(&anchoredTestItem{id: "reading", lines: 5}, &anchoredTestItem{id: "tail", lines: 5})
	l.SetSize(80, 4)
	l.HandleMouseDown(0, 0, 1)
	l.HandleMouseDrag(7, 0)
	l.HandleMouseUp()
	want := l.SelectedText()
	l.SetItemsKeepScroll(&anchoredTestItem{id: "reading", lines: 5}, &anchoredTestItem{id: "tail", lines: 6})
	if want == "" || l.SelectedText() != want {
		t.Fatalf("unrelated tail update changed selection: %q -> %q", want, l.SelectedText())
	}
	l.SetItemsKeepScroll(&anchoredTestItem{id: "reading", lines: 3}, &anchoredTestItem{id: "tail", lines: 6})
	if l.HasSelection() {
		t.Fatal("same ID with changed content retained stale selection")
	}
}

type positionedTestItem struct {
	anchoredTestItem
	positionCalls *int
}

func (i *positionedTestItem) SameScrollAnchorLayout(other Item) bool {
	previous, ok := other.(*positionedTestItem)
	return ok && i.id == previous.id && i.lines == previous.lines
}
func (i *positionedTestItem) ScrollAnchorAt(line, width int) (string, int) {
	*i.positionCalls++
	return i.id, line
}
func (i *positionedTestItem) ScrollAnchorLine(id string, line, width int) (int, bool) {
	*i.positionCalls++
	return line, id == i.id
}

func TestScrollAnchor_UnchangedCompositeSkipsMemberLayout(t *testing.T) {
	calls := 0
	l := NewList(&positionedTestItem{anchoredTestItem: anchoredTestItem{id: "group", lines: 100}, positionCalls: &calls})
	l.SetSize(80, 5)
	l.ScrollBy(30)
	calls = 0 // Initial width negotiation may lay out the existing item.
	for round := 0; round < 3; round++ {
		l.SetItemsKeepScroll(&positionedTestItem{anchoredTestItem: anchoredTestItem{id: "group", lines: 100}, positionCalls: &calls})
	}
	if calls != 0 || l.offsetLine != 30 {
		t.Fatalf("stable composite recalculated layout: calls=%d row=%d", calls, l.offsetLine)
	}
}
