package permission

import (
	"context"
	"errors"
	"testing"
	"time"
)

// TestSafetyCheck_BypassImmuneOnSensitivePaths verifies that path-aware
// safetyCheck wins over mode=Bypass — even if the user opted into
// "approve everything", writes to .git/config / .ssh/ / ~/.bashrc
// must still ASK.
func TestSafetyCheck_BypassImmuneOnSensitivePaths(t *testing.T) {
	cases := []struct {
		tool string
		path string
		want Decision
	}{
		{"Edit", "/Users/x/proj/normal.go", DecisionAllow},          // bypass passes
		{"Edit", "/Users/x/proj/.git/config", DecisionAsk},          // bypass-immune
		{"Edit", "/Users/x/proj/.git/hooks/pre-commit", DecisionAsk}, // immune
		{"Write", "/Users/x/.ssh/authorized_keys", DecisionAsk},     // immune
		{"Write", "/Users/x/.bashrc", DecisionAsk},                  // immune
		{"Bash", "echo foo > ~/.zshrc", DecisionAsk},                // immune
		{"Bash", "ls /Users/x/proj", DecisionAllow},                 // bypass ok
		{"Read", "/Users/x/.bashrc", DecisionAllow},                 // Read isn't file-touching
	}
	for _, tc := range cases {
		g := New(ModeBypass)
		got, src := g.Check(context.Background(), tc.tool, tc.path)
		if got != tc.want {
			t.Errorf("safetyCheck(%s, %q) = %v (%s), want %v", tc.tool, tc.path, got, src, tc.want)
		}
	}
}

// TestDenialBreaker_FallsBackAfterStreak: 3 consecutive denies via
// rule, the 4th would-be-deny is forced to ASK so a human breaks
// the streak.
func TestDenialBreaker_FallsBackAfterStreak(t *testing.T) {
	g := New(ModeAsk)
	g.AppendRules(Rule{Tool: "Bash", Verb: DecisionDeny, Source: "rule"})

	// First 3 denies count as denies.
	for i := 0; i < 3; i++ {
		got, _ := g.Check(context.Background(), "Bash", "")
		if got != DecisionDeny {
			t.Fatalf("call %d: expected DENY, got %v", i, got)
		}
	}
	consec, total, fb := g.DenialState()
	if consec != 3 || total != 3 || !fb {
		t.Errorf("after 3 denies: consec=%d total=%d fb=%v, want 3/3/true", consec, total, fb)
	}

	// 4th call: breaker downgrades DENY → ASK.
	got, src := g.Check(context.Background(), "Bash", "")
	if got != DecisionAsk {
		t.Errorf("4th call after streak: expected ASK fallback, got %v (%s)", got, src)
	}
}

// TestDenialBreaker_ResetsOnAllow: a successful allow clears the
// consecutive counter (total persists per claude-code spec).
func TestDenialBreaker_ResetsOnAllow(t *testing.T) {
	g := New(ModeAsk)
	g.AppendRules(
		Rule{Tool: "Bash", Verb: DecisionDeny, Source: "deny"},
		Rule{Tool: "Read", Verb: DecisionAllow, Source: "allow"},
	)
	g.Check(context.Background(), "Bash", "")
	g.Check(context.Background(), "Bash", "")
	consec, _, _ := g.DenialState()
	if consec != 2 {
		t.Fatalf("setup: consec=%d, want 2", consec)
	}
	// Successful allow.
	g.Check(context.Background(), "Read", "")
	consec, total, _ := g.DenialState()
	if consec != 0 {
		t.Errorf("after allow: consec=%d, want 0", consec)
	}
	if total != 2 {
		t.Errorf("after allow: total=%d, want 2 (total never resets on allow)", total)
	}
}

// TestDenialBreaker_TotalLimitTrips: even without consecutive streak,
// hitting MaxTotal trips the breaker.
func TestDenialBreaker_TotalLimitTrips(t *testing.T) {
	g := New(ModeAsk)
	g.SetDenialLimits(DenialLimits{MaxConsecutive: 100, MaxTotal: 5})
	g.AppendRules(
		Rule{Tool: "Bash", Verb: DecisionDeny},
		Rule{Tool: "Read", Verb: DecisionAllow},
	)
	for i := 0; i < 5; i++ {
		g.Check(context.Background(), "Bash", "")
		g.Check(context.Background(), "Read", "") // resets consec
	}
	_, total, fb := g.DenialState()
	if total != 5 || !fb {
		t.Errorf("after 5 total denies: total=%d fb=%v, want 5/true", total, fb)
	}
}

// stagedTestClassifier is a programmable test double for the two-stage
// classifier path. fastFn / thinkingFn return whatever the test wants.
type stagedTestClassifier struct {
	fastFn     func(tool, in string) (YoloVerdict, string, error)
	thinkingFn func(tool, in string) (YoloVerdict, string, error)
}

func (s *stagedTestClassifier) Classify(_ context.Context, tool, in string) (YoloVerdict, string, error) {
	return s.fastFn(tool, in)
}
func (s *stagedTestClassifier) ClassifyFast(_ context.Context, tool, in string) (YoloVerdict, string, error) {
	return s.fastFn(tool, in)
}
func (s *stagedTestClassifier) ClassifyThinking(_ context.Context, tool, in string) (YoloVerdict, string, error) {
	return s.thinkingFn(tool, in)
}

// TestStagedClassifier_FastAllowSkipsThinking: stage 1 saying allow
// short-circuits and never calls thinking.
func TestStagedClassifier_FastAllowSkipsThinking(t *testing.T) {
	thinkCalled := false
	c := &stagedTestClassifier{
		fastFn: func(string, string) (YoloVerdict, string, error) {
			return YoloAllow, "fast_allow", nil
		},
		thinkingFn: func(string, string) (YoloVerdict, string, error) {
			thinkCalled = true
			return YoloAllow, "should_not_be_called", nil
		},
	}
	g := New(ModeBypass)
	g.SetClassifier(c)
	got, _ := g.Check(context.Background(), "Bash", "ls")
	if got != DecisionAllow {
		t.Errorf("fast allow should yield ALLOW, got %v", got)
	}
	if thinkCalled {
		t.Error("thinking stage should NOT be called when fast says allow")
	}
}

// TestStagedClassifier_ThinkingReversesFastDeny: a fast deny that
// thinking reverses to allow ends up as ALLOW (false positive saved).
func TestStagedClassifier_ThinkingReversesFastDeny(t *testing.T) {
	c := &stagedTestClassifier{
		fastFn: func(string, string) (YoloVerdict, string, error) {
			return YoloHardDeny, "fast_thinks_bad", nil
		},
		thinkingFn: func(string, string) (YoloVerdict, string, error) {
			return YoloAllow, "thinking_reversed", nil
		},
	}
	g := New(ModeBypass)
	g.SetClassifier(c)
	got, src := g.Check(context.Background(), "Bash", "ls")
	if got != DecisionAllow {
		t.Errorf("thinking-reversal should yield ALLOW, got %v (src=%s)", got, src)
	}
}

// TestStagedClassifier_ThinkingConfirmsFastDeny: fast + thinking both
// deny → final DENY.
func TestStagedClassifier_ThinkingConfirmsFastDeny(t *testing.T) {
	c := &stagedTestClassifier{
		fastFn: func(string, string) (YoloVerdict, string, error) {
			return YoloHardDeny, "fast_deny", nil
		},
		thinkingFn: func(string, string) (YoloVerdict, string, error) {
			return YoloHardDeny, "thinking_confirms", nil
		},
	}
	g := New(ModeBypass)
	g.SetClassifier(c)
	got, _ := g.Check(context.Background(), "Bash", "rm -rf /")
	if got != DecisionDeny {
		t.Errorf("staged confirm: want DENY, got %v", got)
	}
}

// TestClassifierFailClosed_30Min: classifier error opens a 30-min
// fail-closed window. Within that window the gate skips the
// classifier entirely (and bypass falls open with mode:bypass).
func TestClassifierFailClosed_30Min(t *testing.T) {
	calls := 0
	stub := &simpleClassifier{
		fn: func(_, _ string) (YoloVerdict, string, error) {
			calls++
			return 0, "", errors.New("transient outage")
		},
	}
	g := New(ModeBypass)
	g.SetClassifier(stub)

	// 1st call: errors → marks fail-closed, returns ALLOW (bypass
	// fall-open semantics).
	got, _ := g.Check(context.Background(), "Bash", "x")
	if got != DecisionAllow || calls != 1 {
		t.Fatalf("first call: want ALLOW + 1 classifier call, got %v + %d", got, calls)
	}

	// 2nd call: classifier should be skipped because fail-closed
	// window is open.
	got, src := g.Check(context.Background(), "Bash", "y")
	if calls != 1 {
		t.Errorf("classifier should be skipped during fail-closed window; calls=%d", calls)
	}
	if got != DecisionAllow {
		t.Errorf("during fail-closed: want ALLOW (bypass fall-open), got %v (%s)", got, src)
	}

	// Force the window past so we exit fail-closed.
	g.classifierFailUntil = time.Now().Add(-1 * time.Second)
	g.Check(context.Background(), "Bash", "z")
	if calls != 2 {
		t.Errorf("after window expires: classifier should be called again; calls=%d", calls)
	}
}

// simpleClassifier is a single-stage test double for the fail-closed
// test (we don't need the staged path here).
type simpleClassifier struct {
	fn func(tool, in string) (YoloVerdict, string, error)
}

func (s *simpleClassifier) Classify(_ context.Context, tool, in string) (YoloVerdict, string, error) {
	return s.fn(tool, in)
}

// TestAcceptEdits_AllowsLocalWritesNotBash: ModeAcceptEdits should
// auto-allow Edit/Write/NotebookEdit but NOT Bash (shell stays gated).
func TestAcceptEdits_AllowsLocalWritesNotBash(t *testing.T) {
	g := New(ModeAcceptEdits)
	cases := []struct {
		tool string
		want Decision
	}{
		{"Read", DecisionAllow},
		{"Glob", DecisionAllow},
		{"Edit", DecisionAllow},
		{"Write", DecisionAllow},
		{"NotebookEdit", DecisionAllow},
		{"Bash", DecisionAsk}, // shell still gated
	}
	for _, tc := range cases {
		got, _ := g.Check(context.Background(), tc.tool, "/tmp/x.go")
		if got != tc.want {
			t.Errorf("acceptEdits %s: got %v, want %v", tc.tool, got, tc.want)
		}
	}
}

// TestAcceptEdits_StillRespectsSafetyCheck: writing to .ssh/ in
// acceptEdits mode still gets the safetyCheck ASK.
func TestAcceptEdits_StillRespectsSafetyCheck(t *testing.T) {
	g := New(ModeAcceptEdits)
	got, _ := g.Check(context.Background(), "Edit", "/Users/x/.ssh/authorized_keys")
	if got != DecisionAsk {
		t.Errorf("acceptEdits should still ASK for .ssh/, got %v", got)
	}
}

// TestResetDenials_ClearsBreaker: explicit reset (slash command)
// clears all denial state.
func TestResetDenials_ClearsBreaker(t *testing.T) {
	g := New(ModeAsk)
	g.AppendRules(Rule{Tool: "Bash", Verb: DecisionDeny})
	for i := 0; i < 4; i++ {
		g.Check(context.Background(), "Bash", "")
	}
	if _, _, fb := g.DenialState(); !fb {
		t.Fatal("setup: breaker should be tripped")
	}
	g.ResetDenials()
	if c, total, fb := g.DenialState(); c != 0 || total != 0 || fb {
		t.Errorf("after reset: %d/%d/fb=%v, want 0/0/false", c, total, fb)
	}
}
