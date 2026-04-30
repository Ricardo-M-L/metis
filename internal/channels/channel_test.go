package channels

import (
	"context"
	"errors"
	"testing"
)

type stubAdapter struct {
	name       string
	configured bool
	called     bool
	gotTarget  string
	gotMsg     Message
	err        error
}

func (a *stubAdapter) Name() string     { return a.name }
func (a *stubAdapter) Configured() bool { return a.configured }
func (a *stubAdapter) Send(_ context.Context, target string, msg Message) error {
	a.called = true
	a.gotTarget = target
	a.gotMsg = msg
	return a.err
}

func TestRegistry_RegisterDropsUnconfigured(t *testing.T) {
	r := NewRegistry()
	r.Register(&stubAdapter{name: "x", configured: false})
	if _, ok := r.Get("x"); ok {
		t.Error("unconfigured adapter should not be registered")
	}
}

func TestRegistry_NamesSorted(t *testing.T) {
	r := NewRegistry()
	r.Register(&stubAdapter{name: "telegram", configured: true})
	r.Register(&stubAdapter{name: "discord", configured: true})
	r.Register(&stubAdapter{name: "slack", configured: true})

	names := r.Names()
	want := []string{"discord", "slack", "telegram"}
	if len(names) != 3 {
		t.Fatalf("got %v", names)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Errorf("Names()[%d] = %q, want %q", i, names[i], want[i])
		}
	}
}

func TestRegistry_SendRoutesByPrefix(t *testing.T) {
	a := &stubAdapter{name: "slack", configured: true}
	r := NewRegistry()
	r.Register(a)

	if err := r.Send(context.Background(), "slack:#general", "", Message{Text: "hi"}); err != nil {
		t.Fatal(err)
	}
	if !a.called {
		t.Fatal("adapter not called")
	}
	if a.gotTarget != "#general" {
		t.Errorf("target = %q, want #general", a.gotTarget)
	}
	if a.gotMsg.Text != "hi" {
		t.Errorf("text = %q", a.gotMsg.Text)
	}
}

func TestRegistry_SendUsesDefaultPlatformWhenNoPrefix(t *testing.T) {
	a := &stubAdapter{name: "slack", configured: true}
	r := NewRegistry()
	r.Register(a)

	if err := r.Send(context.Background(), "#general", "slack", Message{Text: "hi"}); err != nil {
		t.Fatal(err)
	}
	if a.gotTarget != "#general" {
		t.Errorf("target = %q", a.gotTarget)
	}
}

func TestRegistry_SendUnknownPlatform(t *testing.T) {
	r := NewRegistry()
	if err := r.Send(context.Background(), "nope:x", "", Message{}); err == nil {
		t.Error("expected error for unknown platform")
	}
}

func TestRegistry_SendNoDefaultAndNoPrefix(t *testing.T) {
	r := NewRegistry()
	r.Register(&stubAdapter{name: "x", configured: true})
	if err := r.Send(context.Background(), "target-only", "", Message{}); err == nil {
		t.Error("expected error when no default platform configured")
	}
}

func TestRegistry_SendPropagatesAdapterError(t *testing.T) {
	a := &stubAdapter{name: "slack", configured: true, err: errors.New("oops")}
	r := NewRegistry()
	r.Register(a)
	if err := r.Send(context.Background(), "slack:#x", "", Message{}); err == nil {
		t.Error("adapter error should propagate")
	}
}
