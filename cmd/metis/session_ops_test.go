package main

// Phase-E smoke tests — exercise the pure helpers (flattenContent
// shape transform, readPidIfExists' missing-file path). The
// store-listing branch (cmdPs / cmdLogs) reads ~/.metis which we
// don't want to touch from CI; those run in the e2e harness.

import (
	"strings"
	"testing"
)

func TestFlattenContent_Mixed(t *testing.T) {
	raw := []any{
		map[string]any{"type": "text", "text": "hello"},
		map[string]any{"type": "tool_use", "name": "Read"},
		map[string]any{"type": "text", "text": "world"},
	}
	got := flattenContent(raw)
	if !strings.Contains(got, "hello") || !strings.Contains(got, "[→ Read]") || !strings.Contains(got, "world") {
		t.Errorf("unexpected flatten output: %q", got)
	}
}

func TestFlattenContent_ToolError(t *testing.T) {
	raw := []any{
		map[string]any{"type": "tool_result", "is_error": true},
	}
	got := flattenContent(raw)
	if !strings.Contains(got, "[← error]") {
		t.Errorf("error result not surfaced: %q", got)
	}
}

func TestFlattenContent_Nil(t *testing.T) {
	if got := flattenContent(nil); got != "" {
		t.Errorf("nil should flatten to empty string; got %q", got)
	}
}

func TestReadPidIfExists_MissingFile(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if got := readPidIfExists("ghost"); got != "-" {
		t.Errorf("missing pidfile should return '-'; got %q", got)
	}
}
