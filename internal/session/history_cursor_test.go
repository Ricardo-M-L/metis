package session

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
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

func TestCheckpointCompaction_AppendsRawTailBeforeReplacement(t *testing.T) {
	dir := t.TempDir()
	store, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	const sid = "checkpoint-order"
	if err := store.WriteHeader(sid, "model", "system"); err != nil {
		t.Fatal(err)
	}
	durable := []llm.Message{historyText(llm.RoleUser, "persisted prompt")}
	if err := store.AppendMessage(sid, durable[0]); err != nil {
		t.Fatal(err)
	}
	cursor := NewHistoryCursor(durable)

	rawAssistant := llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{
		{Type: "text", Text: "working"},
		{Type: "tool_use", ToolUseID: "tool-1", ToolName: "Read", ToolInput: map[string]any{"path": "notes.txt"}},
	}}
	rawResult := llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{
		{Type: "tool_result", ToolUseID: "tool-1", ToolResult: "raw tool result"},
	}}
	before := append(append([]llm.Message(nil), durable...), rawAssistant, rawResult)
	after := []llm.Message{
		historyText(llm.RoleUser, "<checkpoint/>"),
		historyText(llm.RoleAssistant, "summary retaining tool-1 outcome"),
	}
	if err := store.CheckpointCompaction(sid, before, after, &cursor); err != nil {
		t.Fatal(err)
	}

	_, loaded, err := store.Load(sid)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(loaded, after) {
		t.Fatalf("logical history = %#v, want replacement %#v", loaded, after)
	}

	f, err := os.Open(filepath.Join(dir, sid+".jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var entries []Entry
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		var entry Entry
		if err := json.Unmarshal(scanner.Bytes(), &entry); err != nil {
			t.Fatal(err)
		}
		entries = append(entries, entry)
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	if len(entries) != 5 {
		t.Fatalf("entry count = %d, want header + 3 raw messages + replacement: %#v", len(entries), entries)
	}
	wantTypes := []string{"header", "message", "message", "message", "history_replace"}
	for i, want := range wantTypes {
		if entries[i].Type != want {
			t.Fatalf("entry[%d].Type = %q, want %q", i, entries[i].Type, want)
		}
	}
	if entries[2].Message == nil || !reflect.DeepEqual(*entries[2].Message, rawAssistant) {
		t.Fatalf("raw assistant ledger entry = %#v, want %#v", entries[2].Message, rawAssistant)
	}
	if entries[3].Message == nil || !reflect.DeepEqual(*entries[3].Message, rawResult) {
		t.Fatalf("raw tool-result ledger entry = %#v, want %#v", entries[3].Message, rawResult)
	}
	if !reflect.DeepEqual(entries[4].Messages, after) {
		t.Fatalf("replacement ledger entry = %#v, want %#v", entries[4].Messages, after)
	}

	continued := append(append([]llm.Message(nil), after...), historyText(llm.RoleUser, "continue"))
	if err := store.AppendHistoryTail(sid, continued, &cursor); err != nil {
		t.Fatal(err)
	}
	_, loaded, err = store.Load(sid)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(loaded, continued) {
		t.Fatalf("continued history = %#v, want %#v", loaded, continued)
	}
}

func TestCheckpointCompaction_SyncsRawTailBeforeDurableReplacement(t *testing.T) {
	dir := t.TempDir()
	store, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	const sid = "checkpoint-sync-order"
	if err := store.WriteHeader(sid, "model", "system"); err != nil {
		t.Fatal(err)
	}
	durable := []llm.Message{historyText(llm.RoleUser, "persisted")}
	if err := store.AppendMessage(sid, durable[0]); err != nil {
		t.Fatal(err)
	}
	cursor := NewHistoryCursor(durable)
	before := append(append([]llm.Message(nil), durable...),
		historyText(llm.RoleAssistant, "raw answer"),
		historyText(llm.RoleUser, "raw follow-up"),
	)
	after := []llm.Message{historyText(llm.RoleAssistant, "durable checkpoint")}

	var syncedLastTypes []string
	store.syncFile = func(file *os.File) error {
		body, err := os.ReadFile(file.Name())
		if err != nil {
			return err
		}
		lines := bytes.Split(bytes.TrimSuffix(body, []byte("\n")), []byte("\n"))
		var entry Entry
		if err := json.Unmarshal(lines[len(lines)-1], &entry); err != nil {
			return err
		}
		syncedLastTypes = append(syncedLastTypes, entry.Type)
		return file.Sync()
	}

	if err := store.CheckpointCompaction(sid, before, after, &cursor); err != nil {
		t.Fatal(err)
	}
	want := []string{"message", "history_replace"}
	if !reflect.DeepEqual(syncedLastTypes, want) {
		t.Fatalf("sync barriers observed after %v, want %v", syncedLastTypes, want)
	}
}

func TestCheckpointCompaction_ReplacementSyncFailureKeepsRawHistoryRetryable(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	const sid = "checkpoint-sync-failure"
	if err := store.WriteHeader(sid, "model", "system"); err != nil {
		t.Fatal(err)
	}
	durable := []llm.Message{historyText(llm.RoleUser, "persisted")}
	if err := store.AppendMessage(sid, durable[0]); err != nil {
		t.Fatal(err)
	}
	cursor := NewHistoryCursor(durable)
	before := append(append([]llm.Message(nil), durable...), historyText(llm.RoleAssistant, "raw tail"))
	after := []llm.Message{historyText(llm.RoleAssistant, "checkpoint")}

	syncCalls := 0
	store.syncFile = func(file *os.File) error {
		syncCalls++
		if syncCalls == 2 {
			return errors.New("injected replacement fsync failure")
		}
		return file.Sync()
	}
	if err := store.CheckpointCompaction(sid, before, after, &cursor); err == nil {
		t.Fatal("expected replacement fsync failure")
	}
	if syncCalls != 3 {
		t.Fatalf("sync calls = %d, want raw barrier + failed replacement + rollback barrier", syncCalls)
	}
	if cursor.count != len(before) || cursor.last == nil || !reflect.DeepEqual(*cursor.last, before[len(before)-1]) {
		t.Fatalf("cursor = %+v, want durable raw history boundary", cursor)
	}
	_, got, err := store.Load(sid)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, before) {
		t.Fatalf("logical history after failed replacement sync = %#v, want raw %#v", got, before)
	}
}

func TestCheckpointCompaction_NilCursorDoesNotWrite(t *testing.T) {
	dir := t.TempDir()
	store, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	const sid = "nil-checkpoint-cursor"
	if err := store.WriteHeader(sid, "model", "system"); err != nil {
		t.Fatal(err)
	}
	before := []llm.Message{historyText(llm.RoleUser, "raw")}
	after := []llm.Message{historyText(llm.RoleAssistant, "summary")}
	if err := store.CheckpointCompaction(sid, before, after, nil); err == nil {
		t.Fatal("expected nil cursor error")
	}
	_, loaded, err := store.Load(sid)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded) != 0 {
		t.Fatalf("nil cursor wrote history: %#v", loaded)
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
