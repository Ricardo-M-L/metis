package tui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/session"
)

func replacementHistory() []llm.Message {
	return []llm.Message{
		{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text", Text: "old prompt"}}},
		{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: "text", Text: "old answer"}}},
	}
}

func seedReplacementSession(t *testing.T, store *session.Store, sid string, history []llm.Message) {
	t.Helper()
	if err := store.WriteHeader(sid, "test-model", "system"); err != nil {
		t.Fatal(err)
	}
	for _, message := range history {
		if err := store.AppendMessage(sid, message); err != nil {
			t.Fatal(err)
		}
	}
}

func TestCmdClearPersistsEmptyHistory(t *testing.T) {
	store, err := session.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	const sid = "repl-clear"
	history := replacementHistory()
	seedReplacementSession(t, store, sid, history)
	loop := &agent.Loop{}
	loop.Restore(history)
	cursor := session.NewHistoryCursor(history)
	r := &REPL{Loop: loop, Session: store, SessionID: sid, historyCursor: &cursor}

	if got := cmdClear(r, ""); got != "(history cleared)" {
		t.Fatalf("cmdClear = %q", got)
	}
	_, loaded, err := store.Load(sid)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded) != 0 || len(loop.History()) != 0 {
		t.Fatalf("clear did not persist/reset: disk=%#v memory=%#v", loaded, loop.History())
	}
}

func TestCmdClearPersistenceFailureKeepsLiveHistory(t *testing.T) {
	dir := t.TempDir()
	store, err := session.NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	const sid = "blocked-repl-clear"
	if err := os.Mkdir(filepath.Join(dir, sid+".jsonl"), 0o755); err != nil {
		t.Fatal(err)
	}
	history := replacementHistory()
	loop := &agent.Loop{}
	loop.Restore(history)
	cursor := session.NewHistoryCursor(history)
	r := &REPL{Loop: loop, Session: store, SessionID: sid, historyCursor: &cursor}

	if got := cmdClear(r, ""); !strings.Contains(got, "clear failed") {
		t.Fatalf("cmdClear failure = %q", got)
	}
	if len(loop.History()) != len(history) {
		t.Fatalf("failed clear discarded live history: %#v", loop.History())
	}
}

func TestModelReloadPersistsClearBeforeReset(t *testing.T) {
	store, err := session.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	const sid = "tui-reload-clear"
	history := replacementHistory()
	seedReplacementSession(t, store, sid, history)
	loop := &agent.Loop{}
	loop.Restore(history)
	m := &Model{
		loop:          loop,
		session:       store,
		sessionID:     sid,
		historyCursor: session.NewHistoryCursor(history),
	}
	if err := m.Reload(ReloadOpts{PreserveInput: true}); err != nil {
		t.Fatal(err)
	}
	_, loaded, err := store.Load(sid)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded) != 0 || len(loop.History()) != 0 {
		t.Fatalf("reload did not clear both views: disk=%#v memory=%#v", loaded, loop.History())
	}
}

func TestModelReloadPersistenceFailureReturnsErrorAndKeepsHistory(t *testing.T) {
	dir := t.TempDir()
	store, err := session.NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	const sid = "blocked-tui-reload"
	if err := os.Mkdir(filepath.Join(dir, sid+".jsonl"), 0o755); err != nil {
		t.Fatal(err)
	}
	history := replacementHistory()
	loop := &agent.Loop{}
	loop.Restore(history)
	m := &Model{
		loop:          loop,
		session:       store,
		sessionID:     sid,
		historyCursor: session.NewHistoryCursor(history),
	}
	if err := m.Reload(ReloadOpts{PreserveInput: true}); err == nil {
		t.Fatal("expected reload persistence error")
	}
	if len(loop.History()) != len(history) {
		t.Fatalf("failed reload cleared live history: %#v", loop.History())
	}
}

func TestHandleSubmitUndoPersistsReplacement(t *testing.T) {
	m := newSlashTestModel(t)
	store, err := session.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	const sid = "tui-undo"
	history := replacementHistory()
	seedReplacementSession(t, store, sid, history)
	m.session = store
	m.sessionID = sid
	m.loop.Restore(history)
	m.historyCursor = session.NewHistoryCursor(history)
	m.input.SetValue("/undo")

	_, _ = m.handleSubmit()
	_, loaded, err := store.Load(sid)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded) != 0 || len(m.loop.History()) != 0 {
		t.Fatalf("undo revived old turn: disk=%#v memory=%#v", loaded, m.loop.History())
	}
}

func TestHandleSubmitClearHistoryPersistsReplacement(t *testing.T) {
	m := newSlashTestModel(t)
	store, err := session.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	const sid = "tui-submit-clear"
	history := replacementHistory()
	seedReplacementSession(t, store, sid, history)
	m.session = store
	m.sessionID = sid
	m.loop.Restore(history)
	m.historyCursor = session.NewHistoryCursor(history)
	m.input.SetValue("/clear-history")

	_, _ = m.handleSubmit()
	_, loaded, err := store.Load(sid)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded) != 0 || len(m.loop.History()) != 0 {
		t.Fatalf("clear-history revived old turn: disk=%#v memory=%#v", loaded, m.loop.History())
	}
}
