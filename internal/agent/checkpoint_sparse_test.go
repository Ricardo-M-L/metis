package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/checkpoint"
	"github.com/Ricardo-M-L/metis/internal/llm"
)

func TestCheckpointHomeSessionDirectEditsRewindOriginals(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("METIS_HOME", filepath.Join(home, ".metis"))
	l := &Loop{Checkpointer: checkpoint.NewManager("home-edits", home, ""), ckptSnappedAt: -1}
	if _, err := l.CheckpointCommand(t.Context(), "on"); err != nil {
		t.Fatal(err)
	}
	a, b, created := filepath.Join(home, "a.txt"), filepath.Join(home, "b.txt"), filepath.Join(home, "new.txt")
	for _, p := range []string{a, b} {
		if err := os.WriteFile(p, []byte("original"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	l.Messages = []llm.Message{msg(llm.RoleUser, "edit several files")}
	write := func(path, content string) {
		t.Helper()
		in := map[string]any{"path": path}
		if err := l.snapPreEditContext(t.Context(), "Write", in); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := l.recordCheckpointMutationContext(t.Context(), "Write", in, nil, nil); err != nil {
			t.Fatal(err)
		}
	}
	write(a, "changed once")
	write(created, "created once")
	write(a, "changed twice")
	write(b, "changed last")
	write(created, "created twice")
	if len(l.ckptStack) != 1 {
		t.Fatalf("one turn became %d checkpoints", len(l.ckptStack))
	}
	if _, err := l.RewindToTurn(1, RewindCode); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{a, b} {
		if body, err := os.ReadFile(p); err != nil || string(body) != "original" {
			t.Fatalf("original lost for %s: %q %v", p, body, err)
		}
	}
	if _, err := os.Stat(created); !os.IsNotExist(err) {
		t.Fatalf("new file not removed: %v", err)
	}
}

func TestCheckpointHomeNewTargetInLaterTurnKeepsItsOriginal(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("METIS_HOME", filepath.Join(home, ".metis"))
	l := &Loop{Checkpointer: checkpoint.NewManager("home-turns", home, ""), ckptSnappedAt: -1}
	if _, err := l.CheckpointCommand(t.Context(), "on"); err != nil {
		t.Fatal(err)
	}
	for turn, name := range []string{"a", "b"} {
		path := filepath.Join(home, name)
		if err := os.WriteFile(path, []byte("original"), 0o600); err != nil {
			t.Fatal(err)
		}
		l.Messages = append(l.Messages, msg(llm.RoleUser, name))
		in := map[string]any{"path": path}
		if err := l.snapPreEditContext(t.Context(), "Write", in); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("changed"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := l.recordCheckpointMutationContext(t.Context(), "Write", in, nil, nil); err != nil {
			t.Fatal(err)
		}
		l.Messages = append(l.Messages, msg(llm.RoleAssistant, name))
		if l.CountTurns() != turn+1 {
			t.Fatal("bad test history")
		}
	}
	if _, err := l.RewindToTurn(1, RewindCode); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"a", "b"} {
		if body, err := os.ReadFile(filepath.Join(home, name)); err != nil || string(body) != "original" {
			t.Fatalf("%s original lost: %q %v", name, body, err)
		}
	}
}

func TestCheckpointHomeShellCannotClaimCompleteProtection(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("METIS_HOME", filepath.Join(home, ".metis"))
	l := &Loop{Checkpointer: checkpoint.NewManager("home-shell", home, ""), ckptSnappedAt: -1,
		Messages: []llm.Message{msg(llm.RoleUser, "shell")}}
	if _, err := l.CheckpointCommand(t.Context(), "on"); err != nil {
		t.Fatal(err)
	}
	out := make(chan Event, 8)
	l.runCheckpointStage(context.Background(), out, "before tool", "Bash", func(ctx context.Context) error {
		return l.snapPreEditContext(ctx, "Bash", map[string]any{"command": "arbitrary-command"})
	})
	if _, err := l.RewindToTurn(1, RewindCode); !errors.Is(err, ErrCheckpointIncomplete) {
		t.Fatalf("unknown shell writes claimed protected: %v", err)
	}
}
