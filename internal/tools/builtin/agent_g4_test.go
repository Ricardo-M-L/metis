package builtin

// agent_g4_test.go — locks Phase G.4 (sub-agent resume on disk,
// 2026-05-12). Pairs with internal/agent/subagent_transcript.go (the
// persistence layer) and agent.go::Execute resume_from branch.
//
// Six contracts pinned:
//
//   1. Foreground spawn with persistence wired writes a transcript file
//      whose path matches <sessionDir>/subagents/<agentID>.jsonl.
//   2. resume_from on a fresh persistence layer (no snapshot exists)
//      → IsError naming the missing file. Roster slot NOT consumed.
//   3. resume_from without WithSessionPersistence → IsError
//      ("requires session persistence") so the model gets a clear
//      capability message rather than a mysterious "file not found".
//   4. resume_from happy path: prior transcript exists → AgentID
//      preserved, TeammateName preserved, recovered messages
//      restored before the new prompt is appended.
//   5. Corrupted transcript file → IsError surfaces the parse error
//      so the model can decide whether to retry or give up.
//   6. Anonymous resume (no `name` on the resume call, but original
//      had a TeammateName) — the prior teammate name comes back so
//      /agents resume alice 还原成 alice not _anon-xxx.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/tools"
	pubprov "github.com/Ricardo-M-L/metis/pkg/provider"
)

// TestAgentExecute_PersistsTranscript — happy path: foreground spawn
// with session persistence wired creates the JSONL file at the
// expected path and writes the header.
func TestAgentExecute_PersistsTranscript(t *testing.T) {
	dir := t.TempDir()
	roster := agent.NewRoster(0)
	tool := NewAgent(permission.New(permission.ModeBypass), helloProvider(), tools.NewRegistry(), "test-model", "test-system").
		WithRoster(roster).
		WithSessionPersistence(dir, "parent-session-id")

	res, err := tool.Execute(context.Background(), map[string]any{"prompt": "do something"})
	if err != nil {
		t.Fatalf("Execute err: %v", err)
	}
	if res.IsError {
		t.Fatalf("expected success; got IsError: %s", res.Output)
	}

	// Locate the freshly-written transcript.
	subDir := filepath.Join(dir, agent.SubAgentTranscriptDirname)
	entries, err := os.ReadDir(subDir)
	if err != nil {
		t.Fatalf("ReadDir %s: %v", subDir, err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 transcript file, got %d (%v)", len(entries), entries)
	}
	if !strings.HasSuffix(entries[0].Name(), ".jsonl") {
		t.Errorf("transcript should be .jsonl; got %s", entries[0].Name())
	}

	agentID := strings.TrimSuffix(entries[0].Name(), ".jsonl")
	snap, err := agent.LoadSubAgentSnapshot(dir, agentID)
	if err != nil {
		t.Fatalf("LoadSubAgentSnapshot: %v", err)
	}
	if snap.Header.SubAgentOf != "parent-session-id" {
		t.Errorf("Header.SubAgentOf = %q, want parent-session-id", snap.Header.SubAgentOf)
	}
	if snap.Header.Model != "test-model" {
		t.Errorf("Header.Model = %q, want test-model", snap.Header.Model)
	}
}

// TestAgentExecute_ResumeMissingFile — resume_from when no transcript
// exists must IsError with the missing-file message and must NOT
// consume a Roster slot.
func TestAgentExecute_ResumeMissingFile(t *testing.T) {
	dir := t.TempDir()
	roster := agent.NewRoster(0)
	tool := NewAgent(permission.New(permission.ModeBypass), helloProvider(), tools.NewRegistry(), "m", "s").
		WithRoster(roster).
		WithSessionPersistence(dir, "p")

	res, err := tool.Execute(context.Background(), map[string]any{
		"prompt":      "carry on",
		"resume_from": "agt-doesnotexist",
	})
	if err != nil {
		t.Fatalf("Execute err: %v", err)
	}
	if !res.IsError {
		t.Fatalf("missing transcript must be IsError; got %+v", res)
	}
	if !strings.Contains(res.Output, "agt-doesnotexist") {
		t.Errorf("error should name the missing id; got %q", res.Output)
	}
	// Roster slot must not be consumed by a failed resume.
	if c := roster.Count(); c != 0 {
		t.Errorf("Roster.Count = %d after failed resume, want 0", c)
	}
}

// TestAgentExecute_ResumeRequiresPersistence — without
// WithSessionPersistence the resume_from path must return a
// capability-error, not a low-level file-not-found.
func TestAgentExecute_ResumeRequiresPersistence(t *testing.T) {
	roster := agent.NewRoster(0)
	tool := NewAgent(permission.New(permission.ModeBypass), helloProvider(), tools.NewRegistry(), "m", "s").
		WithRoster(roster) // NO WithSessionPersistence

	res, err := tool.Execute(context.Background(), map[string]any{
		"prompt":      "x",
		"resume_from": "agt-any",
	})
	if err != nil {
		t.Fatalf("Execute err: %v", err)
	}
	if !res.IsError {
		t.Fatalf("resume w/o persistence must be IsError; got %+v", res)
	}
	if !strings.Contains(res.Output, "session persistence") {
		t.Errorf("error should mention 'session persistence'; got %q", res.Output)
	}
}

// TestAgentExecute_ResumeRestoresHistory — write a fixture transcript
// with two prior turns and a teammate name, then resume from it.
// The reconstructed sub-loop must (a) keep the AgentID, (b) reclaim
// the TeammateName, (c) replay the historical messages before the
// new prompt is appended.
func TestAgentExecute_ResumeRestoresHistory(t *testing.T) {
	dir := t.TempDir()

	// Plant a fixture transcript with an Anonymous=false teammate "alice".
	agentID := "agt-resume-target"
	hdr := agent.NewSubAgentHeader(agentID, "fixture-model", "fixture-parent", "alice", "/tmp/fx", "default")
	tr, err := agent.NewSubAgentTranscript(dir, agentID, hdr)
	if err != nil {
		t.Fatalf("plant transcript: %v", err)
	}
	historic := []llm.Message{
		{Role: pubprov.RoleUser, Content: []pubprov.ContentBlock{{Type: "text", Text: "first round"}}},
		{Role: pubprov.RoleAssistant, Content: []pubprov.ContentBlock{{Type: "text", Text: "first answer"}}},
	}
	for _, m := range historic {
		if err := tr.AppendMessage(m); err != nil {
			t.Fatal(err)
		}
	}
	tr.Close()

	roster := agent.NewRoster(0)
	tool := NewAgent(permission.New(permission.ModeBypass), helloProvider(), tools.NewRegistry(), "m", "s").
		WithRoster(roster).
		WithSessionPersistence(dir, "another-parent")

	res, err := tool.Execute(context.Background(), map[string]any{
		"prompt":      "follow-up question",
		"resume_from": agentID,
	})
	if err != nil {
		t.Fatalf("Execute err: %v", err)
	}
	if res.IsError {
		t.Fatalf("resume should succeed; got IsError: %s", res.Output)
	}

	// Reload the transcript and confirm the historic messages are
	// preserved + the new user prompt + assistant reply are appended.
	snap, err := agent.LoadSubAgentSnapshot(dir, agentID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if snap.Header.ID != agentID {
		t.Errorf("AgentID drifted: got %q want %q", snap.Header.ID, agentID)
	}
	if snap.Header.TeammateName != "alice" {
		t.Errorf("TeammateName not restored from snapshot: got %q", snap.Header.TeammateName)
	}
	// Must contain at least the 2 historic + 1 new user + 1 new assistant.
	if len(snap.Messages) < 4 {
		t.Fatalf("expected ≥4 messages after resume (2 historic + ≥2 new); got %d", len(snap.Messages))
	}
	if snap.Messages[0].Content[0].Text != "first round" {
		t.Errorf("first historic user message lost; got %q", snap.Messages[0].Content[0].Text)
	}
	if snap.Messages[1].Content[0].Text != "first answer" {
		t.Errorf("first historic assistant message lost; got %q", snap.Messages[1].Content[0].Text)
	}
	// The new prompt should appear after the historic block.
	foundNewPrompt := false
	for _, m := range snap.Messages[2:] {
		if m.Role == pubprov.RoleUser && len(m.Content) > 0 && m.Content[0].Text == "follow-up question" {
			foundNewPrompt = true
			break
		}
	}
	if !foundNewPrompt {
		t.Errorf("follow-up prompt not appended after historic messages")
	}
}

// TestAgentExecute_ResumeCorruptedFile — partial / corrupted transcript
// must surface a parse error to the model, not crash.
func TestAgentExecute_ResumeCorruptedFile(t *testing.T) {
	dir := t.TempDir()
	subDir := filepath.Join(dir, agent.SubAgentTranscriptDirname)
	if err := os.MkdirAll(subDir, 0o700); err != nil {
		t.Fatal(err)
	}
	corrupted := filepath.Join(subDir, "agt-corrupt.jsonl")
	// First line valid header, second line garbage.
	body := `{"type":"header","header":{"id":"agt-corrupt","model":"m"}}` + "\n" +
		`this is not json at all` + "\n"
	if err := os.WriteFile(corrupted, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	roster := agent.NewRoster(0)
	tool := NewAgent(permission.New(permission.ModeBypass), helloProvider(), tools.NewRegistry(), "m", "s").
		WithRoster(roster).
		WithSessionPersistence(dir, "p")

	res, err := tool.Execute(context.Background(), map[string]any{
		"prompt":      "ignore me",
		"resume_from": "agt-corrupt",
	})
	if err != nil {
		t.Fatalf("Execute err: %v", err)
	}
	if !res.IsError {
		t.Fatalf("corrupted transcript must be IsError; got %+v", res)
	}
	if !strings.Contains(res.Output, "agt-corrupt") {
		t.Errorf("error should name the bad transcript; got %q", res.Output)
	}
	if roster.Count() != 0 {
		t.Errorf("Roster.Count should stay 0 after corrupt-resume; got %d", roster.Count())
	}
}

// TestAgentExecute_ResumeReclaimsTeammateName — the call site doesn't
// pass a `name`, but the on-disk snapshot has TeammateName="bob". The
// reconstructed teammate should come back as bob, not _anon-xxx.
func TestAgentExecute_ResumeReclaimsTeammateName(t *testing.T) {
	dir := t.TempDir()

	agentID := "agt-bob-paused"
	hdr := agent.NewSubAgentHeader(agentID, "m", "p", "bob", "/tmp", "default")
	tr, err := agent.NewSubAgentTranscript(dir, agentID, hdr)
	if err != nil {
		t.Fatal(err)
	}
	tr.Close()

	roster := agent.NewRoster(0)
	tool := NewAgent(permission.New(permission.ModeBypass), helloProvider(), tools.NewRegistry(), "m", "s").
		WithRoster(roster).
		WithSessionPersistence(dir, "parent")

	// Background so the teammate is still in the Roster when we inspect.
	res, err := tool.Execute(context.Background(), map[string]any{
		"prompt":            "second look",
		"resume_from":       agentID,
		"run_in_background": true,
	})
	if err != nil {
		t.Fatalf("Execute err: %v", err)
	}
	if res.IsError {
		t.Fatalf("resume should succeed; got %+v", res)
	}
	tm, ok := roster.Lookup("bob")
	if !ok {
		t.Fatalf("Roster should have reclaimed 'bob' from snapshot")
	}
	if tm.AgentID != agentID {
		t.Errorf("AgentID drifted on resume: got %q want %q", tm.AgentID, agentID)
	}
	if tm.Anonymous {
		t.Errorf("resumed named teammate must NOT be Anonymous")
	}
	roster.CancelAll()
}
