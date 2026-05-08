package tui

// Phase-C smoke tests. We exercise the pure-string handlers (cmdCopy
// with no history, cmdOutputStyle dispatcher, cmdBreakCache help text,
// cmdSecurityReview prompt shape, cmdFeedback aliasing). The git +
// gh subprocess paths in cmdCommitPushPR aren't worth a fake-git
// fixture for now — those run live during smoke.

import (
	"strings"
	"testing"
)

func TestCopy_NoActiveSession(t *testing.T) {
	// REPL with nil Loop covers the "no session" guard. Useful as the
	// canonical "calling cmdCopy before chat is wired up" path.
	r := &REPL{}
	out := cmdCopy(r, "")
	if !strings.Contains(out, "no active session") {
		t.Errorf("expected 'no active session'; got: %q", out)
	}
}

func TestCopy_BadCount(t *testing.T) {
	r := &REPL{}
	out := cmdCopy(r, "abc")
	if !strings.Contains(out, "usage:") {
		t.Errorf("non-numeric arg should show usage; got: %q", out)
	}
}

func TestOutputStyle_Default(t *testing.T) {
	r := &REPL{}
	out := cmdOutputStyle(r, "")
	if !strings.Contains(out, "full") {
		t.Errorf("default state should be 'full'; got:\n%s", out)
	}
}

func TestOutputStyle_Switch(t *testing.T) {
	r := &REPL{UseMarkdown: true}
	cmdOutputStyle(r, "minimal")
	if r.outputStyle != "minimal" {
		t.Errorf("outputStyle should be 'minimal'; got %q", r.outputStyle)
	}
	if r.UseMarkdown {
		t.Errorf("minimal should disable markdown")
	}
	cmdOutputStyle(r, "full")
	if !r.UseMarkdown {
		t.Errorf("full should re-enable markdown")
	}
}

func TestOutputStyle_UnknownArg(t *testing.T) {
	r := &REPL{}
	out := cmdOutputStyle(r, "loud")
	if !strings.Contains(out, "unknown") {
		t.Errorf("unknown arg should error; got: %q", out)
	}
}

func TestBreakCache_RendersHelp(t *testing.T) {
	r := &REPL{}
	out := cmdBreakCache(r, "")
	if !strings.Contains(out, "/compact") || !strings.Contains(out, "/clear") {
		t.Errorf("break-cache should mention /compact and /clear; got:\n%s", out)
	}
}

func TestSecurityReview_DefaultTarget(t *testing.T) {
	r := &REPL{}
	out := cmdSecurityReview(r, "")
	if !strings.Contains(out, "OWASP") && !strings.Contains(out, "SQL injection") {
		t.Errorf("security-review should mention OWASP or specific class; got: %q", out)
	}
	if !strings.Contains(out, "staged changes") {
		t.Errorf("default target should be staged changes; got: %q", out)
	}
}

func TestSecurityReview_ExplicitTarget(t *testing.T) {
	r := &REPL{}
	out := cmdSecurityReview(r, "internal/auth/")
	if !strings.Contains(out, "internal/auth/") {
		t.Errorf("explicit target should appear in prompt; got: %q", out)
	}
}

func TestFeedback_AliasesBug(t *testing.T) {
	r := &REPL{}
	feedbackOut := cmdFeedback(r, "")
	bugOut := cmdBug(r, "")
	if feedbackOut != bugOut {
		t.Errorf("/feedback should produce identical output to /bug;\nfeedback: %q\nbug: %q", feedbackOut, bugOut)
	}
}
