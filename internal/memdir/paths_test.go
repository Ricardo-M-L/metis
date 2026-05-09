package memdir

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEnsureRoot_CreatesDirectoryAndIsIdempotent(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "memory")
	if err := EnsureRoot(dir); err != nil {
		t.Fatalf("EnsureRoot: %v", err)
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if !info.IsDir() {
		t.Fatalf("not a directory")
	}
	// Idempotent.
	if err := EnsureRoot(dir); err != nil {
		t.Fatalf("EnsureRoot second time: %v", err)
	}
}

func TestEnsureRoot_RejectsEmpty(t *testing.T) {
	if err := EnsureRoot(""); err == nil {
		t.Fatalf("EnsureRoot(\"\") expected error")
	}
}

func TestIsAutoMemPath_HappyPath(t *testing.T) {
	root := t.TempDir()
	cand := filepath.Join(root, "user_role.md")
	if !IsAutoMemPath(root, cand) {
		t.Fatalf("expected %s ⊂ %s to be true", cand, root)
	}
}

func TestIsAutoMemPath_RejectsParentEscape(t *testing.T) {
	root := t.TempDir()
	cand := filepath.Join(root, "..", "evil.md")
	if IsAutoMemPath(root, cand) {
		t.Fatalf("expected %s ⊄ %s after `..`", cand, root)
	}
}

func TestIsAutoMemPath_RejectsRootItself(t *testing.T) {
	root := t.TempDir()
	if IsAutoMemPath(root, root) {
		t.Fatalf("root itself must not count as an auto-mem path (must be a file)")
	}
}

func TestIsAutoMemPath_RejectsEmpty(t *testing.T) {
	if IsAutoMemPath("", "/x.md") {
		t.Fatalf("empty root must not match")
	}
	if IsAutoMemPath("/r", "") {
		t.Fatalf("empty candidate must not match")
	}
}

func TestIsAutoMemPath_FollowsRootSymlink(t *testing.T) {
	if os.Getenv("CI") == "" {
		// Symlink behaviour on tmpfs varies on some sandboxes; gate to local.
	}
	target := t.TempDir()
	link := filepath.Join(t.TempDir(), "memlink")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlink not available: %v", err)
	}
	cand := filepath.Join(target, "user_role.md")
	if !IsAutoMemPath(link, cand) {
		t.Fatalf("symlinked root should still match canonical candidate")
	}
}

func TestIsEntrypoint(t *testing.T) {
	if !IsEntrypoint("/some/path/MEMORY.md") {
		t.Fatalf("MEMORY.md should be entrypoint")
	}
	if IsEntrypoint("/some/path/user.md") {
		t.Fatalf("user.md is not entrypoint")
	}
}

func TestDefaultRoot_HasMetisMemorySuffix(t *testing.T) {
	got, err := DefaultRoot()
	if err != nil {
		t.Skipf("HOME unset: %v", err)
	}
	if !strings.HasSuffix(got, filepath.Join(".metis", "memory")) {
		t.Fatalf("DefaultRoot = %q, want ending in .metis/memory", got)
	}
}

func TestIndexPath(t *testing.T) {
	got := IndexPath("/r")
	if got != "/r/MEMORY.md" {
		t.Fatalf("IndexPath = %q, want /r/MEMORY.md", got)
	}
}
