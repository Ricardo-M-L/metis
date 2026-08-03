package permission

import (
	"context"
	"errors"
	"testing"
)

// stubClassifier returns a fixed verdict per (tool, input) lookup. Lets
// tests pin specific cases without involving a real LLM.
type stubClassifier struct {
	verdict YoloVerdict
	reason  string
	err     error
}

func (s *stubClassifier) Classify(_ context.Context, _, _ string) (YoloVerdict, string, error) {
	return s.verdict, s.reason, s.err
}

// TestYolo_AllowPassesThrough — when classifier says allow, bypass mode
// behaves identically to no-classifier bypass: DecisionAllow.
func TestYolo_AllowPassesThrough(t *testing.T) {
	g := New(ModeBypass)
	g.SetClassifier(&stubClassifier{verdict: YoloAllow, reason: "looks fine"})
	d, src := g.Check(context.Background(), "Bash", "ls /tmp")
	if d != DecisionAllow {
		t.Errorf("YoloAllow → DecisionAllow; got %v src=%q", d, src)
	}
}

// TestYolo_SoftDenyAllowsInBypass — 2026-07-26 behaviour change.
// claude-code's auto-mode classifier has only two terminal verdicts
// (shouldBlock:true → deny, shouldBlock:false → allow); there is no
// third "ambiguous → prompt" state. metis used to map YoloSoftDeny to
// DecisionAsk, which made bypassPermissions mode still pop permission
// dialogs — exactly the UX claude-code's bypassPermissions avoids.
// Soft deny now collapses into the allow path. Hard deny stays hard.
func TestYolo_SoftDenyAllowsInBypass(t *testing.T) {
	g := New(ModeBypass)
	g.SetClassifier(&stubClassifier{verdict: YoloSoftDeny, reason: "rm -rf-ish"})
	d, src := g.Check(context.Background(), "Bash", "rm -r .")
	if d != DecisionAllow {
		t.Errorf("YoloSoftDeny in bypass mode → DecisionAllow (claude-code parity); got %v src=%q", d, src)
	}
}

// TestYolo_HardDenyBlocks — hard_deny is a clear "no" — even in bypass
// mode, the call is blocked. Source string must include "yolo" so the
// user can tell why a bypass call got denied.
func TestYolo_HardDenyBlocks(t *testing.T) {
	g := New(ModeBypass)
	g.SetClassifier(&stubClassifier{verdict: YoloHardDeny, reason: "rm -rf /"})
	d, src := g.Check(context.Background(), "Bash", "rm -rf /")
	if d != DecisionDeny {
		t.Errorf("YoloHardDeny → DecisionDeny; got %v src=%q", d, src)
	}
}

// TestYolo_FailOpenOnError — if the classifier errors (LLM outage,
// timeout), we must NOT block the user — they're already in bypass
// mode so they want maximum throughput. Fail open to DecisionAllow.
func TestYolo_FailOpenOnError(t *testing.T) {
	g := New(ModeBypass)
	g.SetClassifier(&stubClassifier{err: errors.New("upstream timeout")})
	d, _ := g.Check(context.Background(), "Bash", "anything")
	if d != DecisionAllow {
		t.Errorf("classifier error → fail-open allow; got %v", d)
	}
}

// TestYolo_NotConsultedOutsideBypass — auto/ask/plan modes must NOT
// invoke the classifier (extra latency for nothing).
func TestYolo_NotConsultedOutsideBypass(t *testing.T) {
	called := false
	noisy := &stubClassifier{verdict: YoloHardDeny}
	for _, mode := range []Mode{ModeAsk, ModeAcceptEdits, ModePlan} {
		g := New(mode)
		// Wrap to detect any call.
		wrapper := &countingClassifier{inner: noisy, hits: &called}
		g.SetClassifier(wrapper)
		_, _ = g.Check(context.Background(), "Bash", "ls")
	}
	if called {
		t.Errorf("classifier should only run in ModeBypass; got hit in non-bypass mode")
	}
}

type countingClassifier struct {
	inner *stubClassifier
	hits  *bool
}

func (c *countingClassifier) Classify(ctx context.Context, t, s string) (YoloVerdict, string, error) {
	*c.hits = true
	return c.inner.Classify(ctx, t, s)
}
