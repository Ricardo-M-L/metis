package list

import (
	"fmt"
	"slices"
	"strings"
	"testing"
)

type rowLayoutFixture struct {
	rows        []string
	renderCalls int
	rowCalls    int
}

func (i *rowLayoutFixture) Render(int) string {
	i.renderCalls++
	panic("RowLayoutItem must not flatten historical text for list operations")
}
func (i *rowLayoutFixture) RenderRows(int) []string {
	i.rowCalls++
	return i.rows
}

func TestRowLayoutFeedsGeometryRenderingSelectionAndLinksWithoutFlattening(t *testing.T) {
	item := &rowLayoutFixture{rows: []string{"hello world", "second row", "third row", "fourth row"}}
	original := slices.Clone(item.rows)
	l := NewList(item, &rowLayoutFixture{rows: []string{"tail"}})
	l.SetSize(80, 2)
	l.SetGap(2)
	if got := l.TotalLineCount(); got != 7 {
		t.Fatalf("height=%d, want 7", got)
	}
	if got := l.Render(); got != "hello world\nsecond row" {
		t.Fatalf("render=%q", got)
	}
	l.HandleMouseDown(0, 0, 1)
	l.HandleMouseDrag(5, 0)
	l.HandleMouseUp()
	if got := l.SelectedText(); got != "hello" {
		t.Fatalf("selection=%q", got)
	}
	_ = l.Render() // selection overlay must not modify shared cached rows
	_ = l.URLAtPoint(0, 0, 0)
	l.ScrollBy(1)
	if idx, row := l.ItemAtY(0); idx != 0 || row != 1 {
		t.Fatalf("hit=%d,%d", idx, row)
	}
	l.ScrollToBottom()
	if !l.AtBottom() || l.ScrollPercent() != 1 {
		t.Fatal("bottom geometry disagrees")
	}
	if got := l.ItemContent(0); got != strings.Join(original, "\n") {
		t.Fatalf("public content=%q", got)
	}
	if item.renderCalls != 0 || !slices.Equal(item.rows, original) {
		t.Fatal("historical text flattened or cached rows modified")
	}
}

func TestVisibleHeightStopsAtViewportWithoutMeasuringHistoryTail(t *testing.T) {
	first := &rowLayoutFixture{rows: []string{"one", "two", "three"}}
	tail := &rowLayoutFixture{rows: []string{"tail"}}
	l := NewList(first, tail)
	l.SetGap(3)
	for _, limit := range []int{-1, 0, 1, 3, 4, 6} {
		first.rowCalls, tail.rowCalls = 0, 0
		if got := l.VisibleHeight(limit); got != max(0, limit) {
			t.Fatalf("limit %d: height=%d", limit, got)
		}
		if tail.rowCalls != 0 {
			t.Fatalf("limit %d measured tail %d times", limit, tail.rowCalls)
		}
	}
	if got := l.VisibleHeight(100); got != 7 || tail.rowCalls != 1 {
		t.Fatalf("short content height=%d tail calls=%d", got, tail.rowCalls)
	}
}

func TestRowLayoutNormalizesEmptyAndTrailingRowsLikeStringItems(t *testing.T) {
	for _, rows := range [][]string{nil, {""}, {"", ""}, {"first", ""}, {"", "first", "", ""}} {
		t.Run(fmt.Sprintf("rows-%q", rows), func(t *testing.T) {
			l := NewList(&rowLayoutFixture{rows: rows})
			l.SetSize(80, 3)
			fallback := NewList(&staticItem{content: strings.Join(rows, "\n")})
			fallback.SetSize(80, 3)
			if l.TotalLineCount() != fallback.TotalLineCount() || l.Render() != fallback.Render() {
				t.Fatalf("row layout differs from fallback: %q vs %q", l.Render(), fallback.Render())
			}
		})
	}
}

func BenchmarkCachedLongItemViewportRows(b *testing.B) {
	rows := make([]string, 50000)
	for i := range rows {
		rows[i] = "cached historical row"
	}
	l := NewList(&rowLayoutFixture{rows: rows})
	l.SetSize(100, 30)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = l.Render()
	}
}
