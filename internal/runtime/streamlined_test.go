package runtime

import (
	"strings"
	"testing"
)

// TestStreamlinedAccumulator_BasicCounts — round-trip: feed N tool
// names, then Summary() returns the canonical "Searched X, read Y, ..."
// phrasing CC's getToolSummaryText produces.
func TestStreamlinedAccumulator_BasicCounts(t *testing.T) {
	var s StreamlinedAccumulator
	for _, tool := range []string{"Grep", "Grep", "Glob", "Read", "Read", "Bash", "Edit"} {
		s.AccumulateTool(tool)
	}
	got := s.Summary()
	want := "Searched 3 patterns, read 2 files, wrote 1 file, ran 1 command"
	if got != want {
		t.Errorf("summary mismatch:\n got: %q\nwant: %q", got, want)
	}
}

// TestStreamlinedAccumulator_SingularPlural — phrasing must agree with
// count: 1 → singular, N → plural. Bug-prone area; pin all 5 buckets.
func TestStreamlinedAccumulator_SingularPlural(t *testing.T) {
	cases := []struct {
		name string
		tool string
		want string
	}{
		{"single search", "Grep", "Searched 1 pattern"},
		{"single read", "Read", "Read 1 file"},
		{"single write", "Write", "Wrote 1 file"},
		{"single command", "Bash", "Ran 1 command"},
		{"single other", "UnknownTool", "1 other tool"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var s StreamlinedAccumulator
			s.AccumulateTool(tc.tool)
			if got := s.Summary(); got != tc.want {
				t.Errorf("got %q want %q", got, tc.want)
			}
		})
	}
}

// TestStreamlinedAccumulator_Reset — Reset zeroes every counter; next
// summary is empty string.
func TestStreamlinedAccumulator_Reset(t *testing.T) {
	var s StreamlinedAccumulator
	s.AccumulateTool("Grep")
	s.AccumulateTool("Bash")
	if s.Empty() {
		t.Errorf("precondition: should have counts")
	}
	s.Reset()
	if !s.Empty() {
		t.Errorf("after Reset: Empty() should be true")
	}
	if got := s.Summary(); got != "" {
		t.Errorf("after Reset: Summary() should be empty; got %q", got)
	}
}

// TestStreamlinedAccumulator_UnknownToolBucketsToOther — MCP / plugin
// tools metis doesn't recognize fall into "other" rather than panicking
// or being dropped silently. Same fail-open behavior as CC.
func TestStreamlinedAccumulator_UnknownToolBucketsToOther(t *testing.T) {
	var s StreamlinedAccumulator
	s.AccumulateTool("mcp__filesystem__read")
	s.AccumulateTool("CustomPluginTool")
	got := s.Summary()
	if !strings.Contains(got, "2 other tools") {
		t.Errorf("unknown tools should batch to 'other'; got %q", got)
	}
}

// TestStreamlinedAccumulator_EmptyState — initial / fresh accumulator
// returns empty summary. Safe to call without any AccumulateTool calls
// (the cmdRun flush path does this on every text delta).
func TestStreamlinedAccumulator_EmptyState(t *testing.T) {
	var s StreamlinedAccumulator
	if !s.Empty() {
		t.Errorf("zero-value should be Empty")
	}
	if got := s.Summary(); got != "" {
		t.Errorf("empty summary should be empty string; got %q", got)
	}
}

// TestCategorizeStreamlined_AllBuckets — pin the canonical mapping so
// renaming a builtin tool is a deliberate decision (test will fail and
// remind us to update the bucket).
func TestCategorizeStreamlined_AllBuckets(t *testing.T) {
	cases := map[string]string{
		"Grep":         "search",
		"Glob":         "search",
		"WebSearch":    "search",
		"WebFetch":     "search",
		"WebBrowse":    "search",
		"LSP":          "search",
		"Read":         "read",
		"LS":           "read",
		"Write":        "write",
		"Edit":         "write",
		"NotebookEdit": "write",
		"Bash":         "command",
		"Git":          "command",
		"Agent":        "other",
		"Memory":       "other",
		"":             "other",
	}
	for tool, want := range cases {
		if got := categorizeStreamlined(tool); got != want {
			t.Errorf("categorizeStreamlined(%q) = %q, want %q", tool, got, want)
		}
	}
}
