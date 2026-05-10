package tui

// tasks_fulllist_test.go — regression coverage for tasksFullList. The
// TodoWrite tool writes ~/.metis/tasks/<sid>.json in the envelope
// shape `{"session":"…","items":[…]}`; an earlier version of
// tasksFullList tried to unmarshal that bytes into a bare []TaskItem
// and silently returned nil on failure, which is why both the Ctrl+T
// panel and the inline TodoWrite-body renderer showed "no todos yet"
// even when the disk had populated rows (image #1 feedback 2026-05-10).
//
// The fix decodes the envelope first and falls back to the bare array
// for hand-written / legacy files. These tests lock both shapes plus
// the unhappy paths (missing file, invalid JSON, empty sid).

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func writeTaskFile(t *testing.T, dir, sid string, raw []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, "tasks"), 0o755); err != nil {
		t.Fatalf("mkdir tasks: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "tasks", sid+".json"), raw, 0o644); err != nil {
		t.Fatalf("write task file: %v", err)
	}
}

func TestTasksFullList_ReadsToolEnvelopeShape(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("METIS_HOME", dir)

	// The exact shape internal/tasks::TaskList writes (verified against
	// ~/.metis/tasks/<sid>.json on disk after a real TodoWrite call).
	envelope := map[string]any{
		"session": "fake-sid",
		"items": []map[string]string{
			{"id": "1", "content": "first", "status": "completed"},
			{"id": "2", "content": "second", "status": "in_progress"},
			{"id": "3", "content": "third", "status": "pending"},
		},
	}
	raw, _ := json.Marshal(envelope)
	writeTaskFile(t, dir, "fake-sid", raw)

	items := tasksFullList("fake-sid")
	if len(items) != 3 {
		t.Fatalf("expected 3 items from envelope shape; got %d (%+v)", len(items), items)
	}
	if items[0].Content != "first" || items[1].Content != "second" || items[2].Content != "third" {
		t.Errorf("content order wrong: %+v", items)
	}
	if items[0].Status != "completed" || items[1].Status != "in_progress" || items[2].Status != "pending" {
		t.Errorf("status mapping wrong: %+v", items)
	}
}

func TestTasksFullList_ReadsBareArrayFallback(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("METIS_HOME", dir)

	// Hand-edited / hypothetical-legacy file: bare top-level array.
	raw := []byte(`[{"id":"1","content":"alpha","status":"pending"}]`)
	writeTaskFile(t, dir, "bare-sid", raw)

	items := tasksFullList("bare-sid")
	if len(items) != 1 || items[0].Content != "alpha" {
		t.Fatalf("bare-array fallback should return [alpha]; got %+v", items)
	}
}

func TestTasksFullList_EmptySidReturnsNil(t *testing.T) {
	if got := tasksFullList(""); got != nil {
		t.Errorf("empty sid should yield nil; got %+v", got)
	}
}

func TestTasksFullList_MissingFileReturnsNil(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("METIS_HOME", dir)
	if got := tasksFullList("does-not-exist"); got != nil {
		t.Errorf("missing file should yield nil; got %+v", got)
	}
}

func TestTasksFullList_InvalidJSONReturnsNil(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("METIS_HOME", dir)
	writeTaskFile(t, dir, "broken", []byte("{not json"))
	if got := tasksFullList("broken"); got != nil {
		t.Errorf("invalid JSON should yield nil; got %+v", got)
	}
}
