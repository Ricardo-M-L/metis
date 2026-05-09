package agent

// compact_tier_e2e_test.go — end-to-end activation proof for SUMMARY #6.
// The unit test in compact_tier_test.go covers tier selection in
// isolation; the integration test in
// internal/runtime/agent_loop_tier_test.go covers BuildAgentLoop wiring.
// This file closes the loop: after applying a tier, the *actual Snip
// behavior* on real messages reflects the tier's char cap.
//
// Why this matters: theoretically, applyTier could set the field but
// Snip might read a stale copy via some cached path. By running Snip
// with a 1500-char tool_result through both a 16k-tier Compactor and a
// 200k-tier Compactor and comparing the truncation results, we prove
// the tier's effect is visible at the very end of the pipeline (the
// LLM-facing message slice).

import (
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/llm"
)

// makeBigToolResultMsgs returns a message slice with a single
// snippable tool_result (size = bytes) plus enough framing for
// ProtectFirst/ProtectLast to leave it in the snippable middle.
func makeBigToolResultMsgs(bytes int) []llm.Message {
	long := strings.Repeat("X", bytes)
	return []llm.Message{
		msg(llm.RoleUser, "seed"),        // ProtectFirst=1 → kept
		toolUseMsg("t1", "Bash"),         // snippable middle
		toolResultMsg("t1", long),        // snippable middle — TARGET
		toolUseMsg("t2", "Read"),         // snippable middle
		toolResultMsg("t2", "small ok"),  // snippable middle (passes)
		msg(llm.RoleUser, "tail-1"),      // ProtectLast=3 → kept
		msg(llm.RoleAssistant, "tail-2"), // ProtectLast=3 → kept
		msg(llm.RoleUser, "tail-3"),      // ProtectLast=3 → kept
	}
}

// TestSnipE2E_TierFor16kCutsAt200 — the painful-window case. After
// ApplyWindowTier(16_000) the Snip cap is 200 chars; a 1500-char
// tool_result MUST come back ≤ 200+marker chars. This is the proof
// that the tier knob actually changes the LLM-facing prompt, not
// just an internal field nobody reads.
func TestSnipE2E_TierFor16kCutsAt200(t *testing.T) {
	c := newCompactorFor(nil)
	c.ApplyWindowTier(16_000)
	if c.SnipMaxToolResultChars != 200 {
		t.Fatalf("precondition: tier-16k should set SnipMaxToolResultChars=200; got %d",
			c.SnipMaxToolResultChars)
	}
	out := c.Snip(makeBigToolResultMsgs(1500))
	got := out[2].Content[0].ToolResult
	// Must contain the marker AND head must be exactly 200 X's.
	if !strings.Contains(got, "[snipped:") {
		t.Errorf("Snip should have run on the 1500-char block under tier-16k; got result without marker")
	}
	if !strings.HasPrefix(got, strings.Repeat("X", 200)) {
		t.Errorf("16k tier should keep first 200 chars verbatim; got prefix %q...", got[:min(50, len(got))])
	}
	// Sanity: the marker reports the omitted count truthfully.
	if !strings.Contains(got, "1300 chars omitted") {
		t.Errorf("marker should say 1300 chars (1500-200) omitted; got %q",
			got[len(got)-80:])
	}
}

// TestSnipE2E_TierFor200kKeepsMore — the symmetric case: under
// tier-200k the cap is 3000, so a 1500-char tool_result is BELOW the
// threshold and must pass through untouched. This pins that the
// "loosen on big windows" half of the tier table actually matters.
func TestSnipE2E_TierFor200kKeepsMore(t *testing.T) {
	c := newCompactorFor(nil)
	c.ApplyWindowTier(200_000)
	if c.SnipMaxToolResultChars != 3000 {
		t.Fatalf("precondition: tier-200k should set SnipMaxToolResultChars=3000; got %d",
			c.SnipMaxToolResultChars)
	}
	out := c.Snip(makeBigToolResultMsgs(1500))
	got := out[2].Content[0].ToolResult
	if len(got) != 1500 || strings.Contains(got, "[snipped:") {
		t.Errorf("1500-char block should pass UNTOUCHED under tier-200k (cap 3000); got len=%d, marker=%v",
			len(got), strings.Contains(got, "[snipped:"))
	}
}

// TestSnipE2E_TierContrastDifferent — the same input through two
// different tiers must produce two different outputs. This is the
// strongest single-test proof that the tier knob is on the live path:
// without ApplyWindowTier the two compactors share defaults and the
// outputs would be identical.
func TestSnipE2E_TierContrastDifferent(t *testing.T) {
	c16 := newCompactorFor(nil)
	c16.ApplyWindowTier(16_000)
	c200 := newCompactorFor(nil)
	c200.ApplyWindowTier(200_000)

	in := makeBigToolResultMsgs(1500)
	got16 := c16.Snip(in)[2].Content[0].ToolResult
	got200 := c200.Snip(in)[2].Content[0].ToolResult

	if got16 == got200 {
		t.Fatal("16k tier and 200k tier produced identical Snip output — tier knob is NOT on the live path")
	}
	if len(got16) >= len(got200) {
		t.Errorf("16k tier output should be SHORTER than 200k tier output; got 16k=%d, 200k=%d",
			len(got16), len(got200))
	}
}

// TestSnipE2E_RepeatedSnipIsIdempotent — once Snip has tightened a
// tool_result, running it again under the same tier should be a
// no-op. Otherwise long sessions would re-mark the marker repeatedly,
// each time stripping another 800 chars.
func TestSnipE2E_RepeatedSnipIsIdempotent(t *testing.T) {
	c := newCompactorFor(nil)
	c.ApplyWindowTier(16_000)
	in := makeBigToolResultMsgs(1500)
	once := c.Snip(in)
	twice := c.Snip(once)
	if once[2].Content[0].ToolResult != twice[2].Content[0].ToolResult {
		t.Errorf("Snip not idempotent under repeat — once vs twice differ in length: %d vs %d",
			len(once[2].Content[0].ToolResult), len(twice[2].Content[0].ToolResult))
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
