package permission

import (
	"context"
	"testing"
)

var scopedFileTools = []struct {
	name     string
	readOnly bool
}{
	{name: "Read", readOnly: true},
	{name: "LS", readOnly: true},
	{name: "Glob", readOnly: true},
	{name: "Grep", readOnly: true},
	{name: "ViewImage", readOnly: true},
	{name: "Edit"},
	{name: "Write"},
	{name: "NotebookEdit"},
}

func newScopedGate(mode Mode) *Gate {
	g := New(mode)
	g.SetPathScopeHook(func(path string) bool { return path == "/scope/file" })
	g.SetReadOnlyHook(func(tool, _ string) bool {
		for _, candidate := range scopedFileTools {
			if candidate.name == tool {
				return candidate.readOnly
			}
		}
		return false
	})
	return g
}

func TestGate_PathScopeFiveModeMatrix(t *testing.T) {
	t.Parallel()
	tests := []struct {
		mode Mode
		want func(readOnly bool) Decision
	}{
		{mode: ModeDefault, want: func(bool) Decision { return DecisionAsk }},
		{mode: ModeAcceptEdits, want: func(bool) Decision { return DecisionAsk }},
		{mode: ModeDontAsk, want: func(bool) Decision { return DecisionDeny }},
		{mode: ModeBypassPermissions, want: func(bool) Decision { return DecisionAllow }},
		{mode: ModePlan, want: func(readOnly bool) Decision {
			if readOnly {
				return DecisionAsk
			}
			return DecisionDeny
		}},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(string(tc.mode), func(t *testing.T) {
			t.Parallel()
			for _, tool := range scopedFileTools {
				tool := tool
				t.Run(tool.name, func(t *testing.T) {
					g := newScopedGate(tc.mode)
					got, source := g.CheckPath(context.Background(), tool.name, "/outside/file", "/outside/file")
					if want := tc.want(tool.readOnly); got != want {
						t.Fatalf("outside scope = %v (%s), want %v", got, source, want)
					}
					if got == DecisionAsk && source != "scope:outside" {
						t.Fatalf("outside ASK source = %q, want scope:outside", source)
					}
					if tc.mode == ModeDontAsk && source != "mode:dontAsk:scope" {
						t.Fatalf("dontAsk source = %q, want mode:dontAsk:scope", source)
					}
				})
			}
		})
	}
}

func TestGate_PathScopeExplicitAllowOverridesScopeNotPlanWrites(t *testing.T) {
	t.Parallel()
	for _, mode := range Modes {
		mode := mode
		t.Run(string(mode), func(t *testing.T) {
			t.Parallel()
			g := newScopedGate(mode)
			g.AppendRules(Rule{
				Tool: "Read", Match: "/outside/file",
				Verb: DecisionAllow, Source: "interactive",
			})
			got, source := g.CheckPath(context.Background(), "Read", "/outside/file", "/outside/file")
			if got != DecisionAllow {
				t.Fatalf("matching explicit allow = %v (%s), want allow", got, source)
			}
		})
	}

	g := newScopedGate(ModePlan)
	g.AppendRules(Rule{Tool: "Write", Match: "/outside/file", Verb: DecisionAllow, Source: "interactive"})
	if got, source := g.CheckPath(context.Background(), "Write", "/outside/file", "/outside/file"); got != DecisionDeny || source != "mode:plan" {
		t.Fatalf("plan write with scope allow = %v (%s), want hard plan deny", got, source)
	}
}

func TestGate_PathScopeMatchingHigherAuthorityDenyStillWins(t *testing.T) {
	t.Parallel()
	g := newScopedGate(ModeDefault)
	g.AppendRules(Rule{Tool: "Read", Match: "/outside/file", Verb: DecisionAllow, Source: "interactive"})
	g.AppendRules(Rule{Tool: "Read", Match: "/outside/file", Verb: DecisionDeny, Source: "policy"})
	if got, source := g.CheckPath(context.Background(), "Read", "/outside/file", "/outside/file"); got != DecisionDeny || source != "policy" {
		t.Fatalf("policy deny = %v (%s), want deny from policy", got, source)
	}
}

func TestGate_PathScopeOnlyAppliesToCheckPathAndClones(t *testing.T) {
	t.Parallel()
	g := newScopedGate(ModeDefault)
	if got, source := g.Check(context.Background(), "Read", "/outside/file"); got != DecisionAllow {
		t.Fatalf("plain Check unexpectedly enforced path scope: %v (%s)", got, source)
	}
	if got, source := g.CheckPath(context.Background(), "Read", "/scope/file", "/scope/file"); got != DecisionAllow {
		t.Fatalf("inside path = %v (%s), want allow", got, source)
	}

	clone := g.Clone()
	if got, source := clone.CheckPath(context.Background(), "Read", "/outside/file", "/outside/file"); got != DecisionAsk || source != "scope:outside" {
		t.Fatalf("clone lost scope hook: %v (%s)", got, source)
	}
}
