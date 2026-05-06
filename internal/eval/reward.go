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

	default:
		out.Note = fmt.Sprintf("unknown assertion type %q", a.Type)
	}
	return out
}
