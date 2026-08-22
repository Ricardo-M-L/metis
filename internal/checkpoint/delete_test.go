package checkpoint

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDeleteSessionRemovesOnlyExactShadow(t *testing.T) {
	root := t.TempDir()
	owned := filepath.Join(root, "sess")
	other := filepath.Join(root, "sess-extra")
	for _, dir := range []string{owned, other} {
		if err := os.MkdirAll(filepath.Join(dir, ".git", "objects"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, ".git", "HEAD"), []byte("ref"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	if err := DeleteSession("sess", root); err != nil {
		t.Fatalf("DeleteSession: %v", err)
	}
	if err := DeleteSession("sess", root); err != nil {
		t.Fatalf("second DeleteSession should be idempotent: %v", err)
	}
	if _, err := os.Stat(owned); !os.IsNotExist(err) {
		t.Fatalf("owned shadow still exists: %v", err)
	}
	if _, err := os.Stat(other); err != nil {
		t.Fatalf("prefix-collision shadow removed: %v", err)
	}
}

func TestDeleteSessionRejectsUnsafeIDsAndRoots(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(filepath.Dir(root), "outside-checkpoint-delete")
	if err := os.MkdirAll(outside, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(outside) })
	for _, id := range []string{"", ".", "..", "../outside-checkpoint-delete", "a/b", `a\b`, "bad\nname"} {
		if err := DeleteSession(id, root); err == nil {
			t.Errorf("DeleteSession(%q) unexpectedly succeeded", id)
		}
	}
	if err := DeleteSession("safe", string(filepath.Separator)); err == nil {
		t.Error("DeleteSession with filesystem root unexpectedly succeeded")
	}
	if _, err := os.Stat(outside); err != nil {
		t.Fatalf("outside directory changed: %v", err)
	}
}

func TestDeleteSessionDoesNotFollowTargetSymlink(t *testing.T) {
	root := t.TempDir()
	external := t.TempDir()
	marker := filepath.Join(external, "keep.txt")
	if err := os.WriteFile(marker, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "sess")
	if err := os.Symlink(external, link); err != nil {
		t.Fatal(err)
	}

	if err := DeleteSession("sess", root); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(link); !os.IsNotExist(err) {
		t.Fatalf("session symlink still exists: %v", err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("symlink target was modified: %v", err)
	}
}

func TestDeleteSessionRejectsShadowRootSymlinkToFilesystemRoot(t *testing.T) {
	link := filepath.Join(t.TempDir(), "root-link")
	if err := os.Symlink(string(filepath.Separator), link); err != nil {
		t.Fatal(err)
	}
	if err := DeleteSession("sess", link); err == nil {
		t.Error("DeleteSession with root symlink unexpectedly succeeded")
	}
}

func TestDeleteSessionEmptyRootMatchesNewManagerDefault(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	m := NewManager("sess", t.TempDir(), "")
	want := filepath.Join(home, ".metis", "checkpoints", "sess")
	if m.shadowDir != want {
		t.Fatalf("NewManager shadowDir = %q, want %q", m.shadowDir, want)
	}
	if err := os.MkdirAll(m.shadowDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := DeleteSession("sess", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(want); !os.IsNotExist(err) {
		t.Fatalf("default shadow still exists: %v", err)
	}
}
