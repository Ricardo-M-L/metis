package main

// agents_slash_test.go — locks Phase G.11 + G.17 (2026-05-12) /agents
// slash subcommand surface.
//
// Six contracts:
//
//   1. nil roster → safe message (no panic).
//   2. Empty roster → "roster empty" hint.
//   3. /agents list hides anonymous teammates by default.
//   4. /agents list all reveals them.
//   5. /agents status <name> renders the snapshot.
//   6. /agents kill <name> calls the Cancel func.

import (
	"context"
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/agent"
)

func TestFormatAgents_NilRoster(t *testing.T) {
	out := formatAgentsCommand(nil, "list")
	if !strings.Contains(out, "no Roster wired") {
		t.Errorf("nil roster should mention 'no Roster wired'; got %q", out)
	}
}

func TestFormatAgents_EmptyRoster(t *testing.T) {
	roster := agent.NewRoster(0)
	out := formatAgentsCommand(roster, "")
	if !strings.Contains(out, "roster empty") {
		t.Errorf("empty roster output missing hint: %q", out)
	}
}

func TestFormatAgents_HidesAnonByDefault(t *testing.T) {
	roster := agent.NewRoster(0)
	if err := roster.Register(&agent.Teammate{AgentID: "agt-named1"}); err != nil {
		// Empty-name path auto-assigns _anon-. Use Name explicitly.
		t.Fatal(err)
	}
	if err := roster.Register(&agent.Teammate{Name: "alice", AgentID: "agt-alice"}); err != nil {
		t.Fatal(err)
	}
	out := formatAgentsCommand(roster, "list")
	if !strings.Contains(out, "alice") {
		t.Errorf("named teammate should be visible by default: %q", out)
	}
	if !strings.Contains(out, "hidden") {
		t.Errorf("anonymous count should be reported: %q", out)
	}
	if strings.Contains(out, "(anon)") {
		t.Errorf("anonymous teammate should be hidden by default: %q", out)
	}
}

func TestFormatAgents_ListAllShowsAnon(t *testing.T) {
	roster := agent.NewRoster(0)
	if err := roster.Register(&agent.Teammate{AgentID: "agt-anon1"}); err != nil {
		t.Fatal(err)
	}
	out := formatAgentsCommand(roster, "list all")
	if !strings.Contains(out, "(anon)") {
		t.Errorf("`list all` should reveal anonymous teammates: %q", out)
	}
}

func TestFormatAgents_StatusByName(t *testing.T) {
	roster := agent.NewRoster(0)
	if err := roster.Register(&agent.Teammate{Name: "bob", AgentID: "agt-bob"}); err != nil {
		t.Fatal(err)
	}
	out := formatAgentsCommand(roster, "status bob")
	for _, want := range []string{"agt-bob", "bob", "named=true"} {
		if !strings.Contains(out, want) {
			t.Errorf("status output missing %q:\n%s", want, out)
		}
	}
}

func TestFormatAgents_StatusUnknown(t *testing.T) {
	roster := agent.NewRoster(0)
	out := formatAgentsCommand(roster, "status ghost")
	if !strings.Contains(out, "no teammate") {
		t.Errorf("unknown name should report no teammate: %q", out)
	}
}

func TestFormatAgents_KillCallsCancel(t *testing.T) {
	roster := agent.NewRoster(0)
	_, cancel := context.WithCancel(context.Background())
	called := false
	wrapped := func() {
		called = true
		cancel()
	}
	if err := roster.Register(&agent.Teammate{
		Name:    "carol",
		AgentID: "agt-carol",
		Cancel:  wrapped,
	}); err != nil {
		t.Fatal(err)
	}
	out := formatAgentsCommand(roster, "kill carol")
	if !strings.Contains(out, "cancelled") {
		t.Errorf("kill should confirm cancel: %q", out)
	}
	if !called {
		t.Error("kill should invoke the teammate's Cancel func")
	}
}

func TestFormatAgents_UnknownSubcommand(t *testing.T) {
	roster := agent.NewRoster(0)
	out := formatAgentsCommand(roster, "purgify")
	if !strings.Contains(out, "unknown subcommand") {
		t.Errorf("unknown sub should report; got %q", out)
	}
}

func TestTeammate_IsNamed(t *testing.T) {
	t.Parallel()
	named := &agent.Teammate{Name: "alice", Anonymous: false}
	if !named.IsNamed() {
		t.Error("named teammate should report IsNamed=true")
	}
	anon := &agent.Teammate{Name: "_anon-abc", Anonymous: true}
	if anon.IsNamed() {
		t.Error("anonymous teammate should report IsNamed=false")
	}
	var nilT *agent.Teammate
	if nilT.IsNamed() {
		t.Error("nil teammate should be IsNamed=false (nil-safety)")
	}
}
