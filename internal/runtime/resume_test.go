package runtime

import (
	"bytes"
	"path/filepath"
	"strings"
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
		Mode:  "acceptEdits",
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
	if gate.Mode() != permission.ModeAcceptEdits {
		t.Errorf("mode not restored: got %q", gate.Mode())
	}
	rules := gate.Snapshot()
	if len(rules) != 1 || rules[0].Source != "session:resumed(user-allow)" {
		t.Errorf("resumed rule not normalized to session lifetime: %+v", rules)
	}
}

func TestPrepareResume_ExposesProviderModelAndSystemBeforeApply(t *testing.T) {
	store := newResumeStore(t)
	const id = "session-prepare-header"
	if err := store.WriteHeaderFull(session.Header{
		ID:       id,
		Provider: "openai",
		Model:    "stored-model",
		System:   "stored system prompt",
		Mode:     "acceptEdits",
	}); err != nil {
		t.Fatalf("WriteHeaderFull: %v", err)
	}
	if err := store.AppendMessage(id, llm.Message{
		Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text", Text: "stored turn"}},
	}); err != nil {
		t.Fatalf("AppendMessage: %v", err)
	}

	prepared, err := PrepareResume(store, id)
	if err != nil {
		t.Fatalf("PrepareResume: %v", err)
	}
	if prepared.SessionID != id || prepared.Header.Provider != "openai" ||
		prepared.Header.Model != "stored-model" || prepared.Header.System != "stored system prompt" {
		t.Fatalf("prepared header = %+v", prepared)
	}

	loop := agent.NewLoop(nil, tools.NewRegistry(), permission.New(permission.ModeAsk), nil, "current system", 5)
	gate := permission.New(permission.ModeAsk)
	if len(loop.Messages) != 0 {
		t.Fatal("PrepareResume mutated loop before ApplyPreparedResume")
	}
	if _, err := ApplyPreparedResume(prepared, loop, gate, nil); err != nil {
		t.Fatalf("ApplyPreparedResume: %v", err)
	}
	if len(loop.Messages) != 1 || loop.Messages[0].Content[0].Text != "stored turn" {
		t.Fatalf("prepared messages not applied: %+v", loop.Messages)
	}
}

func TestPrepareResumeExposesProviderModelAndSystemBeforeRuntimeBuild(t *testing.T) {
	store := newResumeStore(t)
	const id = "prepare-provider"
	wantHeader := session.Header{
		ID: id, Provider: "openai", Model: "gpt-resumed", System: "resumed-system", Mode: "ask",
	}
	if err := store.WriteHeaderFull(wantHeader); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendMessage(id, llm.Message{
		Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text", Text: "resumed prompt"}},
	}); err != nil {
		t.Fatal(err)
	}

	prepared, err := PrepareResume(store, id)
	if err != nil {
		t.Fatal(err)
	}
	if prepared.SessionID != id || prepared.Header.Provider != "openai" || prepared.Header.Model != "gpt-resumed" || prepared.Header.System != "resumed-system" {
		t.Fatalf("prepared resume lost runtime-selection fields: %+v", prepared)
	}
	if len(prepared.messages) != 1 || prepared.messages[0].Content[0].Text != "resumed prompt" {
		t.Fatalf("prepared transcript = %+v", prepared.messages)
	}
}

func TestApplyResume_DropsPreviousSessionRules(t *testing.T) {
	store := newResumeStore(t)
	id := "session-resume-clean-boundary"
	if err := store.WriteHeaderFull(session.Header{
		ID: id, Mode: "ask",
		AlwaysAllow: []session.SavedRule{{
			Tool: "Read", Verb: int(permission.DecisionAllow), Source: "config:allow",
		}},
	}); err != nil {
		t.Fatalf("WriteHeaderFull: %v", err)
	}
	loop := agent.NewLoop(nil, tools.NewRegistry(), permission.New(permission.ModeAsk), nil, "sys", 5)
	gate := permission.New(permission.ModeBypass)
	gate.AppendRules(
		permission.Rule{Tool: "Edit", Verb: permission.DecisionAllow, Source: "interactive"},
		permission.Rule{Tool: "Bash", Verb: permission.DecisionDeny, Source: "policy:deny"},
	)

	if _, err := ApplyResume(store, id, loop, gate, nil); err != nil {
		t.Fatalf("ApplyResume: %v", err)
	}
	rules := gate.Snapshot()
	if len(rules) != 2 {
		t.Fatalf("rules = %+v, want policy base + resumed destination", rules)
	}
	for _, rule := range rules {
		if rule.Tool == "Edit" {
			t.Fatalf("previous interactive grant leaked: %+v", rules)
		}
	}
	if rules[1].Tool != "Read" || !strings.HasPrefix(rules[1].Source, "session:") {
		t.Fatalf("destination rule not session scoped: %+v", rules)
	}
}

func TestApplyResume_MissingSessionErrors(t *testing.T) {
	store := newResumeStore(t)
	loop := agent.NewLoop(nil, tools.NewRegistry(), permission.New(permission.ModeAcceptEdits), nil, "sys", 5)
	gate := permission.New(permission.ModeAsk)
	if _, err := ApplyResume(store, "does-not-exist", loop, gate, nil); err == nil {
		t.Error("ApplyResume for missing session should error")
	}
}

func TestApplyResume_RejectsUnsafeSessionIDBeforePathLookup(t *testing.T) {
	store := newResumeStore(t)
	loop := agent.NewLoop(nil, tools.NewRegistry(), permission.New(permission.ModeAsk), nil, "sys", 5)
	gate := permission.New(permission.ModeAsk)
	for _, id := range []string{"", ".", "..", "../target", `..\target`, "nested/target", "target\nforged"} {
		t.Run(strings.ReplaceAll(id, "/", "_"), func(t *testing.T) {
			if _, err := ApplyResume(store, id, loop, gate, nil); err == nil || !strings.Contains(err.Error(), "invalid session id") {
				t.Fatalf("ApplyResume(%q) error = %v, want invalid session id", id, err)
			}
		})
	}
}

func TestApplyResume_AllowsSafeLegacyImportedID(t *testing.T) {
	store := newResumeStore(t)
	const id = "旧 session name"
	if err := store.WriteHeaderFull(session.Header{ID: id, Model: "legacy"}); err != nil {
		t.Fatal(err)
	}
	loop := agent.NewLoop(nil, tools.NewRegistry(), permission.New(permission.ModeAsk), nil, "sys", 5)
	gate := permission.New(permission.ModeAsk)
	if _, err := ApplyResume(store, id, loop, gate, nil); err != nil {
		t.Fatalf("safe legacy id should remain resumable: %v", err)
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
	if gate.Mode() != permission.ModeBypassPermissions {
		t.Errorf("blank header mode shouldn't change gate; got %q", gate.Mode())
	}
}

func TestWriteFreshHeader_RoundTrip(t *testing.T) {
	store := newResumeStore(t)
	id := store.NewSessionID()
	if err := WriteFreshHeader(store, id, "anthropic", "claude-x", "you are helpful", "auto"); err != nil {
		t.Fatalf("WriteFreshHeader: %v", err)
	}
	hdr, _, err := store.LoadHeader(id)
	if err != nil {
		t.Fatalf("LoadHeader: %v", err)
	}
	if hdr.ID != id || hdr.Provider != "anthropic" || hdr.Model != "claude-x" || hdr.Mode != "auto" || hdr.System != "you are helpful" {
		t.Errorf("header didn't round-trip: %+v", hdr)
	}
}
