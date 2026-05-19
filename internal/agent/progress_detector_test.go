package agent

import (
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/llm"
)

// uses builds a tool_use ContentBlock list of length n, all with
// the given tool name. Helper to keep test setups tight.
func uses(name string, n int) []llm.ContentBlock {
	out := make([]llm.ContentBlock, n)
	for i := range out {
		out[i] = llm.ContentBlock{Type: "tool_use", ToolName: name}
	}
	return out
}

// resultsOf builds matching tool_result blocks. Each body slot
// may be empty (no useful output) or short/long. isError flips
// the IsError flag per index.
func resultsOf(bodies []string, isError []bool) []llm.ContentBlock {
	out := make([]llm.ContentBlock, len(bodies))
	for i, b := range bodies {
		err := false
		if i < len(isError) {
			err = isError[i]
		}
		out[i] = llm.ContentBlock{Type: "tool_result", ToolResult: b, IsError: err}
	}
	return out
}

func TestProgressDetector_LowBytesTripsConsecutiveCounter(t *testing.T) {
	p := newProgressDetector()
	for i := 0; i < 3; i++ {
		p.RecordIter(uses("Read", 1), resultsOf([]string{"tiny"}, nil))
	}
	if !p.IsDiminishing() {
		t.Errorf("3 consecutive sub-threshold iters should trip; consecutive=%d", p.ConsecutiveLow())
	}
}

func TestProgressDetector_LargeOutputResets(t *testing.T) {
	p := newProgressDetector()
	p.RecordIter(uses("Read", 1), resultsOf([]string{"tiny"}, nil))
	p.RecordIter(uses("Read", 1), resultsOf([]string{"tiny"}, nil))
	// Big output snaps the counter back to 0.
	p.RecordIter(uses("Read", 1), resultsOf([]string{strings.Repeat("x", 1024)}, nil))
	if p.ConsecutiveLow() != 0 {
		t.Errorf("large output should reset counter; got %d", p.ConsecutiveLow())
	}
	if p.IsDiminishing() {
		t.Error("should not be diminishing after a high-bytes iter")
	}
}

func TestProgressDetector_ErrorResultsCountAsLow(t *testing.T) {
	// All results errored out — the model isn't getting useful
	// info back even if the byte counts are non-trivial.
	p := newProgressDetector()
	bigErr := strings.Repeat("E", 2048)
	for i := 0; i < 3; i++ {
		p.RecordIter(uses("Bash", 1), resultsOf([]string{bigErr}, []bool{true}))
	}
	if !p.IsDiminishing() {
		t.Error("3 iters of error-only results should trip diminishing — error bytes don't count")
	}
}

func TestProgressDetector_WritesGetFullCredit(t *testing.T) {
	// A successful Write returns "wrote N bytes" (~50 chars) — far
	// under the threshold. But the model writing files IS useful
	// progress. We award full credit for mutating tools so a
	// model in "finishing work" mode doesn't trip the detector.
	p := newProgressDetector()
	for i := 0; i < 5; i++ {
		p.RecordIter(uses("Write", 1), resultsOf([]string{"wrote 50 bytes"}, nil))
	}
	if p.IsDiminishing() {
		t.Errorf("Write tool should not count as low-progress; consecutive=%d", p.ConsecutiveLow())
	}
}

// TestProgressDetector_SuccessfulBashGetsFullCredit pins the 2026-05-19
// bench6 iter6 fix: a successful `go test ./...` emits only "ok pkg
// 0.123s" (~25 chars) per package, well under the threshold. The model
// running 3 successful test commands in a row past the 75% nudge used
// to trip diminishing-returns and abort — RIGHT after saying "All tests
// pass." Successful Bash (IsError=false) now gets full credit, same as
// Write/Edit.
func TestProgressDetector_SuccessfulBashGetsFullCredit(t *testing.T) {
	p := newProgressDetector()
	for i := 0; i < 5; i++ {
		p.RecordIter(uses("Bash", 1), resultsOf([]string{"ok\tcalc\t0.123s\n"}, nil))
	}
	if p.IsDiminishing() {
		t.Errorf("successful Bash with short output should not trip; consecutive=%d", p.ConsecutiveLow())
	}
}

// TestProgressDetector_FailedBashStillCountsLow — counterpart safety:
// a Bash that errored out (IsError=true) IS low-progress, just like
// any other errored result. Don't accidentally credit failed test runs
// via the new Bash branch.
func TestProgressDetector_FailedBashStillCountsLow(t *testing.T) {
	p := newProgressDetector()
	for i := 0; i < 3; i++ {
		p.RecordIter(uses("Bash", 1), resultsOf([]string{"--- FAIL: TestX\nFAIL\n"}, []bool{true}))
	}
	if !p.IsDiminishing() {
		t.Errorf("3 failed Bash iters should still trip diminishing-returns; consecutive=%d", p.ConsecutiveLow())
	}
}

func TestProgressDetector_EmptyResultsCountAsLow(t *testing.T) {
	// Model emitted tool_use blocks but executeBatch returned
	// nothing (e.g. all tools failed pre-execution). That's not
	// progress either.
	p := newProgressDetector()
	for i := 0; i < 3; i++ {
		p.RecordIter(uses("Read", 1), nil)
	}
	if !p.IsDiminishing() {
		t.Error("3 empty-result iters should trip")
	}
}

func TestProgressDetector_MixedToolKinds(t *testing.T) {
	// Read + Write in same iter: Read gives some bytes, Write
	// gets full credit. Total should easily clear threshold.
	p := newProgressDetector()
	p.RecordIter(
		[]llm.ContentBlock{{Type: "tool_use", ToolName: "Read"}, {Type: "tool_use", ToolName: "Write"}},
		resultsOf([]string{"file content here is short", "wrote ok"}, nil),
	)
	if p.ConsecutiveLow() != 0 {
		t.Errorf("Write half should rescue mixed iter from low-progress; got %d", p.ConsecutiveLow())
	}
}

func TestProgressDetector_Reset(t *testing.T) {
	p := newProgressDetector()
	p.RecordIter(uses("Read", 1), resultsOf([]string{"tiny"}, nil))
	p.RecordIter(uses("Read", 1), resultsOf([]string{"tiny"}, nil))
	p.Reset()
	if p.ConsecutiveLow() != 0 {
		t.Errorf("Reset should clear counter; got %d", p.ConsecutiveLow())
	}
}

func TestProgressDetector_RequiresThreeNotTwo(t *testing.T) {
	p := newProgressDetector()
	p.RecordIter(uses("Read", 1), resultsOf([]string{"x"}, nil))
	p.RecordIter(uses("Read", 1), resultsOf([]string{"x"}, nil))
	if p.IsDiminishing() {
		t.Errorf("2 low iters should not trip yet; consecutive=%d", p.ConsecutiveLow())
	}
}
