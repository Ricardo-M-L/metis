package channel

import (
	"context"
	"errors"
	"testing"
)

// fakeAdapter is the bare-minimum Adapter implementation a 3rd party
// would write. We use it both to verify the Registry plumbing and to
// document the contract authors target.
type fakeAdapter struct {
	name       string
	configured bool
	gotTarget  string
	gotMessage Message
	sendErr    error
}

func (a *fakeAdapter) Name() string     { return a.name }
func (a *fakeAdapter) Configured() bool { return a.configured }
func (a *fakeAdapter) Send(_ context.Context, target string, m Message) error {
	a.gotTarget = target
	a.gotMessage = m
	return a.sendErr
}

func TestRegister_DropsUnconfiguredAdapters(t *testing.T) {
	r := NewRegistry()
	r.Register(&fakeAdapter{name: "slack", configured: false})
	if _, ok := r.Get("slack"); ok {
		t.Error("unconfigured adapter should be silently dropped")
	}
}

func TestRegister_KeepsConfiguredAdapters(t *testing.T) {
	r := NewRegistry()
	r.Register(&fakeAdapter{name: "slack", configured: true})
	if _, ok := r.Get("slack"); !ok {
		t.Error("configured adapter should be retained")
	}
}

func TestRegister_NilSafe(t *testing.T) {
	r := NewRegistry()
	r.Register(nil) // must not panic
	if len(r.Names()) != 0 {
		t.Error("nil registration should not affect the registry")
	}
}

func TestNames_Sorted(t *testing.T) {
	r := NewRegistry()
	r.Register(&fakeAdapter{name: "slack", configured: true})
	r.Register(&fakeAdapter{name: "discord", configured: true})
	r.Register(&fakeAdapter{name: "telegram", configured: true})
	got := r.Names()
	want := []string{"discord", "slack", "telegram"}
	if len(got) != len(want) {
		t.Fatalf("Names len = %d, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("Names[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestSend_RoutesToCorrectAdapter(t *testing.T) {
	slack := &fakeAdapter{name: "slack", configured: true}
	tg := &fakeAdapter{name: "telegram", configured: true}
	r := NewRegistry()
	r.Register(slack)
	r.Register(tg)

	if err := r.Send(context.Background(), "telegram:42", "slack",
		Message{Text: "hi"}); err != nil {
		t.Fatalf("Send err: %v", err)
	}
	if tg.gotTarget != "42" || tg.gotMessage.Text != "hi" {
		t.Errorf("telegram adapter didn't receive call: target=%q msg=%+v", tg.gotTarget, tg.gotMessage)
	}
	if slack.gotTarget != "" {
		t.Errorf("slack adapter shouldn't have been called; got target=%q", slack.gotTarget)
	}
}

func TestSend_UsesDefaultPlatformWhenNoPrefix(t *testing.T) {
	tg := &fakeAdapter{name: "telegram", configured: true}
	r := NewRegistry()
	r.Register(tg)
	if err := r.Send(context.Background(), "channel-id", "telegram",
		Message{Text: "x"}); err != nil {
		t.Fatalf("Send err: %v", err)
	}
	if tg.gotTarget != "channel-id" {
		t.Errorf("default-platform path didn't pass target through; got %q", tg.gotTarget)
	}
}

func TestSend_NoDefaultErrorsClearly(t *testing.T) {
	r := NewRegistry()
	err := r.Send(context.Background(), "no-prefix", "", Message{})
	if err == nil {
		t.Fatal("missing default platform should error")
	}
}

func TestSend_PropagatesAdapterError(t *testing.T) {
	want := errors.New("network failure")
	a := &fakeAdapter{name: "x", configured: true, sendErr: want}
	r := NewRegistry()
	r.Register(a)
	got := r.Send(context.Background(), "x:foo", "", Message{})
	if !errors.Is(got, want) {
		t.Errorf("adapter error not propagated: got %v, want %v", got, want)
	}
}

func TestParseChannel(t *testing.T) {
	cases := []struct {
		in, def, plat, target string
	}{
		{"slack:#general", "telegram", "slack", "#general"},
		{"telegram:42", "slack", "telegram", "42"},
		{"plain-target", "slack", "slack", "plain-target"},
		{"plain-target", "", "", "plain-target"},
	}
	for _, c := range cases {
		gotPlat, gotT := ParseChannel(c.in, c.def)
		if gotPlat != c.plat || gotT != c.target {
			t.Errorf("ParseChannel(%q, %q) = (%q, %q), want (%q, %q)",
				c.in, c.def, gotPlat, gotT, c.plat, c.target)
		}
	}
}
