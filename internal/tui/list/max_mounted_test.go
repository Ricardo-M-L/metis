package list

import "testing"

func TestSetMaxMounted_TruncatesOldest(t *testing.T) {
	l := NewList()
	for i := 0; i < 500; i++ {
		l.AppendItems(&fakeItem{idx: i, lines: 1})
	}
	l.SetMaxMounted(300)
	if got := l.Len(); got != 300 {
		t.Errorf("after SetMaxMounted(300) on 500-item list, Len=%d, want 300", got)
	}
}

func TestAppendItems_RespectsCap(t *testing.T) {
	l := NewList()
	l.SetMaxMounted(100)
	for i := 0; i < 500; i++ {
		l.AppendItems(&fakeItem{idx: i, lines: 1})
	}
	if got := l.Len(); got != 100 {
		t.Errorf("Append past cap, Len=%d, want 100", got)
	}
}

func TestSetMaxMounted_Zero_Unbounded(t *testing.T) {
	l := NewList()
	l.SetMaxMounted(0)
	for i := 0; i < 500; i++ {
		l.AppendItems(&fakeItem{idx: i, lines: 1})
	}
	if got := l.Len(); got != 500 {
		t.Errorf("unbounded list, Len=%d, want 500", got)
	}
}
