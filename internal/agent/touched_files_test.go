package agent

// touched_files_test.go — pins the session-scoped file activity
// scanner that /diff surfaces. Reused by the compact subsystem
// (extractRecentToolInputPaths uses the same underlying tool-name
// match), so behavior changes here ripple to the post-compact
// attachment block.

import (
	"testing"

	"github.com/Ricardo-M-L/metis/internal/llm"
)

func toolUse(id, name string, input map[string]any) llm.Message {
	return llm.Message{
		Role: llm.RoleAssistant,
		Content: []llm.ContentBlock{
			{
				Type:      "tool_use",
				ToolUseID: id,
				ToolName:  name,
				ToolInput: input,
			},
		},
	}
}

func TestTouchedFiles_Empty(t *testing.T) {
	if got := TouchedFiles(nil); got != nil {
		t.Errorf("nil messages should yield nil; got %#v", got)
	}
	if got := TouchedFiles([]llm.Message{}); got != nil {
		t.Errorf("empty messages should yield nil; got %#v", got)
	}
}

func TestTouchedFiles_CountsReadEditWritePerPath(t *testing.T) {
	msgs := []llm.Message{
		toolUse("u1", "Read", map[string]any{"path": "a.go"}),
		toolUse("u2", "Read", map[string]any{"path": "a.go"}),
		toolUse("u3", "Edit", map[string]any{"path": "b.go"}),
		toolUse("u4", "Write", map[string]any{"path": "c.go"}),
		toolUse("u5", "Edit", map[string]any{"path": "b.go"}),
		// NotebookEdit folds into Edits.
		toolUse("u6", "NotebookEdit", map[string]any{"path": "nb.ipynb"}),
	}
	got := TouchedFiles(msgs)
	if len(got) != 4 {
		t.Fatalf("expected 4 distinct paths; got %d (%#v)", len(got), got)
	}
	idx := map[string]TouchedFile{}
	for _, tf := range got {
		idx[tf.Path] = tf
	}
	if idx["a.go"].Reads != 2 || idx["a.go"].Edits != 0 || idx["a.go"].Writes != 0 {
		t.Errorf("a.go: want R=2 E=0 W=0; got %+v", idx["a.go"])
	}
	if idx["b.go"].Reads != 0 || idx["b.go"].Edits != 2 || idx["b.go"].Writes != 0 {
		t.Errorf("b.go: want R=0 E=2 W=0; got %+v", idx["b.go"])
	}
	if idx["c.go"].Writes != 1 {
		t.Errorf("c.go: want W=1; got %+v", idx["c.go"])
	}
	if idx["nb.ipynb"].Edits != 1 {
		t.Errorf("nb.ipynb: NotebookEdit should count as Edit; got %+v", idx["nb.ipynb"])
	}
}

func TestTouchedFiles_SortsModifiedFirstThenRecent(t *testing.T) {
	// Read-only file appears EARLIER than the edits. The result
	// must still surface the modified files at the top, then sort
	// the rest by recency.
	msgs := []llm.Message{
		toolUse("u1", "Read", map[string]any{"path": "old-read.go"}),
		toolUse("u2", "Edit", map[string]any{"path": "old-edit.go"}),
		toolUse("u3", "Read", map[string]any{"path": "mid-read.go"}),
		toolUse("u4", "Write", map[string]any{"path": "new-write.go"}),
	}
	got := TouchedFiles(msgs)
	if len(got) != 4 {
		t.Fatalf("expected 4; got %d", len(got))
	}
	// Modified ones first, in newest→oldest order.
	if got[0].Path != "new-write.go" {
		t.Errorf("expected new-write.go first (modified + newest); got %q", got[0].Path)
	}
	if got[1].Path != "old-edit.go" {
		t.Errorf("expected old-edit.go second (modified, older); got %q", got[1].Path)
	}
	// Then reads, newest first.
	if got[2].Path != "mid-read.go" {
		t.Errorf("expected mid-read.go third (read, newer); got %q", got[2].Path)
	}
	if got[3].Path != "old-read.go" {
		t.Errorf("expected old-read.go fourth (read, oldest); got %q", got[3].Path)
	}
}

func TestTouchedFiles_IgnoresUnrecognizedTools(t *testing.T) {
	// Bash / Grep / Glob have inputs (cmd / pattern) that are NOT
	// concrete file paths. They must NOT register as touched files
	// even when the input map happens to have a "path" field.
	msgs := []llm.Message{
		toolUse("u1", "Bash", map[string]any{"command": "ls"}),
		toolUse("u2", "Grep", map[string]any{"pattern": "func"}),
		toolUse("u3", "Glob", map[string]any{"pattern": "**/*.go"}),
		// This Bash artificially carries a path field — must still be ignored.
		toolUse("u4", "Bash", map[string]any{"path": "/tmp/x", "command": "rm /tmp/x"}),
	}
	if got := TouchedFiles(msgs); len(got) != 0 {
		t.Errorf("non-file-shaped tools must not surface as touched; got %#v", got)
	}
}

func TestTouchedFiles_FilePathFieldFallback(t *testing.T) {
	// Some tool variants name the input "file_path" instead of "path".
	// The scanner tries both (mirrors stringFieldAny in post_compact_attachments.go).
	msgs := []llm.Message{
		toolUse("u1", "Read", map[string]any{"file_path": "alt.go"}),
		toolUse("u2", "Write", map[string]any{"file_path": "alt2.go"}),
	}
	got := TouchedFiles(msgs)
	if len(got) != 2 {
		t.Fatalf("file_path fallback failed; got %d entries", len(got))
	}
}

func TestTouchedFiles_LastTurnIdxIsMessageIndex(t *testing.T) {
	// Pin that LastTurnIdx is the messages-slice index (NOT a turn
	// counter). Test catches a future refactor that re-bases the
	// index against something else.
	msgs := []llm.Message{
		toolUse("u1", "Read", map[string]any{"path": "x.go"}), // i=0
		{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text", Text: "noise"}}},
		toolUse("u2", "Read", map[string]any{"path": "x.go"}), // i=2
	}
	got := TouchedFiles(msgs)
	if len(got) != 1 || got[0].LastTurnIdx != 2 {
		t.Errorf("expected LastTurnIdx=2 (newest reference); got %+v", got)
	}
}

func TestLoop_TouchedFiles_HoldsRLock(t *testing.T) {
	// Loop.TouchedFiles must work on a freshly-built Loop (no
	// goroutine fanout, no live Run). Pins the lock release path
	// — a missing RUnlock would deadlock the next Lock.
	l := &Loop{}
	if got := l.TouchedFiles(); got != nil {
		t.Errorf("empty Loop should yield nil; got %#v", got)
	}
	// Add messages and re-check.
	l.Messages = []llm.Message{
		toolUse("u1", "Write", map[string]any{"path": "z.go"}),
	}
	got := l.TouchedFiles()
	if len(got) != 1 || got[0].Path != "z.go" {
		t.Errorf("Loop.TouchedFiles didn't see appended messages; got %#v", got)
	}
	// Take a Lock to confirm the prior call released RLock cleanly.
	l.mu.Lock()
	l.mu.Unlock()
}
