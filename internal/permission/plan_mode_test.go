package permission

// Plan-mode regression tests — 2026-05-18 fix for the deny-storm
// where every non-Read/LS/Glob/Grep/WebFetch tool got denied in
// plan mode, and the Gate→Loop mode bridge wasn't wired so users
// who flipped Gate.Mode=plan via Shift+Tab couldn't get the loop to
// short-circuit to emitPlan.

import (
	"context"
	"sync"
	"testing"
	"time"
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
// so MetisInfo, Agent, SubAgentOutput, etc., aren't stranded in deny.
// TodoWrite and Fork are mutating wrappers and must remain denied.
func TestPlanMode_ReadOnlyHookConsulted(t *testing.T) {
	g := New(ModePlan)
	readOnly := map[string]bool{
		"AskUser":         true,
		"MetisInfo":       true,
		"TodoRead":        true,
		"Agent":           true,
		"TodoWrite":       false,
		"Fork":            false,
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

func TestPlanMode_SecretReadAsksBeforeReadOnlyAllow(t *testing.T) {
	t.Parallel()
	g := New(ModePlan)
	g.SetReadOnlyHook(func(tool, _ string) bool { return tool == "Read" })

	decision, source := g.Check(context.Background(), "Read", "/Users/x/.ssh/id_ed25519")
	if decision != DecisionAsk {
		t.Fatalf("plan-mode secret Read must ask before readonly allow; got %v (%s)", decision, source)
	}
	if source != "secret_read:bypass_immune" {
		t.Fatalf("plan-mode secret Read source = %q, want secret_read:bypass_immune", source)
	}
}

func TestPlanMode_ReadOnlyBashSafetyPathAsks(t *testing.T) {
	t.Parallel()
	g := New(ModePlan)
	g.SetReadOnlyHook(func(tool, _ string) bool { return tool == "Bash" })

	decision, source := g.Check(context.Background(), "Bash", "cat ~/.ssh/config")
	if decision != DecisionAsk {
		t.Fatalf("plan-mode read-only Bash touching a safety path must ask; got %v (%s)", decision, source)
	}
	if source != "safety_check:bypass_immune" {
		t.Fatalf("plan-mode safety-path source = %q, want safety_check:bypass_immune", source)
	}
}

func TestPlanMode_MutatingSafetyPathStillDenied(t *testing.T) {
	t.Parallel()
	g := New(ModePlan)
	g.SetReadOnlyHook(func(string, string) bool { return false })

	decision, source := g.Check(context.Background(), "Write", "/Users/x/.ssh/config")
	if decision != DecisionDeny || source != "mode:plan" {
		t.Fatalf("plan-mode mutating safety path must remain denied, got %v (%s)", decision, source)
	}
}

func TestDontAsk_SecretAndSafetyChecksDenyWithoutPrompt(t *testing.T) {
	t.Parallel()
	g := New(ModeDontAsk)
	g.SetReadOnlyHook(func(tool, _ string) bool { return tool == "Read" })

	if decision, source := g.Check(context.Background(), "Read", "/Users/x/.ssh/id_rsa"); decision != DecisionDeny || source != "mode:dontAsk:secret_read" {
		t.Fatalf("dontAsk secret Read = %v (%s), want deny mode:dontAsk:secret_read", decision, source)
	}
	if decision, source := g.Check(context.Background(), "Write", "/Users/x/.ssh/config"); decision != DecisionDeny || source != "mode:dontAsk:safety_check" {
		t.Fatalf("dontAsk safety Write = %v (%s), want deny mode:dontAsk:safety_check", decision, source)
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

// A slow older callback must not land after a newer mode update. Before the
// ordered notifier, concurrent SetMode calls invoked captured listeners on
// their caller goroutines and could leave Gate=default while Loop still saw
// plan.
func TestSetMode_ConcurrentCallbacksFinishAtLatestMode(t *testing.T) {
	g := New(ModeDefault)
	enteredPlan := make(chan struct{})
	releasePlan := make(chan struct{})
	var observedMu sync.Mutex
	observed := ModeDefault
	g.SetModeChangeListener(func(mode Mode) {
		if mode == ModePlan {
			close(enteredPlan)
			<-releasePlan
		}
		observedMu.Lock()
		observed = mode
		observedMu.Unlock()
	})

	planDone := make(chan struct{})
	go func() {
		g.SetMode(ModePlan)
		close(planDone)
	}()
	select {
	case <-enteredPlan:
	case <-time.After(2 * time.Second):
		t.Fatal("plan listener did not start")
	}

	defaultDone := make(chan struct{})
	go func() {
		g.SetMode(ModeDefault)
		close(defaultDone)
	}()
	select {
	case <-defaultDone:
	case <-time.After(2 * time.Second):
		t.Fatal("newer SetMode blocked behind the older listener")
	}
	close(releasePlan)
	select {
	case <-planDone:
	case <-time.After(2 * time.Second):
		t.Fatal("ordered listener drain did not finish")
	}

	observedMu.Lock()
	last := observed
	observedMu.Unlock()
	if got := g.Mode(); got != ModeDefault || last != got {
		t.Fatalf("mode/listener diverged: gate=%q listener=%q", got, last)
	}
}

// A committed mode is not safe to use until its listener has reconciled the
// runtime posture (sandbox, plan overlay, and related lifecycle state). While
// that callback drain is in flight, both ordinary and path-aware checks must
// fail closed instead of authorizing against a half-applied mode.
func TestResetSessionState_DeniesChecksUntilListenerDrain(t *testing.T) {
	g := New(ModeFullAccess)
	entered := make(chan struct{})
	release := make(chan struct{})
	g.SetModeChangeListener(func(mode Mode) {
		if mode == ModeBypassPermissions {
			close(entered)
			<-release
		}
	})

	done := make(chan struct{})
	go func() {
		g.ResetSessionState(ModeBypassPermissions, nil)
		close(done)
	}()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("reset listener did not start")
	}

	if decision, source := g.Check(context.Background(), "Bash", "echo ok"); decision != DecisionDeny || source != "mode:transition" {
		t.Fatalf("Check during reset = %v (%s), want deny mode:transition", decision, source)
	}
	if decision, source := g.CheckPath(context.Background(), "Read", "/tmp/value", "/tmp/value"); decision != DecisionDeny || source != "mode:transition" {
		t.Fatalf("CheckPath during reset = %v (%s), want deny mode:transition", decision, source)
	}

	close(release)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("reset did not finish after listener release")
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
