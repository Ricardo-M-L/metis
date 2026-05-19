package permission

// Plan-mode regression tests — 2026-05-18 fix for the deny-storm
// where every non-Read/LS/Glob/Grep/WebFetch tool got denied in
// plan mode, and the Gate→Loop mode bridge wasn't wired so users
// who flipped Gate.Mode=plan via Shift+Tab couldn't get the loop to
// short-circuit to emitPlan.

import (
	"context"
	"testing"
)

// TestPlanMode_MetaToolsAlwaysPass — EnterPlanMode/ExitPlanMode are
// the user's only way out of plan mode; denying them would be a trap.
func TestPlanMode_MetaToolsAlwaysPass(t *testing.T) {
	g := New(ModePlan)
	cases := []string{"EnterPlanMode", "ExitPlanMode"}
	for _, tool := range cases {
		d, reason := g.Check(context.Background(), tool, "")
		if d != DecisionAllow {
			t.Errorf("plan-mode %s should ALLOW (meta tool); got %v (%s)", tool, d, reason)
		}
	}
}

// TestPlanMode_ReadOnlyHookConsulted — when the runtime wires a hook
// (it does in main.go: tools.IsReadOnly), plan mode must respect it
// so AskUser, MetisInfo, TodoWrite, SubAgentOutput, etc., aren't
// stranded in deny.
func TestPlanMode_ReadOnlyHookConsulted(t *testing.T) {
	g := New(ModePlan)
	readOnly := map[string]bool{
		"AskUser":         true,
		"MetisInfo":       true,
		"TodoRead":        true,
		"TodoWrite":       true,
		"SubAgentOutput":  true,
		"BashOutput":      true,
		"Skill":           true,
		"NotAReadOnlyOne": false,
	}
	g.SetReadOnlyHook(func(tool, _ string) bool {
		return readOnly[tool]
	})

	for tool, isRO := range readOnly {
		d, reason := g.Check(context.Background(), tool, "")
		if isRO && d != DecisionAllow {
			t.Errorf("plan-mode %s (read-only) should ALLOW; got %v (%s)", tool, d, reason)
		}
		if !isRO && d != DecisionDeny {
			t.Errorf("plan-mode %s (NOT read-only) should DENY; got %v (%s)", tool, d, reason)
		}
	}
}

// TestPlanMode_LegacyAllowlistStillWorks — headless / test paths that
// don't wire SetReadOnlyHook still need basic read tools (Read, LS,
// Glob, Grep, WebFetch) to pass in plan mode.
func TestPlanMode_LegacyAllowlistStillWorks(t *testing.T) {
	g := New(ModePlan) // no readOnlyHook
	for _, tool := range []string{"Read", "LS", "Glob", "Grep", "WebFetch"} {
		d, reason := g.Check(context.Background(), tool, "")
		if d != DecisionAllow {
			t.Errorf("plan-mode %s should ALLOW via fallback list; got %v (%s)", tool, d, reason)
		}
	}
}

// TestPlanMode_WriteToolsStillDeny — the whole point of plan mode is
// blocking side-effect tools. Make sure the relaxation didn't open
// the floodgates.
func TestPlanMode_WriteToolsStillDeny(t *testing.T) {
	g := New(ModePlan)
	g.SetReadOnlyHook(func(tool, _ string) bool { return false }) // pessimistic
	for _, tool := range []string{"Write", "Edit", "Bash", "NotebookEdit"} {
		d, reason := g.Check(context.Background(), tool, "")
		if d != DecisionDeny {
			t.Errorf("plan-mode %s should still DENY (side effects); got %v (%s)", tool, d, reason)
		}
	}
}

// TestSetMode_FiresListener — the Gate→Loop bridge depends on the
// listener firing on every SetMode call. Lost callback = lost sync.
func TestSetMode_FiresListener(t *testing.T) {
	g := New(ModeAsk)
	var got []Mode
	g.SetModeChangeListener(func(m Mode) {
		got = append(got, m)
	})

	g.SetMode(ModePlan)
	g.SetMode(ModeBypass)
	g.SetMode(ModeAsk)

	want := []Mode{ModePlan, ModeBypass, ModeAsk}
	if len(got) != len(want) {
		t.Fatalf("listener fired %d times, want %d (modes seen: %v)", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("listener call %d: got %q, want %q", i, got[i], want[i])
		}
	}
}

// TestSetMode_ListenerNoDeadlock — listener fires AFTER releasing the
// lock so listeners that call back into Gate (e.g., to read current
// state) don't deadlock. Regression for the "listener-induced
// deadlock" risk noted in the SetMode comment.
func TestSetMode_ListenerNoDeadlock(t *testing.T) {
	g := New(ModeAsk)
	done := make(chan struct{})
	g.SetModeChangeListener(func(m Mode) {
		// Re-entrant Gate access — would deadlock if SetMode held the
		// lock during the callback.
		_ = g.Mode()
		close(done)
	})

	g.SetMode(ModePlan)

	select {
	case <-done:
	default:
		t.Fatal("listener never fired (or deadlocked) — Gate.Mode() inside listener should be safe")
	}
}

// TestSetMode_ClearListener — passing nil to SetModeChangeListener
// must clear the previous one (otherwise listeners leak across
// sessions / tests).
func TestSetMode_ClearListener(t *testing.T) {
	g := New(ModeAsk)
	var fires int
	g.SetModeChangeListener(func(Mode) { fires++ })
	g.SetMode(ModePlan)
	if fires != 1 {
		t.Fatalf("listener should have fired once, got %d", fires)
	}
	g.SetModeChangeListener(nil)
	g.SetMode(ModeAsk)
	if fires != 1 {
		t.Errorf("after clearing listener, fires count should not change; got %d", fires)
	}
}
