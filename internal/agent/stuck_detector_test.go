package agent

// stuck_detector_test.go — pin the state machine of stuckDetector.
// Each test models one outer-loop iteration as a call to AfterTurn
// with synthetic toolUses+results that mimic what the Loop would
// pass in. The detector's internal state is asserted by chaining
// calls: stuckResetNeeded must fire on the 4th same-signature
// Bash-failure turn, and stuckAbort must fire on the 4th such
// turn AFTER the reset.

import (
	"testing"

	"github.com/Ricardo-M-L/metis/internal/llm"
)

// helper: build a Bash tool_use block running the given command.
func bashOf(cmd string) llm.ContentBlock {
	return llm.ContentBlock{
		Type:      "tool_use",
		ToolUseID: "b-" + cmd,
		ToolName:  "Bash",
		ToolInput: map[string]any{"command": cmd},
	}
}

// helper: build a tool_result for Bash that contains a FAIL line
// for the given test name. Matches go-test format.
func bashResultFail(toolUseID, testName string) llm.ContentBlock {
	body := "=== RUN   " + testName + "\n" +
		"--- FAIL: " + testName + " (0.02s)\n" +
		"    foo_test.go:42: expected 5 got 7\n" +
		"FAIL\ncalc/foo\t0.123s\nFAIL\n"
	return llm.ContentBlock{
		Type:       "tool_result",
		ToolUseID:  toolUseID,
		ToolResult: body,
		IsError:    true,
	}
}

// helper: build a passing Bash result.
func bashResultPass(toolUseID string) llm.ContentBlock {
	return llm.ContentBlock{
		Type:       "tool_result",
		ToolUseID:  toolUseID,
		ToolResult: "ok  \tcalc/foo\t0.123s\n",
		IsError:    false,
	}
}

// helper: build a Read tool_use (non-Bash) for the "neutral turn" cases.
func readOf(path string) llm.ContentBlock {
	return llm.ContentBlock{
		Type:      "tool_use",
		ToolUseID: "r-" + path,
		ToolName:  "Read",
		ToolInput: map[string]any{"path": path},
	}
}

// runFailTurn synthesizes one full turn: Bash ran, result FAILed
// with the given test name. Returns the AfterTurn outcome.
func runFailTurn(t *testing.T, s *stuckDetector, testName string) stuckOutcome {
	t.Helper()
	return s.AfterTurn(
		[]llm.ContentBlock{bashOf("go test ./...")},
		[]llm.ContentBlock{bashResultFail("b-go test ./...", testName)},
	)
}

func TestStuckDetector_FiresOnFourthSameFailureSignature(t *testing.T) {
	s := &stuckDetector{}

	// Turns 1-3: same fail. Counter 1,2,3 — under threshold (4).
	for i := 1; i <= 3; i++ {
		out := runFailTurn(t, s, "TestParse_SimpleLet")
		if out != stuckNone {
			t.Errorf("turn %d: want stuckNone, got %v", i, out)
		}
	}

	// Turn 4: same fail → threshold met → reset reminder.
	if out := runFailTurn(t, s, "TestParse_SimpleLet"); out != stuckResetNeeded {
		t.Errorf("turn 4: want stuckResetNeeded, got %v", out)
	}
	if s.resetsFired != 1 {
		t.Errorf("resetsFired = %d, want 1 after first fire", s.resetsFired)
	}
	if s.sigCount != 0 || s.lastSig != "" {
		t.Errorf("post-reset state: want count=0 lastSig=\"\", got count=%d lastSig=%q", s.sigCount, s.lastSig)
	}
}

func TestStuckDetector_DifferentFailureResetsCounter(t *testing.T) {
	s := &stuckDetector{}
	runFailTurn(t, s, "TestA")
	runFailTurn(t, s, "TestA")
	runFailTurn(t, s, "TestA")
	// Now a different first-failed-test → counter restarts at 1,
	// NOT roll over to 4.
	out := runFailTurn(t, s, "TestB")
	if out != stuckNone {
		t.Errorf("different failure: want stuckNone (counter restarted), got %v", out)
	}
	if s.sigCount != 1 {
		t.Errorf("different failure: want count=1, got %d", s.sigCount)
	}
	if s.lastSig != "--- FAIL: TestB" {
		t.Errorf("different failure: lastSig = %q, want \"--- FAIL: TestB\"", s.lastSig)
	}
}

func TestStuckDetector_PassingBashResetsCounter(t *testing.T) {
	s := &stuckDetector{}
	runFailTurn(t, s, "TestA")
	runFailTurn(t, s, "TestA")
	runFailTurn(t, s, "TestA")
	// Bash ran successfully (no FAIL) → reset counter.
	out := s.AfterTurn(
		[]llm.ContentBlock{bashOf("go test ./...")},
		[]llm.ContentBlock{bashResultPass("b-go test ./...")},
	)
	if out != stuckNone {
		t.Errorf("passing bash: want stuckNone, got %v", out)
	}
	if s.sigCount != 0 || s.lastSig != "" {
		t.Errorf("passing bash: state should reset; got count=%d lastSig=%q", s.sigCount, s.lastSig)
	}
	// And next failure starts fresh, NOT at 4.
	out = runFailTurn(t, s, "TestA")
	if out != stuckNone {
		t.Errorf("after pass+fail: want stuckNone (fresh count), got %v", out)
	}
	if s.sigCount != 1 {
		t.Errorf("after pass+fail: want count=1, got %d", s.sigCount)
	}
}

func TestStuckDetector_NoBashTurnLeavesStateAlone(t *testing.T) {
	s := &stuckDetector{}
	runFailTurn(t, s, "TestA")
	runFailTurn(t, s, "TestA")
	// Now a turn with no Bash (model just reads / edits) → state
	// unchanged. This is critical: the model often spaces test
	// runs across multiple think+edit turns and we don't want to
	// reset the counter every time it reads a file.
	out := s.AfterTurn(
		[]llm.ContentBlock{readOf("foo.go")},
		[]llm.ContentBlock{{Type: "tool_result", ToolUseID: "r-foo.go", ToolResult: "(file content)"}},
	)
	if out != stuckNone {
		t.Errorf("no-bash turn: want stuckNone, got %v", out)
	}
	if s.sigCount != 2 {
		t.Errorf("no-bash turn: count should be untouched at 2, got %d", s.sigCount)
	}
	if s.lastSig != "--- FAIL: TestA" {
		t.Errorf("no-bash turn: lastSig should be untouched, got %q", s.lastSig)
	}
	// Two more Bash-fail turns with same sig → trip on 4th total.
	runFailTurn(t, s, "TestA")
	out = runFailTurn(t, s, "TestA")
	if out != stuckResetNeeded {
		t.Errorf("4th fail across no-bash gap: want stuckResetNeeded, got %v", out)
	}
}

func TestStuckDetector_BuildFailedHasOwnSignature(t *testing.T) {
	s := &stuckDetector{}
	// 4 consecutive build_failed turns.
	for i := 1; i <= 3; i++ {
		out := s.AfterTurn(
			[]llm.ContentBlock{bashOf("go test ./...")},
			[]llm.ContentBlock{{
				Type:       "tool_result",
				ToolUseID:  "b-go test ./...",
				ToolResult: "# calc/parser\n./parser.go:10:5: syntax error\nFAIL\tcalc/parser [build failed]\n",
				IsError:    true,
			}},
		)
		if out != stuckNone {
			t.Errorf("build-failed turn %d: want stuckNone, got %v", i, out)
		}
	}
	out := s.AfterTurn(
		[]llm.ContentBlock{bashOf("go test ./...")},
		[]llm.ContentBlock{{
			Type:       "tool_result",
			ToolUseID:  "b-go test ./...",
			ToolResult: "# calc/parser\n./parser.go:99:5: another syntax error\nFAIL\tcalc/parser [build failed]\n",
			IsError:    true,
		}},
	)
	if out != stuckResetNeeded {
		t.Errorf("4th build-failed turn: want stuckResetNeeded, got %v", out)
	}
	if s.lastSig != "" || s.sigCount != 0 {
		t.Errorf("post-reset state: count=%d lastSig=%q, want 0/\"\"", s.sigCount, s.lastSig)
	}
}

// TestStuckDetector_AllowsTwoResetsThenAborts — Phase C-mini v3
// (iter4 evidence): single reset budget left model stranded when
// it cleared early-layer bugs but hit fresh later-layer bugs.
// New budget allows 2 resets; the 3rd trip escalates to abort.
func TestStuckDetector_AllowsTwoResetsThenAborts(t *testing.T) {
	s := &stuckDetector{}
	// Trip 1: 4 same-sig fails → reset #1.
	for i := 1; i <= 4; i++ {
		runFailTurn(t, s, "TestA")
	}
	if s.resetsFired != 1 {
		t.Fatalf("after trip 1: resetsFired = %d, want 1", s.resetsFired)
	}

	// Trip 2: another 4 same-sig fails (could be a NEW sig — the
	// reset budget isn't sig-specific) → reset #2.
	for i := 1; i <= 3; i++ {
		runFailTurn(t, s, "TestB")
	}
	if out := runFailTurn(t, s, "TestB"); out != stuckResetNeeded {
		t.Errorf("trip 2 (within reset budget): want stuckResetNeeded, got %v (resetsFired=%d)", out, s.resetsFired)
	}
	if s.resetsFired != 2 {
		t.Errorf("after trip 2: resetsFired = %d, want 2", s.resetsFired)
	}

	// Trip 3: budget exhausted → abort.
	for i := 1; i <= 3; i++ {
		runFailTurn(t, s, "TestC")
	}
	if out := runFailTurn(t, s, "TestC"); out != stuckAbort {
		t.Errorf("trip 3 (budget exhausted): want stuckAbort, got %v", out)
	}
}

// TestStuckDetector_ResetBudgetSticksAcrossUnrelatedActivity —
// resetsFired must persist across non-stuck activity (passes,
// reads). The budget is per-run, not per-stuck-episode.
func TestStuckDetector_ResetBudgetSticksAcrossUnrelatedActivity(t *testing.T) {
	s := &stuckDetector{}
	// Trip twice — exhaust the 2-reset budget.
	for i := 1; i <= 4; i++ {
		runFailTurn(t, s, "TestA")
	}
	for i := 1; i <= 4; i++ {
		runFailTurn(t, s, "TestB")
	}
	if s.resetsFired != 2 {
		t.Fatalf("after 2 trips: resetsFired = %d, want 2", s.resetsFired)
	}

	// Unrelated activity in between.
	for i := 0; i < 5; i++ {
		s.AfterTurn(
			[]llm.ContentBlock{readOf("x.go")},
			[]llm.ContentBlock{{Type: "tool_result", ToolUseID: "r-x.go", ToolResult: "ok"}},
		)
	}
	s.AfterTurn(
		[]llm.ContentBlock{bashOf("go test")},
		[]llm.ContentBlock{bashResultPass("b-go test")},
	)
	if s.resetsFired != 2 {
		t.Error("resetsFired must persist across unrelated activity (budget is per-run)")
	}

	// Next stuck streak goes straight to abort.
	for i := 1; i <= 3; i++ {
		runFailTurn(t, s, "TestC")
	}
	if out := runFailTurn(t, s, "TestC"); out != stuckAbort {
		t.Errorf("next trip after budget exhausted: want stuckAbort, got %v", out)
	}
}

func TestExtractFailureSignature(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{"single fail", "--- FAIL: TestX (0.00s)\n  foo: bar\n", "--- FAIL: TestX"},
		{"timing variation collapses", "--- FAIL: TestX (1.23s)\n", "--- FAIL: TestX"},
		{"first of two fails", "--- FAIL: TestA (0.00s)\n  err\n--- FAIL: TestB (0.00s)\n  err\nFAIL\n", "--- FAIL: TestA"},
		{"build failed", "# calc/foo\n./foo.go:10: syntax\nFAIL\tcalc/foo [build failed]\n", "BUILD_FAILED"},
		{"FAIL summary but no per-test", "ok\tpkg/a\nFAIL\tpkg/b 0.1s\n", ""},
		{"only ok", "ok  \tcalc/foo\t0.12s\n", ""},
		{"empty", "", ""},
		{"prose 'failed'", "the test failed because of a thing\n", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			results := []llm.ContentBlock{{Type: "tool_result", ToolResult: c.body}}
			if got := extractFailureSignature(results); got != c.want {
				t.Errorf("extractFailureSignature(%q) = %q, want %q", c.body, got, c.want)
			}
		})
	}
}

// TestStuckDetector_NoGreenCounterTripsOnRotatingFailures —
// Pattern 2: model rotates through DIFFERENT failure modes every
// turn (so sigCount keeps resetting to 1) but never produces a
// green build. After stuckNoGreenThreshold (8) such turns the
// broad counter must trip. This is the iter3 bug — sig-only
// detector missed this loop entirely.
func TestStuckDetector_NoGreenCounterTripsOnRotatingFailures(t *testing.T) {
	s := &stuckDetector{}
	tests := []string{"TestA", "TestB", "TestA", "TestC", "TestB", "TestD", "TestA"}
	for i, name := range tests {
		out := runFailTurn(t, s, name)
		if out != stuckNone {
			t.Errorf("turn %d (%s): want stuckNone, got %v (sigCount=%d noGreenCount=%d)",
				i+1, name, out, s.sigCount, s.noGreenCount)
		}
	}
	// Turn 8: any different fail → noGreenCount hits 8 → trip.
	if out := runFailTurn(t, s, "TestE"); out != stuckResetNeeded {
		t.Errorf("turn 8 (rotating fails): want stuckResetNeeded, got %v (sigCount=%d noGreenCount=%d)",
			out, s.sigCount, s.noGreenCount)
	}
	if s.resetsFired != 1 {
		t.Errorf("resetsFired = %d, want 1 after broad-pattern trip", s.resetsFired)
	}
	if s.noGreenCount != 0 || s.sigCount != 0 || s.lastSig != "" {
		t.Errorf("post-reset: want all counters/sig zeroed; got noGreen=%d sig=%d lastSig=%q",
			s.noGreenCount, s.sigCount, s.lastSig)
	}
}

// TestStuckDetector_GreenBuildResetsBothCounters — a successful
// Bash run must reset BOTH counters so a fresh fail-streak starts
// from scratch.
func TestStuckDetector_GreenBuildResetsBothCounters(t *testing.T) {
	s := &stuckDetector{}
	// Build up some pressure on both counters.
	runFailTurn(t, s, "TestA")
	runFailTurn(t, s, "TestB")
	runFailTurn(t, s, "TestA")
	if s.sigCount == 0 || s.noGreenCount == 0 {
		t.Fatalf("setup: expected non-zero counters; got sig=%d noGreen=%d", s.sigCount, s.noGreenCount)
	}
	// Green build.
	s.AfterTurn(
		[]llm.ContentBlock{bashOf("go test ./...")},
		[]llm.ContentBlock{bashResultPass("b-go test ./...")},
	)
	if s.sigCount != 0 || s.noGreenCount != 0 || s.lastSig != "" {
		t.Errorf("after green: counters must be reset; got sig=%d noGreen=%d lastSig=%q",
			s.sigCount, s.noGreenCount, s.lastSig)
	}
}

// TestStuckDetector_DeniedBashDoesNotResetCounter — Phase C-mini v5
// (iter9 bug fix): a DENIED Bash (rule #23 etc.) returns IsError=true
// with body "denied: ..." — NO "--- FAIL:" marker. The pre-fix detector
// treated "no FAIL marker" as green build and reset the noGreenCount,
// which let model loops with interleaved denies escape detection. After
// the fix, IsError=true counts as failure regardless of marker.
func TestStuckDetector_DeniedBashDoesNotResetCounter(t *testing.T) {
	s := &stuckDetector{}
	// 3 same-test failures.
	runFailTurn(t, s, "TestA")
	runFailTurn(t, s, "TestA")
	runFailTurn(t, s, "TestA")
	if s.noGreenCount != 3 {
		t.Fatalf("setup: noGreenCount=%d, want 3", s.noGreenCount)
	}
	// Now a denied Bash (mimics the iter9 rule #23 trip).
	deniedResult := llm.ContentBlock{
		Type:       "tool_result",
		ToolUseID:  "b-bad",
		ToolResult: "denied by permission policy: bash-security rule #23: newline inside an unclosed quoted region — multi-line smuggling",
		IsError:    true,
	}
	s.AfterTurn(
		[]llm.ContentBlock{bashOf("badcmd")},
		[]llm.ContentBlock{deniedResult},
	)
	// Both counters must persist (not reset to 0). And the
	// noGreenCount must have INCREMENTED since denied still counts
	// as a failed Bash turn.
	if s.noGreenCount != 4 {
		t.Errorf("noGreenCount after deny should be 4, got %d (deny may have wrongly reset)", s.noGreenCount)
	}
	if s.sigCount == 0 {
		t.Error("sigCount must persist after deny; got 0 (deny wrongly reset)")
	}
	// And 4 more failures → noGreenCount=8 → trips broad detector.
	runFailTurn(t, s, "TestA")
	runFailTurn(t, s, "TestA")
	runFailTurn(t, s, "TestA")
	out := runFailTurn(t, s, "TestA")
	if out != stuckResetNeeded {
		t.Errorf("after 8 effective fails (with deny in middle): want stuckResetNeeded, got %v (noGreenCount=%d)", out, s.noGreenCount)
	}
}

func TestBashRan(t *testing.T) {
	cases := []struct {
		name     string
		toolUses []llm.ContentBlock
		want     bool
	}{
		{"single bash", []llm.ContentBlock{bashOf("ls")}, true},
		{"bash + read", []llm.ContentBlock{readOf("x.go"), bashOf("ls")}, true},
		{"no bash", []llm.ContentBlock{readOf("x.go")}, false},
		{"empty", nil, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := bashRan(c.toolUses); got != c.want {
				t.Errorf("bashRan = %v, want %v", got, c.want)
			}
		})
	}
}
