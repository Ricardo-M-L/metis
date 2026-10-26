package agent

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/checkpoint"
	"github.com/Ricardo-M-L/metis/internal/llm"
)

func TestCheckpointControlScopedBashRewindLeavesSiblingAndOffRetainsSnapshots(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("METIS_HOME", filepath.Join(home, ".metis"))
	for _, name := range []string{"a", "b"} {
		dir := filepath.Join(home, name)
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "file.txt"), []byte("before"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	loop := &Loop{Checkpointer: checkpoint.NewManager("scoped-loop", home, ""), ckptSnappedAt: -1, Messages: []llm.Message{msg(llm.RoleUser, "edit")}}
	if _, err := loop.CheckpointCommand(context.Background(), "on a"); err != nil {
		t.Fatal(err)
	}
	if err := loop.snapPreEditContext(context.Background(), "Bash", nil); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"a", "b"} {
		if err := os.WriteFile(filepath.Join(home, name, "file.txt"), []byte("after"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := loop.recordCheckpointMutationContext(context.Background(), "Bash", nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := loop.CheckpointCommand(context.Background(), "off"); err != nil {
		t.Fatal(err)
	}
	loop.Messages = append(loop.Messages, msg(llm.RoleAssistant, "done"))
	if _, err := loop.RewindWithPersist(nil); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{"a": "before", "b": "after"} {
		body, err := os.ReadFile(filepath.Join(home, name, "file.txt"))
		if err != nil || string(body) != want {
			t.Fatalf("%s: %s %v", name, body, err)
		}
	}
}

func TestCheckpointControlScopedSymlinkStillReportsProtectionGap(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("METIS_HOME", filepath.Join(home, ".metis"))
	project := filepath.Join(home, "project")
	if err := os.Mkdir(project, 0o700); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(home, "outside.txt")
	if err := os.WriteFile(outside, []byte("outside"), 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(project, "link.txt")
	if err := os.Symlink(outside, path); err != nil {
		t.Skip(err)
	}
	loop := &Loop{Checkpointer: checkpoint.NewManager("scoped-link", home, ""), ckptSnappedAt: -1, Messages: []llm.Message{msg(llm.RoleUser, "edit")}}
	if _, err := loop.CheckpointCommand(context.Background(), "on project"); err != nil {
		t.Fatal(err)
	}
	if err := loop.snapPreEditContext(context.Background(), "Write", map[string]any{"path": path}); err != nil {
		t.Fatal(err)
	}
	if err := loop.recordCheckpointMutationContext(context.Background(), "Write", map[string]any{"path": path}, nil, nil); err == nil {
		t.Fatal("in-scope symlink silently advertised as protected")
	}
}
