package slash

import (
	"strings"
	"testing"
)

// 2026-07-28 rewrite — the old table-driven tests covered the
// target-resolution enum + git subprocess helpers. Both are gone:
// the handler now emits a single model-directed prompt (CC-style
// LOCAL_REVIEW_PROMPT) and lets the model decide which git/gh
// commands to run. These tests lock in the new prompt shape and
// the handler's pass-through behaviour for free-form args.

func TestReviewHandler_EmitsModelDirectedPrompt(t *testing.T) {
	display, sig := reviewHandler("")
	if sig != SignalCustomPrompt {
		t.Errorf("sig = %v, want SignalCustomPrompt", sig)
	}
	// The prompt should instruct the model to inspect repo state
	// (not pre-collect a diff).
	for _, want := range []string{
		"git status",
		"git diff --cached",
		"git diff",
		"gh pr view",
		"git merge-base",
		"VERDICT: PASS",
		"VERDICT: NEEDS WORK",
		"VERDICT: FAIL",
		"[P0]",
		"[P1]",
		"path:line",
	} {
		if !strings.Contains(display, want) {
			t.Errorf("prompt missing %q", want)
		}
	}
}

func TestReviewHandler_NoDiffIsInlined(t *testing.T) {
	// Critical: the handler must NOT call git itself. If it did,
	// the prompt would contain a "```diff" fenced block. The new
	// model-directed shape leaves diff collection to the model.
	display, _ := reviewHandler("")
	if strings.Contains(display, "```diff") {
		t.Error("prompt should not inline a diff fence — diff collection belongs to the model")
	}
}

func TestReviewHandler_PassesArgsThrough(t *testing.T) {
	in := "focus on the auth flow only"
	display, sig := reviewHandler(in)
	if sig != SignalCustomPrompt {
		t.Errorf("sig = %v, want SignalCustomPrompt", sig)
	}
	if !strings.Contains(display, in) {
		t.Errorf("prompt should contain the user's verbatim input %q", in)
	}
}

func TestReviewHandler_TrimsArgs(t *testing.T) {
	display, _ := reviewHandler("  main  ")
	if !strings.Contains(display, "main") {
		t.Error("args should appear in the prompt")
	}
	if strings.Contains(display, "  main  ") {
		t.Error("args should be trimmed before being embedded")
	}
}

func TestReviewHandler_NoArgsHasNoUserInputSection(t *testing.T) {
	display, _ := reviewHandler("")
	if strings.Contains(display, "## User input") {
		t.Error("empty args should NOT render the 'User input' section header")
	}
}

func TestReviewHandler_NonEmptyArgsRendersUserInputSection(t *testing.T) {
	display, _ := reviewHandler("main")
	if !strings.Contains(display, "## User input") {
		t.Error("non-empty args should render the 'User input' section")
	}
}

// RegisterReviewCommand smoke: the registration must succeed and the
// handler must be reachable. Mirrors the same smoke test debug.go has.
func TestRegisterReviewCommand_RegistersHandler(t *testing.T) {
	r := NewRegistry()
	RegisterReviewCommand(r)
	cmd, ok := r.Get("review")
	if !ok {
		t.Fatal("review command not registered")
	}
	if cmd.Handler == nil {
		t.Fatal("review handler is nil")
	}
	display, sig := cmd.Handler("")
	if sig != SignalCustomPrompt {
		t.Errorf("handler sig = %v, want SignalCustomPrompt", sig)
	}
	if display == "" {
		t.Error("handler returned empty display")
	}
}
