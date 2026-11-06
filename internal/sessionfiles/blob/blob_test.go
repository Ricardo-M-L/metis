package blob

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBlobBoundedVerifiedAndScoped(t *testing.T) {
	dir := t.TempDir()
	hash, err := Put(dir, []byte("image"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Read(dir, "../../outside"); err == nil {
		t.Fatal("invalid identity accepted")
	}
	path := filepath.Join(dir, hash)
	if err := os.Truncate(path, MaxBytes+1); err != nil {
		t.Fatal(err)
	}
	if _, err := Read(dir, hash); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("oversized attachment: %v", err)
	}
	if err := os.WriteFile(path, []byte("tampered"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Read(dir, hash); err == nil || !strings.Contains(err.Error(), "checksum") {
		t.Fatalf("corruption not reported: %v", err)
	}
}
