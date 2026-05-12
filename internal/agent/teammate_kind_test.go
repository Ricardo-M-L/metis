package agent

// teammate_kind_test.go — locks Phase G.12 (kind tags) + G.16
// (Roster summary) contracts. 2026-05-12.
//
// Five contracts:
//
//   1. TeammateKind.String maps each value to its expected label.
//   2. Teammate.Kind derives from Anonymous (named ↔ KindNamed,
//      anon ↔ KindAnon).
//   3. nil Teammate.Kind returns KindAnon safely.
//   4. Roster.Summary counts total/named/anonymous/background.
//   5. Empty roster summary returns the zero value (Total=0).

import (
	"testing"
)

func TestTeammateKind_String(t *testing.T) {
	t.Parallel()
	cases := map[TeammateKind]string{
		KindAnon:       "anon",
		KindNamed:      "named",
		KindWorkflow:   "workflow",
		KindMcpMonitor: "mcp_monitor",
		TeammateKind(999): "unknown",
	}
	for k, want := range cases {
		if got := k.String(); got != want {
			t.Errorf("TeammateKind(%d).String = %q, want %q", k, got, want)
		}
	}
}

func TestTeammate_KindDerivedFromAnonymous(t *testing.T) {
	t.Parallel()
	named := &Teammate{Anonymous: false}
	if got := named.Kind(); got != KindNamed {
		t.Errorf("named teammate Kind = %v, want KindNamed", got)
	}
	anon := &Teammate{Anonymous: true}
	if got := anon.Kind(); got != KindAnon {
		t.Errorf("anon teammate Kind = %v, want KindAnon", got)
	}
	var nilT *Teammate
	if got := nilT.Kind(); got != KindAnon {
		t.Errorf("nil teammate Kind = %v, want KindAnon (nil-safe)", got)
	}
}

func TestRoster_Summary_EmptyZeroValue(t *testing.T) {
	t.Parallel()
	r := NewRoster(0)
	got := r.Summary()
	if got.Total != 0 || got.Named != 0 || got.Anonymous != 0 || got.Background != 0 {
		t.Errorf("empty Roster.Summary should be zero-valued; got %+v", got)
	}
}

func TestRoster_Summary_Counts(t *testing.T) {
	t.Parallel()
	r := NewRoster(0)
	// 2 named, 1 anonymous, 1 background-named, 1 background-anon.
	if err := r.Register(&Teammate{Name: "alice"}); err != nil {
		t.Fatal(err)
	}
	if err := r.Register(&Teammate{Name: "bob"}); err != nil {
		t.Fatal(err)
	}
	if err := r.Register(&Teammate{}); err != nil { // anonymous, auto-named
		t.Fatal(err)
	}
	if err := r.Register(&Teammate{Name: "carol", Background: true}); err != nil {
		t.Fatal(err)
	}
	if err := r.Register(&Teammate{Background: true}); err != nil { // anon background
		t.Fatal(err)
	}

	got := r.Summary()
	if got.Total != 5 {
		t.Errorf("Total = %d, want 5", got.Total)
	}
	if got.Named != 3 {
		t.Errorf("Named = %d, want 3", got.Named)
	}
	if got.Anonymous != 2 {
		t.Errorf("Anonymous = %d, want 2", got.Anonymous)
	}
	if got.Background != 2 {
		t.Errorf("Background = %d, want 2", got.Background)
	}
}

func TestRoster_Summary_NilReceiver(t *testing.T) {
	t.Parallel()
	var r *Roster
	got := r.Summary()
	if got.Total != 0 {
		t.Errorf("nil roster Summary should be zero value; got %+v", got)
	}
}
