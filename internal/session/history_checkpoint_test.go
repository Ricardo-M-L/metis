package session

import (
	"bytes"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/llm"
)

func checkpointHistory(t *testing.T, store *Store, id string, history []llm.Message, cursor *HistoryCursor) error {
	t.Helper()
	return store.CheckpointHistory(id, history, cursor)
}

func checkpointStore(t *testing.T) (*Store, string) {
	t.Helper()
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	const id = "checkpoint"
	if err := store.WriteHeader(id, "model", "system"); err != nil {
		t.Fatal(err)
	}
	return store, id
}

func checkpointPair(id string) []llm.Message {
	return []llm.Message{
		{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: "tool_use", ToolUseID: id, ToolName: "Read", ToolInput: map[string]any{"path": "note.txt"}}}},
		{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "tool_result", ToolUseID: id, ToolResult: "real tool output"}}},
	}
}

func checkpointBody(t *testing.T, store *Store, id string) []byte {
	t.Helper()
	body, err := os.ReadFile(store.path(id))
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func checkpointLoaded(t *testing.T, store *Store, id string, want []llm.Message) {
	t.Helper()
	_, got, err := store.Load(id)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("loaded history = %#v, want %#v", got, want)
	}
}

func TestCheckpointHistory_DurableIncrementalAndRepeatedFlush(t *testing.T) {
	store, id := checkpointStore(t)
	history := []llm.Message{historyText(llm.RoleUser, "prompt")}
	cursor := HistoryCursor{}
	syncCalls := 0
	store.syncFile = func(f *os.File) error {
		syncCalls++
		return f.Sync()
	}
	if err := checkpointHistory(t, store, id, history, &cursor); err != nil {
		t.Fatal(err)
	}
	history = append(history, checkpointPair("read-1")...)
	if err := checkpointHistory(t, store, id, history, &cursor); err != nil {
		t.Fatal(err)
	}
	before := checkpointBody(t, store, id)
	if err := checkpointHistory(t, store, id, history, &cursor); err != nil {
		t.Fatal(err)
	}
	if got := checkpointBody(t, store, id); !bytes.Equal(got, before) {
		t.Fatalf("repeated checkpoint changed ledger: %s", got)
	}
	if syncCalls != 3 {
		t.Fatalf("sync calls = %d, want one per checkpoint including no-op flush", syncCalls)
	}
	if got := bytes.Count(before, []byte(`"type":"message"`)); got != len(history) {
		t.Fatalf("message entries = %d, want %d incremental entries", got, len(history))
	}
	if bytes.Contains(before, []byte(`"type":"history_replace"`)) {
		t.Fatal("ordinary incremental checkpoint rewrote whole history")
	}
	checkpointLoaded(t, store, id, history)
}

func TestCheckpointHistory_SyncFailureRetriesWithoutDuplicateAppend(t *testing.T) {
	store, id := checkpointStore(t)
	history := append([]llm.Message{historyText(llm.RoleUser, "prompt")}, checkpointPair("read-1")...)
	cursor := HistoryCursor{}
	wantErr := errors.New("injected checkpoint fsync failure")
	syncCalls := 0
	store.syncFile = func(f *os.File) error {
		syncCalls++
		if syncCalls <= 2 {
			return wantErr
		}
		return f.Sync()
	}
	if err := checkpointHistory(t, store, id, history, &cursor); !errors.Is(err, wantErr) {
		t.Fatalf("error = %v, want injected fsync failure", err)
	}
	if cursor.count != len(history) {
		t.Fatalf("cursor count = %d, want visible prefix %d", cursor.count, len(history))
	}
	before := checkpointBody(t, store, id)
	if err := checkpointHistory(t, store, id, history, &cursor); !errors.Is(err, wantErr) {
		t.Fatalf("no-new-messages retry = %v, want repeated fsync failure", err)
	}
	if err := checkpointHistory(t, store, id, history, &cursor); err != nil {
		t.Fatal(err)
	}
	if syncCalls != 3 || !bytes.Equal(before, checkpointBody(t, store, id)) {
		t.Fatalf("retry duplicated messages or skipped sync: calls=%d", syncCalls)
	}
	checkpointLoaded(t, store, id, history)
}

func TestCheckpointHistory_RejectsUnpairedCallsBeforeWriting(t *testing.T) {
	pair := checkpointPair("read-1")
	for name, history := range map[string][]llm.Message{
		"missing result": {historyText(llm.RoleUser, "prompt"), pair[0]},
		"orphan result":  {pair[1]},
		"wrong result id": {pair[0], {Role: llm.RoleUser, Content: []llm.ContentBlock{
			{Type: "tool_result", ToolUseID: "different", ToolResult: "output"},
		}}},
		"duplicate pending id": {pair[0], pair[0], pair[1]},
		"empty call id":        {{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: "tool_use", ToolName: "Read"}}}},
	} {
		t.Run(name, func(t *testing.T) {
			store, id := checkpointStore(t)
			before := checkpointBody(t, store, id)
			cursor := HistoryCursor{}
			if err := checkpointHistory(t, store, id, history, &cursor); err == nil {
				t.Fatal("accepted unpaired history")
			}
			if cursor.count != 0 || !bytes.Equal(before, checkpointBody(t, store, id)) {
				t.Fatal("unpaired history changed disk or cursor")
			}
		})
	}
}

func TestCheckpointHistory_ProviderMayReuseIDAfterCompletedPair(t *testing.T) {
	store, id := checkpointStore(t)
	history := append(checkpointPair("reused"), checkpointPair("reused")...)
	cursor := HistoryCursor{}
	if err := checkpointHistory(t, store, id, history, &cursor); err != nil {
		t.Fatal(err)
	}
	checkpointLoaded(t, store, id, history)
}

func TestCheckpointHistory_PartialAppendFailureKeepsRetryableVisiblePrefix(t *testing.T) {
	store, id := checkpointStore(t)
	history := append([]llm.Message{historyText(llm.RoleUser, "prompt")}, checkpointPair("read-1")...)
	// Serialization of a later result fails after the preceding messages have
	// been appended. This is deliberately not a cross-message ACID guarantee:
	// the caller must abort provider work, then retry or use resume repair.
	history[2].Content[0].Presentation = map[string]any{"invalid": func() {}}
	cursor := HistoryCursor{}
	if err := checkpointHistory(t, store, id, history, &cursor); err == nil {
		t.Fatal("expected unserializable message failure")
	}
	if cursor.count != 2 {
		t.Fatalf("cursor count = %d, want successful two-message prefix", cursor.count)
	}
	checkpointLoaded(t, store, id, history[:2])
	history[2].Content[0].Presentation = nil
	if err := checkpointHistory(t, store, id, history, &cursor); err != nil {
		t.Fatal(err)
	}
	if got := bytes.Count(checkpointBody(t, store, id), []byte(`"type":"message"`)); got != 3 {
		t.Fatalf("retry persisted %d message entries, want exactly 3", got)
	}
	checkpointLoaded(t, store, id, history)
}

func TestCheckpointHistory_OpenFailureDoesNotAdvanceCursor(t *testing.T) {
	store, id := checkpointStore(t)
	history := []llm.Message{historyText(llm.RoleUser, "prompt")}
	cursor := HistoryCursor{}
	// A directory at the exact session path makes append fail without relying
	// on chmod behavior when the tests run with elevated filesystem privileges.
	blocked := "blocked"
	if err := os.Mkdir(store.path(blocked), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := checkpointHistory(t, store, blocked, history, &cursor); err == nil || cursor.count != 0 {
		t.Fatalf("open failure = %v, cursor = %+v", err, cursor)
	}
	if err := checkpointHistory(t, store, id, history, &cursor); err != nil {
		t.Fatal(err)
	}
	checkpointLoaded(t, store, id, history)
}

func TestCheckpointHistory_CompactionCursorInteroperates(t *testing.T) {
	store, id := checkpointStore(t)
	history := append([]llm.Message{historyText(llm.RoleUser, "prompt")}, checkpointPair("read-1")...)
	cursor := HistoryCursor{}
	if err := checkpointHistory(t, store, id, history, &cursor); err != nil {
		t.Fatal(err)
	}
	compacted := []llm.Message{historyText(llm.RoleAssistant, "summary")}
	if err := store.CheckpointCompaction(id, history, compacted, &cursor); err != nil {
		t.Fatal(err)
	}
	before := checkpointBody(t, store, id)
	if err := checkpointHistory(t, store, id, compacted, &cursor); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, checkpointBody(t, store, id)) {
		t.Fatal("checkpoint duplicated already-persisted compacted snapshot")
	}
	continued := append(compacted, checkpointPair("read-2")...)
	if err := checkpointHistory(t, store, id, continued, &cursor); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendHistoryTail(id, continued, &cursor); err != nil {
		t.Fatal(err)
	}
	checkpointLoaded(t, store, id, continued)
}

func TestCheckpointHistory_RepairsTornTrailingRecordBeforeRetry(t *testing.T) {
	store, id := checkpointStore(t)
	history := []llm.Message{historyText(llm.RoleUser, "prompt")}
	cursor := HistoryCursor{}
	if err := checkpointHistory(t, store, id, history, &cursor); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(store.path(id), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, writeErr := f.WriteString(`{"type":"message","message":{"role":"assistant"`)
	closeErr := f.Close()
	if writeErr != nil || closeErr != nil {
		t.Fatalf("create torn-tail fixture: write=%v close=%v", writeErr, closeErr)
	}
	history = append(history, checkpointPair("read-after-restart")...)
	if err := checkpointHistory(t, store, id, history, &cursor); err != nil {
		t.Fatal(err)
	}
	checkpointLoaded(t, store, id, history)
	if got := bytes.Count(checkpointBody(t, store, id), []byte(`"type":"message"`)); got != len(history) {
		t.Fatalf("message entries after torn-tail repair = %d, want %d", got, len(history))
	}
}

func TestHistoryCursor_MarkKeepsIndependentPersistedAnchor(t *testing.T) {
	store, id := checkpointStore(t)
	history := []llm.Message{historyText(llm.RoleUser, "original")}
	history[0].Content[0].ProviderHint = map[string]string{"state": "original"}
	history[0].Content[0].Presentation = map[string]any{"nested": []any{map[string]any{"value": "original"}}}
	if err := store.AppendMessage(id, history[0]); err != nil {
		t.Fatal(err)
	}
	cursor := NewHistoryCursor(history)
	history[0].Content[0].Text = "changed"
	history[0].Content[0].ProviderHint["state"] = "changed"
	history[0].Content[0].Presentation["nested"].([]any)[0].(map[string]any)["value"] = "changed"
	if cursor.last.Content[0].Text != "original" || cursor.last.Content[0].ProviderHint["state"] != "original" ||
		cursor.last.Content[0].Presentation["nested"].([]any)[0].(map[string]any)["value"] != "original" {
		t.Fatal("cursor anchor aliases mutable history content")
	}
	if err := store.AppendHistoryTail(id, history, &cursor); err != nil {
		t.Fatal(err)
	}
	checkpointLoaded(t, store, id, history)
}

func TestCheckpointHistory_NumericAnchorDoesNotRewriteEveryIteration(t *testing.T) {
	store, id := checkpointStore(t)
	history := []llm.Message{historyText(llm.RoleUser, "prompt")}
	cursor := HistoryCursor{}
	for i := 0; i < 12; i++ {
		message := historyText(llm.RoleAssistant, "step")
		message.Content[0].Presentation = map[string]any{"step": i, "nested": []int{i}}
		history = append(history, message)
		if err := checkpointHistory(t, store, id, history, &cursor); err != nil {
			t.Fatal(err)
		}
	}
	if body := checkpointBody(t, store, id); bytes.Contains(body, []byte(`"type":"history_replace"`)) {
		t.Fatalf("numeric anchor caused full-history rewrites: %s", body)
	}
}

func TestCheckpointHistory_NilCursor(t *testing.T) {
	store, id := checkpointStore(t)
	before := checkpointBody(t, store, id)
	if err := checkpointHistory(t, store, id, nil, nil); err == nil || !strings.Contains(err.Error(), "cursor") {
		t.Fatalf("nil cursor error = %v", err)
	}
	if !bytes.Equal(before, checkpointBody(t, store, id)) {
		t.Fatal("nil cursor changed ledger")
	}
}
