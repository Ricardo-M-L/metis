package agent

// turn_diffs_test.go — pins the claude-code-style per-turn diff
// derivation. Helpers (toolUse, msg) come from compact_test.go +
// touched_files_test.go.

import (
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/llm"
)

// userText is shorthand for the start-of-turn user prompt.
func userText(s string) llm.Message {
	return llm.Message{
		Role:    llm.RoleUser,
		Content: []llm.ContentBlock{{Type: "text", Text: s}},
	}
}

func TestTurnDiffs_EmptyMessages(t *testing.T) {
	if got := TurnDiffs(nil); got != nil {
		t.Errorf("expected nil; got %#v", got)
	}
}

func TestTurnDiffs_SingleEditOpensOneTurn(t *testing.T) {
	msgs := []llm.Message{
		userText("Refactor the auth middleware"),
		toolUse("u1", "Edit", map[string]any{
			"path": "auth.go",
			"old":  "func old() {}",
			"new":  "func new() {\n  return\n}",
		}),
	}
	got := TurnDiffs(msgs)
	if len(got) != 1 {
		t.Fatalf("expected 1 turn; got %d (%#v)", len(got), got)
	}
	tn := got[0]
	if tn.Index != 1 {
		t.Errorf("Index=%d, want 1", tn.Index)
	}
	if !strings.Contains(tn.UserPromptPreview, "Refactor the auth") {
		t.Errorf("UserPromptPreview missing prompt; got %q", tn.UserPromptPreview)
	}
	if tn.FilesChanged != 1 {
		t.Errorf("FilesChanged=%d, want 1", tn.FilesChanged)
	}
	f := tn.Files["auth.go"]
	if f == nil {
		t.Fatalf("auth.go not in Files map")
	}
	if f.IsNewFile {
		t.Errorf("auth.go should NOT be IsNewFile (Edit, not Write)")
	}
	if f.LinesAdded < 1 || f.LinesRemoved < 1 {
		t.Errorf("expected at least +1/-1 from a real Edit; got +%d/-%d", f.LinesAdded, f.LinesRemoved)
	}
}

func TestTurnDiffs_WriteFirstTimeMarksNewFile(t *testing.T) {
	msgs := []llm.Message{
		userText("Create a hello world file"),
		toolUse("u1", "Write", map[string]any{
			"path":    "/tmp/hello.txt",
			"content": "hello\nworld\n",
		}),
	}
	got := TurnDiffs(msgs)
	if len(got) != 1 {
		t.Fatalf("expected 1 turn; got %d", len(got))
	}
	f := got[0].Files["/tmp/hello.txt"]
	if f == nil {
		t.Fatalf("missing /tmp/hello.txt in Files")
	}
	if !f.IsNewFile {
		t.Errorf("first-time Write should be IsNewFile=true")
	}
	if f.LinesAdded != 2 {
		t.Errorf("LinesAdded=%d, want 2 (hello + world)", f.LinesAdded)
	}
	if f.LinesRemoved != 0 {
		t.Errorf("LinesRemoved=%d, want 0 for new file", f.LinesRemoved)
	}
}

func TestTurnDiffs_WriteAfterReadIsNotNewFile(t *testing.T) {
	// Pre-existing file: agent Reads it, then later in the same turn
	// Writes it. Write should NOT be flagged as new.
	msgs := []llm.Message{
		userText("Rewrite README.md"),
		toolUse("u1", "Read", map[string]any{"path": "README.md"}),
		toolUse("u2", "Write", map[string]any{
			"path":    "README.md",
			"content": "new content\n",
		}),
	}
	got := TurnDiffs(msgs)
	if len(got) != 1 {
		t.Fatalf("expected 1 turn; got %d", len(got))
	}
	f := got[0].Files["README.md"]
	if f == nil {
		t.Fatalf("README.md not in Files")
	}
	if f.IsNewFile {
		t.Errorf("Write after Read should NOT be IsNewFile")
	}
}

func TestTurnDiffs_MultipleTurnsNewestFirst(t *testing.T) {
	msgs := []llm.Message{
		userText("First task: fix login"),
		toolUse("u1", "Edit", map[string]any{
			"path": "login.go", "old": "x", "new": "y",
		}),
		userText("Second task: write docs"),
		toolUse("u2", "Write", map[string]any{
			"path": "DOCS.md", "content": "intro\n",
		}),
		userText("Third task: tweak login again"),
		toolUse("u3", "Edit", map[string]any{
			"path": "login.go", "old": "y", "new": "z",
		}),
	}
	got := TurnDiffs(msgs)
	if len(got) != 3 {
		t.Fatalf("expected 3 turns; got %d", len(got))
	}
	// Newest first.
	if !strings.Contains(got[0].UserPromptPreview, "Third task") {
		t.Errorf("turn[0] should be Third; got %q", got[0].UserPromptPreview)
	}
	if !strings.Contains(got[1].UserPromptPreview, "Second task") {
		t.Errorf("turn[1] should be Second; got %q", got[1].UserPromptPreview)
	}
	if !strings.Contains(got[2].UserPromptPreview, "First task") {
		t.Errorf("turn[2] should be First; got %q", got[2].UserPromptPreview)
	}
	// Index assigned chronologically: Index=3 is the newest.
	if got[0].Index != 3 {
		t.Errorf("newest turn Index=%d, want 3", got[0].Index)
	}
}

func TestTurnDiffs_ToolResultDoesNotOpenNewTurn(t *testing.T) {
	// User-role tool_result messages must NOT start a new turn —
	// they're part of the existing turn. claude-code's
	// useTurnDiffs.ts checks isToolResult before splitting.
	msgs := []llm.Message{
		userText("Edit foo"),
		toolUse("u1", "Edit", map[string]any{
			"path": "foo.go", "old": "a", "new": "b",
		}),
		// This is a user-role tool_result reply, NOT a new prompt:
		{Role: llm.RoleUser, Content: []llm.ContentBlock{
			{Type: "tool_result", ToolUseID: "u1", ToolResult: "edited foo.go"},
		}},
		// And another edit in the same turn:
		toolUse("u2", "Edit", map[string]any{
			"path": "bar.go", "old": "x", "new": "y",
		}),
	}
	got := TurnDiffs(msgs)
	if len(got) != 1 {
		t.Fatalf("tool_result must not open new turn; got %d turns", len(got))
	}
	if got[0].FilesChanged != 2 {
		t.Errorf("expected 2 files in single turn; got %d", got[0].FilesChanged)
	}
}

func TestTurnDiffs_RepeatedEditsSameFileAccumulate(t *testing.T) {
	msgs := []llm.Message{
		userText("Make 3 edits to log.go"),
		toolUse("u1", "Edit", map[string]any{"path": "log.go", "old": "a", "new": "A"}),
		toolUse("u2", "Edit", map[string]any{"path": "log.go", "old": "b", "new": "B"}),
		toolUse("u3", "Edit", map[string]any{"path": "log.go", "old": "c", "new": "C"}),
	}
	got := TurnDiffs(msgs)
	if len(got) != 1 {
		t.Fatalf("expected 1 turn")
	}
	f := got[0].Files["log.go"]
	if f == nil {
		t.Fatalf("missing log.go")
	}
	if f.EditCount != 3 {
		t.Errorf("EditCount=%d, want 3", f.EditCount)
	}
	if got[0].FilesChanged != 1 {
		t.Errorf("FilesChanged should still be 1 (same path); got %d", got[0].FilesChanged)
	}
}

func TestTurnDiffs_PromptPreviewTrimmed(t *testing.T) {
	longPrompt := "Please refactor the entire authentication subsystem to support a new OAuth flow and rewrite the docs"
	msgs := []llm.Message{
		userText(longPrompt),
		toolUse("u1", "Edit", map[string]any{"path": "a", "old": "x", "new": "y"}),
	}
	got := TurnDiffs(msgs)
	if len(got) != 1 {
		t.Fatalf("expected 1 turn")
	}
	if len(got[0].UserPromptPreview) > turnPromptPreviewLen {
		t.Errorf("preview len=%d exceeds cap %d", len(got[0].UserPromptPreview), turnPromptPreviewLen)
	}
	if !strings.HasSuffix(got[0].UserPromptPreview, "…") {
		t.Errorf("long preview should end with ellipsis; got %q", got[0].UserPromptPreview)
	}
}

func TestTurnDiffs_PreTurnReadsRecordedForLaterNewFileCheck(t *testing.T) {
	// Resume-replay scenario: the loaded messages include some
	// system-injected Reads BEFORE the first user prompt. A Write
	// later in the first turn must NOT be flagged as a new file
	// when the path was seen pre-turn.
	msgs := []llm.Message{
		// pre-turn tool_use (no user prompt yet):
		toolUse("u_pre", "Read", map[string]any{"path": "config.toml"}),
		userText("Update the config"),
		toolUse("u1", "Write", map[string]any{
			"path": "config.toml", "content": "new\n",
		}),
	}
	got := TurnDiffs(msgs)
	if len(got) != 1 {
		t.Fatalf("expected 1 turn; got %d", len(got))
	}
	f := got[0].Files["config.toml"]
	if f == nil {
		t.Fatalf("missing config.toml")
	}
	if f.IsNewFile {
		t.Errorf("pre-turn Read should prevent new-file flag; got IsNewFile=true")
	}
}

func TestLoop_TurnDiffs_HoldsRLock(t *testing.T) {
	l := &Loop{}
	if got := l.TurnDiffs(); got != nil {
		t.Errorf("empty Loop should give nil; got %#v", got)
	}
	l.Messages = []llm.Message{
		userText("hi"),
		toolUse("u1", "Edit", map[string]any{"path": "a.go", "old": "x", "new": "y"}),
	}
	if got := l.TurnDiffs(); len(got) != 1 {
		t.Errorf("expected 1 turn after appending; got %d", len(got))
	}
	// Confirm RLock released cleanly.
	l.mu.Lock()
	l.mu.Unlock()
}
