package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/checkpoint"
	"github.com/Ricardo-M-L/metis/internal/llm"
)

func appendCompletedTurn(l *Loop, user, assistant string) {
	l.Messages = append(l.Messages,
		msg(llm.RoleUser, user),
		msg(llm.RoleAssistant, assistant),
	)
}

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
	l.recordCheckpointMutation("Edit", map[string]any{"path": f}, nil, nil)
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

func TestRewindPoints_ListsEveryUserPromptNewestFirst(t *testing.T) {
	l := &Loop{ckptSnappedAt: -1}
	appendCompletedTurn(l, "first request", "first answer")
	appendCompletedTurn(l, "second request", "second answer")

	points := l.RewindPoints()
	if len(points) != 2 {
		t.Fatalf("RewindPoints len=%d, want 2: %+v", len(points), points)
	}
	if points[0].Turn != 2 || points[0].Prompt != "second request" {
		t.Fatalf("newest point=%+v, want turn 2 / second request", points[0])
	}
	if points[1].Turn != 1 || points[1].Prompt != "first request" {
		t.Fatalf("oldest point=%+v, want turn 1 / first request", points[1])
	}
}

func TestRewindToTurn_ConversationOnlyKeepsCodeAndPrefillsPrompt(t *testing.T) {
	cwd := t.TempDir()
	shadow := t.TempDir()
	f := filepath.Join(cwd, "code.txt")
	if err := os.WriteFile(f, []byte("v1"), 0o644); err != nil {
		t.Fatal(err)
	}
	l := &Loop{Checkpointer: checkpoint.NewManager("conversation-only", cwd, shadow), ckptSnappedAt: -1}
	appendCompletedTurn(l, "make v2", "done")
	l.snapPreEdit("Edit", map[string]any{"path": f})
	if err := os.WriteFile(f, []byte("v2"), 0o644); err != nil {
		t.Fatal(err)
	}
	l.recordCheckpointMutation("Edit", map[string]any{"path": f}, nil, nil)
	appendCompletedTurn(l, "explain it", "explanation")

	res, err := l.RewindToTurn(1, RewindConversation)
	if err != nil {
		t.Fatalf("RewindToTurn: %v", err)
	}
	if res.Prompt != "make v2" || res.TurnsUndone != 2 || !res.ConversationRestored || res.CodeRestored {
		t.Fatalf("result=%+v", res)
	}
	if got := l.CountTurns(); got != 0 {
		t.Fatalf("turns after conversation rewind=%d, want 0", got)
	}
	if body, _ := os.ReadFile(f); string(body) != "v2" {
		t.Fatalf("conversation-only changed code to %q, want v2", body)
	}
}

func TestRewindToTurn_CodeOnlyKeepsConversation(t *testing.T) {
	cwd := t.TempDir()
	shadow := t.TempDir()
	f := filepath.Join(cwd, "code.txt")
	if err := os.WriteFile(f, []byte("v1"), 0o644); err != nil {
		t.Fatal(err)
	}
	l := &Loop{Checkpointer: checkpoint.NewManager("code-only", cwd, shadow), ckptSnappedAt: -1}
	l.Messages = append(l.Messages, msg(llm.RoleUser, "make v2"))
	l.snapPreEdit("Edit", map[string]any{"path": f})
	if err := os.WriteFile(f, []byte("v2"), 0o644); err != nil {
		t.Fatal(err)
	}
	l.recordCheckpointMutation("Edit", map[string]any{"path": f}, nil, nil)
	l.Messages = append(l.Messages, msg(llm.RoleAssistant, "done"))
	appendCompletedTurn(l, "explain it", "explanation")

	res, err := l.RewindToTurn(1, RewindCode)
	if err != nil {
		t.Fatalf("RewindToTurn: %v", err)
	}
	if !res.CodeRestored || res.ConversationRestored || res.TurnsUndone != 0 {
		t.Fatalf("result=%+v", res)
	}
	if got := l.CountTurns(); got != 2 {
		t.Fatalf("code-only turns=%d, want 2", got)
	}
	if body, _ := os.ReadFile(f); string(body) != "v1" {
		t.Fatalf("code-only restored %q, want v1", body)
	}
}

func TestRewindToTurn_BothCanChooseOlderCheckpoint(t *testing.T) {
	cwd := t.TempDir()
	shadow := t.TempDir()
	f := filepath.Join(cwd, "code.txt")
	if err := os.WriteFile(f, []byte("v1"), 0o644); err != nil {
		t.Fatal(err)
	}
	l := &Loop{Checkpointer: checkpoint.NewManager("older-point", cwd, shadow), ckptSnappedAt: -1}
	l.Messages = append(l.Messages, msg(llm.RoleUser, "make v2"))
	l.snapPreEdit("Edit", map[string]any{"path": f})
	_ = os.WriteFile(f, []byte("v2"), 0o644)
	l.recordCheckpointMutation("Edit", map[string]any{"path": f}, nil, nil)
	l.Messages = append(l.Messages, msg(llm.RoleAssistant, "v2 done"))
	l.Messages = append(l.Messages, msg(llm.RoleUser, "make v3"))
	l.snapPreEdit("Edit", map[string]any{"path": f})
	_ = os.WriteFile(f, []byte("v3"), 0o644)
	l.recordCheckpointMutation("Edit", map[string]any{"path": f}, nil, nil)
	l.Messages = append(l.Messages, msg(llm.RoleAssistant, "v3 done"))

	res, err := l.RewindToTurn(1, RewindCodeAndConversation)
	if err != nil {
		t.Fatalf("RewindToTurn: %v", err)
	}
	if !res.CodeRestored || !res.ConversationRestored || res.Prompt != "make v2" {
		t.Fatalf("result=%+v", res)
	}
	if body, _ := os.ReadFile(f); string(body) != "v1" {
		t.Fatalf("restored %q, want v1", body)
	}
	if got := l.CountTurns(); got != 0 {
		t.Fatalf("turns=%d, want 0", got)
	}
}

func TestRewindToTurn_CodeOnlyCanMoveAcrossMultipleCheckpoints(t *testing.T) {
	cwd := t.TempDir()
	f := filepath.Join(cwd, "code.txt")
	_ = os.WriteFile(f, []byte("v1"), 0o600)
	l := &Loop{Checkpointer: checkpoint.NewManager("multi-code-only", cwd, t.TempDir()), ckptSnappedAt: -1}
	l.Messages = append(l.Messages, msg(llm.RoleUser, "make v2"))
	l.snapPreEdit("Edit", map[string]any{"path": f})
	_ = os.WriteFile(f, []byte("v2"), 0o600)
	l.recordCheckpointMutation("Edit", map[string]any{"path": f}, nil, nil)
	l.Messages = append(l.Messages, msg(llm.RoleAssistant, "v2 done"), msg(llm.RoleUser, "make v3"))
	l.snapPreEdit("Edit", map[string]any{"path": f})
	_ = os.WriteFile(f, []byte("v3"), 0o600)
	l.recordCheckpointMutation("Edit", map[string]any{"path": f}, nil, nil)
	l.Messages = append(l.Messages, msg(llm.RoleAssistant, "v3 done"))

	if _, err := l.RewindToTurn(2, RewindCode); err != nil {
		t.Fatalf("rewind to v2: %v", err)
	}
	if body, _ := os.ReadFile(f); string(body) != "v2" {
		t.Fatalf("first code restore=%q, want v2", body)
	}
	if _, err := l.RewindToTurn(1, RewindCode); err != nil {
		t.Fatalf("rewind to v1 after v2: %v", err)
	}
	if body, _ := os.ReadFile(f); string(body) != "v1" {
		t.Fatalf("second code restore=%q, want v1", body)
	}
	_ = os.WriteFile(f, []byte("user edit"), 0o600)
	if _, err := l.RewindToTurn(2, RewindCode); !errors.Is(err, checkpoint.ErrManagedPathChanged) {
		t.Fatalf("independent edit err=%v, want ErrManagedPathChanged", err)
	}
}

func TestRewindToTurn_StaleConversationDoesNotModifyFiles(t *testing.T) {
	cwd := t.TempDir()
	shadow := t.TempDir()
	f := filepath.Join(cwd, "code.txt")
	if err := os.WriteFile(f, []byte("v1"), 0o644); err != nil {
		t.Fatal(err)
	}
	l := &Loop{Checkpointer: checkpoint.NewManager("cas-before-code", cwd, shadow), ckptSnappedAt: -1}
	l.Messages = append(l.Messages, msg(llm.RoleUser, "make v2"))
	l.snapPreEdit("Edit", map[string]any{"path": f})
	if err := os.WriteFile(f, []byte("v2"), 0o644); err != nil {
		t.Fatal(err)
	}
	l.Messages = append(l.Messages, msg(llm.RoleAssistant, "done"))
	expected := l.History()
	appendCompletedTurn(l, "concurrent prompt", "concurrent answer")

	_, err := l.rewindToTurnExpected(expected, 1, RewindCodeAndConversation)
	if !errors.Is(err, ErrConversationChanged) {
		t.Fatalf("rewind stale history err=%v, want ErrConversationChanged", err)
	}
	if body, _ := os.ReadFile(f); string(body) != "v2" {
		t.Fatalf("stale rewind modified files to %q, want v2", body)
	}
	if got := l.CountTurns(); got != 2 {
		t.Fatalf("stale rewind changed conversation turns=%d, want 2", got)
	}
}

func TestRewindToTurn_CodeWithoutSnapshotFailsInsteadOfFalseSuccess(t *testing.T) {
	l := &Loop{Checkpointer: checkpoint.NewManager("missing-point", t.TempDir(), t.TempDir()), ckptSnappedAt: -1}
	appendCompletedTurn(l, "conversation only", "answer")
	res, err := l.RewindToTurn(1, RewindCode)
	if !errors.Is(err, ErrCheckpointUnavailable) {
		t.Fatalf("err=%v, want ErrCheckpointUnavailable; result=%+v", err, res)
	}
	if res.CodeRestored {
		t.Fatalf("missing checkpoint falsely reported code restored: %+v", res)
	}
}

func TestRewind_FirstWriteInEmptyProjectDeletesCreatedFile(t *testing.T) {
	cwd := t.TempDir()
	shadow := t.TempDir()
	created := filepath.Join(cwd, "first.go")
	l := &Loop{Checkpointer: checkpoint.NewManager("empty-first-write", cwd, shadow), ckptSnappedAt: -1}
	l.Messages = append(l.Messages, msg(llm.RoleUser, "create first.go"))
	l.snapPreEdit("Write", map[string]any{"path": created})
	if err := os.WriteFile(created, []byte("package first"), 0o644); err != nil {
		t.Fatal(err)
	}
	l.recordCheckpointMutation("Write", map[string]any{"path": created}, nil, nil)
	l.Messages = append(l.Messages, msg(llm.RoleAssistant, "created"))

	if _, err := l.RewindToTurn(1, RewindCodeAndConversation); err != nil {
		t.Fatalf("RewindToTurn: %v", err)
	}
	if _, err := os.Stat(created); !os.IsNotExist(err) {
		t.Fatalf("first created file survived rewind: err=%v", err)
	}
}

func TestRewind_BashCreatedFileIsAttributedAndDeleted(t *testing.T) {
	cwd := t.TempDir()
	l := &Loop{Checkpointer: checkpoint.NewManager("bash-create", cwd, t.TempDir()), ckptSnappedAt: -1}
	created := filepath.Join(cwd, "from-bash.txt")
	l.Messages = append(l.Messages, msg(llm.RoleUser, "create via shell"))
	l.snapPreEdit("Bash", map[string]any{"command": "create"})
	if err := os.WriteFile(created, []byte("shell output"), 0o600); err != nil {
		t.Fatal(err)
	}
	l.recordCheckpointMutation("Bash", map[string]any{"command": "create"}, nil, nil)
	l.Messages = append(l.Messages, msg(llm.RoleAssistant, "done"))

	if _, err := l.RewindToTurn(1, RewindCodeAndConversation); err != nil {
		t.Fatalf("RewindToTurn: %v", err)
	}
	if _, err := os.Stat(created); !os.IsNotExist(err) {
		t.Fatalf("Bash-created file survived rewind: %v", err)
	}
}

func TestRewind_BashRenameRestoresOldPathAndRemovesNewPath(t *testing.T) {
	cwd := t.TempDir()
	oldPath := filepath.Join(cwd, "old.txt")
	newPath := filepath.Join(cwd, "new.txt")
	if err := os.WriteFile(oldPath, []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}
	l := &Loop{Checkpointer: checkpoint.NewManager("bash-rename", cwd, t.TempDir()), ckptSnappedAt: -1}
	l.Messages = append(l.Messages, msg(llm.RoleUser, "rename via shell"))
	l.snapPreEdit("Bash", map[string]any{"command": "mv"})
	if err := os.Rename(oldPath, newPath); err != nil {
		t.Fatal(err)
	}
	l.recordCheckpointMutation("Bash", map[string]any{"command": "mv"}, nil, nil)
	l.Messages = append(l.Messages, msg(llm.RoleAssistant, "done"))

	if _, err := l.RewindToTurn(1, RewindCode); err != nil {
		t.Fatalf("RewindToTurn: %v", err)
	}
	if body, err := os.ReadFile(oldPath); err != nil || string(body) != "original" {
		t.Fatalf("old path body=%q err=%v", body, err)
	}
	if _, err := os.Stat(newPath); !os.IsNotExist(err) {
		t.Fatalf("renamed path survived rewind: %v", err)
	}
}

func TestRewind_RestoreFailureKeepsLegacyStackEntry(t *testing.T) {
	l := &Loop{Checkpointer: checkpoint.NewManager("bad-hash", t.TempDir(), t.TempDir()), ckptSnappedAt: -1}
	appendCompletedTurn(l, "edit", "done")
	l.ckptStack = []ckptEntry{{hash: "not-a-commit", restoreToTurns: 0, label: "bad"}}
	if _, ok := l.Rewind(); ok {
		t.Fatal("Rewind unexpectedly succeeded")
	}
	if !l.HasRewindPoints() {
		t.Fatal("failed restore consumed the legacy rewind point")
	}
}

func TestSummarizeFromTurn_ReplacesSelectedSuffixAndPrefillsPrompt(t *testing.T) {
	l := &Loop{ckptSnappedAt: -1}
	appendCompletedTurn(l, "keep this", "kept answer")
	appendCompletedTurn(l, "side quest", "long detour")
	appendCompletedTurn(l, "more detour", "more details")
	cfg := DefaultCompactionConfig()
	l.Compactor = NewCompactor(cfg, "test", 200_000, &fakeSummarizer{})

	res, err := l.SummarizeFromTurn(context.Background(), 2)
	if err != nil {
		t.Fatalf("SummarizeFromTurn: %v", err)
	}
	if res.Prompt != "side quest" || res.Summary != "MOCK_SUMMARY" || res.TurnsUndone != 2 {
		t.Fatalf("result=%+v", res)
	}
	history := l.History()
	if len(history) < 3 {
		t.Fatalf("history too short after targeted summary: %+v", history)
	}
	joined := ""
	for _, message := range history {
		for _, block := range message.Content {
			if block.Type == "text" {
				joined += block.Text + "\n"
			}
		}
	}
	if !strings.Contains(joined, "keep this") || !strings.Contains(joined, "MOCK_SUMMARY") {
		t.Fatalf("targeted summary lost prefix/summary:\n%s", joined)
	}
	if strings.Contains(joined, "long detour") || strings.Contains(joined, "more details") {
		t.Fatalf("selected suffix survived targeted summary:\n%s", joined)
	}
}
