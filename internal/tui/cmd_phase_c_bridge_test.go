package tui

// Tests for the Phase-C commands that gained REPL-bridge closures
// (cmdBg / cmdSecurityReview / cmdBreakCache / cmdUsage / cmdBug).
// The bridge fields on REPL are funcs, so we can fully drive
// them without spinning up a Model — pass closures that capture test-
// local state and assert on it.
//
// 2026-07-28: cmdReview was removed (its REPL shadow of the slash
// registry's /review handler is gone). The two cmdReview tests that
// lived here moved to internal/slash/review_test.go where the new
// handler actually runs.

import (
	"strings"
	"testing"
	"time"
)

func TestBreakCache_CallsBypassClosure(t *testing.T) {
	called := 0
	r := &REPL{BypassCache: func() { called++ }}
	out := cmdBreakCache(r, "")
	if called != 1 {
		t.Errorf("BypassCache should fire exactly once; got %d", called)
	}
	if !strings.Contains(out, "armed") {
		t.Errorf("output should confirm arming; got: %q", out)
	}
}

func TestBreakCache_FallsBackWithoutBridge(t *testing.T) {
	// nil BypassCache → render the help table fallback.
	r := &REPL{}
	out := cmdBreakCache(r, "")
	if !strings.Contains(out, "/compact") {
		t.Errorf("fallback should mention /compact; got:\n%s", out)
	}
}

func TestSecurityReview_LoadsPromptIntoInput(t *testing.T) {
	var captured string
	r := &REPL{InsertInput: func(s string) { captured = s }}
	cmdSecurityReview(r, "")
	if !strings.Contains(captured, "OWASP") {
		t.Errorf("InsertInput body should be OWASP-flavored; got: %q", captured)
	}
}

func TestBg_RendersActiveSnapshot(t *testing.T) {
	r := &REPL{
		BgTurnSnapshot: func() BgTurnState {
			return BgTurnState{
				IsActive:    true,
				StartTime:   time.Now().Add(-5 * time.Second),
				Model:       "claude-opus-4-7",
				QueuedCount: 2,
			}
		},
	}
	out := cmdBg(r, "")
	if !strings.Contains(out, "claude-opus-4-7") {
		t.Errorf("expected model in bg out; got: %q", out)
	}
	if !strings.Contains(out, "2") {
		t.Errorf("queued count should surface; got: %q", out)
	}
}

func TestBg_RendersIdleSnapshot(t *testing.T) {
	r := &REPL{
		BgTurnSnapshot: func() BgTurnState { return BgTurnState{} },
	}
	out := cmdBg(r, "")
	if !strings.Contains(strings.ToLower(out), "no") && !strings.Contains(out, "idle") {
		t.Errorf("idle path should say no/idle; got: %q", out)
	}
}

func TestUsage_KnownProviderHasDashboardURL(t *testing.T) {
	r := &REPL{providerName: "deepseek", model: "deepseek-chat"}
	out := cmdUsage(r, "")
	if !strings.Contains(out, "platform.deepseek.com") {
		t.Errorf("deepseek dashboard URL should appear; got:\n%s", out)
	}
	if !strings.Contains(out, "deepseek-chat") {
		t.Errorf("model should surface; got:\n%s", out)
	}
}

func TestUsage_UnknownProviderShowsHint(t *testing.T) {
	r := &REPL{providerName: "moonbase-alpha"}
	out := cmdUsage(r, "")
	if !strings.Contains(strings.ToLower(out), "unknown") {
		t.Errorf("unknown provider should be flagged; got:\n%s", out)
	}
}

func TestBug_BodyContainsTemplateSections(t *testing.T) {
	r := &REPL{}
	out := cmdBug(r, "agent freezes after long Edit")
	// cmdBug writes to clipboard + prints a URL hint; the printed body
	// should at minimum carry the user's free-form complaint as a
	// "Description" hint.
	if !strings.Contains(out, "github.com") {
		t.Errorf("bug command should print the issue URL; got: %q", out)
	}
	if !strings.Contains(out, "clipboard") {
		t.Errorf("bug command should mention clipboard copy; got: %q", out)
	}
}
