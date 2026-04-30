package session

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/llm"
)

func newTempStore(t *testing.T) *Store {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "sessions")
	store, err := NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	return store
}

func TestSetTitle_RoundTripsViaLoad(t *testing.T) {
	st := newTempStore(t)
	id := "session-1"
	if err := st.WriteHeader(id, "claude-opus-4-7", "you are helpful"); err != nil {
		t.Fatalf("WriteHeader: %v", err)
	}
	if err := st.AppendMessage(id, llm.Message{
		Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text", Text: "hi"}},
	}); err != nil {
		t.Fatalf("AppendMessage: %v", err)
	}
	if err := st.SetTitle(id, "refactor sprint"); err != nil {
		t.Fatalf("SetTitle: %v", err)
	}
	hdr, msgs, err := st.Load(id)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if hdr.Title != "refactor sprint" {
		t.Errorf("Title after SetTitle = %q, want refactor sprint", hdr.Title)
	}
	// Pre-existing fields must NOT have been clobbered.
	if hdr.Model != "claude-opus-4-7" {
		t.Errorf("Model got reset to %q after SetTitle", hdr.Model)
	}
	if hdr.System != "you are helpful" {
		t.Errorf("System got reset to %q after SetTitle", hdr.System)
	}
	if len(msgs) != 1 {
		t.Errorf("messages were lost; got %d", len(msgs))
	}
}

func TestSetTitle_LatestWins(t *testing.T) {
	st := newTempStore(t)
	id := "session-1"
	_ = st.WriteHeader(id, "m", "")
	_ = st.SetTitle(id, "first")
	_ = st.SetTitle(id, "second")
	hdr, _, err := st.Load(id)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if hdr.Title != "second" {
		t.Errorf("Title = %q, want second (latest write wins)", hdr.Title)
	}
}

func TestSetTitle_RejectsEmpty(t *testing.T) {
	st := newTempStore(t)
	id := "session-1"
	_ = st.WriteHeader(id, "m", "")
	if err := st.SetTitle(id, ""); err == nil {
		t.Error("SetTitle with empty string should error")
	}
}

func TestLoadHeader_PicksUpTitleAfterMessages(t *testing.T) {
	// LoadHeader is the cheap path used by `metis sessions list`.
	// It must find titles set after messages too — title appended at any
	// point in the JSONL stream should be reflected.
	st := newTempStore(t)
	id := "session-1"
	_ = st.WriteHeader(id, "m", "")
	_ = st.AppendMessage(id, llm.Message{
		Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text", Text: "hi"}},
	})
	_ = st.SetTitle(id, "post-message-title")
	hdr, _, err := st.LoadHeader(id)
	if err != nil {
		t.Fatalf("LoadHeader: %v", err)
	}
	if hdr.Title != "post-message-title" {
		t.Errorf("LoadHeader didn't see title: got %q", hdr.Title)
	}
}

func TestSync_OnNonexistentIsNoop(t *testing.T) {
	st := newTempStore(t)
	if err := st.Sync("missing"); err != nil {
		t.Errorf("Sync on missing session should be nil; got %v", err)
	}
}

func TestSync_AfterAppend(t *testing.T) {
	st := newTempStore(t)
	id := "session-1"
	_ = st.WriteHeader(id, "m", "")
	if err := st.Sync(id); err != nil {
		t.Errorf("Sync existing session err: %v", err)
	}
	// Sanity: file actually exists after sync (not deleted).
	if _, err := os.Stat(st.path(id)); err != nil {
		t.Errorf("session file missing after Sync: %v", err)
	}
}

func TestBranch_PreservesTitleViaMergedHeader(t *testing.T) {
	// Branch copies hdr (Model/System) to the new session but doesn't
	// carry a custom title. That's intentional — a branch is a fresh
	// fork the user is presumably going to re-title differently. This
	// test pins the current behavior so a future change is intentional.
	st := newTempStore(t)
	id := "src"
	_ = st.WriteHeader(id, "claude-opus-4-7", "sys")
	_ = st.SetTitle(id, "named")
	newID, err := st.Branch(id, []llm.Message{
		{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text", Text: "hi"}}},
	})
	if err != nil {
		t.Fatalf("Branch: %v", err)
	}
	hdr, msgs, err := st.Load(newID)
	if err != nil {
		t.Fatalf("Load branched: %v", err)
	}
	if hdr.Model != "claude-opus-4-7" {
		t.Errorf("branch lost Model; got %q", hdr.Model)
	}
	if hdr.Title != "" {
		t.Errorf("branch carried over title %q; expected fresh slate", hdr.Title)
	}
	if len(msgs) != 1 {
		t.Errorf("branch dropped messages; got %d", len(msgs))
	}
}
