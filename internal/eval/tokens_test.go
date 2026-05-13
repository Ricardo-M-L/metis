package eval

// tokens_test.go — covers the per-run token-spend plumbing added
// 2026-05-13. End-to-end the chain is:
//
//   cmd/metis/main.go::cmdRun  → prints [metrics] line on LoopDone
//      ↓
//   internal/eval/runner.go::scrapeMetrics → fills RunResult.Tokens
//      ↓
//   internal/eval/reward.go::AssertMaxInputTokens  → scores it
//
// These tests pin the middle two stages with synthetic stderr; the
// metis-side emission is covered by main.go's existing run-mode tests.

import (
	"strings"
	"testing"
)

func TestScrapeMetrics_HappyPath(t *testing.T) {
	stderr := "[info] starting\n" +
		"[tool] Read\n" +
		"[metrics] tokens.in=1234 tokens.out=56 tokens.cache_read=8000 tokens.cache_create=200 duration_ms=4123\n"
	got := scrapeMetrics(stderr)
	want := TokenStats{Input: 1234, Output: 56, CacheReadInput: 8000, CacheCreate: 200}
	if got != want {
		t.Errorf("scrapeMetrics() = %+v; want %+v", got, want)
	}
	if total := got.TotalBilledInput(); total != 9434 {
		t.Errorf("TotalBilledInput() = %d; want 9434 (fresh+cache_read+cache_create)", total)
	}
}

func TestScrapeMetrics_MissingLineReturnsZero(t *testing.T) {
	stderr := "[info] some run that died before LoopDone\n"
	got := scrapeMetrics(stderr)
	if got != (TokenStats{}) {
		t.Errorf("missing metrics line should return zero TokenStats; got %+v", got)
	}
}

func TestScrapeMetrics_IgnoresOldBinaryNoDurationSuffix(t *testing.T) {
	// duration_ms trailing field is optional — the regex anchors only on
	// the four tokens.* fields so an older metis without duration still
	// scrapes cleanly.
	stderr := "[metrics] tokens.in=100 tokens.out=20 tokens.cache_read=0 tokens.cache_create=0\n"
	got := scrapeMetrics(stderr)
	if got != (TokenStats{Input: 100, Output: 20}) {
		t.Errorf("missing duration_ms should not break scrape; got %+v", got)
	}
}

func TestComputeReward_MaxInputTokens_PassesWhenUnderBudget(t *testing.T) {
	s := Scenario{
		ID: "tok-budget",
		Assertions: []Assertion{
			{Type: AssertMaxInputTokens, MaxTokens: 5000, Weight: 1},
		},
	}
	r := RunResult{ScenarioID: "tok-budget",
		Response: "done",
		Tokens:   TokenStats{Input: 100, CacheReadInput: 2000, CacheCreate: 200, Output: 50},
	}
	score := ComputeReward(s, r)
	if !score.Passed {
		t.Errorf("under-budget run should pass; got %+v", score)
	}
}

func TestComputeReward_MaxInputTokens_FailsWhenOverBudget(t *testing.T) {
	s := Scenario{
		ID: "tok-budget",
		Assertions: []Assertion{
			{Type: AssertMaxInputTokens, MaxTokens: 1000, Weight: 1},
		},
	}
	r := RunResult{ScenarioID: "tok-budget",
		Response: "done",
		// 600 + 500 + 100 = 1200 > 1000 → fail
		Tokens: TokenStats{Input: 600, CacheReadInput: 500, CacheCreate: 100, Output: 50},
	}
	score := ComputeReward(s, r)
	if score.Passed {
		t.Errorf("over-budget run should fail; got %+v", score)
	}
	// The note should explain WHY for the diff viewer.
	if len(score.Breakdown) != 1 || !strings.Contains(score.Breakdown[0].Note, "exceeds 1000") {
		t.Errorf("breakdown should mention the threshold; got %+v", score.Breakdown)
	}
}

func TestComputeReward_MaxOutputTokens_FailsWhenTooChatty(t *testing.T) {
	s := Scenario{
		ID: "chatty",
		Assertions: []Assertion{
			{Type: AssertMaxOutputTokens, MaxTokens: 100, Weight: 1},
		},
	}
	r := RunResult{ScenarioID: "chatty",
		Response: "long-winded response",
		Tokens:   TokenStats{Input: 100, Output: 500},
	}
	score := ComputeReward(s, r)
	if score.Passed {
		t.Errorf("chatty run should fail max_output_tokens; got %+v", score)
	}
}

func TestComputeReward_MaxTokens_NeutralWhenNoMetricsLine(t *testing.T) {
	// If metis didn't emit the metrics line (older binary), the assertion
	// must not falsely fail. Pass with a note explaining the situation.
	s := Scenario{
		ID: "tok-budget",
		Assertions: []Assertion{
			{Type: AssertMaxInputTokens, MaxTokens: 10, Weight: 1},
		},
	}
	r := RunResult{ScenarioID: "tok-budget",
		Response: "ok",
		// All token fields zero — indistinguishable from "no metrics line"
		Tokens: TokenStats{},
	}
	score := ComputeReward(s, r)
	if !score.Passed {
		t.Errorf("missing metrics should not fail the assertion; got %+v", score)
	}
	if len(score.Breakdown) != 1 || !strings.Contains(score.Breakdown[0].Note, "no metrics line") {
		t.Errorf("breakdown should explain neutral pass; got %+v", score.Breakdown)
	}
}

func TestAtoiSafe_ParsesPositiveIntegers(t *testing.T) {
	cases := map[string]int{
		"0":     0,
		"1":     1,
		"123":   123,
		"99999": 99999,
		"":      0, // empty → 0
		"abc":   0, // non-digit → 0
		"12abc": 0, // contains non-digit → 0
		"-5":    0, // negatives not supported (token counts are non-negative)
	}
	for in, want := range cases {
		if got := atoiSafe(in); got != want {
			t.Errorf("atoiSafe(%q) = %d; want %d", in, got, want)
		}
	}
}
