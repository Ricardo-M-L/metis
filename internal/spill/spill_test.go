package spill

import (
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"
	"testing"
)

func TestStoreWritesAndStubs(t *testing.T) {
	dir := t.TempDir()
	content := strings.Repeat("x", PreviewChars+500)
	r, err := Store(dir, "toolu_abc123", content)
	if err != nil {
		t.Fatalf("Store: %v", err)
	}
	if r.OriginalSize != len(content) {
		t.Fatalf("OriginalSize = %d, want %d", r.OriginalSize, len(content))
	}
	if !r.HasMore {
		t.Fatal("HasMore = false, want true")
	}
	// Head-only (no error tail): preview is the head plus a truncation
	// marker, so it's bounded near PreviewChars but not exactly equal.
	if len(r.Preview) > PreviewChars+80 {
		t.Fatalf("preview unexpectedly long: %d", len(r.Preview))
	}
	if !strings.Contains(r.Preview, "truncated") {
		t.Errorf("head-only preview should carry a truncation marker; got %q", r.Preview[:60])
	}
	got, err := os.ReadFile(r.Path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if string(got) != content {
		t.Fatal("persisted content differs from original")
	}
	stub := r.Stub()
	if !strings.Contains(stub, r.Path) {
		t.Fatal("stub does not mention the file path")
	}
	if !strings.Contains(stub, "use Read tool") {
		t.Fatal("stub does not teach the recovery idiom")
	}
}

// Second Store with the same tool_use_id must not rewrite the file
// (write-once via O_EXCL) and still return a usable stub.
func TestStoreIdempotent(t *testing.T) {
	dir := t.TempDir()
	if _, err := Store(dir, "toolu_dup", "original"); err != nil {
		t.Fatalf("first Store: %v", err)
	}
	r, err := Store(dir, "toolu_dup", "original")
	if err != nil {
		t.Fatalf("second Store: %v", err)
	}
	got, _ := os.ReadFile(r.Path)
	if string(got) != "original" {
		t.Fatalf("file content = %q, want %q", got, "original")
	}
}

func TestStoreSanitizesID(t *testing.T) {
	dir := t.TempDir()
	r, err := Store(dir, "weird/../id", "data")
	if err != nil {
		t.Fatalf("Store: %v", err)
	}
	if filepath.Dir(r.Path) != dir {
		t.Fatalf("path escaped dir: %s", r.Path)
	}
}

// Spill must NOT key its file as bare "<id>.txt": that namespace
// belongs to the Compactor's Microcompact offload (os.WriteFile,
// truncating). A collision would let a Microcompact pass clobber the
// full spilled content. The ".spill.txt" suffix keeps them disjoint.
func TestStoreFilenameDisjointFromMicrocompact(t *testing.T) {
	dir := t.TempDir()
	r, err := Store(dir, "toolu_x", strings.Repeat("a", 100))
	if err != nil {
		t.Fatalf("Store: %v", err)
	}
	if filepath.Base(r.Path) == "toolu_x.txt" {
		t.Fatal("spill must not use the bare <id>.txt name reserved by Microcompact")
	}
	if !strings.HasSuffix(r.Path, ".spill.txt") {
		t.Errorf("expected .spill.txt suffix, got %s", r.Path)
	}
}

// Preview must not split a multi-byte rune at the cut point (head-only
// path — all-CJK content with no error markers).
func TestPreviewRespectsUTF8(t *testing.T) {
	content := strings.Repeat("中", PreviewChars) // 3 bytes each — guaranteed mid-rune cut
	preview, hasMore := makePreview(content, PreviewChars)
	if !hasMore {
		t.Fatal("hasMore = false, want true")
	}
	if !utf8.ValidString(preview) {
		t.Fatal("preview contains an invalid UTF-8 sequence (split a rune)")
	}
	if !strings.Contains(preview, "truncated") {
		t.Errorf("head-only preview should carry a truncation marker; got tail %q", preview[len(preview)-30:])
	}
}

// When the dropped tail carries error output, the preview keeps both
// head AND tail so the model still sees the failure verdict (compiler /
// test / stack-trace errors land at the very end). This is the MiMo-Code
// error-aware behavior Claude Code lacks.
func TestPreviewKeepsErrorTail(t *testing.T) {
	head := strings.Repeat("compiling step ok\n", 300) // pushes well past the limit
	tail := "\n\npanic: runtime error: index out of range [5] with length 3\n--- FAIL: TestThing"
	content := head + tail

	preview, hasMore := makePreview(content, PreviewChars)
	if !hasMore {
		t.Fatal("hasMore = false, want true")
	}
	if !strings.Contains(preview, "panic: runtime error") {
		t.Error("error tail was dropped — pure-head truncation lost the failure")
	}
	if !strings.Contains(preview, "--- FAIL: TestThing") {
		t.Error("test-failure verdict at the very end was dropped")
	}
	if !strings.Contains(preview, "omitted") || !strings.Contains(preview, "error output") {
		t.Errorf("head+tail preview should explain the omission; got %q", preview)
	}
	// Head must still be present (not just the tail).
	if !strings.Contains(preview, "compiling step ok") {
		t.Error("head was dropped")
	}
}

// No error markers in the dropped tail → stays head-only (don't pay the
// tail-preservation cost for ordinary large output).
func TestPreviewHeadOnlyWhenNoErrorTail(t *testing.T) {
	content := strings.Repeat("ordinary log line\n", 400)
	preview, _ := makePreview(content, PreviewChars)
	if strings.Contains(preview, "error output") {
		t.Error("kept tail for non-error output — should be head-only")
	}
	if !strings.Contains(preview, "truncated") {
		t.Error("expected a plain head truncation marker")
	}
}

func TestShortContentNotMarkedHasMore(t *testing.T) {
	dir := t.TempDir()
	r, err := Store(dir, "toolu_small", "tiny")
	if err != nil {
		t.Fatalf("Store: %v", err)
	}
	if r.HasMore || r.Preview != "tiny" {
		t.Fatalf("Preview=%q HasMore=%v, want full content inline", r.Preview, r.HasMore)
	}
}

func TestStoreRequiresDir(t *testing.T) {
	if _, err := Store("", "id", "x"); err == nil {
		t.Fatal("Store with empty dir should error")
	}
}
