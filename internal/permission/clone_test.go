package permission

// clone_test.go — locks Phase G.9 (2026-05-12) Gate.Clone() contract:
//
//   1. Cloned gate carries the same Mode + Rules as the parent.
//   2. SetMode on the clone doesn't affect the parent (and vice-
//      versa) — that's the whole point of sub-agent isolation.
//   3. AppendRules on the clone doesn't bleed back to the parent.
//   4. The clone starts with a FRESH memoAllow / denial counter so
//      a sub-agent's "ask once" doesn't pollute the parent's memo
//      and the parent's denial streak doesn't follow the sub-agent.
//   5. nil receiver returns nil — defensive nil-safety because the
//      Agent tool's gate may be nil in headless unit tests.

import (
	"context"
	"testing"
)

func TestGate_Clone_CopiesModeAndRules(t *testing.T) {
	t.Parallel()
	parent := New(ModeAsk)
	parent.AppendRules(Rule{Tool: "Bash", Match: "ls -la", Verb: DecisionAllow, Source: "test"})

	clone := parent.Clone()
	if clone == nil {
		t.Fatal("Clone returned nil")
	}
	if clone.Mode() != ModeAsk {
		t.Errorf("clone Mode = %q, want %q", clone.Mode(), ModeAsk)
	}
	// Indirectly verify rules carried over by Check'ing one.
	dec, _ := clone.Check(context.Background(), "Bash", "ls -la")
	if dec != DecisionAllow {
		t.Errorf("clone should allow rule-matched call; got %v", dec)
	}
}

func TestGate_Clone_ModeIsolation(t *testing.T) {
	t.Parallel()
	parent := New(ModeAsk)
	clone := parent.Clone()

	clone.SetMode(ModeBypass)

	if parent.Mode() != ModeAsk {
		t.Errorf("clone.SetMode leaked to parent; parent.Mode = %q", parent.Mode())
	}
	if clone.Mode() != ModeBypass {
		t.Errorf("clone.SetMode didn't stick; clone.Mode = %q", clone.Mode())
	}
}

func TestGate_Clone_RulesAreIndependent(t *testing.T) {
	t.Parallel()
	parent := New(ModeAsk)
	parent.AppendRules(Rule{Tool: "Bash", Match: "ls -la", Verb: DecisionAllow})

	clone := parent.Clone()
	// Tool="*" + Match="" matches any Edit call (substring of empty is
	// always true; tool wildcard matches by name).
	clone.AppendRules(Rule{Tool: "Edit", Verb: DecisionAllow})

	// Parent should NOT see the Edit rule.
	dec, _ := parent.Check(context.Background(), "Edit", "anything")
	if dec == DecisionAllow {
		t.Errorf("parent should NOT have the Edit rule the clone added; got Allow")
	}
	// Clone SHOULD see the Edit rule.
	dec, _ = clone.Check(context.Background(), "Edit", "anything")
	if dec != DecisionAllow {
		t.Errorf("clone should have its own Edit rule; got %v", dec)
	}
}

func TestGate_Clone_DenialCountersFresh(t *testing.T) {
	t.Parallel()
	parent := New(ModeAsk)
	// Plant denial state on the parent. Tool match without Match
	// substring acts as "any Bash call".
	parent.AppendRules(Rule{Tool: "Bash", Verb: DecisionDeny})
	for i := 0; i < 3; i++ {
		_, _ = parent.Check(context.Background(), "Bash", "anything")
	}
	pCons, pTotal, _ := parent.DenialState()
	if pCons == 0 && pTotal == 0 {
		t.Fatalf("test prerequisite: parent should have denial counters > 0")
	}
	clone := parent.Clone()
	cCons, cTotal, _ := clone.DenialState()
	if cCons != 0 || cTotal != 0 {
		t.Errorf("clone denial counters should reset; got cons=%d total=%d", cCons, cTotal)
	}
}

func TestGate_Clone_NilSafe(t *testing.T) {
	t.Parallel()
	var g *Gate
	if got := g.Clone(); got != nil {
		t.Errorf("nil.Clone() should return nil; got %v", got)
	}
}
