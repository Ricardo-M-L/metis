package permission

import (
	"context"
	"testing"
)

func TestGate_DefaultModeAsks(t *testing.T) {
	g := New(ModeAsk)
	d, _ := g.Check(context.Background(), "Bash", "rm -rf /tmp/foo")
	if d != DecisionAsk {
		t.Errorf("default ask mode → want DecisionAsk, got %v", d)
	}
}

func TestGate_BypassAllowsEverything(t *testing.T) {
	g := New(ModeBypass)
	d, _ := g.Check(context.Background(), "Bash", "anything")
	if d != DecisionAllow {
		t.Errorf("bypass → want allow, got %v", d)
	}
}

func TestGate_DenyDeniesEverything(t *testing.T) {
	g := New(ModeDeny)
	d, _ := g.Check(context.Background(), "Read", "/etc/passwd")
	if d != DecisionDeny {
		t.Errorf("deny → want deny, got %v", d)
	}
}

func TestGate_PlanAllowsReadOnly(t *testing.T) {
	g := New(ModePlan)
	for _, tool := range []string{"Read", "LS", "Glob", "Grep", "WebFetch"} {
		if d, _ := g.Check(context.Background(), tool, ""); d != DecisionAllow {
			t.Errorf("plan should allow %s, got %v", tool, d)
		}
	}
	for _, tool := range []string{"Bash", "Write", "Edit"} {
		if d, _ := g.Check(context.Background(), tool, ""); d != DecisionDeny {
			t.Errorf("plan should deny %s, got %v", tool, d)
		}
	}
}

func TestGate_AutoAllowsReadOnly(t *testing.T) {
	g := New(ModeAuto)
	if d, _ := g.Check(context.Background(), "Read", ""); d != DecisionAllow {
		t.Errorf("auto should auto-allow Read, got %v", d)
	}
	if d, _ := g.Check(context.Background(), "Bash", ""); d != DecisionAsk {
		t.Errorf("auto should still ask for Bash, got %v", d)
	}
}

// Regression: ModeAuto and ModePlan must agree on the read-only allowlist.
// Previously WebFetch was allowed in plan but missing from auto.
func TestGate_AutoAllowsWebFetchSameAsPlan(t *testing.T) {
	for _, mode := range []Mode{ModeAuto, ModePlan} {
		g := New(mode)
		if d, _ := g.Check(context.Background(), "WebFetch", "https://example.com"); d != DecisionAllow {
			t.Errorf("%s should allow WebFetch (got %v)", mode, d)
		}
	}
}

func TestGate_RuleStackLastWins(t *testing.T) {
	g := New(ModeAsk)
	g.AppendRules(
		Rule{Tool: "Bash", Verb: DecisionDeny, Source: "early"},
		Rule{Tool: "Bash", Match: "ls", Verb: DecisionAllow, Source: "late"},
	)
	d, src := g.Check(context.Background(), "Bash", "ls -la")
	if d != DecisionAllow {
		t.Errorf("late matching rule should win → want allow, got %v from %q", d, src)
	}
	d, _ = g.Check(context.Background(), "Bash", "rm -rf /")
	if d != DecisionDeny {
		t.Errorf("non-matching late rule → fall through to early deny, got %v", d)
	}
}

func TestGate_WildcardTool(t *testing.T) {
	g := New(ModeAsk)
	g.AppendRules(Rule{Tool: "*", Verb: DecisionAllow, Source: "wild"})
	if d, _ := g.Check(context.Background(), "AnythingAtAll", ""); d != DecisionAllow {
		t.Errorf("wildcard should match any tool")
	}
}

func TestGate_SnapshotReturnsRulesCopy(t *testing.T) {
	g := New(ModeAsk)
	g.AppendRules(
		Rule{Tool: "Bash", Match: "git", Verb: DecisionAllow, Source: "user"},
		Rule{Tool: "Read", Verb: DecisionAllow, Source: "config"},
	)
	snap := g.Snapshot()
	if len(snap) != 2 {
		t.Fatalf("snapshot len = %d, want 2", len(snap))
	}
	// Mutating the snapshot must not affect the gate.
	snap[0].Tool = "MUTATED"
	again := g.Snapshot()
	if again[0].Tool != "Bash" {
		t.Errorf("Snapshot returned a live reference, not a copy")
	}
}

func TestGate_RememberAcrossCalls(t *testing.T) {
	g := New(ModeAsk)
	if g.Remembered("Bash", "git status") {
		t.Fatal("should not remember before first call")
	}
	g.Remember("Bash", "git status")
	if !g.Remembered("Bash", "git status") {
		t.Errorf("should remember after Remember()")
	}
}

// TestGate_PopRulesUndoesAppend covers the cron scheduler's per-job
// tool blacklist installation: AppendRules + run + PopRules(N) must
// leave the gate in exactly its pre-Append state.
func TestGate_PopRulesUndoesAppend(t *testing.T) {
	g := New(ModeAsk)
	g.AppendRules(Rule{Tool: "Read", Verb: DecisionAllow, Source: "config"})
	baseLen := len(g.Snapshot())

	g.AppendRules(
		Rule{Tool: "WebFetch", Verb: DecisionDeny, Source: "cron"},
		Rule{Tool: "Agent", Verb: DecisionDeny, Source: "cron"},
	)
	if len(g.Snapshot()) != baseLen+2 {
		t.Fatalf("after Append: len = %d, want %d", len(g.Snapshot()), baseLen+2)
	}

	g.PopRules(2)
	after := g.Snapshot()
	if len(after) != baseLen {
		t.Fatalf("after Pop: len = %d, want %d", len(after), baseLen)
	}
	if after[0].Tool != "Read" {
		t.Errorf("Pop didn't preserve baseline rule; got %v", after[0])
	}
}

// TestGate_PopRulesOversizeIsSafe ensures we don't panic when caller
// asks to pop more rules than exist (defensive — the cron daemon's
// defer block can run after a rule was hand-removed elsewhere).
func TestGate_PopRulesOversizeIsSafe(t *testing.T) {
	g := New(ModeAsk)
	g.AppendRules(Rule{Tool: "Read", Verb: DecisionAllow})
	g.PopRules(99)
	if len(g.Snapshot()) != 0 {
		t.Errorf("oversized Pop should clear the stack; got %d rules", len(g.Snapshot()))
	}
}
