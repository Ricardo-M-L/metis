package runtime

import (
	"bytes"
	"path/filepath"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/session"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

func newResumeStore(t *testing.T) *session.Store {
	t.Helper()
	st, err := session.NewStore(filepath.Join(t.TempDir(), "sessions"))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	return st
}

func TestApplyResume_RestoresMessagesAndMode(t *testing.T) {
	store := newResumeStore(t)
	id := "session-resume-1"

	// Persist a session with messages + mode + always-allow rules.
	hdr := session.Header{
		ID:    id,
		Model: "claude-x",
		Mode:  "auto",
		AlwaysAllow: []session.SavedRule{
			{Tool: "Bash", Match: "git status", Verb: int(permission.DecisionAllow), Source: "user-allow"},
		},
	}
	if err := store.WriteHeaderFull(hdr); err != nil {
		t.Fatalf("WriteHeaderFull: %v", err)
	}
	if err := store.AppendMessage(id, llm.Message{
		Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text", Text: "first prompt"}},
	}); err != nil {
		t.Fatalf("AppendMessage: %v", err)
	}

	// Build a fresh loop + gate; ApplyResume should populate them.
	loop := agent.NewLoop(nil, tools.NewRegistry(), permission.New(permission.ModeAsk), nil, "sys", 10)
	gate := permission.New(permission.ModeAsk)

	var warnBuf bytes.Buffer
	res, err := ApplyResume(store, id, loop, gate, &warnBuf)
	if err != nil {
		t.Fatalf("ApplyResume: %v", err)
	}
	if res.SessionID != id {
		t.Errorf("SessionID = %q, want %q", res.SessionID, id)
	}
	if len(loop.Messages) != 1 || loop.Messages[0].Content[0].Text != "first prompt" {
		t.Errorf("messages not restored: %+v", loop.Messages)
	}
	if string(gate.Mode()) != "auto" {
		t.Errorf("mode not restored: got %q", gate.Mode())
	}
}

func TestApplyResume_MissingSessionErrors(t *testing.T) {
	store := newResumeStore(t)
	loop := agent.NewLoop(nil, tools.NewRegistry(), permission.New(permission.ModeAuto), nil, "sys", 5)
	gate := permission.New(permission.ModeAsk)
	if _, err := ApplyResume(store, "does-not-exist", loop, gate, nil); err == nil {
		t.Error("ApplyResume for missing session should error")
	}
}

func TestApplyResume_KeepsExistingModeWhenHeaderIsBlank(t *testing.T) {
	store := newResumeStore(t)
	id := "session-no-mode"
	if err := store.WriteHeader(id, "claude-x", ""); err != nil {
		t.Fatalf("WriteHeader: %v", err)
	}
	loop := agent.NewLoop(nil, tools.NewRegistry(), permission.New(permission.ModeBypass), nil, "sys", 5)
	gate := permission.New(permission.ModeBypass)
	if _, err := ApplyResume(store, id, loop, gate, nil); err != nil {
		t.Fatalf("ApplyResume: %v", err)
	}
	if string(gate.Mode()) != "bypass" {
		t.Errorf("blank header mode shouldn't change gate; got %q", gate.Mode())
	}
}

func TestWriteFreshHeader_RoundTrip(t *testing.T) {
	store := newResumeStore(t)
	id := store.NewSessionID()
	if err := WriteFreshHeader(store, id, "claude-x", "you are helpful", "auto"); err != nil {
		t.Fatalf("WriteFreshHeader: %v", err)
	}
	hdr, _, err := store.LoadHeader(id)
	if err != nil {
		t.Fatalf("LoadHeader: %v", err)
	}
	if hdr.ID != id || hdr.Model != "claude-x" || hdr.Mode != "auto" || hdr.System != "you are helpful" {
		t.Errorf("header didn't round-trip: %+v", hdr)
	}
}
