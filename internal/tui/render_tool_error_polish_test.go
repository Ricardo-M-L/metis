package tui

import (
	"strings"
	"testing"
	"time"
)

func TestToolPolishBashErrorShowsDiagnosticOnce(t *testing.T) {
	// This is the synthetic error from the offline visual fixture; the test
	// does not execute git or depend on whether this example path exists.
	const diagnostic = "fatal: cannot change to '/tmp/missing-fixture': No such file or directory"
	for _, tc := range []struct {
		name, output string
	}{
		{"fixture diagnostic", diagnostic + "\nexit status 128"},
		{"empty diagnostic", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			te := ToolEvent{
				Kind: "result", ToolName: "Bash", IsError: true,
				Input: map[string]any{
					"command":     "git -C /tmp/missing-fixture status",
					"description": "检查示例仓库状态",
				},
				Output: tc.output, Duration: 20 * time.Millisecond,
			}
			for _, expanded := range []bool{false, true} {
				visible := stripANSI(clampBlock(renderToolEventAtWidth(te, expanded, 100), 100))
				if !strings.Contains(visible, "✗") || !strings.Contains(visible, "Failed") {
					t.Fatalf("expanded=%t: Bash failure lost its explicit failure state:\n%s", expanded, visible)
				}
				if tc.output != "" {
					if strings.Count(visible, diagnostic) != 1 || strings.Count(visible, "fatal:") != 1 {
						t.Fatalf("expanded=%t: diagnostic must appear once, without a repeated summary:\n%s", expanded, visible)
					}
					if strings.Contains(visible, "…") || !strings.Contains(visible, "exit status 128") {
						t.Fatalf("expanded=%t: short diagnostic was truncated or lost its exit status:\n%s", expanded, visible)
					}
				} else if !strings.Contains(visible, "no diagnostic output") || strings.Contains(visible, " · No output") {
					t.Fatalf("expanded=%t: empty error was presented like a quiet success:\n%s", expanded, visible)
				}
			}
			compact := stripANSI((&compactToolEventItem{te: te}).Render(100))
			summary := summarizeToolResult(te)
			if !strings.Contains(compact, "✗") || !strings.Contains(compact, summary) {
				t.Fatalf("compact/export view lost its standalone error summary %q:\n%s", summary, compact)
			}
			if tc.output != "" && !strings.Contains(compact, "fatal: cannot change to") {
				t.Fatalf("compact mode lost the diagnostic reason:\n%s", compact)
			}
			if te.Output != tc.output || !te.IsError {
				t.Fatal("rendering changed the raw diagnostic or failure state")
			}
		})
	}
}

func TestToolPolishErrorPreviewPromotesOnlyDiagnostics(t *testing.T) {
	for _, tc := range []struct {
		name, tail string
		promote    bool
	}{
		{"unknown error text", "evidence 7", false},
		{"actionable diagnostic", "Authentication failed for fixture repository.", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			original := "evidence 1\nevidence 2\nevidence 3\nevidence 4\nevidence 5\nevidence 6\n" + tc.tail
			te := ToolEvent{Kind: "end", ToolName: "Bash", IsError: true,
				Input: map[string]any{"command": "git status"}, Output: original}
			compact := stripANSI(renderToolEventAtWidth(te, false, 100))
			for _, line := range []string{"evidence 1", "evidence 2", "evidence 3", "evidence 4", "+2 output lines"} {
				if !strings.Contains(compact, line) {
					t.Errorf("error preview lost %q:\n%s", line, compact)
				}
			}
			if got := strings.Contains(compact, "evidence 5"); got == tc.promote {
				t.Errorf("fifth preview row presence=%t, want %t:\n%s", got, !tc.promote, compact)
			}
			if got := strings.Contains(compact, tc.tail); got != tc.promote {
				t.Errorf("tail promotion=%t, want %t:\n%s", got, tc.promote, compact)
			}
			if strings.Contains(compact, "evidence 6") {
				t.Errorf("preview should still be limited to five output rows:\n%s", compact)
			}
			expanded := stripANSI(renderToolEventAtWidth(te, true, 100))
			orderedBody := "       " + strings.ReplaceAll(original, "\n", "\n       ") + "\n"
			if !strings.Contains(expanded, orderedBody) {
				t.Fatalf("expanded output lost its original content or order:\n%s", expanded)
			}
			if te.Output != original {
				t.Fatal("preview promotion mutated the original tool output")
			}
		})
	}
}
