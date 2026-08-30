package memdir

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestAtomicWritePrivateFileRejectsSymlinkRoot(t *testing.T) {
	requireSecureFileSymlinks(t)
	realRoot := t.TempDir()
	alias := filepath.Join(t.TempDir(), "memory")
	if err := os.Symlink(realRoot, alias); err != nil {
		t.Fatal(err)
	}
	err := AtomicWritePrivateFile(alias, "topic.md", []byte("secret"), 0o600)
	if err == nil || !strings.Contains(strings.ToLower(err.Error()), "root") {
		t.Fatalf("symlink root error=%v, want root rejection", err)
	}
	if _, statErr := os.Stat(filepath.Join(realRoot, "topic.md")); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("writer escaped through root symlink: %v", statErr)
	}
}

func TestAtomicWritePrivateFileRejectsSymlinkParent(t *testing.T) {
	requireSecureFileSymlinks(t)
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "topics")); err != nil {
		t.Fatal(err)
	}
	err := AtomicWritePrivateFile(root, filepath.Join("topics", "topic.md"), []byte("secret"), 0o600)
	if err == nil || !strings.Contains(strings.ToLower(err.Error()), "parent") {
		t.Fatalf("symlink parent error=%v, want parent rejection", err)
	}
	if _, statErr := os.Stat(filepath.Join(outside, "topic.md")); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("writer escaped through parent symlink: %v", statErr)
	}
}

func TestAtomicWritePrivateFileRejectsDanglingLeafSymlink(t *testing.T) {
	requireSecureFileSymlinks(t)
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside")
	leaf := filepath.Join(root, "topic.md")
	if err := os.Symlink(outside, leaf); err != nil {
		t.Fatal(err)
	}
	err := AtomicWritePrivateFile(root, "topic.md", []byte("secret"), 0o600)
	if err == nil || !strings.Contains(strings.ToLower(err.Error()), "regular") {
		t.Fatalf("dangling leaf error=%v, want regular-file rejection", err)
	}
	if _, statErr := os.Stat(outside); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("writer followed dangling leaf symlink: %v", statErr)
	}
}

func TestReadPrivateRegularFileIsBoundedAndRejectsLeafSymlink(t *testing.T) {
	requireSecureFileSymlinks(t)
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside.md")
	if err := os.WriteFile(outside, []byte("outside secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "topic.md")); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadPrivateRegularFile(root, "topic.md", 64); err == nil {
		t.Fatal("pinned read accepted a leaf symlink")
	}
	if err := os.Remove(filepath.Join(root, "topic.md")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "topic.md"), []byte("12345"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadPrivateRegularFile(root, "topic.md", 4); err == nil || !strings.Contains(err.Error(), "4 byte") {
		t.Fatalf("bounded read error=%v, want size rejection", err)
	}
}

func requireSecureFileSymlinks(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation requires additional Windows privileges")
	}
}
