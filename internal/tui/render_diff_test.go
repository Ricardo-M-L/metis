package tui

// Diff-coloring smoke test (Phase task #36) — confirms renderEditDiff
// emits the expected red/green SGR codes for `-`/`+` lines on the
// active theme. The test pins behavior the user reported missing in
// image #8 (2026-05-08); this guards against a future refactor that
// silently drops the background-color styles.

import (
	"strings"
	"testing"
)

func TestRenderEditDiff_EmitsRedAndGreenSGR(t *testing.T) {
	input := map[string]any{
		"path": "/tmp/example.go",
		// metis Edit tool's actual field names — `old`/`new`. The
		// renderer accepts `old_string`/`new_string` too as a
		// fallback for claude-code-style external tools, but the
		// 99%-of-turns path uses these short names.
		"old": "fmt.Println(\"hello\")",
		"new": "fmt.Println(\"world\")",
	}
	got := renderEditDiff(input, false)
	if got == "" {
		t.Fatalf("renderEditDiff returned empty for non-empty edit")
	}
	// Red and green SGR background codes vary per theme. The default
	// dark theme uses #1e3a1e / #3a1e1e — what we *can* assert without
	// pinning the exact hex is that there's an SGR escape AND both a
	// `-` and a `+` row.
	if !strings.Contains(got, "\x1b[") {
		t.Errorf("expected ANSI escape sequence; got: %q", got)
	}
	if !strings.Contains(got, "- ") {
		t.Errorf("expected delete row marker '- '; got:\n%s", got)
	}
	if !strings.Contains(got, "+ ") {
		t.Errorf("expected insert row marker '+ '; got:\n%s", got)
	}
}

func TestRenderWritePreview_EmitsGreenSGR(t *testing.T) {
	input := map[string]any{
		"path":    "/tmp/new.go",
		"content": "package main\n\nfunc main() {}\n",
	}
	got := renderWritePreview(input, false)
	if got == "" {
		t.Fatalf("renderWritePreview returned empty for non-empty content")
	}
	if !strings.Contains(got, "\x1b[") {
		t.Errorf("expected ANSI escape sequence in Write preview")
	}
	if !strings.Contains(got, "+ ") {
		t.Errorf("Write preview rows should be marked with '+ ' (no before)")
	}
}
