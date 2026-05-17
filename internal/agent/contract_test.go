package agent

// contract_test.go — pin the dispatch-contract trigger logic so a
// future refactor that "looks correct" can't quietly weaken the
// gate. Each test exercises one specific edge case the design
// promises to handle (Round-4 failure mode, override phrase,
// gate-attempt cap, env disable, etc.).

import (
	"os"
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/llm"
)

// makeToolUse — tiny helper. Tests construct tool_use blocks by
// hand; this keeps the table-style tests below readable.
func makeToolUse(name string, input map[string]any) llm.ContentBlock {
	return llm.ContentBlock{Type: "tool_use", ToolName: name, ToolInput: input}
}

func TestContract_ThresholdViaWrites(t *testing.T) {
	var ct contractTracker
	for i := 0; i < contractWriteThreshold-1; i++ {
		ct.observeToolUses([]llm.ContentBlock{makeToolUse("Write", nil)})
	}
	if ct.thresholdMet() {
		t.Fatalf("threshold should not yet be met at %d writes", ct.mainWrites)
	}
	ct.observeToolUses([]llm.ContentBlock{makeToolUse("Write", nil)})
	if !ct.thresholdMet() {
		t.Errorf("threshold expected at %d writes; mainWrites=%d", contractWriteThreshold, ct.mainWrites)
	}
}

func TestContract_ThresholdViaAgentDispatches(t *testing.T) {
	var ct contractTracker
	ct.observeToolUses([]llm.ContentBlock{makeToolUse("Agent", map[string]any{"subagent_type": "plan"})})
	if ct.thresholdMet() {
		t.Fatalf("threshold should not yet be met at %d dispatches", ct.agentDispatches)
	}
	ct.observeToolUses([]llm.ContentBlock{makeToolUse("Agent", map[string]any{"subagent_type": "general"})})
	if !ct.thresholdMet() {
		t.Errorf("threshold expected at %d agent dispatches; got %d", contractAgentThreshold, ct.agentDispatches)
	}
}

func TestContract_VerifyDispatchedFlag(t *testing.T) {
	var ct contractTracker
	ct.observeToolUses([]llm.ContentBlock{
		makeToolUse("Agent", map[string]any{"subagent_type": "plan"}),
		makeToolUse("Agent", map[string]any{"subagent_type": "general"}),
	})
	if ct.verifyDispatched {
		t.Errorf("verifyDispatched should be false for non-verify subagent_types")
	}
	ct.observeToolUses([]llm.ContentBlock{
		makeToolUse("Agent", map[string]any{"subagent_type": "verify"}),
	})
	if !ct.verifyDispatched {
		t.Errorf("verifyDispatched should be true after a verify dispatch")
	}
}

// TestContract_MidTurnReminder_FiresOnceAtThreshold —
// the reminder appears the iteration threshold crosses, not before,
// and never repeats.
func TestContract_MidTurnReminder_FiresOnceAtThreshold(t *testing.T) {
	var ct contractTracker
	// 4 writes — below threshold
	for i := 0; i < contractWriteThreshold-1; i++ {
		ct.observeToolUses([]llm.ContentBlock{makeToolUse("Write", nil)})
	}
	if body := ct.shouldFireMidTurnReminder(); body != "" {
		t.Fatalf("reminder fired below threshold; got: %q", body)
	}
	// 5th write — should fire
	ct.observeToolUses([]llm.ContentBlock{makeToolUse("Write", nil)})
	body := ct.shouldFireMidTurnReminder()
	if body == "" {
		t.Fatalf("reminder should fire at threshold")
	}
	for _, want := range []string{"CONTRACT REMINDER", "subagent_type: \"verify\"", "VERDICT"} {
		if !strings.Contains(body, want) {
			t.Errorf("reminder body missing %q\n---\n%s", want, body)
		}
	}
	// 6th write — must not refire (one-time)
	ct.observeToolUses([]llm.ContentBlock{makeToolUse("Write", nil)})
	if body := ct.shouldFireMidTurnReminder(); body != "" {
		t.Errorf("reminder re-fired after the first hit; got: %q", body)
	}
}

func TestContract_MidTurnReminder_QuietIfVerifyAlreadyDispatched(t *testing.T) {
	var ct contractTracker
	// Cross threshold via Agent dispatches, one of which IS verify
	ct.observeToolUses([]llm.ContentBlock{
		makeToolUse("Agent", map[string]any{"subagent_type": "plan"}),
		makeToolUse("Agent", map[string]any{"subagent_type": "verify"}),
	})
	if !ct.thresholdMet() {
		t.Fatalf("test premise: threshold should be met")
	}
	if body := ct.shouldFireMidTurnReminder(); body != "" {
		t.Errorf("reminder should stay quiet when verify already dispatched; got: %q", body)
	}
}

// TestContract_GateEnd_FiresAtThresholdNoVerify —
// the end-of-turn gate produces a non-empty body when work was
// substantial and no verify dispatched.
func TestContract_GateEnd_FiresAtThresholdNoVerify(t *testing.T) {
	var ct contractTracker
	for i := 0; i < contractWriteThreshold; i++ {
		ct.observeToolUses([]llm.ContentBlock{makeToolUse("Write", nil)})
	}
	body := ct.shouldGateEnd("All done!")
	if body == "" {
		t.Fatalf("gate should fire on first end-attempt without verify")
	}
	for _, want := range []string{"CONTRACT GATE — HALT", "attempt 1 of 2", "OVERRIDE CONTRACT:", "Agent({subagent_type: \"verify\""} {
		if !strings.Contains(body, want) {
			t.Errorf("gate body missing %q\n---\n%s", want, body)
		}
	}
}

// TestContract_GateEnd_CapsAtTwoAttempts — third attempt releases
// silently rather than infinite-looping the model into a token
// burn. Each gated end increments attempts; the third end-attempt
// must release.
func TestContract_GateEnd_CapsAtTwoAttempts(t *testing.T) {
	var ct contractTracker
	for i := 0; i < contractWriteThreshold; i++ {
		ct.observeToolUses([]llm.ContentBlock{makeToolUse("Write", nil)})
	}
	if ct.shouldGateEnd("done") == "" {
		t.Fatalf("attempt 1 should fire")
	}
	if ct.shouldGateEnd("done again") == "" {
		t.Fatalf("attempt 2 should fire")
	}
	if body := ct.shouldGateEnd("done a third time"); body != "" {
		t.Errorf("attempt 3 should release silently; got: %q", body)
	}
}

func TestContract_GateEnd_QuietBelowThreshold(t *testing.T) {
	var ct contractTracker
	// Just 1 write — below threshold
	ct.observeToolUses([]llm.ContentBlock{makeToolUse("Write", nil)})
	if body := ct.shouldGateEnd("done"); body != "" {
		t.Errorf("gate should stay quiet below threshold; got: %q", body)
	}
}

func TestContract_GateEnd_QuietWhenVerifyDispatched(t *testing.T) {
	var ct contractTracker
	for i := 0; i < contractWriteThreshold; i++ {
		ct.observeToolUses([]llm.ContentBlock{makeToolUse("Write", nil)})
	}
	ct.observeToolUses([]llm.ContentBlock{
		makeToolUse("Agent", map[string]any{"subagent_type": "verify"}),
	})
	if body := ct.shouldGateEnd("verified"); body != "" {
		t.Errorf("gate should stay quiet when verify dispatched; got: %q", body)
	}
}

func TestContract_GateEnd_OverridePhraseReleases(t *testing.T) {
	var ct contractTracker
	for i := 0; i < contractWriteThreshold; i++ {
		ct.observeToolUses([]llm.ContentBlock{makeToolUse("Write", nil)})
	}
	const text = "Refactor was pure rename across 5 files, no behavior change.\n" +
		"OVERRIDE CONTRACT: rename-only refactor, no tests apply"
	if body := ct.shouldGateEnd(text); body != "" {
		t.Errorf("override phrase should release the gate; got: %q", body)
	}
	if !ct.wasOverridden(text) {
		t.Errorf("wasOverridden should return true for the same text")
	}
}

func TestContract_EnvDisableBypassesAll(t *testing.T) {
	t.Setenv("METIS_CONTRACT_DISABLE", "1")
	var ct contractTracker
	for i := 0; i < contractWriteThreshold; i++ {
		ct.observeToolUses([]llm.ContentBlock{makeToolUse("Write", nil)})
	}
	if body := ct.shouldFireMidTurnReminder(); body != "" {
		t.Errorf("mid-turn reminder should be disabled by env; got: %q", body)
	}
	if body := ct.shouldGateEnd("done"); body != "" {
		t.Errorf("end gate should be disabled by env; got: %q", body)
	}
}

func TestContract_Reset(t *testing.T) {
	var ct contractTracker
	for i := 0; i < contractWriteThreshold; i++ {
		ct.observeToolUses([]llm.ContentBlock{makeToolUse("Write", nil)})
	}
	ct.shouldFireMidTurnReminder()
	ct.shouldGateEnd("done")
	if ct.mainWrites == 0 || !ct.reminderFired || ct.gateAttempts == 0 {
		t.Fatalf("test premise: state should be non-zero before reset")
	}
	ct.reset()
	if ct.mainWrites != 0 || ct.agentDispatches != 0 || ct.verifyDispatched ||
		ct.reminderFired || ct.gateAttempts != 0 {
		t.Errorf("reset should zero all counters; got %+v", ct)
	}
}

// TestContract_AssistantText_JoinsAllTextBlocks — the override
// detector reads assistant text from a multi-block message; this
// helper must concatenate every text block (and skip tool_use ones)
// otherwise an override placed in the second text block wouldn't
// release the gate.
func TestContract_AssistantText_JoinsAllTextBlocks(t *testing.T) {
	msg := []llm.ContentBlock{
		{Type: "text", Text: "first part"},
		{Type: "tool_use", ToolName: "Bash"},
		{Type: "text", Text: "OVERRIDE CONTRACT: small refactor"},
	}
	got := assistantText(msg)
	for _, want := range []string{"first part", "OVERRIDE CONTRACT: small refactor"} {
		if !strings.Contains(got, want) {
			t.Errorf("assistantText missing %q in result %q", want, got)
		}
	}
}

// TestContract_GateEnd_RespectsEnvFromConstFn — defensive: the
// env check uses os.Getenv directly. If a later refactor caches
// the value at startup, this test would catch the regression.
func TestContract_GateEnd_RespectsEnvFromConstFn(t *testing.T) {
	os.Unsetenv("METIS_CONTRACT_DISABLE")
	if contractDisabled() {
		t.Fatalf("contractDisabled should be false when env unset")
	}
	t.Setenv("METIS_CONTRACT_DISABLE", "1")
	if !contractDisabled() {
		t.Errorf("contractDisabled should be true when env=1")
	}
}
