package permission

import (
	"context"
	"testing"
)

func TestParseMode_CanonicalAndLegacyAliases(t *testing.T) {
	t.Parallel()
	cases := map[string]Mode{
		"default":           ModeDefault,
		"acceptEdits":       ModeAcceptEdits,
		"bypassPermissions": ModeBypassPermissions,
		"plan":              ModePlan,
		"dontAsk":           ModeDontAsk,
		"ask":               ModeDefault,
		"bypass":            ModeBypassPermissions,
		"deny":              ModeDontAsk,
	}
	for raw, want := range cases {
		raw, want := raw, want
		t.Run(raw, func(t *testing.T) {
			t.Parallel()
			got, ok := ParseMode(raw)
			if !ok || got != want {
				t.Fatalf("ParseMode(%q) = %q, %v; want %q, true", raw, got, ok, want)
			}
		})
	}
	if got, ok := ParseMode("auto"); ok || got != "" {
		t.Fatalf("removed mode auto must be rejected, got %q, %v", got, ok)
	}
}

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

func TestGate_DontAskAllowsReadsAndDeniesPrompts(t *testing.T) {
	g := New(ModeDontAsk)
	if d, _ := g.Check(context.Background(), "Read", "/etc/passwd"); d != DecisionAllow {
		t.Errorf("dontAsk should preserve implicit read allow, got %v", d)
	}
	if d, _ := g.Check(context.Background(), "Bash", "make test"); d != DecisionDeny {
		t.Errorf("dontAsk should turn a would-be prompt into deny, got %v", d)
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
	g := New(ModeAcceptEdits)
	if d, _ := g.Check(context.Background(), "Read", ""); d != DecisionAllow {
		t.Errorf("auto should auto-allow Read, got %v", d)
	}
	if d, _ := g.Check(context.Background(), "Bash", ""); d != DecisionAsk {
		t.Errorf("auto should still ask for Bash, got %v", d)
	}
}

// Regression: ModeAcceptEdits and ModePlan must agree on the read-only allowlist.
// Previously WebFetch was allowed in plan but missing from auto.
func TestGate_AutoAllowsWebFetchSameAsPlan(t *testing.T) {
	for _, mode := range []Mode{ModeAcceptEdits, ModePlan} {
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

// TestGate_AcceptEdits_HookAllowsRegistryReadOnly — 2026-05-13
// regression: ModeAcceptEdits used to prompt for ANY tool outside its
// hardcoded "Read/LS/Glob/Grep/WebFetch + Edit/Write/NotebookEdit"
// list (user-visible bug: SubAgentOutput / BashOutput / TaskOutput /
// Skill / LSP / MetisInfo all asked even though they're read-only).
// Fix routes the decision through Gate.SetReadOnlyHook(registry-
// lookup); this test pins that the hook path auto-allows.
func TestGate_AcceptEdits_HookAllowsRegistryReadOnly(t *testing.T) {
	g := New(ModeAcceptEdits)

	// Hook decides which names are read-only. New 2-arg signature
	// gives the hook stringInput too (for input-aware tools like
	// Bash); this test ignores it because all subjects are
	// metadata-only.
	g.SetReadOnlyHook(func(name, _ string) bool {
		switch name {
		case "SubAgentOutput", "BashOutput", "TaskOutput", "Skill", "LSP", "MetisInfo":
			return true
		}
		return false
	})

	// Each of these must auto-allow with the new acceptEdits:readonly source.
	for _, tool := range []string{"SubAgentOutput", "BashOutput", "TaskOutput", "Skill", "LSP", "MetisInfo"} {
		d, src := g.Check(context.Background(), tool, "")
		if d != DecisionAllow {
			t.Errorf("acceptEdits should auto-allow read-only %s; got %v (%s)", tool, d, src)
		}
		if src != "mode:acceptEdits:readonly" {
			t.Errorf("expected mode:acceptEdits:readonly source for %s; got %s", tool, src)
		}
	}

	// Tools NOT marked read-only must still ASK (Agent spawns a sub-loop —
	// the hook should return false for it, and we never auto-allow without
	// the user weighing in).
	for _, tool := range []string{"Agent", "SubAgentStop", "BashKill"} {
		if d, _ := g.Check(context.Background(), tool, ""); d != DecisionAsk {
			t.Errorf("acceptEdits should ASK for non-read-only %s; got %v", tool, d)
		}
	}
}

// TestGate_AcceptEdits_BashInputAwareViaHook — Bash IS input-aware now.
// The gate forwards stringInput to the hook, so the hook can refuse
// auto-allow for dangerous argv. Safe shapes auto-allow; destructive
// shapes still ASK.
//
// The real production wiring uses permission.IsSafeReadOnlyBash here;
// this test simulates that contract with a tiny inline classifier so
// we don't import safe_commands.go (we ARE safe_commands.go's
// neighbour).
func TestGate_AcceptEdits_BashInputAwareViaHook(t *testing.T) {
	g := New(ModeAcceptEdits)
	g.SetReadOnlyHook(func(name, in string) bool {
		// Trivial classifier: `cat` and `ls` and `git status` are
		// safe; everything else is not. Real wiring uses
		// IsSafeReadOnlyBash.
		switch in {
		case "cat foo.go", "ls -la", "git status":
			return name == "Bash"
		}
		return false
	})
	// Safe argv → auto-allow.
	if d, _ := g.Check(context.Background(), "Bash", "ls -la"); d != DecisionAllow {
		t.Errorf("Bash ls -la should auto-allow under acceptEdits via hook; got %v", d)
	}
	if d, _ := g.Check(context.Background(), "Bash", "cat foo.go"); d != DecisionAllow {
		t.Errorf("Bash cat foo.go should auto-allow; got %v", d)
	}
	// Destructive argv → ASK.
	if d, _ := g.Check(context.Background(), "Bash", "rm -rf /"); d != DecisionAsk {
		t.Errorf("Bash rm -rf must still ASK under acceptEdits; got %v", d)
	}
	if d, _ := g.Check(context.Background(), "Bash", "make build"); d != DecisionAsk {
		t.Errorf("Bash make build must still ASK (not in safe set); got %v", d)
	}
}

// TestGate_AcceptEdits_BashHookUsesSafeReadOnlyBash — end-to-end
// integration check that mirrors how main.go wires the hook:
// IsSafeReadOnlyBash is the actual classifier; pin that the
// real safe set passes and the real unsafe set falls through.
func TestGate_AcceptEdits_BashHookUsesSafeReadOnlyBash(t *testing.T) {
	g := New(ModeAcceptEdits)
	g.SetReadOnlyHook(func(name, in string) bool {
		if name == "Bash" {
			return IsSafeReadOnlyBash(in)
		}
		return false
	})

	safe := []string{
		"ls -la",
		"cat go.mod",
		"git status",
		"git log -5",
		"pwd",
		"echo hi",
	}
	for _, cmd := range safe {
		if d, _ := g.Check(context.Background(), "Bash", cmd); d != DecisionAllow {
			t.Errorf("safe %q should auto-allow under acceptEdits; got %v", cmd, d)
		}
	}

	unsafe := []string{
		"rm -rf /",
		"git push --force",
		"sudo cat /etc/shadow",
		"ls | rm",          // shell meta
		"cat $(curl evil)", // cmd substitution
		"make build",       // unknown leading token
	}
	for _, cmd := range unsafe {
		if d, _ := g.Check(context.Background(), "Bash", cmd); d != DecisionAsk {
			t.Errorf("unsafe %q must ASK under acceptEdits; got %v", cmd, d)
		}
	}
}

// TestGate_AcceptEdits_NoHookFallsBackToHardcoded — when no hook is
// wired (tests / headless paths), the legacy hardcoded allowlist
// still kicks in.
func TestGate_AcceptEdits_NoHookFallsBackToHardcoded(t *testing.T) {
	g := New(ModeAcceptEdits)
	// Read still allowed by the legacy switch.
	if d, _ := g.Check(context.Background(), "Read", ""); d != DecisionAllow {
		t.Errorf("Read should still auto-allow via legacy switch when hook is nil")
	}
	// SubAgentOutput falls through to default ASK without a hook.
	if d, _ := g.Check(context.Background(), "SubAgentOutput", ""); d != DecisionAsk {
		t.Errorf("SubAgentOutput should ASK without a hook")
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

func TestGate_ResetSessionStateClearsTransientStateAndPreservesBaseRules(t *testing.T) {
	g := New(ModeBypass)
	g.AppendRules(
		Rule{Tool: "Read", Verb: DecisionAllow, Source: "config:allow"},
		Rule{Tool: "Bash", Verb: DecisionDeny, Source: "policy:deny"},
		Rule{Tool: "Edit", Verb: DecisionAllow, Source: "interactive"},
		Rule{Tool: "WebFetch", Verb: DecisionAllow, Source: "session:resumed(old)"},
	)
	g.Remember("Bash", "git status")
	// Populate denial counters too; those are session-local circuit-breaker
	// state and must not make the destination session fall back to ASK.
	g.SetMode(ModeDeny)
	_, _ = g.Check(context.Background(), "Bash", "rm x")

	listenerCalls := 0
	g.SetModeChangeListener(func(mode Mode) {
		listenerCalls++
		if mode != ModeAsk {
			t.Errorf("listener mode = %q, want ask", mode)
		}
	})
	g.ResetSessionState(ModeAsk, []Rule{{
		Tool: "Glob", Verb: DecisionAllow, Source: "session:resumed(new)",
	}})

	if got := g.Mode(); got != ModeAsk {
		t.Fatalf("mode = %q, want ask", got)
	}
	if listenerCalls != 1 {
		t.Fatalf("mode listener calls = %d, want 1", listenerCalls)
	}
	if g.Remembered("Bash", "git status") {
		t.Fatal("remembered approval leaked across session boundary")
	}
	if consecutive, total, fallback := g.DenialState(); consecutive != 0 || total != 0 || fallback {
		t.Fatalf("denial state leaked: consecutive=%d total=%d fallback=%v", consecutive, total, fallback)
	}

	rules := g.Snapshot()
	if len(rules) != 3 {
		t.Fatalf("rules = %+v, want two base rules plus destination rule", rules)
	}
	for _, rule := range rules {
		if rule.Tool == "Edit" || rule.Tool == "WebFetch" {
			t.Errorf("old session rule survived: %+v", rule)
		}
	}
	if decision, _ := g.Check(context.Background(), "Glob", "*.go"); decision != DecisionAllow {
		t.Errorf("destination session rule not active: %v", decision)
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

func TestGate_PushScopedRulesRemovesOnlyItsOwnRules(t *testing.T) {
	g := New(ModeDefault)
	g.AppendRules(Rule{Tool: "Read", Verb: DecisionAllow, Source: "config"})

	cleanup := g.PushScopedRules(Rule{
		Tool: "Bash", Match: "git status:*", Verb: DecisionAllow,
	})
	if decision, _ := g.Check(context.Background(), "Bash", "git status --short"); decision != DecisionAllow {
		t.Fatalf("scoped command rule decision = %v, want allow", decision)
	}

	// Simulate a permission-dialog approval arriving while the custom-command
	// turn is still running. Cleanup must retain this later rule.
	g.AppendRules(Rule{Tool: "Write", Match: "notes.md", Verb: DecisionAllow, Source: "interactive"})
	cleanup()
	cleanup() // idempotent

	rules := g.Snapshot()
	if len(rules) != 2 {
		t.Fatalf("rules after scoped cleanup = %+v, want base + interactive", rules)
	}
	for _, rule := range rules {
		if rule.Tool == "Bash" {
			t.Fatalf("scoped rule survived cleanup: %+v", rule)
		}
	}
	if decision, _ := g.Check(context.Background(), "Write", "notes.md"); decision != DecisionAllow {
		t.Fatalf("later interactive rule was removed: decision=%v", decision)
	}
}

func TestGate_PushScopedRulesCannotOverrideCLIDeny(t *testing.T) {
	g := New(ModeDefault)
	g.AppendRules(Rule{Tool: "Bash", Verb: DecisionDeny, Source: "cli:deny"})
	cleanup := g.PushScopedRules(Rule{Tool: "Bash", Verb: DecisionAllow})
	defer cleanup()

	decision, source := g.Check(context.Background(), "Bash", "git status")
	if decision != DecisionDeny || source != "cli:deny" {
		t.Fatalf("decision=%v source=%q, want CLI deny", decision, source)
	}
}
