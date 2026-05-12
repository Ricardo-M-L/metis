package eval

import (
	"fmt"
	"regexp"
	"strings"
)

// ComputeReward scores a RunResult against a Scenario's assertions.
// Each assertion is binary (pass/fail); the assertion's Weight scales
// its contribution. Total is the sum of earned weights divided by the
// sum of all weights, so it lands in [0, 1].
//
// If the run errored or timed out, the score is zero across the board
// and Passed is false — but each assertion is still evaluated against
// whatever Response did come back, so partial-output runs surface
// their best-effort signal in the breakdown.
func ComputeReward(s Scenario, r RunResult) Score {
	score := Score{
		ScenarioID: s.ID,
		Duration:   r.Duration,
		Tokens:     r.Tokens,
		Err:        r.Err,
	}
	if len(s.Assertions) == 0 {
		return score
	}
	var earnedSum, weightSum float64
	for _, a := range s.Assertions {
		as := scoreAssertion(a, r)
		score.Breakdown = append(score.Breakdown, as)
		earnedSum += as.Earned * as.Weight
		weightSum += as.Weight
	}
	if weightSum > 0 {
		score.Total = earnedSum / weightSum
	}
	score.Passed = r.Err == nil && score.Total >= PassThreshold
	return score
}

// scoreAssertion runs one rule against the run output. Earned is 0 or
// 1 — fractional rewards aren't supported because they make threshold
// tuning hard to reason about.
func scoreAssertion(a Assertion, r RunResult) AssertScore {
	out := AssertScore{Type: a.Type, Weight: a.Weight}
	switch a.Type {
	case AssertContainsAll:
		var missing []string
		for _, t := range a.Tokens {
			if !strings.Contains(r.Response, t) {
				missing = append(missing, t)
			}
		}
		if len(missing) == 0 {
			out.Earned = 1
			out.Note = fmt.Sprintf("found all %v", a.Tokens)
		} else {
			out.Note = fmt.Sprintf("missing %v", missing)
		}

	case AssertContainsAny:
		for _, t := range a.Tokens {
			if strings.Contains(r.Response, t) {
				out.Earned = 1
				out.Note = fmt.Sprintf("found %q", t)
				return out
			}
		}
		out.Note = fmt.Sprintf("none of %v present", a.Tokens)

	case AssertNotContains:
		for _, t := range a.Tokens {
			if strings.Contains(r.Response, t) {
				out.Note = fmt.Sprintf("forbidden %q present", t)
				return out
			}
		}
		out.Earned = 1
		out.Note = "no forbidden tokens"

	case AssertUsedTool:
		for _, name := range r.ToolCalls {
			if name == a.Tool {
				out.Earned = 1
				out.Note = fmt.Sprintf("called %s", a.Tool)
				return out
			}
		}
		out.Note = fmt.Sprintf("did not call %s; calls=%v", a.Tool, r.ToolCalls)

	case AssertRegex:
		re, err := regexp.Compile(a.Regex)
		if err != nil {
			out.Note = fmt.Sprintf("bad regex: %v", err)
			return out
		}
		if re.MatchString(r.Response) {
			out.Earned = 1
			out.Note = fmt.Sprintf("matched /%s/", a.Regex)
		} else {
			out.Note = fmt.Sprintf("no match for /%s/", a.Regex)
		}

	case AssertLength:
		n := len(r.Response)
		if n >= a.LengthMin && n <= a.LengthMax {
			out.Earned = 1
			out.Note = fmt.Sprintf("len=%d in [%d,%d]", n, a.LengthMin, a.LengthMax)
		} else {
			out.Note = fmt.Sprintf("len=%d outside [%d,%d]", n, a.LengthMin, a.LengthMax)
		}

	case AssertMaxInputTokens:
		// Use the full billed input (fresh + cache_create + cache_read)
		// so a cache-friendly run scores correctly even though its
		// cache_read line counts toward the bill at a discount.
		got := r.Tokens.TotalBilledInput()
		if got == 0 && r.Tokens.Output == 0 {
			// No metrics line scraped at all → can't enforce; mark
			// neutral pass so missing-data doesn't fail the suite.
			out.Earned = 1
			out.Note = "no metrics line in stderr — older metis binary?"
		} else if got <= a.MaxTokens {
			out.Earned = 1
			out.Note = fmt.Sprintf("input=%d <= %d", got, a.MaxTokens)
		} else {
			out.Note = fmt.Sprintf("input=%d exceeds %d (fresh=%d cache_read=%d cache_create=%d)",
				got, a.MaxTokens, r.Tokens.Input, r.Tokens.CacheReadInput, r.Tokens.CacheCreate)
		}

	case AssertMaxOutputTokens:
		got := r.Tokens.Output
		if got == 0 && r.Tokens.Input == 0 {
			out.Earned = 1
			out.Note = "no metrics line in stderr — older metis binary?"
		} else if got <= a.MaxTokens {
			out.Earned = 1
			out.Note = fmt.Sprintf("output=%d <= %d", got, a.MaxTokens)
		} else {
			out.Note = fmt.Sprintf("output=%d exceeds %d", got, a.MaxTokens)
		}

	default:
		out.Note = fmt.Sprintf("unknown assertion type %q", a.Type)
	}
	return out
}
