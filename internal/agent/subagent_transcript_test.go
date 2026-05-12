package agent

import (
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/llm"
	pubprov "github.com/Ricardo-M-L/metis/pkg/provider"
	pubsess "github.com/Ricardo-M-L/metis/pkg/session"
)

// helper — three-message conversation we round-trip through Save/Load.
func makeFixtureMessages() []llm.Message {
	return []llm.Message{
		{
			Role:    pubprov.RoleUser,
			Content: []pubprov.ContentBlock{{Type: "text", Text: "hi"}},
		},
		{
			Role:    pubprov.RoleAssistant,
			Content: []pubprov.ContentBlock{{Type: "text", Text: "hello there"}},
		},
		{
			Role:    pubprov.RoleUser,
			Content: []pubprov.ContentBlock{{Type: "text", Text: "what's 2+2?"}},
		},
	}
}

func TestSubAgentTranscript_SaveLoadRoundtrip(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	agentID := "agt-roundtrip"

	hdr := NewSubAgentHeader(agentID, "test-model", "parent-1", "alice", "/tmp/foo", "default")
	tr, err := NewSubAgentTranscript(dir, agentID, hdr)
	if err != nil {
		t.Fatalf("NewSubAgentTranscript: %v", err)
	}

	msgs := makeFixtureMessages()
	for _, m := range msgs {
		if err := tr.AppendMessage(m); err != nil {
			t.Fatalf("AppendMessage: %v", err)
		}
	}
	if err := tr.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	snap, err := LoadSubAgentSnapshot(dir, agentID)
	if err != nil {
		t.Fatalf("LoadSubAgentSnapshot: %v", err)
	}
	if snap.Header.ID != agentID {
		t.Errorf("Header.ID = %q, want %q", snap.Header.ID, agentID)
	}
	if snap.Header.Model != "test-model" {
		t.Errorf("Header.Model = %q, want test-model", snap.Header.Model)
	}
	if snap.Header.SubAgentOf != "parent-1" {
		t.Errorf("Header.SubAgentOf = %q, want parent-1", snap.Header.SubAgentOf)
	}
	if snap.Header.TeammateName != "alice" {
		t.Errorf("Header.TeammateName = %q, want alice", snap.Header.TeammateName)
	}
	if got, want := len(snap.Messages), len(msgs); got != want {
		t.Fatalf("len(Messages) = %d, want %d", got, want)
	}
	for i := range msgs {
		if snap.Messages[i].Role != msgs[i].Role {
			t.Errorf("msg[%d].Role mismatch", i)
		}
		if len(snap.Messages[i].Content) != len(msgs[i].Content) ||
			snap.Messages[i].Content[0].Text != msgs[i].Content[0].Text {
			t.Errorf("msg[%d].Content mismatch", i)
		}
	}
}

func TestSubAgentTranscript_NilReceiverNoop(t *testing.T) {
	t.Parallel()
	var tr *SubAgentTranscript
	if err := tr.AppendMessage(llm.Message{Role: pubprov.RoleUser}); err != nil {
		t.Errorf("nil AppendMessage should be no-op, got %v", err)
	}
	if err := tr.Close(); err != nil {
		t.Errorf("nil Close should be no-op, got %v", err)
	}
	if tr.Path() != "" {
		t.Errorf("nil Path should be empty, got %q", tr.Path())
	}
}

func TestSubAgentTranscript_MissingFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	_, err := LoadSubAgentSnapshot(dir, "agt-does-not-exist")
	if err == nil {
		t.Fatal("expected error loading missing transcript")
	}
}

func TestSubAgentTranscript_MalformedJSON(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	sub := filepath.Join(dir, SubAgentTranscriptDirname)
	if err := os.MkdirAll(sub, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(sub, "agt-bad.jsonl")
	if err := os.WriteFile(path, []byte("{not valid json}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := LoadSubAgentSnapshot(dir, "agt-bad")
	if err == nil {
		t.Fatal("expected error loading malformed transcript")
	}
}

func TestSubAgentTranscript_MissingHeader(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	sub := filepath.Join(dir, SubAgentTranscriptDirname)
	if err := os.MkdirAll(sub, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(sub, "agt-no-header.jsonl")
	// Just a message entry, no header line — should be rejected.
	body := `{"type":"message","message":{"role":"user","content":[{"type":"text","text":"hi"}]}}` + "\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := LoadSubAgentSnapshot(dir, "agt-no-header")
	if err == nil {
		t.Fatal("expected error loading transcript without header")
	}
}

func TestSubAgentTranscript_HeaderMergeOnReload(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	agentID := "agt-merge"

	hdr := NewSubAgentHeader(agentID, "m1", "parent", "bob", "/tmp/x", "default")
	tr, err := NewSubAgentTranscript(dir, agentID, hdr)
	if err != nil {
		t.Fatal(err)
	}
	tr.Close()

	// Manually append a second header entry to exercise mergeSubagentHeader.
	path := filepath.Join(dir, SubAgentTranscriptDirname, agentID+".jsonl")
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	// Simulate a /title update later in the run.
	if _, err := f.WriteString(`{"type":"header","header":{"id":"agt-merge","title":"renamed","teammate_name":"bob2"}}` + "\n"); err != nil {
		t.Fatal(err)
	}
	f.Close()

	snap, err := LoadSubAgentSnapshot(dir, agentID)
	if err != nil {
		t.Fatalf("LoadSubAgentSnapshot: %v", err)
	}
	if snap.Header.Title != "renamed" {
		t.Errorf("merged Title = %q, want renamed", snap.Header.Title)
	}
	if snap.Header.TeammateName != "bob2" {
		t.Errorf("merged TeammateName = %q, want bob2", snap.Header.TeammateName)
	}
	if snap.Header.Model != "m1" {
		t.Errorf("Model should survive merge unchanged, got %q", snap.Header.Model)
	}
}

func TestListSubAgentTranscripts(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	// Empty / non-existent subagents dir → nil slice no error.
	ids, err := ListSubAgentTranscripts(dir)
	if err != nil {
		t.Fatalf("ListSubAgentTranscripts on empty: %v", err)
	}
	if len(ids) != 0 {
		t.Fatalf("expected 0 ids on empty dir, got %v", ids)
	}

	// Create two transcripts.
	want := []string{"agt-aaa", "agt-bbb"}
	for _, id := range want {
		hdr := NewSubAgentHeader(id, "m", "p", "", "", "default")
		tr, err := NewSubAgentTranscript(dir, id, hdr)
		if err != nil {
			t.Fatal(err)
		}
		tr.Close()
	}

	got, err := ListSubAgentTranscripts(dir)
	if err != nil {
		t.Fatalf("ListSubAgentTranscripts: %v", err)
	}
	sort.Strings(got)
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

func TestNewSubAgentHeader_FieldsPopulated(t *testing.T) {
	t.Parallel()
	hdr := NewSubAgentHeader("agt-1", "m", "parent-id", "alice", "/wd", "bypass")
	if hdr.ID != "agt-1" {
		t.Error("ID not set")
	}
	if hdr.Model != "m" {
		t.Error("Model not set")
	}
	if hdr.SubAgentOf != "parent-id" {
		t.Error("SubAgentOf not set")
	}
	if hdr.TeammateName != "alice" {
		t.Error("TeammateName not set")
	}
	if hdr.WorkDir != "/wd" {
		t.Error("WorkDir not set")
	}
	if hdr.Mode != "bypass" {
		t.Error("Mode not set")
	}
	if hdr.CreatedAt.IsZero() {
		t.Error("CreatedAt should be populated")
	}
}

// Ensure the header field is reachable through the pubsess alias too.
func TestSubAgentHeader_AliasInteropt(t *testing.T) {
	t.Parallel()
	var h pubsess.Header = NewSubAgentHeader("agt-x", "m", "p", "n", "/", "default")
	if h.SubAgentOf != "p" {
		t.Error("SubAgentOf not reachable via pubsess.Header alias")
	}
}
