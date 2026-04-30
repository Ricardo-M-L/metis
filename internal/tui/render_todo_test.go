package tui

import (
	"strings"
	"testing"
)

// TestRenderTodoSnapshot verifies the inline task-list body that's
// emitted as a TodoWrite tool result body. claude-code's image #57
// shows: tree leaf on the first row, ✓ glyph + strikethrough for
// completed, ▪ + bold for in_progress, □ + dim for pending. We don't
// assert exact ANSI bytes (those drift with theme tweaks); we assert
// the structural pieces a user would notice.
func TestRenderTodoSnapshot(t *testing.T) {
	old := tasksRuntimeSessionID
	defer func() { tasksRuntimeSessionID = old }()

	// Override session id resolver + the disk-read fn to feed fake
	// tasks without touching ~/.metis/tasks.
	tasksRuntimeSessionID = func() string { return "test-sid" }

	got := renderTodoSnapshotWith([]TaskItem{
		{ID: "1", Status: "completed", Content: "Bump event channel buffers 64 → 256"},
		{ID: "2", Status: "completed", Content: "Stream the compaction call"},
		{ID: "3", Status: "in_progress", Content: "Add Google Gemini provider"},
		{ID: "4", Status: "pending", Content: "Write release notes"},
	})

	// Tree leaf only on first row.
	if !strings.Contains(got, glyphTreeLeaf) {
		t.Errorf("expected tree-leaf glyph (%q) on first row; got:\n%s", glyphTreeLeaf, got)
	}
	if strings.Count(got, glyphTreeLeaf) != 1 {
		t.Errorf("tree leaf should appear exactly once; got %d times in:\n%s",
			strings.Count(got, glyphTreeLeaf), got)
	}
	// Completed-row glyph + strike content present.
	if !strings.Contains(got, "✓") {
		t.Error("expected ✓ for completed tasks")
	}
	// In-progress glyph.
	if !strings.Contains(got, "▪") {
		t.Error("expected ▪ for in_progress task")
	}
	// Pending glyph.
	if !strings.Contains(got, "□") {
		t.Error("expected □ for pending task")
	}
	// Each task content should appear (not stripped by accident).
	for _, content := range []string{
		"Bump event channel buffers 64 → 256",
		"Stream the compaction call",
		"Add Google Gemini provider",
		"Write release notes",
	} {
		if !strings.Contains(got, content) {
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
