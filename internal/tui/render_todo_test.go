package tui

import (
	"strings"
	"testing"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

// TestRenderTodoSnapshot verifies the inline task-list body emitted as
// a TodoWrite tool result. Mirrors claude-code's TaskListV2.tsx
// getTaskIcon mapping: tree-leaf connector on row 1, ◻/◼/✔ status
// glyphs (figures package's squareSmall / squareSmallFilled / tick),
// strikethrough on completed titles. We assert structural pieces +
// the ANSI bytes for color (orange = AccentOrange for in_progress,
// green = AccentGreen for completed) since a regression to all-blue
// or wrong glyphs is the regression image #1 surfaced.
func TestRenderTodoSnapshot(t *testing.T) {
	old := tasksRuntimeSessionID
	defer func() { tasksRuntimeSessionID = old }()
	tasksRuntimeSessionID = func() string { return "test-sid" }

	got := renderTodoSnapshotWith([]TaskItem{
		{ID: "1", Status: "completed", Content: "Bump event channel buffers 64 → 256"},
		{ID: "2", Status: "completed", Content: "Stream the compaction call"},
		{ID: "3", Status: "in_progress", Content: "Add Google Gemini provider"},
		{ID: "4", Status: "pending", Content: "Write release notes"},
	})

	// Tree leaf only on first row — the rest indent without the connector.
	if c := strings.Count(got, glyphTreeLeaf); c != 1 {
		t.Errorf("tree leaf should appear exactly once; got %d times in:\n%s", c, got)
	}

	// claude-code-matched glyphs (figures package). Don't accept the
	// pre-fix ▪/✓/□ — those were close but visibly different in
	// side-by-side terminals (image #1 user feedback 2026-05-10).
	for _, want := range []struct{ glyph, role string }{
		{glyphTaskCompleted, "completed (✔)"},
		{glyphTaskInProgress, "in_progress (◼)"},
		{glyphTaskPending, "pending (◻)"},
	} {
		if !strings.Contains(got, want.glyph) {
			t.Errorf("expected %s glyph %q in:\n%s", want.role, want.glyph, got)
		}
	}
	// Regression bait: the OLD glyphs must NOT appear. If a refactor
	// reintroduces them, this catches it.
	for _, oldGlyph := range []string{"▪", "□", "✓"} {
		if strings.Contains(got, oldGlyph) {
			t.Errorf("legacy glyph %q leaked through; should be claude-code-matched glyph", oldGlyph)
		}
	}

	// Strikethrough on completed lines — lipgloss v2 combines color +
	// strike into one SGR (e.g. "\x1b[38;2;R;G;B;9m"), so we look for
	// ";9m" rather than the standalone "\x1b[9m". Without strike,
	// completed/pending look identical and the user can't scan for
	// what's done.
	if !strings.Contains(got, ";9m") {
		t.Errorf("expected SGR 9 (strikethrough) on completed rows; not found in:\n%q", got)
	}

	// Color contract: in_progress glyph in orange (AccentOrange) and
	// completed glyph in green (AccentGreen). We don't pin the exact
	// hex (theme might switch) but we assert the bytes for the CURRENT
	// theme appear, which is enough to catch "all blue" or "all muted"
	// regressions like the one image #1 surfaced.
	wantOrange := lipgloss.NewStyle().Foreground(accentOrange).Render(glyphTaskInProgress)
	wantGreen := lipgloss.NewStyle().Foreground(accentGreen).Render(glyphTaskCompleted)
	if !strings.Contains(got, wantOrange[:strings.Index(wantOrange, glyphTaskInProgress)]) {
		t.Errorf("in_progress glyph should be styled in AccentOrange; not found in:\n%q", got)
	}
	if !strings.Contains(got, wantGreen[:strings.Index(wantGreen, glyphTaskCompleted)]) {
		t.Errorf("completed glyph should be styled in AccentGreen; not found in:\n%q", got)
	}

	// Each task title must survive (no accidental strip from the
	// strikethrough wrapping). lipgloss splits per-run ANSI so we
	// strip codes before substring search.
	plain := ansi.Strip(got)
	for _, content := range []string{
		"Bump event channel buffers 64 → 256",
		"Stream the compaction call",
		"Add Google Gemini provider",
		"Write release notes",
	} {
		if !strings.Contains(plain, content) {
			t.Errorf("task content missing: %q", content)
		}
	}
}

// TestRenderTodoSnapshot_EmptyOrMissing covers the no-data paths.
func TestRenderTodoSnapshot_EmptyOrMissing(t *testing.T) {
	old := tasksRuntimeSessionID
	defer func() { tasksRuntimeSessionID = old }()

	// No session id → empty body, not a panic.
	tasksRuntimeSessionID = func() string { return "" }
	if got := renderTodoSnapshotWith(nil); got != "" {
		t.Errorf("empty session should render empty; got %q", got)
	}

	// Empty list → empty body, no header.
	tasksRuntimeSessionID = func() string { return "sid" }
	if got := renderTodoSnapshotWith([]TaskItem{}); got != "" {
		t.Errorf("empty task list should render empty; got %q", got)
	}
}

// renderTodoSnapshotWith is a test-only wrapper that lets the test
// inject a fixed task list instead of reading from disk. Production
// renderTodoSnapshot reads via tasksFullList.
func renderTodoSnapshotWith(items []TaskItem) string {
	if tasksRuntimeSessionID() == "" {
		return ""
	}
	if len(items) == 0 {
		return ""
	}
	return renderTaskItems(items)
}
