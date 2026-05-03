package overlay

import (
	"testing"

	tea "charm.land/bubbletea/v2"
)

// fakeOverlay is a minimal Overlay impl driven by exported fields so
// tests can poke its state. The "consume next key + dismiss" behavior
// covers the path Esc-style closes use.
type fakeOverlay struct {
	name        string
	active      bool
	pushedCount int
	poppedCount int
	consume     bool
	updateCount int
}

func (f *fakeOverlay) Name() string { return f.name }
func (f *fakeOverlay) Active() bool { return f.active }
func (f *fakeOverlay) Update(msg tea.KeyMsg) (Overlay, tea.Cmd, bool) {
	f.updateCount++
	return f, nil, f.consume
}
func (f *fakeOverlay) View(int, int) string { return "<" + f.name + ">" }
func (f *fakeOverlay) OnPush() tea.Cmd      { f.pushedCount++; return nil }
func (f *fakeOverlay) OnPop() tea.Cmd       { f.poppedCount++; return nil }

func TestStack_PushAndTop(t *testing.T) {
	s := New()
	if s.Top() != nil {
		t.Errorf("empty stack Top should be nil")
	}
	a := &fakeOverlay{name: "a", active: true}
	b := &fakeOverlay{name: "b", active: true}
	s.Push(a)
	if s.Top() != a {
		t.Errorf("Top after one push should be a")
	}
	s.Push(b)
	if s.Top() != b {
		t.Errorf("Top after pushing b should be b")
	}
	if a.pushedCount != 1 || b.pushedCount != 1 {
		t.Errorf("OnPush count wrong: a=%d b=%d", a.pushedCount, b.pushedCount)
	}
}

func TestStack_DuplicateNameReplaces(t *testing.T) {
	s := New()
	a := &fakeOverlay{name: "x", active: true}
	a2 := &fakeOverlay{name: "x", active: true}
	s.Push(a)
	s.Push(a2)
	if s.Len() != 1 {
		t.Errorf("duplicate name should replace, len = %d", s.Len())
	}
	if a.poppedCount != 1 {
		t.Errorf("old overlay should have OnPop fired: %d", a.poppedCount)
	}
	if s.Top() != a2 {
		t.Errorf("Top should be the replacement")
	}
}

func TestStack_PopRemovesTopActive(t *testing.T) {
	s := New()
	a := &fakeOverlay{name: "a", active: true}
	b := &fakeOverlay{name: "b", active: true}
	s.Push(a)
	s.Push(b)
	s.Pop()
	if b.poppedCount != 1 {
		t.Errorf("b should be popped: %d", b.poppedCount)
	}
	if s.Top() != a {
		t.Errorf("after popping b, Top should be a")
	}
}

func TestStack_TopSkipsInactive(t *testing.T) {
	s := New()
	a := &fakeOverlay{name: "a", active: true}
	b := &fakeOverlay{name: "b", active: false}
	s.Push(a)
	s.Push(b)
	if s.Top() != a {
		t.Errorf("inactive top should be skipped, got %v", s.Top())
	}
	if !s.Active() {
		t.Errorf("Active() should be true since `a` is active")
	}
}

func TestStack_UpdateRoutesToTop(t *testing.T) {
	s := New()
	a := &fakeOverlay{name: "a", active: true, consume: true}
	b := &fakeOverlay{name: "b", active: true, consume: true}
	s.Push(a)
	s.Push(b)
	_, consumed := s.Update(tea.KeyPressMsg{Code: 'k', Text: "k"})
	if !consumed {
		t.Errorf("top consume=true should propagate")
	}
	if b.updateCount != 1 {
		t.Errorf("b should have received update: %d", b.updateCount)
	}
	if a.updateCount != 0 {
		t.Errorf("a (below) should NOT have received update: %d", a.updateCount)
	}
}

func TestStack_UpdateNoOpWhenEmpty(t *testing.T) {
	s := New()
	_, consumed := s.Update(tea.KeyPressMsg{Code: 'k', Text: "k"})
	if consumed {
		t.Errorf("empty stack should not consume")
	}
}

func TestStack_PopByName(t *testing.T) {
	s := New()
	a := &fakeOverlay{name: "a", active: true}
	b := &fakeOverlay{name: "b", active: true}
	c := &fakeOverlay{name: "c", active: true}
	s.Push(a)
	s.Push(b)
	s.Push(c)
	s.PopByName("b")
	if b.poppedCount != 1 {
		t.Errorf("b should be popped")
	}
	if s.Len() != 2 {
		t.Errorf("len after PopByName = %d, want 2", s.Len())
	}
	if s.Top() != c {
		t.Errorf("c should remain top")
	}
}

func TestStack_View_OnlyActive(t *testing.T) {
	s := New()
	s.Push(&fakeOverlay{name: "a", active: true})
	s.Push(&fakeOverlay{name: "b", active: false})
	s.Push(&fakeOverlay{name: "c", active: true})
	views := s.View(80, 24)
	if len(views) != 2 {
		t.Errorf("View should skip inactive, got %d entries", len(views))
	}
}
