package agent

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/checkpoint"
	"github.com/Ricardo-M-L/metis/internal/llm"
)

// TestRewind_RestoresFilesAndConversation is the end-to-end unified
// rewind: a pre-edit snapshot is taken, the turn mutates a file + appends
// an assistant reply, then Rewind reverts BOTH the file and the
// conversation to the pre-edit point.
func TestRewind_RestoresFilesAndConversation(t *testing.T) {
	cwd := t.TempDir()
	shadow := t.TempDir()
	f := filepath.Join(cwd, "code.txt")
	if err := os.WriteFile(f, []byte("v1"), 0o644); err != nil {
		t.Fatal(err)
	}

	l := &Loop{Checkpointer: checkpoint.NewManager("sess-rewind", cwd, shadow), ckptSnappedAt: -1}
	// Turn 1 begins: the user asks for an edit.
	l.Messages = []llm.Message{msg(llm.RoleUser, "edit the file")}

	// Before the first mutating tool, snap the working tree (captures v1).
	l.snapPreEdit("Edit", map[string]any{"path": f})
	if !l.HasRewindPoints() {
		t.Fatal("expected a rewind point after snapPreEdit")
	}

	// The turn performs the edit and the assistant replies.
	if err := os.WriteFile(f, []byte("v2-EDITED"), 0o644); err != nil {
		t.Fatal(err)
	}
	l.Messages = append(l.Messages, msg(llm.RoleAssistant, "done editing"))

	// Rewind.
	res, ok := l.Rewind()
	if !ok {
		t.Fatal("Rewind returned ok=false")
	}
	if res.TurnsUndone < 1 {
		t.Errorf("expected at least 1 turn undone, got %d", res.TurnsUndone)
	}

	// File reverted to v1.
	b, _ := os.ReadFile(f)
	if string(b) != "v1" {
		t.Errorf("file not restored: got %q want \"v1\"", b)
	}
	// Conversation rewound (the turn is gone).
	if l.CountTurns() != 0 {
		t.Errorf("conversation not undone: %d turns remain", l.CountTurns())
	}
	// Stack empty; nothing more to rewind.
	if l.HasRewindPoints() {
		t.Error("rewind point should have been consumed")
	}
}

// TestRewind_OncePerTurn — snapPreEdit only snaps the first mutating tool
// of a turn, not every one.
func TestRewind_OncePerTurn(t *testing.T) {
	cwd := t.TempDir()
	shadow := t.TempDir()
	if err := os.WriteFile(filepath.Join(cwd, "x"), []byte("a"), 0o644); err != nil {
		t.Fatal(err)
	}
	l := &Loop{Checkpointer: checkpoint.NewManager("sess-once", cwd, shadow), ckptSnappedAt: -1}
	l.Messages = []llm.Message{msg(llm.RoleUser, "do edits")}

	l.snapPreEdit("Edit", map[string]any{"n": 1})
	l.snapPreEdit("Write", map[string]any{"n": 2})
	l.snapPreEdit("Bash", map[string]any{"n": 3})

	l.ckptMu.Lock()
	n := len(l.ckptStack)
	l.ckptMu.Unlock()
	if n != 1 {
		t.Errorf("expected exactly 1 snapshot for the turn, got %d", n)
	}
}

// TestRewind_NoCheckpointerDisabled — Rewind is a graceful no-op without
// a Checkpointer.
func TestRewind_NoCheckpointerDisabled(t *testing.T) {
	l := &Loop{ckptSnappedAt: -1}
	l.snapPreEdit("Edit", map[string]any{}) // no panic, no-op
	if _, ok := l.Rewind(); ok {
		t.Error("Rewind should be a no-op without a Checkpointer")
	}
}
