package eval

import (
	"errors"
	"testing"
	"time"
)

func TestComputeReward_AllPass(t *testing.T) {
	s := Scenario{
		ID: "t1",
		Assertions: []Assertion{
			{Type: AssertContainsAll, Tokens: []string{"hello"}, Weight: 1},
			{Type: AssertUsedTool, Tool: "Read", Weight: 0.5},
		},
	}
	r := RunResult{
		Response:  "hello world",
		ToolCalls: []string{"Read", "Glob"},
	}
	score := ComputeReward(s, r)
	if !score.Passed {
		t.Fatalf("expected pass, got total=%v breakdown=%+v", score.Total, score.Breakdown)
	}
	if score.Total != 1.0 {
		t.Errorf("expected total 1.0, got %v", score.Total)
	}
}

func TestComputeReward_PartialPass(t *testing.T) {
	s := Scenario{
		ID: "t2",
		Assertions: []Assertion{
			{Type: AssertContainsAll, Tokens: []string{"foo"}, Weight: 1},
			{Type: AssertContainsAll, Tokens: []string{"bar"}, Weight: 1}, // miss
		},
	}
	r := RunResult{Response: "only foo here"}
	score := ComputeReward(s, r)
	if score.Total != 0.5 {
		t.Errorf("expected 0.5 with one of two passing, got %v", score.Total)
	}
	if score.Passed {
		t.Error("0.5 < threshold (0.7), should not pass")
	}
}

func TestComputeReward_ErrorBlocksPass(t *testing.T) {
	s := Scenario{ID: "t3", Assertions: []Assertion{
		{Type: AssertContainsAll, Tokens: []string{"x"}, Weight: 1},
	}}
	r := RunResult{Response: "x", Err: errors.New("subprocess timeout")}
	score := ComputeReward(s, r)
	if score.Total != 1.0 {
		t.Errorf("breakdown should still earn the assertion; got %v", score.Total)
	}
	if score.Passed {
		t.Error("Err non-nil must block Passed regardless of total")
	}
}

func TestScoreAssertion_NotContains(t *testing.T) {
	a := Assertion{Type: AssertNotContains, Tokens: []string{"error"}, Weight: 1}
	if as := scoreAssertion(a, RunResult{Response: "all good"}); as.Earned != 1 {
		t.Errorf("clean response should earn 1; got %+v", as)
	}
	if as := scoreAssertion(a, RunResult{Response: "got error"}); as.Earned != 0 {
		t.Errorf("forbidden token present should earn 0; got %+v", as)
	}
}

func TestScoreAssertion_Regex(t *testing.T) {
	a := Assertion{Type: AssertRegex, Regex: `\d{3}`, Weight: 1}
	if as := scoreAssertion(a, RunResult{Response: "version 123"}); as.Earned != 1 {
		t.Errorf("regex match should earn 1; got %+v", as)
	}
	if as := scoreAssertion(a, RunResult{Response: "no nums"}); as.Earned != 0 {
		t.Errorf("no match should earn 0; got %+v", as)
	}
}

func TestScoreAssertion_Length(t *testing.T) {
	a := Assertion{Type: AssertLength, LengthMin: 5, LengthMax: 20, Weight: 1}
	if as := scoreAssertion(a, RunResult{Response: "hello world"}); as.Earned != 1 {
		t.Errorf("11 chars in [5,20] should earn 1; got %+v", as)
	}
	if as := scoreAssertion(a, RunResult{Response: "x"}); as.Earned != 0 {
		t.Errorf("1 char outside [5,20] should earn 0; got %+v", as)
	}
}

func TestScoreAssertion_ContainsAny(t *testing.T) {
	a := Assertion{Type: AssertContainsAny, Tokens: []string{"go", "rust", "py"}, Weight: 1}
	if as := scoreAssertion(a, RunResult{Response: "we love rust"}); as.Earned != 1 {
		t.Errorf("any-of match should earn 1; got %+v", as)
	}
	if as := scoreAssertion(a, RunResult{Response: "totally unrelated"}); as.Earned != 0 {
		t.Errorf("none-of-three should earn 0; got %+v", as)
	}
}

func TestReport_PassRateAndAvg(t *testing.T) {
	rep := Report{
		Scores: []Score{
			{Total: 1.0, Passed: true},
			{Total: 0.5, Passed: false},
			{Total: 0.8, Passed: true},
		},
	}
	if got := rep.PassRate(); got < 0.66 || got > 0.67 {
		t.Errorf("pass rate ~0.667; got %v", got)
	}
	if got := rep.AvgScore(); got < 0.76 || got > 0.77 {
		t.Errorf("avg score ~0.7666; got %v", got)
	}
}

func TestReport_EmptyIsZero(t *testing.T) {
	rep := Report{}
	if rep.PassRate() != 0 || rep.AvgScore() != 0 {
		t.Errorf("empty report must zero out, not panic; got pass=%v avg=%v",
			rep.PassRate(), rep.AvgScore())
	}
}

func TestScrapeToolCalls(t *testing.T) {
	stderr := `2026-05-06T13:00:00Z level=info tool=Read path=/tmp/foo
2026-05-06T13:00:01Z level=info  tool=Glob pattern="*.go"
2026-05-06T13:00:02Z some random log without a tool name
2026-05-06T13:00:03Z level=info  tool="Bash" command=ls`
	got := scrapeToolCalls(stderr)
	want := []string{"Read", "Glob", "Bash"}
	if len(got) != len(want) {
		t.Fatalf("expected %v; got %v", want, got)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("[%d] expected %s; got %s", i, want[i], got[i])
		}
	}
}

func TestRunner_Defaults(t *testing.T) {
	rn := &Runner{}
	if rn.MetisBinary != "" {
		t.Error("zero value should have empty binary; Run() will resolve to 'metis' on PATH")
	}
	// Smoke that GlobalGrace addition compiles
	rn.GlobalGrace = 5 * time.Second
}
