package list

import (
	"fmt"
	"math"
	"strings"
	"testing"
)

func TestScrollByUsesEveryRenderedGapRowInBothDirections(t *testing.T) {
	for _, gap := range []int{0, 1, 3} {
		t.Run(fmt.Sprintf("gap-%d", gap), func(t *testing.T) {
			items := []Item{&fakeItem{idx: 0, lines: 2}, &fakeItem{idx: 1, lines: 1}, &fakeItem{idx: 2, lines: 4}, &fakeItem{idx: 3, lines: 3}}
			l := NewList(items...)
			l.SetSize(80, 3)
			l.SetGap(gap)
			var rows []string
			for index, item := range items {
				if index > 0 {
					rows = append(rows, make([]string, gap)...)
				}
				rows = append(rows, strings.Split(item.Render(80), "\n")...)
			}
			last := len(rows) - l.Height()
			check := func(offset int) {
				t.Helper()
				if got, want := l.Render(), strings.Join(rows[offset:offset+l.Height()], "\n"); got != want {
					t.Fatalf("offset %d: got %q, want %q (item=%d row=%d)", offset, got, want, l.offsetIdx, l.offsetLine)
				}
				if got := l.ScrollPercent(); math.Abs(got-float64(offset)/float64(last)) > 1e-9 {
					t.Fatalf("offset %d: percent=%f, want %f", offset, got, float64(offset)/float64(last))
				}
				if got := l.AtBottom(); got != (offset == last) {
					t.Fatalf("offset %d: AtBottom=%v", offset, got)
				}
			}
			check(0)
			for offset := 1; offset <= last; offset++ {
				l.ScrollBy(1)
				check(offset)
			}
			l.ScrollBy(100)
			check(last)
			for offset := last - 1; offset >= 0; offset-- {
				l.ScrollBy(-1)
				check(offset)
			}
			l.ScrollBy(-100)
			check(0)
		})
	}
}

func TestScrollByGapRowsRetainHitTestAndTextSelection(t *testing.T) {
	l := NewList(&fakeItem{idx: 0, lines: 2}, &fakeItem{idx: 1, lines: 4})
	l.SetSize(80, 3)
	l.SetGap(2)
	l.HandleMouseDown(0, 0, 1)
	l.HandleMouseDrag(5, 1)
	l.HandleMouseUp()
	want := l.SelectedText()
	if want == "" {
		t.Fatal("setup selection empty")
	}
	l.ScrollBy(2)
	for _, y := range []int{0, 1} {
		if item, row := l.ItemAtY(y); item != -1 || row != -1 {
			t.Fatalf("gap at y=%d selected item=%d row=%d", y, item, row)
		}
	}
	if item, row := l.ItemAtY(2); item != 1 || row != 0 {
		t.Fatalf("row after gap selected item=%d row=%d", item, row)
	}
	if got := l.SelectedText(); got != want {
		t.Fatalf("scroll changed logical selection: %q -> %q", want, got)
	}
	l.ScrollBy(-2)
	if l.offsetIdx != 0 || l.offsetLine != 0 || l.SelectedText() != want {
		t.Fatal("reverse scroll did not restore top while retaining selection")
	}
}
