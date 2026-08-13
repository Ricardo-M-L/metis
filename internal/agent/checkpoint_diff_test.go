package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/checkpoint"
)

func TestCheckpointTurnDiffsUsesAdjacentSnapshotsAndLiveTree(t *testing.T) {
	cwd := t.TempDir()
	write := func(body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(cwd, "main.go"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	manager := checkpoint.NewManager("turn-diff", cwd, t.TempDir())
	write("one\n")
	first, err := manager.Snap("Edit", "one", "before turn 1")
	if err != nil {
		t.Fatal(err)
	}
	write("two\n")
	second, err := manager.Snap("Edit", "two", "before turn 3")
	if err != nil {
		t.Fatal(err)
	}
	write("three\n")

	loop := &Loop{
		Checkpointer: manager,
		ckptStack: []ckptEntry{
			{hash: first, restoreToTurns: 0, label: "before turn 1"},
			{hash: second, restoreToTurns: 2, label: "before turn 3"},
		},
	}
	got := loop.CheckpointTurnDiffs()
	if len(got) != 2 {
		t.Fatalf("diffs = %d, want 2: %+v", len(got), got)
	}
	if got[0].Turn != 3 || !strings.Contains(got[0].Patch, "+three") {
		t.Fatalf("newest turn diff = %+v", got[0])
	}
	if got[1].Turn != 1 || !strings.Contains(got[1].Patch, "-one") || !strings.Contains(got[1].Patch, "+two") {
		t.Fatalf("oldest turn diff = %+v", got[1])
	}
	if len(loop.ckptStack) != 2 {
		t.Fatalf("read-only diff mutated checkpoint stack: %+v", loop.ckptStack)
	}
}
