package overlay

import (
	"context"
	"errors"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

func TestBtwOverlay_LifecycleHappyPath(t *testing.T) {
	called := 0
	ask := func(_ context.Context, q string) (string, error) {
		called++
		if q != "what time is it" {
			t.Errorf("ask got %q", q)
		}
		return "  late  ", nil
	}
	o := NewBtwOverlay(context.Background(), "what time is it", ask)
	if !o.loading {
		t.Errorf("should start loading")
	}
	cmd := o.OnPush()
	if cmd == nil {
		t.Fatalf("OnPush should return Cmd")
	}
	msg := cmd().(BtwAnswerMsg)
	if msg.Err != nil {
		t.Errorf("unexpected err: %v", msg.Err)
	}
	o.Apply(msg)
	if o.loading {
		t.Errorf("should clear loading after Apply")
	}
	if o.answer != "late" {
		t.Errorf("answer should be trimmed: %q", o.answer)
	}
	if called != 1 {
		t.Errorf("ask should be called once: %d", called)
	}
}

func TestBtwOverlay_ErrorPath(t *testing.T) {
	ask := func(_ context.Context, _ string) (string, error) {
		return "", errors.New("api down")
	}
	o := NewBtwOverlay(context.Background(), "anything", ask)
	cmd := o.OnPush()
	o.Apply(cmd().(BtwAnswerMsg))
	if o.err != "api down" {
		t.Errorf("err = %q", o.err)
	}
	view := o.View(80, 24)
	if !strings.Contains(view, "error") || !strings.Contains(view, "api down") {
		t.Errorf("error view missing reason: %q", view)
	}
}

func TestBtwOverlay_NoBackendShowsImmediateError(t *testing.T) {
	o := NewBtwOverlay(context.Background(), "q", nil)
	if cmd := o.OnPush(); cmd != nil {
		t.Errorf("OnPush should not Cmd when ask is nil")
	}
	if o.loading {
		t.Errorf("should clear loading immediately")
	}
	if !strings.Contains(o.err, "not wired") {
		t.Errorf("err = %q", o.err)
	}
}

func TestBtwOverlay_EscDismisses(t *testing.T) {
	o := NewBtwOverlay(context.Background(), "q", func(context.Context, string) (string, error) { return "", nil })
	_, _, consumed := o.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if !consumed {
		t.Errorf("Esc should be consumed")
	}
	if o.Active() {
		t.Errorf("after Esc Active should be false")
	}
	if v := o.View(80, 24); v != "" {
		t.Errorf("inactive View should be empty, got %q", v)
	}
}

func TestBtwOverlay_NonEscDoesNotConsume(t *testing.T) {
	o := NewBtwOverlay(context.Background(), "q", func(context.Context, string) (string, error) { return "", nil })
	_, _, consumed := o.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'a'}})
	if consumed {
		t.Errorf("regular keys should pass through to input box")
	}
}

func TestBtwOverlay_LoadingViewHasSpinner(t *testing.T) {
	o := NewBtwOverlay(context.Background(), "q", func(context.Context, string) (string, error) { return "", nil })
	view := o.View(80, 24)
	if !strings.Contains(view, "thinking") {
		t.Errorf("loading view should mention thinking: %q", view)
	}
	if !strings.Contains(view, "/btw") {
		t.Errorf("missing title: %q", view)
	}
}
