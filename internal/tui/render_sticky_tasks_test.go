package tui

// render_sticky_tasks_test.go — pin the sticky strip's behavior:
// hides when empty / when overlay is up, prioritizes in_progress,
// rolls up completed into a count line, truncates long content.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// setupStickyTasksTestEnv sandboxes ~/.metis to a temp dir and seeds
// the task file the sticky strip reads from. Returns the (model,
// sessionID) the test should pass into renderStickyTaskStrip.
func setupStickyTasksTestEnv(t *testing.T, items []TaskItem) (*Model, string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("METIS_HOME", home)
	sid := "sticky-test-" + t.Name()
	// Replicate the on-disk envelope shape (matches what tasksFullList
	// reads — { items: [...] } under ~/.metis/tasks/<sid>.json).
	dir := filepath.Join(home, "tasks")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	payload := struct {
		Items []TaskItem `json:"items"`
	}{Items: items}
	data, _ := json.Marshal(payload)
	if err := os.WriteFile(filepath.Join(dir, sid+".json"), data, 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	return &Model{sessionID: sid}, sid
}

func TestStickyTaskStrip_EmptySessionRendersNothing(t *testing.T) {
	m, _ := setupStickyTasksTestEnv(t, nil)
	if got := renderStickyTaskStrip(m); got != "" {
		t.Errorf("empty session should render nothing; got %q", got)
	}
}

func TestStickyTaskStrip_HiddenWhenOverlayShowing(t *testing.T) {
	// Overlay-up case: avoid double-rendering. The Ctrl+T panel
	// (m.showTaskPanel=true) already shows the full list; the
	// sticky strip must stay empty in that frame.
	m, _ := setupStickyTasksTestEnv(t, []TaskItem{
		{ID: "1", Status: "in_progress", Content: "Refactor auth gate"},
	})
	m.showTaskPanel = true
	if got := renderStickyTaskStrip(m); got != "" {
		t.Errorf("overlay-up should suppress strip; got %q", got)
	}
}

func TestStickyTaskStrip_InProgressOnTop(t *testing.T) {
	m, _ := setupStickyTasksTestEnv(t, []TaskItem{
		{ID: "1", Status: "completed", Content: "Read source"},
		{ID: "2", Status: "in_progress", Content: "Write parser"},
		{ID: "3", Status: "pending", Content: "Add tests"},
	})
	out := renderStickyTaskStrip(m)
	if out == "" {
		t.Fatalf("non-empty session should render")
	}
	iInProgress := strings.Index(out, "Write parser")
	iPending := strings.Index(out, "Add tests")
	iCompleted := strings.Index(out, "done")
	if iInProgress < 0 || iPending < 0 || iCompleted < 0 {
		t.Fatalf("strip missing expected rows: %q", out)
	}
	if !(iInProgress < iPending && iPending < iCompleted) {
		t.Errorf("ordering should be in_progress → pending → done rollup; got positions: in_progress=%d pending=%d completed=%d in %q",
			iInProgress, iPending, iCompleted, out)
	}
}

func TestStickyTaskStrip_CompletedRollsUpToCount(t *testing.T) {
	// 7 completed items → one "7 done" row, never 7 separate rows.
	// Cap exists so a long task history doesn't push the active
	// row off-screen.
	items := []TaskItem{
		{ID: "active", Status: "in_progress", Content: "Working on X"},
	}
	for i := 0; i < 7; i++ {
		items = append(items, TaskItem{ID: "done-" + string(rune('a'+i)), Status: "completed", Content: "finished step"})
	}
	m, _ := setupStickyTasksTestEnv(t, items)
	out := renderStickyTaskStrip(m)
	if !strings.Contains(out, "7 done") {
		t.Errorf("expected '7 done' rollup; got: %q", out)
	}
	// Should NOT contain 7 separate "finished step" rows
	if strings.Count(out, "finished step") > 0 {
		t.Errorf("completed item content should be hidden under rollup; got: %q", out)
	}
}

func TestStickyTaskStrip_PendingLookaheadCappedAtThree(t *testing.T) {
	items := []TaskItem{{ID: "a", Status: "in_progress", Content: "now"}}
	for i := 0; i < 5; i++ {
		items = append(items, TaskItem{
			ID: "p" + string(rune('1'+i)), Status: "pending",
			Content: "pending-" + string(rune('A'+i)),
		})
	}
	m, _ := setupStickyTasksTestEnv(t, items)
	out := renderStickyTaskStrip(m)
	// First 3 pending should appear, next 2 should not (cap = 3).
	for _, want := range []string{"pending-A", "pending-B", "pending-C"} {
		if !strings.Contains(out, want) {
			t.Errorf("expected pending %q within first 3; got %q", want, out)
		}
	}
	for _, dontWant := range []string{"pending-D", "pending-E"} {
		if strings.Contains(out, dontWant) {
			t.Errorf("pending beyond cap %q should be hidden; got %q", dontWant, out)
		}
	}
}

func TestTruncateTodoContent_RuneSafe(t *testing.T) {
	// 100 CJK characters at 3 bytes each = 300 bytes; rune-aware
	// truncation must cut at character boundaries, not mid-codepoint.
	in := strings.Repeat("公", 100)
	got := truncateTodoContent(in)
	gotRunes := []rune(got)
	if len(gotRunes) != stickyContentWidth {
		t.Errorf("rune count = %d; want exactly %d", len(gotRunes), stickyContentWidth)
	}
	if gotRunes[stickyContentWidth-1] != '…' {
		t.Errorf("last rune should be ellipsis, got %q", string(gotRunes[stickyContentWidth-1]))
	}
}

func TestTruncateTodoContent_ShortPassThrough(t *testing.T) {
	in := "short content"
	if got := truncateTodoContent(in); got != in {
		t.Errorf("short content modified: got %q, want %q", got, in)
	}
}
