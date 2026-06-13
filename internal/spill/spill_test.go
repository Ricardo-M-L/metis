package spill

import (
	"os"
	"path/filepath"
	"strings"
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
	if len(r.Preview) != PreviewChars {
		t.Fatalf("Preview len = %d, want %d", len(r.Preview), PreviewChars)
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

// Preview must not split a multi-byte rune at the cut point.
func TestPreviewRespectsUTF8(t *testing.T) {
	content := strings.Repeat("中", PreviewChars) // 3 bytes each — guaranteed mid-rune cut
	preview, hasMore := makePreview(content, PreviewChars)
	if !hasMore {
		t.Fatal("hasMore = false, want true")
	}
	if !strings.HasSuffix(preview, "中") {
		t.Fatalf("preview ends mid-rune: %q", preview[len(preview)-4:])
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
