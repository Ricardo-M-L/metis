package builtin

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/memory"
	"github.com/Ricardo-M-L/metis/internal/permission"
)

// helper: build a Memory tool wired to a fresh on-disk MemoryManager.
// Returns the tool + the manager so callers can sanity-check writes
// landed in the same store BuildContext reads from (the whole point
// of bug #11's rewrite).
func newTestMemory(t *testing.T) (Memory, *memory.MemoryManager) {
	t.Helper()
	mm, err := memory.NewMemoryManager(filepath.Join(t.TempDir(), "memory"))
	if err != nil {
		t.Fatalf("NewMemoryManager: %v", err)
	}
	return NewMemory(permission.New(permission.ModeBypass), mm), mm
}

func TestMemory_AddPersistsToBlock(t *testing.T) {
	tool, mm := newTestMemory(t)
	res, err := tool.Execute(context.Background(), map[string]any{
		"action":  "add",
		"target":  "user",
		"content": "user prefers Chinese responses",
	})
	if err != nil || res.IsError {
		t.Fatalf("add: err=%v isError=%v out=%q", err, res.IsError, res.Output)
	}
	// Verify the same store BuildContext reads from has the new content.
	blk := mm.Core().GetBlock("user")
	if !strings.Contains(blk.Content, "Chinese responses") {
		t.Errorf("Block.Content lost the add: %q", blk.Content)
	}
	// And BuildContext (system-prompt path) renders it. This is the
	// whole disconnect bug — the original Memory tool wrote to a
	// different file and BuildContext never saw it.
	rendered := mm.BuildContext()
	if !strings.Contains(rendered, "Chinese responses") {
		t.Errorf("BuildContext didn't pick up Memory.add — disconnect regressed:\n%s", rendered)
	}
}

func TestMemory_AddAppendsToExisting(t *testing.T) {
	tool, mm := newTestMemory(t)
	for _, c := range []string{"first fact", "second fact"} {
		res, _ := tool.Execute(context.Background(), map[string]any{
			"action": "add", "target": "user", "content": c,
		})
		if res.IsError {
			t.Fatalf("add %q: %s", c, res.Output)
		}
	}
	blk := mm.Core().GetBlock("user")
	if !strings.Contains(blk.Content, "first fact") || !strings.Contains(blk.Content, "second fact") {
		t.Errorf("second add overwrote first: %q", blk.Content)
	}
}

func TestMemory_ReplaceFindsMatch(t *testing.T) {
	tool, mm := newTestMemory(t)
	mm.Core().UpdateBlock("user", "uses Python\nuses Vim")
	res, err := tool.Execute(context.Background(), map[string]any{
		"action": "replace", "target": "user",
		"match": "uses Vim", "content": "uses Neovim",
	})
	if err != nil || res.IsError {
		t.Fatalf("replace: %s", res.Output)
	}
	blk := mm.Core().GetBlock("user")
	if strings.Contains(blk.Content, "uses Vim") || !strings.Contains(blk.Content, "Neovim") {
		t.Errorf("replace didn't swap correctly: %q", blk.Content)
	}
}

func TestMemory_ReplaceMissingMatchErrors(t *testing.T) {
	tool, _ := newTestMemory(t)
	res, _ := tool.Execute(context.Background(), map[string]any{
		"action": "replace", "target": "user",
		"match": "nope", "content": "x",
	})
	if !res.IsError {
		t.Errorf("expected error when match not found, got %q", res.Output)
	}
}

func TestMemory_ReadEmptyBlock(t *testing.T) {
	tool, _ := newTestMemory(t)
	res, _ := tool.Execute(context.Background(), map[string]any{
		"action": "read", "target": "working",
	})
	if !strings.Contains(res.Output, "empty") {
		t.Errorf("read on empty block should say (empty); got %q", res.Output)
	}
}

func TestMemory_RejectUnknownTarget(t *testing.T) {
	tool, _ := newTestMemory(t)
	res, _ := tool.Execute(context.Background(), map[string]any{
		"action": "add", "target": "garbage", "content": "x",
	})
	if !res.IsError {
		t.Errorf("unknown target should error, got %q", res.Output)
	}
}

// TestMemory_NilManagerReturnsClearError covers the `metis tools`
// listing case where NewMemory(gate, nil) is registered just to show
// the capability — Execute must error gracefully, not nil-deref.
func TestMemory_NilManagerReturnsClearError(t *testing.T) {
	tool := NewMemory(permission.New(permission.ModeBypass), nil)
	res, _ := tool.Execute(context.Background(), map[string]any{
		"action": "add", "target": "user", "content": "x",
	})
	if !res.IsError {
		t.Errorf("nil manager should produce IsError, got %q", res.Output)
	}
	if !strings.Contains(res.Output, "manager not initialized") {
		t.Errorf("error should mention manager not initialized, got %q", res.Output)
	}
}
