package agent

// stuck_detector.go — Phase C-mini: catches "test keeps failing
// with the same symptom across multiple Bash runs" loops. This
// supplements LoopDetector (signature-based — only fires on
// identical tool_use+result repeats) and progressDetector (fires
// only past 75% budget). When a test keeps failing the same way,
// neither of those catches it early — the model spins for tens of
// minutes alternating edits across files until budget abort.
//
// Observed failure modes (2026-05-18 bench6 mini-interpreter):
//
//  iter1 — model wrote eval/eval.go and eval/eval_test.go with
//  disagreeing if-expression semantics; spent 36 min / 5M tokens
//  alternating edits between BOTH files, never asking which side
//  was wrong. The original Phase C-mini detector tracked "same
//  file edited N times" — which missed this because the model
//  was alternating files, not hammering one.
//
//  iter2 — model wrote a parser that didn't accept ';' between
//  statements. TestRun_SimpleLet, TestRun_ConditionalPrint,
//  TestRun_NestedIfElse all kept failing with the same
//  "unexpected token: SEMICOLON" error across many edit cycles.
//  Across 17 minutes the FIRST failing test in `go test` output
//  was consistently TestRun_SimpleLet — that's the signal.
//
// New strategy: extract the FIRST `--- FAIL: TestX` line from
// every Bash result. When the same failure signature persists
// across stuckSigThreshold consecutive Bash-running turns, the
// model is stuck on one root cause. Fire a reset reminder once;
// if it trips again, abort with StopReason="stuck_after_reset"
// (mapped to exit code 11 by cmdRun's Phase A switch).
//
// Bash-ran-but-no-FAIL clears the counter (model produced a
// green build, real progress). Turns with no Bash leave state
// alone (model is reading/thinking — neutral).

import (
	"strings"

	"github.com/Ricardo-M-L/metis/internal/llm"
)

// stuckSigThreshold is the number of consecutive Bash-running
// turns with the same failure signature that trips the detector.
// 4 was chosen empirically from iter1/iter2 logs: by the 4th
// repeat the model has had 3 chances to break out and clearly
// isn't going to without intervention. Threshold of 3 would have
// false-positived once in the iter1 log where the model briefly
// regressed on a previously-fixed test.
const stuckSigThreshold = 4

// stuckNoGreenThreshold is the number of consecutive Bash-running
// turns without ANY green build that trips the broad detector.
// Complements stuckSigThreshold: catches the case (iter3) where
// the model rotates through DIFFERENT failures every turn but
// never produces a green build for N consecutive Bash runs. The
// signature-based detector resets each time the failure changes,
// so iter3 failed to fire despite obviously being stuck.
//
// 8 is more permissive than 4 (sig-threshold) because the broad
// criterion has weaker per-turn evidence — many failure modes IS
// progress in a complex multi-package codebase, until it isn't.
// 8 consecutive failing `go test ./...` runs without ANY green
// is a real "the whole approach is wrong" signal.
const stuckNoGreenThreshold = 8

// stuckMaxResets is the per-Run cap on how many reset reminders
// the detector may inject before escalating to stuckAbort. 2 was
// chosen from iter4 data: the first reset clearly helped the
// model converge on early-layer bugs, and the model then hit
// fresh later-layer bugs that needed their own nudge.
const stuckMaxResets = 2

// stuckOutcome describes what the loop should do this turn based
// on the detector's state after the latest tool batch.
type stuckOutcome int

const (
	stuckNone        stuckOutcome = iota
	stuckResetNeeded              // inject reset reminder, give model another chance
	stuckAbort                    // already reset once and still looping — abort
)

// stuckDetector tracks two complementary patterns:
//
//  1. Same failure SIGNATURE persists across N=stuckSigThreshold
//     consecutive Bash-running turns. Catches "model knows where
//     the bug is, just can't fix it" loops.
//
//  2. No GREEN BUILD across N=stuckNoGreenThreshold consecutive
//     Bash-running turns (different failures each time count).
//     Catches "model is rotating through symptoms, each fix
//     breaks something else" loops.
//
// Both share one resetBudget — reset reminders can fire up to
// stuckMaxResets times per run. Each fire zeroes both counters
// so the model gets a fresh window after every reminder. The
// (N+1)-th trip after the budget is exhausted escalates to
// stuckAbort, bounding total work in an obviously-stuck run.
//
// Why allow >1 reset: iter4 evidence (2026-05-19 bench6 run)
// showed the first reset reminder produced real progress (token
// usage down 16%, eval+ast packages went green) but the model
// then hit a fresh set of bugs and got stuck again. The single-
// reset budget left no room to nudge a second time. Increasing
// to 2 captures more of the iter4-style "fix one layer, get
// stuck on next layer" sequences without losing the abort cap.
//
// Construct per Loop.Run() invocation; not safe for concurrent
// use (loop is single-threaded by design).
type stuckDetector struct {
	// Pattern 1: same failure signature
	lastSig  string // most recent failure signature (e.g. "--- FAIL: TestX" or "BUILD_FAILED")
	sigCount int    // consecutive Bash-ran turns where sig matched lastSig

	// Pattern 2: no green build across many Bash turns
	noGreenCount int // consecutive Bash-ran turns with ANY failure (not necessarily same)

	resetsFired int // count of reset reminders already issued this run (cap = stuckMaxResets)
}

// AfterTurn is called once per outer-loop iteration, after the
// tool batch's results are known. Returns what the loop should
// do (none / inject reminder / abort).
//
// Three turn classes:
//
//   - No Bash this turn — neutral, state unchanged. The model
//     might be reading or thinking; that's not a stuck signal.
//   - Bash ran and produced a non-FAIL result (test passed or
//     build succeeded) — progress, RESET the counter.
//   - Bash ran with a FAIL signature — compare to lastSig.
//     Same → increment. Different → reset to 1.
func (s *stuckDetector) AfterTurn(toolUses, results []llm.ContentBlock) stuckOutcome {
	if !bashRan(toolUses) {
		return stuckNone
	}
	sig := extractFailureSignature(results)
	if sig == "" {
		// Bash ran with no failure marker — green build. Reset
		// BOTH counters; the model just demonstrated progress.
		s.lastSig = ""
		s.sigCount = 0
		s.noGreenCount = 0
		return stuckNone
	}

	// Pattern 1: same-signature counter.
	if sig == s.lastSig {
		s.sigCount++
	} else {
		s.lastSig = sig
		s.sigCount = 1
	}
	// Pattern 2: any-failure counter — increments on EVERY
	// failed Bash turn, regardless of signature.
	s.noGreenCount++

	// Trip check: EITHER pattern hitting its threshold counts.
	// Capture which pattern fired so the abort/reset reason is
	// debuggable in the log.
	tripped := s.sigCount >= stuckSigThreshold || s.noGreenCount >= stuckNoGreenThreshold
	if !tripped {
		return stuckNone
	}

	if s.resetsFired < stuckMaxResets {
		s.resetsFired++
		// Reset BOTH counters so the model gets a fresh window.
		s.sigCount = 0
		s.noGreenCount = 0
		s.lastSig = ""
		return stuckResetNeeded
	}
	return stuckAbort
}

// bashRan returns true when the turn's tool_uses include at
// least one Bash call. Required because we only care about
// failures from THIS turn's test run, not stale failures from
// earlier turns sitting in our state.
func bashRan(toolUses []llm.ContentBlock) bool {
	for _, b := range toolUses {
		if b.Type == "tool_use" && b.ToolName == "Bash" {
			return true
		}
	}
	return false
}

// extractFailureSignature returns a short, stable string
// identifying the FIRST failure observed in any tool_result body.
// Empty means no failure was observed.
//
// Signature forms (in priority order):
//   - "--- FAIL: TestName" — first per-test failure line. The
//     "TestName" piece is taken up to the next space or "(" so
//     timing suffixes "(0.00s)" don't make every run look unique.
//   - "BUILD_FAILED" — compile error stopped the test run
//     before any test executed. The model wrote uncompilable code.
//
// Note: package-summary "FAIL\t<pkg>" lines are NOT a signature
// on their own — they appear in EVERY failing run including ones
// with new failure modes, so they don't distinguish stuck from
// progress.
func extractFailureSignature(results []llm.ContentBlock) string {
	for _, r := range results {
		if r.Type != "tool_result" {
			continue
		}
		body := r.ToolResult
		// First "--- FAIL: TestName" — the strongest signal.
		if idx := strings.Index(body, "--- FAIL: "); idx >= 0 {
			line := body[idx:]
			if nl := strings.IndexByte(line, '\n'); nl > 0 {
				line = line[:nl]
			}
			// Trim "(0.00s)" timing suffix so identical
			// failures across runs collapse to the same key.
			if p := strings.Index(line, " ("); p > 0 {
				line = line[:p]
			}
			return line
		}
		// Compile failure — no individual test ran. Treat as
		// its own signature so build-error loops also trip.
		if strings.Contains(body, "build failed") {
			return "BUILD_FAILED"
		}
	}
	return ""
}

// stuckResetReminderText is the system-reminder body the loop
// folds into the next user message when the detector fires its
// reset signal. Written as if the user is speaking, with a
// concrete plan rather than vague "try harder" — claude-code's
// equivalent nudges follow the same "name the wrong assumption"
// pattern instead of generic "try a different approach."
//
// The wording is failure-symptom-agnostic (doesn't mention a
// specific file) because the detector is signature-driven now
// and may have triggered on multi-file-rotation loops where
// no single file is the "stuck" target.
const stuckResetReminderText = `<system-reminder>STUCK-LOOP DETECTED — your recent ` + "`go test`" + ` runs have either repeated the same failure many times OR cycled through many different failures without ANY green build. Either way: your edits are not converging on a working solution.

STOP making more incremental edits right now. In your NEXT turn:

1. Look at the most recent ` + "`go test`" + ` output. Pick the FIRST failing test ("--- FAIL: TestName") or, if the build itself fails, the FIRST compile error.
2. Read BOTH the relevant test file AND the implementation file it exercises, end to end (no offset/limit — get the whole picture). Read them in ONE turn so you have both in context.
3. State explicitly: which side has the wrong assumption — the implementation, or the test? If you can't decide, the bug lives in a third file (a shared type definition, an upstream parser, a missing import) — name it and read that file first.
4. Rewrite the wrong side from scratch in ONE Write call. Do not try to patch in place.

This reminder fires only once per run. The next stuck-loop detection will abort.</system-reminder>`
