package tui

import (
	"strings"
	"testing"
)

func TestGlobSummarySeparatesPathsAndIncompleteDiagnostic(t *testing.T) {
	for _, tc := range []struct {
		name, output, want string
	}{
		{"complete", "a.go\nb.go\n", "Found 2 files"},
		{"empty lines are not files", "a.go\n\nb.go\n\n", "Found 2 files"},
		{"limited", "a.go\n\n[Glob search incomplete: showing 1 of 2 matches; narrow root or the pattern]\n", "Found 1 file · search incomplete"},
		{"unreadable", "(no matches)\n[Glob search incomplete: skipped 1 unreadable entries; check filesystem permissions]\n", "No files matched · search incomplete"},
		{"no matches", "(no matches)", "No files matched"},
		{"empty", "", "No files matched"},
	} {
		for _, tool := range []string{"Glob", "sub: Glob"} {
			t.Run(tc.name+"/"+tool, func(t *testing.T) {
				got := summarizeToolResult(ToolEvent{ToolName: tool, Output: tc.output})
				if !strings.HasSuffix(got, tc.want) {
					t.Fatalf("summary = %q, want suffix %q", got, tc.want)
				}
			})
		}
	}
}

func TestGlobCollapsedResultRetainsIncompleteWarning(t *testing.T) {
	output := "a.go\nb.go\nc.go\nd.go\ne.go\nf.go\n\n[Glob search incomplete: skipped 1 generated/cache directories; select the intended directory as root]\n"
	got := stripANSI(renderToolEvent(ToolEvent{Kind: "result", ToolName: "Glob", Output: output}, false))
	if !strings.Contains(got, "Found 6 files · search incomplete") {
		t.Fatalf("collapsed output hides search completeness or miscounts diagnostics:\n%s", got)
	}
	expanded := stripANSI(renderToolEvent(ToolEvent{Kind: "result", ToolName: "Glob", Output: output}, true))
	if !strings.Contains(expanded, "select the intended directory as root") {
		t.Fatalf("expanded output lost the recovery hint:\n%s", expanded)
	}
}
