package tui

// vision_gate_test.go — covers the 2026-05-18 P2 fix: when the active
// provider doesn't advertise SupportsVision(), pasted image blocks
// must be stripped before AppendUserBlocks so the API doesn't see
// images it'll reject. The remainder (text blocks) still goes
// through so the user's question survives.

import (
	"testing"

	"github.com/Ricardo-M-L/metis/internal/llm"
)

func TestSplitOffImageBlocks_StripsImagesOnly(t *testing.T) {
	t.Parallel()
	blocks := []llm.ContentBlock{
		{Type: "text", Text: "hello"},
		{Type: "image", MediaType: "image/png", Data: "..."},
		{Type: "text", Text: " world"},
		{Type: "image", MediaType: "image/jpeg", Data: "..."},
	}
	stripped, kept := splitOffImageBlocks(blocks)
	if stripped != 2 {
		t.Errorf("stripped count = %d, want 2", stripped)
	}
	if len(kept) != 2 {
		t.Fatalf("kept = %d, want 2 (text only)", len(kept))
	}
	for i, b := range kept {
		if b.Type != "text" {
			t.Errorf("kept[%d].Type = %q, want text", i, b.Type)
		}
	}
}

func TestSplitOffImageBlocks_NoImages_NoChange(t *testing.T) {
	t.Parallel()
	blocks := []llm.ContentBlock{
		{Type: "text", Text: "hi"},
		{Type: "text", Text: " there"},
	}
	stripped, kept := splitOffImageBlocks(blocks)
	if stripped != 0 {
		t.Errorf("nothing to strip, got count = %d", stripped)
	}
	if len(kept) != len(blocks) {
		t.Errorf("kept len = %d, want %d", len(kept), len(blocks))
	}
}

func TestSplitOffImageBlocks_AllImages_LeavesEmpty(t *testing.T) {
	t.Parallel()
	blocks := []llm.ContentBlock{
		{Type: "image", MediaType: "image/png", Data: "a"},
		{Type: "image", MediaType: "image/jpeg", Data: "b"},
	}
	stripped, kept := splitOffImageBlocks(blocks)
	if stripped != 2 {
		t.Errorf("stripped = %d, want 2", stripped)
	}
	if len(kept) != 0 {
		t.Errorf("kept = %d, want 0 — only images present", len(kept))
	}
}
