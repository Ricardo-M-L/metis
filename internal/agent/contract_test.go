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
	// Loop-parameterised on contractAgentThreshold (2026-05-21:
	// bumped 2 → 10, so the old "1 dispatch then 1 more" fixture
	// no longer matched). After (threshold-1) dispatches the
	// tracker must still report not-met; the next one tips it over.
	for i := 0; i < contractAgentThreshold-1; i++ {
		ct.observeToolUses([]llm.ContentBlock{makeToolUse("Agent", map[string]any{"subagent_type": "plan"})})
	}
	if ct.thresholdMet() {
		t.Fatalf("threshold should not yet be met at %d dispatches (threshold=%d)", ct.agentDispatches, contractAgentThreshold)
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
	// Cross threshold via Agent dispatches, one of which IS verify.
	// Loop-parameterised on contractAgentThreshold (2026-05-21 bump:
	// 2 → 10; the old fixture's "1 plan + 1 verify" no longer meets
	// the threshold).
	uses := []llm.ContentBlock{
		makeToolUse("Agent", map[string]any{"subagent_type": "verify"}),
	}
	for i := 0; i < contractAgentThreshold-1; i++ {
		uses = append(uses, makeToolUse("Agent", map[string]any{"subagent_type": "plan"}))
	}
	ct.observeToolUses(uses)
	if !ct.thresholdMet() {
		t.Fatalf("test premise: threshold should be met (have %d dispatches, threshold=%d)", ct.agentDispatches, contractAgentThreshold)
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

// ----- Phase B verdict-gate tests (2026-05-19) -----

// makeToolResult builds a tool_result block to pair with a tool_use.
func makeToolResult(body string) llm.ContentBlock {
	return llm.ContentBlock{Type: "tool_result", ToolResult: body}
}

func TestExtractVerdict_AllShapes(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{"pass at end", "did some work\nVERDICT: PASS\n", "PASS"},
		{"fail at end", "found bugs\nVERDICT: FAIL — 2 tests failed\n", "FAIL"},
		{"partial at end", "covered half\nVERDICT: PARTIAL — only ran unit tests\n", "PARTIAL"},
		{"no verdict line", "subagent forgot to summarize", "MISSING"},
		{"verdict mid-body, then pass at end", "VERDICT: FAIL\nactually wait, fixed it\nVERDICT: PASS\n", "PASS"},
		{"unknown verdict word", "VERDICT: UNCLEAR\n", "MISSING"},
		{"verdict with tab", "VERDICT:\tPASS\n", "PASS"},
		{"empty body", "", "MISSING"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := extractVerdict(c.body); got != c.want {
				t.Errorf("extractVerdict(%q) = %q, want %q", c.body, got, c.want)
			}
		})
	}
}

func TestObserveToolResults_SetsVerdictFromVerifyOnly(t *testing.T) {
	var ct contractTracker
	uses := []llm.ContentBlock{
		makeToolUse("Agent", map[string]any{"subagent_type": "plan"}), // not verify
		makeToolUse("Agent", map[string]any{"subagent_type": "verify"}),
	}
	results := []llm.ContentBlock{
		makeToolResult("plan report\nVERDICT: PASS\n"), // verdict in non-verify result must be ignored
		makeToolResult("verify report\nVERDICT: FAIL — broken\n"),
	}
	ct.observeToolResults(uses, results)
	if ct.lastVerifyVerdict != "FAIL" {
		t.Errorf("lastVerifyVerdict = %q, want FAIL (verify only)", ct.lastVerifyVerdict)
	}
}

func TestGateEnd_VerdictGate_FailHolds(t *testing.T) {
	var ct contractTracker
	// Threshold via writes + verify dispatched.
	for i := 0; i < contractWriteThreshold; i++ {
		ct.observeToolUses([]llm.ContentBlock{makeToolUse("Write", nil)})
	}
	verifyUse := makeToolUse("Agent", map[string]any{"subagent_type": "verify"})
	ct.observeToolUses([]llm.ContentBlock{verifyUse})
	ct.observeToolResults([]llm.ContentBlock{verifyUse}, []llm.ContentBlock{
		makeToolResult("findings\nVERDICT: FAIL — 3 tests failed\n"),
	})
	body := ct.shouldGateEnd("done")
	if body == "" {
		t.Fatal("FAIL verdict must hold the end gate; got empty")
	}
	if !strings.Contains(body, "VERDICT GATE") || !strings.Contains(body, "FAIL") {
		t.Errorf("gate message should mention VERDICT GATE + the verdict; got: %q", body)
	}
}

func TestGateEnd_VerdictGate_PartialHolds(t *testing.T) {
	var ct contractTracker
	for i := 0; i < contractWriteThreshold; i++ {
		ct.observeToolUses([]llm.ContentBlock{makeToolUse("Write", nil)})
	}
	verifyUse := makeToolUse("Agent", map[string]any{"subagent_type": "verify"})
	ct.observeToolUses([]llm.ContentBlock{verifyUse})
	ct.observeToolResults([]llm.ContentBlock{verifyUse}, []llm.ContentBlock{
		makeToolResult("VERDICT: PARTIAL — only ran half the tests\n"),
	})
	if body := ct.shouldGateEnd("done"); body == "" {
		t.Error("PARTIAL verdict must hold the end gate")
	}
}

func TestGateEnd_VerdictGate_MissingHolds(t *testing.T) {
	var ct contractTracker
	for i := 0; i < contractWriteThreshold; i++ {
		ct.observeToolUses([]llm.ContentBlock{makeToolUse("Write", nil)})
	}
	verifyUse := makeToolUse("Agent", map[string]any{"subagent_type": "verify"})
	ct.observeToolUses([]llm.ContentBlock{verifyUse})
	ct.observeToolResults([]llm.ContentBlock{verifyUse}, []llm.ContentBlock{
		makeToolResult("did some checks but forgot the verdict line"),
	})
	if body := ct.shouldGateEnd("done"); body == "" {
		t.Error("MISSING verdict must hold the end gate (verifier broke protocol)")
	}
}

func TestGateEnd_VerdictGate_PassReleases(t *testing.T) {
	var ct contractTracker
	for i := 0; i < contractWriteThreshold; i++ {
		ct.observeToolUses([]llm.ContentBlock{makeToolUse("Write", nil)})
	}
	verifyUse := makeToolUse("Agent", map[string]any{"subagent_type": "verify"})
	ct.observeToolUses([]llm.ContentBlock{verifyUse})
	ct.observeToolResults([]llm.ContentBlock{verifyUse}, []llm.ContentBlock{
		makeToolResult("all good\nVERDICT: PASS\n"),
	})
	if body := ct.shouldGateEnd("done"); body != "" {
		t.Errorf("PASS verdict should release; got: %q", body)
	}
}

func TestGateEnd_VerdictGate_OverrideReleases(t *testing.T) {
	var ct contractTracker
	for i := 0; i < contractWriteThreshold; i++ {
		ct.observeToolUses([]llm.ContentBlock{makeToolUse("Write", nil)})
	}
	verifyUse := makeToolUse("Agent", map[string]any{"subagent_type": "verify"})
	ct.observeToolUses([]llm.ContentBlock{verifyUse})
	ct.observeToolResults([]llm.ContentBlock{verifyUse}, []llm.ContentBlock{
		makeToolResult("VERDICT: FAIL — test was wrong, not the code\n"),
	})
	override := "OVERRIDE CONTRACT: verifier mis-scoped, test expects old API"
	if body := ct.shouldGateEnd(override); body != "" {
		t.Errorf("OVERRIDE CONTRACT phrase must release even on FAIL verdict; got: %q", body)
	}
}

func TestGateEnd_VerdictGate_CapsAtTwoAttempts(t *testing.T) {
	var ct contractTracker
	for i := 0; i < contractWriteThreshold; i++ {
		ct.observeToolUses([]llm.ContentBlock{makeToolUse("Write", nil)})
	}
	verifyUse := makeToolUse("Agent", map[string]any{"subagent_type": "verify"})
	ct.observeToolUses([]llm.ContentBlock{verifyUse})
	ct.observeToolResults([]llm.ContentBlock{verifyUse}, []llm.ContentBlock{
		makeToolResult("VERDICT: FAIL\n"),
	})
	// First two end-tries must hold.
	for i := 1; i <= contractMaxGateAttempts; i++ {
		if body := ct.shouldGateEnd("done"); body == "" {
			t.Errorf("attempt %d should hold", i)
		}
	}
	// Third attempt releases (cap exhausted, don't burn tokens).
	if body := ct.shouldGateEnd("done"); body != "" {
		t.Errorf("after %d attempts the verdict gate must release; got: %q", contractMaxGateAttempts, body)
	}
}
