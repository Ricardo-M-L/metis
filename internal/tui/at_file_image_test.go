package tui

// at_file_image_test.go — covers the 2026-05-18 P3 fix:
// `@/path/to/image.png` references in user text get auto-expanded to
// image content blocks. Mirrors claude-code's tryReadImageFromPath.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestExpandAtFileImageBlocks_SuccessfulAbsolutePath(t *testing.T) {
	dir := t.TempDir()
	p := makeTinyPNG(t, dir, "screenshot.png", 10, 10)

	text := "please look at @" + p + " thanks"
	rewritten, blocks, errs := expandAtFileImageBlocks(text, "")
	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if len(blocks) != 1 {
		t.Fatalf("expected 1 image block; got %d", len(blocks))
	}
	if blocks[0].Type != "image" {
		t.Errorf("block.Type = %q, want image", blocks[0].Type)
	}
	if strings.Contains(rewritten, p) {
		t.Errorf("rewritten text should NOT contain the @path; got %q", rewritten)
	}
	if !strings.Contains(rewritten, "please look at") {
		t.Errorf("surrounding text lost: %q", rewritten)
	}
	if !strings.Contains(rewritten, "thanks") {
		t.Errorf("trailing text lost: %q", rewritten)
	}
}

func TestExpandAtFileImageBlocks_RelativeResolvesAgainstCwd(t *testing.T) {
	dir := t.TempDir()
	makeTinyPNG(t, dir, "bug.png", 10, 10)

	text := "@bug.png reproduce this"
	_, blocks, errs := expandAtFileImageBlocks(text, dir)
	if len(errs) != 0 {
		t.Fatalf("errs: %v", errs)
	}
	if len(blocks) != 1 {
		t.Fatalf("expected 1 block; got %d (cwd=%s)", len(blocks), dir)
	}
}

func TestExpandAtFileImageBlocks_NonExistentPathLeavesVerbatim(t *testing.T) {
	text := "see @/does/not/exist.png for the bug"
	rewritten, blocks, errs := expandAtFileImageBlocks(text, "")
	if len(blocks) != 0 {
		t.Errorf("expected 0 blocks for missing file; got %d", len(blocks))
	}
	if len(errs) != 1 {
		t.Errorf("expected 1 error for missing file; got %d", len(errs))
	}
	if !strings.Contains(rewritten, "@/does/not/exist.png") {
		t.Errorf("verbatim @path should survive on error; got %q", rewritten)
	}
}

func TestExpandAtFileImageBlocks_IgnoresNonImageAtMentions(t *testing.T) {
	// @-mentions without recognized image extensions should pass
	// through completely unchanged: no error, no blocks, no text rewrite.
	cases := []string{
		"hi @username",
		"reach me at me@example.com",
		"open @./README.md",
		"check @config.toml",
	}
	for _, text := range cases {
		rewritten, blocks, errs := expandAtFileImageBlocks(text, "")
		if rewritten != text {
			t.Errorf("non-image @ref should not rewrite text: %q → %q", text, rewritten)
		}
		if len(blocks) != 0 {
			t.Errorf("non-image @ref should produce no blocks: %q", text)
		}
		if len(errs) != 0 {
			t.Errorf("non-image @ref should produce no errors: %q", text)
		}
	}
}

func TestExpandAtFileImageBlocks_MultipleMatches(t *testing.T) {
	dir := t.TempDir()
	p1 := makeTinyPNG(t, dir, "before.png", 10, 10)
	p2 := makeTinyPNG(t, dir, "after.jpg", 10, 10)

	text := "compare @" + p1 + " with @" + p2 + " — what changed?"
	rewritten, blocks, errs := expandAtFileImageBlocks(text, "")
	if len(errs) != 0 {
		t.Fatalf("errs: %v", errs)
	}
	if len(blocks) != 2 {
		t.Fatalf("expected 2 image blocks; got %d", len(blocks))
	}
	if !strings.Contains(rewritten, "compare") || !strings.Contains(rewritten, "what changed?") {
		t.Errorf("surrounding text lost: %q", rewritten)
	}
}

func TestExpandAtFileImageBlocks_EmptyInput(t *testing.T) {
	rewritten, blocks, errs := expandAtFileImageBlocks("", "")
	if rewritten != "" || len(blocks) != 0 || len(errs) != 0 {
		t.Errorf("empty input should return zero values; got %q blocks=%d errs=%v", rewritten, len(blocks), errs)
	}
}

// Defensive: make sure a path with the wrong extension but right
// magic bytes (e.g. PNG saved as .jpg-named file) still WORKS as long
// as the extension matches one of our regex options. loadAndPrepImage
// then does http.DetectContentType for the actual MIME.
func TestExpandAtFileImageBlocks_ExtensionMatchVsContent(t *testing.T) {
	dir := t.TempDir()
	// Write a real PNG to a .jpg-named path. Regex matches on .jpg,
	// loadAndPrepImage sniffs and reports it as PNG via mime detect.
	pathJpg := filepath.Join(dir, "misnamed.jpg")
	pngPath := makeTinyPNG(t, dir, "ref.png", 10, 10)
	body, err := os.ReadFile(pngPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pathJpg, body, 0o644); err != nil {
		t.Fatal(err)
	}

	text := "see @" + pathJpg
	_, blocks, errs := expandAtFileImageBlocks(text, "")
	if len(errs) != 0 {
		t.Fatalf("errs: %v", errs)
	}
	if len(blocks) != 1 {
		t.Fatalf("expected 1 block; got %d", len(blocks))
	}
	// MIME should reflect actual content (png), not the .jpg extension.
	if blocks[0].MediaType != "image/png" {
		t.Errorf("MediaType = %q, want image/png (sniffed from magic bytes)", blocks[0].MediaType)
	}
}
