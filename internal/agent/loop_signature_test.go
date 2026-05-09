package agent

// loop_signature_test.go pins the crush-style sliding-window signature
// detector that closes the 2026-05-08 1h-18m hang gap.
//
// The plain count-based heuristics in loopdetection.go (per-tool
// callCounts, ping-pong pairs, global circuit breaker) didn't catch
// the live bug because the user's session had a *high* GlobalThreshold
// and the model alternated tools enough that no single per-tool count
// pegged. The signature detector folds the entire (toolName, input,
// result) tuple into a SHA-256 and trips when the same tuple appears
// past a threshold within a sliding window — no matter how much
// other noise sits around it.

import (
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/pkg/provider"
)

// makeStep produces a (toolUses, results) pair for a single Bash call
// with deterministic input + result. Helper avoids repeating the same
// 6-line struct literal across the test bodies.
func makeStep(id, cmd, output string) ([]provider.ContentBlock, []provider.ContentBlock) {
	use := provider.ContentBlock{
		Type:      "tool_use",
		ToolUseID: id,
		ToolName:  "Bash",
		ToolInput: map[string]any{"command": cmd},
	}
	res := provider.ContentBlock{
		Type:       "tool_result",
		ToolUseID:  id,
		ToolResult: output,
	}
	return []provider.ContentBlock{use}, []provider.ContentBlock{res}
}

// TestLoopDetector_SignatureRepeatTrips — the canonical case: the
// same `cd … && git rebase --continue` retried over and over with the
// same conflict-error output. After 6 identical steps inside a 10-step
// window the detector trips (window threshold = 5).
func TestLoopDetector_SignatureRepeatTrips(t *testing.T) {
	d := NewLoopDetector()

	for i := 0; i < 6; i++ {
		uses, results := makeStep("call-x", "cd /tmp && git rebase --continue",
			"error: cannot rebase, you have unstaged changes")
		d.RecordStep(uses, results)
	}

	if !d.ShouldAbort() {
		t.Fatalf("ShouldAbort = false after 6 identical steps; signatureWindow=%v",
			d.signatureWindow)
	}
	if d.AbortReason() != LoopSignatureRepeat {
		t.Errorf("AbortReason = %q, want %q", d.AbortReason(), LoopSignatureRepeat)
	}
}

// TestLoopDetector_DifferentInputsDontTrip — two different commands
// that each happen 5 times shouldn't trip; only sameness past the
// threshold counts. (5 of one + 5 of another = 10 steps, neither
// signature exceeds the 5-repeat ceiling.)
func TestLoopDetector_DifferentInputsDontTrip(t *testing.T) {
	d := NewLoopDetector()

	for i := 0; i < 5; i++ {
		uses, results := makeStep("a", "ls /tmp", "ok")
		d.RecordStep(uses, results)
	}
	for i := 0; i < 5; i++ {
		uses, results := makeStep("b", "ls /var", "ok")
		d.RecordStep(uses, results)
	}
	if d.ShouldAbort() {
		t.Errorf("two distinct repeating signatures shouldn't trip; window=%v",
			d.signatureWindow)
	}
}

// TestLoopDetector_ProgressiveOutputDoesNotTrip — a poll that gets
// different output each time (e.g. tailing a growing log) is exactly
// what the count-based heuristics over-trigger on. The signature
// version correctly stays quiet because each result is unique.
func TestLoopDetector_ProgressiveOutputDoesNotTrip(t *testing.T) {
	d := NewLoopDetector()

	for i := 0; i < 8; i++ {
		uses, results := makeStep("poll",
			"tail -n 5 /var/log/build.log",
			"line "+string(rune('a'+i)))
		d.RecordStep(uses, results)
	}
	if d.ShouldAbort() {
		t.Errorf("progressive output should not trip signature detector; window=%v",
			d.signatureWindow)
	}
}

// TestLoopDetector_TextOnlyStepIgnored — a step with no tool calls
// (model produced a text reply only) yields an empty signature and is
// skipped, so a turn that ended with a textual recap doesn't push
// the window forward and reset the counter.
func TestLoopDetector_TextOnlyStepIgnored(t *testing.T) {
	d := NewLoopDetector()

	// 5 identical loops + 1 text-only step + 1 more identical loop:
	// the text step shouldn't shift signatures out of the window.
	for i := 0; i < 5; i++ {
		uses, results := makeStep("c", "git status", "nothing to commit")
		d.RecordStep(uses, results)
	}
	d.RecordStep([]provider.ContentBlock{{Type: "text", Text: "thinking…"}},
		nil)
	uses, results := makeStep("c", "git status", "nothing to commit")
	d.RecordStep(uses, results)

	// 6 identical signatures still in the window (text-only contributed
	// nothing). Need one more to push past 5-of-10 if window already
	// has 6 + need >5; with 6 same out of 6 entries, count[X] = 6 > 5.
	if !d.ShouldAbort() {
		t.Fatal("text-only step shouldn't reset signature counting")
	}
}

// TestLoopDetector_WindowSlides — once the window fills, old
// signatures fall off. A 10-step run of A, A, A, A, A, B, B, B, B, B
// has neither count exceeding 5, so should NOT trip.
func TestLoopDetector_WindowSlides(t *testing.T) {
	d := NewLoopDetector()

	for i := 0; i < 5; i++ {
		uses, results := makeStep("A", "echo a", "a")
		d.RecordStep(uses, results)
	}
	for i := 0; i < 5; i++ {
		uses, results := makeStep("B", "echo b", "b")
		d.RecordStep(uses, results)
	}
	if d.ShouldAbort() {
		t.Errorf("5+5 split shouldn't trip — neither signature exceeds 5")
	}

	// One more A: now window holds [A×4, B×5, A×1] → counts A=5, B=5.
	// Still under threshold (>5).
	uses, results := makeStep("A", "echo a", "a")
	d.RecordStep(uses, results)
	if d.ShouldAbort() {
		t.Errorf("after sliding to 5/5/5/(no excess) shouldn't trip")
	}

	// Push more A's through; eventually A count > 5 within the window.
	for i := 0; i < 5; i++ {
		uses, results := makeStep("A", "echo a", "a")
		d.RecordStep(uses, results)
	}
	if !d.ShouldAbort() {
		t.Fatal("after enough A pushes the window should trip")
	}
}

// TestLoopDetector_SignatureSurvivesFalseProgress — RecordProgress is
// called when a turn ends without tool_use (a "successful" textual
// turn). The loop the user actually hit included assistant text
// summaries between every retry — those should NOT clear the
// sliding-window history, otherwise the detector resets every time
// the model writes a paragraph.
func TestLoopDetector_SignatureSurvivesFalseProgress(t *testing.T) {
	d := NewLoopDetector()
	for i := 0; i < 5; i++ {
		uses, results := makeStep("p", "ping -c 1 host.invalid",
			"ping: host.invalid: Name or service not known")
		d.RecordStep(uses, results)
		d.RecordProgress() // model wrote a recap between each retry
	}
	// 5 identical signatures so far, not enough to trip.
	if d.ShouldAbort() {
		t.Fatal("trip too early")
	}
	uses, results := makeStep("p", "ping -c 1 host.invalid",
		"ping: host.invalid: Name or service not known")
	d.RecordStep(uses, results)
	if !d.ShouldAbort() {
		t.Fatal("RecordProgress wiped signature window — bug; the detector should " +
			"survive textual recaps between retries")
	}
}

// TestStepSignature_StableOrderingAcrossInputKeys — go map iteration
// order is randomized; without sorted-key marshaling, two equivalent
// inputs would produce different SHA-256s and the detector would miss
// real loops. Pins the stable ordering.
func TestStepSignature_StableOrderingAcrossInputKeys(t *testing.T) {
	use1 := provider.ContentBlock{
		Type: "tool_use", ToolUseID: "x", ToolName: "Edit",
		ToolInput: map[string]any{"file": "/tmp/a", "old": "x", "new": "y"},
	}
	use2 := provider.ContentBlock{
		Type: "tool_use", ToolUseID: "x", ToolName: "Edit",
		ToolInput: map[string]any{"new": "y", "file": "/tmp/a", "old": "x"},
	}
	res := []provider.ContentBlock{{Type: "tool_result", ToolUseID: "x", ToolResult: "ok"}}

	a := stepSignature([]provider.ContentBlock{use1}, res)
	b := stepSignature([]provider.ContentBlock{use2}, res)
	if a == "" || a != b {
		t.Errorf("signature should be order-stable; a=%q b=%q", a, b)
	}
}

// TestLoopDetector_AbortReasonPriority — when both the global counter
// and the signature window have tripped, AbortReason returns the more
// specific signature kind so the loop's exit message is actionable.
func TestLoopDetector_AbortReasonPriority(t *testing.T) {
	d := NewLoopDetector()
	d.GlobalThreshold = 5

	for i := 0; i < 6; i++ {
		uses, results := makeStep("dup", "id", "uid=501(...)")
		d.RecordStep(uses, results)
		d.Record("Bash", map[string]any{"command": "id"}) // bumps globalCount
	}
	if !d.ShouldAbort() {
		t.Fatal("expected ShouldAbort = true")
	}
	if d.AbortReason() != LoopSignatureRepeat {
		t.Errorf("AbortReason should prefer signature kind over global; got %q",
			d.AbortReason())
	}
}

// TestLoopDetector_AbortMessageFormat — sanity check that the message
// emitted on critical fire mentions both window size and repeat count
// (helps users debug "why did it stop?").
func TestLoopDetector_AbortMessageFormat(t *testing.T) {
	d := NewLoopDetector()
	var msg string
	d.OnCritical(func(k LoopDetectorKind, _ int, m string) {
		if k == LoopSignatureRepeat {
			msg = m
		}
	})
	for i := 0; i < 6; i++ {
		uses, results := makeStep("x", "echo same", "same output")
		d.RecordStep(uses, results)
	}
	if !strings.Contains(msg, "window of") {
		t.Errorf("message should mention the window size; got %q", msg)
	}
	if !strings.Contains(msg, "repeated") {
		t.Errorf("message should mention repetition count; got %q", msg)
	}
}
