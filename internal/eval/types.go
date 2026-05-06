// Package eval implements an end-to-end evaluation harness for metis.
//
// Inspired by openclaw's qa-lab markdown scenario packs, hermes-agent's
// Atropos compute_reward pattern, and kimi-cli's Terminal-Bench-2 task
// suite. A scenario is a markdown file with YAML front-matter declaring
// id/description/timeout, plus body sections for prompt and reward
// rules. The runner spawns `metis run <prompt>`, captures stdout +
// transcript, then ComputeReward scores it deterministically.
package eval

import "time"

// Scenario is one evaluation case loaded from a markdown file.
type Scenario struct {
	ID          string        // unique slug, also filename stem
	Description string        // one-line summary
	Tags        []string      // smoke / regression / feature / etc
	Timeout     time.Duration // hard cap on the run, default 60s
	Prompt      string        // what to send to `metis run`
	Setup       string        // free-form setup notes (logged, not executed)
	Assertions  []Assertion   // reward rules
	SourcePath  string        // file the scenario came from
}

// Assertion is one rule in the reward computation. The Type drives
// which fields are read; weights are normalized at compute time so
// authors can use any positive numbers.
type Assertion struct {
	Type   AssertionType
	Weight float64

	// ContainsAll / ContainsAny / NotContains: tokens to check in the
	// agent's final stdout response.
	Tokens []string

	// UsedTool: tool name that must appear in the run's tool-call list.
	Tool string

	// Regex: pattern to match against the response (re2 syntax).
	Regex string

	// LengthMin / LengthMax: response character bounds (inclusive).
	LengthMin int
	LengthMax int
}

// AssertionType is the kind of rule. Add new ones in reward.go's
// scoreAssertion switch.
type AssertionType string

const (
	AssertContainsAll AssertionType = "contains_all"
	AssertContainsAny AssertionType = "contains_any"
	AssertNotContains AssertionType = "not_contains"
	AssertUsedTool    AssertionType = "used_tool"
	AssertRegex       AssertionType = "regex"
	AssertLength      AssertionType = "length"
)

// RunResult is what the runner captures from one `metis run` invocation.
type RunResult struct {
	ScenarioID string
	Response   string        // stdout from `metis run` (the final answer)
	ToolCalls  []string      // tool names the agent invoked, in order
	Duration   time.Duration // wall time
	Err        error         // non-nil if the subprocess failed or timed out
	ExitCode   int
}

// Score is the reward outcome for one scenario.
type Score struct {
	ScenarioID string
	Total      float64       // 0.0 — 1.0, normalized
	Passed     bool          // Total >= PassThreshold
	Breakdown  []AssertScore // per-assertion detail
	Duration   time.Duration // copied from RunResult for the report
	Err        error         // copied for the report
}

// AssertScore is one assertion's contribution to a Score.
type AssertScore struct {
	Type   AssertionType
	Weight float64
	Earned float64 // 0.0 or 1.0 (assertions are pass/fail, weight scales)
	Note   string  // human-readable: "found 'metis'" / "missing 'agent'"
}

// PassThreshold is the score above which a scenario counts as passed.
// Set to 0.7 to match common e2e harness conventions.
const PassThreshold = 0.7

// Report aggregates many Score results into a single eval run summary.
type Report struct {
	Started     time.Time
	Finished    time.Time
	MetisBinary string  // path to the metis binary used
	Scores      []Score // one entry per scenario
}

// PassRate returns passes / total in [0, 1]. Empty report → 0.
func (r Report) PassRate() float64 {
	if len(r.Scores) == 0 {
		return 0
	}
	pass := 0
	for _, s := range r.Scores {
		if s.Passed {
			pass++
		}
	}
	return float64(pass) / float64(len(r.Scores))
}

// AvgScore returns the mean Total across all scenarios. Empty → 0.
func (r Report) AvgScore() float64 {
	if len(r.Scores) == 0 {
		return 0
	}
	var sum float64
	for _, s := range r.Scores {
		sum += s.Total
	}
	return sum / float64(len(r.Scores))
}
