package agent

// progress_detector.go — diminishing-returns detector that
// supplements LoopDetector (which catches identical-signature
// repeats) with a softer signal: the model is still moving but
// each iteration produces almost no useful tool output.
//
// claude-code's tokenBudget.ts:59-63 has the equivalent check:
// after ≥3 budget-continuation nudges, if the per-turn token
// delta drops below 500, it stops — "the model is spinning, not
// making progress."
//
// metis doesn't budget by tokens, so we use a bytes-of-tool-
// output proxy: a low-progress iter is one whose successful
// tool_result outputs sum to less than progressLowBytesThreshold.
// Errors and empty-stop iters count as low-progress too.
//
// The detector only fires AFTER the 75% iter nudge has already
// gone out — before that we trust the model to find its rhythm.
// Three consecutive low-progress iters past 75% → inject one focused
// recovery reminder. This detector must never hard-stop inside a tool
// batch: doing so would discard completed results and suppress the final
// assistant response. MaxIters remains the bounded terminal backstop.

import (
	"github.com/Ricardo-M-L/metis/internal/llm"
)

// progressLowBytesThreshold is the per-iter tool-output byte
// floor. Iters whose successful (non-error) tool results sum to
// less than this count as "no useful progress this turn."
//
// 256 chosen empirically: a real Read returns at least a few KB
// of file content; a real Grep returns at least 100-200 chars
// per hit; a successful Write returns "wrote N bytes" (~50 chars
// — sub-threshold but Write is excluded anyway, see below). An
// iter whose only output is a 30-char "(empty file)" or "(no
// matches)" message is genuinely no-progress.
const progressLowBytesThreshold = 256

// progressMaxLowIters is the number of consecutive low-progress
// iters that triggers the one-shot recovery advisory.
const progressMaxLowIters = 3

// progressDetector tracks recent iter productivity. Lives next
// to LoopDetector on the Loop but stays orthogonal: LoopDetector
// catches "same call+result repeated"; this one catches "no
// repeat, but also no new info."
type progressDetector struct {
	consecutiveLowIters int
	lastIterBytes       int
	totalIters          int
}

// newProgressDetector builds a zero-state detector. The Loop
// creates one per Run() call.
func newProgressDetector() *progressDetector {
	return &progressDetector{}
}

// RecordIter folds one iter's tool results into the detector.
// Call after executeBatch returns, before checking IsDiminishing.
//
// Write/Edit tool kinds are exempt from the low-progress check:
// a successful Write returns "wrote N bytes" (~50 chars) which
// would always trip the threshold even though writing IS useful
// progress. The model writing a file is exactly the kind of
// "finishing work" we want to encourage in the late iters.
//
// The `results` parameter carries the tool_result blocks that
// will be sent back to the model (Type="tool_result", body in
// ToolResult, error flag in IsError). `toolUses` are the parallel
// originating tool_use blocks so we can map result[i] → tool name.
func (p *progressDetector) RecordIter(toolUses []llm.ContentBlock, results []llm.ContentBlock) {
	p.totalIters++

	// No-tool iter (model just emitted text + stopped): handled
	// by the "stop != tool_use" branch in Loop.Run; we never get
	// called for those. So len(results) == 0 here means the
	// model emitted tool_use blocks but they all errored out
	// before producing results — that's low-progress.
	if len(results) == 0 {
		p.consecutiveLowIters++
		p.lastIterBytes = 0
		return
	}

	usefulBytes := 0
	for i, r := range results {
		if r.IsError {
			continue
		}
		// Skip mutating tools — see comment above. Writes /
		// edits / shell mutations are valuable even when their
		// echo output is short.
		//
		// Bash is added here (2026-05-19) after the bench6 iter6
		// false-positive: a successful `go test ./...` prints just
		// "ok    pkg    0.123s" (~25 bytes) per package. Three
		// consecutive green test runs past the 75% nudge → all
		// below the threshold → diminishing-returns aborted the
		// run RIGHT AFTER the model said "All tests pass. Now run
		// with -race -cover as requested." We mistook the green
		// finish-line for a stuck spin. Any Bash that reached this
		// branch already passed the IsError=true skip above, so it
		// exited 0 — that's the strongest possible progress signal
		// (the model actually got something to work), regardless of
		// how chatty its stdout was.
		if i < len(toolUses) {
			switch toolUses[i].ToolName {
			case "Write", "Edit", "MultiEdit", "NotebookEdit", "Bash":
				usefulBytes += progressLowBytesThreshold // count as full credit
				continue
			}
		}
		usefulBytes += len(r.ToolResult)
	}

	p.lastIterBytes = usefulBytes
	if usefulBytes < progressLowBytesThreshold {
		p.consecutiveLowIters++
	} else {
		p.consecutiveLowIters = 0
	}
}

// IsDiminishing returns true when the detector has seen
// progressMaxLowIters consecutive low-progress iters. The Loop
// should also gate on "already past 75% nudge" before acting —
// early-session low-progress (model still picking a strategy)
// is not interesting.
func (p *progressDetector) IsDiminishing() bool {
	return p.consecutiveLowIters >= progressMaxLowIters
}

// Reset clears the consecutive-low counter, used after a
// significant event (new user message, hook intervention) so
// the next stall window starts clean.
func (p *progressDetector) Reset() {
	p.consecutiveLowIters = 0
	p.lastIterBytes = 0
}

// ConsecutiveLow returns the current streak length. Used by
// the recovery EventInfo message and detector tests.
func (p *progressDetector) ConsecutiveLow() int {
	return p.consecutiveLowIters
}
