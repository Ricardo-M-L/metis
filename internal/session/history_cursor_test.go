package session

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/llm"
)

func historyText(role llm.Role, text string) llm.Message {
	return llm.Message{Role: role, Content: []llm.ContentBlock{{Type: "text", Text: text}}}
}

func TestAppendHistoryTail_ReplacesHistoryAfterCompaction(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	sid := "compacted"
	if err := store.WriteHeader(sid, "model", "system"); err != nil {
		t.Fatal(err)
	}
	original := []llm.Message{
		historyText(llm.RoleUser, "old prompt"),
		historyText(llm.RoleAssistant, "old answer"),
		historyText(llm.RoleUser, "current prompt"),
	}
	for _, msg := range original {
		if err := store.AppendMessage(sid, msg); err != nil {
			t.Fatal(err)
		}
	}
	cursor := NewHistoryCursor(original)

	// Compaction replaced the old prefix but retained the cursor anchor. A
	// suffix append would leave the old prompt/answer live on resume, so the
	// persisted view must become exactly the compacted history.
	compacted := []llm.Message{
		historyText(llm.RoleAssistant, "summary of old context"),
		original[2],
		historyText(llm.RoleAssistant, "new answer"),
	}
	if err := store.AppendHistoryTail(sid, compacted, &cursor); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendHistoryTail(sid, compacted, &cursor); err != nil {
		t.Fatal(err)
	}
	_, loaded, err := store.Load(sid)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(loaded, compacted) {
		t.Fatalf("persisted history = %#v, want compacted %#v", loaded, compacted)
	}
}

func TestAppendHistoryTail_ShrinkWritesEmptyAndNonEmptySnapshots(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	const sid = "undo-clear"
	if err := store.WriteHeader(sid, "model", "system"); err != nil {
		t.Fatal(err)
	}
	original := []llm.Message{
		historyText(llm.RoleUser, "first"),
		historyText(llm.RoleAssistant, "answer"),
		historyText(llm.RoleUser, "second"),
	}
	for _, msg := range original {
		if err := store.AppendMessage(sid, msg); err != nil {
			t.Fatal(err)
		}
	}
	cursor := NewHistoryCursor(original)
	undone := original[:1]
	if err := store.AppendHistoryTail(sid, undone, &cursor); err != nil {
		t.Fatal(err)
	}
	_, loaded, err := store.Load(sid)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(loaded, undone) {
		t.Fatalf("undo snapshot = %#v, want %#v", loaded, undone)
	}
	if err := store.ReplaceHistoryAndMark(sid, nil, &cursor); err != nil {
		t.Fatal(err)
	}
	_, loaded, err = store.Load(sid)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded) != 0 {
		t.Fatalf("clear snapshot retained %d messages: %#v", len(loaded), loaded)
	}
}

func TestAppendHistoryTail_DuplicateAnchorCannotSkipUnwrittenMessages(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	const sid = "duplicate-anchor"
	if err := store.WriteHeader(sid, "model", "system"); err != nil {
		t.Fatal(err)
	}
	anchor := historyText(llm.RoleAssistant, "same answer")
	original := []llm.Message{historyText(llm.RoleUser, "old"), anchor}
	for _, msg := range original {
		if err := store.AppendMessage(sid, msg); err != nil {
			t.Fatal(err)
		}
	}
	cursor := NewHistoryCursor(original)
	current := []llm.Message{
		historyText(llm.RoleAssistant, "summary"),
		anchor,
		historyText(llm.RoleUser, "must not be skipped"),
		anchor,
		historyText(llm.RoleAssistant, "new tail"),
	}
	if err := store.AppendHistoryTail(sid, current, &cursor); err != nil {
		t.Fatal(err)
	}
	_, loaded, err := store.Load(sid)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(loaded, current) {
		t.Fatalf("duplicate-anchor replacement = %#v, want %#v", loaded, current)
	}
}

func TestReplaceHistoryAndMark_WriteFailureLeavesCursorRetryable(t *testing.T) {
	dir := t.TempDir()
	store, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	const sid = "blocked"
	original := []llm.Message{historyText(llm.RoleUser, "old")}
	cursor := NewHistoryCursor(original)
	if err := os.Mkdir(filepath.Join(dir, sid+".jsonl"), 0o755); err != nil {
		t.Fatal(err)
	}
	replacement := []llm.Message{historyText(llm.RoleUser, "new")}
	if err := store.ReplaceHistoryAndMark(sid, replacement, &cursor); err == nil {
		t.Fatal("expected snapshot append failure")
	}
	if cursor.count != len(original) || cursor.last == nil || !reflect.DeepEqual(*cursor.last, original[0]) {
		t.Fatalf("failed write advanced cursor: %+v", cursor)
	}
}

func TestHistoryReplace_ExportImportPreservesLogicalHistory(t *testing.T) {
	src, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	const sid = "replace-export"
	if err := src.WriteHeader(sid, "model", "system"); err != nil {
		t.Fatal(err)
	}
	old := historyText(llm.RoleUser, "old")
	if err := src.AppendMessage(sid, old); err != nil {
		t.Fatal(err)
	}
	cursor := NewHistoryCursor([]llm.Message{old})
	live := []llm.Message{historyText(llm.RoleAssistant, "summary")}
	if err := src.ReplaceHistoryAndMark(sid, live, &cursor); err != nil {
		t.Fatal(err)
	}
	live = append(live, historyText(llm.RoleUser, "new tail"))
	if err := src.AppendHistoryTail(sid, live, &cursor); err != nil {
		t.Fatal(err)
	}

	var exported bytes.Buffer
	if err := src.Export(sid, &exported); err != nil {
		t.Fatal(err)
	}
	dst, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	importedID, err := dst.Import(&exported, "imported-replace")
	if err != nil {
		t.Fatal(err)
	}
	_, loaded, err := dst.Load(importedID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(loaded, live) {
		t.Fatalf("imported logical history = %#v, want %#v", loaded, live)
	}
}
