package builtin

import (
	"context"
	"strings"
	"testing"
)

// TestGlob_EmptyPatternRichError — image #34 repro: model called
// Glob with no `pattern` and the bare "pattern required" gave no
// hint about what to do. Verify the new message names the expected
// shape AND examples.
func TestGlob_EmptyPatternRichError(t *testing.T) {
	res, err := (Glob{}).Execute(context.Background(), map[string]any{})
	if err != nil {
		t.Fatalf("Execute returned err (want soft error in Result): %v", err)
	}
	if !res.IsError {
		t.Error("empty-pattern call should set Result.IsError=true")
	}
	for _, want := range []string{"`pattern` field is required", `"**/*.go"`, "shell commands", "list directories"} {
		if !strings.Contains(res.Output, want) {
			t.Errorf("error output missing %q; got:\n%s", want, res.Output)
		}
	}
}

// TestGlob_MisuseHintCommandField — the canonical image #34 shape:
// model put a shell invocation in `command` field thinking Glob was
// Bash. New hint should call out "you passed command, use Bash".
func TestGlob_MisuseHintCommandField(t *testing.T) {
	res, _ := (Glob{}).Execute(context.Background(), map[string]any{
		"command": `ls -la /Users/ricardo/Documents/foo/ 2>/dev/null || echo "DIR NOT FOUND"`,
	})
	if !res.IsError {
		t.Fatal("expected IsError=true")
	}
	for _, want := range []string{"You passed a `command` field", "Bash"} {
		if !strings.Contains(res.Output, want) {
			t.Errorf("missing hint %q; got:\n%s", want, res.Output)
		}
	}
}

// TestGlob_MisuseHintPathField — `path` field with a file path means
// the model meant Read; with a directory it means LS. The hint
// covers both by naming both alternatives.
func TestGlob_MisuseHintPathField(t *testing.T) {
	res, _ := (Glob{}).Execute(context.Background(), map[string]any{
		"path": "/some/file.py",
	})
	if !res.IsError {
		t.Fatal("expected IsError=true")
	}
	for _, want := range []string{"LS", "Read"} {
		if !strings.Contains(res.Output, want) {
			t.Errorf("missing %q in path-misuse hint; got:\n%s", want, res.Output)
		}
	}
}

// TestGlob_ShellShapedPatternRejected — model passed shell syntax
// (with `||`, `2>`, `/dev/null`) as `pattern`. Should reject
// immediately with a Bash redirect, NOT silently return zero
// matches (which the model would misread as "wrong glob, retry").
func TestGlob_ShellShapedPatternRejected(t *testing.T) {
	res, _ := (Glob{}).Execute(context.Background(), map[string]any{
		"pattern": `ls -la /some/path/ 2>/dev/null || echo "x"`,
	})
	if !res.IsError {
		t.Fatal("shell-shaped pattern should reject with IsError")
	}
	if !strings.Contains(res.Output, "Bash") {
		t.Errorf("rejection should redirect to Bash; got:\n%s", res.Output)
	}
}

// TestGlob_ShellShapeDetectorPositives — exhaustive list of shapes
// the heuristic catches.
func TestGlob_ShellShapeDetectorPositives(t *testing.T) {
	cases := []string{
		"foo || bar",
		"foo && bar",
		"foo 2> /dev/null",
		"x &> /tmp/log",
		"a >| b",
		"do_thing > /dev/stdin",
		"foo /dev/stderr",
		"ls -la /path/",
		"find . -name '*.go'",
		"grep -r pattern /src",
		"cat foo.go",
		"echo hi",
		"cd /tmp",
		"rg pattern",
		"head file",
		"tail -f log",
		"awk '{print}'",
		"sed 's/x/y/'",
	}
	for _, p := range cases {
		if globShellShapeHint(p) == "" {
			t.Errorf("globShellShapeHint(%q) returned empty; want non-empty hint", p)
		}
	}
}

// TestGlob_ShellShapeDetectorNegatives — common LEGIT glob patterns
// must NOT trigger the shell-shape rejection. False-positive cost
// here is a legitimate search returning a confusing rejection.
func TestGlob_ShellShapeDetectorNegatives(t *testing.T) {
	cases := []string{
		"**/*.go",
		"src/**/*.ts",
		"*.md",
		"foo/**",
		"internal/**/*_test.go",
		"src/[abc]/*.tsx",
		"docs/?*.md",
		"path/with/dots.x.y.z.md",
		"file-with-dashes.txt",
		"file_with_underscores.go",
	}
	for _, p := range cases {
		if hint := globShellShapeHint(p); hint != "" {
			t.Errorf("globShellShapeHint(%q) falsely matched: %s", p, hint)
		}
	}
}

// TestGlob_HintTruncatesLongCommand — the embedded command preview
// in the hint must truncate so a 500-char shell command doesn't
// blow the result row.
func TestGlob_HintTruncatesLongCommand(t *testing.T) {
	huge := strings.Repeat("abc/", 80) // 320 chars
	res, _ := (Glob{}).Execute(context.Background(), map[string]any{
		"command": huge,
	})
	if !strings.Contains(res.Output, "…") {
		t.Errorf("long command should truncate with …; got:\n%s", res.Output)
	}
}
